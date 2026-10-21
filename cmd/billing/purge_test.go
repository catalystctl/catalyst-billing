package main

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The purge command is the only thing in the product that can delete a journal entry, an audit
// row or an issued credit note, so its guards matter more than its happy path. These tests cover
// the guards first and the deletion second.

// purgeEnv points the command at the test database and seeds a purgeable customer.
//
// It sets BILLING_DATABASE_URL because the command opens its own pool from the environment — the
// point is to exercise the command as an operator would run it, not to reach into its internals.
func purgeEnv(t *testing.T, pool *store.Pool, pattern string) (customerID, userID string) {
	t.Helper()
	t.Setenv("BILLING_DATABASE_URL", dsnFromEnv(t))
	ctx := context.Background()

	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, 'Purge Me') RETURNING id`,
		pattern).Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ($1, 'x', now()) RETURNING id`, pattern).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		userID, customerID); err != nil {
		t.Fatalf("account: %v", err)
	}
	return customerID, userID
}

// seedImmutableRows creates the rows that refuse deletion: an issued invoice with lines, an issued
// credit note, journal entries and an audit row.
func seedImmutableRows(t *testing.T, pool *store.Pool, customerID, userID string) {
	t.Helper()
	ctx := context.Background()

	var invoiceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, NULL, 'draft', 'USD', 1000, 0, 1000, 0, NULL, now(),
		        '{"schema_version":1}'::jsonb) RETURNING id`, customerID).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	// Lines are frozen once issued, so they are added while the invoice is still a draft.
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor,
			net_minor, tax_rate, tax_minor, total_minor)
		VALUES ($1, 1, 'Hosting', 1, 1000, 1000, '0%', 0, 1000)`, invoiceID); err != nil {
		t.Fatalf("invoice line: %v", err)
	}
	// Then issue it, which is what makes the line and the snapshot immutable.
	if _, err := pool.Exec(ctx, `
		UPDATE invoices SET status = 'open', number = 'INV-PURGE-1', issued_at = now()
		WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO credit_notes (number, customer_id, invoice_id, currency, total_minor,
			tax_minor, reason, status, issued_at)
		VALUES ('CN-PURGE-1', $1, $2, 'USD', 100, 0, 'purge fixture', 'issued', now())`,
		customerID, invoiceID); err != nil {
		t.Fatalf("credit note: %v", err)
	}

	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: invoiceID,
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(1000, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(1000, usd)}},
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO audit_events (actor_id, actor_kind, action, target_type, target_id, outcome)
			VALUES ($1, 'user', 'invoice.create', 'invoice', $2, 'success')`, userID, invoiceID)
		return err
	}); err != nil {
		t.Fatalf("post and audit: %v", err)
	}
}

// countFor counts the rows that belong to a pattern.
func countFor(t *testing.T, pool *store.Pool, table, column, pattern string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE `+column+` LIKE $1`, pattern).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// A missing, short or unconfirmed pattern is refused before the database is even opened.
func TestPurgeRequiresASafePattern(t *testing.T) {
	cases := map[string][]string{
		"no pattern":      {"--yes"},
		"empty pattern":   {"--pattern", "", "--yes"},
		"short pattern":   {"--pattern", "a%", "--yes"},
		"no confirmation": {"--pattern", "smoke-%@example.test"},
	}
	for name, args := range cases {
		if err := adminPurgeTestData(args); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// The command refuses a database whose name does not look like a test database.
func TestPurgeRefusesNonTestDatabase(t *testing.T) {
	if looksLikeTestDatabase("catalyst_billing") {
		t.Fatal("the production database name was treated as a test database")
	}
	for _, name := range []string{"billing_test", "billing_dev", "sandbox", "staging"} {
		if !looksLikeTestDatabase(name) {
			t.Fatalf("%q was not recognised as a test database", name)
		}
	}
}

// A dry run reports the rows and deletes nothing, including the rows that refuse deletion.
func TestPurgeDryRunDeletesNothing(t *testing.T) {
	pool := storetest.DB(t)
	pattern := "smoke-dryrun@example.test"
	customerID, userID := purgeEnv(t, pool, pattern)
	seedImmutableRows(t, pool, customerID, userID)

	if err := adminPurgeTestData([]string{"--pattern", pattern, "--dry-run"}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := countFor(t, pool, "customers", "email", pattern); n != 1 {
		t.Fatalf("the dry run deleted the customer (%d left)", n)
	}
	if n := countFor(t, pool, "users", "email", pattern); n != 1 {
		t.Fatalf("the dry run deleted the user (%d left)", n)
	}
}

// The real purge removes every row belonging to the pattern, including the ones behind immutability
// triggers, and leaves the guarantees switched on.
func TestPurgeRemovesTestDataAndRestoresTriggers(t *testing.T) {
	pool := storetest.DB(t)
	pattern := "smoke-purge@example.test"
	customerID, userID := purgeEnv(t, pool, pattern)
	seedImmutableRows(t, pool, customerID, userID)

	// Precondition: the rows really are undeletable, so the test proves the command does something
	// an ordinary DELETE cannot.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM credit_notes WHERE customer_id = $1`, customerID); err == nil {
		t.Fatal("an issued credit note was deletable; the fixture does not prove anything")
	}

	if err := adminPurgeTestData([]string{"--pattern", pattern, "--yes"}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, check := range []struct{ table, column string }{
		{"customers", "email"},
		{"users", "email"},
	} {
		if n := countFor(t, pool, check.table, check.column, pattern); n != 0 {
			t.Fatalf("%s still has %d row(s)", check.table, n)
		}
	}

	var leftovers int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE actor_id = $1 OR target_id = $1`, userID).
		Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("%d audit row(s) survived", leftovers)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM journal_entries WHERE customer_id = $1`, customerID).
		Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("%d journal entry(ies) survived", leftovers)
	}

	// The guarantees must be back: every immutability trigger enabled.
	for _, trig := range purgeTriggers {
		var enabled string
		if err := pool.QueryRow(context.Background(), `
			SELECT tgenabled::text FROM pg_trigger WHERE tgname = $1`, trig.name).Scan(&enabled); err != nil {
			t.Fatalf("read trigger %s: %v", trig.name, err)
		}
		if enabled != "O" {
			t.Fatalf("trigger %s is %q, want enabled (O)", trig.name, enabled)
		}
	}
}

// The trigger list must match the database. A renamed trigger would otherwise be silently skipped
// and the purge would fail with a confusing "append-only" error instead.
func TestPurgeTriggerListMatchesDatabase(t *testing.T) {
	pool := storetest.DB(t)
	for _, trig := range purgeTriggers {
		var exists bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_trigger
			               WHERE tgname = $1 AND tgrelid = $2::regclass)`,
			trig.name, trig.table).Scan(&exists); err != nil {
			t.Fatalf("check %s on %s: %v", trig.name, trig.table, err)
		}
		if !exists {
			t.Fatalf("trigger %s does not exist on %s; the purge list is stale", trig.name, trig.table)
		}
	}
}

// dsnFromEnv returns the test DSN, skipping the test when no database is configured.
func dsnFromEnv(t *testing.T) string {
	t.Helper()
	dsn := storetest.DSN()
	if strings.TrimSpace(dsn) == "" {
		t.Skip("no test database configured")
	}
	return dsn
}
