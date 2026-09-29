package buyer_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func TestLaterWaitCancellationLeavesOwnerRunning(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var polls atomic.Int32
	s, _, cancels := server(t, func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			ready(w)
		case <-r.Context().Done():
		}
	})
	p := prepare(t, client(t, s))
	owner := make(chan error, 1)
	go func() { _, err := p.Wait(context.Background()); owner <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Wait(ctx)
	// The owner is deliberately blocked. A joining caller must return without waiting for it.
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joining caller: %v", err)
	}
	if err = <-owner; err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 1 || cancels.Load() != 0 {
		t.Fatal("cancelled or restarted shared work")
	}
}

func TestExplicitCancelInterruptsPoll(t *testing.T) {
	entered := make(chan struct{})
	s, _, cancels := server(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	p := prepare(t, client(t, s))
	result := make(chan error, 1)
	go func() { _, err := p.Wait(context.Background()); result <- err }()
	<-entered
	if err := p.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	var e *buyer.Error
	if err := <-result; !errors.As(err, &e) || e.Code != "payment-cancelled" {
		t.Fatal(err)
	}
	if cancels.Load() != 1 {
		t.Fatal("missing server cancellation")
	}
}

func TestPollFailuresRemainResumable(t *testing.T) {
	for _, body := range []string{`not-json`, `{} {}`, `{"status":"EXPIRED"}`, `{"status":"GENERAL_ERROR"}`, `{"status":"INSUFFICIENT_FUNDS"}`} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			s, creates, cancels := server(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					fmt.Fprint(w, body)
				} else {
					ready(w)
				}
			})
			p := prepare(t, client(t, s))
			if _, err := p.Wait(context.Background()); err == nil {
				t.Fatal("accepted failed response")
			}
			if _, err := p.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if creates.Load() != 1 || cancels.Load() != 0 {
				t.Fatal("changed approval")
			}
		})
	}
}

func TestBeforeFailureCannotRecoverOrCreateApproval(t *testing.T) {
	s, creates, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	sentinel := errors.New("policy denied")
	c := client(t, s, buyer.Hooks{
		Before: func(context.Context, x402.PaymentRequired) error { return sentinel },
		Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) {
			t.Error("before failure entered recovery")
			return nil, nil
		},
	})
	if _, err := c.Sign(context.Background(), required(), buyer.SignOptions{}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := c.Prepare(context.Background(), required(), buyer.SignOptions{}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if creates.Load() != 0 {
		t.Fatal("created disallowed approval")
	}
}

func TestOneShotAfterFailureDoesNotCancelReadyPayment(t *testing.T) {
	s, _, cancels := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	sentinel := errors.New("observer failed")
	c := client(t, s, buyer.Hooks{After: func(context.Context, x402.PaymentRequired, x402.PaymentPayload) error { return sentinel }})
	if _, err := c.Sign(context.Background(), required(), buyer.SignOptions{}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if cancels.Load() != 0 {
		t.Fatal("cancelled completed payment after observer failure")
	}
}
