package seller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/extensions/bazaar"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

func TestRequestPricingLifecycle(t *testing.T) {
	var verified, settled, handled, resolved, supported atomic.Int32
	var configured, protectedHooks, verifyHooks atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/x402/config":
			json.NewEncoder(w).Encode(sampleConfig())
		case "/v1/x402/supported":
			supported.Add(1)
			fmt.Fprint(w, `{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"}]}`)
		case "/v1/x402/verify":
			verified.Add(1)
			fmt.Fprint(w, `{"isValid":true}`)
		case "/v1/x402/settle":
			settled.Add(1)
			var input struct{ PaymentRequirements x402.PaymentRequirements }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.PaymentRequirements.Amount != "1000000000000000000" {
				t.Error("wrong settlement amount", input.PaymentRequirements.Amount)
			}
			fmt.Fprint(w, `{"success":true,"transaction":"test","network":"inflow:1"}`)
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer api.Close()
	options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
	client, _ := New(options)
	facilitator, _ := NewFacilitator(options)
	registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{"balance"}})
	if err != nil {
		t.Fatal(err)
	}
	serverOptions := []foundation.ResourceServerOption{foundation.WithFacilitatorClient(facilitator)}
	for _, registration := range registrations {
		serverOptions = append(serverOptions, foundation.WithSchemeServer(registration.Network, registration.Server))
	}
	resource := foundation.Newx402ResourceServer(serverOptions...)
	if err := resource.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	supportCalls := supported.Load()
	var settlementCallbacks atomic.Int32
	server := RequestPricingServer{Options: serverOptions, Configure: func(resource *xhttp.HTTPServer) {
		configured.Add(1)
		resource.OnProtectedRequest(func(context.Context, xhttp.HTTPRequestContext, xhttp.RouteConfig) (*xhttp.ProtectedRequestHookResult, error) {
			protectedHooks.Add(1)
			return nil, nil
		})
		resource.OnBeforeVerify(func(foundation.VerifyContext) (*foundation.BeforeHookResult, error) {
			verifyHooks.Add(1)
			return nil, nil
		})
	}}
	handler, err := ProtectRequest(server, inflow.RequestPricingOptions{MaxBodyBytes: 8}, func(r *http.Request, body []byte) (xhttp.RouteConfig, error) {
		resolved.Add(1)
		price := r.URL.Query().Get("price")
		if string(body) == "large" {
			price = "2"
		}
		route, err := client.Route(r.Context(), RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: price + " USDC"}, Schemes: []string{"balance"}}})
		if err != nil {
			return route, err
		}
		route.Description = "bundle " + price
		route.Resource = "https://seller.example/bundle/" + price
		route.MimeType = "text/plain"
		route.ServiceName = "Widgets"
		route.Extensions = map[string]any{"test": map[string]any{"price": price}}
		return route, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		io.Copy(w, r.Body)
	}), nethttp.WithSettlementHandler(func(http.ResponseWriter, *http.Request, *foundation.SettleResponse) { settlementCallbacks.Add(1) }), nethttp.WithSyncFacilitatorOnStart(true))
	if err != nil {
		t.Fatal(err)
	}
	call := func(price, body, payment string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/paid?price="+price, strings.NewReader(body))
		if payment != "" {
			r.Header.Set(x402.HeaderPaymentSignature, payment)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	unpaid := call("1", "small", "")
	required := pricingRequirements(t, unpaid)
	if required.Resource == nil || required.Resource.Description != "bundle 1" || required.Resource.URL != "https://seller.example/bundle/1" || required.Extensions["test"] == nil {
		t.Fatal("lost route metadata", required)
	}
	payment := base64.StdEncoding.EncodeToString(encode(t, x402.PaymentPayload{X402Version: 2, Accepted: required.Accepts[0], Payload: map[string]any{"transactionId": "test"}}))
	changed := call("1", "large", payment)
	if changed.Code != 402 || verified.Load() != 0 || handled.Load() != 0 || settled.Load() != 0 {
		t.Fatal("stale price accepted", changed)
	}
	paid := call("1", "small", payment)
	if paid.Code != 200 || paid.Body.String() != "small" || paid.Header().Get(x402.HeaderPaymentResponse) == "" || handled.Load() != 1 || verified.Load() != 1 || settled.Load() != 1 || settlementCallbacks.Load() != 1 {
		t.Fatal(paid, handled.Load(), settled.Load(), settlementCallbacks.Load())
	}
	before := resolved.Load()
	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			price := fmt.Sprint(i + 1)
			requirements := pricingRequirements(t, call(price, "small", ""))
			if requirements.Accepts[0].Amount != price+"000000000000000000" || requirements.Resource.Description != "bundle "+price {
				t.Errorf("cross-request terms: %v", requirements)
			}
		})
		group.Go(func() {
			response := call("1", "small", payment)
			if response.Code != 200 || response.Header().Get(x402.HeaderPaymentResponse) == "" {
				t.Error("concurrent paid request failed", response)
			}
		})
	}
	group.Wait()
	if resolved.Load()-before != 40 || supported.Load() != supportCalls {
		t.Fatal("resolver or initialization repeated", resolved.Load(), supported.Load())
	}
	if configured.Load() != resolved.Load() || protectedHooks.Load() != resolved.Load() || verifyHooks.Load() != 21 || settlementCallbacks.Load() != 21 || settled.Load() != 21 || handled.Load() != 21 {
		t.Fatal("lost hooks or repeated processing", configured.Load(), protectedHooks.Load(), verifyHooks.Load(), settlementCallbacks.Load(), settled.Load(), handled.Load())
	}
}

