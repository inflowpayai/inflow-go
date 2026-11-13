package requestpricing

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestRequestIsolation(t *testing.T) {
	for _, limit := range []int64{0, 4} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			request := httptest.NewRequest("POST", "/paid?price=1", strings.NewReader("body"))
			request.Header.Set("X-Price", "1")
			reader := &trackedBody{Reader: request.Body}
			request.Body = reader
			calls := 0
			handler, err := Handler(inflow.RequestPricingOptions{MaxBodyBytes: limit}, func(r *http.Request, body []byte) (string, error) {
				calls++
				if r.Body != http.NoBody || r.GetBody != nil {
					t.Fatal("callback has a readable request body")
				}
				if limit == 0 {
					if body != nil || reader.reads != 0 || reader.closed {
						t.Fatal("metadata pricing consumed the body")
					}
				} else {
					if string(body) != "body" || !reader.closed {
						t.Fatal("body was not read and closed")
					}
					body[0] = 'X'
				}
				r.Header.Set("X-Price", "99")
				r.URL.RawQuery = "price=99"
				return "resolved", nil
			}, func(_ context.Context, value string, body []byte) (http.Handler, error) {
				if value != "resolved" || (limit > 0 && string(body) != "body") {
					t.Fatal("callback mutation escaped")
				}
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Price") != "1" || r.URL.Query().Get("price") != "1" {
						t.Fatal("request metadata changed")
					}
					io.Copy(w, r.Body)
				}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || response.Body.String() != "body" || calls != 1 {
				t.Fatal(response, calls)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
	}{
		{"cancelled", 500}, {"resolver", 500}, {"cancel-in-resolver", 500},
		{"build", 500}, {"read", 400}, {"oversize", 413},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest("POST", "/", strings.NewReader("large")).WithContext(ctx)
			private := errors.New("private pricing detail")
			if scenario.name == "cancelled" {
				cancel()
			}
			if scenario.name == "read" {
				request.Body = &trackedBody{err: private}
			}
			limit := int64(10)
			if scenario.name == "oversize" {
				limit = 4
			}
			var reported error
			handler, err := Handler(inflow.RequestPricingOptions{MaxBodyBytes: limit, OnError: func(r *http.Request, err error) {
				if r != request {
					t.Fatal("wrong diagnostic request")
				}
				reported = err
			}}, func(*http.Request, []byte) (string, error) {
				switch scenario.name {
				case "resolver":
					return "", private
				case "cancel-in-resolver":
					cancel()
				case "build":
				default:
					t.Fatal("resolver reached after input failure")
				}
				return "", nil
			}, func(context.Context, string, []byte) (http.Handler, error) {
				if scenario.name != "build" {
					t.Fatal("build reached after pricing failure")
				}
				return nil, private
			})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != scenario.status || reported == nil || strings.Contains(response.Body.String(), "private") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response, reported)
			}
		})
	}
}

func TestOptionsAndEmptyBody(t *testing.T) {
	resolve := func(*http.Request, []byte) (int, error) { return 1, nil }
	build := func(context.Context, int, []byte) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
	}
	for _, limit := range []int64{-1, math.MaxInt64} {
		if _, err := Handler(inflow.RequestPricingOptions{MaxBodyBytes: limit}, resolve, build); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
	if _, err := Handler[int](inflow.RequestPricingOptions{}, nil, build); err == nil {
		t.Fatal("accepted nil resolver")
	}
	handler, err := Handler(inflow.RequestPricingOptions{MaxBodyBytes: 1}, resolve, build)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Body = nil
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 204 {
		t.Fatal(response)
	}
}

type trackedBody struct {
	io.Reader
	err    error
	reads  int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.reads++
	if b.err != nil {
		return 0, b.err
	}
	return b.Reader.Read(p)
}

func (b *trackedBody) Close() error { b.closed = true; return nil }
