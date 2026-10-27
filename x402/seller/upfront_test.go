package seller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	mppseller "github.com/inflowpayai/inflow-go/mpp/seller"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

func TestPaymentFlowConfiguration(t *testing.T) {
	for _, tc := range []struct {
		scheme, network, method string
		allowed                 bool
	}{
		{"exact", "eip155:8453", "", true},
		{"exact", "eip155:8453", "eip3009", true},
		{"exact", "eip155:8453", "permit2", true},
		{"exact", "eip155:8453", "other", false},
		{"exact", "solana:main", "", true},
		{"exact", "solana:main", "solana", true},
		{"exact", "solana:main", "permit2", false},
		{"exact", "other:1", "", false},
		{"balance", "inflow:1", "", true},
		{"instrument", "inflow:1", "default", true},
		{"balance", "inflow:2", "", false},
		{"balance", "inflow:1", "other", false},
		{"upto", "eip155:8453", "permit2", false},
		{"other", "inflow:1", "", false},
	} {
		t.Run(tc.scheme+"/"+tc.network+"/"+tc.method, func(t *testing.T) {
			config := sampleConfig()
			config.Wallets = nil
			config.Assets = nil
			config.PaymentMethods = []x402.PaymentMethodInfo{{Scheme: tc.scheme, Network: tc.network, Decimals: 18, Extra: map[string]any{}}}
			if tc.method != "" {
				config.PaymentMethods[0].Extra["assetTransferMethod"] = tc.method
			}
			before := encode(t, config)
			client := configured(t, config)
			options := AcceptsOptions{Price: PriceSpec{Amount: "1 USDC"}, Schemes: []string{tc.scheme}}
			if tc.scheme == "instrument" {
				options.Price.Amount = "$1"
			}
			for _, flow := range []foundation.PaymentFlowName{"", foundation.PaymentFlowAuthorization, foundation.PaymentFlowUpfront} {
				options.PaymentFlow = flow
				offers, err := client.Accepts(context.Background(), options)
				if flow == foundation.PaymentFlowUpfront && !tc.allowed {
					if err == nil {
						t.Fatal("unsupported upfront offer accepted")
					}
					continue
				}
				if err != nil || len(offers) != 1 {
					t.Fatal(offers, err)
				}
				if flow == foundation.PaymentFlowUpfront {
					if offers[0].Extra["paymentFlow"] != "upfront" {
						t.Fatal(offers)
					}
				} else if _, ok := offers[0].Extra["paymentFlow"]; ok {
					t.Fatal("authorization changed wire default")
				}
			}
			// Use the real registration path; metered schemes have a separate upstream server.
			if tc.scheme != "upto" {
				registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{tc.scheme}})
				if err != nil || len(registrations) != 1 {
					t.Fatal(registrations, err)
				}
				for _, flow := range registrations[0].Server.PaymentFlows() {
					if flow.Default != foundation.PaymentFlowAuthorization || slices.Contains(flow.Supported, foundation.PaymentFlowUpfront) != tc.allowed {
						t.Fatal(flow)
					}
				}
			}
			sameJSON(t, encode(t, config), before)
		})
	}
	config := sampleConfig()
	before := encode(t, config)
	for _, flow := range []foundation.PaymentFlowName{"escrow", "other"} {
		if _, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: "$1"}, PaymentFlow: flow}, ""); err == nil {
			t.Fatal("unknown flow accepted")
		}
	}
	for _, transfer := range []string{"", "permit2"} {
		offers, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: "$1"}, PaymentFlow: foundation.PaymentFlowUpfront}, transfer)
		if err != nil || len(offers) != 2 {
			t.Fatal(offers, err)
		}
		for _, offer := range offers {
			if offer.Extra["paymentFlow"] != "upfront" {
				t.Fatal(offer)
			}
		}
	}
	if _, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: "$1"}, PaymentFlow: foundation.PaymentFlowUpfront, Schemes: []string{"exact", "upto"}}, ""); err == nil {
		t.Fatal("mixed supported/unsupported offers accepted")
	}
	sameJSON(t, encode(t, config), before)
}

