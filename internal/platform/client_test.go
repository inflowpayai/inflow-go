package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newClient(t *testing.T, options inflow.Options) *Client {
	t.Helper()
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func apiError(t *testing.T, err error, code string, status int) *inflow.APIError {
	t.Helper()
	var e *inflow.APIError
	if !errors.As(err, &e) || e.Code != code || e.HTTPStatus != status {
		t.Fatalf("expected %s/%d, got %#v", code, status, err)
	}
	return e
}

func TestConfiguration(t *testing.T) {
	for _, tc := range []struct {
		options inflow.Options
		base    string
	}{
		{inflow.Options{}, "https://api.inflowpay.ai"},
		{inflow.Options{Environment: inflow.Production}, "https://api.inflowpay.ai"},
		{inflow.Options{Environment: inflow.Sandbox}, "https://sandbox.inflowpay.ai"},
		{inflow.Options{Environment: inflow.Sandbox, BaseURL: "http://localhost:1234/prefix///"}, "http://localhost:1234/prefix"},
	} {
		c := newClient(t, tc.options)
		if c.baseURL != tc.base || c.options.Timeout != 30*time.Second {
			t.Fatalf("unexpected config: %s %s", c.baseURL, c.options.Timeout)
		}
	}
	for _, options := range []inflow.Options{
		{Environment: "unknown"}, {BaseURL: "relative"}, {BaseURL: "ftp://example.org"}, {BaseURL: "http://"},
		{BaseURL: "https://user:password@example.org"}, {BaseURL: "https://example.org/?q=1"},
		{BaseURL: "https://example.org/?"}, {BaseURL: "https://example.org/#fragment"}, {BaseURL: "%"},
		{APIKey: " "}, {APIKey: "key\nvalue"}, {APIKey: "é"}, {Timeout: -1},
		{APIKey: "key", AccessToken: func(context.Context) (string, error) { return "token", nil }},
	} {
		if _, err := New(options); err == nil {
			t.Fatalf("accepted invalid options")
		}
	}
	newClient(t, inflow.Options{AccessToken: func(context.Context) (string, error) { t.Fatal("constructor called provider"); return "", nil }, Transport: transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("constructor requested network"); return nil, nil })})
}

