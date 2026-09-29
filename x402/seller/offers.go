package seller

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
)

type PriceSpec struct {
	Amount   string
	Currency string
}

type AcceptsOptions struct {
	Price             PriceSpec
	MaxTimeoutSeconds int
	Schemes           []string
	Networks          []string
}

type PriceError struct{ Input string }

func (e *PriceError) Error() string {
	return fmt.Sprintf("invalid x402 price %q: use $0.01, 0.01 USDC, or an amount with Currency; at most 8 decimal places and no loss of asset precision", e.Input)
}

func (c *Client) Accepts(ctx context.Context, options AcceptsOptions) ([]xhttp.PaymentOption, error) {
	config, err := c.Config(ctx)
	if err != nil {
		return nil, err
	}
	return buildOffers(config, options, "")
}

var pricePattern = regexp.MustCompile(`^(\$)?([0-9]+)(?:\.([0-9]{1,8}))?(?:\s+([A-Z][A-Z0-9_]*))?$`)

func parsePrice(price PriceSpec) (string, string, string, error) {
	match := pricePattern.FindStringSubmatch(price.Amount)
	if match == nil || (match[1] != "" && match[4] != "") {
		return "", "", "", &PriceError{Input: price.Amount}
	}
	currency := match[4]
	if match[1] != "" {
		currency = "USD"
	}
	if price.Currency != "" {
		currency = price.Currency
	}
	if currency == "" {
		return "", "", "", &PriceError{Input: price.Amount}
	}
	return match[2], match[3], currency, nil
}

func atomicAmount(integer, fraction string, decimals int, original string) (string, error) {
	if decimals < 0 || decimals > 255 {
		return "", &PriceError{Input: original}
	}
	if len(fraction) > decimals {
		if strings.Trim(fraction[decimals:], "0") != "" {
			return "", &PriceError{Input: original}
		}
		fraction = fraction[:decimals]
	}
	value := strings.TrimLeft(integer+fraction+strings.Repeat("0", decimals-len(fraction)), "0")
	if value == "" {
		value = "0"
	}
	return value, nil
}

func selected(filter []string, value string) bool {
	return filter == nil || slices.Contains(filter, value)
}

func permit2(asset x402.AssetInfo) bool {
	return strings.HasPrefix(asset.Network, "eip155:") && strings.EqualFold(asset.Permit2Proxy, x402.Permit2Proxy)
}

func meteredKind(config x402.ConfigResponse, asset x402.AssetInfo) *x402.SupportedKind {
	if !strings.HasPrefix(asset.Network, "eip155:") || asset.Permit2Proxy == "" {
		return nil
	}
	for _, kind := range config.Supported {
		if kind.X402Version == 2 && kind.Scheme == x402.SchemeUpto && kind.Network == asset.Network && kind.Extra["assetTransferMethod"] == "permit2" && stringField(kind.Extra, "permit2Proxy") != "" && stringField(kind.Extra, "facilitatorAddress") != "" {
			return &kind
		}
	}
	return nil
}

func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func buildOffers(config x402.ConfigResponse, options AcceptsOptions, transfer string) ([]xhttp.PaymentOption, error) {
	integer, fraction, currency, err := parsePrice(options.Price)
	if err != nil {
		return nil, err
	}
	timeout := options.MaxTimeoutSeconds
	if timeout == 0 {
		timeout = 300
	}
	if timeout < 0 {
		return nil, fmt.Errorf("max timeout seconds must be positive")
	}
	offers := []xhttp.PaymentOption{}
	for _, wallet := range config.Wallets {
		for _, asset := range config.Assets {
			if asset.Blockchain != wallet.Blockchain || (currency != "USD" && asset.Currency != currency) || !selected(options.Networks, asset.Network) {
				continue
			}
			method := asset.AssetTransferMethod
			if transfer != "" {
				method = transfer
			}
			if selected(options.Schemes, x402.SchemeExact) && (transfer == "" || permit2(asset)) {
				amount, err := atomicAmount(integer, fraction, asset.Decimals, options.Price.Amount)
				if err != nil {
					return nil, err
				}
				offers = append(offers, chainOffer(wallet, asset, method, amount, timeout, x402.SchemeExact, nil))
			}
			if slices.Contains(options.Schemes, x402.SchemeUpto) {
				if kind := meteredKind(config, asset); kind != nil {
					amount, err := atomicAmount(integer, fraction, asset.Decimals, options.Price.Amount)
					if err != nil {
						return nil, err
					}
					offers = append(offers, chainOffer(wallet, asset, "permit2", amount, timeout, x402.SchemeUpto, kind.Extra))
				}
			}
		}
	}
	currencies := []string{currency}
	if currency == "USD" {
		currencies = []string{}
		for _, asset := range config.Assets {
			if !slices.Contains(currencies, asset.Currency) {
				currencies = append(currencies, asset.Currency)
			}
		}
	}
	for _, method := range config.PaymentMethods {
		if !selected(options.Schemes, method.Scheme) || !selected(options.Networks, method.Network) {
			continue
		}
		for _, currency := range currencies {
			amount, err := atomicAmount(integer, fraction, method.Decimals, options.Price.Amount)
			if err != nil {
				return nil, err
			}
			extra := map[string]any{}
			maps.Copy(extra, method.Extra)
			extra["assetName"] = currency
			offers = append(offers, xhttp.PaymentOption{Scheme: method.Scheme, Network: foundation.Network(method.Network), PayTo: method.PayTo, Price: map[string]any{"asset": currency, "amount": amount}, MaxTimeoutSeconds: timeout, Extra: extra})
		}
	}
	return offers, nil
}

func chainOffer(wallet x402.WalletInfo, asset x402.AssetInfo, method, amount string, timeout int, scheme string, additional map[string]any) xhttp.PaymentOption {
	extra := map[string]any{"assetName": asset.AssetName}
	for key, value := range map[string]string{"name": asset.TokenName, "version": asset.TokenVersion, "assetTransferMethod": method, "feePayer": wallet.FeePayer} {
		if value != "" {
			extra[key] = value
		}
	}
	if method == "permit2" {
		if asset.Permit2Proxy != "" {
			extra["permit2Proxy"] = asset.Permit2Proxy
		}
		if asset.SupportsEip2612 != nil && *asset.SupportsEip2612 {
			extra["supportsEip2612"] = true
		}
		if asset.SupportsEip7702 != nil && *asset.SupportsEip7702 {
			extra["supportsEip7702"] = true
		}
	}
	maps.Copy(extra, additional)
	return xhttp.PaymentOption{Scheme: scheme, Network: foundation.Network(asset.Network), PayTo: wallet.Address, Price: map[string]any{"asset": asset.AssetID, "amount": amount}, MaxTimeoutSeconds: timeout, Extra: extra}
}
