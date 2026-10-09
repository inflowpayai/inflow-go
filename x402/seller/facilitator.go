package seller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
)

type Facilitator struct {
	api       *platform.Client
	supported cache
}

var _ foundation.FacilitatorClient = (*Facilitator)(nil)

func NewFacilitator(options inflow.Options) (*Facilitator, error) {
	if options.APIKey == "" && options.APIKeyProvider == nil {
		return nil, errors.New("authenticated x402 facilitator requires an API key")
	}
	return facilitator(options)
}

// NewAnonymousFacilitator ignores credentials in options and sends anonymous requests.
func NewAnonymousFacilitator(options inflow.Options) (*Facilitator, error) {
	options.APIKey, options.APIKeyProvider, options.AccessToken = "", nil, nil
	return facilitator(options)
}

func facilitator(options inflow.Options) (*Facilitator, error) {
	api, err := platform.New(options)
	if err != nil {
		return nil, err
	}
	return &Facilitator{api: api}, nil
}

func (f *Facilitator) GetSupported(ctx context.Context) (foundation.SupportedResponse, error) {
	return cached[foundation.SupportedResponse](ctx, &f.supported, f.api, "/v1/x402/supported", false)
}

func (f *Facilitator) Verify(ctx context.Context, payload, requirements []byte) (*foundation.VerifyResponse, error) {
	body, err := paymentRequest(payload, requirements)
	if err != nil {
		return nil, err
	}
	response, err := f.api.Do(ctx, platform.Request{Method: "POST", Path: "/v1/x402/verify", Body: body})
	if err != nil {
		var apiError *inflow.APIError
		if errors.As(err, &apiError) && apiError.HTTPStatus == 412 {
			fields, _ := apiError.Body.(map[string]any)
			if fields["isValid"] == false && fields["invalidReason"] == "permit2_allowance_required" {
				body, _ := json.Marshal(fields)
				return decodeResult[foundation.VerifyResponse](&platform.Response{Body: body})
			}
		}
		return nil, err
	}
	return decodeResult[foundation.VerifyResponse](response)
}

func (f *Facilitator) Settle(ctx context.Context, payload, requirements []byte) (*foundation.SettleResponse, error) {
	body, err := paymentRequest(payload, requirements)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		response, err := f.api.Do(ctx, platform.Request{Method: "POST", Path: "/v1/x402/settle", Body: body})
		if err == nil {
			return decodeResult[foundation.SettleResponse](response)
		}
		var apiError *inflow.APIError
		if !errors.As(err, &apiError) || apiError.HTTPStatus != 409 || attempt == 4 {
			return nil, err
		}
		fields, _ := apiError.Body.(map[string]any)
		if fields["errorReason"] != "idempotency_pending" {
			return nil, err
		}
		// Reuse the identical identifier and request. Other errors may follow a
		// completed payment and must not trigger an automatic resubmission.
		if err := platform.Wait(ctx, retryDelay(apiError.Headers.Get("Retry-After"))); err != nil {
			return nil, err
		}
	}
}

func decodeResult[T any](response *platform.Response) (*T, error) {
	value, err := platform.Decode[T](response)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func retryDelay(value string) time.Duration {
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 5 * time.Second
	}
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err != nil || seconds > 5 {
		return 5 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func paymentRequest(payload, requirements []byte) (map[string]any, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return nil, errors.New("invalid x402 payment payload")
	}
	var version int
	if json.Unmarshal(fields["x402Version"], &version) != nil || version != x402.Version {
		return nil, errors.New("x402 Seller supports version 2 payments")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(fields["payload"], &data) != nil || data == nil {
		return nil, errors.New("invalid x402 payment data")
	}
	var required map[string]json.RawMessage
	if json.Unmarshal(requirements, &required) != nil || required == nil {
		return nil, errors.New("invalid x402 payment requirements")
	}
	extensions := map[string]json.RawMessage{}
	if raw := fields["extensions"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &extensions) != nil {
			return nil, errors.New("invalid x402 extensions")
		}
	}
	declaration := x402.ReadPaymentIdentifier(extensions[x402.PaymentIdentifier])
	id := ""
	if declaration != nil {
		_ = json.Unmarshal(declaration.Info["id"], &id)
	}
	if !x402.ValidatePaymentID(id) {
		var compact bytes.Buffer
		_ = json.Compact(&compact, fields["payload"])
		material := "payload:" + compact.String()
		for _, key := range []string{"transactionId", "transaction", "signature"} {
			var value string
			if json.Unmarshal(data[key], &value) == nil && value != "" {
				material = key + ":" + value
				break
			}
		}
		hash := sha256.Sum256([]byte(material))
		id = "pay_" + hex.EncodeToString(hash[:16])
		entry := x402.PaymentIdentifierEntry(x402.DeclarePaymentIdentifier(), id)
		extensions[x402.PaymentIdentifier], _ = json.Marshal(entry)
		fields["extensions"], _ = json.Marshal(extensions)
	}
	return map[string]any{"x402Version": x402.Version, "paymentPayload": fields, "paymentRequirements": required}, nil
}
