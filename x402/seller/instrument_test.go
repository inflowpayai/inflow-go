package seller

import (
	"reflect"
	"testing"

	"github.com/inflowpayai/inflow-go/x402"
)

func TestInstrumentOffers(t *testing.T) {
	config := x402.ConfigResponse{PaymentMethods: []x402.PaymentMethodInfo{{Scheme: "instrument", Network: "inflow:1", PayTo: "seller", Decimals: 18, Extra: map[string]any{"preserve": "value"}}}}
	for _, test := range []struct {
		name, amount, currency, want string
		schemes, networks            []string
		invalid                      bool
	}{
		{name: "default", amount: "1", currency: "USD"},
		{name: "non-fiat", amount: "1", currency: "USDC", schemes: []string{"instrument"}},
		{name: "network-filter", amount: "1", currency: "USD", schemes: []string{"instrument"}, networks: []string{"other"}},
		{name: "minimum", amount: "0.50", currency: "USD", schemes: []string{"instrument"}, want: "500000000000000000"},
		{name: "maximum", amount: "92233720368547758.07", currency: "USD", schemes: []string{"instrument"}, want: "92233720368547758070000000000000000"},
		{name: "zero-tail", amount: "1.000", currency: "USD", schemes: []string{"instrument"}, want: "1000000000000000000"},
		{name: "too-small", amount: "0.49", currency: "USD", schemes: []string{"instrument"}, invalid: true},
		{name: "too-large", amount: "92233720368547758.08", currency: "USD", schemes: []string{"instrument"}, invalid: true},
		{name: "fractional-cent", amount: "1.001", currency: "USD", schemes: []string{"instrument"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := string(encode(t, config))
			offers, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: test.amount, Currency: test.currency}, Schemes: test.schemes, Networks: test.networks}, "")
			if (err != nil) != test.invalid {
				t.Fatalf("error %v", err)
			}
			if test.want != "" {
				if len(offers) != 1 {
					t.Fatalf("offers: %#v", offers)
				}
				if !reflect.DeepEqual(offers[0].Price, map[string]any{"asset": "USD", "amount": test.want}) || offers[0].Extra["assetName"] != "USD" || offers[0].Extra["preserve"] != "value" {
					t.Fatalf("offer: %#v", offers[0])
				}
			} else if len(offers) != 0 {
				t.Fatalf("unexpected offers: %#v", offers)
			}
			if string(encode(t, config)) != before {
				t.Fatal("mutated configuration")
			}
		})
	}
}