// This fixture replaces only the platform HTTP endpoints. Offer construction,
// matching, execution order, receipt handling and retries run through real SDK code.
func upfrontMiddleware(t *testing.T, scheme, network string, flow foundation.PaymentFlowName, settle http.HandlerFunc, protected http.HandlerFunc) (http.Handler, x402.PaymentPayload) {
	t.Helper()
	config := sampleConfig()
	if scheme == "instrument" {
		config.PaymentMethods = append(config.PaymentMethods, x402.PaymentMethodInfo{Scheme: scheme, Network: network, PayTo: "seller", Decimals: 18})
	}
	if strings.HasPrefix(network, "solana:") {
		config.Wallets = []x402.WalletInfo{{Blockchain: "SOLANA", Address: "recipient"}}
		config.Assets = []x402.AssetInfo{{Blockchain: "SOLANA", Network: network, AssetID: "USDC", AssetName: "USDC", Currency: "USDC", Decimals: 6, AssetTransferMethod: "solana"}}
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "key" {
			t.Error("missing Seller credential")
		}
		switch r.URL.Path {
		case "/v1/x402/config":
			json.NewEncoder(w).Encode(config)
		case "/v1/x402/supported":
			json.NewEncoder(w).Encode(x402.SupportedResponse{Kinds: []x402.SupportedKind{{X402Version: 2, Scheme: scheme, Network: network}}})
		case "/v1/x402/verify", "/v1/x402/settle":
			settle(w, r)
		default:
			t.Error("unexpected platform call", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(api.Close)
	options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	facilitator, err := NewFacilitator(options)
	if err != nil {
		t.Fatal(err)
	}
	route, err := client.Route(context.Background(), RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "$1"}, Schemes: []string{scheme}, Networks: []string{network}, PaymentFlow: flow}})
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{scheme}})
	if err != nil {
		t.Fatal(err)
	}
	resource := xhttp.Newx402HTTPResourceServer(xhttp.RoutesConfig{"GET /paid": route}, foundation.WithFacilitatorClient(facilitator))
	for _, registration := range registrations {
		resource.Register(registration.Network, registration.Server)
	}
	if err := resource.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := nethttp.PaymentMiddlewareFromHTTPServer(resource, nethttp.WithSyncFacilitatorOnStart(false))(protected)
	unpaid := httptest.NewRecorder()
	handler.ServeHTTP(unpaid, httptest.NewRequest("GET", "http://seller.example/paid", nil))
	if unpaid.Code != 402 {
		t.Fatal("missing payment did not return 402", unpaid)
	}
	raw, err := base64.StdEncoding.DecodeString(unpaid.Header().Get(x402.HeaderPaymentRequired))
	if err != nil {
		t.Fatal(err)
	}
	var required x402.PaymentRequired
	if err := json.Unmarshal(raw, &required); err != nil || len(required.Accepts) != 1 {
		t.Fatal(required, err)
	}
	return handler, x402.PaymentPayload{X402Version: 2, Accepted: required.Accepts[0], Payload: map[string]any{"signature": "synthetic-signature"}}
}

func paidRequest(t *testing.T, payment x402.PaymentPayload) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "http://seller.example/paid", nil)
	r.Header.Set(x402.HeaderPaymentSignature, base64.StdEncoding.EncodeToString(encode(t, payment)))
	return r
}

func TestUpfrontMiddlewareOrderAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		status       int
		panicHandler bool
		attempts     int
		runs         bool
	}{
		{"success", "success", 200, false, 1, true},
		{"handler-error", "success", 500, false, 1, true},
		{"handler-panic", "success", 200, true, 1, true},
		{"rejected", "rejected", 200, false, 1, false},
		{"pending", "pending", 200, false, 5, false},
		{"pending-resolved", "pending-resolved", 200, false, 2, true},
		{"uncertain", "disconnect", 200, false, 1, false},
		{"invalid-response", "invalid", 200, false, 1, false},
		{"missing-success", "empty", 200, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			events := []string{}
			var first []byte
			attempts := 0
			handler, payment := upfrontMiddleware(t, "balance", "inflow:1", foundation.PaymentFlowUpfront, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path != "/v1/x402/settle" {
					t.Error("upfront called verify")
				}
				events = append(events, "settle")
				attempts++
				body, _ := io.ReadAll(r.Body)
				if first == nil {
					first = body
				} else {
					sameJSON(t, body, first)
				}
				var input struct {
					PaymentPayload      x402.PaymentPayload
					PaymentRequirements x402.PaymentRequirements
				}
				if err := json.Unmarshal(body, &input); err != nil {
					t.Error(err)
				}
				if input.PaymentRequirements.Extra["paymentFlow"] != "upfront" || input.PaymentPayload.Accepted.Extra["paymentFlow"] != "upfront" || x402.ReadPaymentIdentifier(input.PaymentPayload.Extensions[x402.PaymentIdentifier]) == nil {
					t.Error("lost flow or payment identifier")
				}
				switch tc.result {
				case "pending", "pending-resolved":
					if tc.result == "pending" || attempts == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(409)
						io.WriteString(w, `{"errorReason":"idempotency_pending"}`)
						return
					}
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				case "invalid":
					io.WriteString(w, `{`)
					return
				case "empty":
					io.WriteString(w, `{}`)
					return
				}
				result := x402.SettleResponse{Success: tc.result != "rejected", Transaction: "transaction", Network: "inflow:1"}
				if !result.Success {
					result.ErrorReason = "rejected"
				}
				json.NewEncoder(w).Encode(result)
			}, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				events = append(events, "handler")
				mu.Unlock()
				if tc.panicHandler {
					panic("application failure after payment")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, "protected-content")
			})
			response := httptest.NewRecorder()
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				handler.ServeHTTP(response, paidRequest(t, payment))
			}()
			if panicked != tc.panicHandler {
				t.Fatal("unexpected panic", panicked)
			}
			mu.Lock()
			defer mu.Unlock()
			want := make([]string, tc.attempts)
			for i := range want {
				want[i] = "settle"
			}
			if tc.runs {
				want = append(want, "handler")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v, want %v", events, want)
			}
			if tc.runs {
				raw, err := base64.StdEncoding.DecodeString(response.Header().Get(x402.HeaderPaymentResponse))
				if err != nil {
					t.Fatal(err)
				}
				var receipt x402.SettleResponse
				if err := json.Unmarshal(raw, &receipt); err != nil || !receipt.Success || receipt.Transaction != "transaction" {
					t.Fatal("lost successful settlement receipt", receipt, err)
				}
				if !tc.panicHandler && response.Code != tc.status {
					t.Fatal(response)
				}
			} else if response.Code < 400 || strings.Contains(response.Body.String(), "protected-content") {
				t.Fatal("released unpaid content", response)
			}
		})
	}
}

func TestUpfrontWaitsForSettlement(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "rejection"}[success], func(t *testing.T) {
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var calls atomic.Int32
			handler, payment := upfrontMiddleware(t, "exact", "eip155:8453", foundation.PaymentFlowUpfront, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				json.NewEncoder(w).Encode(x402.SettleResponse{Success: success, Transaction: "transaction", Network: "eip155:8453"})
			}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "content") })
			t.Cleanup(unblock)
			response := httptest.NewRecorder()
			request := paidRequest(t, payment)
			go func() { defer close(done); handler.ServeHTTP(response, request) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("settlement did not start")
			}
			if calls.Load() != 0 {
				t.Fatal("handler ran before settlement completed")
			}
			select {
			case <-done:
				t.Fatal("request completed before settlement")
			default:
			}
			unblock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not complete")
			}
			if (calls.Load() == 1) != success {
				t.Fatal("incorrect handler execution", calls.Load())
			}
		})
	}
}

