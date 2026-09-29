package buyer_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func TestHTTPReplayIsolation(t *testing.T) {
	platform, creates, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL, APIKey: "platform-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	var requests, leaks atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1) }))
	defer target.Close()
	requiredJSON, _ := json.Marshal(required())
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer application" || r.Header.Get("X-API-Key") != "" {
			t.Error("authentication leak")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "request-body" {
			t.Errorf("body %s", body)
		}
		if r.Header.Get(x402.HeaderPaymentSignature) == "" {
			w.Header().Set(x402.HeaderPaymentRequired, base64.StdEncoding.EncodeToString(requiredJSON))
			w.WriteHeader(402)
			return
		}
		if r.Header.Get(x402.HeaderPaymentSignature) != "server-original-value" {
			t.Error("encoded value altered")
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer merchant.Close()
	request, _ := http.NewRequestWithContext(context.Background(), "POST", merchant.URL, strings.NewReader("request-body"))
	request.Header.Set("Authorization", "Bearer application")
	response, err := c.Do(request, buyer.SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 307 || requests.Load() != 2 || creates.Load() != 1 || leaks.Load() != 0 {
		t.Fatal("unsafe replay")
	}
	if request.Header.Get(x402.HeaderPaymentSignature) != "" {
		t.Fatal("caller header mutated")
	}
}

func TestHTTPRejectBeforeSigning(t *testing.T) {
	platform, creates, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	c := client(t, platform)
	raw, _ := json.Marshal(required())
	valid := base64.StdEncoding.EncodeToString(raw)
	for _, header := range []string{"", "!invalid", base64.StdEncoding.EncodeToString([]byte(`{`)), valid} {
		t.Run(header, func(t *testing.T) {
			merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if header != "" {
					w.Header().Set(x402.HeaderPaymentRequired, header)
				}
				w.WriteHeader(402)
			}))
			defer merchant.Close()
			request, _ := http.NewRequest("POST", merchant.URL, io.NopCloser(bytes.NewBufferString("body")))
			if _, err := c.Do(request, buyer.SignOptions{}); err == nil {
				t.Fatal("accepted unreplayable or malformed input")
			}
		})
	}
	if creates.Load() != 0 {
		t.Fatal("signed before validation")
	}
	if _, err := c.Do(nil, buyer.SignOptions{}); err == nil {
		t.Fatal("nil request")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(x402.HeaderPaymentRequired, valid)
		w.WriteHeader(402)
	}))
	defer s.Close()
	sentinel := errors.New("GetBody failed")
	for _, body := range []func() (io.ReadCloser, error){func() (io.ReadCloser, error) { return nil, sentinel }, func() (io.ReadCloser, error) { return nil, nil }} {
		request, _ := http.NewRequest("POST", s.URL, strings.NewReader("body"))
		request.GetBody = body
		if _, err := c.Do(request, buyer.SignOptions{}); err == nil {
			t.Fatal("ignored GetBody failure")
		}
	}
	request, _ := http.NewRequest("GET", s.URL, nil)
	if _, err := c.Do(request, buyer.SignOptions{PaymentID: "invalid"}); err == nil {
		t.Fatal("invalid identifier")
	}
}

type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }

func TestHTTPResponseAndBodyOwnership(t *testing.T) {
	platform, _, _ := server(t, func(w http.ResponseWriter, r *http.Request) { ready(w) })
	c := client(t, platform)
	raw, _ := json.Marshal(required())
	var calls atomic.Int32
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public" {
			fmt.Fprint(w, "public")
			return
		}
		if calls.Add(1)%2 == 1 {
			w.Header().Set(x402.HeaderPaymentRequired, base64.StdEncoding.EncodeToString(raw))
			w.WriteHeader(402)
			return
		}
		if r.Header.Get(x402.HeaderPaymentRequired) != "" || r.Header.Get(x402.HeaderPaymentResponse) != "" {
			t.Error("stale payment headers")
		}
		fmt.Fprint(w, "paid")
	}))
	defer merchant.Close()
	for _, path := range []string{"/public", "/paid"} {
		req, _ := http.NewRequest("GET", merchant.URL+path, nil)
		req.Header = nil
		res, err := c.Do(req, buyer.SignOptions{})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	req, _ := http.NewRequest("GET", merchant.URL+"/paid", nil)
	req.Header["payment-required"] = []string{"old"}
	req.Header["payment-response"] = []string{"old"}
	res, err := c.Do(req, buyer.SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	unused := &trackedBody{Reader: strings.NewReader("body")}
	req, _ = http.NewRequest("POST", merchant.URL+"/paid", strings.NewReader("body"))
	req.GetBody = func() (io.ReadCloser, error) { return unused, nil }
	if _, err = c.Do(req, buyer.SignOptions{PaymentID: "invalid"}); err == nil || !unused.closed.Load() {
		t.Fatal("unused replay body leaked")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(302)
	}))
	defer redirect.Close()
	req, _ = http.NewRequest("GET", redirect.URL, nil)
	res, err = c.Do(req, buyer.SignOptions{})
	if err != nil || res.StatusCode != 302 {
		t.Fatal(err)
	}
	res.Body.Close()
}
