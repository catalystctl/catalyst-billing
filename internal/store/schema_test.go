package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- helpers --

type fixture struct {
	customerID string
	productID  string
	priceID    string
	orderID    string
	invoiceID  string
	subID      string
}

// seed creates the minimal catalog and customer graph the tests build on.
func seed(t *testing.T, ctx context.Context, pool *store.Pool) fixture {
	t.Helper()
	var f fixture
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('a@example.test', 'Customer A') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return fmt.Errorf("customer: %w", err)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ('vps-s', 'VPS Small') RETURNING id`,
		).Scan(&f.productID); err != nil {
			return fmt.Errorf("product: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions
				(product_id, version, currency, amount_minor, setup_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', 1200, 0, 'monthly', 1, 'UTC')
			RETURNING id`, f.productID,
		).Scan(&f.priceID); err != nil {
			return fmt.Errorf("price: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor)
			VALUES ($1, 'pending_payment', 'USD', 1200) RETURNING id`, f.customerID,
		).Scan(&f.orderID); err != nil {
			return fmt.Errorf("order: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, price_version_id, description, quantity, unit_minor, line_minor, position)
			VALUES ($1, $2, 'VPS Small monthly', 1, 1200, 1200, 1)`, f.orderID, f.priceID,
		); err != nil {
			return fmt.Errorf("order item: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return f
}

func newInvoice(t *testing.T, ctx context.Context, tx pgx.Tx, f fixture, number string, total int64) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency, subtotal_minor, tax_minor, total_minor, issued_at)
		VALUES ($1, $2, $3, 'open', 'USD', $4, 0, $4, now())
		RETURNING id`, number, f.customerID, f.orderID, total,
	).Scan(&id); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	return id
}

func post(t *testing.T, ctx context.Context, tx pgx.Tx, kind, refType, refID string, entries ...[3]any) string {
	t.Helper()
	var txID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO journal_transactions (kind, reference_type, reference_id, currency)
		VALUES ($1, $2, $3, 'USD') RETURNING id`, kind, refType, refID,
	).Scan(&txID); err != nil {
		t.Fatalf("insert journal transaction: %v", err)
	}
	for _, e := range entries {
		account := e[0].(string)
		debit := e[1].(int64)
		credit := e[2].(int64)
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor)
			VALUES ($1, $2, 'USD', $3, $4)`, txID, account, debit, credit,
		); err != nil {
			t.Fatalf("insert journal entry: %v", err)
		}
	}
	return txID
}

// ------------------------------------------------------------------- tests --

func TestMigrationsApplyIdempotently(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	migrations, err := store.LoadMigrations()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}

	// Re-running Up must apply nothing: the schema is already current.
	applied, err := pool.Migrator().Up(ctx)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Up applied %d migrations, want 0", len(applied))
	}

	pending, err := pool.Migrator().Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
}

// A migration file that changed after being applied must be a hard error:
// environments silently diverging is how "works in staging" incidents start.
func TestMigrationDriftDetected(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`,
	); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	t.Cleanup(func() {
		migrations, _ := store.LoadMigrations()
		_, _ = pool.Exec(context.Background(),
			`UPDATE schema_migrations SET checksum = $1 WHERE version = 1`, migrations[0].Checksum)
	})

	_, err := pool.Migrator().Up(ctx)
	if !errors.Is(err, store.ErrMigrationDrift) {
		t.Fatalf("got %v, want ErrMigrationDrift", err)
	}
}

// The subledger must balance per transaction, and the check happens at commit
// so entries can be inserted in any order.
func TestJournalMustBalance(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		// Deliberately unbalanced: 1200 debit against 1100 credit.
		post(t, ctx, tx, "invoice_issued", "invoice", inv,
			[3]any{"ar", int64(1200), int64(0)},
			[3]any{"revenue", int64(0), int64(1100)},
		)
		return nil
	})
	if err == nil {
		t.Fatal("unbalanced journal transaction committed")
	}
	if !store.IsConstraintViolation(err) {
		t.Fatalf("got %v, want a constraint violation", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed transaction left %d journal entries", count)
	}
}

