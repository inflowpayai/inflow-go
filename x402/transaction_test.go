package x402_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/x402"
)

func TestInflowTransactionID(t *testing.T) {
	const id = "ABCDEFAB-1234-5678-90AB-ABCDEF123456"
	if got := x402.InflowTransactionID(nil); got != "" {
		t.Fatal(got)
	}
	for _, extension := range []string{`null`, `false`, `42`, `"text"`, `[]`, `{}`, `{"info":null}`, `{"info":[]}`, `{"info":{}}`, `{"info":{"transactionId":123}}`, `{"info":{"transactionId":""}}`, `{"info":{"transactionId":"not-a-uuid"}}`, `{"info":{"transactionId":"` + strings.Repeat("g", 36) + `"}}`, `{"info":{"transactionId":"` + id + `\n"}}`, `{"info":{"transactionId":"` + id + `"},"schema":{"type":"object"}}`} {
		for _, success := range []bool{false, true} {
			t.Run(extension, func(t *testing.T) {
				var value any
				if err := json.Unmarshal([]byte(extension), &value); err != nil {
					t.Fatal(err)
				}
				result := x402.SettleResponse{Success: success, Transaction: "chain-or-processor-reference", Extensions: map[string]any{x402.InflowTransaction: value, "other": map[string]any{"keep": true}}}
				before, _ := json.Marshal(result)
				want := ""
				if success && strings.Contains(extension, `"`+id+`"`) {
					want = id
				}
				if got := x402.InflowTransactionID(&result); got != want {
					t.Fatalf("got %q, want %q", got, want)
				}
				after, _ := json.Marshal(result)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("accessor mutated settlement result")
				}
			})
		}
	}
	if got := x402.InflowTransactionID(&x402.SettleResponse{Success: true, Transaction: id}); got != "" {
		t.Fatal("inferred transaction ID from settlement reference")
	}
	result := x402.SettleResponse{Success: true, Extensions: map[string]any{x402.InflowTransaction: map[string]any{"info": map[string]any{"transactionId": id}}}}
	if got := x402.InflowTransactionID(&result); got != id {
		t.Fatal("accessor required a schema to read the identifier", got)
	}
}
