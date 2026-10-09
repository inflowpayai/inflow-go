package seller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
)

var validPayload = []byte(`{"x402Version":2,"payload":{"signature":"secret-signature"},"accepted":{},"extensions":{"unknown":{"keep":true}},"unknownTop":"keep"}`)
var validRequirements = []byte(`{"scheme":"exact"}`)

func TestPaymentRequest(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `[]`, `{"x402Version":1}`, `{"x402Version":2}`, `{"x402Version":2,"payload":null}`, `{"x402Version":2,"payload":{},"extensions":[]}`} {
		if _, err := paymentRequest([]byte(raw), validRequirements); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{`, `null`, `[]`} {
		if _, err := paymentRequest(validPayload, []byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{`{"x402Version":2,"payload":{"other":"data"}}`, `{"x402Version":2,"payload":{},"extensions":null}`} {
		first, err := paymentRequest([]byte(raw), validRequirements)
		if err != nil {
			t.Fatal(err)
		}
		second, err := paymentRequest([]byte(raw), validRequirements)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, encode(t, first), encode(t, second))
	}
	first, err := paymentRequest(validPayload, validRequirements)
	if err != nil {
		t.Fatal(err)
	}
	fields := first["paymentPayload"].(map[string]json.RawMessage)
	if string(fields["unknownTop"]) != `"keep"` {
		t.Fatal("lost unknown field")
	}
	second, err := paymentRequest(encode(t, fields), validRequirements)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, encode(t, first), encode(t, second))
	var extensions map[string]json.RawMessage
	json.Unmarshal(fields["extensions"], &extensions)
	if string(extensions["unknown"]) != `{"keep":true}` {
		t.Fatal("lost extension")
	}
	entry := x402.ReadPaymentIdentifier(extensions[x402.PaymentIdentifier])
	var id string
	json.Unmarshal(entry.Info["id"], &id)
	if !x402.ValidatePaymentID(id) {
		t.Fatal(id)
	}
}

func TestFacilitatorFailuresAndAnonymous(t *testing.T) {
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
			t.Error("anonymous credential leak")
		}
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"code":"failure"}`))
	}))
	defer server.Close()
	f, err := NewAnonymousFacilitator(inflow.Options{BaseURL: server.URL, APIKey: "secret", APIKeyProvider: func(context.Context) (string, error) { t.Error("provider called"); return "secret", nil }, AccessToken: func(context.Context) (string, error) { t.Error("token called"); return "secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = f.Verify(ctx, []byte("bad"), validRequirements); err == nil {
		t.Fatal("invalid input")
	}
	if _, err = f.Settle(ctx, []byte("bad"), validRequirements); err == nil {
		t.Fatal("invalid input")
	}
	if calls.Load() != 0 {
		t.Fatal("sent invalid request")
	}
	if _, err = f.Verify(ctx, validPayload, validRequirements); err == nil {
		t.Fatal("500 accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("verify retried")
	}
	if _, err = f.Settle(ctx, validPayload, validRequirements); err == nil {
		t.Fatal("500 accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("settle retried")
	}
	status.Store(200)
	if _, err = f.GetSupported(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.GetSupported(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("supported cache missed")
	}
}

func TestPendingCancellationAndDelays(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 5 * time.Second, "1": time.Second, "0": 0, "15": 5 * time.Second, "-1": 5 * time.Second, "1.5": 5 * time.Second, "tomorrow": 5 * time.Second, "9999999999999999999999999": 5 * time.Second} {
		if got := retryDelay(value); got != want {
			t.Errorf("%s: %v", value, got)
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(409)
		w.Write([]byte(`{"errorReason":"idempotency_pending"}`))
	}))
	defer server.Close()
	f, _ := NewFacilitator(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := f.Settle(ctx, validPayload, validRequirements); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("retried after cancel")
	}
}

func TestRedirectIsolationAndMalformedSuccess(t *testing.T) {
	var destination atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destination.Add(1) }))
	defer target.Close()
	var redirect atomic.Bool
	redirect.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, target.URL, 307)
		} else {
			io.WriteString(w, "{")
		}
	}))
	defer server.Close()
	f, _ := NewFacilitator(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	for _, run := range []func(context.Context, []byte, []byte) error{
		func(c context.Context, p, r []byte) error { _, e := f.Verify(c, p, r); return e },
		func(c context.Context, p, r []byte) error { _, e := f.Settle(c, p, r); return e },
	} {
		redirect.Store(true)
		var apiError *inflow.APIError
		if err := run(context.Background(), validPayload, validRequirements); !errors.As(err, &apiError) || apiError.HTTPStatus != 307 {
			t.Fatal(err)
		}
		redirect.Store(false)
		if err := run(context.Background(), validPayload, validRequirements); err == nil || !strings.Contains(err.Error(), "invalid InFlow JSON") {
			t.Fatal(err)
		}
	}
	if destination.Load() != 0 {
		t.Fatal("followed redirect")
	}
}
