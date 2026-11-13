package seller

import (
	"context"
	"errors"
	"net/http"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/requestpricing"
	upstream "github.com/tempoxyz/mpp-go/pkg/mpp"
)

// ProtectRequest resolves a complete route once per request, before payment.
// The resolver must be concurrency-safe and honor the request context. Its request
// has no readable body; body bytes are supplied separately when enabled.
// Body-aware challenges bind the exact request bytes to the credential.
func (c *Client) ProtectRequest(options inflow.RequestPricingOptions, resolve func(*http.Request, []byte) (Route, error), next http.Handler) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("request pricing requires a handler")
	}
	return requestpricing.Handler(options, resolve, func(_ context.Context, route Route, body []byte) (http.Handler, error) {
		var digest *string
		if options.MaxBodyBytes > 0 {
			value := upstream.ComputeBodyDigest(body)
			digest = &value
		}
		return c.protect(route, next, digest)
	})
}
