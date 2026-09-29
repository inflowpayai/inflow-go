package x402_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
)

func asJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCoreCases(t *testing.T) {
	b, err := os.ReadFile("testdata/core.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID        string
		Operation string
		Input     struct {
			Value       any
			Declaration json.RawMessage
			PaymentID   string `json:"payment_id"`
		}
		Expect struct{ Result json.RawMessage }
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 14 {
		t.Fatalf("unexpected corpus size: %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			var result any
			switch c.Operation {
			case "x402.core.identifier-valid":
				value, ok := c.Input.Value.(string)
				result = ok && x402.ValidatePaymentID(value)
			case "x402.core.identifier-declaration":
				result = x402.DeclarePaymentIdentifier()
			case "x402.core.identifier-entry":
				result = x402.PaymentIdentifierEntry(c.Input.Declaration, c.Input.PaymentID)
			default:
				t.Fatalf("unhandled operation: %s", c.Operation)
			}
			var got, want any
			if err := json.Unmarshal([]byte(asJSON(t, result)), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Expect.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v; want %v", got, want)
			}
		})
	}
}

func TestIdentifierGeneration(t *testing.T) {
	seen := make(map[string]bool)
	for _, prefix := range []string{"", x402.DefaultPaymentIDPrefix, strings.Repeat("x", 96)} {
		for range 32 {
			id, err := x402.GeneratePaymentID(prefix)
			if err != nil || !x402.ValidatePaymentID(id) || !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+32 || seen[id] {
				t.Fatalf("bad generated identifier: %q %v", id, err)
			}
			seen[id] = true
		}
	}
	for _, prefix := range []string{strings.Repeat("x", 97), "space ", "é", "\n"} {
		if id, err := x402.GeneratePaymentID(prefix); id != "" || err == nil {
			t.Fatalf("accepted prefix %q", prefix)
		}
	}
}

func TestMalformedDeclarations(t *testing.T) {
	base := asJSON(t, x402.DeclarePaymentIdentifier())
	values := []any{nil, true, []any{}, func() {}, json.RawMessage(`{"info":false}`), json.RawMessage(`{"info":{}}`), json.RawMessage(`{"info":{"required":null}}`), json.RawMessage(`{"info":{"required":false},"schema":false}`)}
	for _, replacement := range [][2]string{
		{`"info"`, `"INFO"`}, {`"schema"`, `"SCHEMA"`}, {`"properties"`, `"Properties"`},
		{`"minLength"`, `"MinLength"`}, {`"type"`, `"TYPE"`},
		{`"required":false`, `"required":0`}, {`"required":false`, `"required":"false"`},
		{`"object"`, `"array"`}, {`"string"`, `"number"`}, {`"minLength":16`, `"minLength":15`},
		{`"maxLength":128`, `"maxLength":129`}, {`"boolean"`, `"integer"`},
		{`["required"]`, `[]`}, {`["required"]`, `["id"]`}, {`["required"]`, `["required","id"]`},
		{`2020-12`, `draft-07`}, {`^[a-zA-Z0-9_-]+$`, `.*`},
	} {
		values = append(values, json.RawMessage(strings.Replace(base, replacement[0], replacement[1], 1)))
	}
	for _, value := range values {
		if x402.ReadPaymentIdentifier(value) != nil {
			t.Fatalf("accepted malformed value %v", value)
		}
	}
	if x402.PaymentIdentifierEntry(nil, strings.Repeat("a", 16)) != nil {
		t.Fatal("accepted missing declaration")
	}
}

func TestIdentifierPreservation(t *testing.T) {
	original := x402.DeclarePaymentIdentifier()
	original.Info["merchant"] = json.RawMessage(`{"proof":9007199254740993}`)
	original.Schema = json.RawMessage(strings.Replace(string(original.Schema), `"minLength":16`, `"minLength":16.0,"description":"merchant rule"`, 1))
	before := asJSON(t, original)
	entry := x402.PaymentIdentifierEntry(original, strings.Repeat("a", 16))
	if entry == nil || string(entry.Info["merchant"]) != `{"proof":9007199254740993}` || !bytes.Contains(entry.Schema, []byte("merchant rule")) {
		t.Fatal("lost extension fields")
	}
	entry.Info["merchant"][0] = ' '
	entry.Schema[0] = ' '
	if asJSON(t, original) != before {
		t.Fatal("mutated caller input")
	}
	first := x402.DeclarePaymentIdentifier()
	first.Info["required"][0] = 'x'
	first.Schema[0] = 'x'
	if x402.ReadPaymentIdentifier(x402.DeclarePaymentIdentifier()) == nil {
		t.Fatal("shared mutable declaration")
	}
}

func TestNormalize(t *testing.T) {
	for input, want := range map[string]string{
		"001.2300": "1.23", "-000.0100": "-0.01", "-0.000": "0", "000": "0", "010": "10", "-12": "-12",
		"900719925474099312345.123400": "900719925474099312345.1234", "1e3": "1e3", "": "", ".1": ".1", "1.": "1.", " 1": " 1", "+1": "+1", "1,000": "1,000",
	} {
		if got := x402.NormalizeDecimalString(input); got != want {
			t.Fatalf("%q -> %q, want %q", input, got, want)
		}
	}
}

func TestUpstreamTypesAndExtensions(t *testing.T) {
	var requirement foundation.PaymentRequirements = x402.PaymentRequirements{Scheme: x402.SchemeBalance, Network: x402.NetworkInflow, Amount: "9007199254740993", Extra: map[string]any{"unknown": "retained"}}
	var payload foundation.PaymentPayload = x402.PaymentPayload{X402Version: x402.Version, Accepted: requirement, Payload: map[string]any{"proof": json.Number("9007199254740993")}, Extensions: x402.DeclareSponsorship()}
	wire := asJSON(t, payload)
	if !strings.Contains(wire, `"proof":9007199254740993`) || !strings.Contains(wire, `"unknown":"retained"`) {
		t.Fatal(wire)
	}
	if payload.GetVersion() != 2 || payload.GetScheme() != x402.SchemeBalance || payload.GetNetwork() != x402.NetworkInflow {
		t.Fatal("upstream interface mismatch")
	}
	var required foundation.PaymentRequired = x402.PaymentRequired{Accepts: []x402.PaymentRequirements{requirement}}
	if required.Accepts[0].Amount != requirement.Amount {
		t.Fatal("requirement mismatch")
	}
	first := x402.DeclareSponsorship()
	delete(first, x402.InflowEip7702GasSponsoring)
	if len(x402.DeclareSponsorship()) != 1 {
		t.Fatal("shared declaration")
	}
	if got := asJSON(t, x402.SponsorshipInfo{Version: "1", SponsorshipID: "id", Signature: "0xab"}); strings.Contains(got, "authorizationSignature") {
		t.Fatal(got)
	}
	no := false
	asset := x402.AssetInfo{SupportsEip7702: &no}
	if !strings.Contains(asJSON(t, asset), `"supportsEip7702":false`) || strings.Contains(asJSON(t, x402.AssetInfo{}), "supportsEip7702") {
		t.Fatal("absent and false conflated")
	}
}
