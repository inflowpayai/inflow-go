// Package buyer obtains x402 payments through InFlow or an upstream external-wallet client.
package buyer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
)

// ExternalClient is implemented by the upstream x402 client. Its spending controls remain enabled.
type ExternalClient interface {
	SelectPaymentRequirements([]x402.PaymentRequirements) (x402.PaymentRequirements, error)
	CreatePaymentPayload(context.Context, x402.PaymentRequirements, *x402.ResourceInfo, map[string]any) (x402.PaymentPayload, error)
}

// Hooks belong to this wrapper so errors propagate for both InFlow-managed and
// external-wallet payments. Upstream x402 Go ignores after-hook errors; forwarding
// these callbacks to that client would change the application's error handling.
type Hooks struct {
	Before func(context.Context, x402.PaymentRequired) error
	After  func(context.Context, x402.PaymentRequired, x402.PaymentPayload) error
	// Failure may recover a one-shot operation. Prepared payments never replace their transaction.
	Failure func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error)
}

type Policy func(context.Context, []x402.PaymentRequirements) ([]x402.PaymentRequirements, error)

type Options struct {
	inflow.Options
	PollInterval time.Duration
	// WaitTimeout is a separate budget for each Wait attempt. Zero selects fifteen minutes.
	WaitTimeout time.Duration
	Prefer      []string
	External    ExternalClient
	// Configure hooks and policies before construction. Callbacks must support concurrent operations.
	Hooks    []Hooks
	Policies []Policy
}

type SignOptions struct {
	PaymentID                    string
	TransactionRequestExtensions map[string]any
}

type EncodedPayment struct {
	EncodedPayload string              `json:"encodedPayload"`
	PaymentPayload x402.PaymentPayload `json:"paymentPayload"`
	TransactionID  string              `json:"transactionId,omitempty"`
}

type Error struct {
	Code       string
	Status     string
	ApprovalID string
	Cause      error
}

func (e *Error) Error() string { return "x402 " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }

type Client struct {
	api       *platform.Client
	resource  *http.Client
	options   Options
	mu        sync.Mutex
	supported x402.BuyerSupportedResponse
	expires   time.Time
	loading   *capabilityLoad
}
type capabilityLoad struct {
	done  chan struct{}
	value x402.BuyerSupportedResponse
	err   error
}

func New(options Options) (*Client, error) {
	if options.PollInterval < 0 || options.WaitTimeout < 0 {
		return nil, errors.New("x402 polling interval and wait timeout must not be negative")
	}
	api, err := platform.New(options.Options)
	if err != nil {
		return nil, err
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.WaitTimeout == 0 {
		options.WaitTimeout = 15 * time.Minute
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Prefer == nil {
		options.Prefer = []string{x402.SchemeBalance, x402.SchemeExact}
	}
	options.Prefer = slices.Clone(options.Prefer)
	options.Hooks = slices.Clone(options.Hooks)
	options.Policies = slices.Clone(options.Policies)
	for _, policy := range options.Policies {
		if policy == nil {
			return nil, errors.New("x402 policy must not be nil")
		}
	}
	return &Client{api: api, options: options, resource: &http.Client{Transport: options.Transport, Timeout: options.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func decode[T any](data []byte) (T, error) {
	var result T
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&result); err != nil {
		return result, &Error{Code: "invalid-response", Cause: err}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return result, &Error{Code: "invalid-response"}
	}
	return result, nil
}
func snapshot[T any](value T) (T, error) {
	b, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, &Error{Code: "invalid-input"}
	}
	return decode[T](b)
}

func (c *Client) Supported(ctx context.Context) (x402.BuyerSupportedResponse, error) {
	if err := ctx.Err(); err != nil {
		return x402.BuyerSupportedResponse{}, err
	}
	c.mu.Lock()
	if time.Now().Before(c.expires) {
		value := c.supported
		c.mu.Unlock()
		return snapshot(value)
	}
	if pending := c.loading; pending != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return x402.BuyerSupportedResponse{}, ctx.Err()
		case <-pending.done:
		}
		if pending.err != nil {
			return x402.BuyerSupportedResponse{}, pending.err
		}
		return snapshot(pending.value)
	}
	pending := &capabilityLoad{done: make(chan struct{})}
	c.loading = pending
	c.mu.Unlock()
	raw, err := c.api.Do(ctx, platform.Request{Method: http.MethodGet, Path: "/v1/transactions/x402-supported", Retries: 3})
	if err == nil {
		pending.value, err = decode[x402.BuyerSupportedResponse](raw.Body)
	}
	c.mu.Lock()
	pending.err = err
	if err == nil {
		c.supported = pending.value
		c.expires = time.Now().Add(time.Hour)
	}
	c.loading = nil
	close(pending.done)
	c.mu.Unlock()
	if err != nil {
		return x402.BuyerSupportedResponse{}, err
	}
	return snapshot(pending.value)
}

func supports(supported x402.BuyerSupportedResponse, requirement x402.PaymentRequirements) bool {
	if requirement.Extra["assetTransferMethod"] == "permit2" {
		return false
	}
	for _, kind := range supported.Kinds {
		if kind.Scheme == requirement.Scheme && kind.Network == requirement.Network {
			return true
		}
	}
	return false
}

func (c *Client) before(ctx context.Context, required x402.PaymentRequired) error {
	for _, hook := range c.options.Hooks {
		if hook.Before != nil {
			value, err := snapshot(required)
			if err != nil {
				return err
			}
			if err = hook.Before(ctx, value); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
func (c *Client) after(ctx context.Context, required x402.PaymentRequired, payload x402.PaymentPayload) error {
	for _, hook := range c.options.Hooks {
		if hook.After != nil {
			r, err := snapshot(required)
			if err != nil {
				return err
			}
			p, err := snapshot(payload)
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = hook.After(ctx, r, p); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
