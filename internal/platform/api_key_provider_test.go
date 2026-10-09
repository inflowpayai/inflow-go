package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestAPIKeyProviderConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	var received sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, loaded := received.LoadOrStore(r.Header.Get("X-API-Key"), true); loaded {
			t.Error("reused provider result")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(inflow.Options{BaseURL: server.URL, APIKeyProvider: func(context.Context) (string, error) { return fmt.Sprintf("key-%d", calls.Add(1)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := client.Do(context.Background(), Request{Method: "GET", Path: "/example"}); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 4 {
		t.Fatalf("provider calls: %d", calls.Load())
	}
}

func TestAPIKeyProviderDeadline(t *testing.T) {
	calls := 0
	client, err := New(inflow.Options{Timeout: time.Millisecond, APIKeyProvider: func(ctx context.Context) (string, error) { calls++; <-ctx.Done(); return "", ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Do(context.Background(), Request{Method: "GET", Path: "/example", Retries: 3}); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.Do(ctx, Request{Method: "GET", Path: "/example"}); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestAPIKeyProvider(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fmt.Sprintf("key-%d", calls.Load()) || r.Header.Get("Authorization") != "" {
			t.Error("unexpected authentication headers")
		}
		if calls.Load() == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(inflow.Options{BaseURL: server.URL, APIKeyProvider: func(context.Context) (string, error) { return fmt.Sprintf("key-%d", calls.Add(1)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("provider called at construction")
	}
	for i := 0; i < 2; i++ {
		if _, err = client.Do(context.Background(), Request{Method: "GET", Path: "/example", Retries: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls: %d", calls.Load())
	}
}

func TestAPIKeyProviderFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	failure := errors.New("provider unavailable")
	for _, value := range []string{"", " ", "bad\nkey", "é", "failure", "cancel"} {
		t.Run(value, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, err := New(inflow.Options{BaseURL: server.URL, APIKeyProvider: func(context.Context) (string, error) {
				calls++
				if value == "failure" {
					return "", failure
				}
				if value == "cancel" {
					cancel()
					return "key", nil
				}
				return value, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Do(ctx, Request{Method: "GET", Path: "/example", Retries: 3})
			if err == nil || calls != 1 || requests.Load() != 0 {
				t.Fatalf("err=%v calls=%d requests=%d", err, calls, requests.Load())
			}
			if value == "failure" && !errors.Is(err, failure) {
				t.Fatal("provider error changed")
			}
		})
	}
	for _, opts := range []inflow.Options{
		{APIKey: "key", APIKeyProvider: func(context.Context) (string, error) { return "key", nil }},
		{APIKeyProvider: func(context.Context) (string, error) { return "key", nil }, AccessToken: func(context.Context) (string, error) { return "token", nil }},
	} {
		if _, err := New(opts); err == nil {
			t.Fatal("conflicting authentication accepted")
		}
	}
}
