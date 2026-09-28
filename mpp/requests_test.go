package mpp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
)

func TestChargeValidation(t *testing.T) {
	valid := mpp.ChargeRequest{Amount: "10.5", Currency: "USDC", Recipient: "11111111-1111-1111-1111-111111111111", MethodDetails: &mpp.InflowMethodDetails{Rail: "instrument", InstrumentID: "33333333-3333-3333-3333-333333333333"}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, amount := range []string{"0", "-1.2", "01.00"} {
		r := mpp.ChargeRequest{Amount: amount, Currency: "USDC"}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*mpp.ChargeRequest){func(r *mpp.ChargeRequest) { r.Amount = "1e3" }, func(r *mpp.ChargeRequest) { r.Currency = "" }, func(r *mpp.ChargeRequest) { r.Recipient = "bad" }, func(r *mpp.ChargeRequest) { r.MethodDetails = &mpp.InflowMethodDetails{Rail: "blockchain"} }, func(r *mpp.ChargeRequest) { r.MethodDetails = &mpp.InflowMethodDetails{InstrumentID: "bad"} }} {
		r := valid
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Fatal(r)
		}
	}
}

func TestSubscriptionValidation(t *testing.T) {
	valid := mpp.SubscriptionRequest{ChargeRequest: mpp.ChargeRequest{Amount: "5.00", Currency: "USDC"}, PeriodUnit: "month", PeriodCount: 1, SubscriptionExpires: "2099-01-01T00:00:00Z", ExternalID: ptr("merchant-plan")}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"amount":"5.00"`) || strings.Contains(string(raw), "ChargeRequest") {
		t.Fatal(string(raw))
	}
	for _, mutate := range []func(*mpp.SubscriptionRequest){func(r *mpp.SubscriptionRequest) { r.Currency = "" }, func(r *mpp.SubscriptionRequest) { r.Amount = "0.00" }, func(r *mpp.SubscriptionRequest) { r.Amount = "-1" }, func(r *mpp.SubscriptionRequest) { r.PeriodUnit = "forever" }, func(r *mpp.SubscriptionRequest) { r.PeriodCount = 0 }, func(r *mpp.SubscriptionRequest) { r.PeriodCount = 9007199254740992 }, func(r *mpp.SubscriptionRequest) { r.PeriodUnit = "minute"; r.PeriodCount = 4 }, func(r *mpp.SubscriptionRequest) { r.SubscriptionExpires = "not a date" }, func(r *mpp.SubscriptionRequest) { r.ExternalID = ptr("  ") }, func(r *mpp.SubscriptionRequest) { r.ExternalID = ptr(strings.Repeat("a", 129)) }} {
		r := valid
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Fatal(r)
		}
	}
	valid.PeriodUnit = "minute"
	valid.PeriodCount = 5
	valid.ExternalID = nil
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTempoValidation(t *testing.T) {
	address := "0x" + strings.Repeat("1", 40)
	memo := "0x" + strings.Repeat("a", 64)
	valid := mpp.TempoRequest{Amount: "0", Currency: address, Recipient: address, Description: ptr("service"), ExternalID: ptr("invoice"), MethodDetails: &mpp.TempoMethodDetails{ChainID: ptr(int64(4217)), FeePayer: ptr(false), Memo: &memo, Splits: []mpp.TempoSplit{{Amount: "1", Recipient: address, Memo: &memo}}, SupportedModes: []string{"pull", "push"}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*mpp.TempoRequest){func(r *mpp.TempoRequest) { r.Amount = "01" }, func(r *mpp.TempoRequest) { r.Amount = "-1" }, func(r *mpp.TempoRequest) { r.Currency = "USD" }, func(r *mpp.TempoRequest) { r.Recipient = "invalid" }, func(r *mpp.TempoRequest) { r.Description = ptr("") }, func(r *mpp.TempoRequest) { r.ExternalID = ptr("") }, func(r *mpp.TempoRequest) { r.MethodDetails = &mpp.TempoMethodDetails{Memo: ptr("bad")} }, func(r *mpp.TempoRequest) {
		r.MethodDetails = &mpp.TempoMethodDetails{Splits: []mpp.TempoSplit{{Amount: "1", Recipient: "bad"}}}
	}, func(r *mpp.TempoRequest) {
		r.MethodDetails = &mpp.TempoMethodDetails{Splits: []mpp.TempoSplit{{Amount: "1", Recipient: address, Memo: ptr("bad")}}}
	}, func(r *mpp.TempoRequest) {
		r.MethodDetails = &mpp.TempoMethodDetails{SupportedModes: []string{"other"}}
	}} {
		r := valid
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Fatal(r)
		}
	}
	if err := (mpp.TempoRequest{Amount: "1"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"transaction", "proof"} {
		if err := (mpp.TempoPayload{Type: kind, Signature: "0xdeadbeef"}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if err := (mpp.TempoPayload{Type: "hash", Hash: "0xdeadbeef"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []mpp.TempoPayload{{Type: "hash", Signature: "0xdeadbeef"}, {Type: "transaction", Hash: "0xdeadbeef"}, {Type: "proof", Hash: "0xdeadbeef"}, {Type: "unknown", Hash: "0xdeadbeef"}, {Type: "hash", Hash: "invalid"}, {Type: "proof", Signature: "invalid"}} {
		if err := p.Validate(); err == nil {
			t.Fatal(p)
		}
	}
}
