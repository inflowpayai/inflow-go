package buyer

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/inflowpayai/inflow-go/x402"
)

var ErrBodyNotReplayable = errors.New("x402 payment requires a replayable request body; provide Request.GetBody")

// Do sends one unpaid request and at most one paid replay. It preserves application
// authentication and never follows redirects. The caller closes the returned response body.
func (c *Client) Do(request *http.Request, options SignOptions) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("x402 resource request is required")
	}
	response, err := c.resource.Do(singleAttempt(request.Clone(request.Context())))
	if err != nil || response.StatusCode != http.StatusPaymentRequired {
		return response, err
	}
	response.Body.Close()
	headers := response.Header.Values(x402.HeaderPaymentRequired)
	if len(headers) != 1 || len(headers[0]) > 8<<20 {
		return nil, &Error{Code: "invalid-response"}
	}
	data, err := base64.StdEncoding.DecodeString(headers[0])
	if err != nil {
		return nil, &Error{Code: "invalid-response", Cause: err}
	}
	required, err := decode[x402.PaymentRequired](data)
	if err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	if request.Body != nil && request.Body != http.NoBody {
		if request.GetBody == nil {
			return nil, ErrBodyNotReplayable
		}
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, err
		}
		if retry.Body == nil {
			return nil, ErrBodyNotReplayable
		}
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
	if retry.Header == nil {
		retry.Header = make(http.Header)
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

func singleAttempt(request *http.Request) *http.Request {
	// Paid GET requests must not be transparently replayed by net/http on a reused connection.
	request.GetBody = nil
	if request.Body == nil || request.Body == http.NoBody {
		request.Body = io.NopCloser(strings.NewReader(""))
	}
	return request
}
