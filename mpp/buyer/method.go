package buyer

import (
	"context"
	"errors"

	"github.com/inflowpayai/inflow-go/internal/mppwire"
	"github.com/inflowpayai/inflow-go/mpp"
	foundationclient "github.com/tempoxyz/mpp-go/pkg/client"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

// Method connects the managed buyer's approval flow to upstream HTTP and MCP clients.
// The upstream transport controls challenge selection, origins, and payment retries.
func (c *Client) Method(name, intent string, options PaymentOptions, limits ...MethodLimits) (foundationclient.Method, error) {
	if (name != mpp.MethodInflow && name != mpp.MethodTempo && name != mpp.MethodCard) ||
		(intent != mpp.IntentCharge && (name != mpp.MethodInflow || intent != mpp.IntentSubscription)) {
		return nil, &Error{Code: Unsupported}
	}
	check, err := newMethodLimits(name, intent, limits)
	if err != nil {
		return nil, err
	}
	return &method{client: c, name: name, intent: intent, options: copyPaymentOptions(options), checkLimits: check}, nil
}

type method struct {
	client      *Client
	name        string
	intent      string
	options     PaymentOptions
	checkLimits func(mpp.Challenge) error
}

func (m *method) Name() string   { return m.name }
func (m *method) Intent() string { return m.intent }

func (m *method) CanHandleChallenge(challenge *foundation.Challenge) bool {
	if challenge == nil || challenge.Method != m.name || challenge.Intent != m.intent {
		return false
	}
	wire, err := mppwire.FromChallenge(*challenge)
	if err != nil {
		return false
	}
	// The upstream matcher has no request context. Payment selectors and merchant
	// details are validated in CreateCredential after resolving request options.
	if m.name == mpp.MethodCard {
		_, err = mpp.DecodeCardRequest(wire.Request)
	}
	return err == nil && m.checkLimits(wire) == nil
}

func (m *method) CreateCredential(ctx context.Context, challenge *foundation.Challenge) (*foundation.Credential, error) {
	if challenge == nil {
		return nil, errors.New("MPP challenge is required")
	}
	if challenge.Method != m.name || challenge.Intent != m.intent {
		return nil, &Error{Code: Unsupported}
	}
	wire, err := mppwire.FromChallenge(*challenge)
	if err != nil {
		return nil, err
	}
	if err := m.checkLimits(wire); err != nil {
		return nil, err
	}
	options, overridden := ctx.Value(paymentOptionsKey{}).(PaymentOptions)
	if !overridden {
		options = m.options
	}
	credential, err := m.client.Fulfil(ctx, wire, copyPaymentOptions(options))
	if err != nil {
		return nil, err
	}
	native, err := mppwire.ToCredential(credential)
	if err != nil {
		return nil, err
	}
	return &native, nil
}
