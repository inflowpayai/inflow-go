package seller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

const stripeConfig = `{"sellerId":"seller","supportedMethods":[{"id":"stripe","supportedCurrencies":["USD"],"supportedIntents":["charge"],"methodDetails":{"networkId":"profile_test","paymentMethodTypes":["card","link"]}}]}`

func TestStripePrepare(t *testing.T) {
	for _, test := range []struct{ amount, cents string }{
		{"0.5", "50"}, {"0.50", "50"}, {"1", "100"}, {"1.25", "125"}, {"999999.99", "99999999"},
		{"", ""}, {"00.50", ""}, {"0.49", ""}, {"1.001", ""}, {"1000000", ""}, {strings.Repeat("9", 50), ""},
	} {
		t.Run(test.amount, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/mpp/config" || r.Header.Get("X-Api-Key") != "test-only-seller-key" {
					t.Error("wrong configuration request")
				}
				fmt.Fprint(w, stripeConfig)
			}))
			defer server.Close()
			offer := seller.StripeOffer{Amount: test.amount, ExternalID: new(""), Metadata: map[string]string{"purpose": "test"}, Description: new("A report"), Recipient: new("merchant")}
			before, _ := json.Marshal(offer)
			prepared, err := clientFor(t, server).Prepare(context.Background(), seller.Offer{Stripe: &offer})
			if test.cents == "" {
				if err == nil {
					t.Fatal("accepted invalid price")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := mpp.Decode(prepared.Request)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"amount": test.cents, "currency": "usd", "externalId": "", "description": "A report", "recipient": "merchant", "methodDetails": map[string]any{"networkId": "profile_test", "paymentMethodTypes": []any{"card", "link"}, "metadata": map[string]any{"purpose": "test"}}}
			if prepared.Method != "stripe" || prepared.Intent != "charge" || !reflect.DeepEqual(value, want) {
				t.Fatalf("prepared: %#v", value)
			}
			after, _ := json.Marshal(offer)
			if string(before) != string(after) {
				t.Fatal("mutated offer")
			}
		})
	}
}

func TestStripeConfigurationAndMetadata(t *testing.T) {
	for _, config := range []string{
		strings.ReplaceAll(stripeConfig, "stripe", "tempo"), strings.ReplaceAll(stripeConfig, "USD", "EUR"),
		strings.ReplaceAll(stripeConfig, "charge", "subscription"), strings.ReplaceAll(stripeConfig, "profile_test", " "),
		strings.ReplaceAll(stripeConfig, `["card","link"]`, `[]`), strings.ReplaceAll(stripeConfig, `"link"`, `" "`),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, config) }))
		_, err := clientFor(t, server).Prepare(context.Background(), seller.Offer{Stripe: &seller.StripeOffer{Amount: "1"}})
		server.Close()
		if err == nil {
			t.Fatal("accepted unsupported configuration")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, stripeConfig) }))
	defer server.Close()
	client := clientFor(t, server)
	tooMany := map[string]string{}
	for i := 0; i < 46; i++ {
		tooMany[fmt.Sprint(i)] = ""
	}
	for _, offer := range []seller.StripeOffer{
		{Amount: "1", ExternalID: new(strings.Repeat("x", 256))}, {Amount: "1", Metadata: tooMany},
		{Amount: "1", Metadata: map[string]string{" ": ""}}, {Amount: "1", Metadata: map[string]string{"x[y]": ""}},
		{Amount: "1", Metadata: map[string]string{strings.Repeat("x", 41): ""}}, {Amount: "1", Metadata: map[string]string{"externalId": ""}},
		{Amount: "1", Metadata: map[string]string{"key": strings.Repeat("x", 501)}},
		{Amount: "1", ExternalID: new(strings.Repeat("😀", 128))},
	} {
		if _, err := client.Prepare(context.Background(), seller.Offer{Stripe: &offer}); err == nil {
			t.Fatalf("accepted %#v", offer)
		}
	}
	if _, err := client.Prepare(context.Background(), seller.Offer{Stripe: &seller.StripeOffer{Amount: "1"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Prepare(ctx, seller.Offer{Stripe: &seller.StripeOffer{Amount: "1"}}); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestStripeCredentialRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid credential reached network")
		w.WriteHeader(500)
	}))
	defer server.Close()
	for _, test := range []struct {
		intent, request string
		payload         map[string]any
	}{
		{"subscription", "e30", map[string]any{"spt": "test"}},
		{"charge", "e30", map[string]any{"spt": 1}},
		{"charge", "e30", map[string]any{"spt": "test", "externalId": 1}},
		{"charge", "bad", map[string]any{"spt": "test"}},
		{"charge", "W10", map[string]any{"spt": "test"}},
		{"charge", "eyJleHRlcm5hbElkIjoiIn0", map[string]any{"spt": "test"}},
		{"charge", "eyJleHRlcm5hbElkIjoxfQ", map[string]any{"spt": "test", "externalId": "1"}},
	} {
		credential := mpp.Credential{Challenge: mpp.Challenge{ID: "test", Realm: "test", Method: "stripe", Intent: test.intent, Request: test.request}, Payload: test.payload}
		if _, err := clientFor(t, server).Validate(context.Background(), credential); err == nil {
			t.Fatal("accepted invalid credential")
		}
	}
}

