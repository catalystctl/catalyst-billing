package growth

// Recurring affiliate commission (GRW-03).
//
// docs/parity-status.md recorded this as deliberately unpaid, with the reason stated
// plainly: "a renewal invoice has no signup to attribute". That is true of the *invoice*
// and false of the *customer* — the referral is recorded on the customer, once, at signup,
// and it stays true for every renewal they ever pay. So the attribution does not have to be
// found on the invoice; it has to be found on the customer the invoice bills.
//
// Four rules make a recurring commission as trustworthy as an order commission:
//
//   - **The rate is an exact rational.** The commission is money.ApplyRate on the exact
//     product with an explicit rounding mode, exactly as the order path computes it. A
//     renewal commission recomputed by a payout report must land on the same minor units,
//     which a float cannot promise.
//
//   - **It is recorded in the settlement transaction.** The call happens inside the
//     transaction that took the money, next to the order commission, so a payout report
//     never sees a paid renewal whose commission has not been written yet.
//
//   - **It is idempotent on the invoice id.** The unique index on
//     affiliate_recurring_commissions.invoice_id is the guard, not a "have I done this
//     already?" check in Go, so a replayed provider webhook inserts nothing and returns the
//     existing row with Duplicate set. The balance is incremented only on a genuine insert.
//
//   - **It is bounded by `affiliates.recurring_cycles`.** 0 means every renewal (a
//     deliberate choice, not a missing value); N means the first N renewals of one referred
//     customer earn a commission. The bound is a count of the rows that already exist for
//     that customer, so it is answerable from the ledger itself, and the attribution row is
//     locked while the count is taken so two concurrent renewals cannot both claim the last
//     slot.
//
// And one gate that makes the feature additive rather than a change in behaviour: with
// `affiliates.recurring` off — the default — this file writes nothing and returns
// ErrNotReferred, which is exactly what the settlement path saw before it existed.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

// Setting keys this file reads. They are declared in internal/settings/settings.go; the
// declaration owns the default, and the read below falls back to it when no row exists, so
// a fresh installation behaves like one whose operator has never opened the settings page.
const (
	settingGroupAffiliates = "affiliates"
	settingRecurring       = "recurring"
	settingRecurringCycles = "recurring_cycles"
)

// RenewalCommission is the outcome of a recurring-commission call.
//
// It embeds Referral so the settlement path can treat it like any other commission, and adds
// the two facts that only a renewal has: which invoice it was earned on, and which renewal
// of that customer it was. OrderID on the embedded Referral is always empty — a renewal has
// no order, which is the whole point.
type RenewalCommission struct {
	Referral
	// InvoiceID is the renewal invoice that earned the commission.
	InvoiceID string
	// RenewalIndex is 1 for the first renewal after the signup order, 2 for the second, and
	// so on. Together with affiliates.recurring_cycles it states the bound on the row.
	RenewalIndex int
}

// RecordSettledInvoiceCommission records whatever commission a paid invoice earns.
//
// It is the single call the settlement path needs, and it exists so the path does not have
// to know that there are two kinds of commission:
//
//   - an invoice raised from an order earns the first-order commission, on what the invoice
//     actually settled (the order total may have been adjusted before issue);
//   - a renewal invoice — one raised from a subscription with no order — earns the recurring
//     commission, attributed to the referral recorded on the customer.
//
// Both are no-ops (ErrNotReferred) for an invoice that is not paid, for a customer who was
// not referred, and — for a renewal — when `affiliates.recurring` is off. The caller treats
// ErrNotReferred as success, which is what makes this additive.
func RecordSettledInvoiceCommission(ctx context.Context, tx pgx.Tx, invoiceID string) (Referral, error) {
	first, err := RecordCommissionForPaidInvoice(ctx, tx, invoiceID)
	switch {
	case err == nil:
		return first, nil
	case errors.Is(err, ErrNotReferred):
		// Not an order invoice, or the customer was not referred. A renewal may still earn
		// a recurring commission; anything else falls out as another ErrNotReferred.
		renewal, rerr := RecordRecurringCommission(ctx, tx, invoiceID)
		if rerr != nil {
			return Referral{}, rerr
		}
		return renewal.Referral, nil
	default:
		return Referral{}, err
	}
}