func pricingRequirements(t *testing.T, response *httptest.ResponseRecorder) x402.PaymentRequired {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(response.Header().Get(x402.HeaderPaymentRequired))
	if err != nil {
		t.Fatal(err)
	}
	var required x402.PaymentRequired
	if err := json.Unmarshal(raw, &required); err != nil || response.Code != 402 || len(required.Accepts) != 1 {
		t.Fatalf("challenge: %d %s %v", response.Code, response.Body, err)
	}
	return required
}

func TestRequestPricingErrors(t *testing.T) {
	server := RequestPricingServer{}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") })
	if _, err := ProtectRequest(server, inflow.RequestPricingOptions{}, nil, nil); err == nil {
		t.Fatal("accepted nil handler")
	}
	for _, mode := range []string{"resolver", "empty", "unregistered"} {
		var reported error
		handler, err := ProtectRequest(server, inflow.RequestPricingOptions{OnError: func(_ *http.Request, err error) { reported = err }}, func(*http.Request, []byte) (xhttp.RouteConfig, error) {
			if mode == "resolver" {
				return xhttp.RouteConfig{}, errors.New("private")
			}
			if mode == "unregistered" {
				return xhttp.RouteConfig{Accepts: []xhttp.PaymentOption{{Scheme: "balance", Network: "inflow:1", Price: "1", PayTo: "seller"}}}, nil
			}
			return xhttp.RouteConfig{}, nil
		}, next)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
		if response.Code != 500 || reported == nil || response.Header().Get(x402.HeaderPaymentRequired) != "" || strings.Contains(response.Body.String(), "private") {
			t.Fatal(response, reported)
		}
	}
}

func TestRequestPricingCurrenciesAndSponsorship(t *testing.T) {
	config := sampleConfig()
	enabled, disabled := true, false
	config.Assets[0].SupportsEip2612 = &enabled
	second := config.Assets[0]
	second.AssetID, second.AssetName, second.Currency = "0x2222222222222222222222222222222222222222", "USDT", "USDT"
	second.SupportsEip2612 = &disabled
	config.Assets = append(config.Assets, second)
	config.PaymentMethods = append(config.PaymentMethods, x402.PaymentMethodInfo{Scheme: "instrument", Network: "inflow:1", PayTo: "seller", Decimals: 18})
	var unavailable atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/x402/config":
			json.NewEncoder(w).Encode(config)
		case "/v1/x402/supported":
			if unavailable.Load() {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, `{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"},{"x402Version":2,"scheme":"exact","network":"eip155:8453"},{"x402Version":2,"scheme":"instrument","network":"inflow:1"}],"extensions":["eip2612GasSponsoring"]}`)
		default:
			t.Error("unexpected payment call", r.URL.Path)
		}
	}))
	defer api.Close()
	options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
	client, _ := New(options)
	facilitator, _ := NewFacilitator(options)
	registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{"balance", "exact", "instrument"}})
	if err != nil {
		t.Fatal(err)
	}
	serverOptions := []foundation.ResourceServerOption{foundation.WithFacilitatorClient(facilitator)}
	for _, registration := range registrations {
		serverOptions = append(serverOptions, foundation.WithSchemeServer(registration.Network, registration.Server))
	}
	var reported error
	handler, err := ProtectRequest(RequestPricingServer{Options: serverOptions}, inflow.RequestPricingOptions{OnError: func(_ *http.Request, err error) { reported = err }}, func(r *http.Request, _ []byte) (xhttp.RouteConfig, error) {
		return client.Route(r.Context(), RouteOptions{
			AcceptsOptions:      AcceptsOptions{Price: PriceSpec{Amount: "1", Currency: r.URL.Query().Get("currency")}, Schemes: []string{"balance", "exact", "instrument"}},
			AssetTransferMethod: "permit2",
		})
	}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unpaid request reached handler") }))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		currency  string
		count     int
		sponsored bool
	}{
		{"USDC", 2, true}, {"USDT", 2, false}, {"USD", 5, false}, {"USDC", 2, true},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/paid?currency="+tc.currency, nil))
		data, err := base64.StdEncoding.DecodeString(response.Header().Get(x402.HeaderPaymentRequired))
		var required x402.PaymentRequired
		if err != nil || json.Unmarshal(data, &required) != nil || response.Code != 402 || len(required.Accepts) != tc.count {
			t.Fatalf("%s offers: %d %s %v", tc.currency, response.Code, response.Body, reported)
		}
		if (required.Extensions["eip2612GasSponsoring"] != nil) != tc.sponsored {
			t.Fatal("sponsorship leaked across currencies", tc.currency, required.Extensions)
		}
		for _, offer := range required.Accepts {
			if offer.Scheme == "exact" {
				if offer.Amount != "1000000" || offer.PayTo != config.Wallets[0].Address {
					t.Fatal("incorrect blockchain amount or recipient", offer)
				}
			} else if offer.Amount != "1000000000000000000" || offer.PayTo != "seller" || (offer.Scheme == "instrument" && offer.Asset != "USD") {
				t.Fatal("incorrect balance or instrument offer", offer)
			}
		}
	}
	// USDT needs no sponsorship lookup, so this failure happens during server initialization.
	facilitator.supported.expires = time.Time{}
	unavailable.Store(true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/paid?currency=USDT", nil))
	if response.Code != 500 || reported == nil || response.Header().Get(x402.HeaderPaymentRequired) != "" {
		t.Fatal("initialization failure did not fail closed", response, reported)
	}
}

