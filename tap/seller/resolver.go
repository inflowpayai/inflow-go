package seller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type KeyResolverOptions struct {
	URL string
	// Transport must honor request cancellation, including response-body reads.
	Transport http.RoundTripper
	// Zero selects the default: one hour fresh, 24 hours outage fallback,
	// and three seconds per retrieval including reading the response.
	CacheTTL    time.Duration
	CacheMaxAge time.Duration
	Timeout     time.Duration
	Clock       func() time.Time
}

type keyRefresh struct {
	done chan struct{}
	err  error
}

type VisaKeyResolver struct {
	options KeyResolverOptions
	http    *http.Client
	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	missing map[string]bool
	updated time.Time
	loaded  bool
	refresh *keyRefresh
}

func NewVisaKeyResolver(options KeyResolverOptions) *VisaKeyResolver {
	if options.URL == "" {
		options.URL = "https://mcp.visa.com/.well-known/jwks"
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.CacheTTL == 0 {
		options.CacheTTL = time.Hour
	}
	if options.CacheMaxAge == 0 {
		options.CacheMaxAge = 24 * time.Hour
	}
	if options.Timeout == 0 {
		options.Timeout = 3 * time.Second
	}
	return &VisaKeyResolver{options: options, http: &http.Client{Transport: options.Transport}, missing: make(map[string]bool)}
}

func (r *VisaKeyResolver) Resolve(ctx context.Context, keyID, algorithm string) (ed25519.PublicKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if algorithm != "ed25519" {
		return nil, nil
	}
	r.mu.Lock()
	if r.loaded && r.options.Clock().Sub(r.updated) <= r.options.CacheTTL {
		if key := r.keys[keyID]; key != nil || r.missing[keyID] {
			result := cloneKey(key)
			r.mu.Unlock()
			return result, nil
		}
	}
	load := r.refresh
	if load != nil {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-load.done:
		}
		r.mu.Lock()
	} else {
		// The initiating caller owns the refresh, as in the Go payment clients.
		// Waiting callers can leave without cancelling that caller's request.
		load = &keyRefresh{done: make(chan struct{})}
		r.refresh = load
		r.mu.Unlock()
		keys, err := r.fetch(ctx)
		r.mu.Lock()
		load.err = err
		if err == nil {
			r.keys, r.missing = keys, make(map[string]bool)
			r.updated, r.loaded = r.options.Clock(), true
		}
		r.refresh = nil
		close(load.done)
	}
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if load.err != nil {
		if r.loaded && r.options.Clock().Sub(r.updated) <= r.options.CacheMaxAge {
			if key := r.keys[keyID]; key != nil {
				return cloneKey(key), nil
			}
		}
		return nil, &Error{Code: "KEY_RETRIEVAL_FAILED", Cause: load.err}
	}
	key := r.keys[keyID]
	if key == nil {
		r.missing[keyID] = true
	}
	return cloneKey(key), nil
}

func cloneKey(key ed25519.PublicKey) ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), key...)
}

func (r *VisaKeyResolver) fetch(parent context.Context) (map[string]ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(parent, r.options.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.options.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("key service returned HTTP %d", response.StatusCode)
	}
	// A key set is metadata, not an unbounded download.
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 8<<20 {
		return nil, errors.New("key set exceeds 8 MiB")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("invalid key set")
	}
	var entries []map[string]json.RawMessage
	if encoded, exists := object["keys"]; exists {
		if len(encoded) == 0 || encoded[0] != '[' {
			return nil, errors.New("invalid keys array")
		}
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return nil, err
		}
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, entry := range entries {
		stringField := func(name string) string {
			var value string
			_ = json.Unmarshal(entry[name], &value)
			return value
		}
		algorithm := stringField("alg")
		if stringField("kty") != "OKP" || stringField("crv") != "Ed25519" || (algorithm != "ed25519" && algorithm != "Ed25519") {
			continue
		}
		if _, exists := entry["use"]; exists && stringField("use") != "sig" {
			continue
		}
		var keyID string
		if raw := entry["kid"]; len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &keyID) != nil {
			continue
		}
		if _, exists := keys[keyID]; exists {
			return nil, errors.New("duplicate key identifier")
		}
		key, err := base64.RawURLEncoding.DecodeString(stringField("x"))
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("invalid Ed25519 key")
		}
		keys[keyID] = ed25519.PublicKey(key)
	}
	return keys, nil
}