func TestMiddlewareFlowDefaultsAndMatching(t *testing.T) {
	for _, flow := range []foundation.PaymentFlowName{"", foundation.PaymentFlowAuthorization, foundation.PaymentFlowUpfront} {
		for _, scheme := range []string{"balance", "instrument", "exact", "solana"} {
			t.Run(string(flow)+"/"+scheme, func(t *testing.T) {
				network := "inflow:1"
				actualScheme := scheme
				if scheme == "exact" {
					network = "eip155:8453"
				}
				if scheme == "solana" {
					actualScheme, network = "exact", "solana:main"
				}
				var mu sync.Mutex
				events := []string{}
				handler, payment := upfrontMiddleware(t, actualScheme, network, flow, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if strings.HasSuffix(r.URL.Path, "/verify") {
						events = append(events, "verify")
						json.NewEncoder(w).Encode(x402.VerifyResponse{IsValid: true})
					} else {
						events = append(events, "settle")
						json.NewEncoder(w).Encode(x402.SettleResponse{Success: true, Transaction: "transaction", Network: foundation.Network(network)})
					}
				}, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					events = append(events, "handler")
					mu.Unlock()
					if r.Header.Get("Authorization") != "Bearer application" {
						t.Error("lost application authentication")
					}
					io.WriteString(w, "protected-content")
				})
				for _, change := range []string{"amount", "flow", "missing-flow", "malformed"} {
					if (change == "missing-flow" || change == "flow") && flow != foundation.PaymentFlowUpfront {
						continue
					}
					var altered x402.PaymentPayload
					if err := json.Unmarshal(encode(t, payment), &altered); err != nil {
						t.Fatal(err)
					}
					switch change {
					case "amount":
						altered.Accepted.Amount = "1"
					case "flow":
						altered.Accepted.Extra["paymentFlow"] = "escrow"
					case "missing-flow":
						delete(altered.Accepted.Extra, "paymentFlow")
					}
					r := paidRequest(t, altered)
					if change == "malformed" {
						r.Header.Set(x402.HeaderPaymentSignature, "invalid")
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, r)
					if response.Code < 400 {
						t.Fatal("mismatched payment accepted", change)
					}
				}
				mu.Lock()
				if len(events) != 0 {
					t.Fatal("invalid payment reached facilitator/handler", events)
				}
				mu.Unlock()
				request := paidRequest(t, payment)
				request.Header.Set("Authorization", "Bearer application")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want := []string{"verify", "handler", "settle"}
				if flow == foundation.PaymentFlowUpfront {
					want = []string{"settle", "handler"}
				}
				mu.Lock()
				defer mu.Unlock()
				if !reflect.DeepEqual(events, want) || response.Code != 200 || response.Header().Get(x402.HeaderPaymentResponse) == "" || !strings.Contains(response.Header().Get("Cache-Control"), "private") {
					t.Fatal(events, response)
				}
			})
		}
	}
}

func TestUpfrontRepeatedAndConcurrentPayments(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var first []byte
	var calls atomic.Int32
	handler, payment := upfrontMiddleware(t, "balance", "inflow:1", foundation.PaymentFlowUpfront, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/v1/x402/settle" {
			t.Error("unexpected verification")
		}
		body, _ := io.ReadAll(r.Body)
		if first == nil {
			first = body
		} else {
			sameJSON(t, body, first)
		}
		requests++
		// The platform returns the original receipt for a repeated payment.
		json.NewEncoder(w).Encode(x402.SettleResponse{Success: true, Transaction: "original-transaction", Network: "inflow:1"})
	}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	payment.Extensions = map[string]any{x402.PaymentIdentifier: x402.PaymentIdentifierEntry(x402.DeclarePaymentIdentifier(), "pay_12345678901234567890123456789012")}
	before := encode(t, payment)
	var wg sync.WaitGroup
	for range 3 {
		request := paidRequest(t, payment)
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 500 || response.Header().Get(x402.HeaderPaymentResponse) == "" {
				t.Error(response)
			}
		}()
	}
	wg.Wait()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, paidRequest(t, payment))
	mu.Lock()
	defer mu.Unlock()
	if requests != 4 || calls.Load() != 4 {
		t.Fatal("payment replay must not be confused with fulfillment deduplication", requests, calls.Load())
	}
	var forwarded struct{ PaymentPayload x402.PaymentPayload }
	if err := json.Unmarshal(first, &forwarded); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, encode(t, forwarded.PaymentPayload.Extensions[x402.PaymentIdentifier]), encode(t, payment.Extensions[x402.PaymentIdentifier]))
	sameJSON(t, encode(t, payment), before)
}

