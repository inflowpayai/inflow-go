package seller

import (
	"context"
	"errors"
	"slices"

	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
)

type SchemeRegistration struct {
	Network foundation.Network
	Server  foundation.SchemeNetworkServer
}

type RegistrationOptions struct {
	Schemes []string
	// MeteredScheme supplies the upstream upto server. Keeping this optional avoids
	// compiling Ethereum dependencies into applications accepting only fixed prices.
	MeteredScheme foundation.SchemeNetworkServer
}

func (c *Client) SchemeRegistrations(ctx context.Context, options RegistrationOptions) ([]SchemeRegistration, error) {
	config, err := c.Config(ctx)
	if err != nil {
		return nil, err
	}
	registrations := []SchemeRegistration{}
	add := func(scheme, network, method string) {
		if !selected(options.Schemes, scheme) {
			return
		}
		if method == "" {
			method = foundation.SDKDefaultAssetTransferMethod
		}
		for _, registration := range registrations {
			if registration.Network == foundation.Network(network) && registration.Server.Scheme() == scheme {
				server := registration.Server.(*passthrough)
				if !slices.Contains(server.methods, method) {
					server.methods = append(server.methods, method)
				}
				return
			}
		}
		registrations = append(registrations, SchemeRegistration{Network: foundation.Network(network), Server: &passthrough{scheme: scheme, methods: []string{method}}})
	}
	for _, asset := range config.Assets {
		add(x402.SchemeExact, asset.Network, asset.AssetTransferMethod)
		if permit2(asset) {
			add(x402.SchemeExact, asset.Network, "permit2")
		}
		if slices.Contains(options.Schemes, x402.SchemeUpto) && meteredKind(config, asset) != nil {
			add(x402.SchemeUpto, asset.Network, "permit2")
		}
	}
	for _, method := range config.PaymentMethods {
		add(method.Scheme, method.Network, stringField(method.Extra, "assetTransferMethod"))
	}
	for i := range registrations {
		if registrations[i].Server.Scheme() == x402.SchemeUpto {
			if options.MeteredScheme == nil || options.MeteredScheme.Scheme() != x402.SchemeUpto {
				return nil, errors.New("upto registration requires an upstream metered scheme; supply RegistrationOptions.MeteredScheme")
			}
			registrations[i].Server = options.MeteredScheme
		}
	}
	return registrations, nil
}

type passthrough struct {
	scheme  string
	methods []string
}

func (s *passthrough) Scheme() string                     { return s.scheme }
func (s *passthrough) DefaultAssetTransferMethod() string { return s.methods[0] }
func (s *passthrough) PaymentFlows() map[string]foundation.PaymentFlowConfig {
	flows := map[string]foundation.PaymentFlowConfig{}
	for _, method := range s.methods {
		flows[method] = foundation.PaymentFlowConfig{Supported: []foundation.PaymentFlowName{foundation.PaymentFlowAuthorization}, Default: foundation.PaymentFlowAuthorization}
	}
	return flows
}

func (s *passthrough) ParsePrice(price foundation.Price, _ foundation.Network) (foundation.AssetAmount, error) {
	if amount, ok := price.(foundation.AssetAmount); ok {
		return amount, nil
	}
	if fields, ok := price.(map[string]any); ok {
		asset, assetOK := fields["asset"].(string)
		amount, amountOK := fields["amount"].(string)
		if assetOK && amountOK {
			extra, _ := fields["extra"].(map[string]any)
			return foundation.AssetAmount{Asset: asset, Amount: amount, Extra: extra}, nil
		}
	}
	return foundation.AssetAmount{}, errors.New("expected an asset and atomic amount; use Client.Accepts or Client.Route to resolve the price")
}

func (s *passthrough) EnhancePaymentRequirements(_ context.Context, requirements foundation.PaymentRequirements, _ foundation.SupportedKind, _ []string) (foundation.PaymentRequirements, error) {
	return requirements, nil
}
