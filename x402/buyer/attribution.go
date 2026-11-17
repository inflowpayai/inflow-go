package buyer

import (
	"context"

	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/extensions/buildercode"
)

// The native client owns extension merging. This metadata-only scheme does not
// sign or pay: attribution must reach the server before it creates an approval,
// so later retrieval returns the same canonical payload without local enrichment.
type attributionScheme string

func (s attributionScheme) Scheme() string { return string(s) }

func (s attributionScheme) CreatePaymentPayload(ctx context.Context, _ x402.PaymentRequirements, _ foundation.PaymentPayloadContext) (x402.PaymentPayload, error) {
	return x402.PaymentPayload{X402Version: 2, Payload: map[string]any{}}, ctx.Err()
}

func (c *Client) managedExtensions(ctx context.Context, required x402.PaymentRequired) (map[string]any, error) {
	extensions := make(map[string]any)
	if declaration, ok := required.Extensions[buildercode.BUILDER_CODE]; ok {
		extensions[buildercode.BUILDER_CODE] = declaration
	}
	if len(extensions) == 0 && c.options.BuilderCode == nil {
		return nil, ctx.Err()
	}
	requirement := required.Accepts[0]
	client := foundation.Newx402Client().DisableSpendControls()
	client.Register(foundation.Network(requirement.Network), attributionScheme(requirement.Scheme))
	if c.options.BuilderCode != nil {
		client.RegisterExtension(c.options.BuilderCode)
	}
	payload, err := client.CreatePaymentPayload(ctx, requirement, required.Resource, extensions)
	if err != nil {
		return nil, err
	}
	return payload.Extensions, ctx.Err()
}
