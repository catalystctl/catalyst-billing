package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// adminPurgeTestData removes test fixtures from a database.
//
// It exists because six tables refuse deletion by trigger — the journal, the audit log, issued
// invoices and issued credit notes — and that refusal is deliberate. Purging test data is
// therefore an owner maintenance operation, and the point of this command is to make it a
// reviewed, repeatable, single-transaction operation instead of an ad-hoc psql session that
// somebody has to reconstruct from memory at the worst possible moment.
//
// The guards are the design:
//
//   - A pattern is mandatory, and it must be a non-trivial one. There is no "purge everything"
//     mode: a command that can empty the books with one flag is a command that eventually does.
//   - It refuses to run without --yes, so a shell history recall cannot delete data.
//   - It refuses to run against a database whose name does not look like a test or development
//     database, unless --force is given. Production is not a place to discover a typo.
//   - Everything happens in one transaction. Triggers are disabled inside it and re-enabled
//     before COMMIT, so an error leaves the guarantees switched on — PostgreSQL DDL is
//     transactional, which is what makes the rollback safe.
//   - It reports what it deleted and re-verifies afterwards that the triggers are back and the
//     ledger still balances.
//
// What it does not do: decide whether deleting financial records is appropriate. If this is being
// run against anything a customer or an accountant has seen, the answer is a counter-document (a
// credit note, a refund, a reversal posting), not a delete.

// purgeTrigger is one immutability trigger that has to be disabled for the purge.
type purgeTrigger struct {
	table string
	name  string
}

// purgeTriggers is every trigger that can block the purge.
//
// The list is explicit rather than discovered, because disabling a trigger by pattern is exactly
// the kind of cleverness that one day disables the wrong one. A test asserts that each of these
// exists, so a renamed trigger fails loudly instead of being silently skipped.
var purgeTriggers = []purgeTrigger{
	{"audit_events", "audit_events_append_only"},
	{"credit_notes", "credit_notes_guard_issued"},
	{"invoice_lines", "invoice_lines_guard"},
	{"invoices", "invoices_guard_issued"},
	{"journal_entries", "journal_entries_append_only"},
	{"journal_transactions", "journal_transactions_append_only"},
}

// purgeSets are the temp tables that hold the id sets the purge works from.
//
// They exist because the foreign-key graph is not navigable from the pattern in one direction:
// payments carry no customer id, and journal_transactions carry no customer id either — the
// customer is on the entry, not the transaction. Capturing the sets once, up front, makes every
// later DELETE a single indexed predicate and removes the temptation to write a sub-select that
// silently matches nothing.
const (
	purgeCustomerSet = "_purge_customer"
	purgeUserSet     = "_purge_user"
	purgeInvoiceSet  = "_purge_invoice"
	purgePaymentSet  = "_purge_payment"
	purgeJournalSet  = "_purge_journal"
)

// purgeCapture creates and fills the id sets.
//
// The order matters: payments are discovered through allocations, which point at invoices; the
// journal transactions are discovered through the entries, which point at the customer.
func purgeCapture() []string {
	return []string{
		`CREATE TEMP TABLE ` + purgeCustomerSet + ` ON COMMIT DROP AS
			SELECT id FROM customers WHERE email LIKE $1`,
		`CREATE TEMP TABLE ` + purgeUserSet + ` ON COMMIT DROP AS
			SELECT id FROM users WHERE email LIKE $1`,
		`CREATE TEMP TABLE ` + purgeInvoiceSet + ` ON COMMIT DROP AS
			SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM ` + purgeCustomerSet + `)`,
		`CREATE TEMP TABLE ` + purgePaymentSet + ` ON COMMIT DROP AS
			SELECT DISTINCT payment_id AS id FROM payment_allocations
			WHERE invoice_id IN (SELECT id FROM ` + purgeInvoiceSet + `)
			UNION
			SELECT DISTINCT payment_id AS id FROM refunds
			WHERE payment_id IN (
				SELECT payment_id FROM payment_allocations
				WHERE invoice_id IN (SELECT id FROM ` + purgeInvoiceSet + `))`,
		`CREATE TEMP TABLE ` + purgeJournalSet + ` ON COMMIT DROP AS
			SELECT DISTINCT transaction_id AS id FROM journal_entries
			WHERE customer_id IN (SELECT id FROM ` + purgeCustomerSet + `)`,
	}
}

