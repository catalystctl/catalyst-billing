package growth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// Affiliate payouts (GRW-03, ADM-37).
//
// The property under test is reconciliation: an operator must be able to read what is owed,
// what has been paid and the rows that make up the difference, and the three must agree.
// The second property is that a payout is a record — it links the commissions it settled and
// never restates what they were worth.

// payoutWorld is a referred customer with an order commission and a renewal commission, so
// a payout has both kinds of row to settle.
type payoutWorld struct {
	*world
	subscriptionID string
	renewalID      string
	orderMinor     int64
	renewalMinor   int64
}

// newPayoutWorld builds the two commissions.
func newPayoutWorld(t *testing.T) *payoutWorld {
	t.Helper()
	w := &payoutWorld{world: newWorld(t, "10%")}
	ctx := context.Background()
	setAffiliateSetting(t, w.pool, "recurring", "true")
	w.subscriptionID = seedSubscription(t, w.pool, w.customerID)
	w.renewalID = seedRenewalInvoice(t, w.pool, w.customerID, w.subscriptionID, 8000)

	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := growth.RecordCommissionForPaidInvoice(ctx, tx, w.invoiceID); err != nil {
			return err
		}
		_, err := growth.RecordRecurringCommission(ctx, tx, w.renewalID)
		return err
	}); err != nil {
		t.Fatalf("seed commissions: %v", err)
	}
	// 10% of 12345 half-up, and 10% of 8000.
	w.orderMinor = 1235
	w.renewalMinor = 800
	return w
}

