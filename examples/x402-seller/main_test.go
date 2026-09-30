package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
)

func api(t *testing.T, scenario string) *httptest.Server {
	t.Helper()
	config := []byte(`{"sellerId":"seller","paymentMethods":[{"scheme":"balance","network":"inflow:1","payTo":"seller","decimals":18}]}`)
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
			if scenario == "precision" {
				fmt.Fprint(w, `{"paymentMethods":[{"scheme":"balance","network":"inflow:1","payTo":"seller","decimals":0}]}`)
				return
			}
			w.Write(config)
		case strings.HasSuffix(r.URL.Path, "/supported"):
			if scenario == "unsupported" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"},{"x402Version":2,"scheme":"exact","network":"eip155:8453"}]}`)
		case strings.HasSuffix(r.URL.Path, "/verify"):
			fmt.Fprint(w, `{"isValid":true}`)
		case strings.HasSuffix(r.URL.Path, "/settle"):
			fmt.Fprint(w, `{"success":true,"transaction":"test","network":"inflow:1"}`)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStartup(t *testing.T) {
	for _, scenario := range []string{"missing-key", "bad-key", "rejected", "empty", "listen", "default-listen", "precision", "unsupported"} {
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
				listener, err := net.Listen("tcp", "127.0.0.1:3001")
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

}

func TestRoutes(t *testing.T) {
	platform := api(t, "valid")
	handler, err := newHandler(context.Background(), inflow.Options{APIKey: "test-only-key", BaseURL: platform.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/widgets"} {
		unpaid := httptest.NewRecorder()
		handler.ServeHTTP(unpaid, httptest.NewRequest("GET", path, nil))
		if unpaid.Code != 402 {
			t.Fatalf("unpaid: %d %s", unpaid.Code, unpaid.Body)
		}
		data, err := base64.StdEncoding.DecodeString(unpaid.Header().Get(x402.HeaderPaymentRequired))
		if err != nil {
			t.Fatal(err)
		}
		var required x402.PaymentRequired
		if err := json.Unmarshal(data, &required); err != nil {
			t.Fatal(err)
		}
		if len(required.Accepts) == 0 {
			t.Fatal("missing offers")
		}
		payload := x402.PaymentPayload{X402Version: 2, Accepted: required.Accepts[0], Resource: required.Resource, Payload: map[string]any{"transactionId": "test"}}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set(x402.HeaderPaymentSignature, base64.StdEncoding.EncodeToString(encoded))
		paid := httptest.NewRecorder()
		handler.ServeHTTP(paid, request)
		if paid.Code != 200 || paid.Header().Get("PAYMENT-RESPONSE") == "" {
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
