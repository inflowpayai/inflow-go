package inflow

import (
	"context"
	"net/http"
	"time"
)

type Environment string

const (
	Production Environment = "production"
	Sandbox    Environment = "sandbox"
)

// Options configures an MPP or x402 client. Its zero value selects anonymous
// requests to production. APIKey and AccessToken are mutually exclusive.
type Options struct {
	Environment Environment
	BaseURL     string
	APIKey      string
	// AccessToken is called for each attempt, including retries. It must honor
	// cancellation and be safe for concurrent calls. Provider errors are returned unchanged.
	AccessToken func(context.Context) (string, error)
	// Timeout covers each attempt, including token retrieval and reading the response.
	// Zero selects 30 seconds. An earlier caller deadline takes precedence.
	Timeout time.Duration
	// Transport must honor request cancellation and perform a single HTTP exchange
	// without following redirects. Nil selects http.DefaultTransport.
	Transport http.RoundTripper
}
