package buyer_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func TestPreparationInvalidInputsAndResponses(t *testing.T) {
	s, _, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	c := client(t, s)
	r := required()
	r.Extensions = map[string]any{"bad": make(chan int)}
	if _, err := c.Prepare(context.Background(), r, buyer.SignOptions{}); err == nil {
		t.Fatal("invalid requirement")
	}
	if _, err := c.Sign(context.Background(), r, buyer.SignOptions{}); err == nil {
		t.Fatal("invalid requirement")
	}
	if _, err := c.Prepare(context.Background(), required(), buyer.SignOptions{TransactionRequestExtensions: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Fatal("invalid request extension")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Prepare(ctx, required(), buyer.SignOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Select(ctx, required()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Sign(ctx, required(), buyer.SignOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p := prepare(t, c)
	if _, err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, body := range []string{`{`, `{}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/transactions/x402-supported" {
					fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer s.Close()
			if _, err := client(t, s).Sign(context.Background(), required(), buyer.SignOptions{}); err == nil {
				t.Fatal("accepted invalid creation response")
			}
		})
	}
}

func TestApprovedInitialStatusPollsImmediately(t *testing.T) {
	var polls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transactions/x402-supported":
			fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
		case "/v1/transactions/x402":
			fmt.Fprint(w, `{"transactionId":"txn","approvalId":"approval","approvalStatus":"APPROVED"}`)
		default:
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"status":"PENDING"}`)
			} else {
				ready(w)
			}
		}
	}))
	defer s.Close()
	c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, PollInterval: time.Hour, WaitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepare(t, c).Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 {
		t.Fatal("incorrect poll count")
	}
}

func TestCancellationDuringBalanceRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/transactions/x402-supported" {
			fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
			return
		}
		cancel()
		<-r.Context().Done()
	}))
	defer s.Close()
	r := required()
	r.Accepts = append(r.Accepts, r.Accepts[0])
	if _, err := client(t, s).Select(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestExternalPolicyAndBeforeFailure(t *testing.T) {
	s, _, _ := server(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected poll") })
	sentinel := errors.New("rejected")
	r := required()
	r.Accepts[0].Network = "external:1"
	for _, policy := range []bool{false, true} {
		options := buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: externalStub{}}
		if policy {
			options.Policies = []buyer.Policy{func(context.Context, []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
				return nil, sentinel
			}}
		} else {
			options.Hooks = []buyer.Hooks{{Before: func(context.Context, x402.PaymentRequired) error { return sentinel }}}
		}
		c, err := buyer.New(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Sign(context.Background(), r, buyer.SignOptions{}); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
	}
}
