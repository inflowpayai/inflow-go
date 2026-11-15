package buyer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/inflowpayai/inflow-go/internal/mppwire"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	fc "github.com/tempoxyz/mpp-go/pkg/client"
)

func TestNativeTransportRequestOptions(t *testing.T) {
	defaults := "11111111-1111-4111-8111-111111111111"
	override := "22222222-2222-4222-8222-222222222222"
	expected := map[string]string{"default": defaults, "override": override, "clear": ""}
	var creations atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creations.Add(1)
		var input struct {
			Challenge mpp.Challenge
			Options   buyer.PaymentOptions
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Options.InstrumentID != expected[input.Challenge.ID] {
			t.Errorf("%s got instrument %s", input.Challenge.ID, input.Options.InstrumentID)
		}
		encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
		if err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": encoded})
	}))
	defer api.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Payment ") {
			w.WriteHeader(204)
			return
		}
		c := challenge()
		c.ID = strings.TrimPrefix(r.URL.Path, "/")
		header, err := mpp.RenderChallenge(c)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("WWW-Authenticate", header)
		w.WriteHeader(402)
	}))
	defer resource.Close()
	method, err := newClient(t, api, nil).Method("inflow", "charge", buyer.PaymentOptions{InstrumentID: defaults})
	if err != nil {
		t.Fatal(err)
	}
	client := fc.New([]fc.Method{method}, fc.WithMaxRetries(1))
	var group sync.WaitGroup
	for name := range expected {
		group.Go(func() {
			ctx := context.Background()
			if name != "default" {
				ctx = buyer.WithPaymentOptions(ctx, buyer.PaymentOptions{InstrumentID: expected[name]})
			}
			r, err := http.NewRequestWithContext(ctx, "GET", resource.URL+"/"+name, nil)
			if err != nil {
				t.Error(err)
				return
			}
			response, err := client.Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 204 {
				t.Errorf("status %d", response.StatusCode)
			}
		})
	}
	group.Wait()
	if creations.Load() != 3 {
		t.Fatalf("created %d payments", creations.Load())
	}
}

func TestRequestMerchantSnapshotAndValidation(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Challenge mpp.Challenge
			Options   buyer.PaymentOptions
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Options.Merchant == nil || input.Options.Merchant.Name != "Example" || input.Options.InstrumentID != transactionID {
			t.Errorf("changed options: %#v", input.Options)
		}
		fmt.Fprintf(w, `{"state":"ready","credential":%q}`, cardCredential(t, input.Challenge))
	}))
	defer api.Close()
	method, err := newClient(t, api, nil).Method("card", "charge", buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	card := cardChallenge(t)
	opaque, err := mpp.Encode(map[string]string{"route": "card"})
	if err != nil {
		t.Fatal(err)
	}
	card.Opaque = &opaque
	native, err := mppwire.ToChallenge(card)
	if err != nil {
		t.Fatal(err)
	}
	if !method.(fc.ChallengeMatcher).CanHandleChallenge(&native) {
		t.Fatal("request-local merchant was blocked by constructor defaults")
	}
	options := cardOptions()
	ctx := buyer.WithPaymentOptions(context.Background(), options)
	options.Merchant.Name = "modified"
	for range 2 {
		if _, err := method.CreateCredential(ctx, &native); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []buyer.PaymentOptions{{}, {Merchant: &buyer.CardMerchant{Name: "Example", URL: "bad", CountryCode: "US"}}, {InstrumentID: "invalid", Merchant: cardOptions().Merchant}} {
		if _, err := method.CreateCredential(buyer.WithPaymentOptions(ctx, invalid), &native); err == nil {
			t.Fatal("invalid request options accepted")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := method.CreateCredential(cancelled, &native); err == nil {
		t.Fatal("cancelled request accepted")
	}
	if calls.Load() != 2 {
		t.Fatalf("invalid/cancelled options started payment: %d", calls.Load())
	}
	malformed := native
	malformed.RequestB64 = "e30"
	malformed.Request = map[string]any{}
	if method.(fc.ChallengeMatcher).CanHandleChallenge(&malformed) {
		t.Fatal("invalid CARD request accepted")
	}
}

func TestRequestSubscriptionReplacement(t *testing.T) {
	first := "11111111-1111-4111-8111-111111111111"
	second := "22222222-2222-4222-8222-222222222222"
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var input struct{ Challenge mpp.Challenge }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
		if err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": encoded})
	}))
	defer api.Close()
	client := newClient(t, api, nil)
	method, err := client.Method("inflow", "subscription", buyer.PaymentOptions{SubscriptionID: first})
	if err != nil {
		t.Fatal(err)
	}
	wire := challenge()
	wire.Intent = "subscription"
	native, err := mppwire.ToChallenge(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{context.Background(), buyer.WithPaymentOptions(context.Background(), buyer.PaymentOptions{SubscriptionID: second}), buyer.WithPaymentOptions(context.Background(), buyer.PaymentOptions{})} {
		if _, err := method.CreateCredential(ctx, &native); err != nil {
			t.Fatal(err)
		}
	}
	// The direct API's explicit argument takes precedence over unrelated context data.
	ctx := buyer.WithPaymentOptions(context.Background(), buyer.PaymentOptions{SubscriptionID: second})
	if _, err := client.Fulfil(ctx, wire, buyer.PaymentOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/subscriptions/" + first + "/authorize", "/v1/subscriptions/" + second + "/authorize", "/v1/transactions/mpp", "/v1/transactions/mpp"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}
