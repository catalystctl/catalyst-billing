package tax

// How a tax decision is said out loud.
//
// A storefront has to render two things next to a price — the percentage, and the phrase that says
// whether it is inside the price or added to it — and both must agree with what the invoice will
// charge. Neither is a place for floating point: `0.0725 * 100` in a browser is 7.249999999999999,
// and a page that prints "7.249999999999999% sales tax" has made a rounding error visible to every
// customer. So the wording and the percentage are rendered here, from the exact rational, and the
// page prints a string.

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/catalyst/billing/internal/money"
)

// DisclosureLabel renders the sentence a price may be shown with.
//
// The cases are deliberately distinct: a zero that comes from a rule (reverse charge, no nexus, a
// staff exemption) is not the same statement as "we do not know", and a page that shows "0% tax" for
// either would be saying something the invoice does not say.
func DisclosureLabel(d Decision) (string, error) {
	switch d.Kind {
	case DecisionRate:
		percent, err := PercentLabel(d.Rate)
		if err != nil {
			return "", err
		}
		switch {
		case d.Inclusive:
			return "incl. " + percent + " " + taxWord(d.Jurisdiction), nil
		default:
			return "plus " + percent + " " + taxWord(d.Jurisdiction), nil
		}
	case DecisionReverseCharge:
		return "VAT reverse charge", nil
	case DecisionNoNexus:
		return "no sales tax", nil
	case DecisionStaffExempt:
		return "tax exempt", nil
	default:
		// No rate is configured for the destination, and no rule decided otherwise. The honest
		// sentence is that the price shown is not the final one.
		return "tax is calculated at checkout", nil
	}
}

// PercentLabel renders an exact rational rate as a percentage string: "0.19" → "19%",
// "0.0725" → "7.25%", "1/3" → "1/3 (33.3333%)". A rate whose decimal expansion does not terminate
// is shown as the fraction as well, because printing a truncated percentage would claim a precision
// the rate does not have.
func PercentLabel(rate string) (string, error) {
	ratio, err := money.ParseRatio(strings.TrimSpace(rate))
	if err != nil {
		return "", fmt.Errorf("tax: rate %q is not an exact number: %w", rate, err)
	}
	hundred, err := money.NewRatio(100, 1)
	if err != nil {
		return "", err
	}
	percent := new(big.Rat).Mul(ratio.Rat(), hundred.Rat())
	text, exact := exactDecimal(percent)
	if exact {
		return text + "%", nil
	}
	// Four decimal places is enough for a display, and the exact fraction is printed next to it so
	// the rounded figure is never the only number on the page.
	rounded := new(big.Rat).Set(percent)
	rounded.Mul(rounded, new(big.Rat).SetInt64(10000))
	scaled := new(big.Int).Quo(rounded.Num(), rounded.Denom())
	digits := scaled.String()
	for len(digits) <= 4 {
		digits = "0" + digits
	}
	whole := digits[:len(digits)-4]
	fraction := strings.TrimRight(digits[len(digits)-4:], "0")
	display := whole
	if fraction != "" {
		display += "." + fraction
	}
	return fmt.Sprintf("%s%% (%s%%)", percent.RatString(), display), nil
}

// taxWord names the tax a jurisdiction actually has. "VAT" on a California invoice is the kind of
// detail that makes a customer distrust the whole document.
func taxWord(jurisdiction string) string {
	if strings.HasPrefix(jurisdiction, "US") {
		return "sales tax"
	}
	return "VAT"
}

// exactDecimal renders a rational as a decimal when the denominator is a product of 2s and 5s, and
// reports whether it could. It is the same rule the rate table uses for its own rendering: a human
// typed "0.19" and expects to read it back, while "19/100" is identical and reads like a machine's
// answer.
func exactDecimal(r *big.Rat) (string, bool) {
	den := new(big.Int).Set(r.Denom())
	twos, fives := 0, 0
	two, five := big.NewInt(2), big.NewInt(5)
	zero, rem := big.NewInt(0), new(big.Int)
	for {
		if rem.Mod(den, two).Cmp(zero) == 0 {
			den.Div(den, two)
			twos++
			continue
		}
		if rem.Mod(den, five).Cmp(zero) == 0 {
			den.Div(den, five)
			fives++
			continue
		}
		break
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	places := twos
	if fives > places {
		places = fives
	}
	if places == 0 {
		return r.Num().String(), true
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	digits := scaled.Num().String()
	negative := strings.HasPrefix(digits, "-")
	if negative {
		digits = digits[1:]
	}
	for len(digits) <= places {
		digits = "0" + digits
	}
	out := digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	out = strings.TrimRight(out, "0")
	out = strings.TrimSuffix(out, ".")
	if negative {
		out = "-" + out
	}
	return out, true
}
