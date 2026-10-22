// Package growth implements the two growth extensions the panel needs: an affiliate
// programme (attribution, commission, payout) and Discord notifications.
//
// The rules that shape it:
//
//   - A commission rate is an exact rational parsed with money.ParseRatio, and the
//     commission itself is produced by money.ApplyRate with an explicit rounding mode.
//     There is no float anywhere on this path: 10% of an odd amount must produce the same
//     minor units every time it is recomputed, because a payout report that disagrees
//     with itself by one cent per order is a support ticket per affiliate per month.
//
//   - Attribution is a row, and its uniqueness is the guard. One attribution per customer
//     and one commission per order are partial unique indexes (migration 0024), so a
//     replayed "order paid" event conflicts and becomes a no-op instead of a second
//     commission. The service never relies on "have I done this already?".
//
//   - Attribution only ever attaches to a customer created by the signup it is part of.
//     A registration that presents an email that already has an account is refused
//     (ErrExistingAccount) rather than silently absorbing the established account into a
//     referral, which would pay an affiliate for a customer they did not bring.
//
//   - The Discord webhook URL is a credential. Notify reads it; nothing else returns it.
//     PublicConfig exists so a settings screen can render "configured / not configured"
//     without ever putting the secret in a response body.
package growth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors are sentinels so an HTTP handler maps them to a stable problem code without
// matching on message text.
var (
	// ErrNotFound is returned when a record does not exist, or belongs to someone else.
	ErrNotFound = errors.New("growth: not found")
	// ErrUnknownCode is returned when a referral code does not resolve to an enabled
	// affiliate. It is deliberately the same error for "no such code" and "disabled
	// code", so a probe cannot enumerate retired affiliates.
	ErrUnknownCode = errors.New("growth: unknown referral code")
	// ErrCodeTaken is returned when an affiliate code is already in use.
	ErrCodeTaken = errors.New("growth: referral code is already in use")
	// ErrAlreadyAttributed is returned when the customer already has an attribution.
	ErrAlreadyAttributed = errors.New("growth: customer is already attributed")
	// ErrExistingAccount is returned when attribution is asked for a customer that
	// existed before the current transaction. That is the "register with an existing
	// email" case: the account is not merged into the referral.
	ErrExistingAccount = errors.New("growth: the account already existed before this signup")
	// ErrSelfReferral is returned when an affiliate's own customer account is attributed
	// to their own code.
	ErrSelfReferral = errors.New("growth: an affiliate cannot refer themselves")
	// ErrAffiliateDisabled is returned when a retired affiliate is asked to earn.
	ErrAffiliateDisabled = errors.New("growth: the affiliate is not enabled")
	// ErrInvalidRate is returned for a rate that is not a rational in [0, 1].
	ErrInvalidRate = errors.New("growth: invalid commission rate")
	// ErrNothingToPay is returned by a payout when the outstanding balance is zero.
	ErrNothingToPay = errors.New("growth: nothing to pay out")
	// ErrInvalidInput is returned for a malformed request.
	ErrInvalidInput = errors.New("growth: invalid input")
)

// Querier is the subset of pgx both a transaction and the pool satisfy.
//
// Attribution and commission take a pgx.Tx (they must commit with the change that
// caused them); the read surface takes whatever the caller has.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the affiliate read/write surface.
type Store struct {
	pool *store.Pool
}

// New constructs a Store. A nil pool is allowed for the pure-computation tests; every
// database method then fails with ErrNotFound rather than panicking.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// Balance is one currency's earnings for an affiliate.
type Balance struct {
	Currency string
	// Owed is earned and not yet paid out.
	Owed money.Amount
	// Paid is lifetime paid out.
	Paid money.Amount
}

// Affiliate is one affiliate and what the panel shows about them.
type Affiliate struct {
	ID             string
	UserID         string
	Email          string
	Code           string
	CommissionRate string
	PayoutMethod   string
	Enabled        bool
	Balances       []Balance
	// ReferredCount is how many customers this affiliate brought.
	ReferredCount int
	CreatedAt     string
}

// Referral is one attribution or one commission.
//
// OrderID is empty on an attribution row (written at signup) and set on a commission row
// (written when the order's invoice was paid).
type Referral struct {
	ID           string
	AffiliateID  string
	CustomerID   string
	OrderID      string
	Commission   money.Amount
	Currency     string
	Rate         string
	Status       string
	AttributedAt string
	PaidAt       string
	// Duplicate reports that this call found an existing row rather than creating one.
	// It is the expected outcome of a replayed order-paid event, not an error.
	Duplicate bool
}

// PayoutLine is one affiliate's outstanding balance in one currency.
type PayoutLine struct {
	AffiliateID  string
	Code         string
	UserID       string
	Email        string
	Currency     string
	Owed         money.Amount
	LifetimePaid money.Amount
	// PendingCommissions is the number of commission rows that make up Owed.
	PendingCommissions int
}

// PaidOut is the result of settling one affiliate's balance in one currency.
type PaidOut struct {
	AffiliateID string
	Currency    string
	Amount      money.Amount
	// Commissions is how many referral rows were marked paid.
	Commissions int
}

// ---------------------------------------------------------------- helpers --

// currencyOf resolves a persisted currency code. A code outside the ISO table is
// reconstructed at scale 2, which is what the schema's default assumes.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// NormalizeCode canonicalises a referral code the way the table stores it.
//
// Codes are upper-case in the database (there is a CHECK), so the reader normalises
// rather than relying on the writer: a link copied from an email client that lower-cased
// it must still attribute.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// ParseRate parses a commission rate and returns it with its canonical rational form.
//
// "10%", "0.1" and "1/10" all become "1/10". Storing the canonical form matters: the
// payout report recomputes nothing, but a future recomputation of a commission from the
// stored rate must land on the same minor units, and that is only true if the stored
// text is the exact rational rather than a decimal that happened to parse.
func ParseRate(text string) (money.Ratio, string, error) {
	r, err := money.ParseRatio(text)
	if err != nil {
		return money.Ratio{}, "", fmt.Errorf("%w: %q", ErrInvalidRate, text)
	}
	if r.IsNegative() {
		return money.Ratio{}, "", fmt.Errorf("%w: %q is negative", ErrInvalidRate, text)
	}
	if cmp := r.Rat().Cmp(money.One().Rat()); cmp > 0 {
		return money.Ratio{}, "", fmt.Errorf("%w: %q exceeds 100%%", ErrInvalidRate, text)
	}
	return r, r.Rat().RatString(), nil
}

// Commission computes the commission on an amount at an exact rational rate.
//
// Rounding is half-up on the exact product, which is the commercial convention and the
// same mode the invoice pricer uses for a customer-facing total. The point is not which
// mode is "right" but that it is explicit: money.ApplyRate cannot round implicitly.
func Commission(amount money.Amount, rate string) (money.Amount, error) {
	r, _, err := ParseRate(rate)
	if err != nil {
		return money.Amount{}, err
	}
	if !amount.IsValid() {
		return money.Amount{}, fmt.Errorf("%w: no currency", ErrInvalidInput)
	}
	out, err := money.ApplyRate(amount, r, money.RoundHalfUp)
	if err != nil {
		return money.Amount{}, fmt.Errorf("growth: compute commission: %w", err)
	}
	return out, nil
}
