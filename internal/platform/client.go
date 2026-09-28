package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
)

const maxBodyBytes = 8 << 20

type Client struct {
	baseURL   string
	options   inflow.Options
	http      *http.Client
	userAgent string
}

// Request is internal: each protocol operation decides whether retrying is safe.
// Zero Retries performs one attempt; retries are capped at three.
type Request struct {
	Method  string
	Path    string
	Body    any
	Headers http.Header
	Retries int
}

type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func New(options inflow.Options) (*Client, error) {
	if options.Environment != "" && options.Environment != inflow.Production && options.Environment != inflow.Sandbox {
		return nil, errors.New("unknown InFlow environment")
	}
	base := options.BaseURL
	if base == "" {
		base = "https://api.inflowpay.ai"
		if options.Environment == inflow.Sandbox {
			base = "https://sandbox.inflowpay.ai"
		}
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("InFlow base URL must be an HTTP or HTTPS URL without credentials, query, or fragment")
	}
	if options.APIKey != "" && options.AccessToken != nil {
		return nil, errors.New("API key and access token provider are mutually exclusive")
	}
	if options.APIKey != "" && !validCredential(options.APIKey) {
		return nil, errors.New("API key must be nonempty and contain no whitespace or control characters")
	}
	if options.Timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	info, _ := debug.ReadBuildInfo()
	return &Client{
		baseURL: strings.TrimRight(u.String(), "/"), options: options,
		userAgent: sdkUserAgent(info),
		http: &http.Client{Transport: options.Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func validCredential(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c <= ' ' || c >= 127 {
			return false
		}
	}
	return true
}

func (c *Client) Do(ctx context.Context, input Request) (*Response, error) {
	u, err := url.Parse(input.Path)
	if err != nil || !strings.HasPrefix(input.Path, "/") || strings.HasPrefix(input.Path, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return nil, errors.New("expected a relative InFlow API path beginning with one slash")
	}
	if input.Retries < 0 {
		return nil, errors.New("retries must not be negative")
	}
	var body []byte
	if input.Body != nil {
		body, err = json.Marshal(input.Body)
		if err != nil {
			return nil, errors.New("request body cannot be encoded as JSON")
		}
		if len(body) > maxBodyBytes {
			return nil, errors.New("request body exceeds 8 MiB")
		}
	}
	headers := input.Headers.Clone()
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, transportError(input.Path, ctx.Err())
		}
		response, retry, err := c.attempt(ctx, input, headers, body)
		if err == nil || !retry || attempt >= min(input.Retries, 3) || ctx.Err() != nil {
			return response, err
		}
		base := 200 * time.Millisecond * time.Duration(1<<attempt)
		if err := Wait(ctx, base+time.Duration(rand.Int64N(int64(base/4)))); err != nil {
			return nil, transportError(input.Path, err)
		}
	}
}

func (c *Client) attempt(parent context.Context, input Request, headers http.Header, body []byte) (*Response, bool, error) {
	ctx, cancel := context.WithTimeout(parent, c.options.Timeout)
	defer cancel()
	token := ""
	if c.options.AccessToken != nil {
		var err error
		token, err = c.options.AccessToken(ctx)
		if err != nil {
			return nil, false, err
		}
		if !validCredential(token) {
			return nil, false, errors.New("access token must be nonempty and contain no whitespace or control characters")
		}
	}
	if ctx.Err() != nil {
		return nil, false, transportError(input.Path, ctx.Err())
	}
	req, err := http.NewRequestWithContext(ctx, input.Method, c.baseURL+input.Path, bytes.NewReader(body))
	if err != nil {
		return nil, false, errors.New("invalid InFlow HTTP request")
	}
	// net/http treats an Idempotency-Key as permission to replay a mutation on
	// connection failure. Only the protocol operation may authorize that retry.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.GetBody = nil
		if req.Body == nil || req.Body == http.NoBody {
			req.Body = io.NopCloser(bytes.NewReader(nil))
		}
	}
	// Header maps can contain noncanonical keys. Rebuild rather than allowing a
	// second, differently cased authentication field alongside SDK credentials.
	for name, values := range headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-API-Key") || strings.EqualFold(name, "Cookie") {
			return nil, false, errors.New("request headers must not override authentication")
		}
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.options.APIKey != "" {
		req.Header.Set("X-API-Key", c.options.APIKey)
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	raw, err := c.http.Do(req)
	if err != nil {
		cause := ctx.Err()
		return nil, true, transportError(input.Path, cause)
	}
	defer raw.Body.Close()
	data, err := io.ReadAll(io.LimitReader(raw.Body, maxBodyBytes+1))
	if ctx.Err() != nil {
		return nil, true, transportError(input.Path, ctx.Err())
	}
	if err != nil {
		return nil, true, transportError(input.Path, nil)
	}
	if len(data) > maxBodyBytes {
		return nil, false, &inflow.APIError{Code: "RESPONSE_TOO_LARGE", Message: "InFlow response exceeds 8 MiB", HTTPStatus: raw.StatusCode, Endpoint: input.Path}
	}
	if raw.StatusCode >= 200 && raw.StatusCode < 300 {
		return &Response{Status: raw.StatusCode, Headers: raw.Header.Clone(), Body: data}, false, nil
	}
	retry := raw.StatusCode == 429 || raw.StatusCode == 502 || raw.StatusCode == 503 || raw.StatusCode == 504
	return nil, retry, responseError(input.Path, raw.StatusCode, raw.Header, data, c.options.APIKey, token)
}

func transportError(path string, cause error) *inflow.APIError {
	code, message := "NETWORK_ERROR", "InFlow request failed"
	if errors.Is(cause, context.DeadlineExceeded) {
		code, message = "TIMEOUT", "InFlow request timed out"
	} else if errors.Is(cause, context.Canceled) {
		message = "InFlow request cancelled"
	}
	return &inflow.APIError{Code: code, Message: message, Endpoint: path, Cause: cause}
}

// Decode leaves empty successful responses (including cancellation's 204) empty.
// Protocol callers provide the concrete response type.
func Decode[T any](response *Response) (T, error) {
	var result T
	if len(response.Body) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return result, fmt.Errorf("invalid InFlow JSON response: %w", err)
	}
	return result, nil
}

// Wait is shared by retry and polling paths; cancellation never initiates a
// server-side cancellation request. The owning payment flow makes that decision.
func Wait(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
