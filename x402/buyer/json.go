package buyer

import (
	"bytes"
	"encoding/json"

	"github.com/inflowpayai/inflow-go/x402"
)

// Upstream's custom unmarshallers ignore the outer decoder's UseNumber option:
// https://github.com/x402-foundation/x402/issues/4258. After upstream validation,
// decode its arbitrary JSON maps again without rounding numeric proof or metadata values.
func preserveNumbers(data []byte, target any) error {
	switch value := target.(type) {
	case *x402.PaymentPayload:
		return payloadNumbers(data, value)
	case *x402.PaymentRequired:
		var fields struct {
			Extensions map[string]any `json:"extensions"`
			Accepts    []struct {
				Extra map[string]any `json:"extra"`
			} `json:"accepts"`
		}
		if err := numberFields(data, &fields); err != nil {
			return err
		}
		value.Extensions = fields.Extensions
		for i := range value.Accepts {
			value.Accepts[i].Extra = fields.Accepts[i].Extra
		}
	case *x402.PaymentRequirements:
		var fields struct {
			Extra map[string]any `json:"extra"`
		}
		if err := numberFields(data, &fields); err != nil {
			return err
		}
		value.Extra = fields.Extra
	case *[]x402.PaymentRequirements:
		var fields []struct {
			Extra map[string]any `json:"extra"`
		}
		if err := numberFields(data, &fields); err != nil {
			return err
		}
		for i := range *value {
			(*value)[i].Extra = fields[i].Extra
		}
	case *EncodedPayment:
		var fields struct {
			PaymentPayload json.RawMessage `json:"paymentPayload"`
		}
		if err := numberFields(data, &fields); err != nil {
			return err
		}
		return payloadNumbers(fields.PaymentPayload, &value.PaymentPayload)
	case *payloadResponse:
		if value.PaymentPayload == nil {
			return nil
		}
		var fields struct {
			PaymentPayload json.RawMessage `json:"paymentPayload"`
		}
		if err := numberFields(data, &fields); err != nil {
			return err
		}
		return payloadNumbers(fields.PaymentPayload, value.PaymentPayload)
	}
	return nil
}

func payloadNumbers(data []byte, value *x402.PaymentPayload) error {
	if len(data) == 0 {
		return nil
	}
	var fields struct {
		Payload    map[string]any `json:"payload"`
		Extensions map[string]any `json:"extensions"`
		Accepted   struct {
			Extra map[string]any `json:"extra"`
		} `json:"accepted"`
	}
	if err := numberFields(data, &fields); err != nil {
		return err
	}
	value.Payload = fields.Payload
	value.Extensions = fields.Extensions
	value.Accepted.Extra = fields.Accepted.Extra
	return nil
}

func numberFields(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}
