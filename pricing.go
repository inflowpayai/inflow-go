package inflow

import "net/http"

// RequestPricingOptions controls request data exposed to seller pricing callbacks.
type RequestPricingOptions struct {
	// MaxBodyBytes enables bounded body access when positive. Zero leaves the body
	// unread and passes nil to the callback. Negative values are invalid.
	MaxBodyBytes int64
	// OnError receives pricing and body-read errors without exposing them to buyers.
	// It must be safe for concurrent calls. The SDK does not log these errors.
	OnError func(*http.Request, error)
}
