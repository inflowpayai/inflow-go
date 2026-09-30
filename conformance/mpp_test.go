package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
	b "github.com/inflowpayai/inflow-go/mpp/buyer"
	s "github.com/inflowpayai/inflow-go/mpp/seller"
)

func executeMPP(ctx context.Context, operation string, raw json.RawMessage) (result any, outcome error) {
	var input struct {
		Value     any
		Headers   json.RawMessage
		Challenge mpp.Challenge
		Context   struct {
			InstrumentID   string
			SubscriptionID string
		}
		Timeout        int `json:"timeout_ms"`
		Method, Intent string
		Request        json.RawMessage
		Replacement    json.RawMessage `json:"replacement_request"`
		Credential     mpp.Credential
		Payload        map[string]any `json:"credential_payload"`
		Source         *string
	}
	if err := decode(raw, &input); err != nil {
		return nil, err
	}
	defer watchInput(&input, &outcome)()
	value, _ := input.Value.(string)
	switch operation {
	case "mpp.core.encode":
		return mpp.Encode(input.Value)
	case "mpp.core.decode":
		return mpp.Decode(value)
	case "mpp.core.decode-credential":
		return mpp.DecodeCredential(value)
	case "mpp.core.decode-receipt":
		return mpp.DecodeReceipt(value)
	case "mpp.core.parse-challenges":
		var headers []string
		var single string
		if json.Unmarshal(input.Headers, &single) == nil {
			headers = []string{single}
		} else if err := decode(input.Headers, &headers); err != nil {
			return nil, err
		}
		return mpp.ParseChallenges(headers)
	}
	options, err := platformOptions(raw)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(operation, "mpp.buyer.") {
		if operation != "mpp.buyer.fulfil" && operation != "mpp.buyer.cancel" {
			return nil, errors.New("unknown MPP buyer operation")
		}
		timeout := 5 * time.Second
		if input.Timeout > 0 {
			timeout = time.Duration(input.Timeout) * time.Millisecond
		}
		client, err := b.New(b.Options{Options: options, PollInterval: time.Nanosecond, WaitTimeout: timeout})
		if err != nil {
			return nil, err
		}
		paymentOptions := b.PaymentOptions{InstrumentID: input.Context.InstrumentID, SubscriptionID: input.Context.SubscriptionID}
		if operation == "mpp.buyer.fulfil" {
			return client.Fulfil(ctx, input.Challenge, paymentOptions)
		}
		payment, err := client.Prepare(ctx, input.Challenge, paymentOptions)
		if err != nil {
			return nil, err
		}
		if err = payment.Cancel(ctx); err != nil {
			return nil, err
		}
		return payment.Wait(ctx)
	}
	client, err := s.New(options)
	if err != nil {
		return nil, err
	}
	switch operation {
	case "mpp.seller.prepare":
		offer, err := mppOffer(input.Method, input.Intent, input.Request)
		if err != nil {
			return nil, err
		}
		defer watchInput(&offer, &outcome)()
		prepared, err := client.Prepare(ctx, offer)
		if err != nil {
			return nil, err
		}
		return mpp.Decode(prepared.Request)
	case "mpp.seller.validate":
		return client.Validate(ctx, input.Credential)
	case "mpp.seller.verify":
		return client.Verify(ctx, input.Credential)
	case "mpp.seller.route-binding":
		offer, err := mppOffer(input.Method, input.Intent, input.Request)
		if err != nil {
			return nil, err
		}
		route := s.Route{Realm: "seller.example", SecretKey: "test-only-binding-secret-at-least-32-bytes", Offers: []s.Offer{offer}}
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		protected, err := client.Protect(route, handler)
		if err != nil {
			return nil, err
		}
		first := httptest.NewRecorder()
		protected.ServeHTTP(first, httptest.NewRequest("GET", "http://seller.example/test", nil).WithContext(ctx))
		challenges, err := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
		if err != nil {
			return nil, err
		}
		if first.Code != 402 || len(challenges) != 1 {
			return nil, errors.New("expected one issued challenge")
		}
		credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: input.Payload, Source: input.Source})
		if err != nil {
			return nil, err
		}
		offer, err = mppOffer(input.Method, input.Intent, input.Replacement)
		if err != nil {
			return nil, err
		}
		route.Offers = []s.Offer{offer}
		protected, err = client.Protect(route, handler)
		if err != nil {
			return nil, err
		}
		req := httptest.NewRequest("GET", "http://seller.example/test", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Payment "+credential)
		second := httptest.NewRecorder()
		protected.ServeHTTP(second, req)
		return map[string]int{"status": second.Code}, nil
	default:
		return nil, errors.New("unknown MPP operation")
	}
}

func mppOffer(method, intent string, raw json.RawMessage) (s.Offer, error) {
	var offer s.Offer
	var target any
	switch {
	case method == "tempo" && intent == "charge":
		offer.Tempo = &mpp.TempoRequest{}
		target = offer.Tempo
	case method == "inflow" && intent == "charge":
		offer.Charge = &mpp.ChargeRequest{}
		target = offer.Charge
	case method == "inflow" && intent == "subscription":
		offer.Subscription = &mpp.SubscriptionRequest{}
		target = offer.Subscription
	default:
		return offer, errors.New("unsupported MPP method and intent")
	}
	return offer, decode(raw, target)
}
