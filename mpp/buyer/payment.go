package buyer

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/mpp"
)

// Payment shares one polling sequence and terminal outcome across concurrent waits.
// Cancelling any wait abandons this payment for all waiters. Separate payments are independent.
type Payment struct {
	client     *Client
	parent     context.Context
	ctx        context.Context
	cancel     context.CancelCauseFunc
	release    context.CancelFunc
	initial    transaction
	once       sync.Once
	done       chan struct{}
	credential string
	err        error
	cleanupErr error
}

func (p *Payment) TransactionID() string { return p.initial.TransactionID }
func (p *Payment) ApprovalID() string    { return p.initial.ApprovalID }

func (p *Payment) start() { p.once.Do(func() { go p.run() }) }

// Wait returns an independent credential value. The pending budget includes time
// spent between Prepare and Wait. Cancellation waits for bounded approval cleanup.
func (p *Payment) Wait(ctx context.Context) (mpp.Credential, error) {
	encoded, err := p.wait(ctx)
	if err != nil {
		return mpp.Credential{}, err
	}
	return mpp.DecodeCredential(encoded)
}

func (p *Payment) wait(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		p.cancel(ctx.Err())
	}
	p.start()
	select {
	case <-p.done:
	case <-ctx.Done():
		p.cancel(ctx.Err())
		<-p.done
	}
	if p.err != nil {
		return "", p.err
	}
	return p.credential, nil
}

// Cancel abandons an unfinished payment and returns its approval cleanup error,
// if any. It does not reverse a completed payment or cancel a subscription.
func (p *Payment) Cancel(ctx context.Context) error {
	select {
	case <-p.done:
		return p.cleanupErr
	default:
	}
	p.cancel(context.Canceled)
	p.start()
	select {
	case <-p.done:
		return p.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Payment) run() {
	defer close(p.done)
	defer p.release()
	defer p.cancel(context.Canceled)
	current := p.initial
	approvalID := current.ApprovalID
	transactionID := current.TransactionID
	defer func() {
		if p.err == nil || approvalID == "" {
			return
		}
		// Cleanup must work even after payment cancellation. Retain authentication
		// context values, with a separate five-second deadline for the known approval.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(p.parent), 5*time.Second)
		defer cancel()
		_, p.cleanupErr = p.client.api.Do(ctx, platform.Request{
			Method: http.MethodPost, Path: "/v1/approvals/" + url.PathEscape(approvalID) + "/cancel",
		})
	}()
	for {
		if err := p.ctx.Err(); err != nil {
			code := Cancelled
			cause := context.Cause(p.ctx)
			if cause == pendingTimeout {
				code = Timeout
				cause = context.DeadlineExceeded
			}
			p.err = &Error{Code: code, TransactionID: transactionID, ApprovalID: approvalID, Cause: cause}
			return
		}
		switch current.State {
		case "ready":
			if _, err := mpp.DecodeCredential(current.Credential); err != nil {
				p.err = &Error{Code: InvalidCredential, Cause: err}
			} else {
				p.credential = current.Credential
			}
			return
		case "failed":
			p.err = &Error{Code: Failed, Problem: current.Problem}
			return
		case "expired":
			p.err = &Error{Code: Expired, TransactionID: transactionID}
			return
		case "pending":
			if current.TransactionID == "" {
				p.err = &Error{Code: InvalidCredential}
				return
			}
		default:
			p.err = &Error{Code: InvalidResponse}
			return
		}
		delay := p.client.pollInterval
		if current.RetryAfterSeconds != nil {
			seconds := *current.RetryAfterSeconds
			if seconds < 0 {
				p.err = &Error{Code: InvalidResponse}
				return
			}
			// Cap before conversion to Duration so large advice cannot overflow.
			deadline, _ := p.ctx.Deadline()
			remaining := time.Until(deadline)
			delay = remaining
			if seconds <= int64(remaining/time.Second) {
				delay = time.Duration(seconds) * time.Second
			}
		}
		if err := platform.Wait(p.ctx, delay); err != nil {
			continue
		}
		raw, err := p.client.api.Do(p.ctx, platform.Request{Method: http.MethodGet,
			Path: "/v1/transactions/" + url.PathEscape(transactionID) + "/mpp"})
		if p.ctx.Err() != nil {
			continue
		}
		if err != nil {
			p.err = err
			return
		}
		current, err = platform.Decode[transaction](raw)
		if err != nil {
			p.err = &Error{Code: InvalidResponse, Cause: err}
			return
		}
		if current.ApprovalID != "" {
			approvalID = current.ApprovalID
		}
		if current.TransactionID != "" {
			transactionID = current.TransactionID
		}
	}
}
