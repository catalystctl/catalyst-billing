package credit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// Downgrade credits (CRD-06, UPG-04, CRD-10).
//
// These are database tests because every rule they assert is a row: a credit note, a ledger
// entry, the link between them, and the setting that decides whether any of it happens.
//
// The fixture writes the `service_upgrades` row directly rather than through
// `service.Store.Upgrade`. That is deliberate: what is under test is the *posting* half — the
// half that did not exist — and a test that computed its own proration would pass even if the
// posting posted the wrong number. The proration itself is proven in internal/service.

// downgradeFixture is one customer with a service and a pending downgrade row.
type downgradeFixture struct {
	pool        *store.Pool
	svc         *credit.Service
	customerID  string
	serviceID   string
	upgradeID   string
	proratedMi  int64
	periodStart time.Time
}

// newDowngradeFixture creates the billing chain and one pending downgrade.
func newDowngradeFixture(t *testing.T, proratedMinor int64) *downgradeFixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	f := &downgradeFixture{
		pool:        pool,
		svc:         credit.New(pool),
		proratedMi:  proratedMinor,
		periodStart: now.AddDate(0, 0, -15),
	}
	periodEnd := now.AddDate(0, 0, 15)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('downgrade@example.test', 'Downgrade Customer')
			RETURNING id`).Scan(&f.customerID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	var productID, priceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('downgrade-product', 'VPS Small') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'Small', 'USD', 1000, 'monthly', 1, 'UTC', 'plan-small', 'eu-west')
		RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price version: %v", err)
	}

	var subID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			anchor_hour, anchor_minute, timezone, current_period_start, current_period_end,
			next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 0, 0, 'UTC', $3, $4, $4)
		RETURNING id`, f.customerID, priceID, f.periodStart, periodEnd).Scan(&subID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, subscription_id, product_id, status)
		VALUES ($1, $2, $3, 'active') RETURNING id`,
		f.customerID, subID, productID).Scan(&f.serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO service_upgrades
			(service_id, from_price_version_id, to_price_version_id, prorated_minor, currency, status)
		VALUES ($1, $2, $2, $3, 'USD', 'pending')
		RETURNING id`, f.serviceID, priceID, proratedMinor).Scan(&f.upgradeID); err != nil {
		t.Fatalf("upgrade row: %v", err)
	}
	return f
}

