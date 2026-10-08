package seller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/inflowpayai/inflow-go/mpp"
)

// Offer selects exactly one typed payment request.
type Offer struct {
	Stripe       *StripeOffer
	Card         *CardOffer
	Charge       *mpp.ChargeRequest
	Subscription *mpp.SubscriptionRequest
	Tempo        *mpp.TempoRequest
}

type PreparedOffer struct {
	Method  string
	Intent  string
	Request string
}

func (c *Client) Prepare(ctx context.Context, offer Offer) (PreparedOffer, error) {
	count := 0
	for _, present := range []bool{offer.Charge != nil, offer.Subscription != nil, offer.Tempo != nil, offer.Stripe != nil, offer.Card != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return PreparedOffer{}, errors.New("MPP offer requires exactly one request")
	}
	if offer.Stripe != nil {
		return c.prepareStripe(ctx, *offer.Stripe)
	}
	if offer.Card != nil {
		return c.prepareCard(ctx, *offer.Card)
	}
	prepared := PreparedOffer{Method: mpp.MethodInflow, Intent: mpp.IntentCharge}
	var request any
	if offer.Tempo != nil {
		value := *offer.Tempo
		if err := value.Validate(); err != nil {
			return PreparedOffer{}, err
		}
		if value.Currency == "" || value.Recipient == "" {
			return PreparedOffer{}, errors.New("Tempo currency and recipient are required")
		}
		if err := c.Load(ctx); err != nil {
			return PreparedOffer{}, err
		}
		details := mpp.TempoMethodDetails{}
		if value.MethodDetails != nil {
			details = *value.MethodDetails
		}
		if details.FeePayer == nil {
			fee := false
			details.FeePayer = &fee
		}
		if details.SupportedModes == nil {
			details.SupportedModes = []string{"pull"}
		}
		value.MethodDetails = &details
		prepared.Method = mpp.MethodTempo
		request = value
	} else {
		var charge mpp.ChargeRequest
		if offer.Charge != nil {
			charge = *offer.Charge
			if err := charge.Validate(); err != nil {
				return PreparedOffer{}, err
			}
		} else {
			if err := offer.Subscription.Validate(); err != nil {
				return PreparedOffer{}, err
			}
			charge = offer.Subscription.ChargeRequest
			prepared.Intent = mpp.IntentSubscription
		}
		config, err := c.config(ctx)
		if err != nil {
			return PreparedOffer{}, err
		}
		details := mpp.InflowMethodDetails{}
		if charge.MethodDetails != nil {
			details = *charge.MethodDetails
		}
		var rails []rail
		for _, method := range config.SupportedMethods {
			if method.ID != mpp.MethodInflow {
				continue
			}
			if len(method.MethodDetails.IntentCurrencyRails) > 0 {
				rails = method.MethodDetails.IntentCurrencyRails[prepared.Intent][charge.Currency]
			} else if legacy, ok := method.MethodDetails.CurrencyRails[charge.Currency]; ok {
				rails = []rail{legacy}
			}
			break
		}
		if len(rails) == 0 {
			return PreparedOffer{}, &Error{Code: "unsupported-capability"}
		}
		if details.Rail == "" && len(rails) != 1 {
			return PreparedOffer{}, &Error{Code: "ambiguous-rail"}
		}
		var selected *rail
		for _, candidate := range rails {
			if details.Rail == "" || details.Rail == candidate.Rail {
				selected = &candidate
				break
			}
		}
		if selected == nil || (selected.Rail != "balance" && selected.Rail != "instrument") {
			return PreparedOffer{}, &Error{Code: "unsupported-capability"}
		}
		if selected.Rail == "instrument" && selected.InstrumentID == "required" && details.InstrumentID == "" {
			return PreparedOffer{}, &Error{Code: "instrument-required"}
		}
		details.Rail = selected.Rail
		charge.MethodDetails = &details
		charge.Recipient = config.SellerID
		if err := charge.Validate(); err != nil {
			return PreparedOffer{}, err
		}
		request = charge
		if offer.Subscription != nil {
			value := *offer.Subscription
			value.ChargeRequest = charge
			request = value
		}
	}
	var err error
	prepared.Request, err = mpp.Encode(request)
	return prepared, err
}

func requestObject(encoded string) (map[string]any, error) {
	value, err := mpp.Decode(encoded)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("MPP request must be an object")
	}
	return object, nil
}

func payloadValid(credential mpp.Credential) error {
	if _, err := mpp.EncodeCredential(credential); err != nil {
		return err
	}
	if credential.Challenge.Method == mpp.MethodTempo {
		raw, _ := json.Marshal(credential.Payload)
		var payload mpp.TempoPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		return payload.Validate()
	}
	if credential.Challenge.Method == mpp.MethodStripe {
		return stripeCredentialValid(credential)
	}
	if credential.Challenge.Method == mpp.MethodCard {
		if credential.Challenge.Intent != mpp.IntentCharge {
			return &Error{Code: "unsupported-capability"}
		}
		if err := mpp.ValidateCardPayload(credential.Payload); err != nil {
			return &Error{Code: "invalid-credential"}
		}
		return nil
	}
	if credential.Challenge.Method != mpp.MethodInflow {
		return &Error{Code: "unsupported-capability"}
	}
	return nil
}
