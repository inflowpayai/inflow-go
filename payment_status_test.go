package inflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	mb "github.com/inflowpayai/inflow-go/mpp/buyer"
	xb "github.com/inflowpayai/inflow-go/x402/buyer"
)

type statusReader interface {
	PaymentStatus(context.Context, string, inflow.PaymentStatusOptions) (inflow.PaymentStatus, error)
}

func statusClient(t *testing.T, protocol string, options inflow.Options) statusReader {
	t.Helper()
	if protocol == "mpp" {
		c, err := mb.New(mb.Options{Options: options})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c, err := xb.New(xb.Options{Options: options})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPaymentStatusSnapshots(t *testing.T) {
	for _, protocol := range []string{"mpp", "x402"} {
		t.Run(protocol, func(t *testing.T) {
			var calls atomic.Int32
			values := []inflow.PaymentStatus{
				{TransactionID: "original", Status: "PENDING", NextAction: &inflow.PaymentAction{Type: "authenticate_card", URL: "https://dashboard.example/verify"}},
				{TransactionID: "original", Status: "SETTLED"},
				{TransactionID: "original", Status: "FUTURE_STATUS", NextAction: &inflow.PaymentAction{Type: "future_action", URL: "https://dashboard.example/step"}},
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.RequestURI != "/v1/transactions/a%2Fb%3Fq%3Dx%23fragment" || r.Header.Get("X-API-Key") != "test-key" {
					t.Errorf("unexpected request: %s %s", r.Method, r.RequestURI)
				}
				i := int(calls.Add(1)) - 1
				if i >= len(values) {
					t.Error("extra request")
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(values[i])
			}))
			defer s.Close()
			client := statusClient(t, protocol, inflow.Options{BaseURL: s.URL, APIKey: "test-key"})
			options := inflow.PaymentStatusOptions{}
			for _, want := range values {
				got, err := client.PaymentStatus(context.Background(), "a/b?q=x#fragment", options)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("got %#v, %v; want %#v", got, err, want)
				}
			}
			if calls.Load() != 3 || options.Retries != 0 {
				t.Fatal("read count or options changed")
			}
		})
	}
}

func TestPaymentStatusFailureAndRetry(t *testing.T) {
	for _, protocol := range []string{"mpp", "x402"} {
		for _, retries := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", protocol, retries), func(t *testing.T) {
				var calls, tokens atomic.Int32
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					i := calls.Add(1)
					if r.Method != "GET" || r.URL.Path != "/v1/transactions/original" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", i) || r.Header.Get("X-API-Key") != "" {
						t.Error("wrong status request")
					}
					if i == 1 {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"code":"UNAVAILABLE","message":"Try later"}`)
						return
					}
					fmt.Fprint(w, `{"transactionId":"original","status":"GENERAL_ERROR"}`)
				}))
				defer s.Close()
				client := statusClient(t, protocol, inflow.Options{BaseURL: s.URL, AccessToken: func(context.Context) (string, error) { return fmt.Sprintf("token-%d", tokens.Add(1)), nil }})
				got, err := client.PaymentStatus(context.Background(), "original", inflow.PaymentStatusOptions{Retries: retries})
				if retries == 0 {
					var failure *inflow.APIError
					if !errors.As(err, &failure) || failure.HTTPStatus != 503 || !reflect.DeepEqual(failure.Body, map[string]any{"code": "UNAVAILABLE", "message": "Try later"}) {
						t.Fatalf("error: %#v", err)
					}
				} else if err != nil || got.Status != "GENERAL_ERROR" {
					t.Fatalf("result: %#v %v", got, err)
				}
				if calls.Load() != int32(retries+1) || tokens.Load() != calls.Load() {
					t.Fatal("incorrect retries or token refresh")
				}
			})
		}
	}
}

func TestPaymentStatusRedirectAndInvalidResponse(t *testing.T) {
	for _, protocol := range []string{"mpp", "x402"} {
		for _, redirect := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", protocol, redirect), func(t *testing.T) {
				var targetCalls atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
				defer target.Close()
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if redirect {
						http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					} else {
						fmt.Fprint(w, `{"status":`)
					}
				}))
				defer s.Close()
				client := statusClient(t, protocol, inflow.Options{BaseURL: s.URL, APIKey: "secret"})
				_, err := client.PaymentStatus(context.Background(), "original", inflow.PaymentStatusOptions{})
				if err == nil || targetCalls.Load() != 0 {
					t.Fatal("accepted failure or followed redirect")
				}
				if redirect {
					var failure *inflow.APIError
					if !errors.As(err, &failure) || failure.HTTPStatus != 307 {
						t.Fatalf("redirect: %v", err)
					}
				}
			})
		}
	}
}

func TestPaymentStatusCancellation(t *testing.T) {
	for _, protocol := range []string{"mpp", "x402"} {
		t.Run(protocol, func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 || r.Method != "GET" {
					t.Error("unexpected request")
					w.WriteHeader(500)
					return
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer s.Close()
			client := statusClient(t, protocol, inflow.Options{BaseURL: s.URL})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := client.PaymentStatus(ctx, "original", inflow.PaymentStatusOptions{}); result <- err }()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("request ended before reaching server: %v", err)
			}
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("cancelled payment instead of request")
			}
		})
	}
}