func TestUpfrontBuyerHTTPRoundTrip(t *testing.T) {
	for _, status := range []int{200, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var settlements, handlers, purchases atomic.Int32
			handler, _ := upfrontMiddleware(t, "balance", "inflow:1", foundation.PaymentFlowUpfront, func(w http.ResponseWriter, r *http.Request) {
				settlements.Add(1)
				if r.URL.Path != "/v1/x402/settle" {
					t.Error("upfront verification")
				}
				json.NewEncoder(w).Encode(x402.SettleResponse{Success: true, Transaction: "transaction", Network: "inflow:1"})
			}, func(w http.ResponseWriter, r *http.Request) {
				handlers.Add(1)
				if r.Header.Get("Authorization") != "Bearer application" || r.Header.Get("X-API-Key") != "" {
					t.Error("application authentication lost or platform key leaked")
				}
				w.WriteHeader(status)
				io.WriteString(w, "result")
			})
			merchant := httptest.NewServer(handler)
			defer merchant.Close()
			var mu sync.Mutex
			var payment x402.PaymentPayload
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("X-API-Key") != "buyer-key" {
					t.Error("missing Buyer credential")
				}
				switch r.URL.Path {
				case "/v1/transactions/x402-supported":
					io.WriteString(w, `{"kinds":[{"scheme":"balance","network":"inflow:1","x402Version":2}]}`)
				case "/v1/transactions/x402":
					purchases.Add(1)
					var input struct {
						Accept          x402.PaymentRequirements
						Resource        *x402.ResourceInfo
						RemotePaymentID string
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Accept.Extra["paymentFlow"] != "upfront" || input.RemotePaymentID != "pay_12345678901234567890123456789012" {
						t.Error("Buyer lost advertised flow or identifier")
					}
					payment = x402.PaymentPayload{X402Version: 2, Accepted: input.Accept, Resource: input.Resource, Payload: map[string]any{"transactionId": "transaction"}, Extensions: map[string]any{x402.PaymentIdentifier: x402.PaymentIdentifierEntry(x402.DeclarePaymentIdentifier(), input.RemotePaymentID)}}
					io.WriteString(w, `{"transactionId":"transaction","approvalId":"approval","approvalStatus":"APPROVED"}`)
				case "/v1/transactions/transaction/x402":
					json.NewEncoder(w).Encode(map[string]any{"status": "PENDING", "paymentPayload": payment, "encodedPayload": base64.StdEncoding.EncodeToString(encode(t, payment))})
				default:
					t.Error("unexpected Buyer request", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer platform.Close()
			client, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL, APIKey: "buyer-key"}, PollInterval: time.Millisecond, WaitTimeout: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest("GET", merchant.URL+"/paid", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer application")
			response, err := client.Do(request, buyer.SignOptions{PaymentID: "pay_12345678901234567890123456789012"})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || response.Header.Get(x402.HeaderPaymentResponse) == "" || purchases.Load() != 1 || handlers.Load() != 1 || settlements.Load() != 1 {
				t.Fatal(response, purchases.Load(), handlers.Load(), settlements.Load())
			}
			if request.Header.Get(x402.HeaderPaymentSignature) != "" {
				t.Fatal("mutated original request")
			}
		})
	}
}

func TestMPPAndUpfrontAreAlternativeHandlers(t *testing.T) {
	var x402Settles, mppBroadcasts, served atomic.Int32
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1); io.WriteString(w, "paid") })
	xhandler, payment := upfrontMiddleware(t, "balance", "inflow:1", foundation.PaymentFlowUpfront, func(w http.ResponseWriter, r *http.Request) {
		x402Settles.Add(1)
		json.NewEncoder(w).Encode(x402.SettleResponse{Success: true, Transaction: "transaction", Network: "inflow:1"})
	}, protected)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mpp/config":
			io.WriteString(w, `{"sellerId":"11111111-1111-4111-8111-111111111111","featureFlags":{"idempotencyKeyEnabled":true},"supportedMethods":[{"id":"inflow","methodDetails":{"intentCurrencyRails":{"charge":{"USDC":[{"rail":"balance"}]}}}}]}`)
		case "/v1/mpp/validate":
			var input struct{ Credential mpp.Credential }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			request, err := mpp.Decode(input.Credential.Challenge.Request)
			if err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": input.Credential.Challenge, "credential": input.Credential, "details": map[string]any{}, "intent": input.Credential.Challenge.Intent, "method": input.Credential.Challenge.Method, "request": request, "source": input.Credential.Source})
		case "/v1/mpp/broadcast":
			mppBroadcasts.Add(1)
			io.WriteString(w, `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00Z"}}`)
		default:
			t.Error("unexpected MPP call", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer platform.Close()
	client, err := mppseller.New(inflow.Options{BaseURL: platform.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	mhandler, err := client.Protect(mppseller.Route{Realm: "seller.example", SecretKey: "test-key", Offers: []mppseller.Offer{{Charge: &mpp.ChargeRequest{Amount: "1", Currency: "USDC"}}}}, protected)
	if err != nil {
		t.Fatal(err)
	}
	// Application routing chooses one payment handler; the SDK has no combined-route wrapper.
	combined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Payment ") {
			mhandler.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(x402.HeaderPaymentSignature) != "" {
			xhandler.ServeHTTP(w, r)
			return
		}
		mResponse, xResponse := httptest.NewRecorder(), httptest.NewRecorder()
		mhandler.ServeHTTP(mResponse, r)
		xhandler.ServeHTTP(xResponse, r)
		w.Header().Set("WWW-Authenticate", mResponse.Header().Get("WWW-Authenticate"))
		w.Header().Set(x402.HeaderPaymentRequired, xResponse.Header().Get(x402.HeaderPaymentRequired))
		w.WriteHeader(402)
	})
	unpaid := httptest.NewRecorder()
	combined.ServeHTTP(unpaid, httptest.NewRequest("GET", "http://seller.example/paid", nil))
	challenges, err := mpp.ParseChallenges(unpaid.Header().Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 || unpaid.Header().Get(x402.HeaderPaymentRequired) == "" {
		t.Fatal(unpaid, err)
	}
	credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"transactionId": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://seller.example/paid", nil)
	r.Header.Set("Authorization", "Payment "+credential)
	response := httptest.NewRecorder()
	combined.ServeHTTP(response, r)
	if response.Code != 200 || response.Header().Get("Payment-Receipt") == "" || mppBroadcasts.Load() != 1 || x402Settles.Load() != 0 || served.Load() != 1 {
		t.Fatal(response, mppBroadcasts.Load(), x402Settles.Load(), served.Load())
	}
	response = httptest.NewRecorder()
	combined.ServeHTTP(response, paidRequest(t, payment))
	if response.Code != 200 || response.Header().Get(x402.HeaderPaymentResponse) == "" || mppBroadcasts.Load() != 1 || x402Settles.Load() != 1 || served.Load() != 2 {
		t.Fatal(response, mppBroadcasts.Load(), x402Settles.Load(), served.Load())
	}
}
