package growth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Recurring affiliate commission (GRW-03).
//
// The parity status used to say this was deliberately unpaid because "a renewal invoice has
// no signup to attribute". These tests assert the resolution: the attribution is on the
// customer, so a renewal can be attributed; the whole thing is gated on
// affiliates.recurring; the cycle bound is a count of the rows that already exist; and the
// guard against a replayed webhook is the unique index on the invoice id, not a check in Go.

// setAffiliateSetting writes one affiliates.* setting and restores the previous state when
// the test ends.
//
// The settings table is reference data: the harness never truncates it, so a test that
// leaves a row behind changes the meaning of every later test in the package. The cleanup
// deletes the row when there was none, which restores the code-side default.
func setAffiliateSetting(t *testing.T, pool *store.Pool, key, value string) {
	t.Helper()
	ctx := context.Background()
	var previous string
	err := pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'affiliates' AND key = $1`, key).Scan(&previous)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := pool.Exec(ctx, `
			INSERT INTO settings (group_key, key, value) VALUES ('affiliates', $1, $2)`,
			key, value); err != nil {
			t.Fatalf("set affiliates.%s: %v", key, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM settings WHERE group_key = 'affiliates' AND key = $1`, key)
		})
	case err != nil:
		t.Fatalf("read affiliates.%s: %v", key, err)
	default:
		if _, err := pool.Exec(ctx, `
			UPDATE settings SET value = $2, updated_at = now()
			WHERE group_key = 'affiliates' AND key = $1`, key, value); err != nil {
			t.Fatalf("set affiliates.%s: %v", key, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `
				UPDATE settings SET value = $2, updated_at = now()
				WHERE group_key = 'affiliates' AND key = $1`, key, previous)
		})
	}
}

// seedSubscription gives a customer an active subscription, so an invoice can be a renewal.
func seedSubscription(t *testing.T, pool *store.Pool, customerID string) string {
	t.Helper()
	ctx := context.Background()
	var productID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ($1, 'Recurring plan') RETURNING id`,
		"recurring-plan-"+customerID[:8]).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceVersionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	var subscriptionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now(), now() + interval '1 month',
		        now() + interval '1 month')
		RETURNING id`, customerID, priceVersionID).Scan(&subscriptionID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	return subscriptionID
}

// seedRenewalInvoice inserts a paid renewal invoice: raised from a subscription, with no
// order, which is exactly the shape the recurring path is about.
func seedRenewalInvoice(t *testing.T, pool *store.Pool, customerID, subscriptionID string, totalMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var invoiceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, subscription_id, period_key, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-R-' || gen_random_uuid()::text, $1, $2, $3, 'paid', 'USD',
		        $4, 0, $4, $4, now(), '{"schema_version":1}'::jsonb)
		RETURNING id`,
		customerID, subscriptionID, "period-"+fmt.Sprint(time.Now().UnixNano()),
		totalMinor).Scan(&invoiceID); err != nil {
		t.Fatalf("renewal invoice: %v", err)
	}
	return invoiceID
}

// seedManualInvoice inserts a paid invoice with no order and no subscription.
func seedManualInvoice(t *testing.T, pool *store.Pool, customerID string, totalMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var invoiceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-M-' || gen_random_uuid()::text, $1, 'paid', 'USD',
		        $2, 0, $2, $2, now(), '{"schema_version":1}'::jsonb)
		RETURNING id`, customerID, totalMinor).Scan(&invoiceID); err != nil {
		t.Fatalf("manual invoice: %v", err)
	}
	return invoiceID
}

// balanceOf reads the materialised balance for an affiliate in USD.
func balanceOf(t *testing.T, pool *store.Pool, affiliateID string) int64 {
	t.Helper()
	var balance int64
	err := pool.QueryRow(context.Background(), `
		SELECT balance_minor FROM affiliate_balances
		WHERE affiliate_id = $1 AND currency = 'USD'`, affiliateID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("load balance: %v", err)
	}
	return balance
}

// ---------------------------------------------------------------- the gate --

