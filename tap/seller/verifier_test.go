package seller

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) (ed25519.PublicKey, error)

func (f resolverFunc) Resolve(ctx context.Context, id, algorithm string) (ed25519.PublicKey, error) {
	return f(ctx, id, algorithm)
}

type storeFunc func(context.Context, string, string, int64) (bool, error)

func (f storeFunc) Claim(ctx context.Context, id, nonce string, expires int64) (bool, error) {
	return f(ctx, id, nonce, expires)
}

var privateKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
var publicKey = privateKey.Public().(ed25519.PublicKey)

func fixedClock() time.Time                                                   { return time.Unix(1000, 0) }
func testResolver(context.Context, string, string) (ed25519.PublicKey, error) { return publicKey, nil }

func signedRequest(body []byte) Request {
	r := Request{Method: "POST", URL: "https://example.com/path%20value?a=1&b=%2f", Headers: make(http.Header), Body: body}
	components := `("@method" "@authority" "@path" "@query"`
	lines := []string{`"@method": POST`, `"@authority": example.com`, `"@path": /path%20value`, `"@query": ?a=1&b=%2f`}
	if body != nil {
		hash := sha256.Sum256(body)
		r.Headers.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash[:])+":")
		r.Headers.Set("Content-Type", "application/json")
		components += ` "content-digest" "content-type"`
		lines = append(lines, `"content-digest": `+r.Headers.Get("Content-Digest"), `"content-type": application/json`)
	}
	parameters := components + `);created=999;expires=1100;keyid="test";alg="ed25519";nonce="nonce";tag="agent-browser-auth"`
	r.Headers.Set("Signature-Input", "sig2="+parameters)
	lines = append(lines, `"@signature-params": `+parameters)
	r.Headers.Set("Signature", "sig2=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(strings.Join(lines, "\n"))))+":")
	return r
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	var problem *Error
	if !errors.As(err, &problem) || problem.Code != want {
		t.Fatalf("want %s, got %v", want, err)
	}
	if problem.Error() != "TAP "+want {
		t.Fatal(problem.Error())
	}
}

func TestVerificationAndReplay(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte(`{"hello":"world"}`)} {
		t.Run(fmt.Sprintf("body-%v-%d", body == nil, len(body)), func(t *testing.T) {
			request := signedRequest(body)
			headers, bytes := request.Headers.Clone(), append([]byte(nil), body...)
			v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(testResolver)})
			called := false
			err := v.WithVerified(context.Background(), request, func(f Facts) error {
				called = true
				if !f.Verified || f.KeyID != "test" || f.Intent != "browse" || f.Algorithm != "ed25519" || f.Created != 999 || f.Expires != 1100 || f.Nonce != "nonce" {
					t.Fatalf("%+v", f)
				}
				return nil
			})
			if err != nil || !called {
				t.Fatalf("%v %v", called, err)
			}
			if !reflect.DeepEqual(headers, request.Headers) || string(bytes) != string(request.Body) {
				t.Fatal("mutated")
			}
			err = v.WithVerified(context.Background(), request, func(Facts) error { t.Fatal("replayed handler"); return nil })
			code(t, err, "NONCE_REPLAYED")
		})
	}
}

func TestRejectsBeforeKeyOrReplay(t *testing.T) {
	cases := []struct {
		name, want string
		modify     func(*Request)
	}{
		{"missing input", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.Headers.Del("Signature-Input") }},
		{"missing signature", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.Headers.Del("Signature") }},
		{"extra component", "SIGNATURE_INPUT_INVALID", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), `"@method"`, `"@method" "extra"`, 1))
		}},
		{"wrong component", "SIGNATURE_INPUT_INVALID", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), `"@method"`, `"extra"`, 1))
		}},
		{"negative lifetime", "SIGNATURE_LIFETIME_INVALID", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), "expires=1100", "expires=999", 1))
		}},
		{"long lifetime", "SIGNATURE_LIFETIME_INVALID", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), "expires=1100", "expires=1500", 1))
		}},
		{"future", "SIGNATURE_NOT_YET_VALID", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), "created=999", "created=1001", 1))
		}},
		{"expired", "SIGNATURE_EXPIRED", func(r *Request) {
			r.Headers.Set("Signature-Input", strings.Replace(r.Headers.Get("Signature-Input"), "expires=1100", "expires=1000", 1))
		}},
		{"URL invalid", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.URL = "%" }},
		{"URL relative", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.URL = "/path" }},
		{"URL credentials", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.URL = "https://a:b@example.com/" }},
		{"missing type", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.Headers.Del("Content-Type") }},
		{"digest", "CONTENT_DIGEST_INVALID", func(r *Request) { r.Body = []byte("changed") }},
		{"duplicate header", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.Headers["signature-input"] = r.Headers["Signature-Input"] }},
		{"array header", "SIGNATURE_INPUT_INVALID", func(r *Request) { r.Headers["Signature-Input"] = append(r.Headers["Signature-Input"], "another") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := signedRequest([]byte("{}"))
			tc.modify(&r)
			v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(func(context.Context, string, string) (ed25519.PublicKey, error) {
				t.Fatal("key lookup")
				return nil, nil
			})})
			_, err := v.Verify(context.Background(), r)
			code(t, err, tc.want)
		})
	}
}

