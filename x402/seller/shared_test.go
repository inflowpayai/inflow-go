package seller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
)

func sameJSON(t *testing.T, actual, expected []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(actual, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("got %s\nwant %s", actual, expected)
	}
}

func encode(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSharedSellerCases(t *testing.T) {
	data, err := os.ReadFile("testdata/seller.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Operation string
		Input         struct {
			APIKey            string          `json:"api_key"`
			Payload           json.RawMessage `json:"payment_payload"`
			Requirements      json.RawMessage `json:"payment_requirements"`
			Config, Supported json.RawMessage
			Options           struct {
				Price               json.RawMessage
				MaxTimeoutSeconds   int
				Schemes, Networks   []string
				AssetTransferMethod string
			}
		}
		Expect struct {
			Result json.RawMessage
			Error  *struct {
				Code       string
				HTTPStatus int `json:"http_status"`
				Details    struct{ Body any }
			}
		}
		Platform struct {
			Exchanges []struct {
				Request struct {
					Method, Path string
					Headers      map[string]string
					JSON         json.RawMessage
				}
				Response struct {
					Status  int
					Headers map[string]string
					JSON    json.RawMessage
				}
			}
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 39 {
		t.Fatalf("unexpected case count %d", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			before := encode(t, tc.Input)
			var mu sync.Mutex
			index := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if len(tc.Input.Config) > 0 {
					switch r.URL.Path {
					case "/v1/x402/config":
						w.Write(tc.Input.Config)
					case "/v1/x402/supported":
						w.Write(tc.Input.Supported)
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(500)
					}
					return
				}
				if index >= len(tc.Platform.Exchanges) {
					t.Error("extra request")
					w.WriteHeader(500)
					return
				}
				e := tc.Platform.Exchanges[index]
				index++
				if r.Method != e.Request.Method || r.URL.Path != e.Request.Path {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				for key, value := range e.Request.Headers {
					if r.Header.Get(key) != value {
						t.Errorf("header %s", key)
					}
				}
				body, _ := io.ReadAll(r.Body)
				sameJSON(t, body, e.Request.JSON)
				for key, value := range e.Response.Headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(e.Response.Status)
				w.Write(e.Response.JSON)
			}))
			defer server.Close()
			ctx := context.Background()
			options := inflow.Options{BaseURL: server.URL, APIKey: tc.Input.APIKey}
			var result any
			var operationError error
			if strings.HasSuffix(tc.Operation, "offers") || strings.HasSuffix(tc.Operation, "route") {
				options.APIKey = "test-only-seller-key"
				client, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				price := PriceSpec{}
				if json.Unmarshal(tc.Input.Options.Price, &price.Amount) != nil {
					if err := json.Unmarshal(tc.Input.Options.Price, &price); err != nil {
						t.Fatal(err)
					}
				}
				accepts := AcceptsOptions{Price: price, MaxTimeoutSeconds: tc.Input.Options.MaxTimeoutSeconds, Schemes: tc.Input.Options.Schemes, Networks: tc.Input.Options.Networks}
				if strings.HasSuffix(tc.Operation, "offers") {
					result, operationError = client.Accepts(ctx, accepts)
				} else {
					result, operationError = client.Route(ctx, RouteOptions{AcceptsOptions: accepts, AssetTransferMethod: tc.Input.Options.AssetTransferMethod})
				}
			} else {
				var f *Facilitator
				if options.APIKey == "" {
					f, err = NewAnonymousFacilitator(options)
				} else {
					f, err = NewFacilitator(options)
				}
				if err != nil {
					t.Fatal(err)
				}
				switch tc.Operation {
				case "x402.seller.verify":
					result, operationError = f.Verify(ctx, tc.Input.Payload, tc.Input.Requirements)
				case "x402.seller.settle":
					result, operationError = f.Settle(ctx, tc.Input.Payload, tc.Input.Requirements)
				case "x402.seller.verify-settle":
					verification, e := f.Verify(ctx, tc.Input.Payload, tc.Input.Requirements)
					operationError = e
					combined := map[string]any{"verification": verification}
					if e == nil && verification.IsValid {
						combined["settlement"], operationError = f.Settle(ctx, tc.Input.Payload, tc.Input.Requirements)
					}
					result = combined
				default:
					t.Fatalf("unsupported operation %s", tc.Operation)
				}
			}
			if tc.Expect.Error == nil {
				if operationError != nil {
					t.Fatal(operationError)
				}
				expected := tc.Expect.Result
				if tc.Operation == "x402.seller.settle" {
					// The upstream Go settlement type serializes absent network as an empty string.
					var settlement map[string]json.RawMessage
					if err := json.Unmarshal(expected, &settlement); err != nil {
						t.Fatal(err)
					}
					if _, present := settlement["network"]; !present {
						settlement["network"] = json.RawMessage(`""`)
					}
					expected = encode(t, settlement)
				}
				sameJSON(t, encode(t, result), expected)
			} else {
				var priceError *PriceError
				var apiError *inflow.APIError
				switch {
				case errors.As(operationError, &priceError):
					if tc.Expect.Error.Code != "invalid-input" {
						t.Fatal(operationError)
					}
				case errors.As(operationError, &apiError):
					if tc.Expect.Error.Code != "api-error" || apiError.HTTPStatus != tc.Expect.Error.HTTPStatus || !reflect.DeepEqual(apiError.Body, tc.Expect.Error.Details.Body) {
						t.Fatalf("unexpected API error %+v", apiError)
					}
				default:
					t.Fatalf("unexpected error %v", operationError)
				}
			}
			sameJSON(t, encode(t, tc.Input), before)
			mu.Lock()
			defer mu.Unlock()
			if index != len(tc.Platform.Exchanges) {
				t.Errorf("used %d/%d exchanges", index, len(tc.Platform.Exchanges))
			}
		})
	}
}
