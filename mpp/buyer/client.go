// Package buyer obtains MPP credentials through InFlow's managed buyer accounts.
package buyer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/mpp"
)

type Options struct {
	inflow.Options
	// Zero selects five seconds. Server polling advice takes precedence.
	PollInterval time.Duration
	// WaitTimeout starts after creation returns. Zero selects fifteen minutes.
	WaitTimeout time.Duration
}

type PaymentOptions struct {
	InstrumentID string        `json:"instrumentId,omitempty"`
	Merchant     *CardMerchant `json:"merchant,omitempty"`
	// SubscriptionID selects authorization of an existing subscription, not purchase.
	SubscriptionID string `json:"-"`
}

// CardMerchant describes the purchase merchant, not the Buyer account.
// InFlow remains authoritative for registered merchant details and card allowances.
type CardMerchant struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	CountryCode string `json:"countryCode"`
}

type Client struct {
	api          *platform.Client
	resource     *http.Client
	pollInterval time.Duration
	waitTimeout  time.Duration
}

func New(options Options) (*Client, error) {
	if options.PollInterval < 0 || options.WaitTimeout < 0 {
		return nil, errors.New("MPP polling interval and wait timeout must not be negative")
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
	resource := &http.Client{Transport: options.Transport, Timeout: options.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{api: api, resource: resource, pollInterval: options.PollInterval, waitTimeout: options.WaitTimeout}, nil
}

// PaymentStatus reads an existing payment without creating or cancelling one.
// It neither follows a next-action URL nor treats credential readiness as settlement.
func (c *Client) PaymentStatus(ctx context.Context, transactionID string, options inflow.PaymentStatusOptions) (inflow.PaymentStatus, error) {
	return c.api.PaymentStatus(ctx, transactionID, options)
}

// Fulfil creates or authorizes a payment and waits for its credential.
func (c *Client) Fulfil(ctx context.Context, challenge mpp.Challenge, options PaymentOptions) (mpp.Credential, error) {
	payment, err := c.Prepare(ctx, challenge, options)
	if err != nil {
		return mpp.Credential{}, err
	}
	return payment.Wait(ctx)
}

// Prepare performs creation or subscription authorization without polling.
// Call Wait or Cancel on the returned handle. Its context owns the payment's lifetime.
func (c *Client) Prepare(ctx context.Context, challenge mpp.Challenge, options PaymentOptions) (*Payment, error) {
	if err := validate(challenge, options); err != nil {
		return nil, err
	}
	var expectedCardChallenge string
	if challenge.Method == mpp.MethodCard {
		var err error
		expectedCardChallenge, err = mpp.Encode(challenge)
		if err != nil {
			return nil, err
		}
	}
	path := "/v1/transactions/mpp"
	var body any = struct {
		Challenge mpp.Challenge  `json:"challenge"`
		Options   PaymentOptions `json:"options"`
	}{challenge, options}
	authorize := options.SubscriptionID != ""
	if authorize {
		path = "/v1/subscriptions/" + url.PathEscape(options.SubscriptionID) + "/authorize"
		body = struct {
			Challenge mpp.Challenge `json:"challenge"`
		}{challenge}
	}
	raw, err := c.api.Do(ctx, platform.Request{Method: http.MethodPost, Path: path, Body: body})
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Code: Cancelled, Cause: ctx.Err()}
		}
		return nil, err
	}
	response, err := platform.Decode[transaction](raw)
	if err != nil {
		return nil, &Error{Code: InvalidResponse, Cause: err}
	}
	if authorize {
		response.State = "ready"
		if len(response.Problem) > 0 && string(response.Problem) != "null" {
			response.State = "failed"
		}
		// Authorization cannot create a cancellable purchase approval.
		response.ApprovalID = ""
	}
	budget, release := context.WithTimeoutCause(ctx, c.waitTimeout, pendingTimeout)
	waitCtx, cancel := context.WithCancelCause(budget)
	return &Payment{client: c, parent: ctx, ctx: waitCtx, cancel: cancel, release: release, initial: response, expectedCardChallenge: expectedCardChallenge, done: make(chan struct{})}, nil
}

var pendingTimeout = errors.New("MPP pending budget expired")

func validate(challenge mpp.Challenge, options PaymentOptions) error {
	if _, err := mpp.RenderChallenge(challenge); err != nil {
		return err
	}
	if challenge.Method != mpp.MethodInflow && challenge.Method != mpp.MethodTempo && challenge.Method != mpp.MethodCard ||
		challenge.Intent != mpp.IntentCharge && challenge.Intent != mpp.IntentSubscription ||
		challenge.Method != mpp.MethodInflow && challenge.Intent != mpp.IntentCharge {
		return &Error{Code: Unsupported}
	}
	if options.Merchant != nil && challenge.Method != mpp.MethodCard {
		return &Error{Code: Unsupported}
	}
	if challenge.Method == mpp.MethodCard {
		if _, err := mpp.DecodeCardRequest(challenge.Request); err != nil {
			return err
		}
		merchant := options.Merchant
		length := func(s string) int { return len(utf16.Encode([]rune(s))) }
		if merchant == nil || strings.TrimSpace(merchant.Name) == "" || length(merchant.Name) > 200 || length(merchant.URL) > 2048 || len(merchant.CountryCode) != 2 {
			return &Error{Code: InvalidInput}
		}
		parsed, err := url.Parse(merchant.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return &Error{Code: InvalidInput}
		}
		for _, letter := range merchant.CountryCode {
			if !(letter >= 'A' && letter <= 'Z' || letter >= 'a' && letter <= 'z') {
				return &Error{Code: InvalidInput}
			}
		}
		if options.InstrumentID != "" && !uuid(options.InstrumentID) || options.SubscriptionID != "" {
			return &Error{Code: InvalidInput}
		}
	}
	if options.InstrumentID != "" && (challenge.Method != mpp.MethodInflow && challenge.Method != mpp.MethodCard || challenge.Intent != mpp.IntentCharge) ||
		options.SubscriptionID != "" && (challenge.Method != mpp.MethodInflow || challenge.Intent != mpp.IntentSubscription) {
		return errors.New("MPP payment options do not match the challenge method and intent")
	}
	for _, id := range []string{options.InstrumentID, options.SubscriptionID} {
		if id != "" && !uuid(id) {
			return errors.New("MPP instrument and subscription identifiers must be UUIDs")
		}
	}
	return nil
}

func uuid(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

type transaction struct {
	State             string          `json:"state"`
	TransactionID     string          `json:"transactionId"`
	ApprovalID        string          `json:"approvalId"`
	Credential        string          `json:"credential"`
	RetryAfterSeconds *int64          `json:"retryAfterSeconds"`
	Problem           json.RawMessage `json:"problem"`
}

type ErrorCode string

const (
	Cancelled         ErrorCode = "payment-cancelled"
	Timeout           ErrorCode = "payment-timeout"
	Failed            ErrorCode = "payment-failed"
	Expired           ErrorCode = "payment-expired"
	InvalidCredential ErrorCode = "invalid-credential"
	InvalidResponse   ErrorCode = "invalid-response"
	Unsupported       ErrorCode = "unsupported-capability"
	InvalidInput      ErrorCode = "invalid-input"
)

type Error struct {
	Code          ErrorCode
	TransactionID string
	ApprovalID    string
	Problem       json.RawMessage
	Cause         error
}

func (e *Error) Error() string { return "MPP " + string(e.Code) }
func (e *Error) Unwrap() error { return e.Cause }