func TestCryptographyAndCustomFailures(t *testing.T) {
	sentinel := errors.New("custom")
	cases := []struct {
		name, want string
		resolver   resolverFunc
		store      storeFunc
		edit       func(*Request)
	}{
		{"missing", "KEY_NOT_FOUND", func(context.Context, string, string) (ed25519.PublicKey, error) { return nil, nil }, nil, nil},
		{"short key", "SIGNATURE_INVALID", func(context.Context, string, string) (ed25519.PublicKey, error) { return []byte{1}, nil }, nil, nil},
		{"signature missing", "SIGNATURE_INPUT_INVALID", nil, nil, func(r *Request) { r.Headers.Del("Signature") }},
		{"signature malformed", "SIGNATURE_INPUT_INVALID", nil, nil, func(r *Request) { r.Headers.Set("Signature", "sig2=:bad-padding=:") }},
		{"tampered method", "SIGNATURE_INVALID", nil, nil, func(r *Request) { r.Method = "GET" }},
		{"tampered path", "SIGNATURE_INVALID", nil, nil, func(r *Request) { r.URL = "https://example.com/" }},
		{"resolver error", "", func(context.Context, string, string) (ed25519.PublicKey, error) { return nil, sentinel }, nil, nil},
		{"store error", "", nil, func(context.Context, string, string, int64) (bool, error) { return false, sentinel }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolve := tc.resolver
			if resolve == nil {
				resolve = resolverFunc(testResolver)
			}
			calls := 0
			store := storeFunc(func(ctx context.Context, id, nonce string, expires int64) (bool, error) {
				calls++
				if tc.store != nil {
					return tc.store(ctx, id, nonce, expires)
				}
				return true, nil
			})
			v := New(Options{Clock: fixedClock, KeyResolver: resolve, ReplayStore: store})
			r := signedRequest(nil)
			if tc.edit != nil {
				tc.edit(&r)
			}
			err := v.WithVerified(context.Background(), r, func(Facts) error { t.Fatal("handler"); return nil })
			if tc.want != "" {
				code(t, err, tc.want)
			} else if err != sentinel {
				t.Fatal(err)
			}
			if tc.store == nil && calls != 0 {
				t.Fatal("invalid signature consumed nonce")
			}
		})
	}
	v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(testResolver)})
	if err := v.WithVerified(context.Background(), signedRequest(nil), func(Facts) error { return sentinel }); err != sentinel {
		t.Fatal(err)
	}
	if !errors.Is(&Error{Code: "KEY_RETRIEVAL_FAILED", Cause: sentinel}, sentinel) {
		t.Fatal("lost cause")
	}
}

func TestCancellationAndCompletionTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := New(Options{})
	if _, err := v.Verify(ctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, phase := range []string{"resolver", "store"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(func(context.Context, string, string) (ed25519.PublicKey, error) {
				if phase == "resolver" {
					cancel()
				}
				return publicKey, nil
			}), ReplayStore: storeFunc(func(context.Context, string, string, int64) (bool, error) { cancel(); return true, nil })})
			if _, err := v.Verify(ctx, signedRequest(nil)); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	now := fixedClock()
	v = New(Options{Clock: func() time.Time { return now }, KeyResolver: resolverFunc(func(context.Context, string, string) (ed25519.PublicKey, error) {
		now = time.Unix(1200, 0)
		return publicKey, nil
	})})
	if _, err := v.Verify(context.Background(), signedRequest(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReplay(t *testing.T) {
	v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(testResolver)})
	var passed atomic.Int64
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Verify(context.Background(), signedRequest(nil))
			if err == nil {
				passed.Add(1)
			} else {
				code(t, err, "NONCE_REPLAYED")
			}
		}()
	}
	wg.Wait()
	if passed.Load() != 1 {
		t.Fatal(passed.Load())
	}
	s := NewMemoryReplayStore(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Claim(ctx, "a", "b", 1000); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	now := fixedClock()
	s = NewMemoryReplayStore(func() time.Time { return now })
	if ok, _ := s.Claim(context.Background(), "a", "b", 1001); !ok {
		t.Fatal("claim")
	}
	now = time.Unix(1001, 0)
	if ok, _ := s.Claim(context.Background(), "a", "b", 1100); !ok {
		t.Fatal("expiry")
	}
}