func TestRequestAuthenticationAndBody(t *testing.T) {
	for _, kind := range []string{"anonymous", "key", "bearer"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.String() != "/prefix/v1/test?q=1" || r.Method != "POST" {
					t.Errorf("unexpected route: %s %s", r.Method, r.URL)
				}
				key, bearer := "", ""
				if kind == "key" {
					key = "test-key"
				}
				if kind == "bearer" {
					bearer = "Bearer test-token"
				}
				if r.Header.Get("X-API-Key") != key || r.Header.Get("Authorization") != bearer {
					t.Error("wrong authentication")
				}
				if r.Header.Get("User-Agent") != "inflow-go/devel (go)" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "test-id" {
					t.Error("wrong headers")
				}
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"amount":"100"}` {
					t.Errorf("body %s", data)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer server.Close()
			options := inflow.Options{BaseURL: server.URL + "/prefix"}
			if kind == "key" {
				options.APIKey = "test-key"
			}
			if kind == "bearer" {
				options.AccessToken = func(context.Context) (string, error) { return "test-token", nil }
			}
			headers := http.Header{"Idempotency-Key": {"test-id"}}
			response, err := newClient(t, options).Do(context.Background(), Request{Method: "POST", Path: "/v1/test?q=1", Body: map[string]string{"amount": "100"}, Headers: headers})
			if err != nil {
				t.Fatal(err)
			}
			value, err := Decode[map[string]bool](response)
			if err != nil || !value["ok"] {
				t.Fatalf("decode %v %v", value, err)
			}
			if len(headers) != 1 {
				t.Fatal("mutated input headers")
			}
		})
	}
}

func TestLocalFailuresDoNotSend(t *testing.T) {
	c := newClient(t, inflow.Options{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected request")
		return nil, errors.New("unexpected")
	})})
	for _, request := range []Request{
		{Path: "relative"}, {Path: "//other.example"}, {Path: "https://other.example"}, {Path: "/foo#fragment"}, {Path: "/%"},
		{Path: "/", Retries: -1}, {Path: "/", Body: make(chan int)}, {Path: "/", Body: strings.Repeat("x", maxBodyBytes)},
		{Path: "/", Method: "invalid method"}, {Path: "/", Headers: http.Header{"authorization": {"secret"}}},
		{Path: "/", Headers: http.Header{"x-api-key": {"secret"}}}, {Path: "/", Headers: http.Header{"cookie": {"secret"}}},
	} {
		if _, err := c.Do(context.Background(), request); err == nil {
			t.Fatalf("accepted invalid request %s", request.Path)
		}
	}
}

func TestProviderFailureAndCancellation(t *testing.T) {
	want := errors.New("provider failed")
	for _, tc := range []struct {
		token string
		err   error
	}{{"", want}, {"", nil}, {"bad\r\ntoken", nil}} {
		calls := 0
		c := newClient(t, inflow.Options{AccessToken: func(context.Context) (string, error) { calls++; return tc.token, tc.err }})
		_, err := c.Do(context.Background(), Request{Path: "/v1/test", Retries: 3})
		if err == nil || calls != 1 || (tc.err != nil && err != want) {
			t.Fatalf("provider result %v, calls %d", err, calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newClient(t, inflow.Options{AccessToken: func(context.Context) (string, error) { t.Fatal("provider after cancellation"); return "", nil }})
	_, err := c.Do(ctx, Request{Path: "/"})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c = newClient(t, inflow.Options{Timeout: time.Millisecond, AccessToken: func(ctx context.Context) (string, error) { <-ctx.Done(); return "token", nil }})
	_, err = c.Do(context.Background(), Request{Path: "/"})
	apiError(t, err, "TIMEOUT", 0)
}

func TestRedirectsDoNotForwardCredentials(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		_, err := newClient(t, inflow.Options{BaseURL: server.URL, APIKey: "secret"}).Do(context.Background(), Request{Method: "POST", Path: "/v1/payment", Body: map[string]string{"credential": "private"}, Retries: 3})
		apiError(t, err, "UNEXPECTED_ERROR", status)
		server.Close()
	}
	if forwarded.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestRetryStatusesAndTokenRotation(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 412, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls, tokens atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", n) {
					t.Error("token was not refreshed")
				}
				if n == 1 {
					w.WriteHeader(status)
				} else {
					fmt.Fprint(w, `{}`)
				}
			}))
			defer server.Close()
			c := newClient(t, inflow.Options{BaseURL: server.URL, AccessToken: func(context.Context) (string, error) { return fmt.Sprintf("token-%d", tokens.Add(1)), nil }})
			_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Retries: 1})
			retry := status == 429 || status == 502 || status == 503 || status == 504
			if retry {
				if err != nil || calls.Load() != 2 {
					t.Fatalf("expected retry %v %d", err, calls.Load())
				}
			} else {
				apiError(t, err, "UNEXPECTED_ERROR", status)
				if calls.Load() != 1 {
					t.Fatal("retried rejection")
				}
			}
		})
	}
}

func TestRetryLimitAndMutationDefault(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Retries: 99})
	apiError(t, err, "UNEXPECTED_ERROR", 503)
	if calls.Load() != 4 {
		t.Fatalf("retry cap: %d", calls.Load())
	}
	calls.Store(0)
	_, err = c.Do(context.Background(), Request{Method: "POST", Path: "/", Body: map[string]int{"amount": 1}})
	apiError(t, err, "UNEXPECTED_ERROR", 503)
	if calls.Load() != 1 {
		t.Fatal("retried mutation by default")
	}
}

func TestNetworkRetryAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL})
	if _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Retries: 1}); err != nil || calls.Load() != 2 {
		t.Fatalf("network retry %v %d", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	c = newClient(t, inflow.Options{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("secret network diagnostics")
	})})
	_, err := c.Do(ctx, Request{Path: "/", Retries: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	c = newClient(t, inflow.Options{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		time.AfterFunc(10*time.Millisecond, cancel)
		return nil, errors.New("secret network diagnostics")
	})})
	_, err = c.Do(ctx, Request{Path: "/", Retries: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTimeoutCoversResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL, Timeout: 20 * time.Millisecond})
	_, err := c.Do(context.Background(), Request{Path: "/"})
	apiError(t, err, "TIMEOUT", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lost deadline cause")
	}
}

func TestResponseBoundsAndDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxBodyBytes+1)) }))
	defer server.Close()
	_, err := newClient(t, inflow.Options{BaseURL: server.URL}).Do(context.Background(), Request{Path: "/", Retries: 3})
	apiError(t, err, "RESPONSE_TOO_LARGE", 200)
	for _, body := range []string{"", `{"count":9007199254740993}`, `{bad`, `{} {}`} {
		value, err := Decode[struct{ Count int64 }](&Response{Body: []byte(body)})
		if body == "" && (err != nil || value.Count != 0) {
			t.Fatal(value, err)
		}
		if strings.Contains(body, "9007199254740993") && (err != nil || value.Count != 9007199254740993) {
			t.Fatal(value, err)
		}
		if (body == `{bad` || body == `{} {}`) && err == nil {
			t.Fatal("accepted malformed JSON")
		}
	}
}

func TestSharedClientConcurrentRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL, APIKey: "test-key"})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := c.Do(context.Background(), Request{Path: "/"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestTruncatedBodyAndMutationReplay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "short")
	}))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL})
	_, err := c.Do(context.Background(), Request{Method: "POST", Path: "/payment"})
	apiError(t, err, "NETWORK_ERROR", 0)
	if calls.Load() != 1 {
		t.Fatal("retried an uncertain mutation")
	}
	_, err = c.Do(context.Background(), Request{Method: "GET", Path: "/state", Retries: 1})
	apiError(t, err, "NETWORK_ERROR", 0)
	if calls.Load() != 3 {
		t.Fatal("did not retry read")
	}
}

type countedBody struct{ marshals int }

func TestTransportDoesNotImplicitlyReplayMutation(t *testing.T) {
	for _, body := range []any{nil, map[string]string{"amount": "1"}} {
		t.Run(fmt.Sprintf("body-%v", body != nil), func(t *testing.T) {
			var payments atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, `{}`)
					return
				}
				io.Copy(io.Discard, r.Body)
				if payments.Add(1) == 1 {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				}
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			c := newClient(t, inflow.Options{BaseURL: server.URL})
			if _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/warmup"}); err != nil {
				t.Fatal(err)
			}
			_, err := c.Do(context.Background(), Request{Method: "POST", Path: "/payment", Body: body, Headers: http.Header{"Idempotency-Key": {"test-id"}}})
			if payments.Load() != 1 {
				t.Fatalf("transport replayed mutation %d times", payments.Load())
			}
			apiError(t, err, "NETWORK_ERROR", 0)
		})
	}
}

func (b *countedBody) MarshalJSON() ([]byte, error) {
	b.marshals++
	return []byte(fmt.Sprintf(`{"attempt":%d}`, b.marshals)), nil
}

func TestExplicitRetryReusesSerializedBodyAndHeaders(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != `{"attempt":1}` || r.Header.Get("Idempotency-Key") != "test-id" {
			t.Error("retry altered body or idempotency key")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	body := &countedBody{}
	response, err := newClient(t, inflow.Options{BaseURL: server.URL}).Do(context.Background(), Request{Method: "POST", Path: "/broadcast", Body: body, Headers: http.Header{"Idempotency-Key": {"test-id"}}, Retries: 1})
	if err != nil || response.Status != 204 || body.marshals != 1 || calls.Load() != 2 {
		t.Fatal(response, err, body.marshals, calls.Load())
	}
}

func TestCallerDeadlineAndResponseClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	c := newClient(t, inflow.Options{Timeout: time.Hour, Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})})
	_, err := c.Do(ctx, Request{Path: "/", Retries: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for _, status := range []int{200, 400} {
		body := &trackedBody{Reader: strings.NewReader("{}")}
		c := newClient(t, inflow.Options{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
		})})
		c.Do(context.Background(), Request{Path: "/"})
		if !body.closed {
			t.Fatal("response body was not closed")
		}
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestStructuredErrorsAndRedaction(t *testing.T) {
	for _, tc := range []struct{ body, code, message string }{
		{`{"errors":[{"code":"SELLER_ACCOUNT_REQUIRED","message":"Seller account required."}],"id":"id"}`, "SELLER_ACCOUNT_REQUIRED", "Seller account required."},
		{`{"code":"INVALID","message":"invalid input"}`, "INVALID", "invalid input"},
		{`{"type":"https://example.org/problem","detail":"Problem detail","status":400}`, "UNEXPECTED_ERROR", "Problem detail"},
		{`{"errors":[null],"message":false}`, "UNEXPECTED_ERROR", "request failed"},
		{"", "UNEXPECTED_ERROR", "request failed"}, {`{bad`, "UNEXPECTED_ERROR", "request failed"},
		{`{"code":"WRONG"} {}`, "UNEXPECTED_ERROR", "request failed"},
	} {
		err := responseError("/", 400, nil, []byte(tc.body))
		if err.Code != tc.code || err.Error() != tc.message {
			t.Fatalf("%s: %#v", tc.body, err)
		}
	}
	headers := http.Header{"Authorization": {"secret"}, "Set-Cookie": {"cookie"}, "X-Request-Id": {"id"}, "Other": {"echo secret"}}
	body := []byte(`{"errors":[{"code":"BAD","message":"echo secret"}],"access_token":"private","nested":[{"apiKey":"private"}],"count":9007199254740993}`)
	err := responseError("/", 400, headers, body, "secret", "")
	if err.RequestID != "id" || err.Message != "echo [REDACTED]" || err.Headers.Get("Authorization") != "" || err.Headers.Get("Set-Cookie") != "" || err.Headers.Get("Other") != "echo [REDACTED]" {
		t.Fatalf("unsafe error: %#v", err)
	}
	encoded, _ := json.Marshal(err.Body)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret") || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("unsafe/rounded body %s", encoded)
	}
	if !reflect.DeepEqual(headers["Other"], []string{"echo secret"}) {
		t.Fatal("mutated response headers")
	}
}
