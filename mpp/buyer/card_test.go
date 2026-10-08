package buyer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
)

func cardChallenge(t *testing.T) mpp.Challenge {
	t.Helper()
	request, err := mpp.Encode(mpp.CardRequest{Amount: "125", Currency: "usd", Recipient: "seller", MethodDetails: mpp.CardMethodDetails{
		AcceptedNetworks: []string{"visa"}, MerchantName: "Example", EncryptionJWK: mpp.CardEncryptionKey{Kty: "RSA", Alg: "RSA-OAEP-256", Use: "enc", Kid: "test", N: "dGVzdA", E: "AQAB"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return mpp.Challenge{ID: "card-test", Realm: "seller.example", Method: "card", Intent: "charge", Request: request, Description: new("Widget"), Opaque: new("route")}
}

func cardOptions() buyer.PaymentOptions {
	return buyer.PaymentOptions{Merchant: &buyer.CardMerchant{Name: "Example", URL: "https://seller.example", CountryCode: "US"}, InstrumentID: transactionID}
}

func cardCredential(t *testing.T, c mpp.Challenge) string {
	t.Helper()
	value, err := mpp.EncodeCredential(mpp.Credential{Challenge: c, Source: new("did:example:buyer"), Payload: map[string]any{
		"encryptedPayload": "opaque encrypted data", "network": "visa", "panLastFour": "1234", "panExpirationMonth": "12", "panExpirationYear": "2030",
		"billingAddress": map[string]any{"zip": "12345", "extension": true}, "extension": json.Number("9007199254740993"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCardHTTP(t *testing.T) {
	for _, bearer := range []bool{false, true} {
		t.Run(fmt.Sprint(bearer), func(t *testing.T) {
			challenge := cardChallenge(t)
			encoded := cardCredential(t, challenge)
			calls, resourceCalls := 0, 0
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if bearer {
					if r.Header.Get("Authorization") != "Bearer test-only-token" {
						t.Error("missing bearer")
					}
				} else if r.Header.Get("X-API-Key") != "test-only-buyer-key" {
					t.Error("missing API key")
				}
				switch r.URL.Path {
				case "/v1/transactions/mpp":
					var body struct {
						Challenge mpp.Challenge
						Options   buyer.PaymentOptions
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(body.Challenge, challenge) || !reflect.DeepEqual(body.Options, cardOptions()) {
						t.Errorf("changed create body: %+v", body)
					}
					json.NewEncoder(w).Encode(map[string]any{"state": "pending", "transactionId": transactionID, "approvalId": approvalID, "retryAfterSeconds": 0})
				case "/v1/transactions/" + transactionID + "/mpp":
					json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": encoded})
				default:
					t.Error("unexpected platform request", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer platform.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resourceCalls++
				if r.Header.Get("X-API-Key") != "" || strings.Contains(r.Header.Get("Authorization"), "test-only") {
					t.Error("Buyer authentication leaked")
				}
				if resourceCalls == 1 {
					// Skip a malformed CARD offer and an unrelated method before selecting CARD.
					invalid := challenge
					invalid.Request = "e30"
					other := challenge
					other.Method = "inflow"
					for _, c := range []mpp.Challenge{invalid, other, challenge} {
						h, err := mpp.RenderChallenge(c)
						if err != nil {
							t.Fatal(err)
						}
						w.Header().Add("WWW-Authenticate", h)
					}
					w.WriteHeader(402)
					return
				}
				if r.Header.Get("Authorization") != "Payment "+encoded {
					t.Error("credential changed")
				}
				fmt.Fprint(w, "paid resource")
			}))
			defer resource.Close()
			client := newClient(t, platform, func(o *buyer.Options) {
				if bearer {
					o.APIKey = ""
					o.AccessToken = func(context.Context) (string, error) { return "test-only-token", nil }
				}
			})
			req, _ := http.NewRequest("GET", resource.URL, nil)
			response, err := client.Do(req, cardOptions())
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 || calls != 2 || resourceCalls != 2 || req.Header.Get("Authorization") != "" {
				t.Fatal("incorrect lifecycle", calls, resourceCalls)
			}
		})
	}
}

func TestCardInvalidOptions(t *testing.T) {
	for _, mode := range []string{"missing", "name", "long-name", "url", "long-url", "scheme", "country", "letters", "instrument", "subscription", "request", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			c, options := cardChallenge(t), cardOptions()
			switch mode {
			case "missing":
				options.Merchant = nil
			case "name":
				options.Merchant.Name = " "
			case "long-name":
				options.Merchant.Name = strings.Repeat("x", 201)
			case "url":
				options.Merchant.URL = "%"
			case "long-url":
				options.Merchant.URL = "https://example.com/" + strings.Repeat("x", 2048)
			case "scheme":
				options.Merchant.URL = "ftp://example.com"
			case "country":
				options.Merchant.CountryCode = "USA"
			case "letters":
				options.Merchant.CountryCode = "1!"
			case "instrument":
				options.InstrumentID = "invalid"
			case "subscription":
				options.SubscriptionID = subscriptionID
			case "request":
				c.Request = "e30"
			case "oversized":
				c.Description = new(strings.Repeat("x", 50000))
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("invalid input reached platform")
				w.WriteHeader(500)
			}))
			defer s.Close()
			_, err := newClient(t, s, nil).Prepare(context.Background(), c, options)
			if err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}

func TestCardReturnedCredential(t *testing.T) {
	for _, mode := range []string{"success", "description", "opaque", "request", "id", "payload", "failed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			c := cardChallenge(t)
			returned := c
			switch mode {
			case "description":
				returned.Description = new("different")
			case "opaque":
				returned.Opaque = new("different")
			case "request":
				returned.Request = "e30"
			case "id":
				returned.ID = "other"
			}
			encoded := cardCredential(t, returned)
			if mode == "payload" {
				decoded, _ := mpp.DecodeCredential(encoded)
				decoded.Payload["network"] = "mastercard"
				encoded, _ = mpp.EncodeCredential(decoded)
			}
			cancelled := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					cancelled++
					fmt.Fprint(w, `{}`)
					return
				}
				state := "ready"
				if mode == "failed" || mode == "expired" {
					state = mode
				}
				json.NewEncoder(w).Encode(map[string]any{"state": state, "transactionId": transactionID, "approvalId": approvalID, "credential": encoded, "problem": map[string]any{"title": "Declined"}})
			}))
			defer s.Close()
			payment, err := newClient(t, s, nil).Prepare(context.Background(), c, cardOptions())
			if err != nil {
				t.Fatal(err)
			}
			*c.Description = "caller mutation"
			got, err := payment.Wait(context.Background())
			if mode == "success" {
				if err != nil || *got.Challenge.Description != "Widget" || cancelled != 0 {
					t.Fatal(got, err, cancelled)
				}
				return
			}
			code := buyer.InvalidCredential
			if mode == "failed" {
				code = buyer.Failed
			}
			if mode == "expired" {
				code = buyer.Expired
			}
			failure := requireCode(t, err, code)
			if (mode == "failed" || mode == "expired") && failure.TransactionID != transactionID {
				t.Fatal("lost transaction")
			}
			if cancelled != 1 {
				t.Fatal("approval not cancelled", cancelled)
			}
		})
	}
}
