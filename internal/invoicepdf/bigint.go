package invoicepdf

import "math/big"

// bigInt helpers.
//
// They exist so the rate formatter can stay exact. big.Rat arithmetic is exact by
// construction; the only rounding is the truncation performed here, once, at the
// point of display.

func newBigInt(v *big.Int) *big.Int {
	return new(big.Int).Set(v)
}

// bigIntPow10 returns 10^n.
func bigIntPow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
