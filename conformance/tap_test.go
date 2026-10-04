package conformance

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	tap "github.com/inflowpayai/inflow-go/tap/seller"
)

type tapResolver func(context.Context, string, string) (ed25519.PublicKey, error)

func (f tapResolver) Resolve(ctx context.Context, id, algorithm string) (ed25519.PublicKey, error) {
	return f(ctx, id, algorithm)
}

type tapStore func(context.Context, string, string, int64) (bool, error)

func (f tapStore) Claim(ctx context.Context, id, nonce string, expires int64) (bool, error) {
	return f(ctx, id, nonce, expires)
}

func executeTAP(ctx context.Context, operation string, raw json.RawMessage) (result any, outcome error) {
	if operation != "tap.seller.verify" {
		return nil, errors.New("unsupported TAP operation")
	}
	var input struct {
		Key struct {
			Kid string
			X   string
		}
		Resolver           string
		BaseURL            string `json:"base_url"`
		CacheTTL           int64  `json:"cache_ttl_ms"`
		CacheMaxAge        int64  `json:"cache_max_age_ms"`
		ResolverCompletion *int64 `json:"resolver_completion_ms"`
		ResolverFailure    bool   `json:"resolver_failure"`
		StoreFailure       bool   `json:"store_failure"`
		Steps              []struct {
			Now      int64 `json:"now_ms"`
			Requests []struct {
				Method  string
				URL     string
				Headers map[string]json.RawMessage
				Body    *string `json:"body_base64"`
			}
		}
	}
	if err := decode(raw, &input); err != nil {
		return nil, err
	}
	defer watchInput(input, &outcome)()
	key, err := base64.RawURLEncoding.DecodeString(input.Key.X)
	if err != nil {
		return nil, err
	}
	var now atomic.Int64
	clock := func() time.Time { return time.UnixMilli(now.Load()) }
	resolverFailure, storeFailure := errors.New("synthetic resolver failure"), errors.New("synthetic store failure")
	var resolver tap.KeyResolver = tapResolver(func(_ context.Context, id, algorithm string) (ed25519.PublicKey, error) {
		if input.ResolverFailure {
			return nil, resolverFailure
		}
		if input.ResolverCompletion != nil {
			now.Store(*input.ResolverCompletion)
		}
		if id == input.Key.Kid && algorithm == "ed25519" {
			return ed25519.PublicKey(key), nil
		}
		return nil, nil
	})
	if input.Resolver == "http" {
		if _, err := platformOptions(raw); err != nil {
			return nil, err
		}
		resolver = tap.NewVisaKeyResolver(tap.KeyResolverOptions{URL: input.BaseURL + "/keys", Clock: clock, CacheTTL: time.Duration(input.CacheTTL) * time.Millisecond, CacheMaxAge: time.Duration(input.CacheMaxAge) * time.Millisecond})
	}
	memory := tap.NewMemoryReplayStore(clock)
	var claims, handlers atomic.Int64
	verifier := tap.New(tap.Options{Clock: clock, KeyResolver: resolver, ReplayStore: tapStore(func(ctx context.Context, id, nonce string, expires int64) (bool, error) {
		claims.Add(1)
		if input.StoreFailure {
			return false, storeFailure
		}
		return memory.Claim(ctx, id, nonce, expires)
	})})
	steps := []any{}
	for _, step := range input.Steps {
		now.Store(step.Now)
		accepted := []tap.Facts{}
		rejected := []string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		var stepError error
		requests := make([]tap.Request, 0, len(step.Requests))
		for _, item := range step.Requests {
			request := tap.Request{Method: item.Method, URL: item.URL, Headers: make(http.Header)}
			for name, value := range item.Headers {
				var one string
				if json.Unmarshal(value, &one) == nil {
					request.Headers[name] = []string{one}
				} else {
					var many []string
					if err := json.Unmarshal(value, &many); err != nil {
						return nil, err
					}
					request.Headers[name] = many
				}
			}
			if item.Body != nil {
				body, err := base64.StdEncoding.DecodeString(*item.Body)
				if err != nil {
					return nil, err
				}
				request.Body = append([]byte{}, body...)
			}
			requests = append(requests, request)
		}
		for _, request := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				check := watchInput(request, &err)
				err = verifier.WithVerified(ctx, request, func(facts tap.Facts) error {
					handlers.Add(1)
					mu.Lock()
					accepted = append(accepted, facts)
					mu.Unlock()
					return nil
				})
				check()
				if err == nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				var verification *tap.Error
				switch {
				case errors.As(err, &verification):
					rejected = append(rejected, verification.Code)
				case err == resolverFailure:
					rejected = append(rejected, "CUSTOM_RESOLVER_FAILED")
				case err == storeFailure:
					rejected = append(rejected, "CUSTOM_STORE_FAILED")
				default:
					stepError = err
				}
			}()
		}
		wg.Wait()
		if stepError != nil {
			return nil, stepError
		}
		sort.Strings(rejected)
		steps = append(steps, map[string]any{"accepted": accepted, "rejected": rejected})
	}
	return map[string]any{"steps": steps, "handler_calls": handlers.Load(), "claim_calls": claims.Load()}, nil
}