// With affiliates.recurring off — the default — a renewal earns nothing and writes nothing.
// This is the property that makes the feature additive: a deployment that never turns it on
// behaves exactly as it did before the migration.
func TestRecurringCommissionIsOffByDefault(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 12345)

	before := balanceOf(t, w.pool, w.affiliateID)

	var out growth.RenewalCommission
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = growth.RecordRecurringCommission(ctx, tx, renewalID)
		return err
	})
	if !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("recurring commission with the feature off: err = %v, want ErrNotReferred", err)
	}
	if out.Commission.Minor() != 0 {
		t.Fatalf("commission = %d, want 0 with the feature off", out.Commission.Minor())
	}

	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_recurring_commissions WHERE invoice_id = $1`,
		renewalID).Scan(&rows); err != nil {
		t.Fatalf("count renewal commissions: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d renewal commission rows with the feature off, want 0", rows)
	}
	if after := balanceOf(t, w.pool, w.affiliateID); after != before {
		t.Fatalf("balance moved from %d to %d with the feature off", before, after)
	}
}

// ------------------------------------------------------------ the commission --

// A renewal earns commission at the affiliate's exact rational rate, attributed to the
// referral recorded on the customer, and it is the balance that grows.
func TestRenewalEarnsRecurringCommission(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 12345)

	var out growth.RenewalCommission
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = growth.RecordRecurringCommission(ctx, tx, renewalID)
		return err
	}); err != nil {
		t.Fatalf("recurring commission: %v", err)
	}

	// 10% of 12345 is 1234.5; half-up on the exact product is 1235. A float would land on
	// 1234.4999... and a different number under a different rounding.
	if out.Commission.Minor() != 1235 {
		t.Fatalf("commission = %d, want 1235", out.Commission.Minor())
	}
	if out.AffiliateID != w.affiliateID {
		t.Fatalf("affiliate = %s, want %s (the referral on the customer)", out.AffiliateID, w.affiliateID)
	}
	if out.CustomerID != w.customerID || out.InvoiceID != renewalID {
		t.Fatalf("row = %+v, want customer %s invoice %s", out, w.customerID, renewalID)
	}
	if out.RenewalIndex != 1 {
		t.Fatalf("renewal index = %d, want 1", out.RenewalIndex)
	}
	if out.OrderID != "" {
		t.Fatalf("renewal commission has an order id %q; a renewal has no order", out.OrderID)
	}
	if out.Duplicate {
		t.Fatal("the first call reported a duplicate")
	}
	if got := balanceOf(t, w.pool, w.affiliateID); got != 1235 {
		t.Fatalf("balance = %d, want 1235", got)
	}

	// The rate is stored canonically, so a later recomputation lands on the same minor units.
	var storedRate string
	if err := w.pool.QueryRow(ctx, `
		SELECT commission_rate FROM affiliate_recurring_commissions WHERE id = $1`,
		out.ID).Scan(&storedRate); err != nil {
		t.Fatalf("load rate: %v", err)
	}
	if storedRate != "1/10" {
		t.Fatalf("stored rate = %q, want 1/10", storedRate)
	}
}

// A replayed webhook pays once. The guard is the unique index on the invoice id, so this
// asserts the second call returns the existing row and leaves the balance alone.
func TestRecurringCommissionIsIdempotentOnInvoice(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 10000)

	var first, second growth.RenewalCommission
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = growth.RecordRecurringCommission(ctx, tx, renewalID)
		return err
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = growth.RecordRecurringCommission(ctx, tx, renewalID)
		return err
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !second.Duplicate {
		t.Fatal("the replay was not reported as a duplicate")
	}
	if second.ID != first.ID || second.Commission.Minor() != first.Commission.Minor() {
		t.Fatalf("replay returned %+v, want the existing row %+v", second, first)
	}

	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_recurring_commissions WHERE invoice_id = $1`,
		renewalID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d commission rows for one renewal, want 1", rows)
	}
	if got := balanceOf(t, w.pool, w.affiliateID); got != first.Commission.Minor() {
		t.Fatalf("balance = %d, want %d (a replay must not increment it)",
			got, first.Commission.Minor())
	}
}