func TestJournalBalancedSucceeds(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		post(t, ctx, tx, "invoice_issued", "invoice", inv,
			[3]any{"ar", int64(1200), int64(0)},
			[3]any{"revenue", int64(0), int64(1200)},
		)
		return nil
	})
	if err != nil {
		t.Fatalf("balanced transaction rejected: %v", err)
	}
}

func TestJournalRejectsEmptyAndMixedCurrency(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	// No entries at all.
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		post(t, ctx, tx, "invoice_issued", "invoice", inv)
		return nil
	})
	if err == nil {
		t.Fatal("empty journal transaction committed")
	}

	// Mixed currency inside one transaction.
	err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0002", 1200)
		var txID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO journal_transactions (kind, reference_type, reference_id, currency)
			VALUES ('invoice_issued', 'invoice', $1, 'USD') RETURNING id`, inv).Scan(&txID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor)
			VALUES ($1, 'ar', 'USD', 1200), ($1, 'ar', 'EUR', 0)`, txID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, credit_minor)
			VALUES ($1, 'revenue', 'EUR', 1200)`, txID); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		t.Fatal("mixed-currency journal transaction committed")
	}
}

func TestJournalIsAppendOnly(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	var entryID int64
	var txID string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		txID = post(t, ctx, tx, "invoice_issued", "invoice", inv,
			[3]any{"ar", int64(1200), int64(0)},
			[3]any{"revenue", int64(0), int64(1200)},
		)
		return tx.QueryRow(ctx, `SELECT id FROM journal_entries LIMIT 1`).Scan(&entryID)
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE journal_entries SET debit_minor = 1 WHERE id = $1`, entryID); err == nil {
		t.Fatal("journal entry update allowed")
	} else if store.SQLState(err) != store.StateRestrictViolation {
		t.Fatalf("update: got state %q, want %s", store.SQLState(err), store.StateRestrictViolation)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM journal_entries WHERE id = $1`, entryID); err == nil {
		t.Fatal("journal entry delete allowed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM journal_transactions WHERE id = $1`, txID); err == nil {
		t.Fatal("journal transaction delete allowed")
	}
}

func TestJournalEntryOneSideOnly(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv := newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		var txID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO journal_transactions (kind, reference_type, reference_id, currency)
			VALUES ('invoice_issued', 'invoice', $1, 'USD') RETURNING id`, inv).Scan(&txID); err != nil {
			return err
		}
		// Both sides populated: a debit and a credit in one row is ambiguous.
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor)
			VALUES ($1, 'ar', 'USD', 100, 100)`, txID)
		return err
	})
	if err == nil {
		t.Fatal("journal entry with both sides committed")
	}
}

