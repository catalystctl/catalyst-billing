// Package staff implements the operator read surface.
//
// It is a read package on purpose. Every query here is a SELECT, and the mutations live
// with the domain services that own the invariants. That split is what makes the reads
// cheap to add: a new list is a new query, not a new state machine.
//
// Three rules run through the whole package:
//
//   - Money never leaves its currency. A customer can owe money in two currencies, and
//     summing them would be arithmetic on unrelated quantities, so every balance is a list
//     per currency.
//   - Derived values are derived here, not read from a column. "Overdue" is
//     status = 'open' AND due_at < now(); storing it would make it wrong the moment nobody
//     ran the job.
//   - A list is keyset-paginated. OFFSET repeats and skips rows when something is inserted
//     while an operator scrolls, and the skipped row is a real invoice.
package staff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("staff: not found")

// ErrInvalidQuery is returned for a filter the caller got wrong.
var ErrInvalidQuery = errors.New("staff: invalid query")

// Service reads staff-scoped data.
type Service struct {
	pool *store.Pool
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs the read service.
func New(pool *store.Pool) *Service {
	return &Service{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// CurrencyOf resolves a persisted currency code.
//
// A code outside the ISO table is reconstructed at scale 2, which is what the schema's
// CHECK allows. An unknown currency is not an error: refusing to render an invoice because
// its currency is unusual would be worse than rendering it at the default scale.
func CurrencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// Amount pairs a currency with a minor-unit total, for a wire shape that cannot be summed
// across currencies by accident.
type Amount struct {
	Minor    int64
	Currency money.Currency
}

// Amounts converts a minor total and a currency code.
func Amounts(minor int64, code string) (Amount, error) {
	cur, err := CurrencyOf(code)
	if err != nil {
		return Amount{}, err
	}
	return Amount{Minor: minor, Currency: cur}, nil
}

// SortDirection is the order of a list.
type SortDirection string

const (
	SortAsc  SortDirection = "asc"
	SortDesc SortDirection = "desc"
)

// CursorOf renders the pagination cursor for a row.
func CursorOf(sortKey time.Time, id string) string {
	return paginate.Encode(sortKey.UTC().Format(time.RFC3339Nano), id)
}

// decodeTimeCursor parses a cursor whose sort key is a timestamp.
func decodeTimeCursor(cursor string) (time.Time, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	if sortKey == "" {
		return time.Time{}, id, nil
	}
	when, err := time.Parse(time.RFC3339Nano, sortKey)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad timestamp in cursor", paginate.ErrInvalidCursor)
	}
	return when, id, nil
}

// pageLimit clamps a requested page size.
func pageLimit(limit int) int { return paginate.Clamp(limit) }

// queryBuilder accumulates a WHERE clause and its arguments.
//
// It exists so that "add a filter" cannot accidentally renumber a parameter: the builder
// owns the numbering, so a new condition is one call and the SQL stays readable.
type queryBuilder struct {
	args  []any
	where []string
}

func (b *queryBuilder) add(clause string, value any) {
	b.args = append(b.args, value)
	b.where = append(b.where, fmt.Sprintf(clause, len(b.args)))
}

// addRaw adds a clause with no parameter.
func (b *queryBuilder) addRaw(clause string) { b.where = append(b.where, clause) }

// whereClause renders the WHERE body, or a tautology when nothing was added.
func (b *queryBuilder) whereClause() string {
	if len(b.where) == 0 {
		return "true"
	}
	return strings.Join(b.where, " AND ")
}

// nextArg appends a value and returns its placeholder.
func (b *queryBuilder) nextArg(value any) string {
	b.args = append(b.args, value)
	return fmt.Sprintf("$%d", len(b.args))
}

// inClause adds an "= ANY" condition for a list of ids.
func (b *queryBuilder) inClause(column string, values []string) {
	if len(values) == 0 {
		return
	}
	b.add(column+" = ANY($%d)", values)
}

// isUniqueViolation reports whether an error is a uniqueness conflict.
func isUniqueViolation(err error) bool { return store.IsUniqueViolation(err) }

// receivableFor reads the subledger receivable for a customer in one currency.
//
// It exists so a staff screen can cross-check the invoice columns against the ledger. A
// disagreement means something is wrong, and showing both is how an operator sees it
// instead of trusting a number in doubt.
func receivableFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, customerID string, cur money.Currency) (money.Amount, error) {
	var minor int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_minor - credit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2 AND currency = $3`,
		customerID, ledger.AccountReceivable, cur.Code()).Scan(&minor)
	if err != nil {
		return money.Amount{}, fmt.Errorf("staff: read receivable: %w", err)
	}
	return money.New(minor, cur), nil
}
