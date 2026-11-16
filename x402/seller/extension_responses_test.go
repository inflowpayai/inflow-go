package seller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestFacilitatorExtensionResponses(t *testing.T) {
	metadata := map[string]any{"bazaar": map[string]any{"status": "accepted", "private": "seller-only"}}
	encoded := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	for _, header := range []struct {
		name, value string
		want        map[string]any
	}{
		{"present", encoded(`{"bazaar":{"status":"accepted","private":"seller-only"}}`), metadata},
		{"absent", "", nil},
		{"bad-base64", "%", nil},
		{"bad-json", encoded("{"), nil},
		{"array", encoded("[]"), nil},
		{"null", encoded("null"), nil},
		{"string", encoded(`"text"`), nil},
	} {
		for _, operation := range []string{"verify", "settle", "permit2"} {
			t.Run(header.name+"/"+operation, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("EXTENSION-RESPONSES", header.value)
					switch operation {
					case "settle":
						if r.URL.Path != "/v1/x402/settle" {
							t.Error(r.URL.Path)
						}
						w.Write([]byte(`{"success":true,"transaction":"tx","network":"eip155:8453","extensions":{"public":"receipt"},"extensionResponses":{"forged":true}}`))
					case "permit2":
						w.WriteHeader(http.StatusPreconditionFailed)
						w.Write([]byte(`{"isValid":false,"invalidReason":"permit2_allowance_required","extensionResponses":{"forged":true}}`))
					default:
						w.Write([]byte(`{"isValid":true,"extensionResponses":{"forged":true}}`))
					}
				}))
				defer server.Close()
				client, err := NewAnonymousFacilitator(inflow.Options{BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				var serialized []byte
				if operation == "settle" {
					result, err := client.Settle(context.Background(), validPayload, validRequirements)
					if err != nil {
						t.Fatal(err)
					}
					if !result.Success || result.Transaction != "tx" || result.Extensions["public"] != "receipt" {
						t.Fatalf("payment result changed: %+v", result)
					}
					got = result.ExtensionResponses
					serialized, err = json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					result, err := client.Verify(context.Background(), validPayload, validRequirements)
					if err != nil {
						t.Fatal(err)
					}
					if result.IsValid != (operation == "verify") {
						t.Fatalf("verification changed: %+v", result)
					}
					if operation == "permit2" && result.InvalidReason != "permit2_allowance_required" {
						t.Fatal(result.InvalidReason)
					}
					got = result.ExtensionResponses
					serialized, err = json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, header.want) {
					t.Fatalf("metadata = %#v, want %#v", got, header.want)
				}
				if strings.Contains(string(serialized), "extensionResponses") || strings.Contains(string(serialized), "seller-only") {
					t.Fatalf("metadata leaked into receipt: %s", serialized)
				}
				if calls != 1 {
					t.Fatalf("unexpected retry: %d requests", calls)
				}
			})
		}
	}
}