// Acceptance scenario 5: one renewal invoice per subscription per period, even
// when two workers race.
func TestOneInvoicePerPeriod(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', '2026-01-01', '2026-02-01', '2026-02-01')
			RETURNING id`, f.customerID, f.priceID).Scan(&f.subID)
	})
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}

	insert := func(periodKey string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO invoices (number, customer_id, subscription_id, period_key, status, currency,
				subtotal_minor, tax_minor, total_minor, issued_at)
			VALUES (gen_random_uuid()::text, $1, $2, $3, 'open', 'USD', 1200, 0, 1200, now())`,
			f.customerID, f.subID, periodKey)
		return err
	}
	if err := insert("2026-01-01/2026-02-01"); err != nil {
		t.Fatalf("first invoice: %v", err)
	}
	if err := insert("2026-01-01/2026-02-01"); !store.IsUniqueViolation(err, "invoices_one_per_period") {
		t.Fatalf("duplicate period invoice: got %v, want unique violation on invoices_one_per_period", err)
	}
	if err := insert("2026-02-01/2026-03-01"); err != nil {
		t.Fatalf("next period invoice: %v", err)
	}

	// A voided invoice releases the period so a corrected invoice can be issued.
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET status = 'void' WHERE subscription_id = $1 AND period_key = '2026-01-01/2026-02-01'`,
		f.subID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if err := insert("2026-01-01/2026-02-01"); err != nil {
		t.Fatalf("re-issue after void: %v", err)
	}
}

// Concurrent workers must not both win: the second insert has to fail.
func TestOneInvoicePerPeriodConcurrent(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', '2026-01-01', '2026-02-01', '2026-02-01')
			RETURNING id`, f.customerID, f.priceID).Scan(&f.subID)
	})
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}

	const workers = 8
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(n int) {
			<-start
			_, err := pool.Exec(context.Background(), `
				INSERT INTO invoices (number, customer_id, subscription_id, period_key, status, currency,
					subtotal_minor, tax_minor, total_minor, issued_at)
				VALUES ($1, $2, $3, '2026-01-01/2026-02-01', 'open', 'USD', 1200, 0, 1200, now())`,
				fmt.Sprintf("INV-%04d", n), f.customerID, f.subID)
			errs <- err
		}(i)
	}
	close(start)

	succeeded := 0
	for i := 0; i < workers; i++ {
		if err := <-errs; err == nil {
			succeeded++
		} else if !store.IsUniqueViolation(err) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d workers succeeded, want exactly 1", succeeded)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM invoices WHERE subscription_id = $1`, f.subID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d invoices exist, want 1", count)
	}
}

// Acceptance scenario 2: two distinct events about the same payment cannot
// double-credit, because the provider payment id is unique.
func TestPaymentProviderIdentityUnique(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	insert := func(paymentID string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor, currency, status)
			VALUES ('stripe', 'acct_123', $1, 1200, 'USD', 'succeeded')`, paymentID)
		return err
	}
	if err := insert("pi_abc"); err != nil {
		t.Fatalf("first payment: %v", err)
	}
	if err := insert("pi_abc"); !store.IsUniqueViolation(err, "payments_provider_unique") {
		t.Fatalf("duplicate payment: got %v, want unique violation", err)
	}
	// A different provider account is a different payment identity.
	if _, err := pool.Exec(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor, currency, status)
		VALUES ('stripe', 'acct_other', 'pi_abc', 1200, 'USD', 'succeeded')`); err != nil {
		t.Fatalf("same payment id in another account: %v", err)
	}
}

