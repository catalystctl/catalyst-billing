// Package commerce owns the commercial configuration a shop needs and a ledger does not:
// coupons, jurisdiction tax rates, configured currencies and the customer's cart.
//
// The package is deliberately a *configuration* package. It decides what a discount is
// worth, which rate applies in a jurisdiction, how a currency renders and what a cart
// currently totals — and it never writes a document. Issuing an invoice stays with
// `billing`, taking an order stays with `orders`. That separation is what keeps the
// append-only money path reviewable: nothing here can post to the ledger.
//
// Four rules shape the code, each of them a decision recorded in 0020_commerce.sql:
//
//  1. **A coupon is a price modifier with a life, not a discount column.** Validation
//     returns an exact amount or a typed refusal; redemption counts uses under a row lock
//     so a concurrent checkout cannot overshoot `max_uses`.
//  2. **A percentage is an exact rational.** Discounts are computed with
//     `money.Ratio`/`money.ApplyRate`; there is no float anywhere in this package.
//  3. **Tax is a dated rate table resolved per jurisdiction.** The pricer is a
//     `billing.Pricer`, so the invoice path does not change; the rate, the jurisdiction
//     and any exemption reason travel in `billing.TaxEvidence`.
//  4. **A cart is a draft, not a second pricing engine.** A cart line stores what was
//     chosen; every total re-prices from the catalog, and the cart's currency is fixed
//     once it holds an item because there is no exchange rate to convert with.
//
// Every write takes a caller-supplied transaction (`...Tx`) so the HTTP layer can write
// the change and its audit row in one commit, exactly like `catalog`.
package commerce

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors the HTTP layer maps to stable codes. They are values rather than strings so a
// handler cannot silently change the code a client branches on.
var (
	// ErrNotFound is returned when the record does not exist (or is not the caller's).
	ErrNotFound = errors.New("commerce: not found")
	// ErrInvalidInput is returned for a request the domain refuses to interpret.
	ErrInvalidInput = errors.New("commerce: invalid input")
	// ErrCodeTaken is returned when a coupon code is already in use. Codes are unique
	// case-insensitively, because a customer typing "save10" means SAVE10.
	ErrCodeTaken = errors.New("commerce: coupon code already in use")
	// ErrInUse is returned when a record still has dependants (a currency with issued
	// prices, a tax rate superseded by a later one).
	ErrInUse = errors.New("commerce: still in use")
	// ErrExpired is returned when a coupon is outside its `starts_at`/`expires_at`
	// window. It covers "not started yet" too: both mean "not usable now".
	ErrExpired = errors.New("commerce: coupon outside its validity window")
	// ErrExhausted is returned when a coupon has reached `max_uses` or
	// `max_uses_per_user`.
	ErrExhausted = errors.New("commerce: coupon usage limit reached")
	// ErrNotApplicable is returned when a coupon exists and is live but does not apply to
	// this product, this part of the price, or this currency.
	ErrNotApplicable = errors.New("commerce: coupon does not apply")
	// ErrAlreadyInCart is returned when a product that is sold one at a time is added a
	// second time. It has a code of its own rather than reusing ErrNotApplicable because
	// the two refusals need different words and different next steps: "your cart is in
	// another currency" is fixed by emptying the cart, and "it is already in your cart" is
	// fixed by going to the cart. Merging instead would be worse than either: the cart
	// would hold a line the checkout refuses (`quantity_not_allowed`), which is how a
	// double-clicked "Order this plan" became an unpayable cart.
	ErrAlreadyInCart = errors.New("commerce: already in the cart")
)

// querier is the subset of pgx that both a pool and a transaction satisfy.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the commerce data access.
type Store struct {
	pool *store.Pool
	// Now is the clock used for window and effective-date decisions. Injectable so a test
	// can pin "the day the rate changed" instead of waiting for it.
	Now func() time.Time
}

// New constructs a Store.
func New(pool *store.Pool) *Store {
	return &Store{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// Pool exposes the underlying pool for callers that need their own transaction.
func (s *Store) Pool() *store.Pool { return s.pool }

// now returns the store's clock, defaulting when it was not set.
func (s *Store) now() time.Time {
	if s == nil || s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// stamp normalises an optional caller-supplied instant.
func stamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// sqlState exposes the SQLSTATE of a Postgres error. Matching on the code rather than the
// message keeps the mapping stable across Postgres versions and locales.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isUniqueViolation(err error) bool     { return sqlState(err) == "23505" }
func isForeignKeyViolation(err error) bool { return sqlState(err) == "23503" }
func isCheckViolation(err error) bool      { return sqlState(err) == "23514" }

// invalid wraps a message as ErrInvalidInput so handlers can map it without inspecting
// the text.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// notApplicable wraps a refusal reason.
func notApplicable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotApplicable, fmt.Sprintf(format, args...))
}
