package money

import (
	"fmt"
	"math/big"
	"strings"
)

// Rat is an exact arbitrary-precision rational. It is exported so callers can
// compose ratios without dropping to binary floating point.
type Rat = big.Rat

// Ratio is an exact rational factor used for percentages, tax rates and
// proration. It never loses precision; precision is only given up at the single
// point where a Ratio is applied to an Amount with an explicit Rounding mode.
type Ratio struct {
	r *big.Rat
}

// One returns the ratio 1.
func One() Ratio { return Ratio{r: big.NewRat(1, 1)} }

// ZeroRatio returns the ratio 0.
func ZeroRatio() Ratio { return Ratio{r: new(big.Rat)} }

// NewRatio returns num/den exactly. den must not be zero.
func NewRatio(num, den int64) (Ratio, error) {
	if den == 0 {
		return Ratio{}, fmt.Errorf("%w: zero denominator", ErrInvalidAmount)
	}
	return Ratio{r: big.NewRat(num, den)}, nil
}

// MustRatio is NewRatio for static values; it panics on error.
func MustRatio(num, den int64) Ratio {
	r, err := NewRatio(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// ParseRatio parses an exact ratio from "0.2", "1/12", "7.5%" or "0.075".
// A trailing '%' divides by 100. No binary floating point is involved.
func ParseRatio(s string) (Ratio, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return Ratio{}, fmt.Errorf("%w: empty ratio", ErrInvalidAmount)
	}
	scale := big.NewRat(1, 1)
	if strings.HasSuffix(t, "%") {
		t = strings.TrimSpace(strings.TrimSuffix(t, "%"))
		scale = big.NewRat(1, 100)
	}
	r, ok := new(big.Rat).SetString(t)
	if !ok {
		return Ratio{}, fmt.Errorf("%w: cannot parse ratio %q", ErrInvalidAmount, s)
	}
	return Ratio{r: r.Mul(r, scale)}, nil
}

// MustParseRatio is ParseRatio for static values; it panics on error.
func MustParseRatio(s string) Ratio {
	r, err := ParseRatio(s)
	if err != nil {
		panic(err)
	}
	return r
}

// IsZero reports whether the ratio is zero.
func (r Ratio) IsZero() bool { return r.r == nil || r.r.Sign() == 0 }

// IsNegative reports whether the ratio is below zero.
func (r Ratio) IsNegative() bool { return r.r != nil && r.r.Sign() < 0 }

// Mul returns r*o exactly.
func (r Ratio) Mul(o Ratio) Ratio {
	out := new(big.Rat).Mul(r.rat(), o.rat())
	return Ratio{r: out}
}

// Complement returns 1-r, useful for "price minus discount".
func (r Ratio) Complement() Ratio {
	return Ratio{r: new(big.Rat).Sub(big.NewRat(1, 1), r.rat())}
}

// Float64 is for display and metrics only. Never use it to compute an Amount.
func (r Ratio) Float64() float64 {
	f, _ := r.rat().Float64()
	return f
}

func (r Ratio) rat() *big.Rat {
	if r.r == nil {
		return new(big.Rat)
	}
	return r.r
}

// RatioFromRat wraps an exact rational, copying it so later mutation of the
// argument cannot change the ratio.
func RatioFromRat(r *Rat) (Ratio, error) {
	if r == nil {
		return Ratio{}, fmt.Errorf("%w: nil rational", ErrInvalidAmount)
	}
	return Ratio{r: new(big.Rat).Set(r)}, nil
}

// Divide returns r/o exactly. Dividing by zero is an error rather than a panic
// or a silent zero, because both would corrupt a price.
func (r Ratio) Divide(o Ratio) (Ratio, error) {
	if o.IsZero() {
		return Ratio{}, fmt.Errorf("%w: division by zero ratio", ErrInvalidAmount)
	}
	return Ratio{r: new(big.Rat).Quo(r.rat(), o.rat())}, nil
}

// Rat returns a copy of the underlying rational.
func (r Ratio) Rat() *big.Rat { return new(big.Rat).Set(r.rat()) }

func (r Ratio) String() string { return r.rat().RatString() }

// ApplyRate multiplies a by r and rounds the exact result to whole minor units
// with the given mode. The rounding stage is therefore explicit at every call
// site, which is what makes invoice totals reproducible.
func ApplyRate(a Amount, r Ratio, mode Rounding) (Amount, error) {
	if err := a.check(); err != nil {
		return Amount{}, err
	}
	exact := new(big.Rat).Mul(new(big.Rat).SetInt64(a.minor), r.rat())
	q, err := roundRat(exact, mode)
	if err != nil {
		return Amount{}, err
	}
	if !q.IsInt64() {
		return Amount{}, fmt.Errorf("%w: %s * %s", ErrOverflow, a, r)
	}
	return Amount{minor: q.Int64(), cur: a.cur}, nil
}
