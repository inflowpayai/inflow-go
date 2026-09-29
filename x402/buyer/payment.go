package buyer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
)

type transaction struct {
	TransactionID  string `json:"transactionId"`
	ApprovalID     string `json:"approvalId"`
	ApprovalStatus string `json:"approvalStatus"`
}
type payloadResponse struct {
	Status         string               `json:"status"`
	EncodedPayload *string              `json:"encodedPayload"`
	PaymentPayload *x402.PaymentPayload `json:"paymentPayload"`
}
type attempt struct {
	done     chan struct{}
	cancel   context.CancelFunc
	value    EncodedPayment
	err      error
	received bool
}

type Payment struct {
	client    *Client
	initial   transaction
	required  x402.PaymentRequired
	mu        sync.Mutex
	active    *attempt
	complete  *attempt
	cancelled bool
}

func (p *Payment) TransactionID() string { return p.initial.TransactionID }
func (p *Payment) ApprovalID() string    { return p.initial.ApprovalID }

// Prepare creates an InFlow approval without polling. Its context covers preparation only.
func (c *Client) Prepare(ctx context.Context, required x402.PaymentRequired, options SignOptions) (*Payment, error) {
	return c.prepare(ctx, required, options, true)
}

func (c *Client) prepare(ctx context.Context, required x402.PaymentRequired, options SignOptions, runBefore bool) (*Payment, error) {
	if required.X402Version != 2 || len(required.Accepts) != 1 || required.Resource == nil || (options.PaymentID != "" && !x402.ValidatePaymentID(options.PaymentID)) {
		return nil, &Error{Code: "invalid-input"}
	}
	required, err := snapshot(required)
	if err != nil {
		return nil, err
	}
	options, err = snapshot(options)
	if err != nil {
		return nil, err
	}
	supported, err := c.Supported(ctx)
	if err != nil {
		return nil, err
	}
	if !supports(supported, required.Accepts[0]) {
		return nil, &Error{Code: "unsupported-capability"}
	}
	if runBefore {
		if err = c.before(ctx, required); err != nil {
			return nil, err
		}
	}
	body := options.TransactionRequestExtensions
	if body == nil {
		body = make(map[string]any)
	}
	body["accept"] = required.Accepts[0]
	body["resource"] = required.Resource
	body["x402Version"] = 2
	if options.PaymentID != "" {
		body["remotePaymentId"] = options.PaymentID
	}
	raw, err := c.api.Do(ctx, platform.Request{Method: http.MethodPost, Path: "/v1/transactions/x402", Body: body})
	if err != nil {
		return nil, err
	}
	created, err := decode[transaction](raw.Body)
	if err != nil {
		return nil, err
	}
	if created.TransactionID == "" || created.ApprovalID == "" {
		return nil, &Error{Code: "invalid-response"}
	}
	return &Payment{client: c, initial: created, required: required}, nil
}

// Wait does not cancel the approval on timeout or polling failure. Call Wait again
// to resume, or Cancel to abandon it. Concurrent calls share the first call's polling
// context and budget; cancelling a later caller only stops that caller's wait.
// This preserves InFlow Node's resumable x402 approval flow, rather than MPP's
// behavior of storing the failure and attempting approval cancellation. A failed
// x402 wait alone must not abandon the approval.
func (p *Payment) Wait(ctx context.Context) (EncodedPayment, error) {
	if err := ctx.Err(); err != nil {
		return EncodedPayment{}, err
	}
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return EncodedPayment{}, &Error{Code: "payment-cancelled", ApprovalID: p.initial.ApprovalID}
	}
	current := p.complete
	owner := false
	if current == nil {
		current = p.active
	}
	if current == nil {
		owner = true
		poll, cancel := context.WithTimeout(ctx, p.client.options.WaitTimeout)
		current = &attempt{done: make(chan struct{}), cancel: cancel}
		p.active = current
		go p.run(poll, current)
	}
	p.mu.Unlock()
	select {
	case <-current.done:
	case <-ctx.Done():
		if !owner {
			return EncodedPayment{}, ctx.Err()
		}
		<-current.done
	}
	if current.err != nil {
		return EncodedPayment{}, current.err
	}
	return snapshot(current.value)
}

