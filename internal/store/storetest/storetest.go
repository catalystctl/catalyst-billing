// Package storetest provides the integration-test harness for anything that
// needs a real PostgreSQL database.
//
// There are no in-memory substitutes: the constraints, triggers, locking
// semantics and privilege model are the thing under test, so the tests run
// against a real server.
//
// # Isolation
//
// Each test *package* gets its own database, named after the package and created
// on demand from the connection string in BILLING_TEST_DSN. Within a package,
// tests are serialised with a session advisory lock and start from truncated
// tables.
//
// Why per package rather than per test:
//
//   - `go test ./...` runs packages in parallel. With one shared database the
//     packages contend for the same lock and, worse, one package's TRUNCATE can
//     interleave with another's fixtures. Separate databases make that
//     impossible instead of unlikely.
//   - Per-test databases would be cleaner still, but each one pays a full
//     migration cost; per package pays it once and still keeps packages fully
//     independent.
//   - A crashed test binary can only wedge its own package, and the lock is
//     acquired with a deadline so it reports rather than hangs.
//
// Tests are skipped when no database is reachable, so `go test ./...` still
// works on a machine without the cluster. CI must set BILLING_REQUIRE_DB=1 so a
// missing database fails the build instead of silently skipping the only tests
// that prove the invariants.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultDSN points at the compose cluster. It matches .env.example.
const DefaultDSN = "postgres://billing:devonly-billing-owner@127.0.0.1:5442/billing_test?sslmode=disable"

// testLockID serialises tests inside one package.
const testLockID int64 = 8_421_196_317_004_212

// lockWait bounds how long a test waits for the package lock before failing.
// A suite that hangs is worse than one that fails: it burns CI minutes and
// reports nothing.
const lockWait = 60 * time.Second

// BaseDSN returns the configured test connection string.
func BaseDSN() string {
	if v := strings.TrimSpace(os.Getenv("BILLING_TEST_DSN")); v != "" {
		return v
	}
	return DefaultDSN
}

// packageName derives a stable database-safe suffix for the running test binary,
// e.g. "billing.test" -> "billing".
func packageName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".test")
	base = strings.TrimSuffix(base, ".exe")

	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "pkg"
	}
	// PostgreSQL identifiers are limited to 63 bytes; the suffix is appended to
	// the configured database name, so keep this side short.
	if len(name) > 20 {
		name = name[:20]
	}
	return name
}

// DSN returns the connection string for this package's database.
//
// The database name is replaced in the URL itself. pgx's ConnConfig.ConnString()
// returns the original string it was parsed from, so mutating
// ConnConfig.Database and then asking for the string silently yields the
// unmodified DSN -- which made every package share one database.
func DSN() string {
	base := BaseDSN()
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		// Not a URL (keyword/value DSN): cannot safely rewrite, so use it as-is.
		return base
	}
	u.Path = "/" + databaseName()
	return u.String()
}

// databaseName is the per-package database name.
func databaseName() string {
	cfg, err := pgxpool.ParseConfig(BaseDSN())
	if err != nil {
		return "billing_test"
	}
	base := cfg.ConnConfig.Database
	if os.Getenv("BILLING_TEST_SHARED_DB") == "1" {
		return base
	}
	return base + "_" + packageName()
}

var (
	openOnce sync.Once
	shared   *store.Pool
	openErr  error
)

// session is one test's exclusive hold on the package database. A test may ask
// for the database more than once (for example to build two independent
// worlds); the first call takes the lock and truncates, later calls reuse the
// same session. Refcounting is what makes that safe: a second truncate would
// silently delete the first world's fixtures.
type session struct {
	conn *pgxpool.Conn
	refs int
}

var (
	sessionsMu sync.Mutex
	sessions   = make(map[string]*session)
)

