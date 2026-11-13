package seller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/inflowpayai/inflow-go/internal/mppwire"
	"github.com/inflowpayai/inflow-go/mpp"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
	foundationserver "github.com/tempoxyz/mpp-go/pkg/server"
)

// Method prepares one fixed offer for upstream HTTP and MCP servers. Its intents
// validate without settling; Broadcast revalidates before accepting payment.
// The upstream server verifies signatures and expiry. Its HTTP middleware also
// checks route and body binding; split-lifecycle callers must check these themselves.
func (c *Client) Method(ctx context.Context, offer Offer) (foundationserver.Method, error) {
	prepared, err := c.Prepare(ctx, offer)
	if err != nil {
		return nil, err
	}
	return &method{client: c, offer: prepared}, nil
}

type method struct {
	client *Client
	offer  PreparedOffer
}

func (m *method) Name() string { return m.offer.Method }
func (m *method) Intents() map[string]foundationserver.Intent {
	return map[string]foundationserver.Intent{m.offer.Intent: &methodIntent{method: m}}
}

func (m *method) BuildChargeRequest(params foundationserver.ChargeParams) (map[string]any, error) {
	// Prices and payment routes come from the prepared offer. Silently accepting
	// an upstream override could charge a different amount than the caller expects.
	if m.offer.Intent != mpp.IntentCharge || params.Amount != "" || params.Currency != "" || params.Recipient != "" || params.ExternalID != "" ||
		len(params.Splits) != 0 || params.FeePayer || params.DisableFeePayer || params.FeePayerURL != "" || len(params.SupportedModes) != 0 || params.ChainID != 0 {
		return nil, errors.New("configure payment terms in the InFlow offer, not ChargeParams")
	}
	return requestObject(m.offer.Request)
}

type methodIntent struct{ method *method }

func (i *methodIntent) Name() string { return i.method.offer.Intent }

func (i *methodIntent) Validate(ctx context.Context, credential *foundation.Credential, request map[string]any) (*foundationserver.Validation, error) {
	if credential == nil {
		return nil, foundation.ErrVerification
	}
	wire, err := mppwire.FromCredential(*credential)
	if err != nil {
		return nil, err
	}
	offer := i.method.offer
	actual, err := requestObject(wire.Challenge.Request)
	if err != nil {
		return nil, err
	}
	encoded, err := mpp.Encode(actual)
	if err != nil {
		return nil, err
	}
	requested, err := mpp.Encode(request)
	if err != nil {
		return nil, err
	}
	if wire.Challenge.Method != offer.Method || wire.Challenge.Intent != offer.Intent || encoded != offer.Request || requested != offer.Request {
		return nil, foundation.ErrVerification
	}
	value, err := i.method.client.Validate(ctx, wire)
	if err != nil {
		return nil, methodError(err)
	}
	details := make(map[string]any, len(value.Details))
	for key, raw := range value.Details {
		// Validate has already parsed these JSON values. UseNumber preserves proof-sized integers.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var item any
		_ = decoder.Decode(&item)
		details[key] = item
	}
	return &foundationserver.Validation{Challenge: credential.Challenge, Credential: credential, Details: details, Intent: offer.Intent, Method: offer.Method, Request: request, Source: credential.Source}, nil
}

func (i *methodIntent) Broadcast(ctx context.Context, credential *foundation.Credential, request map[string]any) (*foundation.Receipt, error) {
	if _, err := i.Validate(ctx, credential, request); err != nil {
		return nil, err
	}
	// Validate checked this conversion without mutating the caller's credential.
	wire, _ := mppwire.FromCredential(*credential)
	receipt, err := i.method.client.Broadcast(ctx, wire, "")
	if err != nil {
		return nil, methodError(err)
	}
	return mppwire.ToReceipt(receipt)
}

func (i *methodIntent) Verify(ctx context.Context, credential *foundation.Credential, request map[string]any) (*foundation.Receipt, error) {
	return i.Broadcast(ctx, credential, request)
}

func methodError(err error) error {
	var failure *Error
	var problem foundation.PaymentError
	if errors.As(err, &failure) && json.Unmarshal(failure.Problem, &problem) == nil && problem.Status >= 400 && problem.Status <= 599 {
		// Preserve pending and uncertain outcomes rather than turning them into a new payment challenge.
		return problem.WithCause(err)
	}
	return err
}
