package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
)

func TestPaymentStatusUsesCallerContext(t *testing.T) {
	c, err := buyer.New(buyer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.PaymentStatus(ctx, "original", inflow.PaymentStatusOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("status cancellation: %v", err)
	}
}

func TestFailurePreservesResponseTransactionID(t *testing.T) {
	for _, id := range []string{"", "failed-transaction"} {
		for _, pending := range []bool{false, true} {
			t.Run(id+map[bool]string{false: "/immediate", true: "/pending"}[pending], func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if pending && calls == 1 {
						json.NewEncoder(w).Encode(map[string]any{"state": "pending", "transactionId": "original"})
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"state": "failed", "transactionId": id, "problem": map[string]any{"type": "payment-failed", "title": "Rejected"}})
				}))
				defer s.Close()
				_, err := newClient(t, s, nil).Fulfil(context.Background(), challenge(), buyer.PaymentOptions{})
				var failure *buyer.Error
				if !errors.As(err, &failure) || failure.Code != buyer.Failed || failure.TransactionID != id || len(failure.Problem) == 0 {
					t.Fatalf("failure: %#v %v", failure, err)
				}
				want := 1
				if pending {
					want++
				}
				if calls != want {
					t.Fatalf("requests: %d", calls)
				}
			})
		}
	}
}
