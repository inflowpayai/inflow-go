package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
)

func challengeHeader(t *testing.T, c mpp.Challenge) string {
	t.Helper()
	header, err := mpp.RenderChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func TestHTTPAuthenticationAndReplay(t *testing.T) {
	for _, auth := range []string{"Authorization", "authorization", "AUTHORIZATION", "separate", "empty"} {
		t.Run(auth, func(t *testing.T) {
			var payments, resources atomic.Int32
			header := challengeHeader(t, challenge())
			encoded := credential(t)
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payments.Add(1)
				if r.Header.Get("X-API-Key") != "test-only-buyer-key" || r.Header.Get("X-AEP-API-Key") != "" || r.Header.Get("Cookie") != "" {
					t.Error("platform credentials mixed with resource credentials")
				}
				fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
			}))
			defer platform.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := resources.Add(1)
				if r.Header.Get("X-API-Key") != "" {
					t.Error("platform key leaked to resource")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "request-body" {
					t.Errorf("body changed: %q", body)
				}
				if r.Header.Get("X-AEP-API-Key") != "app-key" || r.Header.Get("Cookie") != "app-session=value" {
					t.Error("application authentication lost")
				}
				if call == 1 {
					w.Header().Set("WWW-Authenticate", header)
					w.WriteHeader(402)
					return
				}
				if r.Header.Get("Authorization") != "Payment "+encoded {
					t.Error("payment credential changed")
				}
				for _, key := range []string{"Payment-Signature", "Payment-Required", "Payment-Response"} {
					if r.Header.Get(key) != "" {
						t.Error("stale payment header retained")
					}
				}
				w.Header().Set("Payment-Receipt", "synthetic-receipt")
				fmt.Fprint(w, "paid")
			}))
			defer resource.Close()
			request, _ := http.NewRequest("POST", resource.URL, strings.NewReader("request-body"))
			request.Header.Set("X-AEP-API-Key", "app-key")
			request.Header.Set("Cookie", "app-session=value")
			request.Header["payment-signature"] = []string{"stale"}
			request.Header["Payment-Required"] = []string{"stale"}
			request.Header["Payment-Response"] = []string{"stale"}
			if auth == "empty" {
				request.Header["authorization"] = []string{""}
			} else if auth != "separate" {
				request.Header[auth] = []string{"Bearer app-session"}
			}
			original := request.Header.Clone()
			response, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
			if auth != "separate" && auth != "empty" {
				if !errors.Is(err, buyer.ErrAuthorizationConflict) || response != nil {
					t.Fatal(response, err)
				}
				if payments.Load() != 0 || resources.Load() != 1 {
					t.Fatal("conflict caused payment")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 200 || string(body) != "paid" || response.Header.Get("Payment-Receipt") != "synthetic-receipt" {
					t.Fatal(response.Status, string(body))
				}
				if payments.Load() != 1 || resources.Load() != 2 {
					t.Fatal("incorrect request count")
				}
			}
			if !reflect.DeepEqual(request.Header, original) || request.GetBody == nil {
				t.Fatal("mutated caller request")
			}
		})
	}
}

func TestHTTPOrdinaryResponsesAndRedirects(t *testing.T) {
	for _, status := range []int{200, 401, 403, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var followed, payments atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
			defer target.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer app" {
					t.Error("unpaid authentication changed")
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, "original")
			}))
			defer resource.Close()
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { payments.Add(1) }))
			defer platform.Close()
			request, _ := http.NewRequest("GET", resource.URL, nil)
			request.Header.Set("Authorization", "Bearer app")
			response, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != status || string(body) != "original" {
				t.Fatal(response.Status, string(body))
			}
			if followed.Load() != 0 || payments.Load() != 0 {
				t.Fatal("followed redirect or created payment")
			}
		})
	}
}

func TestHTTPChallengeSelection(t *testing.T) {
	unknown := challenge()
	unknown.Method = "other"
	expired := challenge()
	past := "2000-01-01T00:00:00Z"
	expired.Expires = &past
	malformed := challenge()
	bad := "not-a-date"
	malformed.Expires = &bad
	future := challenge()
	later := "2099-01-01T00:00:00Z"
	future.Expires = &later
	for _, test := range []struct {
		name, header string
		code         buyer.ErrorCode
		codec        bool
		options      buyer.PaymentOptions
	}{
		{"no-payment", "Bearer realm=\"app\"", buyer.Unsupported, false, buyer.PaymentOptions{}},
		{"unsupported", challengeHeader(t, unknown), buyer.Unsupported, false, buyer.PaymentOptions{}},
		{"expired", challengeHeader(t, expired), buyer.Expired, false, buyer.PaymentOptions{}},
		{"malformed-expiry", challengeHeader(t, malformed), "", true, buyer.PaymentOptions{}},
		{"malformed-header", "Payment id=\"x\"", "", true, buyer.PaymentOptions{}},
		{"invalid-options", challengeHeader(t, challenge()), "", false, buyer.PaymentOptions{InstrumentID: "bad"}},
		{"second-supported", challengeHeader(t, unknown) + ", " + challengeHeader(t, future), "", false, buyer.PaymentOptions{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var payments atomic.Int32
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payments.Add(1)
				var input struct{ Challenge mpp.Challenge }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input.Challenge.Method != "inflow" {
					t.Error("selected unsupported method")
				}
				encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
				if err != nil {
					t.Error(err)
				}
				fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
			}))
			defer platform.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.Header.Get("Authorization"), "Payment ") {
					w.WriteHeader(204)
					return
				}
				w.Header().Set("WWW-Authenticate", test.header)
				w.WriteHeader(402)
			}))
			defer resource.Close()
			request, _ := http.NewRequest("GET", resource.URL, nil)
			response, err := newClient(t, platform, nil).Do(request, test.options)
			if test.name == "second-supported" {
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != 204 || payments.Load() != 1 {
					t.Fatal("did not pay selected offer")
				}
				return
			}
			if response != nil || err == nil {
				t.Fatal(response, err)
			}
			if test.code != "" {
				requireCode(t, err, test.code)
			}
			if test.codec {
				var codec *mpp.CodecError
				if !errors.As(err, &codec) {
					t.Fatal(err)
				}
			}
			if payments.Load() != 0 {
				t.Fatal("invalid challenge caused payment")
			}
		})
	}
}

