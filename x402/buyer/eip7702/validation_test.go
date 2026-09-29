package eip7702

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/inflowpayai/inflow-go/x402"
)

func TestInvalidPaymentNeverReadsWallet(t *testing.T) {
	mutations := map[string]func(*x402.PaymentPayload){
		"version":     func(p *x402.PaymentPayload) { p.X402Version = 1 },
		"network":     func(p *x402.PaymentPayload) { p.Accepted.Network = "eip155:01" },
		"large chain": func(p *x402.PaymentPayload) { p.Accepted.Network = "eip155:99999999999999999999" },
		"marshal":     func(p *x402.PaymentPayload) { p.Payload["invalid"] = make(chan int) },
		"type":        func(p *x402.PaymentPayload) { p.Payload["permit2Authorization"] = true },
		"signature":   func(p *x402.PaymentPayload) { p.Payload["signature"] = "0x01" },
		"asset":       func(p *x402.PaymentPayload) { p.Accepted.Asset = "invalid" },
		"payTo":       func(p *x402.PaymentPayload) { p.Accepted.PayTo = "invalid" },
		"proxy":       func(p *x402.PaymentPayload) { p.Accepted.Extra["permit2Proxy"] = "invalid" },
		"amount": func(p *x402.PaymentPayload) {
			p.Payload["permit2Authorization"].(map[string]any)["permitted"].(map[string]any)["amount"] = "0"
		},
		"oversized amount": func(p *x402.PaymentPayload) {
			p.Payload["permit2Authorization"].(map[string]any)["permitted"].(map[string]any)["amount"] = strings.Repeat("9", 80)
		},
		"expired": func(p *x402.PaymentPayload) { p.Payload["permit2Authorization"].(map[string]any)["deadline"] = "1" },
		"nonce":   func(p *x402.PaymentPayload) { p.Payload["permit2Authorization"].(map[string]any)["nonce"] = "-1" },
		"validAfter": func(p *x402.PaymentPayload) {
			p.Payload["permit2Authorization"].(map[string]any)["witness"].(map[string]any)["validAfter"] = "1.5"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := load(t)
			mutate(&f.Payload)
			w := newWallet(t)
			e, requests := extension(t, f, w, nil)
			if _, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err == nil {
				t.Fatal("accepted invalid payment")
			}
			if w.reads != 0 || requests.Load() != 0 {
				t.Fatal("performed work for invalid payment")
			}
		})
	}
}

func TestNoopAndConfiguration(t *testing.T) {
	f := load(t)
	w := newWallet(t)
	consent := func(context.Context, types.SetCodeAuthorization) (bool, error) { return true, nil }
	for _, o := range []Options{{}, {Signer: w}, {Signer: w, Consent: consent, BaseURL: "invalid"}} {
		if _, err := New(o); err == nil {
			t.Fatal("invalid options")
		}
	}
	e, requests := extension(t, f, w, nil)
	if _, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{}); err != nil {
		t.Fatal(err)
	}
	p := f.Payload
	p.Accepted.Scheme = "balance"
	if _, err := e.EnrichPaymentPayload(context.Background(), p, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err != nil {
		t.Fatal(err)
	}
	w.allowance = big.NewInt(123)
	if _, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || w.signs != 0 || w.auths != 0 {
		t.Fatal("unnecessary sponsorship")
	}
	for _, value := range []any{make(chan int), map[string]any{"info": map[string]any{"version": "2"}}, map[string]any{"info": map[string]any{"version": "1"}, "extra": true}} {
		if _, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: map[string]any{e.Key(): value}}); err == nil {
			t.Fatal("bad declaration")
		}
	}
}

func TestPreparationHTTPFailures(t *testing.T) {
	for _, body := range []string{"malformed", `null`} {
		t.Run(body, func(t *testing.T) {
			f := load(t)
			w := newWallet(t)
			s := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				if body == "null" {
					out.WriteHeader(403)
				}
				_, _ = out.Write([]byte(body))
			}))
			defer s.Close()
			e, err := New(Options{BaseURL: s.URL, Signer: w, Consent: func(context.Context, types.SetCodeAuthorization) (bool, error) {
				t.Error("requested consent for bad response")
				return false, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err == nil {
				t.Fatal("accepted failed preparation")
			}
		})
	}
}

func TestHelperBoundaries(t *testing.T) {
	for _, s := range []string{"0X00", "0xgg", "0x1"} {
		if _, ok := hexBytes(s); ok {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"0x01", "0x100", "0xGG"} {
		if _, ok := quantity(s, 8); ok {
			t.Fatal(s)
		}
	}
	for _, invalidCall := range []func(){func() { mustEncodeCall("invalid", "approve") }, func() { mustEncodeCall(approveABI, "approve", "wrong argument") }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid static ABI binding did not panic")
				}
			}()
			invalidCall()
		}()
	}
	f := load(t)
	p := f.Payload
	p.Payload = map[string]any{"invalid": make(chan int)}
	if _, err := paymentBatch(p, common.HexToAddress(f.Owner)); err == nil {
		t.Fatal("invalid payload")
	}
	f = load(t)
	expected, err := paymentBatch(f.Payload, common.HexToAddress(f.Owner))
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []any{nil, map[string]any{"chainId": 8453, "address": Delegation}, map[string]any{"chainId": 8453, "nonce": 9007199254740992, "address": Delegation}} {
		f.Prepared["authorization"] = auth
		data, _ := json.Marshal(f.Prepared)
		if _, _, err := validatePreparation(data, expected, common.HexToAddress(f.Owner)); err == nil {
			t.Fatal("invalid authorization")
		}
	}
}