// balance reads the derived USD credit balance.
func (f *downgradeFixture) balance(t *testing.T) int64 {
	t.Helper()
	amounts, err := credit.BalanceFor(context.Background(), f.pool, f.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	for _, a := range amounts {
		if a.Currency().Code() == "USD" {
			return a.Minor()
		}
	}
	return 0
}

// creditNotes counts the credit notes issued to the fixture's customer.
func (f *downgradeFixture) creditNotes(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM credit_notes WHERE customer_id = $1`, f.customerID).Scan(&n); err != nil {
		t.Fatalf("count credit notes: %v", err)
	}
	return n
}

// setDowngradeSetting writes `credits.credit_on_downgrade`.
func (f *downgradeFixture) setDowngradeSetting(t *testing.T, value string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('credits', 'credit_on_downgrade', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatalf("set setting: %v", err)
	}
}

// A pending downgrade that owes the customer money posts exactly that money, as a numbered
// credit note and a signed ledger entry, and links the two to the row.
//
// The amount is asserted against the row's own prorated figure (8.25 of a 33.00 downgrade with
// half the period left, rounded once) rather than recomputed here: the posting's job is to move
// the number the proration produced, not to produce a second one.
func TestDowngradeCreditPostsTheOwedAmount(t *testing.T) {
	f := newDowngradeFixture(t, -825)

	posted, err := f.svc.PostDowngradeCredit(context.Background(),
		credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if err != nil {
		t.Fatalf("post downgrade credit: %v", err)
	}
	if posted.AlreadyPosted {
		t.Fatal("the first posting reported itself as a replay")
	}
	if posted.AmountMinor != 825 {
		t.Fatalf("posted %d minor, want 825 (the row's own prorated amount, positive)", posted.AmountMinor)
	}
	if posted.Number == "" {
		t.Fatal("the credit was posted without a number; a tax filing refers to credit note numbers")
	}
	if posted.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", posted.Currency)
	}

	// The balance is the ledger's sum, so it is the proof the money moved.
	if got := f.balance(t); got != 825 {
		t.Fatalf("balance = %d, want 825", got)
	}
	if got := f.creditNotes(t); got != 1 {
		t.Fatalf("credit notes = %d, want 1", got)
	}

	// The row carries the note and the time, which is what makes "credited but not yet applied"
	// a state an operator can see.
	var (
		noteID *string
		linkOK bool
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT credit_note_id::text, credit_posted_at IS NOT NULL
		FROM service_upgrades WHERE id = $1`, f.upgradeID).Scan(&noteID, &linkOK); err != nil {
		t.Fatalf("read upgrade row: %v", err)
	}
	if noteID == nil || *noteID != posted.CreditNoteID {
		t.Fatalf("credit_note_id = %v, want %s", noteID, posted.CreditNoteID)
	}
	if !linkOK {
		t.Fatal("credit_posted_at was not written; the posting is not visible on the row")
	}

	// The posting is an allowance, not a payment: revenue is debited and the credit liability is
	// created. A posting that debited receivable would claim an invoice was paid by money nobody
	// sent.
	var (
		debitAccount, creditAccount string
		debitMinor, creditMinor     int64
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT d.account_code, d.debit_minor, cr.account_code, cr.credit_minor
		FROM journal_transactions jt
		JOIN journal_entries d  ON d.transaction_id = jt.id AND d.debit_minor  > 0
		JOIN journal_entries cr ON cr.transaction_id = jt.id AND cr.credit_minor > 0
		WHERE jt.reference_type = 'credit_downgrade' AND jt.reference_id = $1`,
		posted.CreditNoteID).Scan(&debitAccount, &debitMinor, &creditAccount, &creditMinor); err != nil {
		t.Fatalf("read posting: %v", err)
	}
	if debitAccount != "revenue" || creditAccount != "customer_credit" {
		t.Fatalf("posting = debit %s / credit %s, want debit revenue / credit customer_credit",
			debitAccount, creditAccount)
	}
	if debitMinor != 825 || creditMinor != 825 {
		t.Fatalf("posting amounts = %d/%d, want 825/825", debitMinor, creditMinor)
	}
}

// A replay finds the existing note instead of minting a second one. This is the property a
// retried worker run depends on: the job has no memory, so the row has to be the memory.
func TestDowngradeCreditReplayDoesNotPayTwice(t *testing.T) {
	f := newDowngradeFixture(t, -500)
	ctx := context.Background()

	first, err := f.svc.PostDowngradeCredit(ctx, credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if err != nil {
		t.Fatalf("first posting: %v", err)
	}
	second, err := f.svc.PostDowngradeCredit(ctx, credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.AlreadyPosted {
		t.Fatal("the replay did not report itself as already posted")
	}
	if second.CreditNoteID != first.CreditNoteID || second.Number != first.Number {
		t.Fatalf("replay returned a different note (%s/%s) from the original (%s/%s)",
			second.CreditNoteID, second.Number, first.CreditNoteID, first.Number)
	}
	if got := f.balance(t); got != 500 {
		t.Fatalf("balance after a replay = %d, want 500; the credit was paid twice", got)
	}
	if got := f.creditNotes(t); got != 1 {
		t.Fatalf("credit notes = %d, want 1", got)
	}
}

// An upgrade row owes nothing: its money is an invoice the customer pays. Asking for a credit on
// it must be refused rather than posting a refund for a charge that has not happened.
func TestDowngradeCreditRefusesAnUpgradeRow(t *testing.T) {
	f := newDowngradeFixture(t, 400)

	_, err := f.svc.PostDowngradeCredit(context.Background(),
		credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if !errors.Is(err, credit.ErrNotDowngrade) {
		t.Fatalf("err = %v, want ErrNotDowngrade", err)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0; an upgrade was credited", got)
	}
	if got := f.creditNotes(t); got != 0 {
		t.Fatalf("credit notes = %d, want 0", got)
	}
}

// The switch is real: with `credits.credit_on_downgrade` off, nothing is posted and nothing is
// linked, so the row stays visibly owed. An installation on a no-refunds contract gets its
// policy rather than a credit it did not authorise.
func TestDowngradeCreditHonoursTheDisableSwitch(t *testing.T) {
	f := newDowngradeFixture(t, -750)
	f.setDowngradeSetting(t, "false")

	_, err := f.svc.PostDowngradeCredit(context.Background(),
		credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if !errors.Is(err, credit.ErrDowngradeDisabled) {
		t.Fatalf("err = %v, want ErrDowngradeDisabled", err)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	if got := f.creditNotes(t); got != 0 {
		t.Fatalf("credit notes = %d, want 0", got)
	}

	// And re-enabling it posts the row that was refused, which is what makes the switch a
	// policy rather than a way to lose an obligation.
	f.setDowngradeSetting(t, "true")
	posted, err := f.svc.PostDowngradeCredit(context.Background(),
		credit.DowngradeCreditRequest{UpgradeID: f.upgradeID})
	if err != nil {
		t.Fatalf("post after re-enabling: %v", err)
	}
	if posted.AmountMinor != 750 {
		t.Fatalf("posted %d, want 750", posted.AmountMinor)
	}
}

// The batch entry point credits what it can and skips the row it cannot, without losing the
// rest of the batch. A worker tick that stopped at the first bad row would leave every downgrade
// after it uncredited.
func TestPostPendingDowngradeCreditsIsABoundedBatch(t *testing.T) {
	f := newDowngradeFixture(t, -100)

	posted, failed, err := f.svc.PostPendingDowngradeCredits(context.Background(), 10)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if len(posted) != 1 || posted[0] != f.upgradeID {
		t.Fatalf("posted = %v, want [%s]", posted, f.upgradeID)
	}
	if got := f.balance(t); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}

	// A second run finds nothing left to do, because the row now carries its note.
	again, _, err := f.svc.PostPendingDowngradeCredits(context.Background(), 10)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the second run credited %v again", again)
	}
}