// RecordRecurringCommission records the commission a renewal invoice earns.
//
// "Renewal" is a precise claim here, not a synonym for "any invoice with no order": the
// invoice must have been raised from a subscription. A manual invoice to a referred customer
// is a manual invoice — paying commission on it would turn every hand-written document into
// a marketing cost, and `affiliates.recurring` is a setting about renewals.
func RecordRecurringCommission(ctx context.Context, tx pgx.Tx, invoiceID string) (RenewalCommission, error) {
	if tx == nil {
		return RenewalCommission{}, fmt.Errorf("%w: commission needs a transaction", ErrInvalidInput)
	}
	if !ValidID(invoiceID) {
		return RenewalCommission{}, ErrNotFound
	}

	terms, err := recurringTerms(ctx, tx)
	if err != nil {
		return RenewalCommission{}, err
	}
	if !terms.Enabled {
		// The gate. With recurring commission off, a renewal behaves exactly as it did
		// before this file existed: nothing is written and nothing is owed.
		return RenewalCommission{}, ErrNotReferred
	}

	var (
		orderID        *string
		subscriptionID *string
		customerID     string
		status         string
		currency       string
		totalMinor     int64
	)
	err = tx.QueryRow(ctx, `
		SELECT order_id, subscription_id, customer_id, status, currency, total_minor
		FROM invoices WHERE id = $1 FOR SHARE`, invoiceID,
	).Scan(&orderID, &subscriptionID, &customerID, &status, &currency, &totalMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenewalCommission{}, ErrNotFound
	}
	if err != nil {
		return RenewalCommission{}, fmt.Errorf("growth: load invoice: %w", err)
	}
	if status != "paid" {
		// Commission is a liability the money has to support. An unpaid renewal earns
		// nothing, and saying so is not an error: the settlement path calls this on every
		// paid invoice, and a partially paid one is simply not a renewal yet.
		return RenewalCommission{}, ErrNotReferred
	}
	if orderID != nil {
		// A first order, not a renewal. RecordCommissionForPaidInvoice owns it; doing it
		// here as well would pay the affiliate twice for one invoice.
		return RenewalCommission{}, ErrNotReferred
	}
	if subscriptionID == nil {
		// No order and no subscription: a manual invoice. See the doc comment.
		return RenewalCommission{}, ErrNotReferred
	}

	// The attribution is on the customer, and it is the row this transaction locks. Locking
	// it is what makes the cycle bound exact: two renewals of the same customer cannot both
	// observe "one slot left".
	var (
		affiliateID string
		attributed  time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT affiliate_id, attributed_at FROM affiliate_referrals
		WHERE customer_id = $1 AND order_id IS NULL
		FOR UPDATE`, customerID).Scan(&affiliateID, &attributed)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenewalCommission{}, ErrNotReferred
	}
	if err != nil {
		return RenewalCommission{}, fmt.Errorf("growth: load attribution: %w", err)
	}

	var (
		rateText string
		enabled  bool
	)
	err = tx.QueryRow(ctx,
		`SELECT commission_rate, enabled FROM affiliates WHERE id = $1`, affiliateID,
	).Scan(&rateText, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenewalCommission{}, ErrNotReferred
	}
	if err != nil {
		return RenewalCommission{}, fmt.Errorf("growth: load affiliate: %w", err)
	}
	if !enabled {
		// Deliberately a no-op rather than ErrAffiliateDisabled: the order path fails the
		// settlement on a disabled affiliate, and a *renewal* must never fail because a
		// marketing payee was retired. The customer's money is not contingent on it.
		return RenewalCommission{}, ErrNotReferred
	}

	// The cycle bound. The count is of commission rows for this customer, which is the
	// ledger's own answer to "how many renewals has this referral already paid for".
	var prior int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_recurring_commissions WHERE customer_id = $1`,
		customerID).Scan(&prior); err != nil {
		return RenewalCommission{}, fmt.Errorf("growth: count renewal commissions: %w", err)
	}
	if terms.Cycles > 0 && prior >= terms.Cycles {
		return RenewalCommission{}, ErrNotReferred
	}
	renewalIndex := prior + 1

	_, canonical, err := ParseRate(rateText)
	if err != nil {
		return RenewalCommission{}, err
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return RenewalCommission{}, err
	}
	// Exact rational arithmetic; rounding happens once, here, on the exact product.
	commission, err := Commission(money.New(totalMinor, cur), canonical)
	if err != nil {
		return RenewalCommission{}, err
	}

	out := RenewalCommission{
		Referral: Referral{
			AffiliateID:  affiliateID,
			CustomerID:   customerID,
			Commission:   commission,
			Currency:     currency,
			Rate:         canonical,
			Status:       "pending",
			AttributedAt: attributed.UTC().Format(time.RFC3339),
		},
		InvoiceID:    invoiceID,
		RenewalIndex: renewalIndex,
	}

	var minor int64
	err = tx.QueryRow(ctx, `
		INSERT INTO affiliate_recurring_commissions
			(affiliate_id, customer_id, invoice_id, commission_minor, currency,
			 commission_rate, renewal_index, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
		ON CONFLICT (invoice_id) DO NOTHING
		RETURNING id`,
		affiliateID, customerID, invoiceID, commission.Minor(), currency, canonical,
		renewalIndex).Scan(&out.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Replay: this renewal has already been paid for. Return the row unchanged and do
		// not touch the balance, so a re-delivered webhook cannot inflate what is owed.
		out.Duplicate = true
		if err := tx.QueryRow(ctx, `
			SELECT id, commission_minor, commission_rate, renewal_index, status
			FROM affiliate_recurring_commissions WHERE invoice_id = $1`, invoiceID,
		).Scan(&out.ID, &minor, &out.Rate, &out.RenewalIndex, &out.Status); err != nil {
			return RenewalCommission{}, fmt.Errorf("growth: load existing renewal commission: %w", err)
		}
		out.Commission = money.New(minor, cur)
		return out, nil
	}
	if err != nil {
		return RenewalCommission{}, fmt.Errorf("growth: record renewal commission: %w", err)
	}

	// The materialised payable, incremented in the same transaction as the row it
	// summarises. A zero commission creates no balance row: nothing is owed.
	if commission.IsPositive() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO affiliate_balances (affiliate_id, currency, balance_minor)
			VALUES ($1, $2, $3)
			ON CONFLICT (affiliate_id, currency) DO UPDATE
				SET balance_minor = affiliate_balances.balance_minor + EXCLUDED.balance_minor,
				    updated_at = now()`,
			affiliateID, currency, commission.Minor()); err != nil {
			return RenewalCommission{}, fmt.Errorf("growth: update affiliate balance: %w", err)
		}
	}
	return out, nil
}

// RecurringTerms is the configured policy for recurring commission.
type RecurringTerms struct {
	// Enabled is affiliates.recurring. Off means no renewal earns anything.
	Enabled bool
	// Cycles is affiliates.recurring_cycles: how many renewals of one referred customer
	// earn a commission. 0 means every renewal.
	Cycles int
}

// recurringTerms reads the policy inside the caller's transaction.
//
// Reading it from the transaction rather than from a separately pooled settings store is
// deliberate: the policy that decides whether this payment pays a commission is then part of
// the same snapshot as the payment, so a settings change cannot land halfway through a
// settlement and be observed in one half of it. A missing row is the declared default, so a
// fresh installation needs no migration to be correct.
func recurringTerms(ctx context.Context, q Querier) (RecurringTerms, error) {
	enabled, err := settingBool(ctx, q, settingRecurring)
	if err != nil {
		return RecurringTerms{}, err
	}
	cycles, err := settingInt(ctx, q, settingRecurringCycles)
	if err != nil {
		return RecurringTerms{}, err
	}
	if cycles < 0 {
		// A negative bound is a configuration mistake, not "unlimited". Reading it as 0
		// would silently turn a typo into "pay every renewal for ever", which is the most
		// expensive possible interpretation.
		return RecurringTerms{}, fmt.Errorf("%w: affiliates.recurring_cycles is negative",
			ErrInvalidInput)
	}
	return RecurringTerms{Enabled: enabled, Cycles: int(cycles)}, nil
}

// settingBool reads one boolean setting from q, falling back to its declared default.
func settingBool(ctx context.Context, q Querier, key string) (bool, error) {
	raw, err := settingText(ctx, q, key)
	if err != nil {
		return false, err
	}
	v, perr := strconv.ParseBool(strings.TrimSpace(raw))
	if perr != nil {
		return false, fmt.Errorf("%w: affiliates.%s is not a boolean", ErrInvalidInput, key)
	}
	return v, nil
}

// settingInt reads one integer setting from q, falling back to its declared default.
func settingInt(ctx context.Context, q Querier, key string) (int64, error) {
	raw, err := settingText(ctx, q, key)
	if err != nil {
		return 0, err
	}
	v, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("%w: affiliates.%s is not an integer", ErrInvalidInput, key)
	}
	return v, nil
}

// settingText reads one setting's stored text, or its declared default when no row exists.
func settingText(ctx context.Context, q Querier, key string) (string, error) {
	def, ok := settings.Lookup(settingGroupAffiliates, key)
	if !ok {
		// The key is declared in internal/settings; a miss here means the declaration was
		// removed, which is a build-time mistake rather than a runtime one.
		return "", fmt.Errorf("%w: affiliates.%s is not a declared setting", ErrInvalidInput, key)
	}
	var stored string
	err := q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = $1 AND key = $2`,
		settingGroupAffiliates, key).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return def.Default, nil
	}
	if err != nil {
		return "", fmt.Errorf("growth: read setting affiliates.%s: %w", key, err)
	}
	return stored, nil
}