// Acceptance scenario 1: a replayed webhook is deduplicated by event id before
// anything else happens.
func TestWebhookEventDeduplication(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO gateway_events (provider, provider_account, event_id, event_type, payload, signature_valid)
			VALUES ('stripe', 'acct_123', 'evt_1', 'payment_intent.succeeded', '{}'::jsonb, true)`)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first event: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := insert(); !store.IsUniqueViolation(err, "gateway_events_unique") {
			t.Fatalf("replay %d: got %v, want unique violation", i, err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d events stored, want 1", count)
	}
}

// A proforma invoice is a document a customer can read and pay that carries no number until
// it becomes real. The immutability trigger from 0008 refused *any* change to `number` on a
// non-draft row, so converting one was impossible; 0021 permits exactly that transition and
// nothing else. This test pins both halves: the conversion works, and every neighbouring
// mutation that used to be refused still is.
//
// The second half matters more than the first. A carve-out that is one condition too wide
// turns "issued invoices are immutable" into a comment, and the whole reason the trigger
// exists is that a wrong invoice must be corrected with a credit note rather than an UPDATE.
func TestProformaConversionAndItsBoundaries(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	// insertProforma creates an open, unnumbered, provisional invoice.
	insertProforma := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO invoices (id, customer_id, status, currency, subtotal_minor, tax_minor,
			                      total_minor, issued_at, proforma)
			VALUES ($1, $2, 'open', 'USD', 1000, 0, 1000, now(), true)`, id, f.customerID); err != nil {
			t.Fatalf("insert proforma: %v", err)
		}
	}

	const proformaID = "aaaaaaaa-0000-0000-0000-000000000001"
	insertProforma(proformaID)

	// A numbered proforma is impossible: that is what "provisional" means.
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor,
		                      issued_at, proforma, number)
		VALUES ($1, 'open', 'USD', 100, 0, 100, now(), true, 'INV-BAD')`, f.customerID); err == nil {
		t.Fatal("a proforma with a number was accepted")
	} else if store.SQLState(err) != store.StateCheckViolation {
		t.Fatalf("numbered proforma: got %q, want a check violation", store.SQLState(err))
	}

	// Converting without assigning a number is refused: a real invoice always has one, and
	// silently producing an unnumbered non-draft invoice would leave a gap in the series.
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET proforma = false WHERE id = $1`, proformaID); err == nil {
		t.Fatal("a proforma was converted without a number")
	} else if store.SQLState(err) != store.StateRestrictViolation {
		t.Fatalf("unnumbered conversion: got %q, want restrict violation", store.SQLState(err))
	}

	// The one permitted transition.
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET proforma = false, number = 'INV-PRO-1' WHERE id = $1`, proformaID); err != nil {
		t.Fatalf("converting a proforma must be allowed: %v", err)
	}

	// ... and once converted, the document is frozen exactly as before.
	frozen := []struct {
		name string
		sql  string
	}{
		{"renumber", `UPDATE invoices SET number = 'INV-OTHER' WHERE id = $1`},
		{"money", `UPDATE invoices SET total_minor = 1, subtotal_minor = 1 WHERE id = $1`},
		{"snapshot", `UPDATE invoices SET snapshot = '{"x":1}'::jsonb WHERE id = $1`},
		{"customer", `UPDATE invoices SET customer_id = gen_random_uuid() WHERE id = $1`},
		{"delete", `DELETE FROM invoices WHERE id = $1`},
	}
	for _, tc := range frozen {
		t.Run("after conversion: "+tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, proformaID)
			if err == nil {
				t.Fatalf("a converted invoice allowed %s", tc.name)
			}
			if store.SQLState(err) != store.StateRestrictViolation {
				t.Fatalf("got %q, want restrict violation", store.SQLState(err))
			}
		})
	}

	// A plain issued invoice is unaffected: it still cannot be renumbered at all.
	const plainID = "bbbbbbbb-0000-0000-0000-000000000002"
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (id, customer_id, status, currency, subtotal_minor, tax_minor,
		                      total_minor, issued_at, number)
		VALUES ($1, $2, 'open', 'USD', 500, 0, 500, now(), 'INV-9')`, plainID, f.customerID); err != nil {
		t.Fatalf("insert plain invoice: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET number = 'INV-10' WHERE id = $1`, plainID); err == nil {
		t.Fatal("a plain issued invoice was renumbered")
	}
}