// reportOf reads one currency's reconciliation.
func reportOf(t *testing.T, w *payoutWorld, currency string) growth.PayoutReconciliation {
	t.Helper()
	now := time.Now().UTC()
	line, err := w.store.Reconcile(context.Background(), w.affiliateID, currency,
		now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return line
}

// ------------------------------------------------------------- the report --

// The report states earned, paid and outstanding, and they agree with the rows. Before a
// payout: everything earned is owed.
func TestPayoutReportReconcilesBeforeAPayout(t *testing.T) {
	w := newPayoutWorld(t)
	line := reportOf(t, w, "USD")

	if line.Earned.Minor() != w.orderMinor+w.renewalMinor {
		t.Fatalf("earned = %d, want %d", line.Earned.Minor(), w.orderMinor+w.renewalMinor)
	}
	if line.Paid.Minor() != 0 {
		t.Fatalf("paid = %d, want 0", line.Paid.Minor())
	}
	if line.Outstanding.Minor() != line.Earned.Minor() {
		t.Fatalf("outstanding = %d, want %d", line.Outstanding.Minor(), line.Earned.Minor())
	}
	if !line.Reconciles {
		t.Fatalf("the report does not reconcile: earned %d, paid %d, outstanding %d",
			line.Earned.Minor(), line.Paid.Minor(), line.Outstanding.Minor())
	}
	if line.Code == "" || line.Email == "" {
		t.Fatalf("report is missing the affiliate identity: %+v", line)
	}

	// Both kinds of row are listed, and neither has a payout yet.
	var commissionRows, renewalRows int
	for _, row := range line.Rows {
		switch row.Kind {
		case growth.PayoutRowCommission:
			commissionRows++
			if row.OrderID != w.orderID {
				t.Fatalf("order commission row points at %q, want %q", row.OrderID, w.orderID)
			}
		case growth.PayoutRowRenewal:
			renewalRows++
			if row.InvoiceID != w.renewalID {
				t.Fatalf("renewal row points at %q, want %q", row.InvoiceID, w.renewalID)
			}
			if row.RenewalIndex != 1 {
				t.Fatalf("renewal row index = %d, want 1", row.RenewalIndex)
			}
		}
		if row.PayoutID != "" {
			t.Fatalf("row %s is settled by a payout that does not exist", row.ID)
		}
	}
	if commissionRows != 1 || renewalRows != 1 {
		t.Fatalf("%d order and %d renewal rows, want 1 and 1", commissionRows, renewalRows)
	}

	// The rows sum to the outstanding figure: that is the reconciliation an operator reads.
	var sum int64
	for _, row := range line.Rows {
		if row.Kind != growth.PayoutRowPayout {
			sum += row.Amount.Minor()
		}
	}
	if sum != line.Outstanding.Minor() {
		t.Fatalf("rows sum to %d, outstanding says %d", sum, line.Outstanding.Minor())
	}
}

// An affiliate with no earnings at all reports zeros, not an error: an empty period is a
// fact, not a failure.
func TestPayoutReportIsZeroForAnIdleAffiliate(t *testing.T) {
	w := newWorld(t, "10%")
	now := time.Now().UTC()
	line, err := w.store.Reconcile(context.Background(), w.affiliateID, "USD",
		now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if line.Earned.Minor() != 0 || line.Paid.Minor() != 0 || line.Outstanding.Minor() != 0 {
		t.Fatalf("idle affiliate reports %+v, want zeros", line)
	}
	if len(line.Rows) != 0 {
		t.Fatalf("%d rows for an idle affiliate, want 0", len(line.Rows))
	}
	if !line.Reconciles {
		t.Fatal("an idle affiliate does not reconcile")
	}
}

// The rows are scoped to the period; the totals are all-time, so a period that predates the
// commissions still states what is owed.
func TestPayoutReportRowsAreScopedToThePeriod(t *testing.T) {
	w := newPayoutWorld(t)
	now := time.Now().UTC()

	// A window that ends before the commissions were earned.
	line, err := w.store.Reconcile(context.Background(), w.affiliateID, "USD",
		now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(line.Rows) != 0 {
		t.Fatalf("%d rows in a period before the commissions, want 0", len(line.Rows))
	}
	if line.Outstanding.Minor() != w.orderMinor+w.renewalMinor {
		t.Fatalf("outstanding = %d, want the full %d: the totals are not period-scoped",
			line.Outstanding.Minor(), w.orderMinor+w.renewalMinor)
	}
}

// ReconcileAll covers every currency the affiliate has touched, and an unknown affiliate is
// a 404 rather than an empty report.
func TestReconcileAllAndUnknownAffiliate(t *testing.T) {
	w := newPayoutWorld(t)
	now := time.Now().UTC()

	lines, err := w.store.ReconcileAll(context.Background(), w.affiliateID,
		now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("reconcile all: %v", err)
	}
	if len(lines) != 1 || lines[0].Currency != "USD" {
		t.Fatalf("reconcile all = %+v, want one USD line", lines)
	}

	if _, err := w.store.ReconcileAll(context.Background(),
		"00000000-0000-0000-0000-000000000000", now, now); !errors.Is(err, growth.ErrNotFound) {
		t.Fatalf("unknown affiliate: err = %v, want ErrNotFound", err)
	}
	if _, err := w.store.Reconcile(context.Background(), w.affiliateID, "not a currency",
		now, now); !errors.Is(err, growth.ErrInvalidInput) {
		t.Fatalf("bad currency: err = %v, want ErrInvalidInput", err)
	}
}

// ------------------------------------------------------------------ payouts --

// A payout settles both kinds of commission, records one auditable row, and leaves the
// report stating earned = paid with nothing outstanding.
func TestRecordPayoutSettlesBothKindsAndReconciles(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()

	var paid growth.RecordedPayout
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		paid, err = w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID,
			Currency:    "USD",
			Reference:   "PAYPAL-1234",
			Note:        "March run",
			ActorID:     w.customerUser,
			PeriodFrom:  &[]time.Time{now.Add(-24 * time.Hour)}[0],
			PeriodTo:    &[]time.Time{now}[0],
		})
		return err
	}); err != nil {
		t.Fatalf("payout: %v", err)
	}

	if paid.Amount.Minor() != w.orderMinor+w.renewalMinor {
		t.Fatalf("paid = %d, want %d", paid.Amount.Minor(), w.orderMinor+w.renewalMinor)
	}
	if paid.OrderCommissions != 1 || paid.RenewalCommissions != 1 || paid.Commissions != 2 {
		t.Fatalf("settled %d order + %d renewal = %d rows, want 1 + 1 = 2",
			paid.OrderCommissions, paid.RenewalCommissions, paid.Commissions)
	}
	if paid.ID == "" || paid.Method != "credit" || paid.Reference != "PAYPAL-1234" {
		t.Fatalf("recorded payout = %+v", paid)
	}

	// The payout is a row, and the commission rows point at it.
	var recorded int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_payouts WHERE affiliate_id = $1`,
		w.affiliateID).Scan(&recorded); err != nil {
		t.Fatalf("count payouts: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("%d payout rows, want 1", recorded)
	}
	var unlinked int
	if err := w.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM affiliate_referrals
		         WHERE affiliate_id = $1 AND order_id IS NOT NULL AND payout_id IS DISTINCT FROM $2)
		     + (SELECT count(*) FROM affiliate_recurring_commissions
		         WHERE affiliate_id = $1 AND payout_id IS DISTINCT FROM $2)`,
		w.affiliateID, paid.ID).Scan(&unlinked); err != nil {
		t.Fatalf("count unlinked: %v", err)
	}
	if unlinked != 0 {
		t.Fatalf("%d commission rows are not linked to the payout", unlinked)
	}

	// The report now says earned == paid, outstanding 0, and it reconciles.
	line := reportOf(t, w, "USD")
	if line.Paid.Minor() != line.Earned.Minor() {
		t.Fatalf("paid = %d, earned = %d", line.Paid.Minor(), line.Earned.Minor())
	}
	if line.Outstanding.Minor() != 0 {
		t.Fatalf("outstanding = %d after a full payout, want 0", line.Outstanding.Minor())
	}
	if !line.Reconciles {
		t.Fatal("the report does not reconcile after a payout")
	}
	if line.Payouts != 1 {
		t.Fatalf("report counts %d payouts, want 1", line.Payouts)
	}

	// Every row is settled by that payout, and a payout row is listed too.
	var payoutRows int
	for _, row := range line.Rows {
		if row.Kind == growth.PayoutRowPayout {
			payoutRows++
			if row.Amount.Minor() != paid.Amount.Minor() || row.ID != paid.ID {
				t.Fatalf("payout row = %+v, want %s at %d", row, paid.ID, paid.Amount.Minor())
			}
			continue
		}
		if row.PayoutID != paid.ID {
			t.Fatalf("commission row %s is not linked to the payout", row.ID)
		}
		if row.Status != "paid" || row.PaidAt == "" {
			t.Fatalf("commission row %s is %q at %q after settling", row.ID, row.Status, row.PaidAt)
		}
	}
	if payoutRows != 1 {
		t.Fatalf("%d payout rows in the report, want 1", payoutRows)
	}

	// The materialised balance agrees with the report.
	var balance, lifetime int64
	if err := w.pool.QueryRow(ctx, `
		SELECT balance_minor, paid_minor FROM affiliate_balances
		WHERE affiliate_id = $1 AND currency = 'USD'`, w.affiliateID).
		Scan(&balance, &lifetime); err != nil {
		t.Fatalf("load balance: %v", err)
	}
	if balance != 0 {
		t.Fatalf("balance = %d after a payout, want 0", balance)
	}
	if lifetime != paid.Amount.Minor() {
		t.Fatalf("lifetime paid = %d, want %d", lifetime, paid.Amount.Minor())
	}
}

