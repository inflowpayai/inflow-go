package buyer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inflowpayai/inflow-go/internal/mppwire"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	fc "github.com/tempoxyz/mpp-go/pkg/client"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func TestMethodLimitsConfiguration(t *testing.T) {
	precision := func(n int) *int { return &n }
	for _, tc := range []struct {
		name, method, intent string
		limits               []buyer.MethodLimits
	}{
		{"multiple", "inflow", "charge", []buyer.MethodLimits{{}, {}}},
		{"subscription", "inflow", "subscription", []buyer.MethodLimits{{Currencies: []string{}}}},
		{"inflow precision", "inflow", "charge", []buyer.MethodLimits{{MaxAmountDecimals: precision(2)}}},
		{"card chain", "card", "charge", []buyer.MethodLimits{{AllowedChainIDs: []int64{1}}}},
		{"inflow atomic", "inflow", "charge", []buyer.MethodLimits{{MaxAtomicAmount: "1"}}},
		{"card currency", "card", "charge", []buyer.MethodLimits{{Currencies: []string{"usd"}}}},
		{"card recipient", "card", "charge", []buyer.MethodLimits{{ExpectedRecipients: []string{"seller"}}}},
		{"empty currency", "tempo", "charge", []buyer.MethodLimits{{Currencies: []string{""}}}},
		{"empty recipient", "inflow", "charge", []buyer.MethodLimits{{ExpectedRecipients: []string{""}}}},
		{"negative chain", "tempo", "charge", []buyer.MethodLimits{{AllowedChainIDs: []int64{-1}}}},
		{"unsafe chain", "tempo", "charge", []buyer.MethodLimits{{AllowedChainIDs: []int64{9007199254740992}}}},
		{"negative precision", "tempo", "charge", []buyer.MethodLimits{{MaxAmountDecimals: precision(-1)}}},
		{"large precision", "tempo", "charge", []buyer.MethodLimits{{MaxAmountDecimals: precision(256)}}},
		{"tempo unpinned decimal", "tempo", "charge", []buyer.MethodLimits{{MaxAmount: "1"}}},
		{"tempo unpinned atomic", "tempo", "charge", []buyer.MethodLimits{{MaxAtomicAmount: "1"}}},
		{"negative amount", "inflow", "charge", []buyer.MethodLimits{{MaxAmount: "-1"}}},
		{"exponent", "inflow", "charge", []buyer.MethodLimits{{MaxAmount: "1e3"}}},
		{"inflow unpinned", "inflow", "charge", []buyer.MethodLimits{{MaxAmount: "1"}}},
		{"tempo missing precision", "tempo", "charge", []buyer.MethodLimits{{MaxAmount: "1", Currencies: []string{"token"}}}},
		{"card excess precision", "card", "charge", []buyer.MethodLimits{{MaxAmount: "0.001"}}},
		{"fractional atomic", "card", "charge", []buyer.MethodLimits{{MaxAtomicAmount: "1.5"}}},
		{"negative atomic", "card", "charge", []buyer.MethodLimits{{MaxAtomicAmount: "-1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (*buyer.Client)(nil).Method(tc.method, tc.intent, buyer.PaymentOptions{}, tc.limits...); err == nil {
				t.Fatal("invalid limits accepted")
			}
		})
	}
}

func limitedChallenge(t *testing.T, name string, request any) foundation.Challenge {
	t.Helper()
	wire := challenge()
	wire.Method = name
	if name == "card" {
		wire = cardChallenge(t)
	}
	encoded, err := mpp.Encode(request)
	if err != nil {
		t.Fatal(err)
	}
	wire.Request = encoded
	wire.Opaque = nil
	native, err := mppwire.ToChallenge(wire)
	if err != nil {
		t.Fatal(err)
	}
	return native
}