// Acceptance scenario 14: an issued invoice's money and identity are frozen;
// catalog edits cannot rewrite it.
func TestIssuedInvoiceIsImmutable(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	var inv string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv = newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		return nil
	}); err != nil {
		t.Fatalf("invoice: %v", err)
	}

	cases := []struct {
		name string
		sql  string
	}{
		{"total", `UPDATE invoices SET total_minor = 1, subtotal_minor = 1 WHERE id = $1`},
		{"number", `UPDATE invoices SET number = 'INV-9999' WHERE id = $1`},
		{"currency", `UPDATE invoices SET currency = 'EUR' WHERE id = $1`},
		{"snapshot", `UPDATE invoices SET snapshot = '{"tampered":true}'::jsonb WHERE id = $1`},
		{"customer", `UPDATE invoices SET customer_id = gen_random_uuid() WHERE id = $1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, inv)
			if err == nil {
				t.Fatalf("mutation of issued invoice %s was allowed", tc.name)
			}
			if store.SQLState(err) != store.StateRestrictViolation {
				t.Fatalf("got state %q, want %s", store.SQLState(err), store.StateRestrictViolation)
			}
		})
	}

	// Status transitions remain allowed: paying an invoice is legitimate.
	if _, err := pool.Exec(ctx, `UPDATE invoices SET status = 'paid', amount_paid_minor = 1200 WHERE id = $1`, inv); err != nil {
		t.Fatalf("paying the invoice must be allowed: %v", err)
	}
	// ... but it cannot be paid beyond its total.
	if _, err := pool.Exec(ctx, `UPDATE invoices SET amount_paid_minor = 1300 WHERE id = $1`, inv); err == nil {
		t.Fatal("overpayment beyond the invoice total was allowed")
	}
	// Issued invoices cannot be deleted; only drafts can.
	if _, err := pool.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, inv); err == nil {
		t.Fatal("deleting an issued invoice was allowed")
	}
}

func TestDraftInvoiceIsEditable(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	var inv string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor)
			VALUES ($1, 'draft', 'USD', 100, 0, 100) RETURNING id`, f.customerID).Scan(&inv); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor, net_minor, total_minor)
			VALUES ($1, 1, 'draft line', 1, 100, 100, 100)`, inv); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("draft: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE invoice_lines SET description = 'edited', net_minor = 150, total_minor = 150 WHERE invoice_id = $1`, inv); err != nil {
		t.Fatalf("editing a draft line must be allowed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE invoices SET subtotal_minor = 150, total_minor = 150 WHERE id = $1`, inv); err != nil {
		t.Fatalf("editing a draft total must be allowed: %v", err)
	}
}

