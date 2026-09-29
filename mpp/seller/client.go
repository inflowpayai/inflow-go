// Package seller accepts MPP payments through InFlow's Seller endpoints.
package seller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
)

type Client struct {
	api     *platform.Client
	mu      sync.Mutex
	loaded  *configuration
	loading *configLoad
}

type configLoad struct {
	done  chan struct{}
	value *configuration
	err   error
}

type rail struct {
	Rail         string `json:"rail"`
	InstrumentID string `json:"instrumentId"`
}

type configuration struct {
	SellerID     string `json:"sellerId"`
	FeatureFlags struct {
		IdempotencyKeyEnabled bool `json:"idempotencyKeyEnabled"`
	} `json:"featureFlags"`
	SupportedMethods []struct {
		ID            string `json:"id"`
		MethodDetails struct {
			CurrencyRails       map[string]rail              `json:"currencyRails"`
			IntentCurrencyRails map[string]map[string][]rail `json:"intentCurrencyRails"`
		} `json:"methodDetails"`
	} `json:"supportedMethods"`
}

func New(options inflow.Options) (*Client, error) {
	api, err := platform.New(options)
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// Load checks Seller connectivity and caches configuration. Construction does no network work.
func (c *Client) Load(ctx context.Context) error {
	_, err := c.config(ctx)
	return err
}

func (c *Client) config(ctx context.Context) (*configuration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.loaded != nil {
		value := c.loaded
		c.mu.Unlock()
		return value, nil
	}
	if load := c.loading; load != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-load.done:
			return load.value, load.err
		}
	}
	load := &configLoad{done: make(chan struct{})}
	c.loading = load
	c.mu.Unlock()
	raw, err := c.api.Do(ctx, platform.Request{Method: "GET", Path: "/v1/mpp/config", Retries: 3})
	if err == nil {
		var value configuration
		value, err = platform.Decode[configuration](raw)
		if err == nil && value.SellerID == "" {
			err = errors.New("MPP configuration has no sellerId")
		}
		if err == nil {
			load.value = &value
		}
	}
	load.err = err
	c.mu.Lock()
	if err == nil {
		c.loaded = load.value
	}
	c.loading = nil
	close(load.done)
	c.mu.Unlock()
	return load.value, err
}

type Error struct {
	Code    string
	Problem json.RawMessage
}

func (e *Error) Error() string { return "MPP " + e.Code }

func rejection(problem json.RawMessage) error {
	return &Error{Code: "payment-failed", Problem: problem}
}
