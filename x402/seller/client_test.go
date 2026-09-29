package seller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestConstructionAndConfig(t *testing.T) {
	if _, err := New(inflow.Options{}); err == nil {
		t.Fatal("missing key")
	}
	if _, err := New(inflow.Options{APIKey: "key", BaseURL: "bad"}); err == nil {
		t.Fatal("bad URL")
	}
	if _, err := NewFacilitator(inflow.Options{}); err == nil {
		t.Fatal("missing key")
	}
	if _, err := NewFacilitator(inflow.Options{APIKey: "key", BaseURL: "bad"}); err == nil {
		t.Fatal("bad URL")
	}
	var calls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/v1/x402/config" {
			w.Write([]byte(`{"sellerId":"seller","paymentMethods":[{"extra":{"nested":"original"}}]}`))
		} else {
			w.Write([]byte(`{"signers":{"eip155:1":["exact"],"eip155:*": ["wildcard"],"eip155:2":[]}}`))
		}
	}))
	defer server.Close()
	c, err := New(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("constructor performed network I/O")
	}
	ctx := context.Background()
	a, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a.PaymentMethods[0].Extra["nested"] = "changed"
	b, err := c.Config(ctx)
	if err != nil || b.PaymentMethods[0].Extra["nested"] != "original" {
		t.Fatal("shared mutable config", err)
	}
	if calls.Load() != 1 {
		t.Fatal("cache missed")
	}
	if _, err = c.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	c.configuration.expires = time.Time{}
	if _, err = c.Config(ctx); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err = c.RefreshConfig(ctx); err == nil {
		t.Fatal("failed refresh accepted")
	}
	if value, err := c.Config(ctx); err != nil || value.SellerID != "seller" {
		t.Fatal("failed refresh replaced cache")
	}
	fail.Store(false)
	for network, want := range map[string]string{"eip155:1": "exact", "eip155:3": "wildcard", "other": "", "eip155:2": "", "solana:1": ""} {
		addresses, err := c.SignerAddresses(ctx, network)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			if len(addresses) != 0 {
				t.Fatal(addresses)
			}
		} else if len(addresses) != 1 || addresses[0] != want {
			t.Fatal(addresses)
		}
		if len(addresses) > 0 {
			addresses[0] = "mutated"
		}
	}
	fail.Store(true)
	c.supported.expires = time.Time{}
	if _, err = c.SignerAddresses(ctx, "eip155:1"); err == nil {
		t.Fatal("missing error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = c.Config(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSharedLoadCancellationAndRecovery(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			w.Write([]byte(`{"sellerId":"seller"}`))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	c, _ := New(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	owner, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := c.Config(owner); result <- err }()
	<-entered
	joinCtx, joinCancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() { _, err := c.Config(joinCtx); joined <- err }()
	joinCancel()
	if err := <-joined; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Cancelling a joining caller leaves the owner's request active.
	c.configuration.mu.Lock()
	pending := c.configuration.loading
	c.configuration.mu.Unlock()
	if pending == nil {
		t.Fatal("no pending load")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, e := c.Config(context.Background())
			if e != nil || v.SellerID != "seller" {
				t.Error(v, e)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestBadConfigurationResponse(t *testing.T) {
	var invalid atomic.Bool
	invalid.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalid.Load() {
			w.Write([]byte("{"))
		} else {
			w.Write([]byte(`{"sellerId":"ok"}`))
		}
	}))
	defer server.Close()
	c, _ := New(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	if _, err := c.Config(context.Background()); err == nil {
		t.Fatal("invalid response cached")
	}
	invalid.Store(false)
	if v, err := c.Config(context.Background()); err != nil || v.SellerID != "ok" {
		t.Fatal(v, err)
	}
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestJoiningCacheLoad(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "failure", false: "success"}[fail], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				if fail {
					w.WriteHeader(400)
				} else {
					w.Write([]byte(`{"sellerId":"seller"}`))
				}
			}))
			defer server.Close()
			client, _ := New(inflow.Options{BaseURL: server.URL, APIKey: "key"})
			owner := make(chan error, 1)
			go func() { _, err := client.Config(context.Background()); owner <- err }()
			<-entered
			ctx := &observedContext{Context: context.Background(), entered: make(chan struct{})}
			joined := make(chan error, 1)
			go func() {
				value, err := client.Config(ctx)
				if !fail && value.SellerID != "seller" {
					t.Error("missing joined result")
				}
				joined <- err
			}()
			<-ctx.entered
			close(release)
			if err := <-owner; (err != nil) != fail {
				t.Fatal(err)
			}
			if err := <-joined; (err != nil) != fail {
				t.Fatal(err)
			}
		})
	}
}

func TestCancelJoinedCacheLoad(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write([]byte(`{"sellerId":"seller"}`))
	}))
	defer server.Close()
	client, _ := New(inflow.Options{BaseURL: server.URL, APIKey: "key"})
	owner := make(chan error, 1)
	go func() { _, err := client.Config(context.Background()); owner <- err }()
	<-entered
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedContext{Context: base, entered: make(chan struct{})}
	joined := make(chan error, 1)
	go func() { _, err := client.Config(ctx); joined <- err }()
	<-ctx.entered
	cancel()
	if err := <-joined; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
}