// A payout never restates what a commission was worth. This is the "a payout is a row, never
// a mutation of the commission rows" property, asserted on the amounts.
func TestPayoutDoesNotRewriteCommissionAmounts(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	var before int64
	if err := w.pool.QueryRow(ctx, `
		SELECT coalesce(sum(commission_minor), 0) FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, w.affiliateID).Scan(&before); err != nil {
		t.Fatalf("sum before: %v", err)
	}

	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("payout: %v", err)
	}

	var after int64
	if err := w.pool.QueryRow(ctx, `
		SELECT coalesce(sum(commission_minor), 0) FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, w.affiliateID).Scan(&after); err != nil {
		t.Fatalf("sum after: %v", err)
	}
	if after != before {
		t.Fatalf("commission total moved from %d to %d across a payout", before, after)
	}
}

// A second payout has nothing to settle: the money left once.
func TestRecordPayoutRefusesASecondTime(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("first payout: %v", err)
	}
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD",
		})
		return err
	}); !errors.Is(err, growth.ErrNothingToPay) {
		t.Fatalf("second payout: err = %v, want ErrNothingToPay", err)
	}

	var recorded int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_payouts WHERE affiliate_id = $1`,
		w.affiliateID).Scan(&recorded); err != nil {
		t.Fatalf("count: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("%d payout rows after a refused second payout, want 1", recorded)
	}
}

// When the materialised balance and the commission rows disagree, the payout refuses. Paying
// either figure would move money nobody can account for, so the refusal is the feature.
func TestRecordPayoutRefusesWhenTheBalanceDisagrees(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	// A balance that does not match the rows: the defect this check exists for.
	if _, err := w.pool.Exec(ctx, `
		UPDATE affiliate_balances SET balance_minor = 1
		WHERE affiliate_id = $1 AND currency = 'USD'`, w.affiliateID); err != nil {
		t.Fatalf("tamper balance: %v", err)
	}

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD",
		})
		return err
	})
	if !errors.Is(err, growth.ErrBalanceMismatch) {
		t.Fatalf("err = %v, want ErrBalanceMismatch", err)
	}

	// Nothing was written: no payout row, and the commissions are still owed.
	var payouts, unsettled int
	if err := w.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM affiliate_payouts WHERE affiliate_id = $1),
		       (SELECT count(*) FROM affiliate_recurring_commissions
		         WHERE affiliate_id = $1 AND payout_id IS NULL)`, w.affiliateID).
		Scan(&payouts, &unsettled); err != nil {
		t.Fatalf("count: %v", err)
	}
	if payouts != 0 || unsettled != 1 {
		t.Fatalf("%d payouts and %d unsettled renewals after a refusal, want 0 and 1",
			payouts, unsettled)
	}
}

