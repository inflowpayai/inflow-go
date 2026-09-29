package buyer_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

type externalStub struct {
	selectErr error
	signErr   error
	payload   x402.PaymentPayload
}

func (s externalStub) SelectPaymentRequirements(r []x402.PaymentRequirements) (x402.PaymentRequirements, error) {
	return r[0], s.selectErr
}
func (s externalStub) CreatePaymentPayload(context.Context, x402.PaymentRequirements, *x402.ResourceInfo, map[string]any) (x402.PaymentPayload, error) {
	return s.payload, s.signErr
}

func TestBalanceSelection(t *testing.T) {
	for _, tc := range []struct {
		name, body, expected string
		status               int
	}{
		{"affordable second", `{"balances":[{"currency":"USD","available":"0"},{"currency":"EUR","available":" 1.000000000000000001 "}]}`, "EUR", 200},
		{"negative and invalid", `{"balances":[{"currency":"USD","available":"-10"},{"currency":"EUR","available":"oops"}]}`, "USD", 200},
		{"malformed", `{`, "USD", 200},
		{"unavailable", `{}`, "USD", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loads atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/transactions/x402-supported" {
					fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
					return
				}
				if r.URL.Path != "/v1/balances" {
					t.Error(r.URL)
					w.WriteHeader(404)
					return
				}
				loads.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			r := required()
			r.Accepts[0].Amount = "1000000000000000001"
			r.Accepts[0].Extra = map[string]any{"assetName": "USD"}
			second := r.Accepts[0]
			second.Extra = map[string]any{"assetName": "EUR"}
			r.Accepts = append(r.Accepts, second)
			c := client(t, s)
			for range 2 {
				selected, err := c.Select(context.Background(), r)
				if err != nil || selected == nil || selected.Extra["assetName"] != tc.expected {
					t.Fatalf("%v %v", selected, err)
				}
			}
			if loads.Load() != 2 {
				t.Fatal("cached account balances")
			}
		})
	}
}

func TestSelectionPoliciesAndFailures(t *testing.T) {
	s, _, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	sentinel := errors.New("policy failed")
	for _, tc := range []struct {
		name     string
		policy   buyer.Policy
		expected error
	}{
		{"error", func(context.Context, []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
			return nil, sentinel
		}, sentinel},
		{"removed", func(context.Context, []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) { return nil, nil }, nil},
		{"changed", func(_ context.Context, r []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
			r[0].Network = "unknown:1"
			return r, nil
		}, nil},
		{"invalid", func(_ context.Context, r []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
			r[0].Extra = map[string]any{"invalid": make(chan int)}
			return r, nil
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, Policies: []buyer.Policy{tc.policy}})
			if err != nil {
				t.Fatal(err)
			}
			r := required()
			_, err = c.Select(context.Background(), r)
			if err == nil || (tc.expected != nil && !errors.Is(err, tc.expected)) {
				t.Fatal(err)
			}
			if r.Accepts[0].Network != "inflow:1" || r.Accepts[0].Extra != nil {
				t.Fatal("policy mutated caller data")
			}
		})
	}
	c := client(t, s)
	r := required()
	r.X402Version = 1
	if _, err := c.Select(context.Background(), r); err == nil {
		t.Fatal("accepted V1")
	}
	r = required()
	r.Extensions = map[string]any{"bad": make(chan int)}
	if _, err := c.Select(context.Background(), r); err == nil {
		t.Fatal("accepted invalid JSON")
	}
	r = required()
	r.Accepts[0].Extra = map[string]any{"assetTransferMethod": "permit2"}
	if selected, err := c.Select(context.Background(), r); err != nil || selected != nil {
		t.Fatal("managed Permit2")
	}
	if _, err := c.Sign(context.Background(), r, buyer.SignOptions{}); err == nil {
		t.Fatal("missing external signer")
	}
}

func TestExternalRecoveryAndIdentifier(t *testing.T) {
	s, creates, _ := server(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected poll") })
	sentinel := errors.New("signer failed")
	r := required()
	r.Accepts[0].Network = "external:1"
	for _, tc := range []struct {
		name      string
		external  externalStub
		hooks     []buyer.Hooks
		mandatory bool
		wantError error
		success   bool
	}{
		{name: "success", success: true},
		{name: "selection error", external: externalStub{selectErr: sentinel}, wantError: sentinel},
		{name: "signing error", external: externalStub{signErr: sentinel}, wantError: sentinel},
		{name: "required identifier", mandatory: true},
		{name: "no recovery", external: externalStub{signErr: sentinel}, hooks: []buyer.Hooks{{Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) { return nil, nil }}}, wantError: sentinel},
		{name: "recovery error", external: externalStub{signErr: sentinel}, hooks: []buyer.Hooks{{Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) { return nil, sentinel }}}, wantError: sentinel},
		{name: "recovered", external: externalStub{signErr: sentinel}, hooks: []buyer.Hooks{{Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) {
			return &x402.PaymentPayload{X402Version: 2}, nil
		}}}, success: true},
		{name: "invalid recovery", external: externalStub{signErr: sentinel}, hooks: []buyer.Hooks{{Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) {
			return &x402.PaymentPayload{Payload: map[string]any{"bad": make(chan int)}}, nil
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := r
			declaration := x402.DeclarePaymentIdentifier()
			declaration.Info["required"] = json.RawMessage(fmt.Sprint(tc.mandatory))
			input.Extensions = map[string]any{x402.PaymentIdentifier: declaration}
			c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: tc.external, Hooks: tc.hooks})
			if err != nil {
				t.Fatal(err)
			}
			value, err := c.Sign(context.Background(), input, buyer.SignOptions{})
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
				data, e := base64.StdEncoding.DecodeString(value.EncodedPayload)
				if e != nil {
					t.Fatal(e)
				}
				var payload x402.PaymentPayload
				if json.Unmarshal(data, &payload) != nil {
					t.Fatal("invalid encoded payload")
				}
			} else if err == nil || (tc.wantError != nil && !errors.Is(err, tc.wantError)) {
				t.Fatal(err)
			}
		})
	}
	if creates.Load() != 0 {
		t.Fatal("external signing created InFlow approval")
	}
}