func TestStripeProtectedRoute(t *testing.T) {
	for _, mode := range []string{"success", "source", "reference", "bad-signature", "expired", "wrong-route", "reference-mismatch", "rejected", "pending", "bad-receipt"} {
		t.Run(mode, func(t *testing.T) {
			var paths []string
			served := 0
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/v1/mpp/config" {
					fmt.Fprint(w, stripeConfig)
					return
				}
				var body struct{ Credential mpp.Credential }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				wantSource := ""
				if mode == "source" {
					wantSource = "did:example:buyer"
				}
				if body.Credential.Source == nil || *body.Credential.Source != wantSource || body.Credential.Payload["spt"] != "spt_test_only" {
					t.Error("lost credential fields")
				}
				if r.URL.Path == "/v1/mpp/validate" {
					if mode == "rejected" {
						fmt.Fprint(w, `{"success":false,"problem":{"title":"Rejected"}}`)
						return
					}
					request, _ := mpp.Decode(body.Credential.Challenge.Request)
					json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": body.Credential.Challenge, "credential": body.Credential, "details": map[string]any{}, "request": request, "method": "stripe", "intent": "charge", "source": body.Credential.Source})
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
				if mode == "bad-receipt" {
					id = "wrong"
				}
				json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: "stripe", Reference: "pi_test", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: &id}})
			}))
			defer platform.Close()
			client := clientFor(t, platform)
			offer := seller.StripeOffer{Amount: "1.25", Metadata: map[string]string{"purpose": "original"}}
			if mode == "reference" || mode == "reference-mismatch" {
				offer.ExternalID = new("")
			}
			route := seller.Route{Realm: "seller.example", SecretKey: "test-only-secret", Offers: []seller.Offer{{Stripe: &offer}}, Opaque: new("route-one")}
			if mode == "expired" {
				route.Lifetime = time.Nanosecond
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(200) })
			handler, err := client.Protect(route, next)
			if err != nil {
				t.Fatal(err)
			}
			// Caller mutation after construction must not change the protected offer.
			offer.Amount = "9"
			offer.Metadata["purpose"] = "changed"
			first := httptest.NewRecorder()
			handler.ServeHTTP(first, httptest.NewRequest("GET", "https://seller.example/paid", nil))
			challenges, err := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
			if err != nil || len(challenges) != 1 {
				t.Fatalf("challenge: %v %v", challenges, err)
			}
			request, _ := mpp.Decode(challenges[0].Request)
			if request.(map[string]any)["amount"] != "125" {
				t.Fatal("caller changed protected amount")
			}
			credential := mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"spt": "spt_test_only"}}
			if mode == "source" {
				credential.Source = new("did:example:buyer")
			}
			if mode == "reference" {
				credential.Payload["externalId"] = ""
			}
			if mode == "bad-signature" {
				credential.Challenge.ID = "forged"
			}
			if mode == "wrong-route" {
				offer.Amount = "1.25"
				offer.Metadata["purpose"] = "original"
				route.Opaque = new("route-two")
				handler, err = client.Protect(route, next)
				if err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := mpp.EncodeCredential(credential)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "https://seller.example/paid", nil)
			r.Header.Set("Authorization", "Payment "+encoded)
			second := httptest.NewRecorder()
			handler.ServeHTTP(second, r)
			success := mode == "success" || mode == "source" || mode == "reference"
			if success {
				if second.Code != 200 || served != 1 || second.Header().Get("Payment-Receipt") == "" || len(paths) != 3 {
					t.Fatalf("success: %d %v", second.Code, paths)
				}
			} else {
				if served != 0 || second.Header().Get("Payment-Receipt") != "" || second.Code != 402 {
					t.Fatalf("released failed payment: %d %v", second.Code, paths)
				}
				want := 1
				if mode == "rejected" {
					want = 2
				}
				if mode == "pending" || mode == "bad-receipt" {
					want = 3
				}
				if len(paths) != want {
					t.Fatalf("unexpected settlement calls: %v", paths)
				}
			}
			if credential.Source != nil && mode != "source" {
				t.Fatal("mutated caller credential")
			}
		})
	}
}
