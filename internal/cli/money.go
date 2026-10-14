package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// currencyExponents mirrors the API's ISO-4217 minor-unit overrides
// (api/billkit/services/currency.py, `_MINOR_UNITS`), which the console keeps
// a copy of too (frontend/packages/shared/src/money.ts). Every currency not
// listed uses defaultCurrencyExponent, exactly as the API does: that is the
// scale the API itself applies when it turns amount_cents into a provider
// amount, so it is the honest one to show. money_test.go reads the Python
// source in a monorepo checkout and fails if the two disagree.
var currencyExponents = map[string]int{
	"ISK": 0,
	"JPY": 0,
	"KRW": 0,
	"BHD": 3,
	"KWD": 3,
	"OMR": 3,
	"TND": 3,
}

// defaultCurrencyExponent is the API's `_DEFAULT_MINOR_UNITS`.
const defaultCurrencyExponent = 2

// formatMoney renders an amount in minor units for a human: `EUR 19.99`,
// `JPY 1999`, `BHD 1.999`. The ISO code comes first and no symbol or
// locale grouping is used, because this is printed in a confirmation a user
// reads before money moves, and `€19.99` vs `19,99 €` is exactly the kind of
// ambiguity that line exists to remove.
//
// It is integer arithmetic throughout, so no amount is ever rounded. A
// currency that is not a three-letter code is not guessed at: the raw
// number is shown and called minor units, which cannot be misread.
func formatMoney(minor int64, currency string) string {
	code := strings.ToUpper(strings.TrimSpace(currency))
	if !isCurrencyCode(code) {
		if code == "" {
			return fmt.Sprintf("%d minor units (currency unknown)", minor)
		}
		return fmt.Sprintf("%d minor units of %q", minor, currency)
	}
	exp, ok := currencyExponents[code]
	if !ok {
		exp = defaultCurrencyExponent
	}
	return code + " " + scaleMinor(minor, exp)
}

// scaleMinor places the decimal point exp digits from the right.
func scaleMinor(minor int64, exp int) string {
	sign := ""
	digits := strconv.FormatInt(minor, 10)
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if exp <= 0 {
		return sign + digits
	}
	if len(digits) <= exp {
		digits = strings.Repeat("0", exp-len(digits)+1) + digits
	}
	cut := len(digits) - exp
	return sign + digits[:cut] + "." + digits[cut:]
}

func isCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
