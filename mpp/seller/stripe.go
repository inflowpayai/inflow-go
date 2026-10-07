package seller

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/inflowpayai/inflow-go/mpp"
)

// StripeOffer accepts decimal USD, for example "1.25", not integer cents.
// The Seller's InFlow configuration supplies the Stripe profile and payment methods.
type StripeOffer struct {
	Amount      string            `json:"amount"`
	Description *string           `json:"description,omitempty"`
	Recipient   *string           `json:"recipient,omitempty"`
	ExternalID  *string           `json:"externalId,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

var stripeAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,2})?$`)

func (c *Client) prepareStripe(ctx context.Context, offer StripeOffer) (PreparedOffer, error) {
	config, err := c.config(ctx)
	if err != nil {
		return PreparedOffer{}, err
	}
	var networkID string
	var methods []string
	for _, method := range config.SupportedMethods {
		if method.ID != mpp.MethodStripe {
			continue
		}
		if slices.Contains(method.SupportedCurrencies, "USD") && slices.Contains(method.SupportedIntents, mpp.IntentCharge) {
			networkID = method.MethodDetails.NetworkID
			methods = method.MethodDetails.PaymentMethodTypes
		}
		break
	}
	if strings.TrimSpace(networkID) == "" || len(methods) == 0 || slices.ContainsFunc(methods, func(s string) bool { return strings.TrimSpace(s) == "" }) {
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
	// Match Node's UTF-16 string limits, including supplementary Unicode characters.
	length := func(s string) int { return len(utf16.Encode([]rune(s))) }
	if (offer.ExternalID != nil && length(*offer.ExternalID) > 255) || len(offer.Metadata) > 45 {
		return PreparedOffer{}, &Error{Code: "invalid-input"}
	}
	for key, value := range offer.Metadata {
		if strings.TrimSpace(key) == "" || length(key) > 40 || strings.ContainsAny(key, "[]") ||
			slices.Contains([]string{"externalId", "inflowMppTransactionId", "mppChallengeId", "mppIntent", "mppMethod", "stripeNetworkProfile"}, key) || length(value) > 500 {
			return PreparedOffer{}, &Error{Code: "invalid-input"}
		}
	}
	details := map[string]any{"networkId": networkID, "paymentMethodTypes": methods}
	if offer.Metadata != nil {
		details["metadata"] = offer.Metadata
	}
	request := map[string]any{"amount": strconv.FormatUint(cents, 10), "currency": "usd", "methodDetails": details}
	if offer.ExternalID != nil {
		request["externalId"] = *offer.ExternalID
	}
	if offer.Description != nil {
		request["description"] = *offer.Description
	}
	if offer.Recipient != nil {
		request["recipient"] = *offer.Recipient
	}
	encoded, err := mpp.Encode(request)
	return PreparedOffer{Method: mpp.MethodStripe, Intent: mpp.IntentCharge, Request: encoded}, err
}

func stripeCredentialValid(credential mpp.Credential) error {
	if credential.Challenge.Intent != mpp.IntentCharge {
		return &Error{Code: "unsupported-capability"}
	}
	if _, ok := credential.Payload["spt"].(string); !ok {
		return &Error{Code: "invalid-credential"}
	}
	if value, exists := credential.Payload["externalId"]; exists {
		if _, ok := value.(string); !ok {
			return &Error{Code: "invalid-credential"}
		}
	}
	request, err := requestObject(credential.Challenge.Request)
	if err != nil {
		return &Error{Code: "invalid-credential"}
	}
	if reference, exists := request["externalId"]; exists {
		expected, valid := reference.(string)
		actual, supplied := credential.Payload["externalId"].(string)
		if !valid || !supplied || actual != expected {
			return &Error{Code: "invalid-credential"}
		}
	}
	return nil
}

func externalWireCredential(credential mpp.Credential) mpp.Credential {
	// External card payers need no InFlow identity; the platform requires the source field.
	if (credential.Challenge.Method == mpp.MethodStripe || credential.Challenge.Method == mpp.MethodCard) && credential.Source == nil {
		credential.Source = new("")
	}
	return credential
}
