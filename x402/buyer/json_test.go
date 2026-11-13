package buyer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
)

const numericMap = `{"large":9007199254740993,"nested":[{"decimal":0.1234567890123456789,"exponent":1e30}],"null":null}`
const numericRequirement = `{"scheme":"exact","network":"eip155:8453","amount":"9007199254740993","extra":` + numericMap + `}`
const numericPayload = `{"x402Version":2,"accepted":` + numericRequirement + `,"payload":` + numericMap + `,"extensions":` + numericMap + `}`

func assertNumbers(t *testing.T, value map[string]any) {
	t.Helper()
	want := map[string]any{
		"large": json.Number("9007199254740993"),
		"nested": []any{map[string]any{
			"decimal":  json.Number("0.1234567890123456789"),
			"exponent": json.Number("1e30"),
		}},
		"null": nil,
	}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("numeric values changed: %#v", value)
	}
}

func TestDecodePreservesUpstreamNumericMaps(t *testing.T) {
	for _, input := range []string{numericPayload, `{"paymentPayload":` + numericPayload + `,"encodedPayload":"original"}`} {
		var payload x402.PaymentPayload
		if strings.HasPrefix(input, `{"paymentPayload"`) {
			response, err := decode[payloadResponse]([]byte(input))
			if err != nil || response.PaymentPayload == nil || *response.EncodedPayload != "original" {
				t.Fatalf("response: %+v, %v", response, err)
			}
			payload = *response.PaymentPayload
			copy, err := snapshot(EncodedPayment{EncodedPayload: "original", PaymentPayload: payload})
			if err != nil || copy.EncodedPayload != "original" {
				t.Fatalf("result copy: %+v, %v", copy, err)
			}
			assertNumbers(t, copy.PaymentPayload.Payload)
			assertNumbers(t, copy.PaymentPayload.Accepted.Extra)
			assertNumbers(t, copy.PaymentPayload.Extensions)
			copy.PaymentPayload.Payload["large"] = "mutated"
		} else {
			var err error
			payload, err = decode[x402.PaymentPayload]([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
		}
		assertNumbers(t, payload.Payload)
		assertNumbers(t, payload.Accepted.Extra)
		assertNumbers(t, payload.Extensions)
		if payload.Accepted.Amount != "9007199254740993" {
			t.Fatal("string amount changed")
		}
	}

	required, err := decode[x402.PaymentRequired]([]byte(`{"x402Version":2,"accepts":[` + numericRequirement + `],"extensions":` + numericMap + `}`))
	if err != nil {
		t.Fatal(err)
	}
	assertNumbers(t, required.Extensions)
	assertNumbers(t, required.Accepts[0].Extra)
	selected, err := snapshot(required.Accepts[0])
	if err != nil {
		t.Fatal(err)
	}
	assertNumbers(t, selected.Extra)
	candidates, err := snapshot(required.Accepts)
	if err != nil {
		t.Fatal(err)
	}
	assertNumbers(t, candidates[0].Extra)
	candidates[0].Extra["large"] = "mutated"
	assertNumbers(t, required.Accepts[0].Extra)
}

func TestDecodeRetainsUpstreamValidation(t *testing.T) {
	invalid := strings.ReplaceAll(numericRequirement, "eip155:8453", "invalid")
	if _, err := decode[x402.PaymentRequirements]([]byte(invalid)); err == nil {
		t.Fatal("invalid requirement network accepted")
	}
	if _, err := decode[[]x402.PaymentRequirements]([]byte(`[` + invalid + `]`)); err == nil {
		t.Fatal("invalid candidate network accepted")
	}
	if _, err := decode[x402.PaymentRequired]([]byte(`{"x402Version":2,"accepts":[` + invalid + `]}`)); err == nil {
		t.Fatal("invalid challenge network accepted")
	}
	if _, err := decode[x402.PaymentPayload]([]byte(strings.ReplaceAll(numericPayload, "eip155:8453", "invalid"))); err == nil {
		t.Fatal("invalid payload network accepted")
	}
	if _, err := decode[x402.PaymentPayload]([]byte(numericPayload + `{}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	for _, input := range []string{`{}`, `{"paymentPayload":null}`} {
		value, err := decode[payloadResponse]([]byte(input))
		if err != nil || value.PaymentPayload != nil {
			t.Fatalf("pending response: %+v, %v", value, err)
		}
	}
	if _, err := decode[EncodedPayment]([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := decode[EncodedPayment]([]byte(`{"paymentPayload":null}`)); err == nil {
		t.Fatal("upstream rejection of a null value payload was bypassed")
	}
}

func TestHTTPPaymentPreservesNumbersThroughHooksAndPolicies(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transactions/x402-supported":
			fmt.Fprint(w, `{"kinds":[{"scheme":"exact","network":"eip155:8453","x402Version":2}]}`)
		case "/v1/transactions/x402":
			var request struct {
				Accept json.RawMessage `json:"accept"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if !strings.Contains(string(request.Accept), `"large":9007199254740993`) {
				t.Errorf("approval metadata changed: %s", request.Accept)
			}
			fmt.Fprint(w, `{"transactionId":"test","approvalId":"test","approvalStatus":"APPROVED"}`)
		case "/v1/transactions/test/x402":
			fmt.Fprint(w, `{"status":"PENDING","encodedPayload":"original-payment","paymentPayload":`+numericPayload+`}`)
		default:
			t.Errorf("unexpected platform request: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer platform.Close()
	before, after, policies := 0, 0, 0
	c, err := New(Options{
		Options: inflow.Options{BaseURL: platform.URL},
		Policies: []Policy{func(_ context.Context, values []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
			policies++
			assertNumbers(t, values[0].Extra)
			return values, nil
		}},
		Hooks: []Hooks{{
			Before: func(_ context.Context, required x402.PaymentRequired) error {
				before++
				assertNumbers(t, required.Extensions)
				assertNumbers(t, required.Accepts[0].Extra)
				required.Accepts[0].Extra["large"] = "hook mutation"
				return nil
			},
			After: func(_ context.Context, _ x402.PaymentRequired, payload x402.PaymentPayload) error {
				after++
				assertNumbers(t, payload.Payload)
				assertNumbers(t, payload.Accepted.Extra)
				assertNumbers(t, payload.Extensions)
				payload.Payload["large"] = "hook mutation"
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(x402.HeaderPaymentSignature) == "original-payment" {
			w.WriteHeader(200)
			return
		}
		required := `{"x402Version":2,"resource":{"url":"https://example.com/data"},"accepts":[` + numericRequirement + `],"extensions":` + numericMap + `}`
		w.Header().Set(x402.HeaderPaymentRequired, base64.StdEncoding.EncodeToString([]byte(required)))
		w.WriteHeader(402)
	}))
	defer merchant.Close()
	request, err := http.NewRequest("GET", merchant.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Do(request, SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || before != 1 || after != 1 || policies != 1 {
		t.Fatalf("payment flow: status=%d before=%d after=%d policies=%d", response.StatusCode, before, after, policies)
	}
}

func TestNumberFieldDecodingRejectsMalformedData(t *testing.T) {
	for _, target := range []any{
		new(x402.PaymentPayload), new(x402.PaymentRequired), new(x402.PaymentRequirements),
		new([]x402.PaymentRequirements), new(EncodedPayment),
		&payloadResponse{PaymentPayload: new(x402.PaymentPayload)},
	} {
		if err := preserveNumbers([]byte(`{`), target); err == nil {
			t.Fatalf("malformed data accepted for %T", target)
		}
	}
}
