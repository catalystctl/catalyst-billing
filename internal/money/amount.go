// Package money provides exact monetary arithmetic for the billing core.
//
// An Amount is an integer number of minor units together with an explicit
// Currency. Binary floating point is never used anywhere in this package.
// Percentage, tax and proration factors are exact rationals; converting a
// rational to an Amount always requires an explicit Rounding mode and reports
// overflow instead of silently truncating.
//
// Operations between different currencies fail. There is no implicit
// conversion: adding a supported currency means publishing explicit prices.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

var (
	// ErrCurrencyMismatch is returned when an operation mixes currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in int64 minor units.
	ErrOverflow = errors.New("money: amount overflow")
	// ErrInvalidAmount is returned for malformed or uninitialised amounts.
	ErrInvalidAmount = errors.New("money: invalid amount")
	// ErrInvalidCurrency is returned for malformed currency definitions.
	ErrInvalidCurrency = errors.New("money: invalid currency")
)

// Amount is a signed quantity of minor units in a single currency.
// The zero value is not a valid amount: it carries no currency.
type Amount struct {
	minor int64
	cur   Currency
}

// New constructs an Amount from minor units.
func New(minor int64, cur Currency) Amount { return Amount{minor: minor, cur: cur} }

// Zero returns a zero amount in cur.
func Zero(cur Currency) Amount { return Amount{minor: 0, cur: cur} }

// Minor returns the raw minor-unit count. Only use this for persistence and
// transport; all arithmetic should go through this package.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.cur }

// IsValid reports whether the amount carries a currency.
func (a Amount) IsValid() bool { return a.cur.IsValid() }

// IsZero reports whether the amount is zero. Invalid amounts are never zero.
func (a Amount) IsZero() bool { return a.IsValid() && a.minor == 0 }

// IsNegative reports whether the amount is below zero.
func (a Amount) IsNegative() bool { return a.minor < 0 }

// IsPositive reports whether the amount is above zero.
func (a Amount) IsPositive() bool { return a.minor > 0 }

// Sign returns -1, 0 or +1.
func (a Amount) Sign() int {
	switch {
	case a.minor < 0:
		return -1
	case a.minor > 0:
		return 1
	default:
		return 0
	}
}

func (a Amount) check() error {
	if !a.IsValid() {
		return fmt.Errorf("%w: no currency set", ErrInvalidAmount)
	}
	return nil
}

func (a Amount) sameCurrency(b Amount) error {
	if err := a.check(); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	if !a.cur.Equal(b.cur) {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.cur, b.cur)
	}
	return nil
}

// Add returns a+b. Both amounts must share a currency.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	sum := new(big.Int).Add(big.NewInt(a.minor), big.NewInt(b.minor))
	if !sum.IsInt64() {
		return Amount{}, fmt.Errorf("%w: %s + %s", ErrOverflow, a, b)
	}
	return Amount{minor: sum.Int64(), cur: a.cur}, nil
}

// Sub returns a-b. Both amounts must share a currency.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	diff := new(big.Int).Sub(big.NewInt(a.minor), big.NewInt(b.minor))
	if !diff.IsInt64() {
		return Amount{}, fmt.Errorf("%w: %s - %s", ErrOverflow, a, b)
	}
	return Amount{minor: diff.Int64(), cur: a.cur}, nil
}

// Neg returns -a. Negating the most negative int64 is reported as overflow.
func (a Amount) Neg() (Amount, error) {
	if err := a.check(); err != nil {
		return Amount{}, err
	}
	if a.minor == -1<<63 {
		return Amount{}, fmt.Errorf("%w: negating %d", ErrOverflow, a.minor)
	}
	return Amount{minor: -a.minor, cur: a.cur}, nil
}

// Abs returns |a|.
func (a Amount) Abs() (Amount, error) {
	if a.minor >= 0 {
		if err := a.check(); err != nil {
			return Amount{}, err
		}
		return a, nil
	}
	return a.Neg()
}

