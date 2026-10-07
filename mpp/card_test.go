package mpp_test

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
)

func cardRequest() mpp.CardRequest {
	return mpp.CardRequest{Amount: "125", Currency: "usd", Recipient: "acct_test", MethodDetails: mpp.CardMethodDetails{AcceptedNetworks: []string{"visa"}, MerchantName: "Test merchant", EncryptionJWK: mpp.CardEncryptionKey{Kty: "RSA", Alg: "RSA-OAEP-256", Use: "enc", Kid: "test", N: "test_only", E: "AQAB"}}}
}

func TestCardRequest(t *testing.T) {
	for name, change := range map[string]func(*mpp.CardRequest){
		"amount": func(r *mpp.CardRequest) { r.Amount = "49" }, "exponent": func(r *mpp.CardRequest) { r.Amount = "1e2" },
		"maximum": func(r *mpp.CardRequest) { r.Amount = "100000000" }, "currency": func(r *mpp.CardRequest) { r.Currency = "USD" },
		"recipient": func(r *mpp.CardRequest) { r.Recipient = "" }, "reference": func(r *mpp.CardRequest) { r.ExternalID = new(strings.Repeat("x", 256)) },
		"name": func(r *mpp.CardRequest) { r.MethodDetails.MerchantName = "" }, "networks": func(r *mpp.CardRequest) { r.MethodDetails.AcceptedNetworks = nil },
		"network": func(r *mpp.CardRequest) { r.MethodDetails.AcceptedNetworks = []string{"mastercard"} },
		"key":     func(r *mpp.CardRequest) { r.MethodDetails.EncryptionJWK.Alg = "RSA-OAEP" }, "n": func(r *mpp.CardRequest) { r.MethodDetails.EncryptionJWK.N = "=" },
		"e": func(r *mpp.CardRequest) { r.MethodDetails.EncryptionJWK.E = "" }, "kid": func(r *mpp.CardRequest) { r.MethodDetails.EncryptionJWK.Kid = "" },
	} {
		t.Run(name, func(t *testing.T) {
			request := cardRequest()
			change(&request)
			if request.Validate() == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
	request := cardRequest()
	request.ExternalID = new("")
	request.Description = new("A report")
	request.MethodDetails.BillingRequired = new(false)
	encoded, err := mpp.Encode(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := mpp.DecodeCardRequest(encoded)
	if err != nil || !reflect.DeepEqual(request, decoded) {
		t.Fatal(decoded, err)
	}
	raw, _ := json.Marshal(request)
	for _, data := range []string{"null", "[]", `{"amount":1}`, strings.Replace(string(raw), `"externalId":""`, `"externalId":null`, 1), strings.Replace(string(raw), `"billingRequired":false`, `"billingRequired":null`, 1)} {
		if _, err := mpp.DecodeCardRequest(base64.RawURLEncoding.EncodeToString([]byte(data))); err == nil {
			t.Fatal("accepted", data)
		}
	}
	if _, err := mpp.DecodeCardRequest("!"); err == nil {
		t.Fatal("accepted invalid base64")
	}
}

func TestCardPayload(t *testing.T) {
	base := map[string]any{"encryptedPayload": "opaque-test-value", "network": "visa", "panLastFour": "1234", "panExpirationMonth": "12", "panExpirationYear": "2030"}
	for name, change := range map[string]any{"encryptedPayload": "", "network": "mastercard", "panLastFour": "123", "panExpirationMonth": "13", "panExpirationYear": "30", "billingAddress": nil, "cardholderFullName": false, "paymentAccountReference": 1} {
		t.Run(name, func(t *testing.T) {
			value := map[string]any{}
			for k, v := range base {
				value[k] = v
			}
			value[name] = change
			if mpp.ValidateCardPayload(value) == nil {
				t.Fatal("accepted invalid payload")
			}
		})
	}
	base["billingAddress"] = map[string]any{"zip": 42}
	if mpp.ValidateCardPayload(base) == nil {
		t.Fatal("accepted numeric postal code")
	}
	base["billingAddress"] = map[string]any{"zip": "94102", "countryCode": "US", "extra": nil}
	base["paymentAccountReference"] = "reference"
	base["cardholderFullName"] = "Buyer"
	base["extra"] = json.Number("9007199254740993")
	before, _ := json.Marshal(base)
	if err := mpp.ValidateCardPayload(base); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(base)
	if string(before) != string(after) {
		t.Fatal("mutated payload")
	}
}