// affiliates.recurring_cycles bounds how many renewals of one referred customer earn. With
// 2, the first two renewals pay and the third does not — and the third writes nothing.
func TestRecurringCommissionIsBoundedByCycles(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	setAffiliateSetting(t, w.pool, "recurring_cycles", "2")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)

	for i := 1; i <= 2; i++ {
		invoiceID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 5000)
		var out growth.RenewalCommission
		if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = growth.RecordRecurringCommission(ctx, tx, invoiceID)
			return err
		}); err != nil {
			t.Fatalf("renewal %d: %v", i, err)
		}
		if out.RenewalIndex != i {
			t.Fatalf("renewal %d recorded index %d", i, out.RenewalIndex)
		}
	}

	thirdID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 5000)
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, thirdID)
		return err
	})
	if !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("third renewal with cycles=2: err = %v, want ErrNotReferred", err)
	}

	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_recurring_commissions WHERE customer_id = $1`,
		w.customerID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("%d renewal commissions with cycles=2, want 2", rows)
	}
	// 10% of 5000 twice.
	if got := balanceOf(t, w.pool, w.affiliateID); got != 1000 {
		t.Fatalf("balance = %d, want 1000", got)
	}
}

// Zero cycles means every renewal, which is a deliberate choice and not a missing value.
func TestRecurringCyclesZeroPaysEveryRenewal(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	setAffiliateSetting(t, w.pool, "recurring_cycles", "0")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)

	for i := 1; i <= 4; i++ {
		invoiceID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 2000)
		if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := growth.RecordRecurringCommission(ctx, tx, invoiceID)
			return err
		}); err != nil {
			t.Fatalf("renewal %d with cycles=0: %v", i, err)
		}
	}
	if got := balanceOf(t, w.pool, w.affiliateID); got != 800 {
		t.Fatalf("balance = %d, want 800 (10%% of 2000, four times)", got)
	}
}

// A first order is not a renewal, and a manual invoice is not a subscription renewal. The
// recurring path refuses both, so the affiliate is never paid twice for one invoice and
// never paid for a document nobody renewed.
func TestRecurringCommissionRefusesNonRenewals(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")

	// The world's invoice came from an order: RecordCommissionForPaidInvoice owns it.
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, w.invoiceID)
		return err
	}); !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("order invoice: err = %v, want ErrNotReferred", err)
	}

	// A manual invoice: paid, no order, no subscription.
	manualID := seedManualInvoice(t, w.pool, w.customerID, 9000)
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, manualID)
		return err
	}); !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("manual invoice: err = %v, want ErrNotReferred", err)
	}

	// An unpaid renewal earns nothing either.
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	var unpaidID string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, subscription_id, period_key, status, currency,
			subtotal_minor, tax_minor, total_minor, issued_at, snapshot)
		VALUES ('INV-U-' || gen_random_uuid()::text, $1, $2, 'p-open', 'open', 'USD',
		        4000, 0, 4000, now(), '{"schema_version":1}'::jsonb)
		RETURNING id`, w.customerID, subscriptionID).Scan(&unpaidID); err != nil {
		t.Fatalf("unpaid invoice: %v", err)
	}
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, unpaidID)
		return err
	}); !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("unpaid renewal: err = %v, want ErrNotReferred", err)
	}

	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_recurring_commissions`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d renewal commissions for non-renewals, want 0", rows)
	}
}

// A customer who was never referred earns nothing, and neither does a retired affiliate:
// a renewal must not fail because a marketing payee was disabled.
func TestRecurringCommissionRequiresAReferralAndAnEnabledAffiliate(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")

	// A customer with no attribution row at all.
	var strangerID string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('stranger@example.test', 'Stranger')
		RETURNING id`).Scan(&strangerID); err != nil {
		t.Fatalf("stranger: %v", err)
	}
	subID := seedSubscription(t, w.pool, strangerID)
	strangerInvoice := seedRenewalInvoice(t, w.pool, strangerID, subID, 5000)
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, strangerInvoice)
		return err
	}); !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("unreferred customer: err = %v, want ErrNotReferred", err)
	}

	// A disabled affiliate: no-op, not a failed settlement.
	if _, err := w.pool.Exec(ctx,
		`UPDATE affiliates SET enabled = false WHERE id = $1`, w.affiliateID); err != nil {
		t.Fatalf("disable affiliate: %v", err)
	}
	subID2 := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subID2, 5000)
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, renewalID)
		return err
	}); !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("disabled affiliate: err = %v, want ErrNotReferred", err)
	}
}

// ------------------------------------------------------------- the dispatcher --

// RecordSettledInvoiceCommission is the one call the settlement path makes: an order invoice
// earns the first-order commission, a renewal earns the recurring one, and neither pays twice.
func TestSettledInvoiceCommissionDispatches(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 8000)

	// The order invoice: the existing commission path, unchanged.
	var orderCommission growth.Referral
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		orderCommission, err = growth.RecordSettledInvoiceCommission(ctx, tx, w.invoiceID)
		return err
	}); err != nil {
		t.Fatalf("order invoice: %v", err)
	}
	if orderCommission.OrderID != w.orderID || orderCommission.Commission.Minor() != 1235 {
		t.Fatalf("order commission = %+v, want order %s and 1235 minor",
			orderCommission, w.orderID)
	}

	// The renewal: the recurring path.
	var renewalCommission growth.Referral
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		renewalCommission, err = growth.RecordSettledInvoiceCommission(ctx, tx, renewalID)
		return err
	}); err != nil {
		t.Fatalf("renewal invoice: %v", err)
	}
	if renewalCommission.Commission.Minor() != 800 {
		t.Fatalf("renewal commission = %d, want 800", renewalCommission.Commission.Minor())
	}
	if renewalCommission.OrderID != "" {
		t.Fatalf("renewal commission carries order id %q", renewalCommission.OrderID)
	}

	// Both, once each.
	var orderRows, renewalRows int
	if err := w.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM affiliate_referrals WHERE order_id IS NOT NULL),
		       (SELECT count(*) FROM affiliate_recurring_commissions)`).
		Scan(&orderRows, &renewalRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if orderRows != 1 || renewalRows != 1 {
		t.Fatalf("%d order and %d renewal commissions, want 1 and 1", orderRows, renewalRows)
	}
	if got := balanceOf(t, w.pool, w.affiliateID); got != 2035 {
		t.Fatalf("balance = %d, want 2035", got)
	}
}

// A replayed settlement of a renewal is a no-op through the dispatcher too.
func TestSettledInvoiceCommissionIsIdempotentForRenewals(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	subscriptionID := seedSubscription(t, w.pool, w.customerID)
	renewalID := seedRenewalInvoice(t, w.pool, w.customerID, subscriptionID, 8000)

	for i := 0; i < 2; i++ {
		if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := growth.RecordSettledInvoiceCommission(ctx, tx, renewalID)
			return err
		}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := balanceOf(t, w.pool, w.affiliateID); got != 800 {
		t.Fatalf("balance = %d, want 800 after two settlements", got)
	}
}
