package inflow_test

import (
	"context"
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
	"github.com/inflowpayai/inflow-go/mpp"
	mppbuyer "github.com/inflowpayai/inflow-go/mpp/buyer"
	mppseller "github.com/inflowpayai/inflow-go/mpp/seller"
	xbuyer "github.com/inflowpayai/inflow-go/x402/buyer"
	xseller "github.com/inflowpayai/inflow-go/x402/seller"
)

type publicBoundary struct {
	name, method, path string
	bearer             bool
	call               func(context.Context, inflow.Options) error
}

func publicBoundaries() []publicBoundary {
	return []publicBoundary{
		{"mpp-buyer", "POST", "/v1/transactions/mpp", true, func(ctx context.Context, options inflow.Options) error {
			client, err := mppbuyer.New(mppbuyer.Options{Options: options})
			if err != nil {
				return err
			}
			_, err = client.Prepare(ctx, mpp.Challenge{ID: "test", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30"}, mppbuyer.PaymentOptions{})
			return err
		}},
		{"mpp-seller", "GET", "/v1/mpp/config", true, func(ctx context.Context, options inflow.Options) error {
			client, err := mppseller.New(options)
			if err != nil {
				return err
			}
			return client.Load(ctx)
		}},
		{"x402-buyer", "GET", "/v1/transactions/x402-supported", true, func(ctx context.Context, options inflow.Options) error {
			client, err := xbuyer.New(xbuyer.Options{Options: options})
			if err != nil {
				return err
			}
			_, err = client.Supported(ctx)
			return err
		}},
		{"x402-seller", "GET", "/v1/x402/config", false, func(ctx context.Context, options inflow.Options) error {
			client, err := xseller.New(options)
			if err != nil {
				return err
			}
			_, err = client.Config(ctx)
			return err
		}},
	}
}

type boundaryTransport func(*http.Request) (*http.Response, error)

func (f boundaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublicEnvironmentSelection(t *testing.T) {
	for _, boundary := range publicBoundaries() {
		for _, environment := range []struct {
			name        string
			environment inflow.Environment
			base, want  string
		}{
			{"default", "", "", "https://api.inflowpay.ai"},
			{"production", inflow.Production, "", "https://api.inflowpay.ai"},
			{"sandbox", inflow.Sandbox, "", "https://sandbox.inflowpay.ai"},
			{"override", inflow.Sandbox, "https://local.example/prefix/", "https://local.example/prefix"},
		} {
			t.Run(boundary.name+"/"+environment.name, func(t *testing.T) {
				calls := 0
				err := boundary.call(context.Background(), inflow.Options{Environment: environment.environment, BaseURL: environment.base, APIKey: "test-only-key", Transport: boundaryTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != environment.want+boundary.path || r.Method != boundary.method || r.Header.Get("X-API-Key") != "test-only-key" {
						t.Errorf("wrong destination or authentication: %s %s", r.Method, r.URL)
					}
					if r.Body != nil {
						r.Body.Close()
					}
					return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
				})})
				var api *inflow.APIError
				if !errors.As(err, &api) || api.HTTPStatus != 401 || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			})
		}
	}
}

func TestPublicErrorsAndRedirectIsolation(t *testing.T) {
	for _, boundary := range publicBoundaries() {
		for _, bearer := range []bool{false, true} {
			if bearer && !boundary.bearer {
				continue
			}
			for _, status := range []int{401, 403, 301, 302, 303, 307, 308} {
				t.Run(fmt.Sprintf("%s/bearer=%t/status=%d", boundary.name, bearer, status), func(t *testing.T) {
					var leaked, calls atomic.Int32
					target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
					defer target.Close()
					const secret = "test-only-secret"
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Method != boundary.method || r.URL.Path != boundary.path {
							t.Error("wrong endpoint", r.Method, r.URL)
						}
						key, token := secret, ""
						if bearer {
							key, token = "", "Bearer "+secret
						}
						if r.Header.Get("X-API-Key") != key || r.Header.Get("Authorization") != token {
							t.Error("wrong authentication")
						}
						io.Copy(io.Discard, r.Body)
						w.Header().Set("Location", target.URL)
						w.Header().Set("X-Request-ID", "test-correlation")
						w.Header().Set("Set-Cookie", "session="+secret)
						w.Header().Set("X-Diagnostic", secret)
						w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="/.well-known/oauth-protected-resource"`)
						w.WriteHeader(status)
						if status != 401 {
							fmt.Fprintf(w, `{"errors":[{"code":"SELLER_ACCOUNT_REQUIRED","message":"A Seller account is required."}],"nested":{"signature":"proof","access_token":%q},"echo":%q}`, secret, secret)
						}
					}))
					defer server.Close()
					options := inflow.Options{BaseURL: server.URL, APIKey: secret}
					if bearer {
						options.APIKey = ""
						options.AccessToken = func(context.Context) (string, error) { return secret, nil }
					}
					err := boundary.call(context.Background(), options)
					var api *inflow.APIError
					if !errors.As(err, &api) || api.HTTPStatus != status || api.Endpoint != boundary.path || api.RequestID != "test-correlation" {
						t.Fatalf("lost error: %#v", err)
					}
					if status == 401 {
						if api.Code != "UNEXPECTED_ERROR" || api.Message != "request failed" || api.Headers.Get("WWW-Authenticate") == "" {
							t.Fatal("lost authentication failure", api)
						}
					} else if api.Code != "SELLER_ACCOUNT_REQUIRED" || api.Message != "A Seller account is required." {
						t.Fatal("lost structured failure", api)
					}
					encoded, e := json.Marshal(api)
					if e != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "proof") || api.Headers.Get("Set-Cookie") != "" {
						t.Fatal("unsafe error", string(encoded), e)
					}
					if api.Headers.Get("Location") != target.URL || calls.Load() != 1 || leaked.Load() != 0 {
						t.Fatal("redirect or retry changed", calls.Load(), leaked.Load())
					}
				})
			}
		}
	}
}
