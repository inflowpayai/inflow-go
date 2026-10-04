package seller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jwk(id string) string {
	return fmt.Sprintf(`{"kid":%q,"kty":"OKP","crv":"Ed25519","alg":"ed25519","x":%q}`, id, base64.RawURLEncoding.EncodeToString(publicKey))
}
func keySet(id string) string { return `{"keys":[` + jwk(id) + `]}` }

func TestResolverCacheLifecycle(t *testing.T) {
	var payload string = keySet("first")
	status := 200
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.Header.Get("Accept") != "application/json" {
			t.Error("request")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, payload)
	}))
	defer server.Close()
	now := fixedClock()
	resolver := NewVisaKeyResolver(KeyResolverOptions{URL: server.URL, Clock: func() time.Time { return now }, CacheTTL: time.Second, CacheMaxAge: 3 * time.Second})
	ctx := context.Background()
	key, err := resolver.Resolve(ctx, "first", "ed25519")
	if err != nil || string(key) != string(publicKey) {
		t.Fatalf("%v %v", key, err)
	}
	key[0] ^= 255
	key, err = resolver.Resolve(ctx, "first", "ed25519")
	if err != nil || string(key) != string(publicKey) || calls != 1 {
		t.Fatal("mutated cache", err, calls)
	}
	for range 2 {
		key, err = resolver.Resolve(ctx, "missing", "ed25519")
		if err != nil || key != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	status = 503
	now = now.Add(2 * time.Second)
	if key, err = resolver.Resolve(ctx, "first", "ed25519"); err != nil || key == nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(ctx, "missing", "ed25519"); err == nil {
		t.Fatal("unknown fallback")
	} else {
		code(t, err, "KEY_RETRIEVAL_FAILED")
	}
	now = now.Add(2 * time.Second)
	if _, err = resolver.Resolve(ctx, "first", "ed25519"); err == nil {
		t.Fatal("old fallback")
	} else {
		code(t, err, "KEY_RETRIEVAL_FAILED")
	}
	status = 200
	payload = keySet("second")
	if key, err = resolver.Resolve(ctx, "second", "ed25519"); err != nil || key == nil {
		t.Fatal(err)
	}
	if key, err = resolver.Resolve(ctx, "first", "ed25519"); err != nil || key != nil {
		t.Fatal("removed key", err)
	}
	if key, err = resolver.Resolve(ctx, "second", "rsa"); err != nil || key != nil {
		t.Fatal("algorithm", err)
	}
}

func TestResolverInvalidKeySets(t *testing.T) {
	cases := []string{
		"null", "[]", "{", `{"keys":null}`, `{"keys":{}}`, `{"keys":[1]}`,
		`{"keys":[` + jwk("test") + "," + jwk("test") + `]}`,
		strings.Replace(keySet("test"), base64.RawURLEncoding.EncodeToString(publicKey), "invalid", 1),
		strings.Replace(keySet("test"), base64.RawURLEncoding.EncodeToString(publicKey), "%", 1),
	}
	for _, payload := range cases {
		t.Run(payload, func(t *testing.T) {
			resolver := NewVisaKeyResolver(KeyResolverOptions{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})})
			_, err := resolver.Resolve(context.Background(), "test", "ed25519")
			code(t, err, "KEY_RETRIEVAL_FAILED")
		})
	}
	for _, payload := range []string{"{}", `{"keys":[]}`, `{"keys":[null,{"kty":"RSA"}]}`,
		strings.Replace(keySet("test"), `"kid":"test"`, `"kid":3`, 1),
		strings.Replace(keySet("test"), `"kty":"OKP"`, `"use":"enc","kty":"OKP"`, 1),
	} {
		resolver := NewVisaKeyResolver(KeyResolverOptions{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
		})})
		key, err := resolver.Resolve(context.Background(), "test", "ed25519")
		if key != nil || err != nil {
			t.Fatalf("%s: %v", payload, err)
		}
	}
}

func TestResolverAtomicReplacementAndFreshFallbackAge(t *testing.T) {
	now := fixedClock()
	payload := keySet("test")
	resolver := NewVisaKeyResolver(KeyResolverOptions{Clock: func() time.Time { return now }, CacheTTL: 10 * time.Second, CacheMaxAge: time.Second,
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
		}),
	})
	if _, err := resolver.Resolve(context.Background(), "test", "ed25519"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	payload = `{"keys":[` + jwk("new") + "," + jwk("new") + `]}`
	if _, err := resolver.Resolve(context.Background(), "new", "ed25519"); err == nil {
		t.Fatal("duplicate set")
	}
	if key, err := resolver.Resolve(context.Background(), "test", "ed25519"); err != nil || key == nil {
		t.Fatal("fresh existing key lost", err)
	}
}

func TestSharedRefreshAndWaitCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	resolver := NewVisaKeyResolver(KeyResolverOptions{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(keySet("test")))}, nil
	})})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := resolver.Resolve(context.Background(), "test", "ed25519"); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	observed := &observedContext{Context: ctx, entered: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := resolver.Resolve(observed, "test", "ed25519"); waiter <- err }()
	<-observed.entered
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if key, err := resolver.Resolve(context.Background(), "test", "ed25519"); err != nil || key == nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestRefreshOwnerCancellationAndRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	resolver := NewVisaKeyResolver(KeyResolverOptions{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			cancel()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(keySet("test")))}, nil
	})})
	if _, err := resolver.Resolve(ctx, "test", "ed25519"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if key, err := resolver.Resolve(context.Background(), "test", "ed25519"); err != nil || key == nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, "test", "ed25519"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }

func TestRetrievalBoundaries(t *testing.T) {
	for _, body := range []io.ReadCloser{brokenBody{}, io.NopCloser(strings.NewReader(strings.Repeat(" ", (8<<20)+1)))} {
		resolver := NewVisaKeyResolver(KeyResolverOptions{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: body}, nil
		})})
		_, err := resolver.Resolve(context.Background(), "test", "ed25519")
		code(t, err, "KEY_RETRIEVAL_FAILED")
	}
	resolver := NewVisaKeyResolver(KeyResolverOptions{URL: "%"})
	_, err := resolver.Resolve(context.Background(), "test", "ed25519")
	code(t, err, "KEY_RETRIEVAL_FAILED")
	resolver = NewVisaKeyResolver(KeyResolverOptions{Timeout: time.Millisecond, Transport: transportFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })})
	_, err = resolver.Resolve(context.Background(), "test", "ed25519")
	code(t, err, "KEY_RETRIEVAL_FAILED")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
