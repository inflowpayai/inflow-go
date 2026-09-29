package x402

import (
	"regexp"
	"strings"
)

var plainDecimal = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

// NormalizeDecimalString removes insignificant zeroes without floating-point conversion.
// Values outside plain decimal notation are returned unchanged.
func NormalizeDecimalString(value string) string {
	if !plainDecimal.MatchString(value) {
		return value
	}
	negative := strings.HasPrefix(value, "-")
	integer, fraction, _ := strings.Cut(strings.TrimPrefix(value, "-"), ".")
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if integer == "0" && fraction == "" {
		return "0"
	}
	result := integer
	if fraction != "" {
		result += "." + fraction
	}
	if negative {
		result = "-" + result
	}
	return result
}