func TestRequestPricingRoutePatterns(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/x402/config":
			json.NewEncoder(w).Encode(sampleConfig())
		case "/v1/x402/supported":
			fmt.Fprint(w, `{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"}]}`)
		default:
			t.Error("unexpected payment processing", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer api.Close()
	options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
	client, _ := New(options)
	facilitator, _ := NewFacilitator(options)
	registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{"balance"}})
	if err != nil {
		t.Fatal(err)
	}
	serverOptions := []foundation.ResourceServerOption{foundation.WithFacilitatorClient(facilitator)}
	for _, registration := range registrations {
		serverOptions = append(serverOptions, foundation.WithSchemeServer(registration.Network, registration.Server))
	}
	for _, tc := range []struct {
		name, pattern, method, path, hookPattern string
		discovery                                bool
		status                                   int
	}{
		{"default", "", "POST", "/anything", "*", false, 402},
		{"named", "GET /widgets/:id", "GET", "/widgets/123", "/widgets/:id", true, 402},
		{"encoded", "GET /widgets/:id", "GET", "/widgets/%31%32%33", "/widgets/:id", true, 402},
		{"wildcard-newline", "GET /widgets/*", "GET", "/widgets/a%0Ab", "/widgets/*", false, 402},
		{"wildcard-prefix", "GET /widgets/*", "GET", "/widgets", "/widgets/*", false, 402},
		{"missing-discovery-pattern", "", "GET", "/widgets/123", "", true, 500},
		{"blank-discovery-pattern", "  ", "GET", "/widgets/123", "", true, 500},
		{"wrong-path", "GET /widgets/:id", "GET", "/other/123", "", false, 404},
		{"extra-segment", "GET /widgets/:id", "GET", "/widgets/123/more", "", false, 404},
		{"wrong-method", "GET /widgets/:id", "POST", "/widgets/123", "", false, 404},
		{"head", "GET /widgets/:id", "HEAD", "/widgets/123", "/widgets/:id", false, 402},
		{"default-options", "", "OPTIONS", "/widgets/123", "", false, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var observedPattern string
			var reported error
			handler, err := ProtectRequest(RequestPricingServer{
				RoutePattern: tc.pattern, Options: serverOptions,
				Configure: func(server *xhttp.HTTPServer) {
					server.OnProtectedRequest(func(_ context.Context, request xhttp.HTTPRequestContext, _ xhttp.RouteConfig) (*xhttp.ProtectedRequestHookResult, error) {
						observedPattern = request.RoutePattern
						return nil, nil
					})
				},
			}, inflow.RequestPricingOptions{OnError: func(_ *http.Request, err error) { reported = err }}, func(r *http.Request, _ []byte) (xhttp.RouteConfig, error) {
				route, err := client.Route(r.Context(), RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "1 USDC"}, Schemes: []string{"balance"}}})
				if err != nil {
					return route, err
				}
				if tc.discovery {
					extension, err := bazaar.DeclareDiscoveryExtension(bazaar.MethodGET, map[string]interface{}{}, nil, "", nil)
					if err != nil {
						return route, err
					}
					route.Extensions = map[string]interface{}{"bazaar": extension}
				}
				return route, nil
			}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unpaid handler executed") }))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, "http://seller.example"+tc.path, nil))
			if response.Code != tc.status || observedPattern != tc.hookPattern || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response, observedPattern, reported)
			}
			if tc.status != 402 {
				if response.Header().Get(x402.HeaderPaymentRequired) != "" || (reported != nil) != (tc.status == 500) {
					t.Fatal("incorrect failure handling", response, reported)
				}
				return
			}
			required := pricingRequirements(t, response)
			if tc.discovery {
				var discovery struct {
					RouteTemplate string
					Info          struct {
						Input struct{ PathParams map[string]string }
					}
				}
				if err := json.Unmarshal(encode(t, required.Extensions["bazaar"]), &discovery); err != nil {
					t.Fatal(err)
				}
				if discovery.RouteTemplate != "/widgets/:id" || discovery.Info.Input.PathParams["id"] != "123" {
					t.Fatal("lost discovery route or parameters", discovery)
				}
			}
		})
	}
}
