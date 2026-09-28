package inflow

import "net/http"

// APIError preserves an InFlow failure without including request credentials.
// HTTPStatus is zero for local transport failures. Errors from token providers
// are returned unchanged rather than converted into APIError.
type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
	Endpoint   string
	RequestID  string
	Headers    http.Header
	// Body contains the decoded error response with credential fields redacted.
	Body any
	// Cause supports errors.Is for context cancellation and deadlines.
	Cause error
}

func (e *APIError) Error() string { return e.Message }

func (e *APIError) Unwrap() error { return e.Cause }