// purgeExec runs a statement, passing the pattern only when the statement references it.
//
// A statement with no placeholder rejects an extra argument, so the caller cannot just always pass
// one: the capture statements that select from an already-captured temp table take no parameter.
func purgeExec(ctx context.Context, tx pgx.Tx, stmt, pattern string) (int64, error) {
	var (
		tag pgconn.CommandTag
		err error
	)
	if strings.Contains(stmt, "$1") {
		tag, err = tx.Exec(ctx, stmt, pattern)
	} else {
		tag, err = tx.Exec(ctx, stmt)
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// purgeStep is one DELETE, in dependency order.
//
// The order is children before parents. A mistake here is not silent: a foreign-key violation
// aborts the whole transaction, so a missing step is loud.
type purgeStep struct {
	table string
	where string
}

// purgeSteps deletes, in order, every row that belongs to a captured customer or user.
func purgeSteps() []purgeStep {
	const customers = ` IN (SELECT id FROM ` + purgeCustomerSet + `)`
	const users = ` IN (SELECT id FROM ` + purgeUserSet + `)`
	const invoices = ` IN (SELECT id FROM ` + purgeInvoiceSet + `)`
	const payments = ` IN (SELECT id FROM ` + purgePaymentSet + `)`

	return []purgeStep{
		// Rows hanging off a service or an invoice.
		{"service_operations", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)`},
		{"service_holds", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)`},
		{"suspension_decisions", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)
			OR invoice_id` + invoices},
		{"renewal_events", `invoice_id` + invoices},
		{"dunning_states", `invoice_id` + invoices},
		{"payment_attempts", `invoice_id` + invoices},

		// Money: refunds before the allocations they were derived from, allocations before payments.
		{"refunds", `payment_id` + payments},
		{"payment_allocations", `invoice_id` + invoices + ` OR payment_id` + payments},
		{"credit_applications", `customer_id` + customers},
		{"credit_notes", `customer_id` + customers},

		// Documents.
		{"invoice_lines", `invoice_id` + invoices},
		{"invoices", `customer_id` + customers},
		{"order_items", `order_id IN (SELECT id FROM orders WHERE customer_id` + customers + `)`},
		{"orders", `customer_id` + customers},

		// Services and the rows that name one. Everything referencing `services` has to go before it,
		// and `services` before `subscriptions` (a service carries `subscription_id`): getting either
		// order wrong makes the command fail on the first customer who has a running server, which is
		// most of them. Tickets are keyed by customer rather than by user, because a ticket can
		// outlive the login that opened it.
		{"ticket_attachments", `message_id IN (SELECT id FROM ticket_messages WHERE ticket_id IN
			(SELECT id FROM tickets WHERE customer_id` + customers + `))`},
		{"ticket_messages", `ticket_id IN (SELECT id FROM tickets WHERE customer_id` + customers + `)`},
		{"tickets", `customer_id` + customers},
		{"service_cancellations", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)`},
		{"service_upgrades", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)`},
		{"service_configs", `service_id IN (SELECT id FROM services WHERE customer_id` + customers + `)`},
		{"services", `customer_id` + customers},
		{"subscriptions", `customer_id` + customers},
		{"billing_profiles", `customer_id` + customers},
		{"customer_accounts", `customer_id` + customers},
		{"payments", `id` + payments},

		// Rows keyed by user.
		{"mfa_challenges", `user_id` + users},
		{"mfa_recovery_codes", `user_id` + users},
		{"mfa_credentials", `user_id` + users},
		{"email_verifications", `user_id` + users},
		{"password_resets", `user_id` + users},
		{"api_keys", `user_id` + users},
		{"sessions", `user_id` + users},
		{"auth_events", `user_id` + users},
		{"staff_roles", `user_id` + users},
		{"approvals", `requested_by` + users + ` OR approved_by` + users},

		// Support. Attachments hang off messages and messages off tickets, so the order matters:
		// deleting a ticket first would be refused by the foreign key. Tickets are keyed by customer
		// rather than by user, because a ticket can outlive the login that opened it.
		{"ticket_mail_logs", `to_address LIKE $1 OR from_address LIKE $1`},

		// Cart. A cart item references a price version, so it goes before the catalog is touched.
		{"cart_items", `cart_id IN (SELECT id FROM carts WHERE customer_id` + customers + `)`},
		{"carts", `customer_id` + customers},

		// Commerce configuration and growth. These are keyed by the records above, so they go after
		// them: a coupon redemption names the invoice it discounted, and a commission names the
		// order it was earned on.
		{"coupon_redemptions", `customer_id` + customers},
		// The affiliate programme: everything names an affiliate, and an affiliate names a user, so
		// the whole chain has to be removed before `users` is. `affiliate_payouts` also names the
		// staff member who approved the payout, which is why it is matched on both columns.
		{"affiliate_payouts", `affiliate_id IN (SELECT id FROM affiliates WHERE user_id` + users + `)
			OR created_by` + users},
		{"affiliate_balances", `affiliate_id IN (SELECT id FROM affiliates WHERE user_id` + users + `)`},
		{"affiliate_recurring_commissions", `affiliate_id IN (SELECT id FROM affiliates WHERE user_id` + users + `)`},
		{"affiliate_referrals", `customer_id` + customers + ` OR affiliate_id IN (SELECT id FROM affiliates WHERE user_id` + users + `)`},
		{"affiliates", `user_id` + users},
		{"notifications", `user_id` + users},
		{"notification_preferences", `user_id` + users},

		// Rows with no foreign key. They cannot be reached by a join, so they are matched on their
		// own columns: an idempotency key outlives its principal by design (it is scoped to a bare
		// uuid), which is why it has to be swept by the id set rather than left behind.
		{"notification_deliveries", `to_address LIKE $1`},
		{"idempotency_keys", `principal_id` + users},

		// The parents. Audit rows name users and customers, so they go before both.
		{"audit_events", `actor_id` + users + ` OR target_id` + customers + ` OR target_id` + users},
		{"journal_entries", `customer_id` + customers},
		{"journal_transactions", `id IN (SELECT id FROM ` + purgeJournalSet + `)`},
		{"users", `id` + users},
		{"customers", `id` + customers},
	}
}

// minPurgePatternLen is the shortest pattern the command will accept.
//
// A one-character pattern matches almost everything, which is the failure mode this command exists
// to avoid.
const minPurgePatternLen = 4

// adminPurgeTestData is the entry point.
func adminPurgeTestData(args []string) error {
	fs := flag.NewFlagSet("admin purge-test-data", flag.ExitOnError)
	pattern := fs.String("pattern", "",
		"SQL LIKE pattern matching the test data (required, e.g. 'smoke-%@example.test')")
	yes := fs.Bool("yes", false, "confirm the deletion (required)")
	force := fs.Bool("force", false, "allow running against a database whose name is not test/dev")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted without deleting anything")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*pattern) == "" {
		return errors.New("--pattern is required: this command never purges a whole database")
	}
	if len(*pattern) < minPurgePatternLen {
		return fmt.Errorf("--pattern must be at least %d characters: %q would match nearly everything",
			minPurgePatternLen, *pattern)
	}
	if !*yes && !*dryRun {
		return errors.New("refusing to delete without --yes (use --dry-run to preview)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	// A production database is not the place to discover a typo in a pattern.
	if !*force {
		var dbName string
		if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
			return fmt.Errorf("read database name: %w", err)
		}
		if !looksLikeTestDatabase(dbName) {
			return fmt.Errorf("database %q does not look like a test or development database; "+
				"pass --force if you are certain", dbName)
		}
		fmt.Printf("database: %s\n", dbName)
	}

	if *dryRun {
		return purgeDryRun(ctx, pool, *pattern)
	}
	return purgeExecute(ctx, pool, *pattern)
}

// looksLikeTestDatabase reports whether a name suggests a database that may be purged.
func looksLikeTestDatabase(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"test", "dev", "local", "sandbox", "staging", "smoke"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// purgeDryRun reports the row counts each step would delete, using a transaction that is rolled
// back so the temp tables and the triggers are handled exactly as they would be for real.
func purgeDryRun(ctx context.Context, pool *store.Pool, pattern string) error {
	fmt.Printf("dry run: pattern %q\n", pattern)

	var total int64
	// The dry run runs the real statement sequence inside a transaction and then rolls back, so the
	// counts cannot disagree with what the real run would do. It also proves the deletes are
	// executable (no foreign-key violation) before anything is committed.
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, stmt := range purgeCapture() {
			if _, err := purgeExec(ctx, tx, stmt, pattern); err != nil {
				return fmt.Errorf("capture: %w", err)
			}
		}
		for _, trig := range purgeTriggers {
			if _, err := tx.Exec(ctx,
				`ALTER TABLE `+trig.table+` DISABLE TRIGGER `+trig.name); err != nil {
				return fmt.Errorf("disable %s on %s: %w", trig.name, trig.table, err)
			}
		}
		for _, step := range purgeSteps() {
			n, err := purgeExec(ctx, tx, `DELETE FROM `+step.table+` WHERE `+step.where, pattern)
			if err != nil {
				return fmt.Errorf("delete from %s: %w", step.table, err)
			}
			if n > 0 {
				fmt.Printf("  %-24s %d\n", step.table, n)
				total += n
			}
		}
		// Rolling back undoes the deletes and restores the triggers.
		return errRollbackDryRun
	})
	if err != nil && !errors.Is(err, errRollbackDryRun) {
		return err
	}
	fmt.Printf("would delete %d row(s); nothing was changed\n", total)
	return nil
}

// errRollbackDryRun unwinds a dry run.
var errRollbackDryRun = errors.New("dry run complete")

// purgeExecute deletes the rows in one transaction.
func purgeExecute(ctx context.Context, pool *store.Pool, pattern string) error {
	var deleted int64
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, stmt := range purgeCapture() {
			if _, err := purgeExec(ctx, tx, stmt, pattern); err != nil {
				return fmt.Errorf("capture: %w", err)
			}
		}

		// Disabling the triggers is the whole reason this is a command rather than a DELETE. It
		// happens inside the transaction, so an error anywhere below restores them on rollback.
		for _, trig := range purgeTriggers {
			if _, err := tx.Exec(ctx,
				`ALTER TABLE `+trig.table+` DISABLE TRIGGER `+trig.name); err != nil {
				return fmt.Errorf("disable %s on %s: %w", trig.name, trig.table, err)
			}
		}

		for _, step := range purgeSteps() {
			n, err := purgeExec(ctx, tx, `DELETE FROM `+step.table+` WHERE `+step.where, pattern)
			if err != nil {
				return fmt.Errorf("delete from %s: %w", step.table, err)
			}
			if n > 0 {
				fmt.Printf("  %-24s %d\n", step.table, n)
				deleted += n
			}
		}

		// Re-enable before commit. If this were skipped the guarantees would stay off, which is the
		// one outcome worse than not purging at all.
		for _, trig := range purgeTriggers {
			if _, err := tx.Exec(ctx,
				`ALTER TABLE `+trig.table+` ENABLE TRIGGER `+trig.name); err != nil {
				return fmt.Errorf("re-enable %s on %s: %w", trig.name, trig.table, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("deleted %d row(s)\n", deleted)

	// Verify the guarantees are back. This is a check, not a fix: if it fails, the transaction
	// committed with a trigger disabled and that has to be known immediately.
	var disabled int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE NOT tgisinternal AND tgenabled = 'D'`).Scan(&disabled); err != nil {
		return fmt.Errorf("verify triggers: %w", err)
	}
	if disabled != 0 {
		return fmt.Errorf("%d trigger(s) are still disabled; re-enable them before using this database",
			disabled)
	}

	// The ledger must still balance: a purge that broke the books would be a much worse outcome than
	// the leftover test rows it removed.
	rows, err := pool.Query(ctx, `
		SELECT currency, SUM(debit_minor) - SUM(credit_minor)
		FROM journal_entries GROUP BY currency`)
	if err != nil {
		return fmt.Errorf("verify ledger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			delta int64
		)
		if err := rows.Scan(&code, &delta); err != nil {
			return fmt.Errorf("verify ledger: %w", err)
		}
		if delta != 0 {
			return fmt.Errorf("the ledger is unbalanced in %s by %d after the purge", code, delta)
		}
	}
	fmt.Println("triggers re-enabled; ledger balances")
	return nil
}