func TestHTTPReplayMustBeReadyBeforePayment(t *testing.T) {
	for _, mode := range []string{"absent", "fails", "nil"} {
		t.Run(mode, func(t *testing.T) {
			var payments atomic.Int32
			header := challengeHeader(t, challenge())
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { payments.Add(1) }))
			defer platform.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", header)
				w.WriteHeader(402)
			}))
			defer resource.Close()
			request, _ := http.NewRequest("POST", resource.URL, io.NopCloser(strings.NewReader("body")))
			failure := errors.New("replay failure")
			if mode == "fails" {
				request.GetBody = func() (io.ReadCloser, error) { return nil, failure }
			}
			if mode == "nil" {
				request.GetBody = func() (io.ReadCloser, error) { return nil, nil }
			}
			_, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
			want := buyer.ErrBodyNotReplayable
			if mode == "fails" {
				want = failure
			}
			if !errors.Is(err, want) || payments.Load() != 0 {
				t.Fatal(err, payments.Load())
			}
		})
	}
}

type trackedBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *trackedBody) Close() error { b.closes.Add(1); return nil }

func TestHTTPPaymentFailureClosesReplay(t *testing.T) {
	for _, reply := range []string{"http-error", `{"state":"failed"}`} {
		t.Run(reply, func(t *testing.T) {
			header := challengeHeader(t, challenge())
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reply == "http-error" {
					w.WriteHeader(503)
				} else {
					fmt.Fprint(w, reply)
				}
			}))
			defer platform.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", header)
				w.WriteHeader(402)
			}))
			defer resource.Close()
			request, _ := http.NewRequest("POST", resource.URL, strings.NewReader("body"))
			body := &trackedBody{Reader: strings.NewReader("body")}
			request.GetBody = func() (io.ReadCloser, error) { return body, nil }
			_, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
			if err == nil || body.closes.Load() != 1 {
				t.Fatal(err, body.closes.Load())
			}
		})
	}
}

func TestHTTPPaidResponseIsNotAnotherPayment(t *testing.T) {
	for _, status := range []int{401, 402, 500, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var payments, requests, leaks atomic.Int32
			header := challengeHeader(t, challenge())
			encoded := credential(t)
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payments.Add(1)
				fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
			}))
			defer platform.Close()
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1) }))
			defer target.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", header)
				if requests.Add(1) == 1 {
					w.WriteHeader(402)
					return
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			defer resource.Close()
			request, _ := http.NewRequest("GET", resource.URL, nil)
			request.Header = nil
			response, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || payments.Load() != 1 || requests.Load() != 2 || leaks.Load() != 0 {
				t.Fatal("repeated payment or followed redirect")
			}
		})
	}
}

func TestHTTPInvalidRequestAndCancellation(t *testing.T) {
	client, err := buyer.New(buyer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Do(nil, buyer.PaymentOptions{}); err == nil {
		t.Fatal("accepted nil request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:1", nil)
	if _, err = client.Do(request, buyer.PaymentOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHTTPResourceTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := newClient(t, server, func(o *buyer.Options) { o.Timeout = 20 * time.Millisecond })
	request, _ := http.NewRequest("GET", server.URL, nil)
	if _, err := client.Do(request, buyer.PaymentOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestHTTPURLAuthenticationConflict(t *testing.T) {
	var payments atomic.Int32
	header := challengeHeader(t, challenge())
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { payments.Add(1) }))
	defer platform.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Error("missing application authentication")
		}
		w.Header().Set("WWW-Authenticate", header)
		w.WriteHeader(402)
	}))
	defer resource.Close()
	request, _ := http.NewRequest("GET", resource.URL, nil)
	request.URL.User = url.UserPassword("app-user", "app-password")
	_, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
	if !errors.Is(err, buyer.ErrAuthorizationConflict) || payments.Load() != 0 {
		t.Fatal(err, payments.Load())
	}
}

func TestHTTPPaidGETIsNotReplayedOnConnectionFailure(t *testing.T) {
	var payments, paidRequests atomic.Int32
	header := challengeHeader(t, challenge())
	encoded := credential(t)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payments.Add(1)
		fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
	}))
	defer platform.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", header)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(402)
			return
		}
		paidRequests.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer resource.Close()
	request, _ := http.NewRequest("GET", resource.URL, nil)
	request.Header.Set("Idempotency-Key", "test-only-key")
	_, err := newClient(t, platform, nil).Do(request, buyer.PaymentOptions{})
	if err == nil || payments.Load() != 1 || paidRequests.Load() != 1 {
		t.Fatal("payment replayed", err, payments.Load(), paidRequests.Load())
	}
}
