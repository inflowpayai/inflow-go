package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
)

func required() x402.PaymentRequired {
	return x402.PaymentRequired{X402Version: 2, Resource: &x402.ResourceInfo{URL: "https://seller.example/data"}, Accepts: []x402.PaymentRequirements{{Scheme: "balance", Network: "inflow:1", Amount: "1", PayTo: "seller", Asset: "USDC", MaxTimeoutSeconds: 300}}}
}
func ready(w http.ResponseWriter) {
	fmt.Fprint(w, `{"status":"PENDING","encodedPayload":"server-original-value","paymentPayload":{"x402Version":2,"accepted":{"scheme":"balance","network":"inflow:1"},"payload":{"proof":9007199254740993}}}`)
}
func server(t *testing.T, poll http.HandlerFunc) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	creates, cancels := new(atomic.Int32), new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transactions/x402-supported":
			fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1","x402Version":2}]}`)
		case "/v1/transactions/x402":
			creates.Add(1)
			fmt.Fprint(w, `{"transactionId":"txn","approvalId":"approval","approvalStatus":"PENDING"}`)
		case "/v1/approvals/approval/cancel":
			cancels.Add(1)
			w.WriteHeader(204)
		case "/v1/transactions/txn/x402":
			poll(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(s.Close)
	return s, creates, cancels
}
func client(t *testing.T, s *httptest.Server, hooks ...buyer.Hooks) *buyer.Client {
	t.Helper()
	c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, PollInterval: time.Millisecond, WaitTimeout: 2 * time.Second, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func prepare(t *testing.T, c *buyer.Client) *buyer.Payment {
	t.Helper()
	p, err := c.Prepare(context.Background(), required(), buyer.SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWaitResumeAndCancellation(t *testing.T) {
	var done atomic.Bool
	s, creates, cancels := server(t, func(w http.ResponseWriter, r *http.Request) {
		if done.Load() {
			ready(w)
		} else {
			fmt.Fprint(w, `{"status":"INITIATED"}`)
		}
	})
	p := prepare(t, client(t, s))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := p.Wait(ctx)
	var paymentErr *buyer.Error
	if !errors.As(err, &paymentErr) || paymentErr.Code != "payment-timeout" || cancels.Load() != 0 {
		t.Fatalf("wait: %v cancels %d", err, cancels.Load())
	}
	done.Store(true)
	value, err := p.Wait(context.Background())
	if err != nil || value.EncodedPayload != "server-original-value" || creates.Load() != 1 {
		t.Fatalf("resume: %v %v", value, err)
	}
	if value.PaymentPayload.Payload["proof"] != json.Number("9007199254740993") {
		t.Fatal("rounded proof")
	}
	value.PaymentPayload.Payload["proof"] = "mutated"
	again, err := p.Wait(context.Background())
	if err != nil || again.PaymentPayload.Payload["proof"] != json.Number("9007199254740993") {
		t.Fatal("shared result")
	}
	if p.TransactionID() != "txn" || p.ApprovalID() != "approval" {
		t.Fatal("IDs")
	}
	if status, err := p.Status(context.Background()); err != nil || status != "PENDING" {
		t.Fatalf("status %s %v", status, err)
	}
	if err = p.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Wait(context.Background()); !errors.As(err, &paymentErr) || paymentErr.Code != "payment-cancelled" {
		t.Fatal(err)
	}
}

func TestSharedWaitAndAfterFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var polls, after atomic.Int32
			sentinel := errors.New("hook failed")
			s, creates, cancels := server(t, func(w http.ResponseWriter, r *http.Request) {
				if polls.Add(1) == 1 {
					close(entered)
				}
				<-release
				ready(w)
			})
			c := client(t, s, buyer.Hooks{After: func(context.Context, x402.PaymentRequired, x402.PaymentPayload) error {
				after.Add(1)
				if fail {
					return sentinel
				}
				return nil
			}})
			p := prepare(t, c)
			var group sync.WaitGroup
			group.Add(2)
			results := make(chan error, 2)
			go func() { defer group.Done(); _, err := p.Wait(context.Background()); results <- err }()
			<-entered
			go func() { defer group.Done(); _, err := p.Wait(context.Background()); results <- err }()
			close(release)
			group.Wait()
			close(results)
			for err := range results {
				if (fail && !errors.Is(err, sentinel)) || (!fail && err != nil) {
					t.Fatal(err)
				}
			}
			_, err := p.Wait(context.Background())
			if fail && !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if after.Load() != 1 || polls.Load() != 1 || creates.Load() != 1 || cancels.Load() != 0 {
				t.Fatal("repeated work")
			}
		})
	}
}

func TestOneShotCleanupAndProviderFailure(t *testing.T) {
	s, creates, cancels := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"DECLINED"}`) })
	_, err := client(t, s).Sign(context.Background(), required(), buyer.SignOptions{})
	var paymentErr *buyer.Error
	if !errors.As(err, &paymentErr) || paymentErr.Status != "DECLINED" || creates.Load() != 1 || cancels.Load() != 1 {
		t.Fatalf("%v creates %d cancels %d", err, creates.Load(), cancels.Load())
	}
	sentinel := errors.New("credential unavailable")
	c, e := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL, AccessToken: func(context.Context) (string, error) { return "", sentinel }}})
	if e != nil {
		t.Fatal(e)
	}
	if _, err = c.Supported(context.Background()); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

type signer struct{ calls *atomic.Int32 }

func (s signer) Scheme() string { return "exact" }
func (s signer) CreatePaymentPayload(context.Context, x402.PaymentRequirements, foundation.PaymentPayloadContext) (x402.PaymentPayload, error) {
	s.calls.Add(1)
	return x402.PaymentPayload{X402Version: 2, Payload: map[string]any{"signature": "synthetic"}}, nil
}

func TestRealUpstreamAndWrapperHooks(t *testing.T) {
	s, creates, _ := server(t, func(w http.ResponseWriter, r *http.Request) { t.Error("managed polling on external route") })
	calls := new(atomic.Int32)
	external := foundation.Newx402Client(foundation.WithSpendControls(foundation.SpendControls{AllowedAssets: []foundation.SpendControlAsset{{Network: "eip155:8453", Asset: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", MaxAmountPerPayment: "1"}}}))
	external.Register("eip155:8453", signer{calls})
	upstreamErr := errors.New("upstream observer error")
	external.OnAfterPaymentCreation(func(foundation.PaymentCreatedContext) error { return upstreamErr })
	wrapperErr := errors.New("wrapper error")
	var before, after atomic.Int32
	c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: external, Hooks: []buyer.Hooks{{Before: func(context.Context, x402.PaymentRequired) error { before.Add(1); return nil }, After: func(context.Context, x402.PaymentRequired, x402.PaymentPayload) error {
		after.Add(1)
		return wrapperErr
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	r := required()
	r.Accepts[0] = x402.PaymentRequirements{Scheme: "exact", Network: "eip155:8453", Asset: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Amount: "1", PayTo: "seller"}
	_, err = c.Sign(context.Background(), r, buyer.SignOptions{})
	if !errors.Is(err, wrapperErr) || before.Load() != 1 || after.Load() != 1 || calls.Load() != 1 || creates.Load() != 0 {
		t.Fatalf("hook behavior %v", err)
	}
	r.Accepts[0].Amount = "2000000"
	_, err = c.Sign(context.Background(), r, buyer.SignOptions{})
	if err == nil || calls.Load() != 1 {
		t.Fatal("upstream spend cap bypassed")
	}
	if strings.Contains(err.Error(), "wrapper error") {
		t.Fatal("not upstream spend rejection")
	}
}
