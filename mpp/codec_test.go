package mpp_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func ptr[T any](value T) *T       { return &value }
func encoded(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
func challenge() mpp.Challenge {
	return mpp.Challenge{ID: "id", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30"}
}
func receipt() mpp.Receipt {
	return mpp.Receipt{Method: "inflow", Reference: "reference", Status: "success", Timestamp: "2026-09-01T12:00:00Z"}
}

func TestSharedCoreCases(t *testing.T) {
	data, err := os.ReadFile("testdata/core.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID        string `json:"id"`
		Operation string `json:"operation"`
		Input     struct {
			Value   json.RawMessage `json:"value"`
			Headers json.RawMessage `json:"headers"`
		} `json:"input"`
		Expect struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 18 {
		t.Fatalf("unexpected core corpus size: %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			var result any
			var err error
			var value string
			if c.Operation != "mpp.core.encode" && len(c.Input.Value) > 0 {
				if err := json.Unmarshal(c.Input.Value, &value); err != nil {
					t.Fatal(err)
				}
			}
			switch c.Operation {
			case "mpp.core.encode":
				result, err = mpp.Encode(c.Input.Value)
			case "mpp.core.decode":
				result, err = mpp.Decode(value)
			case "mpp.core.decode-credential":
				result, err = mpp.DecodeCredential(value)
			case "mpp.core.decode-receipt":
				result, err = mpp.DecodeReceipt(value)
			case "mpp.core.parse-challenges":
				var headers []string
				if c.Input.Headers[0] == '"' {
					var header string
					if err := json.Unmarshal(c.Input.Headers, &header); err != nil {
						t.Fatal(err)
					}
					headers = []string{header}
				} else if err := json.Unmarshal(c.Input.Headers, &headers); err != nil {
					t.Fatal(err)
				}
				result, err = mpp.ParseChallenges(headers)
			default:
				t.Fatalf("unhandled operation %s", c.Operation)
			}
			if c.Expect.Error != nil {
				var codec *mpp.CodecError
				if !errors.As(err, &codec) {
					t.Fatalf("expected codec error, got %v", err)
				}
				if c.Expect.Error.Code == "invalid-credential" && codec.Artifact != "credential" {
					t.Fatal(codec)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(actual, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Expect.Result, &b); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("got %s, expected %s", actual, c.Expect.Result)
			}
		})
	}
}

func TestCanonicalEncoding(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"z":null,"a":[null,true,false],"b":{"c":null}}`, `{"a":[null,true,false],"b":{}}`},
		{`[1e-7,0.000001,1e20,1e21,-0,333333333.33333329]`, `[1e-7,0.000001,100000000000000000000,1e+21,0,333333333.3333333]`},
		{`{"\ue000":1,"😀":2}`, `{"😀":2,"":1}`},
		{`["<>&\u2028\u2029","\u0000\b\f\n\r\t\"\\"]`, "[\"<>&\u2028\u2029\",\"\\u0000\\b\\f\\n\\r\\t\\\"\\\\\"]"},
	} {
		got, err := mpp.Canonicalize(json.RawMessage(pair[0]))
		if err != nil || string(got) != pair[1] {
			t.Fatalf("%s: %s %v", pair[0], got, err)
		}
	}
	for _, v := range []any{make(chan int), math.NaN(), json.RawMessage(`1e9999`)} {
		if _, err := mpp.Canonicalize(v); err == nil {
			t.Fatal("accepted invalid JSON value")
		}
	}
	if _, err := mpp.Encode(make(chan int)); err == nil {
		t.Fatal("accepted channel")
	}
	if _, err := mpp.Encode(strings.Repeat("a", 65536)); err == nil {
		t.Fatal("accepted oversized value")
	}
	for _, value := range []string{"?", "a", "e30\n", encoded(`{} {}`), encoded(`no`), encoded(string([]byte{0xff})), strings.Repeat("a", 65537)} {
		if _, err := mpp.Decode(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if value, err := mpp.Decode("e30="); err != nil || !reflect.DeepEqual(value, map[string]any{}) {
		t.Fatalf("padded decode %v %v", value, err)
	}
}

func TestChallengeHeaders(t *testing.T) {
	c := challenge()
	c.Realm = `seller "realm"`
	c.Description = ptr(`Pay "now", then \ later`)
	c.Expires = ptr("")
	c.Digest = ptr("sha-256=value")
	c.Opaque = ptr("eyJ6IjogMSwgImEiOiBudWxsfQ==")
	header, err := mpp.RenderChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mpp.ParseChallenge(header)
	if err != nil || !reflect.DeepEqual(c, got) {
		t.Fatalf("roundtrip: %#v %v", got, err)
	}
	gotMany, err := mpp.ParseChallenges([]string{`Basic realm="other", ` + header, header})
	if err != nil || len(gotMany) != 2 || !reflect.DeepEqual(c, gotMany[1]) {
		t.Fatalf("combined: %#v %v", gotMany, err)
	}
	base, err := mpp.RenderChallenge(challenge())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := foundation.ParseChallenge(base)
	if err != nil || parsed.RequestB64 != "e30" {
		t.Fatalf("upstream parse: %v %v", parsed, err)
	}
	for _, bad := range []string{"Basic realm=x", base + ", ID=other", base + ", id=other", base + ", x", base + ", x=", base + `, x="unterminated`, base + `, x="escape\`, base + ", x=token junk", base + ",", base + ", =oops", base + "\r\nInjected: yes", base + ", x=\"\x01\"", strings.Repeat("a", 65537)} {
		if _, err := mpp.ParseChallenge(bad); err == nil {
			t.Fatalf("accepted malformed header %q", bad)
		}
	}
	if _, err := mpp.ParseChallenges([]string{base + ", ID=other"}); err == nil {
		t.Fatal("accepted duplicate")
	}
	if _, err := mpp.ParseChallenges([]string{"\n"}); err == nil {
		t.Fatal("accepted control")
	}
	if _, err := mpp.ParseChallenge(`Payment id=i, realm=r, method=inflow, intent=charge, request=e30, custom="ok"`); err != nil {
		t.Fatal(err)
	}
	if _, err := mpp.ParseChallenge("Payment "); err == nil {
		t.Fatal("accepted empty")
	}
	for _, c := range []mpp.Challenge{{}, {ID: "\n", Realm: "r", Method: "inflow", Intent: "charge", Request: "e30"}} {
		if _, err := mpp.RenderChallenge(c); err == nil {
			t.Fatal("accepted invalid challenge")
		}
	}
	c = challenge()
	c.Description = ptr("\x7f")
	if _, err := mpp.RenderChallenge(c); err == nil {
		t.Fatal("accepted control")
	}
	c = challenge()
	c.Description = ptr(strings.Repeat("a", 65536))
	if _, err := mpp.RenderChallenge(c); err == nil {
		t.Fatal("accepted oversized header")
	}
}

func TestCredentialPreservation(t *testing.T) {
	c := mpp.Credential{Challenge: challenge(), Payload: map[string]any{"number": json.Number("9007199254740993"), "null": nil}, Source: ptr("")}
	c.Challenge.Opaque = ptr("literal-opaque")
	c.Challenge.Description = ptr("")
	wire, err := mpp.EncodeCredential(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mpp.DecodeCredential(wire)
	if err != nil || !reflect.DeepEqual(c, got) {
		t.Fatalf("credential changed: %#v %v", got, err)
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"challenge":1}`, `{"challenge":{"id":"x","realm":"r","method":"inflow","intent":"charge","request":"e30"},"payload":[]}`} {
		if _, err := mpp.DecodeCredential(encoded(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := mpp.DecodeCredential("?"); err == nil {
		t.Fatal("accepted invalid encoding")
	}
	if _, err := mpp.EncodeCredential(mpp.Credential{}); err == nil {
		t.Fatal("accepted empty credential")
	}
	c.Challenge = challenge()
	c.Payload = nil
	if _, err := mpp.EncodeCredential(c); err == nil {
		t.Fatal("accepted nil payload")
	}
	c.Payload = map[string]any{"bad": make(chan int)}
	if _, err := mpp.EncodeCredential(c); err == nil {
		t.Fatal("accepted non JSON")
	}
	c.Payload = map[string]any{"large": strings.Repeat("a", 65536)}
	if _, err := mpp.EncodeCredential(c); err == nil {
		t.Fatal("accepted oversized")
	}
	raw := `{"challenge":{"id":"i","realm":"r","method":"inflow","intent":"charge","request":"e30"},"payload":{},"source":123}`
	if _, err := mpp.DecodeCredential(encoded(raw)); err == nil {
		t.Fatal("accepted invalid source")
	}
}

func TestReceiptPreservation(t *testing.T) {
	r := receipt()
	r.ChallengeID = ptr("challenge")
	r.SubscriptionID = ptr("subscription")
	r.ExternalID = ptr("")
	r.Settlement = &mpp.Settlement{Amount: "10.00", Currency: "USD"}
	r.Extensions = map[string]json.RawMessage{"nested": json.RawMessage(`{"n":9007199254740993,"null":null}`)}
	wire, err := mpp.EncodeReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mpp.DecodeReceipt(wire)
	if err != nil || !reflect.DeepEqual(r, got) {
		t.Fatalf("receipt changed %#v %v", got, err)
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"method":123}`, `{"method":"inflow","reference":"r","status":"failed","timestamp":"2026-09-01T12:00:00Z"}`} {
		if _, err := mpp.DecodeReceipt(encoded(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := mpp.DecodeReceipt("?"); err == nil {
		t.Fatal("accepted invalid base64")
	}
	for _, mutate := range []func(*mpp.Receipt){func(r *mpp.Receipt) { r.Method = "" }, func(r *mpp.Receipt) { r.Timestamp = "yesterday" }, func(r *mpp.Receipt) { r.ChallengeID = ptr("") }, func(r *mpp.Receipt) { r.SubscriptionID = ptr("") }, func(r *mpp.Receipt) { r.Settlement = &mpp.Settlement{} }, func(r *mpp.Receipt) {
		r.Extensions = map[string]json.RawMessage{"status": json.RawMessage(`"success"`)}
	}, func(r *mpp.Receipt) { r.Extensions = map[string]json.RawMessage{"bad": json.RawMessage(`invalid`)} }, func(r *mpp.Receipt) { r.ExternalID = ptr(strings.Repeat("a", 65536)) }} {
		r := receipt()
		mutate(&r)
		if _, err := mpp.EncodeReceipt(r); err == nil {
			t.Fatalf("accepted invalid receipt %#v", r)
		}
	}
}

func TestConcurrentCodecsDoNotMutateInputs(t *testing.T) {
	input := map[string]any{"b": []any{true, nil}, "a": "amount"}
	before, _ := json.Marshal(input)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			value, err := mpp.Encode(input)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := mpp.Decode(value); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("mutated input")
	}
}

func TestMalformedPaymentSchemeIsNotIgnored(t *testing.T) {
	for _, header := range []string{"Payment", "Payment\tid=x", "Payment id=x"} {
		_, err := mpp.ParseChallenges([]string{header})
		var codec *mpp.CodecError
		if !errors.As(err, &codec) || !strings.HasPrefix(err.Error(), "mpp: invalid ") {
			t.Fatalf("malformed Payment header was not rejected: %q: %v", header, err)
		}
	}
}

func TestDecodersRejectTrailingJSON(t *testing.T) {
	credential := mpp.Credential{Challenge: challenge(), Payload: map[string]any{}}
	credentialJSON, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	receiptJSON, err := json.Marshal(receipt())
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{" {}", " null", " garbage"} {
		if _, err := mpp.Decode(encoded(`{}` + suffix)); err == nil {
			t.Fatal("accepted trailing value")
		}
		if _, err := mpp.DecodeCredential(encoded(string(credentialJSON) + suffix)); err == nil {
			t.Fatal("accepted trailing credential data")
		}
		if _, err := mpp.DecodeReceipt(encoded(string(receiptJSON) + suffix)); err == nil {
			t.Fatal("accepted trailing receipt data")
		}
	}
	if _, err := mpp.Decode(encoded(`{} `)); err != nil {
		t.Fatal(err)
	}
	if _, err := mpp.DecodeReceipt(encoded(`{"method":`)); err == nil {
		t.Fatal("accepted incomplete receipt JSON")
	}
}

func TestCredentialRequiresPayloadButNotSource(t *testing.T) {
	for _, payload := range []string{"", `,"payload":null`} {
		c, err := json.Marshal(challenge())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mpp.DecodeCredential(encoded(`{"challenge":` + string(c) + payload + `}`)); err == nil {
			t.Fatal("accepted missing payload")
		}
	}
	wire, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenge(), Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := mpp.DecodeCredential(wire)
	if err != nil || result.Source != nil || result.Payload == nil {
		t.Fatalf("optional source: %#v %v", result, err)
	}
}

func TestReceiptJSONRoundTripAndFailureDoesNotMutate(t *testing.T) {
	original := receipt()
	original.Extensions = map[string]json.RawMessage{"custom": json.RawMessage(`{"values":[null,9007199254740993]}`)}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored mpp.Receipt
	if err := json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatalf("receipt changed: %#v", restored)
	}
	for _, input := range []string{`[]`, `{"method":123}`, `{"settlement":false}`} {
		if err := json.Unmarshal([]byte(input), &restored); err == nil {
			t.Fatal("accepted wrong field type")
		}
		if !reflect.DeepEqual(original, restored) {
			t.Fatal("failed decoding mutated receiver")
		}
	}
	for _, reserved := range []string{"method", "reference", "status", "timestamp", "challengeId", "subscriptionId", "externalId", "settlement"} {
		r := receipt()
		r.Extensions = map[string]json.RawMessage{reserved: json.RawMessage(`null`)}
		if _, err := json.Marshal(r); err == nil {
			t.Fatalf("extension replaced %s", reserved)
		}
	}
	after, err := json.Marshal(original)
	if err != nil || string(before) != string(after) {
		t.Fatal("serialization mutated receipt")
	}
}

func FuzzChallengeRoundTrip(f *testing.F) {
	f.Add(`Pay "now", later`)
	f.Fuzz(func(t *testing.T, description string) {
		c := challenge()
		c.Description = &description
		header, err := mpp.RenderChallenge(c)
		if err != nil {
			return
		}
		got, err := mpp.ParseChallenge(header)
		if err != nil || !reflect.DeepEqual(c, got) {
			t.Fatalf("roundtrip %v", err)
		}
	})
}
