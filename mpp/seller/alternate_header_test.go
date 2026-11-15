package seller_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func TestAlternateHeaderBuyerSellerLifecycle(t *testing.T) {
	var created, validated, broadcast, handled atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mpp/config":
			fmt.Fprint(w, configJSON)
		case "/v1/transactions/mpp":
			created.Add(1)
			var input struct{ Challenge mpp.Challenge }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Challenge.CredentialHeader() != "Payment-Authorization" {
				t.Error("lost challenge header")
			}
			credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": "test"}})
			if err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential})
		case "/v1/mpp/validate":
			validated.Add(1)
			var input struct{ Credential mpp.Credential }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Credential.Challenge.CredentialHeader() != "Payment-Authorization" {
				t.Error("lost credential header")
			}
			request, _ := mpp.Decode(input.Credential.Challenge.Request)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": input.Credential.Challenge, "credential": input.Credential, "request": request, "method": "inflow", "intent": "charge"})
		case "/v1/mpp/broadcast":
			broadcast.Add(1)
			fmt.Fprint(w, `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00Z"}}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer api.Close()
	handler, err := clientFor(t, api).Protect(seller.Route{
		Realm: "seller.example", SecretKey: "test", Header: "Payment-Authorization",
		Offers: []seller.Offer{{Charge: &mpp.ChargeRequest{Amount: "1", Currency: "USDC"}}},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		if r.Header.Get("Authorization") != "Bearer application" {
			t.Error("application authentication replaced")
		}
		if !strings.HasPrefix(r.Header.Get("Payment-Authorization"), "Payment ") {
			t.Error("missing payment")
		}
		fmt.Fprint(w, "paid")
	}))
	if err != nil {
		t.Fatal(err)
	}
	resource := httptest.NewServer(handler)
	defer resource.Close()
	client, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: api.URL}})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest("POST", resource.URL, strings.NewReader("body"))
	request.Header.Set("Authorization", "Bearer application")
	response, err := client.Do(request, buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Payment-Receipt") == "" || created.Load() != 1 || validated.Load() != 1 || broadcast.Load() != 1 || handled.Load() != 1 {
		t.Fatalf("status %d; create/validate/broadcast/handler %d/%d/%d/%d", response.StatusCode, created.Load(), validated.Load(), broadcast.Load(), handled.Load())
	}
	if request.Header.Get("Payment-Authorization") != "" {
		t.Fatal("mutated caller request")
	}

	// A challenge for this route cannot be replayed through the default header,
	// nor can its signed header selection be changed to fit that field.
	unpaid := httptest.NewRecorder()
	handler.ServeHTTP(unpaid, httptest.NewRequest("GET", "/", nil))
	challenge, err := mpp.ParseChallenge(unpaid.Header().Get("WWW-Authenticate"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tamper := range []bool{false, true} {
		copy := challenge
		if tamper {
			copy.Header = nil
		}
		encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: copy, Payload: map[string]any{"transactionId": "test"}})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/", nil)
		header := "Authorization"
		if tamper {
			header = "Payment-Authorization"
		}
		r.Header.Set(header, "Payment "+encoded)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, r)
		if result.Code != 402 || validated.Load() != 1 || broadcast.Load() != 1 || handled.Load() != 1 {
			t.Fatal("accepted wrong payment header or tampered binding")
		}
	}
}
