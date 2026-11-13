package buyer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inflowpayai/inflow-go/internal/mppwire"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	fc "github.com/tempoxyz/mpp-go/pkg/client"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func TestUpstreamBuyerMethod(t *testing.T) {
	for _, state := range []string{"ready", "failed", "invalid-upstream"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct{ Challenge mpp.Challenge }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Challenge.Description == nil || *body.Challenge.Description != "Report" {
					t.Error("description lost before approval")
				}
				if state == "failed" {
					fmt.Fprint(w, `{"state":"failed","problem":{"status":403}}`)
					return
				}
				value := mpp.Credential{Challenge: body.Challenge, Payload: map[string]any{"transactionId": transactionID}}
				if state == "invalid-upstream" {
					value.Challenge.Method = "INVALID METHOD"
				}
				encoded, err := mpp.EncodeCredential(value)
				if err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": encoded})
			}))
			defer s.Close()
			c := newClient(t, s, nil)
			method, err := c.Method("inflow", "charge", buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if method.Name() != "inflow" || method.Intent() != "charge" {
				t.Fatal("incorrect method identity")
			}
			challenge := &foundation.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", RequestB64: "e30", Description: "Report"}
			matcher := method.(fc.ChallengeMatcher)
			if !matcher.CanHandleChallenge(challenge) {
				t.Fatal("valid challenge rejected")
			}
			for _, invalid := range []*foundation.Challenge{nil, {}, {ID: "id", Method: "tempo", Intent: "charge"}, {ID: "id", Method: "inflow", Intent: "subscription"}, {ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", RequestB64: "e30", Header: "Payment-Authorization"}} {
				if matcher.CanHandleChallenge(invalid) {
					t.Fatal("invalid challenge matched")
				}
				if _, err := method.CreateCredential(context.Background(), invalid); err == nil {
					t.Fatal("invalid challenge accepted")
				}
			}
			if calls != 0 {
				t.Fatal("invalid challenge started approval")
			}
			credential, err := method.CreateCredential(context.Background(), challenge)
			if state == "ready" {
				if err != nil || credential.Payload["transactionId"] != transactionID {
					t.Fatalf("credential: %#v %v", credential, err)
				}
			} else if err == nil {
				t.Fatal("failure accepted")
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			for _, pair := range [][2]string{{"unknown", "charge"}, {"tempo", "subscription"}, {"inflow", "unknown"}} {
				if _, err := c.Method(pair[0], pair[1], buyer.PaymentOptions{}); err == nil {
					t.Fatal(pair)
				}
			}
			for _, pair := range [][2]string{{"tempo", "charge"}, {"inflow", "subscription"}, {"card", "charge"}} {
				if _, err := c.Method(pair[0], pair[1], buyer.PaymentOptions{}); err != nil {
					t.Fatal(pair, err)
				}
			}
			merchant := &buyer.CardMerchant{Name: "Shop", URL: "https://shop.example", CountryCode: "US"}
			if _, err := c.Method("card", "charge", buyer.PaymentOptions{Merchant: merchant}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpstreamCardMethodCopiesMerchantAndPreservesProof(t *testing.T) {
	challenge := cardChallenge(t)
	opaque, err := mpp.Encode(map[string]string{"route": "card"})
	if err != nil {
		t.Fatal(err)
	}
	challenge.Opaque = &opaque
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Challenge mpp.Challenge
			Options   buyer.PaymentOptions
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Options.Merchant.Name != "Example" {
			t.Error("caller mutation changed payment merchant")
		}
		if body.Challenge.Description == nil || *body.Challenge.Description != "Widget" {
			t.Error("description lost")
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": cardCredential(t, body.Challenge)})
	}))
	defer s.Close()
	options := cardOptions()
	method, err := newClient(t, s, nil).Method("card", "charge", options)
	if err != nil {
		t.Fatal(err)
	}
	options.Merchant.Name = "modified"
	native, err := mppwire.ToChallenge(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !method.(fc.ChallengeMatcher).CanHandleChallenge(&native) {
		t.Fatal("valid CARD offer rejected")
	}
	credential, err := method.CreateCredential(context.Background(), &native)
	if err != nil || credential.Payload["extension"] != json.Number("9007199254740993") {
		t.Fatalf("proof lost: %#v %v", credential, err)
	}
}