func (p *Payment) run(ctx context.Context, current *attempt) {
	defer current.cancel()
	current.value, current.err = p.poll(ctx)
	if current.err == nil {
		current.received = true
		current.err = p.client.after(ctx, p.required, current.value.PaymentPayload)
	}
	p.mu.Lock()
	if p.cancelled {
		current.err = &Error{Code: "payment-cancelled", ApprovalID: p.initial.ApprovalID}
	}
	p.active = nil
	if current.received {
		// Retain the after-hook result with the payload: rerunning a failed hook
		// could repeat application side effects. Its failure is not a payment-creation failure.
		p.complete = current
	}
	close(current.done)
	p.mu.Unlock()
}

func (p *Payment) poll(ctx context.Context) (EncodedPayment, error) {
	first := p.initial.ApprovalStatus == "APPROVED"
	for {
		raw, err := p.client.api.Do(ctx, platform.Request{Method: http.MethodGet, Path: "/v1/transactions/" + url.PathEscape(p.initial.TransactionID) + "/x402"})
		if ctx.Err() != nil {
			code := "payment-cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = "payment-timeout"
			}
			return EncodedPayment{}, &Error{Code: code, ApprovalID: p.initial.ApprovalID, Cause: ctx.Err()}
		}
		if err != nil {
			var api *inflow.APIError
			if !errors.As(err, &api) || (api.HTTPStatus != 0 && api.HTTPStatus != 429 && api.HTTPStatus < 500) {
				return EncodedPayment{}, err
			}
		} else {
			value, err := decode[payloadResponse](raw.Body)
			if err != nil {
				return EncodedPayment{}, err
			}
			if value.EncodedPayload != nil && value.PaymentPayload != nil {
				return EncodedPayment{EncodedPayload: *value.EncodedPayload, PaymentPayload: *value.PaymentPayload, TransactionID: p.initial.TransactionID}, nil
			}
			switch value.Status {
			case "DECLINED", "EXPIRED", "GENERAL_ERROR", "INSUFFICIENT_FUNDS":
				return EncodedPayment{}, &Error{Code: "payment-failed", Status: value.Status, ApprovalID: p.initial.ApprovalID}
			}
		}
		if first {
			first = false
			continue
		}
		if err = platform.Wait(ctx, p.client.options.PollInterval); err != nil {
			code := "payment-cancelled"
			if errors.Is(err, context.DeadlineExceeded) {
				code = "payment-timeout"
			}
			return EncodedPayment{}, &Error{Code: code, Cause: err, ApprovalID: p.initial.ApprovalID}
		}
	}
}

// Cancel prevents further waits and requests approval cancellation. It does not reverse settlement.
func (p *Payment) Cancel(ctx context.Context) error {
	p.mu.Lock()
	p.cancelled = true
	if p.active != nil {
		p.active.cancel()
	}
	p.mu.Unlock()
	_, err := p.client.api.Do(ctx, platform.Request{Method: http.MethodPost, Path: "/v1/approvals/" + url.PathEscape(p.initial.ApprovalID) + "/cancel"})
	return err
}

func (p *Payment) Status(ctx context.Context) (string, error) {
	raw, err := p.client.api.Do(ctx, platform.Request{Method: http.MethodGet, Path: "/v1/transactions/" + url.PathEscape(p.initial.TransactionID) + "/x402"})
	if err != nil {
		return "", err
	}
	value, err := decode[payloadResponse](raw.Body)
	return value.Status, err
}

func (c *Client) sign(ctx context.Context, required x402.PaymentRequired, options SignOptions) (EncodedPayment, error) {
	p, err := c.prepare(ctx, required, options, false)
	if err != nil {
		return EncodedPayment{}, err
	}
	value, err := p.Wait(ctx)
	p.mu.Lock()
	received := p.complete != nil
	p.mu.Unlock()
	if err != nil && !received {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = p.Cancel(cleanup)
	}
	return value, err
}
