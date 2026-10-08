package seller

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/inflowpayai/inflow-go/mpp"
)

// CardOffer accepts decimal USD, for example "1.25". Merchant and encryption
// settings come from the Seller's InFlow configuration, not the route.
type CardOffer struct {
	Amount          string  `json:"amount"`
	Description     *string `json:"description,omitempty"`
	ExternalID      *string `json:"externalId,omitempty"`
	BillingRequired *bool   `json:"billingRequired,omitempty"`
}

func (c *Client) prepareCard(ctx context.Context, offer CardOffer) (PreparedOffer, error) {
	config, err := c.config(ctx)
	if err != nil {
		return PreparedOffer{}, err
	}
	request := mpp.CardRequest{Amount: "100", Currency: "usd"}
	for _, method := range config.SupportedMethods {
		if method.ID != mpp.MethodCard {
			continue
		}
		if slices.Contains(method.SupportedCurrencies, "USD") && slices.Contains(method.SupportedIntents, mpp.IntentCharge) {
			request.Recipient = method.MethodDetails.Recipient
			request.MethodDetails = method.MethodDetails.CardMethodDetails
		}
		break
	}
	if err := request.Validate(); err != nil {
		return PreparedOffer{}, &Error{Code: "unsupported-capability"}
	}
	if !stripeAmount.MatchString(offer.Amount) {
		return PreparedOffer{}, &Error{Code: "invalid-input"}
	}
	whole, fraction, _ := strings.Cut(offer.Amount, ".")
	cents, err := strconv.ParseUint(whole+fraction+strings.Repeat("0", 2-len(fraction)), 10, 64)
	if err != nil || cents < 50 || cents > 99999999 {
		return PreparedOffer{}, &Error{Code: "invalid-input"}
	}
	request.Amount = strconv.FormatUint(cents, 10)
	request.ExternalID = offer.ExternalID
	request.Description = offer.Description
	if offer.BillingRequired != nil {
		request.MethodDetails.BillingRequired = offer.BillingRequired
	}
	if err := request.Validate(); err != nil {
		return PreparedOffer{}, &Error{Code: "invalid-input"}
	}
	encoded, err := mpp.Encode(request)
	return PreparedOffer{Method: mpp.MethodCard, Intent: mpp.IntentCharge, Request: encoded}, err
}
