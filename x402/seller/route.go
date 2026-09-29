package seller

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/inflowpayai/inflow-go/x402"
	"github.com/x402-foundation/x402/go/v2/extensions/eip2612gassponsor"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
)

type RouteOptions struct {
	AcceptsOptions
	// AssetTransferMethod is empty for configured defaults, or permit2 for explicit selection.
	AssetTransferMethod string
}

func (c *Client) Route(ctx context.Context, options RouteOptions) (xhttp.RouteConfig, error) {
	if options.AssetTransferMethod != "" && options.AssetTransferMethod != "permit2" {
		return xhttp.RouteConfig{}, errors.New("asset transfer method must be permit2 or empty")
	}
	config, err := c.Config(ctx)
	if err != nil {
		return xhttp.RouteConfig{}, err
	}
	offers, err := buildOffers(config, options.AcceptsOptions, options.AssetTransferMethod)
	if err != nil {
		return xhttp.RouteConfig{}, err
	}
	route := xhttp.RouteConfig{Accepts: offers}
	permitOffers := []xhttp.PaymentOption{}
	eip2612, eip7702 := true, true
	for _, offer := range offers {
		if offer.Extra["assetTransferMethod"] != "permit2" {
			continue
		}
		if offer.Scheme != x402.SchemeExact || !strings.HasPrefix(string(offer.Network), "eip155:") || !strings.EqualFold(stringField(offer.Extra, "permit2Proxy"), x402.Permit2Proxy) {
			return route, nil
		}
		permitOffers = append(permitOffers, offer)
		eip2612 = eip2612 && offer.Extra["supportsEip2612"] == true && stringField(offer.Extra, "name") != "" && stringField(offer.Extra, "version") != ""
		eip7702 = eip7702 && offer.Extra["supportsEip7702"] == true
	}
	if len(permitOffers) == 0 || (!eip2612 && !eip7702) {
		return route, nil
	}
	supported, err := c.RefreshSupported(ctx)
	if err != nil {
		return xhttp.RouteConfig{}, err
	}
	if eip2612 && slices.Contains(supported.Extensions, "eip2612GasSponsoring") && supportsOffers(supported, permitOffers, false) {
		route.Extensions = eip2612gassponsor.DeclareEip2612GasSponsoringExtension()
	} else if eip7702 && slices.Contains(supported.Extensions, x402.InflowEip7702GasSponsoring) && supportsOffers(supported, permitOffers, true) {
		route.Extensions = x402.DeclareSponsorship()
	}
	return route, nil
}

func supportsOffers(supported x402.SupportedResponse, offers []xhttp.PaymentOption, eip7702 bool) bool {
	for _, offer := range offers {
		found := slices.ContainsFunc(supported.Kinds, func(kind x402.SupportedKind) bool {
			return kind.X402Version == 2 && kind.Scheme == offer.Scheme && kind.Network == string(offer.Network) && (!eip7702 || kind.Extra["supportsEip7702"] == true)
		})
		if !found {
			return false
		}
	}
	return true
}
