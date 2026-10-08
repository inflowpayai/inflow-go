package mpp

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf16"
)

type CardEncryptionKey struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type CardMethodDetails struct {
	AcceptedNetworks []string          `json:"acceptedNetworks"`
	MerchantName     string            `json:"merchantName"`
	EncryptionJWK    CardEncryptionKey `json:"encryptionJwk"`
	BillingRequired  *bool             `json:"billingRequired,omitempty"`
}

// CardRequest is the wire request: Amount is integer USD cents, not dollars.
type CardRequest struct {
	Amount        string            `json:"amount"`
	Currency      string            `json:"currency"`
	Recipient     string            `json:"recipient"`
	Description   *string           `json:"description,omitempty"`
	ExternalID    *string           `json:"externalId,omitempty"`
	MethodDetails CardMethodDetails `json:"methodDetails"`
}

var cardCents = regexp.MustCompile(`^[1-9][0-9]{0,7}$`)
var cardKeyPart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var cardFourDigits = regexp.MustCompile(`^[0-9]{4}$`)
var cardMonth = regexp.MustCompile(`^(0[1-9]|1[0-2])$`)

func (r CardRequest) Validate() error {
	cents, _ := strconv.Atoi(r.Amount)
	length := func(s string) int { return len(utf16.Encode([]rune(s))) }
	key := r.MethodDetails.EncryptionJWK
	if !cardCents.MatchString(r.Amount) || cents < 50 || r.Currency != "usd" || length(r.Recipient) < 1 || length(r.Recipient) > 255 ||
		(r.ExternalID != nil && length(*r.ExternalID) > 255) || length(r.MethodDetails.MerchantName) < 1 || length(r.MethodDetails.MerchantName) > 255 ||
		len(r.MethodDetails.AcceptedNetworks) == 0 || slices.ContainsFunc(r.MethodDetails.AcceptedNetworks, func(s string) bool { return s != "visa" }) ||
		key.Kty != "RSA" || key.Alg != "RSA-OAEP-256" || key.Use != "enc" || key.Kid == "" || !cardKeyPart.MatchString(key.N) || !cardKeyPart.MatchString(key.E) {
		return invalid("CARD request", "invalid USD/Visa payment terms")
	}
	return nil
}

// ValidateCardPayload checks the envelope without decrypting its contents or removing extensions.
func ValidateCardPayload(payload map[string]any) error {
	text := func(key string) string { value, _ := payload[key].(string); return value }
	if value := text("encryptedPayload"); value == "" || len(utf16.Encode([]rune(value))) > 16384 || text("network") != "visa" ||
		!cardFourDigits.MatchString(text("panLastFour")) || !cardMonth.MatchString(text("panExpirationMonth")) || !cardFourDigits.MatchString(text("panExpirationYear")) {
		return invalid("CARD payload", "invalid encrypted payment envelope")
	}
	for _, key := range []string{"cardholderFullName", "paymentAccountReference"} {
		if value, ok := payload[key]; ok {
			if _, ok := value.(string); !ok {
				return invalid("CARD payload", "invalid optional field")
			}
		}
	}
	if value, ok := payload["billingAddress"]; ok {
		address, ok := value.(map[string]any)
		if !ok {
			return invalid("CARD payload", "invalid billing address")
		}
		for _, key := range []string{"line1", "line2", "city", "state", "zip", "countryCode"} {
			if value, ok := address[key]; ok {
				if _, ok := value.(string); !ok {
					return invalid("CARD payload", "invalid billing field")
				}
			}
		}
	}
	return nil
}

func DecodeCardRequest(encoded string) (CardRequest, error) {
	value, err := Decode(encoded)
	if err != nil {
		return CardRequest{}, err
	}
	raw, _ := json.Marshal(value)
	var request CardRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return CardRequest{}, invalid("CARD request", "invalid field type")
	}
	if err := request.Validate(); err != nil {
		return CardRequest{}, err
	}
	object := value.(map[string]any)
	for _, key := range []string{"description", "externalId"} {
		if value, exists := object[key]; exists && value == nil {
			return CardRequest{}, invalid("CARD request", "optional fields cannot be null")
		}
	}
	details := object["methodDetails"].(map[string]any)
	if value, exists := details["billingRequired"]; exists && value == nil {
		return CardRequest{}, invalid("CARD request", "billingRequired cannot be null")
	}
	return request, nil
}
