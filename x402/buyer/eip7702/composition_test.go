package eip7702

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/mechanisms/evm"
	exact "github.com/x402-foundation/x402/go/v2/mechanisms/evm/exact/client"
)

type goldenScheme struct{ payload x402.PaymentPayload }

func (g goldenScheme) Scheme() string { return "exact" }
func (g goldenScheme) CreatePaymentPayload(context.Context, x402.PaymentRequirements, foundation.PaymentPayloadContext) (x402.PaymentPayload, error) {
	return g.payload, nil
}

func TestRealUpstreamExtensionComposition(t *testing.T) {
	f := load(t)
	w := newWallet(t)
	e, requests := extension(t, f, w, nil)
	external := foundation.Newx402Client(foundation.WithSpendControls(foundation.SpendControls{AllowedAssets: []foundation.SpendControlAsset{{Network: "eip155:8453", Asset: f.Payload.Accepted.Asset, MaxAmountPerPayment: "123"}}}))
	external.Register("eip155:8453", goldenScheme{f.Payload})
	external.RegisterExtension(e)
	s := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/transactions/x402-supported" {
			t.Error("unexpected managed call")
		}
		fmt.Fprint(out, `{"kinds":[]}`)
	}))
	defer s.Close()
	c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: external})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Sign(context.Background(), x402.PaymentRequired{X402Version: 2, Accepts: []x402.PaymentRequirements{f.Payload.Accepted}, Resource: &x402.ResourceInfo{URL: "https://merchant.example/paid"}, Extensions: x402.DeclareSponsorship()}, buyer.SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.PaymentPayload.Extensions[e.Key()] == nil || requests.Load() != 1 || w.auths != 1 || w.signs != 1 {
		t.Fatal("extension was bypassed")
	}
	f.Payload.Accepted.Amount = "124"
	if _, err = c.Sign(context.Background(), x402.PaymentRequired{X402Version: 2, Accepts: []x402.PaymentRequirements{f.Payload.Accepted}, Extensions: x402.DeclareSponsorship()}, buyer.SignOptions{}); err == nil {
		t.Fatal("upstream atomic spend cap bypassed")
	}
	if requests.Load() != 1 {
		t.Fatal("over-limit payment requested sponsorship")
	}
}

type typedWallet struct {
	*wallet
	nonceReads      int
	typedSignatures int
}

func (w *typedWallet) ReadContract(ctx context.Context, address string, definition []byte, name string, args ...any) (any, error) {
	if name == "nonces" {
		w.nonceReads++
		return big.NewInt(7), nil
	}
	return w.wallet.ReadContract(ctx, address, definition, name, args...)
}
func (w *typedWallet) SignTypedData(_ context.Context, domain evm.TypedDataDomain, types map[string][]evm.TypedDataField, primary string, message map[string]any) ([]byte, error) {
	w.typedSignatures++
	hash, err := evm.HashTypedData(domain, types, primary, message)
	if err != nil {
		return nil, err
	}
	return crypto.Sign(hash, w.key)
}

func TestRealUpstreamEIP2612(t *testing.T) {
	f := load(t)
	w := &typedWallet{wallet: newWallet(t)}
	scheme := exact.NewExactEvmScheme(w, nil)
	r := f.Payload.Accepted
	r.Extra["name"] = "USD Coin"
	r.Extra["version"] = "2"
	payload, err := scheme.CreatePaymentPayload(context.Background(), r, foundation.PaymentPayloadContext{Extensions: map[string]any{"eip2612GasSponsoring": map[string]any{"info": map[string]any{"version": "1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(payload.Extensions["eip2612GasSponsoring"])
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Info struct{ Amount, Spender, Nonce, Signature string } `json:"info"`
	}
	if json.Unmarshal(b, &result) != nil || result.Info.Amount != "123" || !sameAddress(result.Info.Spender, x402.Permit2) || result.Info.Nonce != "7" || len(result.Info.Signature) != 132 {
		t.Fatalf("incorrect permit: %s", b)
	}
	if w.nonceReads != 1 || w.typedSignatures != 2 {
		t.Fatalf("read/sign counts: %d %d", w.nonceReads, w.typedSignatures)
	}
}
