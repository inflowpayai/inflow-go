package buyer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
)

// Select returns a managed requirement, or nil when external-wallet signing is needed.
func (c *Client) Select(ctx context.Context, required x402.PaymentRequired) (*x402.PaymentRequirements, error) {
	if required.X402Version != 2 {
		return nil, &Error{Code: "invalid-input"}
	}
	required, err := snapshot(required)
	if err != nil {
		return nil, err
	}
	supported, err := c.Supported(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []x402.PaymentRequirements
	for _, r := range required.Accepts {
		if slices.Contains(c.options.Prefer, r.Scheme) && supports(supported, r) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	candidates, err = c.policies(ctx, candidates)
	if err != nil {
		return nil, err
	}
	for _, scheme := range c.options.Prefer {
		var matches []x402.PaymentRequirements
		for _, r := range candidates {
			if r.Scheme == scheme && supports(supported, r) {
				matches = append(matches, r)
			}
		}
		if len(matches) == 0 {
			continue
		}
		if scheme == x402.SchemeBalance && len(matches) > 1 {
			balances := c.balances(ctx)
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			for _, r := range matches {
				name, _ := r.Extra["assetName"].(string)
				amount, ok := new(big.Int).SetString(r.Amount, 10)
				if available := balances[name]; available != nil && ok && available.Cmp(amount) >= 0 {
					return &r, nil
				}
			}
		}
		return &matches[0], nil
	}
	return nil, &Error{Code: "unsupported-capability"}
}

func (c *Client) policies(ctx context.Context, candidates []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
	var err error
	for _, policy := range c.options.Policies {
		candidates, err = policy(ctx, candidates)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, &Error{Code: "unsupported-capability"}
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return snapshot(candidates)
}

var decimalPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

func (c *Client) balances(ctx context.Context) map[string]*big.Int {
	raw, err := c.api.Do(ctx, platform.Request{Method: http.MethodGet, Path: "/v1/balances", Retries: 3})
	if err != nil {
		return nil
	}
	value, err := decode[struct {
		Balances []struct {
			Currency  string `json:"currency"`
			Available string `json:"available"`
		} `json:"balances"`
	}](raw.Body)
	if err != nil {
		return nil
	}
	result := make(map[string]*big.Int)
	for _, b := range value.Balances {
		value := strings.TrimSpace(b.Available)
		if !decimalPattern.MatchString(value) {
			continue
		}
		negative := strings.HasPrefix(value, "-")
		integer, fraction, _ := strings.Cut(strings.TrimPrefix(value, "-"), ".")
		fraction = (fraction + strings.Repeat("0", x402.InflowAmountScale))[:x402.InflowAmountScale]
		amount, _ := new(big.Int).SetString(integer+fraction, 10)
		if negative {
			amount.Neg(amount)
		}
		result[b.Currency] = amount
	}
	return result
}

// Sign prefers managed signing; External is used only when no managed requirement matches.
// Failed one-shot waits attempt approval cleanup for at most five seconds without masking the original error.
func (c *Client) Sign(ctx context.Context, required x402.PaymentRequired, options SignOptions) (EncodedPayment, error) {
	required, err := snapshot(required)
	if err != nil {
		return EncodedPayment{}, err
	}
	if options.PaymentID != "" && !x402.ValidatePaymentID(options.PaymentID) {
		return EncodedPayment{}, &Error{Code: "invalid-input"}
	}
	selected, err := c.Select(ctx, required)
	if err != nil {
		return EncodedPayment{}, err
	}
	if selected != nil {
		required.Accepts = []x402.PaymentRequirements{*selected}
		if err = c.before(ctx, required); err != nil {
			return EncodedPayment{}, err
		}
		value, err := c.sign(ctx, required, options)
		if err != nil {
			return c.failure(ctx, required, err)
		}
		return value, nil
	}
	if c.options.External == nil {
		return EncodedPayment{}, &Error{Code: "unsupported-capability"}
	}
	candidates, err := c.policies(ctx, required.Accepts)
	if err != nil {
		return EncodedPayment{}, err
	}
	chosen, err := c.options.External.SelectPaymentRequirements(candidates)
	if err != nil {
		return EncodedPayment{}, err
	}
	required.Accepts = []x402.PaymentRequirements{chosen}
	if err = c.before(ctx, required); err != nil {
		return EncodedPayment{}, err
	}
	value, err := c.external(ctx, required)
	if err != nil {
		return c.failure(ctx, required, err)
	}
	return value, nil
}

func (c *Client) external(ctx context.Context, required x402.PaymentRequired) (EncodedPayment, error) {
	isolated, err := snapshot(required)
	if err != nil {
		return EncodedPayment{}, err
	}
	payload, err := c.options.External.CreatePaymentPayload(ctx, isolated.Accepts[0], isolated.Resource, isolated.Extensions)
	if err != nil {
		return EncodedPayment{}, err
	}
	if declaration := x402.ReadPaymentIdentifier(required.Extensions[x402.PaymentIdentifier]); declaration != nil {
		// Node's external path cannot supply an InFlow identifier through SignOptions.
		// Optional identifiers are derived by the facilitator; required declarations reject this path.
		var mandatory bool
		_ = json.Unmarshal(declaration.Info["required"], &mandatory)
		if mandatory {
			return EncodedPayment{}, &Error{Code: "unsupported-capability"}
		}
	}
	if err = c.after(ctx, required, payload); err != nil {
		return EncodedPayment{}, err
	}
	return encoded(payload)
}

func encoded(payload x402.PaymentPayload) (EncodedPayment, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return EncodedPayment{}, &Error{Code: "invalid-response"}
	}
	return EncodedPayment{EncodedPayload: base64.StdEncoding.EncodeToString(data), PaymentPayload: payload}, nil
}

func (c *Client) failure(ctx context.Context, required x402.PaymentRequired, cause error) (EncodedPayment, error) {
	if ctx.Err() != nil {
		return EncodedPayment{}, cause
	}
	for _, hook := range c.options.Hooks {
		if hook.Failure != nil {
			input, err := snapshot(required)
			if err != nil {
				return EncodedPayment{}, err
			}
			payload, err := hook.Failure(ctx, input, cause)
			if err != nil {
				return EncodedPayment{}, err
			}
			if payload != nil {
				return encoded(*payload)
			}
		}
	}
	return EncodedPayment{}, cause
}
