package money

import (
	"fmt"
	"math/big"
	"sort"
)

// Allocate splits total across len(weights) parts in proportion to weights and
// returns parts whose sum is exactly total.
//
// The method is largest-remainder (Hamilton): every part receives the floor of
// its exact share, then the leftover minor units go one each to the parts with
// the largest fractional remainders, ties broken by ascending index. Because
// the leftover is always smaller than the number of parts, no part is ever more
// than one minor unit away from its exact share, and the residual is allocated
// deterministically rather than dropped.
//
// Negative totals are allocated by magnitude and then negated, so the sign of
// every part matches the sign of the total and the sum invariant still holds.
//
// Weights must be non-negative and must not all be zero.
func Allocate(total Amount, weights []int64) ([]Amount, error) {
	if err := total.check(); err != nil {
		return nil, err
	}
	n := len(weights)
	if n == 0 {
		return nil, fmt.Errorf("%w: allocate needs at least one weight", ErrInvalidAmount)
	}
	weightSum := new(big.Int)
	for i, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: weight[%d] is negative (%d)", ErrInvalidAmount, i, w)
		}
		weightSum.Add(weightSum, big.NewInt(w))
	}
	if weightSum.Sign() == 0 {
		return nil, fmt.Errorf("%w: all weights are zero", ErrInvalidAmount)
	}

	out := make([]Amount, n)
	if total.minor == 0 {
		for i := range out {
			out[i] = Amount{minor: 0, cur: total.cur}
		}
		return out, nil
	}

	neg := total.minor < 0
	magnitude := new(big.Int).Abs(big.NewInt(total.minor))
	parts := make([]*big.Int, n)
	reminders := make([]*big.Int, n)
	allocated := new(big.Int)
	for i, w := range weights {
		num := new(big.Int).Mul(magnitude, big.NewInt(w))
		q := new(big.Int)
		r := new(big.Int)
		q.QuoRem(num, weightSum, r)
		parts[i] = q
		reminders[i] = r
		allocated.Add(allocated, q)
	}
	leftover := new(big.Int).Sub(magnitude, allocated)
	if !leftover.IsInt64() {
		return nil, fmt.Errorf("%w: inconsistent allocation residual", ErrOverflow)
	}
	left := leftover.Int64()
	if left < 0 || left > int64(n) {
		return nil, fmt.Errorf("%w: inconsistent allocation residual", ErrOverflow)
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if c := reminders[order[a]].Cmp(reminders[order[b]]); c != 0 {
			return c > 0
		}
		return order[a] < order[b]
	})
	one := big.NewInt(1)
	for k := int64(0); k < left; k++ {
		parts[order[k]].Add(parts[order[k]], one)
	}
	for i := range out {
		if !parts[i].IsInt64() {
			return nil, fmt.Errorf("%w: allocation part does not fit int64", ErrOverflow)
		}
		v := parts[i].Int64()
		if neg {
			v = -v
		}
		out[i] = Amount{minor: v, cur: total.cur}
	}
	return out, nil
}

// Split divides total into n equal parts that sum exactly to total, allocating
// the residual to the earliest parts.
func Split(total Amount, n int) ([]Amount, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: split count must be positive, got %d", ErrInvalidAmount, n)
	}
	weights := make([]int64, n)
	for i := range weights {
		weights[i] = 1
	}
	return Allocate(total, weights)
}

// Sum adds amounts of one currency, returning a zero amount in that currency
// when the slice is empty and cur.
func Sum(cur Currency, amounts ...Amount) (Amount, error) {
	total := Zero(cur)
	for _, a := range amounts {
		next, err := total.Add(a)
		if err != nil {
			return Amount{}, err
		}
		total = next
	}
	return total, nil
}
