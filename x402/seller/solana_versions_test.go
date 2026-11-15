package seller

import (
	"context"
	"reflect"
	"testing"

	"github.com/inflowpayai/inflow-go/x402"
)

func TestSolanaOffersUseMatchingSupportedTransactionVersions(t *testing.T) {
	for _, scenario := range []string{"both", "legacy", "missing", "wrong-network", "wrong-scheme", "wrong-protocol", "evm"} {
		t.Run(scenario, func(t *testing.T) {
			network := "solana:main"
			if scenario == "evm" {
				network = "eip155:8453"
			}
			kind := x402.SupportedKind{X402Version: 2, Scheme: "exact", Network: network, Extra: map[string]any{"transactionVersions": []int{0, 1}, "unrelated": "do-not-copy"}}
			var want any = []any{float64(0), float64(1)}
			switch scenario {
			case "legacy":
				kind.Extra["transactionVersions"] = []int{0}
				want = []any{float64(0)}
			case "missing":
				kind.Extra = nil
				want = nil
			case "wrong-network":
				kind.Network = "solana:other"
				want = nil
			case "wrong-scheme":
				kind.Scheme = "upto"
				want = nil
			case "wrong-protocol":
				kind.X402Version = 1
				want = nil
			case "evm":
				want = nil
			}
			config := x402.ConfigResponse{
				Wallets:   []x402.WalletInfo{{Blockchain: "CHAIN", Address: "seller", FeePayer: "payer"}},
				Assets:    []x402.AssetInfo{{Blockchain: "CHAIN", Network: network, AssetID: "token", AssetName: "USDC", Currency: "USDC", Decimals: 6}},
				Supported: []x402.SupportedKind{kind},
			}
			client := configured(t, config)
			offers, err := client.Accepts(context.Background(), AcceptsOptions{Price: PriceSpec{Amount: "$0.50"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(offers) != 1 {
				t.Fatalf("offers: %v", offers)
			}
			if got := offers[0].Extra["transactionVersions"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("versions = %#v, want %#v", got, want)
			}
			if _, exists := offers[0].Extra["unrelated"]; exists {
				t.Fatal("copied unrelated capability metadata")
			}
			if offers[0].Extra["feePayer"] != "payer" || offers[0].Price.(map[string]any)["amount"] != "500000" {
				t.Fatalf("lost offer fields: %#v", offers[0])
			}
		})
	}
}
