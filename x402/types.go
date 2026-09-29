// Package x402 provides x402 V2 types and InFlow payment extensions.
package x402

import foundation "github.com/x402-foundation/x402/go/v2"

type PaymentRequirements = foundation.PaymentRequirements
type PaymentPayload = foundation.PaymentPayload
type PaymentRequired = foundation.PaymentRequired
type ResourceInfo = foundation.ResourceInfo
type SupportedKind = foundation.SupportedKind
type SupportedResponse = foundation.SupportedResponse
type VerifyResponse = foundation.VerifyResponse
type SettleResponse = foundation.SettleResponse

const (
	Version                = 2
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	SchemeBalance          = "balance"
	SchemeExact            = "exact"
	SchemeUpto             = "upto"
	SchemeInstrument       = "instrument"
	NetworkInflow          = "inflow:1"
	InflowAmountScale      = 18
	Permit2Proxy           = "0x402085c248EeA27D92E8b30b2C58ed07f9E20001"
	Permit2                = "0x000000000022D473030F116dDEE9F6B43aC78BA3"
)

type BuyerSupportedResponse struct {
	Kinds []SupportedKind `json:"kinds"`
}

type ConfigResponse struct {
	Assets         []AssetInfo         `json:"assets"`
	PaymentMethods []PaymentMethodInfo `json:"paymentMethods"`
	SellerID       string              `json:"sellerId"`
	Supported      []SupportedKind     `json:"supported"`
	Wallets        []WalletInfo        `json:"wallets"`
}

type AssetInfo struct {
	AssetTransferMethod string `json:"assetTransferMethod,omitempty"`
	AssetID             string `json:"assetId"`
	AssetName           string `json:"assetName"`
	Blockchain          string `json:"blockchain"`
	Currency            string `json:"currency"`
	Decimals            int    `json:"decimals"`
	Network             string `json:"network"`
	Permit2Proxy        string `json:"permit2Proxy,omitempty"`
	SupportsEip2612     *bool  `json:"supportsEip2612,omitempty"`
	SupportsEip7702     *bool  `json:"supportsEip7702,omitempty"`
	TokenName           string `json:"tokenName,omitempty"`
	TokenVersion        string `json:"tokenVersion,omitempty"`
}

type WalletInfo struct {
	Address    string `json:"address"`
	Blockchain string `json:"blockchain"`
	FeePayer   string `json:"feePayer,omitempty"`
	Network    string `json:"network"`
}

type PaymentMethodInfo struct {
	Scheme   string         `json:"scheme"`
	Network  string         `json:"network"`
	PayTo    string         `json:"payTo"`
	Decimals int            `json:"decimals"`
	Extra    map[string]any `json:"extra,omitempty"`
}

const InflowEip7702GasSponsoring = "inflowEip7702GasSponsoring"

type SponsorshipInfo struct {
	Version                string `json:"version"`
	SponsorshipID          string `json:"sponsorshipId"`
	Signature              string `json:"signature"`
	AuthorizationSignature string `json:"authorizationSignature,omitempty"`
}

func DeclareSponsorship() map[string]any {
	return map[string]any{InflowEip7702GasSponsoring: map[string]any{"info": map[string]any{"version": "1"}}}
}
