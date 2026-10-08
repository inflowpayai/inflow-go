package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestPaymentStatusRequest(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		invalid    bool
	}{
		{"snapshot", `{"transactionId":"original","status":"SETTLED"}`, 200, false},
		{"http-error", `{}`, 503, true},
		{"invalid-json", `{`, 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.RequestURI != "/v1/transactions/a%2Fb%20%2B%3F%3D" {
					t.Errorf("request: %s %s", r.Method, r.RequestURI)
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer s.Close()
			value, err := newClient(t, inflow.Options{BaseURL: s.URL}).PaymentStatus(context.Background(), "a/b +?=", inflow.PaymentStatusOptions{})
			if (err != nil) != test.invalid || calls != 1 {
				t.Fatalf("error: %v, calls: %d", err, calls)
			}
			if !test.invalid && (value.TransactionID != "original" || value.Status != "SETTLED") {
				t.Fatalf("value: %#v", value)
			}
		})
	}
}
