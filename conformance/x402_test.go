package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	b "github.com/inflowpayai/inflow-go/x402/buyer"
	s "github.com/inflowpayai/inflow-go/x402/seller"
)

func executeX402(ctx context.Context, operation string, raw json.RawMessage) (result any, outcome error) {
	var input struct {
		Value             string
		Declaration       any
		PaymentID         string `json:"payment_id"`
		Requirement       x402.PaymentRequirements
		Context           x402.PaymentRequired
		Timeout           int             `json:"timeout_ms"`
		Interval          int             `json:"poll_interval_ms"`
		Payload           json.RawMessage `json:"payment_payload"`
		Requirements      json.RawMessage `json:"payment_requirements"`
		Config, Supported json.RawMessage
		Options           struct {
			Price               json.RawMessage
			MaxTimeoutSeconds   int
			Schemes, Networks   []string
			AssetTransferMethod string
		}
	}
	if err := decode(raw, &input); err != nil {
		return nil, err
	}
	defer watchInput(&input, &outcome)()
	switch operation {
	case "x402.core.identifier-valid":
		return x402.ValidatePaymentID(input.Value), nil
	case "x402.core.identifier-declaration":
		return x402.DeclarePaymentIdentifier(), nil
	case "x402.core.identifier-entry":
		return x402.PaymentIdentifierEntry(input.Declaration, input.PaymentID), nil
	}
	var options inflow.Options
	var err error
	if operation == "x402.seller.offers" || operation == "x402.seller.route" {
		// Go's public configuration input is HTTP rather than a provider object.
		// Serve the supplied configuration unchanged; the SDK alone builds offers.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				w.WriteHeader(405)
				return
			}
			switch r.URL.Path {
			case "/v1/x402/config":
				w.Write(input.Config)
			case "/v1/x402/supported":
				w.Write(input.Supported)
			default:
				w.WriteHeader(404)
			}
		}))
		defer server.Close()
		options = inflow.Options{BaseURL: server.URL, APIKey: "test-only-seller-key"}
	} else {
		options, err = platformOptions(raw)
		if err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(operation, "x402.buyer.") {
		if operation != "x402.buyer.sign" && operation != "x402.buyer.cancel" && operation != "x402.buyer.concurrent-await" {
			return nil, errors.New("unknown x402 buyer operation")
		}
		timeout, interval := 2*time.Second, time.Millisecond
		if input.Timeout > 0 {
			timeout = time.Duration(input.Timeout) * time.Millisecond
		}
		if input.Interval > 0 {
			interval = time.Duration(input.Interval) * time.Millisecond
		}
		client, err := b.New(b.Options{Options: options, WaitTimeout: timeout, PollInterval: interval})
		if err != nil {
			return nil, err
		}
		// Node loads capabilities in its asynchronous constructor; Go exposes this
		// as Supported so constructors remain free of network activity.
		if _, err = client.Supported(ctx); err != nil {
			return nil, err
		}
		required := input.Context
		required.Accepts = []x402.PaymentRequirements{input.Requirement}
		defer watchInput(&required, &outcome)()
		payment, err := client.Prepare(ctx, required, b.SignOptions{PaymentID: input.PaymentID})
		if err != nil {
			return nil, err
		}
		switch operation {
		case "x402.buyer.cancel":
			if err = payment.Cancel(ctx); err != nil {
				// This operation observes Wait after cancellation, including an API
				// rejection of cleanup. Go exposes that rejection; Node absorbs it.
				var api *inflow.APIError
				if !errors.As(err, &api) {
					return nil, err
				}
			}
			return payment.Wait(ctx)
		case "x402.buyer.sign":
			return payment.Wait(ctx)
		case "x402.buyer.concurrent-await":
			type answer struct {
				value b.EncodedPayment
				err   error
			}
			answers := make(chan answer, 2)
			for range 2 {
				go func() { v, e := payment.Wait(ctx); answers <- answer{v, e} }()
			}
			a, other := <-answers, <-answers
			if !reflect.DeepEqual(a, other) {
				return nil, errors.New("concurrent waits returned different results")
			}
			return a.value, a.err
		default:
			return nil, errors.New("unknown x402 buyer operation")
		}
	}
	if operation == "x402.seller.offers" || operation == "x402.seller.route" {
		client, err := s.New(options)
		if err != nil {
			return nil, err
		}
		price := s.PriceSpec{}
		if json.Unmarshal(input.Options.Price, &price.Amount) != nil {
			if err := decode(input.Options.Price, &price); err != nil {
				return nil, err
			}
		}
		accepts := s.AcceptsOptions{Price: price, MaxTimeoutSeconds: input.Options.MaxTimeoutSeconds, Schemes: input.Options.Schemes, Networks: input.Options.Networks}
		if operation == "x402.seller.offers" {
			return client.Accepts(ctx, accepts)
		}
		return client.Route(ctx, s.RouteOptions{AcceptsOptions: accepts, AssetTransferMethod: input.Options.AssetTransferMethod})
	}
	var facilitator *s.Facilitator
	if options.APIKey == "" {
		facilitator, err = s.NewAnonymousFacilitator(options)
	} else {
		facilitator, err = s.NewFacilitator(options)
	}
	if err != nil {
		return nil, err
	}
	switch operation {
	case "x402.seller.verify":
		return facilitator.Verify(ctx, input.Payload, input.Requirements)
	case "x402.seller.settle":
		return facilitator.Settle(ctx, input.Payload, input.Requirements)
	case "x402.seller.verify-settle":
		v, err := facilitator.Verify(ctx, input.Payload, input.Requirements)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"verification": v}
		if v.IsValid {
			result["settlement"], err = facilitator.Settle(ctx, input.Payload, input.Requirements)
		}
		return result, err
	default:
		return nil, errors.New("unknown x402 operation")
	}
}
