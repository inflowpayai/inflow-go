package buyer

import (
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/inflowpayai/inflow-go/mpp"
)

// MethodLimits restricts charge credentials created by a configured Method, not
// direct Client calls. Empty amount strings and nil allowlists impose no limit;
// "0" caps the amount at zero and non-nil empty allowlists reject every value.
type MethodLimits struct {
	// MaxAmount uses currency units. InFlow requires Currencies; CARD uses two
	// decimal places; Tempo requires Currencies and MaxAmountDecimals.
	MaxAmount string
	// MaxAtomicAmount uses base units for Tempo or cents for CARD, not InFlow.
	MaxAtomicAmount    string
	MaxAmountDecimals  *int
	Currencies         []string
	ExpectedRecipients []string
	AllowedChainIDs    []int64
}

var limitDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
var limitInteger = regexp.MustCompile(`^[0-9]+$`)

// Callers validate the decimal syntax before converting; base ten is explicit
// so leading zeros never select Go's octal integer syntax.
func limitAmount(value string) *big.Rat {
	whole, fraction, _ := strings.Cut(value, ".")
	numerator, _ := new(big.Int).SetString(whole+fraction, 10)
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
	return new(big.Rat).SetFrac(numerator, denominator)
}

func newMethodLimits(name, intent string, values []MethodLimits) (func(mpp.Challenge) error, error) {
	if len(values) > 1 {
		return nil, errors.New("provide at most one MethodLimits value")
	}
	l := MethodLimits{}
	if len(values) == 1 {
		l = values[0]
	}
	configured := l.MaxAmount != "" || l.MaxAtomicAmount != "" || l.MaxAmountDecimals != nil || l.Currencies != nil || l.ExpectedRecipients != nil || l.AllowedChainIDs != nil
	if !configured {
		return func(mpp.Challenge) error { return nil }, nil
	}
	if intent != mpp.IntentCharge {
		return nil, errors.New("MPP limits apply only to charge intents")
	}
	if name != mpp.MethodTempo && (l.MaxAmountDecimals != nil || l.AllowedChainIDs != nil) {
		return nil, errors.New("MaxAmountDecimals and AllowedChainIDs apply only to Tempo")
	}
	if name == mpp.MethodInflow && l.MaxAtomicAmount != "" {
		return nil, errors.New("InFlow amounts use currency units; use MaxAmount")
	}
	if name == mpp.MethodCard && (l.Currencies != nil || l.ExpectedRecipients != nil) {
		return nil, errors.New("CARD supports amount limits only")
	}
	l.Currencies = slices.Clone(l.Currencies)
	l.ExpectedRecipients = slices.Clone(l.ExpectedRecipients)
	l.AllowedChainIDs = slices.Clone(l.AllowedChainIDs)
	for _, list := range [][]string{l.Currencies, l.ExpectedRecipients} {
		for i, value := range list {
			if value == "" {
				return nil, errors.New("MPP allowlists require nonempty strings")
			}
			if name == mpp.MethodTempo {
				list[i] = strings.ToLower(value)
			}
		}
	}
	for _, chain := range l.AllowedChainIDs {
		if chain < 0 || chain > 9007199254740991 {
			return nil, errors.New("AllowedChainIDs must contain nonnegative safe integers")
		}
	}
	decimals := 2
	if l.MaxAmountDecimals != nil {
		decimals = *l.MaxAmountDecimals
		if decimals < 0 || decimals > 255 {
			return nil, errors.New("MaxAmountDecimals must be between 0 and 255")
		}
	}
	if name == mpp.MethodTempo && (l.MaxAmount != "" || l.MaxAtomicAmount != "") && l.Currencies == nil {
		return nil, errors.New("Tempo amount limits require Currencies")
	}
	var maximum *big.Rat
	if l.MaxAmount != "" {
		if !limitDecimal.MatchString(l.MaxAmount) {
			return nil, errors.New("MaxAmount requires a nonnegative decimal string")
		}
		maximum = limitAmount(l.MaxAmount)
		if name == mpp.MethodInflow {
			if l.Currencies == nil {
				return nil, errors.New("MaxAmount requires Currencies")
			}
		} else {
			if name == mpp.MethodTempo && l.MaxAmountDecimals == nil {
				return nil, errors.New("Tempo MaxAmount requires MaxAmountDecimals")
			}
			scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
			maximum.Mul(maximum, new(big.Rat).SetInt(scale))
			if !maximum.IsInt() {
				return nil, errors.New("MaxAmount exceeds currency precision")
			}
		}
	}
	var atomicMaximum *big.Rat
	if l.MaxAtomicAmount != "" {
		if !limitInteger.MatchString(l.MaxAtomicAmount) {
			return nil, errors.New("MaxAtomicAmount requires a nonnegative integer string")
		}
		atomicMaximum = limitAmount(l.MaxAtomicAmount)
	}
	return func(challenge mpp.Challenge) error {
		value, err := mpp.Decode(challenge.Request)
		if err != nil {
			return err
		}
		// Decode produces JSON values only. TempoRequest also carries the common
		// amount, currency and recipient fields used by InFlow and CARD limits.
		raw, _ := json.Marshal(value)
		var request mpp.TempoRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		validAmount := limitInteger.MatchString(request.Amount)
		if name == mpp.MethodInflow {
			validAmount = limitDecimal.MatchString(request.Amount)
		}
		if !validAmount {
			return errors.New("invalid MPP charge amount")
		}
		currency, recipient := request.Currency, request.Recipient
		if name == mpp.MethodTempo {
			currency, recipient = strings.ToLower(currency), strings.ToLower(recipient)
		}
		if l.Currencies != nil && !slices.Contains(l.Currencies, currency) {
			return errors.New("MPP charge currency is not allowed")
		}
		if l.AllowedChainIDs != nil && (request.MethodDetails == nil || request.MethodDetails.ChainID == nil || !slices.Contains(l.AllowedChainIDs, *request.MethodDetails.ChainID)) {
			return errors.New("MPP charge chain is not allowed")
		}
		if l.ExpectedRecipients != nil {
			if !slices.Contains(l.ExpectedRecipients, recipient) {
				return errors.New("MPP charge recipient is not allowed")
			}
			if name == mpp.MethodTempo && request.MethodDetails != nil {
				for _, split := range request.MethodDetails.Splits {
					if !slices.Contains(l.ExpectedRecipients, strings.ToLower(split.Recipient)) {
						return errors.New("MPP split recipient is not allowed")
					}
				}
			}
		}
		amount := limitAmount(request.Amount)
		if maximum != nil && amount.Cmp(maximum) > 0 {
			return errors.New("MPP charge amount exceeds MaxAmount")
		}
		if atomicMaximum != nil && amount.Cmp(atomicMaximum) > 0 {
			return errors.New("MPP charge amount exceeds MaxAtomicAmount")
		}
		return nil
	}, nil
}
