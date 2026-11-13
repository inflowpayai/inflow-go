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
func (c *Client) Method(name, intent string, options PaymentOptions) (foundationclient.Method, error) {
	if (name != mpp.MethodInflow && name != mpp.MethodTempo && name != mpp.MethodCard) ||
		(intent != mpp.IntentCharge && (name != mpp.MethodInflow || intent != mpp.IntentSubscription)) {
		return nil, &Error{Code: Unsupported}
	}
	if options.Merchant != nil {
		merchant := *options.Merchant
		options.Merchant = &merchant
	}
	return &method{client: c, name: name, intent: intent, options: options}, nil
}

type method struct {
	client  *Client
	name    string
	intent  string
	options PaymentOptions
}

func (m *method) Name() string   { return m.name }
func (m *method) Intent() string { return m.intent }

func (m *method) CanHandleChallenge(challenge *foundation.Challenge) bool {
	if challenge == nil || challenge.Method != m.name || challenge.Intent != m.intent {
		return false
	}
	wire, err := mppwire.FromChallenge(*challenge)
	return err == nil && validate(wire, m.options) == nil
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
	credential, err := m.client.Fulfil(ctx, wire, m.options)
	if err != nil {
		return nil, err
	}
	native, err := mppwire.ToCredential(credential)
	if err != nil {
		return nil, err
	}
	return &native, nil
}
