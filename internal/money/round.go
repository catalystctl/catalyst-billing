package money

import (
	"fmt"
	"math/big"
)

// Rounding selects how an exact rational becomes whole minor units.
//
// The default for customer-facing totals is RoundHalfUp (the commercial
// convention). RoundHalfEven is offered where statistical bias matters.
// Whatever a caller picks is recorded in the invoice snapshot, so a later
// re-computation reproduces the same total.
type Rounding int

const (
	// RoundHalfUp rounds halves away from zero: 2.5 -> 3, -2.5 -> -3.
	RoundHalfUp Rounding = iota
	// RoundHalfEven rounds halves to the nearest even minor unit (banker's).
	RoundHalfEven
	// RoundTowardZero truncates: 2.9 -> 2, -2.9 -> -2.
	RoundTowardZero
	// RoundAwayFromZero rounds any fraction away from zero: 2.1 -> 3, -2.1 -> -3.
	RoundAwayFromZero
	// RoundFloor rounds toward negative infinity.
	RoundFloor
	// RoundCeil rounds toward positive infinity.
	RoundCeil
)

func (r Rounding) String() string {
	switch r {
	case RoundHalfUp:
		return "half_up"
	case RoundHalfEven:
		return "half_even"
	case RoundTowardZero:
		return "toward_zero"
	case RoundAwayFromZero:
		return "away_from_zero"
	case RoundFloor:
		return "floor"
	case RoundCeil:
		return "ceil"
	default:
		return fmt.Sprintf("rounding(%d)", int(r))
	}
}

// Valid reports whether r is a known rounding mode.
func (r Rounding) Valid() bool { return r >= RoundHalfUp && r <= RoundCeil }

// roundRat rounds an exact rational to an integer using mode.
func roundRat(x *big.Rat, mode Rounding) (*big.Int, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAmount, mode)
	}
	num, den := x.Num(), x.Denom() // den > 0
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, den, rem) // truncated toward zero; rem keeps num's sign
	if rem.Sign() == 0 {
		return q, nil
	}
	away := func() *big.Int {
		if num.Sign() < 0 {
			return q.Sub(q, big.NewInt(1))
		}
		return q.Add(q, big.NewInt(1))
	}
	switch mode {
	case RoundTowardZero:
		return q, nil
	case RoundAwayFromZero:
		return away(), nil
	case RoundFloor:
		if num.Sign() < 0 {
			return q.Sub(q, big.NewInt(1)), nil
		}
		return q, nil
	case RoundCeil:
		if num.Sign() > 0 {
			return q.Add(q, big.NewInt(1)), nil
		}
		return q, nil
	case RoundHalfUp, RoundHalfEven:
		twiceRem := new(big.Int).Abs(rem)
		twiceRem.Lsh(twiceRem, 1)
		cmp := twiceRem.Cmp(den)
		switch {
		case cmp > 0:
			return away(), nil
		case cmp < 0:
			return q, nil
		default: // exactly one half
			if mode == RoundHalfUp {
				return away(), nil
			}
			if q.Bit(0) == 1 { // odd -> move to even
				return away(), nil
			}
			return q, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrInvalidAmount, mode)
}
