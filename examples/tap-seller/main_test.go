package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tap "github.com/inflowpayai/inflow-go/tap/seller"
)

type resolver ed25519.PublicKey

func (r resolver) Resolve(context.Context, string, string) (ed25519.PublicKey, error) {
	return ed25519.PublicKey(r), nil
}

func TestRunConfiguration(t *testing.T) {
	if err := run(func(string) string { return "" }); err == nil {
		t.Fatal("missing origin")
	}
	for _, origin := range []string{"%", "/relative", "https://example.com/path", "https://a:b@example.com", "https://example.com?", "https://example.com#fragment"} {
		if err := run(func(name string) string {
			if name == "PUBLIC_ORIGIN" {
				return origin
			}
			return ""
		}); err == nil {
			t.Fatal(origin)
		}
	}
	for _, address := range []string{"", "127.0.0.1:bad"} {
		// An occupied address exercises ListenAndServe without leaving a daemon.
		var listener net.Listener
		if address == "" {
			var err error
			listener, err = net.Listen("tcp", "127.0.0.1:3001")
			if err != nil {
				t.Fatal(err)
			}
		}
		err := run(func(name string) string {
			if name == "PUBLIC_ORIGIN" {
				return "https://example.com"
			}
			if name == "LISTEN_ADDR" {
				return address
			}
			return ""
		})
		if listener != nil {
			listener.Close()
		}
		if err == nil {
			t.Fatal("expected listen error")
		}
	}
}

func TestHTTPVerification(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	for _, body := range []string{"", "{}"} {
		v := tap.New(tap.Options{Clock: func() time.Time { return time.Unix(1000, 0) }, KeyResolver: resolver(key.Public().(ed25519.PublicKey))})
		handler, err := newHandler("https://example.com", v)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		defer server.Close()
		req, err := http.NewRequest("POST", server.URL+"/path%20value?q=%2F", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		hash := sha256.Sum256([]byte(body))
		digest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash[:]) + ":"
		req.Header.Set("Content-Digest", digest)
		params := `("@method" "@authority" "@path" "@query" "content-digest" "content-type");created=999;expires=1100;keyid="test";alg="ed25519";nonce="test";tag="agent-browser-auth"`
		base := `"@method": POST` + "\n" + `"@authority": example.com` + "\n" + `"@path": /path%20value` + "\n" + `"@query": ?q=%2F` + "\n" + `"content-digest": ` + digest + "\n" + `"content-type": application/json` + "\n" + `"@signature-params": ` + params
		req.Header.Set("Signature-Input", "sig2="+params)
		req.Header.Set("Signature", "sig2=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(base)))+":")
		req.Header.Set("Forwarded", "host=attacker.invalid;proto=http")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || string(output) != "Verified agent request (browse)\n" {
			t.Fatalf("%d %s %v", response.StatusCode, output, err)
		}
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("broken") }
func (brokenReader) Close() error             { return nil }

func TestHTTPRejections(t *testing.T) {
	handler, err := newHandler("https://example.com", tap.New(tap.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   io.ReadCloser
		size   int64
		status int
	}{
		{http.NoBody, 0, 401}, {brokenReader{}, -1, 400}, {io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1))), -1, 413},
	} {
		req := httptest.NewRequest("POST", "/", nil)
		req.Body = tc.body
		req.ContentLength = tc.size
		req.URL.Path = ""
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
}
