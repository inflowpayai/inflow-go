package mppwire

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func TestChallengeBridgePreservesEncodedBindings(t *testing.T) {
	opaque := base64.RawURLEncoding.EncodeToString([]byte(`{ "route": "/paid" }`))
	description := "A report, with details"
	value := mpp.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "eyJhbW91bnQiOiIxMDAifQ", Opaque: &opaque, Description: &description}
	native, err := ToChallenge(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromChallenge(native)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("challenge round trip: %#v, %v", got, err)
	}
}

func TestCredentialBridgePreservesProofNumbersAndNulls(t *testing.T) {
	source := "did:key:buyer"
	value := mpp.Credential{Challenge: mpp.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30"}, Source: &source, Payload: map[string]any{"proof": json.Number("9007199254740993"), "optional": nil}}
	native, err := ToCredential(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromCredential(native)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("credential round trip: %#v, %v", got, err)
	}
}

func TestBridgeRejectsUnsupportedAndInvalidValues(t *testing.T) {
	if _, err := FromCredential(foundation.Credential{Challenge: foundation.ChallengeEcho{Header: "Payment-Authorization"}}); err == nil {
		t.Fatal("unsupported credential header accepted")
	}
	if _, err := FromCredential(foundation.Credential{}); err == nil {
		t.Fatal("invalid credential accepted")
	}
	if _, err := FromChallenge(foundation.Challenge{}); err == nil {
		t.Fatal("invalid challenge accepted")
	}
	if _, err := FromChallenge(foundation.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", RequestB64: "e30", Header: "Payment-Authorization"}); err == nil {
		t.Fatal("unsupported challenge header accepted")
	}
	if _, err := ToChallenge(mpp.Challenge{}); err == nil {
		t.Fatal("invalid wire challenge accepted")
	}
	if _, err := ToChallenge(mpp.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "not-json"}); err == nil {
		t.Fatal("invalid request accepted")
	}
	opaque := "not-json"
	if _, err := ToChallenge(mpp.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30", Opaque: &opaque}); err == nil {
		t.Fatal("invalid opaque accepted")
	}
	if _, err := ToCredential(mpp.Credential{}); err == nil {
		t.Fatal("invalid wire credential accepted")
	}
	if _, err := ToCredential(mpp.Credential{Challenge: mpp.Challenge{ID: "id", Realm: "seller.example", Method: "INVALID METHOD", Intent: "charge", Request: "e30"}, Payload: map[string]any{}}); err == nil {
		t.Fatal("invalid credential method accepted")
	}
}

func TestReceiptBridge(t *testing.T) {
	if _, err := ToReceipt(mpp.Receipt{}); err == nil {
		t.Fatal("invalid receipt accepted")
	}
	for _, externalID := range []*string{nil, new(""), new("order-123")} {
		value := mpp.Receipt{Method: "stripe", Reference: "pi_test", Status: "success", Timestamp: "2026-01-01T00:00:00.123456789Z", ExternalID: externalID, Extensions: map[string]json.RawMessage{"proof": json.RawMessage(`9007199254740993`)}}
		native, err := ToReceipt(value)
		if err != nil || native.Extensions["proof"] != json.Number("9007199254740993") || native.Timestamp.Nanosecond() != 123456789 {
			t.Fatalf("receipt conversion: %#v %v", native, err)
		}
		header, err := foundation.FormatReceiptWithError(native)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := mpp.DecodeReceipt(header)
		if err != nil {
			t.Fatal(err)
		}
		if externalID == nil || *externalID == "" {
			if decoded.ExternalID != nil {
				t.Fatal("upstream presence semantics changed; reassess adapter limitation")
			}
		} else if decoded.ExternalID == nil || *decoded.ExternalID != *externalID {
			t.Fatal("nonempty externalId lost")
		}
		if value.ExternalID != externalID {
			t.Fatal("conversion mutated caller")
		}
	}
}
