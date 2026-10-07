package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
)

func api(t *testing.T, scenario string) *httptest.Server {
	t.Helper()
	data, err := os.ReadFile("../../mpp/seller/testdata/seller.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Platform struct {
			Exchanges []struct {
				Request  struct{ Path string }
				Response struct{ JSON json.RawMessage }
			}
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	var config json.RawMessage
	for _, c := range cases {
		for _, e := range c.Platform.Exchanges {
			if e.Request.Path == "/v1/mpp/config" && config == nil {
				config = e.Response.JSON
			}
		}
	}
	if config == nil {
		t.Fatal("configuration fixture missing")
	}
	if scenario == "unsupported-currency" {
		config = []byte(strings.ReplaceAll(string(config), "USDC", "EUR"))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-only-key" {
			t.Error("missing seller key")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			if scenario == "rejected" {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"errors":[{"code":"SELLER_ACCOUNT_REQUIRED","message":"Seller account required"}]}`)
				return
			}
			if scenario == "empty" {
				fmt.Fprint(w, "{}")
				return
			}

			w.Write(config)
		case strings.HasSuffix(r.URL.Path, "/validate"):
			var input struct{ Credential mpp.Credential }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			request, err := mpp.Decode(input.Credential.Challenge.Request)
			if err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": input.Credential.Challenge, "credential": input.Credential, "method": input.Credential.Challenge.Method, "intent": input.Credential.Challenge.Intent, "source": input.Credential.Source, "request": request})
		case strings.HasSuffix(r.URL.Path, "/broadcast"):
			fmt.Fprint(w, `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00Z"}}`)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStartup(t *testing.T) {
	for _, scenario := range []string{"missing-key", "bad-key", "rejected", "empty", "listen", "default-listen", "missing-secret", "unsupported-currency"} {
		t.Run(scenario, func(t *testing.T) {
			server := api(t, scenario)
			env := map[string]string{"INFLOW_API_KEY": "test-only-key", "INFLOW_BASE_URL": server.URL, "MPP_SECRET_KEY": "test-only-secret", "LISTEN_ADDR": "invalid::address"}
			switch scenario {
			case "missing-key":
				delete(env, "INFLOW_API_KEY")
			case "missing-secret":
				delete(env, "MPP_SECRET_KEY")
			case "bad-key":
				env["INFLOW_API_KEY"] = "bad key"
			case "default-listen":
				delete(env, "LISTEN_ADDR")
				listener, err := net.Listen("tcp", "127.0.0.1:3000")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			}
			if err := run(func(k string) string { return env[k] }); err == nil {
				t.Fatal("expected startup failure")
			}
		})
	}
	server := api(t, "valid")
	if _, err := newHandler(context.Background(), inflow.Options{APIKey: "test-only-key", BaseURL: server.URL}, "", ""); err == nil {
		t.Fatal("empty secret accepted")
	}
}

func TestRoutes(t *testing.T) {
	platform := api(t, "valid")
	handler, err := newHandler(context.Background(), inflow.Options{APIKey: "test-only-key", BaseURL: platform.URL}, "test-only-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/widgets", "/api/subscribe"} {
		unpaid := httptest.NewRecorder()
		handler.ServeHTTP(unpaid, httptest.NewRequest("GET", path, nil))
		if unpaid.Code != 402 {
			t.Fatalf("unpaid: %d %s", unpaid.Code, unpaid.Body)
		}
		challenges, err := mpp.ParseChallenges(unpaid.Header().Values("WWW-Authenticate"))
		if err != nil || len(challenges) != 1 {
			t.Fatal(err)
		}
		credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"transactionId": "test"}})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", "Payment "+credential)
		paid := httptest.NewRecorder()
		handler.ServeHTTP(paid, request)
		if paid.Code != 200 || paid.Header().Get("Payment-Receipt") == "" {
			t.Fatalf("paid: %d %s", paid.Code, paid.Body)
		}
	}
	for path, status := range map[string]int{"/free": 200, "/missing": 404} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != status {
			t.Fatal(path, response.Code)
		}
	}
}

