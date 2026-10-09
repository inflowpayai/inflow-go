// Package seller configures upstream x402 middleware to accept payments through InFlow.
package seller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
)

type Client struct {
	api           *platform.Client
	configuration cache
	supported     cache
}

// New requires a Seller API key. Construction performs no network requests.
func New(options inflow.Options) (*Client, error) {
	if options.APIKey == "" && options.APIKeyProvider == nil {
		return nil, errors.New("x402 Seller requires an API key")
	}
	api, err := platform.New(options)
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

func (c *Client) Config(ctx context.Context) (x402.ConfigResponse, error) {
	return cached[x402.ConfigResponse](ctx, &c.configuration, c.api, "/v1/x402/config", false)
}

func (c *Client) RefreshConfig(ctx context.Context) (x402.ConfigResponse, error) {
	return cached[x402.ConfigResponse](ctx, &c.configuration, c.api, "/v1/x402/config", true)
}

func (c *Client) RefreshSupported(ctx context.Context) (x402.SupportedResponse, error) {
	return cached[x402.SupportedResponse](ctx, &c.supported, c.api, "/v1/x402/supported", true)
}

func (c *Client) SignerAddresses(ctx context.Context, network string) ([]string, error) {
	supported, err := cached[x402.SupportedResponse](ctx, &c.supported, c.api, "/v1/x402/supported", false)
	if err != nil {
		return nil, err
	}
	if addresses, ok := supported.Signers[network]; ok {
		return addresses, nil
	}
	if namespace, _, ok := strings.Cut(network, ":"); ok {
		if addresses, ok := supported.Signers[namespace+":*"]; ok {
			return addresses, nil
		}
	}
	return []string{}, nil
}

type cache struct {
	mu      sync.Mutex
	data    []byte
	expires time.Time
	loading *cacheLoad
}

type cacheLoad struct {
	done chan struct{}
	data []byte
	err  error
}

// Cache serialized responses so callers cannot mutate shared configuration maps.
func cached[T any](ctx context.Context, entry *cache, api *platform.Client, path string, refresh bool) (T, error) {
	var value T
	if err := ctx.Err(); err != nil {
		return value, err
	}
	entry.mu.Lock()
	if !refresh && entry.data != nil && time.Now().Before(entry.expires) {
		data := entry.data
		entry.mu.Unlock()
		return platform.Decode[T](&platform.Response{Body: data})
	}
	if pending := entry.loading; pending != nil {
		entry.mu.Unlock()
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-pending.done:
			if pending.err != nil {
				return value, pending.err
			}
			return platform.Decode[T](&platform.Response{Body: pending.data})
		}
	}
	pending := &cacheLoad{done: make(chan struct{})}
	entry.loading = pending
	entry.mu.Unlock()
	response, err := api.Do(ctx, platform.Request{Method: "GET", Path: path, Retries: 3})
	if err == nil {
		value, err = platform.Decode[T](response)
		if err == nil {
			pending.data = response.Body
		}
	}
	pending.err = err
	entry.mu.Lock()
	if err == nil {
		entry.data = pending.data
		entry.expires = time.Now().Add(time.Hour)
	}
	entry.loading = nil
	close(pending.done)
	entry.mu.Unlock()
	return value, err
}
