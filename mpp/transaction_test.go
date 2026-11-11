package mpp_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func TestReceiptTransactionID(t *testing.T) {
	const id = "ABCDEFAB-1234-5678-90AB-ABCDEF123456"
	for _, value := range []string{`"` + id + `"`, `null`, `42`, `true`, `{}`, `[]`, `""`, `"not-a-uuid"`, `"` + strings.Repeat("a", 36) + `"`, `"` + id + `\n"`} {
		t.Run(value, func(t *testing.T) {
			raw := `{"method":"inflow","reference":"settlement-reference","status":"success","timestamp":"2026-09-01T12:00:00Z","inflowTransactionId":` + value + `,"other":{"number":9007199254740993}}`
			r, err := mpp.DecodeReceipt(encoded(raw))
			if err != nil {
				t.Fatal(err)
			}
			valid := value == `"`+id+`"`
			if valid && (r.InflowTransactionID == nil || *r.InflowTransactionID != id) || !valid && r.InflowTransactionID != nil {
				t.Fatalf("unexpected identifier: %v", r.InflowTransactionID)
			}
			if _, exists := r.Extensions["inflowTransactionId"]; exists {
				t.Fatal("correlation field leaked into untyped extensions")
			}
			wire, err := mpp.EncodeReceipt(r)
			if err != nil {
				t.Fatal(err)
			}
			again, err := mpp.DecodeReceipt(wire)
			if err != nil || !reflect.DeepEqual(r, again) || r.Reference != "settlement-reference" {
				t.Fatalf("receipt changed: %#v %v", again, err)
			}
			upstream, err := foundation.ParseReceipt(wire)
			if err != nil || upstream.Reference != r.Reference {
				t.Fatalf("upstream rejected receipt: %#v %v", upstream, err)
			}
		})
	}
}

func TestReceiptTransactionIDEncoding(t *testing.T) {
	for _, id := range []*string{nil, ptr(""), ptr("not-a-uuid"), ptr(strings.Repeat("a", 36))} {
		r := receipt()
		r.InflowTransactionID = id
		wire, err := mpp.EncodeReceipt(r)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := mpp.DecodeReceipt(wire)
		if err != nil || decoded.InflowTransactionID != nil || decoded.Reference != r.Reference {
			t.Fatalf("invalid correlation affected receipt: %#v %v", decoded, err)
		}
		if r.InflowTransactionID != id {
			t.Fatal("encoding mutated caller's receipt")
		}
	}
	r := receipt()
	r.InflowTransactionID = ptr("22222222-2222-4222-8222-222222222222")
	r.Extensions = map[string]json.RawMessage{"inflowTransactionId": json.RawMessage(`"33333333-3333-4333-8333-333333333333"`)}
	if _, err := mpp.EncodeReceipt(r); err == nil {
		t.Fatal("extension overwrote typed identifier")
	}
	if err := json.Unmarshal([]byte(`{"method":"inflow"}`), &r); err != nil || r.InflowTransactionID != nil {
		t.Fatalf("reused receipt retained identifier: %#v %v", r, err)
	}
}