func TestMethodLimitsEnforcement(t *testing.T) {
	decimals := 6
	token := "0x" + strings.Repeat("a", 40)
	recipient := "0x" + strings.Repeat("b", 40)
	other := "0x" + strings.Repeat("c", 40)
	chain := int64(4217)
	card, err := mpp.DecodeCardRequest(cardChallenge(t).Request)
	if err != nil {
		t.Fatal(err)
	}
	card.Amount = "50"
	inflowRequest := func(amount, currency, recipient string) mpp.ChargeRequest {
		return mpp.ChargeRequest{Amount: amount, Currency: currency, Recipient: recipient}
	}
	tempoRequest := func(amount string, details *mpp.TempoMethodDetails) mpp.TempoRequest {
		return mpp.TempoRequest{Amount: amount, Currency: token, Recipient: recipient, MethodDetails: details}
	}
	for _, tc := range []struct {
		name, method string
		limits       buyer.MethodLimits
		request      any
		allowed      bool
	}{
		{"decimal equal", "inflow", buyer.MethodLimits{MaxAmount: "0.50", Currencies: []string{"USDC"}}, inflowRequest("0.5000", "USDC", ""), true},
		{"decimal above", "inflow", buyer.MethodLimits{MaxAmount: "0.50", Currencies: []string{"USDC"}}, inflowRequest("0.500000000000000001", "USDC", ""), false},
		{"large exact", "inflow", buyer.MethodLimits{MaxAmount: "9007199254740993", Currencies: []string{"USDC"}}, inflowRequest("9007199254740993", "USDC", ""), true},
		{"leading zero", "inflow", buyer.MethodLimits{MaxAmount: "010", Currencies: []string{"USDC"}}, inflowRequest("009", "USDC", ""), true},
		{"zero allowed", "inflow", buyer.MethodLimits{MaxAmount: "0", Currencies: []string{"USDC"}}, inflowRequest("0", "USDC", ""), true},
		{"zero rejected", "inflow", buyer.MethodLimits{MaxAmount: "0", Currencies: []string{"USDC"}}, inflowRequest("0.01", "USDC", ""), false},
		{"wrong currency", "inflow", buyer.MethodLimits{Currencies: []string{"USDC"}}, inflowRequest("1", "USD", ""), false},
		{"empty currencies", "inflow", buyer.MethodLimits{Currencies: []string{}}, inflowRequest("1", "USDC", ""), false},
		{"recipient allowed", "inflow", buyer.MethodLimits{ExpectedRecipients: []string{transactionID}}, inflowRequest("1", "USDC", transactionID), true},
		{"recipient absent", "inflow", buyer.MethodLimits{ExpectedRecipients: []string{transactionID}}, inflowRequest("1", "USDC", ""), false},
		{"empty recipients", "inflow", buyer.MethodLimits{ExpectedRecipients: []string{}}, inflowRequest("1", "USDC", transactionID), false},
		{"negative amount", "inflow", buyer.MethodLimits{Currencies: []string{"USDC"}}, inflowRequest("-1", "USDC", ""), false},
		{"field type", "inflow", buyer.MethodLimits{Currencies: []string{"USDC"}}, map[string]any{"amount": 1}, false},
		{"card cents", "card", buyer.MethodLimits{MaxAmount: "0.5000", MaxAtomicAmount: "050"}, card, true},
		{"card decimal exceeded", "card", buyer.MethodLimits{MaxAmount: "0.49"}, card, false},
		{"card atomic exceeded", "card", buyer.MethodLimits{MaxAtomicAmount: "49"}, card, false},
		{"tempo token units", "tempo", buyer.MethodLimits{MaxAmount: "0.5", MaxAmountDecimals: &decimals, Currencies: []string{strings.ToUpper(token)}}, tempoRequest("500000", nil), true},
		{"tempo token above", "tempo", buyer.MethodLimits{MaxAmount: "0.5", MaxAmountDecimals: &decimals, Currencies: []string{token}}, tempoRequest("500001", nil), false},
		{"tempo atomic", "tempo", buyer.MethodLimits{MaxAtomicAmount: "500000", Currencies: []string{token}}, tempoRequest("500000", nil), true},
		{"tempo fractional", "tempo", buyer.MethodLimits{Currencies: []string{token}}, tempoRequest("0.5", nil), false},
		{"tempo chain", "tempo", buyer.MethodLimits{AllowedChainIDs: []int64{chain}}, tempoRequest("1", &mpp.TempoMethodDetails{ChainID: &chain}), true},
		{"tempo absent details", "tempo", buyer.MethodLimits{AllowedChainIDs: []int64{chain}}, tempoRequest("1", nil), false},
		{"tempo absent chain", "tempo", buyer.MethodLimits{AllowedChainIDs: []int64{chain}}, tempoRequest("1", &mpp.TempoMethodDetails{}), false},
		{"tempo empty chains", "tempo", buyer.MethodLimits{AllowedChainIDs: []int64{}}, tempoRequest("1", &mpp.TempoMethodDetails{ChainID: &chain}), false},
		{"tempo recipients", "tempo", buyer.MethodLimits{ExpectedRecipients: []string{strings.ToUpper(recipient)}}, tempoRequest("2", &mpp.TempoMethodDetails{Splits: []mpp.TempoSplit{{Amount: "1", Recipient: recipient}}}), true},
		{"tempo split rejected", "tempo", buyer.MethodLimits{ExpectedRecipients: []string{recipient}}, tempoRequest("2", &mpp.TempoMethodDetails{Splits: []mpp.TempoSplit{{Amount: "1", Recipient: other}}}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var input struct{ Challenge mpp.Challenge }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
				if err != nil {
					t.Error(err)
				}
				if tc.method == "card" {
					credential = cardCredential(t, input.Challenge)
				}
				json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential})
			}))
			defer api.Close()
			options := buyer.PaymentOptions{}
			if tc.method == "card" {
				options = cardOptions()
			}
			method, err := newClient(t, api, nil).Method(tc.method, "charge", options, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			native := limitedChallenge(t, tc.method, tc.request)
			if got := method.(fc.ChallengeMatcher).CanHandleChallenge(&native); got != tc.allowed {
				t.Fatalf("matcher %v, want %v", got, tc.allowed)
			}
			ctx := buyer.WithPaymentOptions(context.Background(), options)
			_, err = method.CreateCredential(ctx, &native)
			if (err == nil) != tc.allowed {
				t.Fatalf("credential error: %v", err)
			}
			wantCalls := int32(0)
			if tc.allowed {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("platform calls: %d", calls.Load())
			}
		})
	}
}