func TestStripeRoute(t *testing.T) {
	var calls []string
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("X-Api-Key") != "test-only-key" {
			t.Error("missing Seller key")
		}
		if r.URL.Path == "/v1/mpp/config" {
			fmt.Fprint(w, `{"sellerId":"seller","supportedMethods":[{"id":"stripe","supportedCurrencies":["USD"],"supportedIntents":["charge"],"methodDetails":{"networkId":"profile_test","paymentMethodTypes":["card","link"]}}]}`)
			return
		}
		var body struct{ Credential mpp.Credential }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/v1/mpp/validate" {
			request, _ := mpp.Decode(body.Credential.Challenge.Request)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": body.Credential.Challenge, "credential": body.Credential, "request": request, "source": body.Credential.Source, "method": "stripe", "intent": "charge"})
			return
		}
		if r.URL.Path != "/v1/mpp/broadcast" {
			t.Error("unexpected call")
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: "stripe", Reference: "pi_test", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: &body.Credential.Challenge.ID}})
	}))
	defer platform.Close()
	options := inflow.Options{APIKey: "test-only-key", BaseURL: platform.URL}
	if _, err := newHandler(context.Background(), options, "test-secret", "unknown"); err == nil {
		t.Fatal("accepted unknown method")
	}
	calls = nil
	handler, err := newHandler(context.Background(), options, "test-secret", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest("GET", "http://seller.example/api/widgets", nil))
	challenges, err := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 {
		t.Fatalf("challenge: %v %v", challenges, err)
	}
	if challenges[0].Method != "stripe" {
		t.Fatal("wrong method")
	}
	encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"spt": "spt_test_only"}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://seller.example/api/widgets", nil)
	r.Header.Set("Authorization", "Payment "+encoded)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != 200 || response.Header().Get("Payment-Receipt") == "" || strings.Join(calls, ",") != "/v1/mpp/config,/v1/mpp/validate,/v1/mpp/broadcast" {
		t.Fatalf("response %d, calls %v", response.Code, calls)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest("GET", "http://seller.example/api/subscribe", nil))
	if missing.Code != 404 {
		t.Fatal("Stripe example exposes subscriptions")
	}
}

func TestCardRoute(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/mpp/config" {
			fmt.Fprint(w, `{"sellerId":"seller","supportedMethods":[{"id":"card","supportedCurrencies":["USD"],"supportedIntents":["charge"],"methodDetails":{"recipient":"seller","merchantName":"Example","acceptedNetworks":["visa"],"encryptionJwk":{"kty":"RSA","alg":"RSA-OAEP-256","use":"enc","kid":"test","n":"dGVzdA","e":"AQAB"}}}]}`)
			return
		}
		var body struct{ Credential mpp.Credential }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/v1/mpp/validate" {
			request, _ := mpp.Decode(body.Credential.Challenge.Request)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": body.Credential.Challenge, "credential": body.Credential, "request": request, "source": body.Credential.Source, "method": "card", "intent": "charge"})
			return
		}
		if r.URL.Path != "/v1/mpp/broadcast" {
			t.Error("unexpected endpoint")
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: "card", Reference: "test", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: &body.Credential.Challenge.ID}})
	}))
	defer platform.Close()
	handler, err := newHandler(context.Background(), inflow.Options{APIKey: "test-only-key", BaseURL: platform.URL}, "test-only-secret", "card")
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest("GET", "/api/widgets", nil))
	challenges, err := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 {
		t.Fatal(challenges, err)
	}
	request, err := mpp.DecodeCardRequest(challenges[0].Request)
	if err != nil || request.Amount != "125" {
		t.Fatal(request, err)
	}
	credential, _ := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"encryptedPayload": "opaque", "network": "visa", "panLastFour": "1234", "panExpirationMonth": "12", "panExpirationYear": "2030"}})
	r := httptest.NewRequest("GET", "/api/widgets", nil)
	r.Header.Set("Authorization", "Payment "+credential)
	paid := httptest.NewRecorder()
	handler.ServeHTTP(paid, r)
	if paid.Code != 200 || paid.Header().Get("Payment-Receipt") == "" {
		t.Fatal(paid.Code, paid.Body)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest("GET", "/api/subscribe", nil))
	if missing.Code != 404 {
		t.Fatal("CARD subscription exposed")
	}
}
