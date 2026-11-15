package buyer

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	foundationhttp "github.com/x402-foundation/x402/go/v2/http"
)

var ErrBodyNotReplayable = errors.New("x402 payment requires a replayable request body; provide Request.GetBody")

// Do sends one unpaid request, at most one authentication retry, and at most one paid replay.
// It preserves application authentication and never follows redirects. The caller closes the returned response body.
// Requests containing a payment credential are sent once without creating a replacement payment.
func (c *Client) Do(request *http.Request, options SignOptions) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("x402 resource request is required")
	}
	alreadyPaid := false
	for name := range request.Header {
		if strings.EqualFold(name, x402.HeaderPaymentSignature) || strings.EqualFold(name, "X-Payment") {
			alreadyPaid = true
			break
		}
	}
	response, err := c.resource.Do(singleAttempt(request.Clone(request.Context())))
	if err != nil || response.StatusCode != http.StatusPaymentRequired || alreadyPaid {
		return response, err
	}
	required, err := readPaymentRequired(response)
	if err != nil {
		return nil, err
	}
	// Establish replayability before invoking a wallet signer or creating a payment.
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return nil, ErrBodyNotReplayable
	}
	authHeaders, err := c.authenticationHeaders(request, required)
	if err != nil {
		return nil, err
	}
	if len(authHeaders) > 0 {
		authRequest, err := replayRequest(request)
		if err != nil {
			return nil, err
		}
		for name, value := range authHeaders {
			authRequest.Header.Set(name, value)
		}
		response, err = c.resource.Do(singleAttempt(authRequest))
		if err != nil || response.StatusCode != http.StatusPaymentRequired {
			return response, err
		}
		// A rejected proof can return different payment terms; do not pay the stale offer.
		required, err = readPaymentRequired(response)
		if err != nil {
			return nil, err
		}
	}
	retry, err := replayRequest(request)
	if err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff && retry.Body != nil {
			retry.Body.Close()
		}
	}()
	payment, err := c.Sign(request.Context(), required, options)
	if err != nil {
		return nil, err
	}
	for name := range retry.Header {
		if strings.EqualFold(name, x402.HeaderPaymentSignature) || strings.EqualFold(name, x402.HeaderPaymentRequired) || strings.EqualFold(name, x402.HeaderPaymentResponse) {
			delete(retry.Header, name)
		}
	}
	retry.Header.Set(x402.HeaderPaymentSignature, payment.EncodedPayload)
	handedOff = true
	return c.resource.Do(singleAttempt(retry))
}

func readPaymentRequired(response *http.Response) (x402.PaymentRequired, error) {
	response.Body.Close()
	headers := response.Header.Values(x402.HeaderPaymentRequired)
	if len(headers) != 1 || len(headers[0]) > 8<<20 {
		return x402.PaymentRequired{}, &Error{Code: "invalid-response"}
	}
	data, err := base64.StdEncoding.DecodeString(headers[0])
	if err != nil {
		return x402.PaymentRequired{}, &Error{Code: "invalid-response", Cause: err}
	}
	return decode[x402.PaymentRequired](data)
}

func replayRequest(request *http.Request) (*http.Request, error) {
	retry := request.Clone(request.Context())
	if retry.Header == nil {
		retry.Header = make(http.Header)
	}
	if request.Body != nil && request.Body != http.NoBody {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		if body == nil {
			return nil, ErrBodyNotReplayable
		}
		retry.Body = body
	}
	return retry, nil
}

func (c *Client) authenticationHeaders(request *http.Request, required x402.PaymentRequired) (map[string]string, error) {
	provider, ok := c.options.External.(interface {
		GetExtensions() []foundation.ClientExtension
	})
	if !ok || required.X402Version != 2 {
		return nil, nil
	}
	for _, extension := range provider.GetExtensions() {
		if _, declared := required.Extensions[extension.Key()]; !declared {
			continue
		}
		hooks, ok := extension.(foundationhttp.ClientExtensionPaymentRequiredHookProvider)
		if !ok {
			continue
		}
		hook := hooks.PaymentRequiredHook()
		if hook == nil {
			continue
		}
		value, err := snapshot(required)
		if err != nil {
			return nil, err
		}
		result, err := hook(request.Context(), value, request.URL.String())
		if err != nil {
			return nil, err
		}
		if result != nil && len(result.Headers) > 0 {
			return result.Headers, nil
		}
	}
	return nil, nil
}

func singleAttempt(request *http.Request) *http.Request {
	// Paid GET requests must not be transparently replayed by net/http on a reused connection.
	request.GetBody = nil
	if request.Body == nil || request.Body == http.NoBody {
		request.Body = io.NopCloser(strings.NewReader(""))
	}
	return request
}