func TestNativeTransportMethodLimits(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var input struct{ Challenge mpp.Challenge }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		value, _ := mpp.Decode(input.Challenge.Request)
		want := "0.50"
		if call == 2 {
			want = "0.51"
		}
		if value.(map[string]any)["amount"] != want {
			t.Errorf("platform amount %v, want %s", value, want)
		}
		credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
		if err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential})
	}))
	defer api.Close()
	client := newClient(t, api, nil)
	currencies := []string{"USDC"}
	method, err := client.Method("inflow", "charge", buyer.PaymentOptions{}, buyer.MethodLimits{MaxAmount: "0.50", Currencies: currencies})
	if err != nil {
		t.Fatal(err)
	}
	currencies[0] = "EUR"
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(204)
			return
		}
		for _, amount := range []string{"0.51", "0.50"} {
			if r.URL.Path == "/denied" && amount == "0.50" {
				continue
			}
			wire := challenge()
			wire.ID = amount
			wire.Request, _ = mpp.Encode(mpp.ChargeRequest{Amount: amount, Currency: "USDC"})
			header, err := mpp.RenderChallenge(wire)
			if err != nil {
				t.Error(err)
			}
			w.Header().Add("WWW-Authenticate", header)
		}
		w.WriteHeader(402)
	}))
	defer resource.Close()
	native := fc.New([]fc.Method{method}, fc.WithMaxRetries(1))
	r, _ := http.NewRequestWithContext(buyer.WithPaymentOptions(context.Background(), buyer.PaymentOptions{}), "GET", resource.URL, nil)
	response, err := native.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 || calls.Load() != 1 {
		t.Fatalf("status %d, calls %d", response.StatusCode, calls.Load())
	}
	denied, err := http.NewRequestWithContext(r.Context(), "GET", resource.URL+"/denied", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = native.Do(denied)
	if err == nil {
		response.Body.Close()
		if response.StatusCode != 402 {
			t.Fatalf("denied status %d", response.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("all-rejected challenges reached the platform")
	}
	// Credential creation rechecks the limits even if selection was skipped.
	above := limitedChallenge(t, "inflow", mpp.ChargeRequest{Amount: "0.51", Currency: "USDC"})
	if _, err := method.CreateCredential(r.Context(), &above); err == nil {
		t.Fatal("direct method call bypassed limits")
	}
	if calls.Load() != 1 {
		t.Fatal("rejected credential reached platform")
	}
	// Direct client calls are outside the configured method's limits.
	wire, err := mppwire.FromChallenge(above)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Fulfil(context.Background(), wire, buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("direct client call inherited method limits")
	}
}

func TestMethodLimitsSnapshotAndMalformedRequest(t *testing.T) {
	decimals := 6
	chain := int64(4217)
	token := "0x" + strings.Repeat("a", 40)
	recipient := "0x" + strings.Repeat("b", 40)
	limits := buyer.MethodLimits{MaxAmount: "0.5", MaxAmountDecimals: &decimals, Currencies: []string{token}, ExpectedRecipients: []string{recipient}, AllowedChainIDs: []int64{chain}}
	method, err := (*buyer.Client)(nil).Method("tempo", "charge", buyer.PaymentOptions{}, limits)
	if err != nil {
		t.Fatal(err)
	}
	decimals = 0
	limits.Currencies[0] = "changed"
	limits.ExpectedRecipients[0] = "changed"
	limits.AllowedChainIDs[0] = 1
	request := mpp.TempoRequest{Amount: "500000", Currency: token, Recipient: recipient, MethodDetails: &mpp.TempoMethodDetails{ChainID: &chain}}
	valid := limitedChallenge(t, "tempo", request)
	matcher := method.(fc.ChallengeMatcher)
	if !matcher.CanHandleChallenge(&valid) {
		t.Fatal("caller mutation changed configured limits")
	}
	request.Amount = "500001"
	above := limitedChallenge(t, "tempo", request)
	if matcher.CanHandleChallenge(&above) {
		t.Fatal("caller mutation relaxed amount limit")
	}
	for _, encoded := range []string{"bm90LWpzb24", "bnVsbA", "W10"} {
		invalid := valid
		invalid.RequestB64 = encoded
		invalid.Request = nil
		if matcher.CanHandleChallenge(&invalid) {
			t.Fatal("malformed request accepted")
		}
		if _, err := method.CreateCredential(context.Background(), &invalid); err == nil {
			t.Fatal("malformed request created credential")
		}
	}
}
