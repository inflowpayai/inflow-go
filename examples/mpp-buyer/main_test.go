package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
)

func TestRun(t *testing.T) {
	for _, scenario := range []string{"free", "receipt", "bad-receipt", "rejected", "truncated", "missing-key", "bad-key", "bad-target", "cancelled", "default-target"} {
		t.Run(scenario, func(t *testing.T) {
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "" {
					t.Error("platform key leaked")
				}
				if scenario == "rejected" {
					w.WriteHeader(402)
					return
				}
				if scenario == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				if scenario == "receipt" {
					encoded, err := mpp.EncodeReceipt(mpp.Receipt{Method: "inflow", Status: "success", Reference: "example-receipt", Timestamp: "2026-09-28T00:00:00Z"})
					if err != nil {
						t.Error(err)
					}
					w.Header().Set("Payment-Receipt", encoded)
				}
				if scenario == "bad-receipt" {
					w.Header().Set("Payment-Receipt", "not-base64!")
				}
				fmt.Fprint(w, "resource-content")
			}))
			defer resource.Close()
			env := map[string]string{"INFLOW_API_KEY": "test-only-key", "TARGET_URL": resource.URL}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "missing-key":
				delete(env, "INFLOW_API_KEY")
			case "bad-key":
				env["INFLOW_API_KEY"] = "bad key"
			case "bad-target":
				env["TARGET_URL"] = "://"
			case "cancelled":
				cancel()
			case "default-target":
				delete(env, "TARGET_URL")
				cancel()
			}
			var out bytes.Buffer
			err := run(ctx, func(k string) string { return env[k] }, &out)
			success := scenario == "free" || scenario == "receipt"
			if (err == nil) != success {
				t.Fatalf("error=%v output=%s", err, &out)
			}
			if success && !strings.Contains(out.String(), "resource-content") {
				t.Fatal(out.String())
			}
			if scenario == "receipt" && !strings.Contains(out.String(), "example-receipt") {
				t.Fatal(out.String())
			}
			if scenario == "free" && !strings.Contains(out.String(), "No payment receipt supplied") {
				t.Fatal(out.String())
			}
			if scenario == "bad-receipt" && !strings.Contains(err.Error(), "cannot read the seller") {
				t.Fatal(err)
			}
		})
	}
	// A non-402 error must be reported without attempting a payment.
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer resource.Close()
	env := map[string]string{"INFLOW_API_KEY": "test-only-key", "TARGET_URL": resource.URL}
	if err := run(context.Background(), func(k string) string { return env[k] }, io.Discard); err == nil {
		t.Fatal("accepted forbidden response")
	}
}

func TestPaidRequest(t *testing.T) {
	data, err := os.ReadFile("../../mpp/buyer/testdata/buyer.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input struct {
			Challenge   mpp.Challenge
			Requirement json.RawMessage
		}
		Platform struct {
			Exchanges []struct {
				Request  struct{ Method, Path string }
				Response struct {
					Status int
					JSON   json.RawMessage
				}
			}
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	fixture := cases[0]
	var calls atomic.Int32
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(fixture.Platform.Exchanges) {
			t.Error("extra platform request")
			w.WriteHeader(400)
			return
		}
		exchange := fixture.Platform.Exchanges[index]
		if r.URL.Path != exchange.Request.Path || r.Method != exchange.Request.Method || r.Header.Get("X-API-Key") != "test-only-key" {
			t.Error("unexpected platform request", r.Method, r.URL)
		}
		w.WriteHeader(exchange.Response.Status)
		w.Write(exchange.Response.JSON)
	}))
	defer platform.Close()
	var resources atomic.Int32
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("platform key leaked")
		}
		if resources.Add(1) == 1 {
			header, err := mpp.RenderChallenge(fixture.Input.Challenge)
			if err != nil {
				t.Error(err)
			}
			w.Header().Set("WWW-Authenticate", header)
			w.WriteHeader(402)
			return
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing payment")
		}
		fmt.Fprint(w, "paid-content")
	}))
	defer resource.Close()
	env := map[string]string{"INFLOW_API_KEY": "test-only-key", "INFLOW_BASE_URL": platform.URL, "TARGET_URL": resource.URL}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	if resources.Load() != 2 || calls.Load() != int32(len(fixture.Platform.Exchanges)) || !strings.Contains(out.String(), "paid-content") {
		t.Fatal(calls.Load(), resources.Load(), out.String())
	}
}

func TestCardPayment(t *testing.T) {
	request, _ := mpp.Encode(mpp.CardRequest{Amount: "125", Currency: "usd", Recipient: "seller", MethodDetails: mpp.CardMethodDetails{
		AcceptedNetworks: []string{"visa"}, MerchantName: "Example", EncryptionJWK: mpp.CardEncryptionKey{Kty: "RSA", Alg: "RSA-OAEP-256", Use: "enc", Kid: "test", N: "dGVzdA", E: "AQAB"},
	}})
	challenge := mpp.Challenge{ID: "test", Realm: "example", Method: "card", Intent: "charge", Request: request}
	credential, _ := mpp.EncodeCredential(mpp.Credential{Challenge: challenge, Payload: map[string]any{"encryptedPayload": "opaque", "network": "visa", "panLastFour": "1234", "panExpirationMonth": "12", "panExpirationYear": "2030"}})
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Options struct {
				Merchant     struct{ Name, URL, CountryCode string }
				InstrumentID string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Options.Merchant.Name != "Example" || body.Options.Merchant.URL != "https://example.com" || body.Options.Merchant.CountryCode != "US" || body.Options.InstrumentID != "22222222-2222-4222-8222-222222222222" {
			t.Error("missing CARD context")
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential})
	}))
	defer platform.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			header, _ := mpp.RenderChallenge(challenge)
			w.Header().Set("WWW-Authenticate", header)
			w.WriteHeader(402)
			return
		}
		if r.Header.Get("Authorization") != "Payment "+credential {
			t.Error("changed credential")
		}
		fmt.Fprint(w, "card-paid")
	}))
	defer resource.Close()
	env := map[string]string{"INFLOW_API_KEY": "test-only-key", "INFLOW_BASE_URL": platform.URL, "TARGET_URL": resource.URL, "MPP_PAYMENT_METHOD": "card", "MERCHANT_NAME": "Example", "MERCHANT_URL": "https://example.com", "MERCHANT_COUNTRY_CODE": "US", "INSTRUMENT_ID": "22222222-2222-4222-8222-222222222222"}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "card-paid") {
		t.Fatal(out.String())
	}
}