// DB returns a migrated pool whose application tables are empty. The test holds
// an exclusive advisory lock on its package database until it finishes, so tests
// are serialised even when they call t.Parallel().
func DB(t *testing.T) *store.Pool {
	t.Helper()

	pool, err := sharedPool()
	if err != nil {
		if os.Getenv("BILLING_REQUIRE_DB") == "1" {
			t.Fatalf("database required but unavailable: %v", err)
		}
		t.Skipf("skipping database test: %v", err)
	}

	name := t.Name()
	sessionsMu.Lock()
	sess, ok := sessions[name]
	sessionsMu.Unlock()

	if !ok {
		ctx := context.Background()
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire connection: %v", err)
		}
		if err := acquireTestLock(conn.Conn()); err != nil {
			conn.Release()
			t.Fatalf("acquire test lock: %v", err)
		}
		if err := Truncate(ctx, conn.Conn()); err != nil {
			conn.Release()
			t.Fatalf("truncate: %v", err)
		}
		sess = &session{conn: conn}
		sessionsMu.Lock()
		sessions[name] = sess
		sessionsMu.Unlock()

		t.Cleanup(func() { release(name) })
	}

	sessionsMu.Lock()
	sess.refs++
	sessionsMu.Unlock()

	return pool
}

// sharedPool creates the package database if needed, migrates it, and returns a
// pool. It runs once per test binary.
func sharedPool() (*store.Pool, error) {
	openOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		if err := ensureDatabase(ctx); err != nil {
			openErr = err
			return
		}

		cfg := store.Config{
			URL:              DSN(),
			ConnectTimeout:   5 * time.Second,
			StatementTimeout: 60 * time.Second,
		}
		pool, err := store.Open(ctx, cfg)
		if err != nil {
			openErr = err
			return
		}
		mig := pool.Migrator()
		mig.LockTimeout = 60 * time.Second
		if _, err := mig.Up(ctx); err != nil {
			pool.Close()
			openErr = fmt.Errorf("migrate test database: %w", err)
			return
		}
		shared = pool
	})
	return shared, openErr
}

// ensureDatabase creates the package database when it does not exist.
//
// It connects to the maintenance database with the same credentials, so the
// configured role must be allowed to create databases. On the compose cluster it
// is (CREATEDB is granted to the owner role in devdb/init); in a locked-down
// environment the database can be created ahead of time and this is a no-op.
func ensureDatabase(ctx context.Context) error {
	cfg, err := pgxpool.ParseConfig(BaseDSN())
	if err != nil {
		return fmt.Errorf("parse test DSN: %w", err)
	}
	want := databaseName()

	admin := cfg.ConnConfig.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, want).Scan(&exists); err != nil {
		return fmt.Errorf("check database %q: %w", want, err)
	}
	if exists {
		return nil
	}
	// The identifier is derived from a sanitised name, but quote it anyway.
	stmt := "CREATE DATABASE " + pgx.Identifier{want}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %q (the test role needs CREATEDB): %w", want, err)
	}
	return nil
}

