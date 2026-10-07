package seller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

const cardConfig = `{"sellerId":"seller","supportedMethods":[{"id":"card","supportedCurrencies":["USD"],"supportedIntents":["charge"],"methodDetails":{"recipient":"acct_test","merchantName":"Test merchant","acceptedNetworks":["visa"],"encryptionJwk":{"kty":"RSA","alg":"RSA-OAEP-256","use":"enc","kid":"test","n":"test_only","e":"AQAB"}}}]}`

func cardPayload() map[string]any {
	return map[string]any{"encryptedPayload": "opaque-test-only", "network": "visa", "panLastFour": "1234", "panExpirationMonth": "12", "panExpirationYear": "2030", "billingAddress": map[string]any{"zip": "94102"}, "extra": json.Number("9007199254740993")}
}

func TestCardPrepare(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, cardConfig) }))
	defer server.Close()
	client := clientFor(t, server)
	for _, test := range []struct{ amount, cents string }{{"1.25", "125"}, {"0.50", "50"}, {"999999.99", "99999999"}, {"0.49", ""}, {"1.001", ""}, {"1000000", ""}, {strings.Repeat("9", 40), ""}} {
		offer := seller.CardOffer{Amount: test.amount, ExternalID: new(""), Description: new("A report"), BillingRequired: new(false)}
		prepared, err := client.Prepare(context.Background(), seller.Offer{Card: &offer})
		if test.cents == "" {
			if err == nil {
				t.Fatal("accepted invalid price")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		request, err := mpp.DecodeCardRequest(prepared.Request)
		if err != nil || request.Amount != test.cents || request.MethodDetails.BillingRequired == nil || *request.MethodDetails.BillingRequired || request.ExternalID == nil || *request.ExternalID != "" {
			t.Fatal(request, err)
		}
	}
	if _, err := client.Prepare(context.Background(), seller.Offer{Card: &seller.CardOffer{Amount: "1", ExternalID: new(strings.Repeat("x", 256))}}); err == nil {
		t.Fatal("accepted long reference")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Prepare(ctx, seller.Offer{Card: &seller.CardOffer{Amount: "1"}}); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, config := range []string{strings.Replace(cardConfig, `"card"`, `"stripe"`, 1), strings.Replace(cardConfig, `"USD"`, `"EUR"`, 1), strings.Replace(cardConfig, `"charge"`, `"subscription"`, 1), strings.Replace(cardConfig, `"AQAB"`, `""`, 1)} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, config) }))
		_, err := clientFor(t, s).Prepare(context.Background(), seller.Offer{Card: &seller.CardOffer{Amount: "1"}})
		s.Close()
		if err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
}

func TestCardProtectedRoute(t *testing.T) {
	for _, mode := range []string{"success", "supplied-source", "signature", "expired", "route", "billing", "reference", "payload", "intent", "validation", "pending", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			calls, served := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/mpp/config" {
					fmt.Fprint(w, cardConfig)
					return
				}
				var body struct{ Credential mpp.Credential }
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Credential.Source == nil {
					t.Error("missing wire source")
				}
				if body.Credential.Challenge.Description == nil || *body.Credential.Challenge.Description != "displayed description" {
					t.Error("lost challenge description")
				}
				if r.URL.Path == "/v1/mpp/validate" {
					if mode == "validation" {
						fmt.Fprint(w, `{"success":false}`)
						return
					}
					request, _ := mpp.Decode(body.Credential.Challenge.Request)
					json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": body.Credential.Challenge, "credential": body.Credential, "method": "card", "intent": "charge", "source": body.Credential.Source, "request": request})
					return
				}
				if r.URL.Path != "/v1/mpp/broadcast" {
					t.Error("unexpected endpoint")
					w.WriteHeader(500)
					return
				}
				if mode == "pending" {
					fmt.Fprint(w, `{"problem":{"title":"Pending","status":503}}`)
					return
				}
				id := body.Credential.Challenge.ID
				if mode == "receipt" {
					id = "other"
				}
				json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: "card", Reference: "test", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: &id}})
			}))
			defer server.Close()
			client := clientFor(t, server)
			offer := seller.CardOffer{Amount: "1.25", BillingRequired: new(false), ExternalID: new("")}
			route := seller.Route{Realm: "seller.example", SecretKey: "test-only-key", Offers: []seller.Offer{{Card: &offer}}, Opaque: new("one")}
			if mode == "expired" {
				route.Lifetime = time.Nanosecond
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(200) })
			handler, err := client.Protect(route, next)
			if err != nil {
				t.Fatal(err)
			}
			first := httptest.NewRecorder()
			handler.ServeHTTP(first, httptest.NewRequest("GET", "http://seller.example/paid", nil))
			challenges, err := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
			if err != nil || len(challenges) != 1 {
				t.Fatal(challenges, err)
			}
			credential := mpp.Credential{Challenge: challenges[0], Payload: cardPayload()}
			// Description is a display field outside the challenge HMAC; it must still be forwarded.
			credential.Challenge.Description = new("displayed description")
			if mode == "supplied-source" {
				credential.Source = new("did:example:buyer")
			}
			switch mode {
			case "signature":
				credential.Challenge.ID = "forged"
			case "payload":
				credential.Payload["network"] = "mastercard"
			case "intent":
				credential.Challenge.Intent = "subscription"
			case "route":
				route.Opaque = new("two")
			case "billing":
				offer.BillingRequired = new(true)
			case "reference":
				offer.ExternalID = new("different")
			}
			if mode == "intent" {
				if _, err := client.Validate(context.Background(), credential); err == nil {
					t.Fatal("accepted subscription")
				}
			}
			if mode == "route" || mode == "billing" || mode == "reference" {
				handler, err = client.Protect(route, next)
				if err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := mpp.EncodeCredential(credential)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "http://seller.example/paid", nil)
			r.Header.Set("Authorization", "Payment "+encoded)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			success := mode == "success" || mode == "supplied-source"
			if success {
				if served != 1 || response.Code != 200 || response.Header().Get("Payment-Receipt") == "" || calls != 3 {
					t.Fatalf("status %d calls %d served %d", response.Code, calls, served)
				}
			} else {
				if served != 0 || response.Header().Get("Payment-Receipt") != "" {
					t.Fatal("released invalid payment")
				}
				want := 1
				if mode == "validation" {
					want = 2
				}
				if mode == "pending" || mode == "receipt" {
					want = 3
				}
				if calls != want {
					t.Fatalf("calls %d want %d", calls, want)
				}
			}
		})
	}
}
