package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	tap "github.com/inflowpayai/inflow-go/tap/seller"
)

func main() {
	if err := run(os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(getenv func(string) string) error {
	origin := getenv("PUBLIC_ORIGIN")
	if origin == "" {
		return errors.New("set PUBLIC_ORIGIN to the externally visible HTTP or HTTPS origin")
	}
	handler, err := newHandler(origin, tap.New(tap.Options{}))
	if err != nil {
		return err
	}
	address := getenv("LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:3001"
	}
	log.Printf("TAP seller listening on http://%s", address)
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

func newHandler(origin string, verifier *tap.Verifier) (http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("PUBLIC_ORIGIN must be an HTTP or HTTPS origin without a path, credentials, query, or fragment")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		// Signed empty bodies still have a digest. Preserve bytes instead of parsing JSON.
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 || r.Header.Get("Content-Digest") != "" {
			var err error
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "Request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "Could not read request body", http.StatusBadRequest)
				}
				return
			}
		}
		// Use the configured public origin, never an untrusted Forwarded header.
		// EscapedPath and RawQuery retain the request bytes covered by the signature.
		path := r.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
		target := origin + path
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			target += "?" + r.URL.RawQuery
		}
		err := verifier.WithVerified(r.Context(), tap.Request{Method: r.Method, URL: target, Headers: r.Header, Body: body}, func(facts tap.Facts) error {
			// Recognition is not buyer authorization or payment. This example serves a
			// free response; place application and payment checks here for a paid route.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Verified agent request (%s)\n", facts.Intent)
			return nil
		})
		if err != nil {
			http.Error(w, "TAP verification failed", http.StatusUnauthorized)
		}
	}), nil
}
