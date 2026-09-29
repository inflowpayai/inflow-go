package buyer

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
)

var (
	ErrAuthorizationConflict = errors.New("MPP payment requires Authorization; use separate application authentication or handle the request explicitly")
	ErrBodyNotReplayable     = errors.New("MPP payment requires a replayable request body; provide Request.GetBody")
)

// Do sends a resource request and, on 402, pays the first supported challenge in
// server order and sends one paid retry. It never follows redirects or starts a
// second payment for a rejected retry. The caller closes the returned response body.
func (c *Client) Do(request *http.Request, options PaymentOptions) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("MPP resource request is required")
	}
	initial := request.Clone(request.Context())
	response, err := c.resource.Do(singleAttempt(initial))
	if err != nil || response.StatusCode != http.StatusPaymentRequired {
		return response, err
	}
	// Closing without draining avoids waiting for an unbounded challenge body.
	response.Body.Close()
	// MPP needs Authorization for its payment credential. Replacing an application's
	// existing authentication, as the upstream transport does, would remove credentials
	// the service may require to identify or authorize the caller.
	if request.URL.User != nil {
		return nil, ErrAuthorizationConflict
	}
	for name, values := range request.Header {
		if strings.EqualFold(name, "Authorization") {
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					return nil, ErrAuthorizationConflict
				}
			}
		}
	}
	challenges, err := mpp.ParseChallenges(response.Header.Values("WWW-Authenticate"))
	if err != nil {
		return nil, err
	}
	var selected *mpp.Challenge
	for _, challenge := range challenges {
		err := validate(challenge, options)
		var paymentError *Error
		if errors.As(err, &paymentError) && paymentError.Code == Unsupported {
			continue
		}
		if err != nil {
			return nil, err
		}
		selected = &challenge
		break
	}
	if selected == nil {
		return nil, &Error{Code: Unsupported}
	}
	if selected.Expires != nil && *selected.Expires != "" {
		expires, err := time.Parse(time.RFC3339Nano, *selected.Expires)
		if err != nil {
			return nil, &mpp.CodecError{Artifact: "challenge", Reason: "invalid expiration timestamp"}
		}
		if time.Now().After(expires) {
			return nil, &Error{Code: Expired}
		}
	}
	// Prove the request can be replayed before obtaining a credential; payment must
	// not start only to fail locally because its request body has already been consumed.
	retry := request.Clone(request.Context())
	closeReplay := false
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
		closeReplay = true
		defer func() {
			if closeReplay {
				retry.Body.Close()
			}
		}()
	}
	payment, err := c.Prepare(request.Context(), *selected, options)
	if err != nil {
		return nil, err
	}
	encoded, err := payment.wait(request.Context())
	if err != nil {
		return nil, err
	}
	if retry.Header == nil {
		retry.Header = make(http.Header)
	}
	for name := range retry.Header {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Payment-Signature") ||
			strings.EqualFold(name, "Payment-Required") || strings.EqualFold(name, "Payment-Response") {
			delete(retry.Header, name)
		}
	}
	retry.Header.Set("Authorization", "Payment "+encoded)
	closeReplay = false
	return c.resource.Do(singleAttempt(retry))
}

func singleAttempt(request *http.Request) *http.Request {
	// net/http may replay requests on a reused connection. A paid GET is not a
	// safe ordinary read: leave replay decisions with the owning payment flow.
	request.GetBody = nil
	if request.Body == nil || request.Body == http.NoBody {
		request.Body = io.NopCloser(strings.NewReader(""))
	}
	return request
}
