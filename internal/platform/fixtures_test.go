package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
)

// runtime.json is the runtimeScenarios export from inflow-specs
// f20033742244dc4220e0cdc05db1e1eeeaccdb63/fixtures/runtime.mjs.
// These test private transport behavior, not the public payment adapters.
type scenario struct {
	Exchanges []struct {
		Request struct {
			Method  string
			Path    string
			Headers map[string]string
		}
		Response struct {
			Status  int
			Headers map[string]string
			JSON    json.RawMessage
		}
	}
}

func scenarios(t *testing.T) map[string]scenario {
	t.Helper()
	data, err := os.ReadFile("testdata/runtime.json")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]scenario
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureServer(t *testing.T, s scenario) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(s.Exchanges) {
			t.Error("extra HTTP request")
			w.WriteHeader(500)
			return
		}
		exchange := s.Exchanges[i]
		if r.Method != exchange.Request.Method || r.URL.String() != exchange.Request.Path {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		for name, value := range exchange.Request.Headers {
			if r.Header.Get(name) != value {
				t.Errorf("incorrect header %s", name)
			}
		}
		for _, name := range []string{"authorization", "x-api-key"} {
			if exchange.Request.Headers[name] == "" && r.Header.Get(name) != "" {
				t.Errorf("unexpected credential %s", name)
			}
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || len(data) != 0 {
			t.Error("unexpected body")
		}
		for name, value := range exchange.Response.Headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(exchange.Response.Status)
		if len(exchange.Response.JSON) > 0 {
			w.Write(exchange.Response.JSON)
		}
	}))
	t.Cleanup(func() {
		server.Close()
		if int(calls.Load()) != len(s.Exchanges) {
			t.Errorf("consumed %d of %d exchanges", calls.Load(), len(s.Exchanges))
		}
	})
	return server
}

func TestSharedRuntimeFixtures(t *testing.T) {
	for name, s := range scenarios(t) {
		t.Run(name, func(t *testing.T) {
			server := fixtureServer(t, s)
			headers := s.Exchanges[0].Request.Headers
			options := inflow.Options{BaseURL: server.URL, APIKey: headers["x-api-key"]}
			if bearer := headers["authorization"]; bearer != "" {
				options.AccessToken = func(context.Context) (string, error) { return strings.TrimPrefix(bearer, "Bearer "), nil }
			}
			c := newClient(t, options)
			for _, exchange := range s.Exchanges {
				response, err := c.Do(context.Background(), Request{Method: exchange.Request.Method, Path: exchange.Request.Path})
				if exchange.Response.Status >= 300 {
					var body struct {
						Errors []struct{ Code, Message string }
					}
					json.Unmarshal(exchange.Response.JSON, &body)
					code, message := "UNEXPECTED_ERROR", "request failed"
					if len(body.Errors) > 0 {
						code, message = body.Errors[0].Code, body.Errors[0].Message
					}
					e := apiError(t, err, code, exchange.Response.Status)
					if e.Message != message || e.Endpoint != exchange.Request.Path {
						t.Fatalf("error did not preserve response: %#v", e)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if response.Status != exchange.Response.Status {
						t.Fatal("wrong status")
					}
					if len(exchange.Response.JSON) == 0 {
						if len(response.Body) != 0 {
							t.Fatal("expected empty response")
						}
						continue
					}
					var want any
					json.Unmarshal(exchange.Response.JSON, &want)
					got, err := Decode[any](response)
					if err != nil || !reflect.DeepEqual(want, got) {
						t.Fatalf("body mismatch %v", err)
					}
				}
			}
		})
	}
}

func TestPollingUsesSharedApprovalFixtures(t *testing.T) {
	for _, status := range []string{"approved", "declined", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			s := scenarios(t)["approval.pending-to-"+status]
			server := fixtureServer(t, s)
			c := newClient(t, inflow.Options{BaseURL: server.URL, APIKey: "test-only-buyer-key"})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			value, err := Poll(ctx, func(ctx context.Context) (string, bool, time.Duration, error) {
				response, err := c.Do(ctx, Request{Method: "GET", Path: s.Exchanges[0].Request.Path})
				if err != nil {
					return "", false, 0, err
				}
				body, err := Decode[struct{ Status string }](response)
				return body.Status, body.Status != "PENDING", time.Millisecond, err
			})
			if status == "unavailable" {
				apiError(t, err, "APPROVAL_NOT_FOUND", 404)
			} else if err != nil || value != strings.ToUpper(status) {
				t.Fatal(value, err)
			}
		})
	}
}

func TestCancelRequestRemainsExplicit(t *testing.T) {
	var reads, cancels atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/approvals/test/cancel" {
			cancels.Add(1)
			w.WriteHeader(204)
			return
		}
		reads.Add(1)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := newClient(t, inflow.Options{BaseURL: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Do(ctx, Request{Method: "GET", Path: "/v1/approvals/test"}); done <- err }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if cancels.Load() != 0 || reads.Load() != 1 {
		t.Fatal("transport initiated another operation")
	}
	response, err := c.Do(context.Background(), Request{Method: "POST", Path: "/v1/approvals/test/cancel"})
	if err != nil || response.Status != 204 || cancels.Load() != 1 {
		t.Fatal(fmt.Sprint(response), err)
	}
}
