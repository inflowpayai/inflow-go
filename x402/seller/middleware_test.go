package seller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
	upto "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/server"
)

func TestActualUpstreamMiddleware(t *testing.T) {
	for _, scenario := range []struct {
		name, scheme   string
		verify, settle bool
		handlerStatus  int
		panicHandler   bool
		amount         string
	}{
		{"balance", "balance", true, true, 200, false, ""},
		{"exact", "exact", true, true, 200, false, ""},
		{"metered", "upto", true, true, 200, false, "123"},
		{"zero-metered", "upto", true, true, 200, false, "0"},
		{"declined", "balance", false, true, 200, false, ""},
		{"handler-failure", "balance", true, true, 500, false, ""},
		{"handler-panic", "balance", true, true, 200, true, ""},
		{"settlement-failure", "balance", true, false, 200, false, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			config := sampleConfig()
			var mu sync.Mutex
			events := []string{}
			identifier := ""
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("X-API-Key") != "key" {
					t.Error("missing seller key")
				}
				switch r.URL.Path {
				case "/v1/x402/config":
					json.NewEncoder(w).Encode(config)
				case "/v1/x402/supported":
					json.NewEncoder(w).Encode(x402.SupportedResponse{Kinds: append(config.Supported, x402.SupportedKind{X402Version: 2, Scheme: "balance", Network: "inflow:1"}, x402.SupportedKind{X402Version: 2, Scheme: "exact", Network: "eip155:8453"})})
				case "/v1/x402/verify", "/v1/x402/settle":
					var request struct {
						PaymentPayload      x402.PaymentPayload
						PaymentRequirements x402.PaymentRequirements
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					declaration := x402.ReadPaymentIdentifier(request.PaymentPayload.Extensions[x402.PaymentIdentifier])
					if declaration == nil {
						t.Error("missing identifier")
						w.WriteHeader(400)
						return
					}
					var id string
					json.Unmarshal(declaration.Info["id"], &id)
					if r.URL.Path == "/v1/x402/verify" {
						identifier = id
						events = append(events, "verify")
						json.NewEncoder(w).Encode(x402.VerifyResponse{IsValid: scenario.verify})
					} else {
						if id != identifier {
							t.Error("identifier changed")
						}
						events = append(events, "settle")
						if scenario.amount != "" && request.PaymentRequirements.Amount != scenario.amount {
							t.Errorf("settled %s, want %s", request.PaymentRequirements.Amount, scenario.amount)
						}
						if scenario.amount != "" && request.PaymentPayload.Accepted.Amount != "1000000" {
							t.Error("signed ceiling changed")
						}
						json.NewEncoder(w).Encode(x402.SettleResponse{Success: scenario.settle, Transaction: "transaction", Network: foundation.Network(request.PaymentRequirements.Network)})
					}
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer api.Close()
			options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
			client, _ := New(options)
			facilitator, _ := NewFacilitator(options)
			offers, err := client.Accepts(context.Background(), AcceptsOptions{Price: PriceSpec{Amount: "$1"}, Schemes: []string{scenario.scheme}})
			if err != nil {
				t.Fatal(err)
			}
			registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{scenario.scheme}, MeteredScheme: upto.NewUptoEvmScheme()})
			if err != nil {
				t.Fatal(err)
			}
			routes := xhttp.RoutesConfig{"GET /paid": {Accepts: offers}}
			server := xhttp.Newx402HTTPResourceServer(routes, foundation.WithFacilitatorClient(facilitator))
			for _, registration := range registrations {
				server.Register(registration.Network, registration.Server)
			}
			if err := server.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			handler := nethttp.PaymentMiddlewareFromHTTPServer(server, nethttp.WithSyncFacilitatorOnStart(false))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				events = append(events, "handler")
				mu.Unlock()
				if scenario.panicHandler {
					panic("handler failed")
				}
				if scenario.amount != "" {
					nethttp.SetSettlementOverrides(w, &foundation.SettlementOverrides{Amount: scenario.amount})
				}
				w.WriteHeader(scenario.handlerStatus)
				w.Write([]byte("protected-content"))
			}))
			unpaid := httptest.NewRecorder()
			handler.ServeHTTP(unpaid, httptest.NewRequest("GET", "http://seller.example/paid", nil))
			if unpaid.Code != 402 || unpaid.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unpaid %d %v", unpaid.Code, unpaid.Header())
			}
			raw, err := base64.StdEncoding.DecodeString(unpaid.Header().Get(x402.HeaderPaymentRequired))
			if err != nil {
				t.Fatal(err)
			}
			var required x402.PaymentRequired
			if err = json.Unmarshal(raw, &required); err != nil {
				t.Fatal(err)
			}
			if len(required.Accepts) != 1 {
				t.Fatal(required)
			}
			payment := x402.PaymentPayload{X402Version: 2, Accepted: required.Accepts[0], Payload: map[string]any{"signature": "synthetic-signature"}}
			request := httptest.NewRequest("GET", "http://seller.example/paid", nil)
			request.Header.Set(x402.HeaderPaymentSignature, base64.StdEncoding.EncodeToString(encode(t, payment)))
			response := httptest.NewRecorder()
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				handler.ServeHTTP(response, request)
			}()
			if panicked != scenario.panicHandler {
				t.Fatal("unexpected panic state")
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"verify"}
			if scenario.verify {
				want = append(want, "handler")
				if scenario.handlerStatus < 400 && !scenario.panicHandler {
					want = append(want, "settle")
				}
			}
			if strings.Join(events, ",") != strings.Join(want, ",") {
				t.Fatalf("events %v want %v", events, want)
			}
			if !scenario.panicHandler {
				if scenario.verify && scenario.settle && scenario.handlerStatus == 200 {
					if response.Code != 200 || response.Body.String() != "protected-content" || response.Header().Get(x402.HeaderPaymentResponse) == "" || !strings.Contains(response.Header().Get("Cache-Control"), "private") {
						t.Fatal(response)
					}
				} else if scenario.handlerStatus != 500 && (response.Code < 400 || strings.Contains(response.Body.String(), "protected-content")) {
					t.Fatal("released protected output", response)
				}
			}
		})
	}
}
