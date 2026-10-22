package importer

import (
	"fmt"

	"github.com/catalyst/billing/internal/money"
)

// optionalAmount parses a column that may be absent.
//
// Absent and empty are the same thing here — the dump did not state the value — and the
// caller decides what that means. An empty string that should have been a number is still a
// rejection: `invalid_amount` is about a value that cannot be read, and an empty value is
// not a value.
func (r *run) optionalAmount(row *csvRow, file, key string, cur money.Currency) (money.Amount, bool) {
	text := row.get(file, key)
	if text == "" {
		return money.Zero(cur), true
	}
	amt, err := money.Parse(text, cur)
	if err != nil {
		r.reject(row, entityFor(file), row.get(file, "id"), CodeInvalidAmount,
			fmt.Sprintf("%s %q is not an exact amount in %s: %v", key, text, cur.Code(), err))
		return money.Amount{}, false
	}
	return amt, true
}