// acquireTestLock takes the package-wide advisory lock with a deadline.
//
// It deliberately does not break locks held by other backends. An idle backend
// is indistinguishable from one that is simply between statements, so
// terminating "stale" holders would eventually kill a healthy test run. A leaked
// lock is reported with the holder's pid and a documented remedy instead.
func acquireTestLock(conn *pgx.Conn) error {
	ctx := context.Background()
	deadline := time.Now().Add(lockWait)

	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, testLockID).Scan(&got); err != nil {
			return fmt.Errorf("try lock: %w", err)
		}
		if got {
			return nil
		}
		if time.Now().After(deadline) {
			holders, _ := lockHolders(ctx, conn)
			return fmt.Errorf(
				"timed out after %s waiting for the package test lock, held by backend(s) %v; "+
					"if a previous test run was killed, clear it with `make db-unlock`",
				lockWait, holders)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lockHolders returns the backend pids currently holding the package test lock.
func lockHolders(ctx context.Context, conn *pgx.Conn) ([]int, error) {
	rows, err := conn.Query(ctx, `
		SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objid = $1`, testLockID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pids []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, rows.Err()
}

// release drops one reference and, when the last one goes, unlocks the database.
func release(name string) {
	sessionsMu.Lock()
	sess, ok := sessions[name]
	if !ok {
		sessionsMu.Unlock()
		return
	}
	sess.refs--
	if sess.refs > 0 {
		sessionsMu.Unlock()
		return
	}
	delete(sessions, name)
	sessionsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sess.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, testLockID); err != nil {
		// A failed unlock would wedge every later test in this package.
		panic(fmt.Sprintf("storetest: release test lock: %v", err))
	}
	sess.conn.Release()
}

// ClosePool closes the shared pool.
func ClosePool() {
	if shared != nil {
		shared.Close()
		shared = nil
	}
}

// Truncate empties every application table. Children are listed before parents
// so a plain TRUNCATE ... CASCADE is not even needed, but CASCADE is used so
// adding a table without updating this list cannot silently leave rows behind.
//
// Before truncating it verifies that every table in the database is classified:
// either it is truncated, or it is declared reference data that a migration
// seeds. A table added by a later migration and forgotten here used to leak
// state between tests -- which looks like an impossible intermittent failure --
// so the check turns that into an immediate, explicit error.
func Truncate(ctx context.Context, conn *pgx.Conn) error {
	if err := verifyTableClassification(ctx, conn); err != nil {
		return err
	}
	tables := make([]string, len(Tables))
	for i, tbl := range Tables {
		tables[i] = pgx.Identifier{tbl}.Sanitize()
	}
	stmt := "TRUNCATE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	// Reference data that tests expect to exist after truncation.
	if _, err := conn.Exec(ctx, `
		INSERT INTO ledger_accounts (code, kind, name) VALUES
			('ar',              'asset',     'Accounts receivable'),
			('cash_clearing',   'asset',     'Payment clearing'),
			('customer_credit', 'liability', 'Customer credit balance'),
			('tax_payable',     'liability', 'Tax payable'),
			('revenue',         'income',    'Service revenue'),
			('refunds',         'expense',   'Refunds and chargebacks')
		ON CONFLICT (code) DO NOTHING`); err != nil {
		return fmt.Errorf("restore ledger accounts: %w", err)
	}

	// Reference rows are not truncated, so a test that edited one would leak into
	// every later test in the package. Restoring them explicitly makes each test start
	// from the migrated state regardless of what the previous one did.
	if _, err := conn.Exec(ctx, `
		DELETE FROM retention_policies WHERE table_name NOT IN
			('login_attempts', 'auth_events', 'gateway_events', 'idempotency_keys',
			 'notification_deliveries', 'password_resets', 'email_verifications', 'mfa_challenges')`); err != nil {
		return fmt.Errorf("restore retention policies: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO retention_policies (table_name, keep_for, age_column) VALUES
			('login_attempts',          interval '1 day',    'created_at'),
			('auth_events',             interval '180 days', 'created_at'),
			('gateway_events',          interval '400 days', 'received_at'),
			('idempotency_keys',        interval '2 days',   'created_at'),
			('notification_deliveries', interval '180 days', 'created_at'),
			('password_resets',         interval '7 days',   'created_at'),
			('email_verifications',     interval '7 days',   'created_at'),
			('mfa_challenges',          interval '1 day',    'created_at')
		ON CONFLICT (table_name) DO UPDATE
			SET keep_for = EXCLUDED.keep_for, age_column = EXCLUDED.age_column`); err != nil {
		return fmt.Errorf("restore retention policies: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		UPDATE billing_policies
		SET invoice_lead_days = 7, payment_terms_days = 7, grace_days = 3,
		    catchup_max_periods = 2, catchup_max_age_days = 30
		WHERE id = 'default'`); err != nil {
		return fmt.Errorf("restore billing policy: %w", err)
	}

	// Settings are configuration an operator edits at runtime, so they are not truncated --
	// but a test that changes one must not leak it into the next. Deleting and re-seeding is
	// the reset: it restores exactly the defaults the migration inserts, so a test always
	// starts from the state a fresh installation has.
	if _, err := conn.Exec(ctx, `DELETE FROM settings`); err != nil {
		return fmt.Errorf("clear settings: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES
			('general',  'company_name',        'Catalyst Hosting'),
			('general',  'system_email_address','no-reply@localhost'),
			('general',  'tos',                 ''),
			('general',  'registration_disabled','false'),
			('general',  'pagination',          '25'),
			('invoices', 'bill_to_text',        ''),
			('invoices', 'notes_client_visible','true'),
			('invoices', 'invoice_number_prefix','INV-'),
			('invoices', 'invoice_number_padding','6'),
			('invoices', 'credit_note_prefix',  'CN-'),
			('invoices', 'credit_note_padding', '6'),
			('invoices', 'due_days',            '7'),
			('invoices', 'proforma',            'false'),
			('credits',  'enabled',             'true'),
			('credits',  'minimum_deposit_minor','500'),
			('credits',  'maximum_deposit_minor','100000'),
			('credits',  'maximum_credit_minor','1000000'),
			('credits',  'auto_use',            'true'),
			('tax',      'enabled',             'false'),
			('tax',      'default_mode',        'exclusive'),
			('tickets',  'disabled',            'false'),
			('tickets',  'client_closing_disabled','false'),
			('tickets',  'departments',         '["general","billing","technical"]'),
			('tickets',  'autoclose_days',      '14'),
			('cronjob',  'order_cancel_days',   '14'),
			('cronjob',  'terminate_days',      '30'),
			('mail',     'disable',             'false'),
			('mail',     'must_verify',         'false'),
			('announcements','enabled',         'true')`); err != nil {
		return fmt.Errorf("restore settings: %w", err)
	}
	return nil
}

// verifyTableClassification fails when a base table exists that the harness does
// not know about.
func verifyTableClassification(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	known := make(map[string]struct{}, len(Tables)+len(ReferenceTables))
	for _, t := range Tables {
		known[t] = struct{}{}
	}
	for _, t := range ReferenceTables {
		known[t] = struct{}{}
	}

	var unknown []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan table name: %w", err)
		}
		if _, ok := known[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	if len(unknown) > 0 {
		return fmt.Errorf(
			"storetest: table(s) %v are neither truncated nor declared reference data; "+
				"add them to Tables (mutable state) or ReferenceTables (seeded by a migration) "+
				"so tests cannot leak state into each other", unknown)
	}
	return nil
}

// ReferenceTables is seeded by migrations and is never truncated, because it is
// configuration or chart-of-accounts data that the application reads but does
// not create.
var ReferenceTables = []string{
	"schema_migrations",
	"ledger_accounts",
	"billing_policies",
	"retention_policies",
	"currencies",
	"settings",
}

// Tables lists the mutable application tables to truncate, children first.
//
// The harness refuses to run when a base table is in neither this list nor
// ReferenceTables, so adding a migration that creates a table forces a decision here: is
// this state a test must start from empty, or is it configuration a migration seeds? The
// check exists because a forgotten table used to leak state between tests, which presents
// as an impossible intermittent failure.
var Tables = []string{
	// Logs and diagnostics: no dependencies, no dependents.
	"http_logs",
	"error_logs",
	"cron_runs",
	"worker_heartbeat",
	"audit_events",
	"idempotency_keys",
	"approvals",
	"api_keys",
	"mfa_challenges",
	"mfa_recovery_codes",
	"mfa_credentials",
	"email_verifications",
	"password_resets",
	"notification_deliveries",
	"notifications",
	"notification_preferences",
	"notification_templates",
	// Tickets: attachments hang off messages, messages off tickets.
	"ticket_attachments",
	"ticket_messages",
	"ticket_mail_logs",
	"tickets",
	// Announcements.
	"announcements",
	// Commerce configuration. Coupon redemptions reference invoices, so they go first.
	"coupon_redemptions",
	"coupon_products",
	"coupons",
	"tax_rates",
	"config_option_prices",
	"config_options",
	"service_configs",
	"product_upgrades",
	// Cart.
	"cart_items",
	"carts",
	// Money and lifecycle records.
	"credit_applications",
	"credit_notes",
	"billing_profiles",
	"auth_events",
	"login_attempts",
	"sessions",
	"staff_roles",
	"customer_accounts",
	"users",
	"suspension_decisions",
	"service_holds",
	"dunning_states",
	"renewal_events",
	"renewal_runs",
	"journal_entries",
	"journal_transactions",
	"payment_allocations",
	"refunds",
	"payments",
	"payment_attempts",
	"gateway_events",
	"service_upgrades",
	"service_cancellations",
	"service_operations",
	"services",
	"invoice_lines",
	"invoices",
	"invoice_number_sequences",
	"subscriptions",
	"order_items",
	"orders",
	"price_versions",
	"products",
	"categories",
	"product_images",
	"customers",
}
