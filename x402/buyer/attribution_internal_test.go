package buyer

import (
	"context"
	"errors"
	"testing"

	"github.com/inflowpayai/inflow-go/x402"
	"github.com/x402-foundation/x402/go/v2/extensions/buildercode"
)

func TestManagedAttributionCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, configured := range []bool{false, true} {
		options := Options{}
		if configured {
			options.BuilderCode = buildercode.NewBuilderCodeClientExtension("buyer")
		}
		c, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.managedExtensions(ctx, x402.PaymentRequired{Accepts: []x402.PaymentRequirements{{Scheme: "exact", Network: "eip155:8453"}}})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}