// Once issued, invoice lines are frozen too: otherwise the totals and the
// lines could disagree.
func TestIssuedInvoiceLinesFrozen(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	// Lines belong to the draft. Issuing is a separate transition, which is
	// what makes the freeze below meaningful.
	var inv string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoices (customer_id, order_id, status, currency, subtotal_minor, tax_minor, total_minor)
			VALUES ($1, $2, 'draft', 'USD', 1200, 0, 1200) RETURNING id`, f.customerID, f.orderID,
		).Scan(&inv); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor, net_minor, total_minor)
			VALUES ($1, 1, 'VPS Small monthly', 1, 1200, 1200, 1200)`, inv); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE invoices SET status = 'open', number = 'INV-0001', issued_at = now() WHERE id = $1`, inv)
		return err
	}); err != nil {
		t.Fatalf("invoice: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE invoice_lines SET net_minor = 1, total_minor = 1 WHERE invoice_id = $1`, inv); err == nil {
		t.Fatal("updating a line of an issued invoice was allowed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1`, inv); err == nil {
		t.Fatal("deleting a line of an issued invoice was allowed")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor, net_minor, total_minor)
		VALUES ($1, 2, 'late line', 1, 100, 100, 100)`, inv); err == nil {
		t.Fatal("adding a line to an issued invoice was allowed")
	}
}

func TestInvoiceTotalMustMatchLines(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	_, err := pool.Exec(ctx, `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor, issued_at)
		VALUES ($1, 'open', 'USD', 100, 10, 200, now())`, f.customerID)
	if err == nil {
		t.Fatal("invoice with total != subtotal + tax was allowed")
	}
	if store.SQLState(err) != store.StateCheckViolation {
		t.Fatalf("got state %q, want check violation", store.SQLState(err))
	}
}

// Acceptance scenario 9: a refund cannot exceed the refundable funds, and the
// allocation cannot exceed the payment.
func TestRefundAndAllocationBounds(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	var paymentID string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor, currency, status)
			VALUES ('stripe', 'acct_123', 'pi_1', 1200, 'USD', 'succeeded')
			RETURNING id`).Scan(&paymentID)
	}); err != nil {
		t.Fatalf("payment: %v", err)
	}

	// Zero and negative refunds are rejected by the schema itself.
	for _, amount := range []int64{0, -100} {
		_, err := pool.Exec(ctx, `
			INSERT INTO refunds (payment_id, operation_key, amount_minor, currency, status)
			VALUES ($1, $2, $3, 'USD', 'created')`, paymentID, fmt.Sprintf("refund-%d", amount), amount)
		if err == nil {
			t.Fatalf("refund of %d was allowed", amount)
		}
	}
	// A positive refund is fine; the sum-versus-payment check belongs to the
	// domain service, which must see all sibling refunds under a lock.
	if _, err := pool.Exec(ctx, `
		INSERT INTO refunds (payment_id, operation_key, amount_minor, currency, status)
		VALUES ($1, 'refund-1', 500, 'USD', 'succeeded')`, paymentID); err != nil {
		t.Fatalf("valid refund: %v", err)
	}

	// An allocation must be positive.
	var inv string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inv = newInvoice(t, ctx, tx, f, "INV-0001", 1200)
		return nil
	}); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 0, 'USD')`, paymentID, inv); err == nil {
		t.Fatal("zero allocation was allowed")
	}
	// One allocation per (payment, invoice): replaying an allocation is a
	// constraint violation rather than a second credit.
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 1200, 'USD')`, paymentID, inv); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 1200, 'USD')`, paymentID, inv); !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate allocation: got %v, want unique violation", err)
	}
}

// Acceptance scenario 4 support: retrying a provisioning operation must reuse
// its logical identity, and one operation key can only exist once.
func TestServiceOperationKeysUnique(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	var svcID string
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, product_id, status, generation)
			VALUES ($1, $2, 'pending', 1) RETURNING id`, f.customerID, f.productID).Scan(&svcID)
	}); err != nil {
		t.Fatalf("service: %v", err)
	}

	insert := func(key string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'create', $2, 1, 'queued')`, svcID, key)
		return err
	}
	if err := insert("create:svc-1"); err != nil {
		t.Fatalf("first operation: %v", err)
	}
	if err := insert("create:svc-1"); !store.IsUniqueViolation(err, "service_operations_operation_key_key") {
		t.Fatalf("duplicate operation key: got %v, want unique violation", err)
	}
}

// Currency codes are constrained, so a typo cannot create a parallel money
// space that never reconciles.
func TestCurrencyCodeConstraint(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	f := seed(t, ctx, pool)

	for _, code := range []string{"usd", "US$", "", "US D"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor)
			VALUES ($1, 'draft', $2, 0, 0, 0)`, f.customerID, code)
		if err == nil {
			t.Fatalf("currency %q was accepted", code)
		}
	}
}

// The runtime role must not be able to change the schema. This is the
// difference between "a bug could corrupt data" and "a bug could drop a table".
func TestRuntimeRoleCannotDDL(t *testing.T) {
	storetest.DB(t) // ensures migrations are applied

	appDSN := os.Getenv("BILLING_APP_TEST_DSN")
	if appDSN == "" {
		t.Skip("set BILLING_APP_TEST_DSN to test the runtime role's privileges")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := store.Config{URL: appDSN, ConnectTimeout: 5 * time.Second}
	pool, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open as app role: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE TABLE should_not_exist (id int)`); err == nil {
		t.Fatal("runtime role was able to CREATE TABLE")
	} else if store.SQLState(err) != store.StateInsufficientPrivilege {
		t.Fatalf("create table: got state %q, want %s", store.SQLState(err), store.StateInsufficientPrivilege)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE invoices`); err == nil {
		t.Fatal("runtime role was able to DROP TABLE")
	}

	// Reads and writes on existing tables must work: the role is restricted,
	// not useless.
	if _, err := pool.Exec(ctx, `SELECT count(*) FROM invoices`); err != nil {
		t.Fatalf("runtime role cannot read invoices: %v", err)
	}
}
