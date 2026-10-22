package invoicepdf

import (
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/money"
)

// FormatAmount renders an amount for the document without ever touching a float.
//
// The minor units are formatted as an integer, then the decimal mark is inserted
// at the position the currency's scale dictates, then the thousands separator is
// inserted from the right. Every step is string manipulation on digits that came
// straight from an int64, which is the only way to be certain that the number on
// the page is the number in the database.
//
// A negative amount — a credit note — keeps its sign in front of the currency
// symbol, so "-12.34 USD" reads as a negative amount rather than as a discount
// label that happens to start with a dash.
func FormatAmount(minor int64, code string) string {
	cur, ok := money.LookupCurrency(code)
	if !ok {
		// An unknown code is still rendered at scale 2, which is what the schema
		// default assumes, so a document never fails to print because of a code
		// the ISO table does not list.
		cur, _ = money.NewCurrency(code, 2)
	}
	return formatMinor(minor, cur.Scale(), code)
}

// formatMinor is FormatAmount with an explicit scale.
func formatMinor(minor int64, scale uint8, code string) string {
	digits := strconv.FormatInt(minor, 10)
	neg := strings.HasPrefix(digits, "-")
	if neg {
		digits = digits[1:]
	}

	// Pad so there is at least one digit before the mark.
	for int(scale) > 0 && len(digits) <= int(scale) {
		digits = "0" + digits
	}

	var whole, frac string
	if scale == 0 {
		whole = digits
	} else {
		cut := len(digits) - int(scale)
		whole, frac = digits[:cut], digits[cut:]
	}

	whole = groupDigits(whole)

	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	if code != "" {
		out += " " + code
	}
	return out
}

// groupDigits inserts a comma every three digits from the right.
//
// Grouping is presentation only. It is applied after the digits are fixed, so it
// can never change a value, and it is done here rather than through a formatter
// because a formatter is where a float usually sneaks in.
func groupDigits(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// FormatQuantity renders a milli-unit quantity as a human quantity.
//
// Quantities are stored in thousandths so that 1.5 GB is exact. The trailing
// zeros are trimmed because "1" reads better than "1.000" on an invoice line,
// and the trimming is on the fractional part only, so it cannot alter the
// integer part.
func FormatQuantity(milli int64) string {
	neg := milli < 0
	if neg {
		milli = -milli
	}
	whole := milli / 1000
	frac := milli % 1000
	if frac == 0 {
		if neg {
			return "-" + strconv.FormatInt(whole, 10)
		}
		return strconv.FormatInt(whole, 10)
	}
	digits := strconv.FormatInt(frac, 10)
	// Left-pad to three, then drop trailing zeros.
	for len(digits) < 3 {
		digits = "0" + digits
	}
	digits = strings.TrimRight(digits, "0")
	out := strconv.FormatInt(whole, 10) + "." + digits
	if neg {
		out = "-" + out
	}
	return out
}

// formatRate renders a tax rate string ("0.19", "1/5", "19%") as a percentage.
//
// The rate in the snapshot is an exact rational string, and it is the evidence a
// tax authority reads. It is therefore rendered by parsing the rational and
// dividing by 100 only for display, with the digits taken from integer
// arithmetic: a rate of 1/3 must not print as 33.33333333% and then be
// multiplied back.
func formatRate(rate string) string {
	r, err := money.ParseRatio(rate)
	if err != nil {
		return rate
	}
	// percent = r * 100, kept as an exact rational.
	hundred := money.MustRatio(100, 1)
	product := r.Mul(hundred)
	rat := product.Rat()

	// Render with at most four decimal places, trimming trailing zeros. A
	// repeating rational (1/3) is truncated rather than rounded up, so the
	// printed rate is never higher than the rate charged.
	scaled := newBigInt(rat.Num())
	scaled.Mul(scaled, bigIntPow10(4))
	scaled.Quo(scaled, rat.Denom())
	text := scaled.String()
	if len(text) <= 4 {
		text = strings.Repeat("0", 5-len(text)) + text
	}
	whole := text[:len(text)-4]
	frac := strings.TrimRight(text[len(text)-4:], "0")
	if frac != "" {
		whole += "." + frac
	}
	return whole + "%"
}