// A payout with no balance at all is ErrNothingToPay, and an unknown currency is refused
// before the balance is touched.
func TestRecordPayoutValidatesItsInput(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "EUR",
		})
		return err
	})
	if !errors.Is(err, growth.ErrNothingToPay) {
		t.Fatalf("a currency with no balance: err = %v, want ErrNothingToPay", err)
	}

	err = w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD", Method: "carrier pigeon",
		})
		return err
	})
	if !errors.Is(err, growth.ErrInvalidInput) {
		t.Fatalf("an unknown method: err = %v, want ErrInvalidInput", err)
	}

	if _, err := w.store.RecordPayout(ctx, nil, growth.NewPayout{
		AffiliateID: w.affiliateID, Currency: "USD",
	}); !errors.Is(err, growth.ErrInvalidInput) {
		t.Fatalf("no transaction: err = %v, want ErrInvalidInput", err)
	}
}

// The history lists the recorded payouts, newest first, with the method and reference an
// operator quotes when reconciling a statement.
func TestPayoutsHistory(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: w.affiliateID, Currency: "USD",
			Method: "bank_transfer", Reference: "SEPA-77",
		})
		return err
	}); err != nil {
		t.Fatalf("payout: %v", err)
	}

	history, err := w.store.Payouts(ctx, w.affiliateID, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("%d payouts in history, want 1", len(history))
	}
	if history[0].Method != "bank_transfer" || history[0].Reference != "SEPA-77" {
		t.Fatalf("history row = %+v", history[0])
	}
	if history[0].Amount.Minor() != w.orderMinor+w.renewalMinor {
		t.Fatalf("history amount = %d, want %d",
			history[0].Amount.Minor(), w.orderMinor+w.renewalMinor)
	}
}

// A commission marked settled with no payout row to account for it is exactly the defect
// migration 0043 reconstructs away for historical data. If it happens anyway — a hand-written
// UPDATE, a half-restored backup — the report must say so rather than present a confident
// total: `reconciles` is false, and the row is listed as still owed.
func TestPayoutReportCatchesASettledCommissionWithNoPayout(t *testing.T) {
	w := newPayoutWorld(t)
	ctx := context.Background()

	// The pre-0043 shape: the commission was paid, but nothing recorded the payment.
	if _, err := w.pool.Exec(ctx, `
		UPDATE affiliate_referrals SET status = 'paid', paid_at = now()
		WHERE affiliate_id = $1 AND order_id = $2`, w.affiliateID, w.orderID); err != nil {
		t.Fatalf("mark commission paid: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `
		UPDATE affiliate_balances SET balance_minor = balance_minor - $2
		WHERE affiliate_id = $1 AND currency = 'USD'`, w.affiliateID, w.orderMinor); err != nil {
		t.Fatalf("reduce balance: %v", err)
	}

	line := reportOf(t, w, "USD")
	if line.Reconciles {
		t.Fatalf("the report reconciles with a settled commission no payout accounts for: %+v", line)
	}
	// Outstanding is what is still genuinely owed — the pending renewal — because a commission
	// already marked paid is not payable again even though nothing accounts for the payment.
	if line.Outstanding.Minor() != w.renewalMinor {
		t.Fatalf("outstanding = %d, want %d", line.Outstanding.Minor(), w.renewalMinor)
	}
	var found bool
	for _, row := range line.Rows {
		if row.Kind == growth.PayoutRowCommission && row.PayoutID == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("the unsettled commission is not listed in the report")
	}
}