// MulInt multiplies by an integer scalar, reporting overflow.
func (a Amount) MulInt(n int64) (Amount, error) {
	if err := a.check(); err != nil {
		return Amount{}, err
	}
	p := new(big.Int).Mul(big.NewInt(a.minor), big.NewInt(n))
	if !p.IsInt64() {
		return Amount{}, fmt.Errorf("%w: %s * %d", ErrOverflow, a, n)
	}
	return Amount{minor: p.Int64(), cur: a.cur}, nil
}

// Cmp compares two amounts of the same currency.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := a.sameCurrency(b); err != nil {
		return 0, err
	}
	switch {
	case a.minor < b.minor:
		return -1, nil
	case a.minor > b.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether two amounts are the same value in the same currency.
// Invalid amounts are never equal to anything, including themselves.
func (a Amount) Equal(b Amount) bool {
	if !a.IsValid() || !b.IsValid() {
		return false
	}
	return a.cur.Equal(b.cur) && a.minor == b.minor
}

// String formats the amount as a decimal string with the currency's scale,
// e.g. "-12.34" for -1234 minor units of a 2-scale currency.
func (a Amount) String() string {
	if !a.IsValid() {
		return "<invalid amount>"
	}
	neg := a.minor < 0
	var u uint64
	if neg {
		u = uint64(-(a.minor + 1)) + 1
	} else {
		u = uint64(a.minor)
	}
	digits := strconv.FormatUint(u, 10)
	scale := int(a.cur.scale)
	if scale == 0 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	out := digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// Format renders the amount with an explicit currency code, e.g. "12.34 USD".
func (a Amount) Format() string {
	if !a.IsValid() {
		return "<invalid amount>"
	}
	return a.String() + " " + a.cur.code
}

// Parse converts a decimal string into an Amount in cur.
//
// It accepts an optional leading '-', at least one digit in the integer or
// fractional part, and at most cur.Scale() fractional digits. Anything that
// would require rounding (too many fractional digits), grouping separators,
// exponents or currency symbols is rejected: callers must round explicitly so
// that no value is ever silently changed.
func Parse(s string, cur Currency) (Amount, error) {
	if !cur.IsValid() {
		return Amount{}, fmt.Errorf("%w: no currency", ErrInvalidAmount)
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Amount{}, fmt.Errorf("%w: empty string", ErrInvalidAmount)
	}
	neg := false
	switch trimmed[0] {
	case '-':
		neg = true
		trimmed = trimmed[1:]
	case '+':
		return Amount{}, fmt.Errorf("%w: %q: explicit plus sign not allowed", ErrInvalidAmount, s)
	}
	intPart, fracPart := trimmed, ""
	if i := strings.IndexByte(trimmed, '.'); i >= 0 {
		intPart, fracPart = trimmed[:i], trimmed[i+1:]
		if fracPart == "" {
			return Amount{}, fmt.Errorf("%w: %q: trailing decimal point", ErrInvalidAmount, s)
		}
	}
	if intPart == "" && fracPart == "" {
		return Amount{}, fmt.Errorf("%w: %q: no digits", ErrInvalidAmount, s)
	}
	for _, part := range []string{intPart, fracPart} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return Amount{}, fmt.Errorf("%w: %q: unexpected character %q", ErrInvalidAmount, s, r)
			}
		}
	}
	scale := int(cur.scale)
	if len(fracPart) > scale {
		return Amount{}, fmt.Errorf("%w: %q has %d fractional digits, currency %s allows %d", ErrInvalidAmount, s, len(fracPart), cur.code, scale)
	}
	if intPart == "" {
		intPart = "0"
	}
	digits := intPart + fracPart + strings.Repeat("0", scale-len(fracPart))
	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q does not fit in int64 minor units: %v", ErrOverflow, s, err)
	}
	if neg {
		minor = -minor
	}
	return Amount{minor: minor, cur: cur}, nil
}

// MustParse is Parse for static values; it panics on error.
func MustParse(s string, cur Currency) Amount {
	a, err := Parse(s, cur)
	if err != nil {
		panic(err)
	}
	return a
}
