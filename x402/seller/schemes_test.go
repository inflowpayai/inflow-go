package seller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	upto "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/server"
)

func sampleConfig() x402.ConfigResponse {
	return x402.ConfigResponse{
		SellerID: "seller",
		Wallets:  []x402.WalletInfo{{Blockchain: "BASE", Address: "0x1111111111111111111111111111111111111111"}},
		Assets: []x402.AssetInfo{{
			Blockchain: "BASE", Network: "eip155:8453",
			AssetID: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", AssetName: "USDC", Currency: "USDC", Decimals: 6,
			AssetTransferMethod: "eip3009", Permit2Proxy: x402.Permit2Proxy, TokenName: "USD Coin", TokenVersion: "2",
		}},
		PaymentMethods: []x402.PaymentMethodInfo{{Scheme: "balance", Network: "inflow:1", PayTo: "seller", Decimals: 18}},
		Supported: []x402.SupportedKind{{
			X402Version: 2, Scheme: "upto", Network: "eip155:8453",
			Extra: map[string]any{
				"assetTransferMethod": "permit2",
				"permit2Proxy":        "0x2222222222222222222222222222222222222222",
				"facilitatorAddress":  "0x3333333333333333333333333333333333333333",
			},
		}},
	}
}

func configured(t *testing.T, config x402.ConfigResponse) *Client {
	t.Helper()
	data := encode(t, config)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	t.Cleanup(s.Close)
	c, err := New(inflow.Options{BaseURL: s.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSchemeRegistrations(t *testing.T) {
	ctx := context.Background()
	config := sampleConfig()
	config.Assets = append(config.Assets, config.Assets[0], x402.AssetInfo{Network: "solana:main"})
	config.PaymentMethods = append(config.PaymentMethods, x402.PaymentMethodInfo{Scheme: "custom", Network: "inflow:1", Extra: map[string]any{"assetTransferMethod": "custom-transfer"}})
	c := configured(t, config)
	registrations, err := c.SchemeRegistrations(ctx, RegistrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations) != 4 {
		t.Fatalf("registrations %d", len(registrations))
	}
	server := registrations[0].Server
	if server.DefaultAssetTransferMethod() != "eip3009" || len(server.PaymentFlows()) != 2 {
		t.Fatal("transfer methods lost")
	}
	for _, flow := range server.PaymentFlows() {
		if flow.Default != foundation.PaymentFlowAuthorization || len(flow.Supported) != 1 {
			t.Fatal(flow)
		}
	}
	if registrations[1].Server.DefaultAssetTransferMethod() != foundation.SDKDefaultAssetTransferMethod {
		t.Fatal("default sentinel")
	}
	for _, price := range []any{foundation.AssetAmount{Asset: "USDC", Amount: "1"}, map[string]any{"asset": "USDC", "amount": "1", "extra": map[string]any{"test": true}}} {
		parsed, err := server.ParsePrice(price, "inflow:1")
		if err != nil || parsed.Amount != "1" {
			t.Fatal(parsed, err)
		}
	}
	for _, price := range []any{"$1", map[string]any{"asset": "USDC", "amount": 1}, nil} {
		if _, err := server.ParsePrice(price, "inflow:1"); err == nil {
			t.Fatal("unresolved price accepted")
		}
	}
	requirement := foundation.PaymentRequirements{Amount: "1"}
	got, err := server.EnhancePaymentRequirements(ctx, requirement, foundation.SupportedKind{}, nil)
	if err != nil || got.Amount != "1" {
		t.Fatal(got, err)
	}
	for _, scheme := range []foundation.SchemeNetworkServer{nil, server} {
		if _, err := c.SchemeRegistrations(ctx, RegistrationOptions{Schemes: []string{"upto"}, MeteredScheme: scheme}); err == nil {
			t.Fatal("missing/wrong metered scheme accepted")
		}
	}
	metered := upto.NewUptoEvmScheme()
	registrations, err = c.SchemeRegistrations(ctx, RegistrationOptions{Schemes: []string{"upto"}, MeteredScheme: metered})
	if err != nil || len(registrations) != 1 || registrations[0].Server != metered {
		t.Fatal(registrations, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.SchemeRegistrations(cancelled, RegistrationOptions{}); err == nil {
		t.Fatal("cancelled")
	}
}

func TestOfferBoundaries(t *testing.T) {
	ctx := context.Background()
	config := sampleConfig()
	c := configured(t, config)
	if offers, err := c.Accepts(ctx, AcceptsOptions{Price: PriceSpec{Amount: "1 OTHER"}, Networks: []string{"not-a-network"}}); err != nil || len(offers) != 0 {
		t.Fatal(offers, err)
	}
	for _, price := range []PriceSpec{{Amount: "1.00", Currency: "USDC"}, {Amount: "$0"}, {Amount: "0.00000000 USDC"}, {Amount: "1.23000000 USDC"}, {Amount: "$1", Currency: "USDC"}} {
		if _, err := c.Accepts(ctx, AcceptsOptions{Price: price}); err != nil {
			t.Fatal(err)
		}
	}
	for _, price := range []string{"$1 USDC", "1e3", "-1", "1.123456789"} {
		if _, err := c.Accepts(ctx, AcceptsOptions{Price: PriceSpec{Amount: price}}); err == nil {
			t.Fatal(price)
		} else {
			if err.Error() == "" {
				t.Fatal("empty error")
			}
		}
	}
	if _, err := c.Accepts(ctx, AcceptsOptions{Price: PriceSpec{Amount: "$1"}, MaxTimeoutSeconds: -1}); err == nil {
		t.Fatal("negative timeout")
	}
	for _, decimals := range []int{-1, 256} {
		if _, err := atomicAmount("1", "", decimals, "1"); err == nil {
			t.Fatal("bad decimals")
		}
	}
	if got, err := atomicAmount("1", "2300", 2, "1.2300"); err != nil || got != "123" {
		t.Fatal(got, err)
	}
	if got, err := atomicAmount("0", "0000", 2, "0"); err != nil || got != "0" {
		t.Fatal(got, err)
	}
	config.Assets[0].Decimals = 2
	if _, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: "$1.001"}, Schemes: []string{"upto"}}, ""); err == nil {
		t.Fatal("metered truncation")
	}
	config.PaymentMethods[0].Decimals = 2
	if _, err := buildOffers(config, AcceptsOptions{Price: PriceSpec{Amount: "$1.001"}, Schemes: []string{"balance"}}, ""); err == nil {
		t.Fatal("balance truncation")
	}
	if meteredKind(config, x402.AssetInfo{Network: "solana:main"}) != nil {
		t.Fatal("non EVM metered")
	}
	if meteredKind(config, x402.AssetInfo{Network: "eip155:1", Permit2Proxy: "proxy"}) != nil {
		t.Fatal("unadvertised metered")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Accepts(cancelled, AcceptsOptions{}); err == nil {
		t.Fatal("cancelled")
	}
}

func TestRouteFailureBoundaries(t *testing.T) {
	c := configured(t, sampleConfig())
	ctx := context.Background()
	if _, err := c.Route(ctx, RouteOptions{AssetTransferMethod: "other"}); err == nil {
		t.Fatal("unknown transfer")
	}
	if _, err := c.Route(ctx, RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "invalid"}}}); err == nil {
		t.Fatal("invalid price")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Route(cancelled, RouteOptions{}); err == nil {
		t.Fatal("cancelled")
	}
	if route, err := c.Route(ctx, RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "$1"}, Schemes: []string{"upto"}}}); err != nil || route.Extensions != nil {
		t.Fatal(route, err)
	}
	config := sampleConfig()
	yes := true
	config.Assets[0].SupportsEip7702 = &yes
	data := encode(t, config)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/x402/config" {
			w.Write(data)
		} else {
			w.WriteHeader(400)
		}
	}))
	defer s.Close()
	c, _ = New(inflow.Options{BaseURL: s.URL, APIKey: "key"})
	if _, err := c.Route(ctx, RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "$1"}}, AssetTransferMethod: "permit2"}); err == nil {
		t.Fatal("unsupported refresh failure")
	}
}
