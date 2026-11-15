package buyer

import "context"

type paymentOptionsKey struct{}

// WithPaymentOptions replaces a native Method's defaults for this request.
// An empty value clears all defaults. Values are copied; cancellation remains
// controlled by ctx. Direct Do, Prepare, and Fulfil calls use their explicit options.
func WithPaymentOptions(ctx context.Context, options PaymentOptions) context.Context {
	return context.WithValue(ctx, paymentOptionsKey{}, copyPaymentOptions(options))
}

func copyPaymentOptions(options PaymentOptions) PaymentOptions {
	if options.Merchant != nil {
		merchant := *options.Merchant
		options.Merchant = &merchant
	}
	return options
}
