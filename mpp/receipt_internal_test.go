package mpp

import (
	"encoding/json"
	"testing"
)

func TestReceiptWireRejectsReservedExtensions(t *testing.T) {
	for _, key := range receiptKeys {
		wire := Receipt{Method: "inflow", Reference: "settled", Status: "success", Timestamp: "2026-01-01T00:00:00Z", Extensions: map[string]json.RawMessage{key: json.RawMessage(`"override"`)}}
		if _, err := json.Marshal(wire); err == nil {
			t.Fatalf("extension replaced %s", key)
		}
	}
}
