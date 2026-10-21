// Package storetest provides the integration-test harness for anything that
// needs a real PostgreSQL database.
//
// Tests run against the dedicated cluster from compose.yaml (BILLING_TEST_DSN,
// default 127.0.0.1:5442/billing_test). There are no in-memory substitutes:
// the constraints, triggers and locking semantics are the thing under test.
//
// Isolation strategy: a session advisory lock serialises tests, and each test
// starts from truncated tables. That is slower than one database per test but
// it keeps a single migrated schema, so a schema mistake cannot be masked by
// per-test DDL, and a test that forgets to clean up cannot affect its
// neighbour.
package storetest

import (
	"context"
	"fmt"
	"os"
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

// testLockID serialises tests that share the database.
const testLockID int64 = 8_421_196_317_004_212

var (
	openOnce sync.Once
	shared   *store.Pool
	openErr  error
)

// DSN returns the test connection string.
func DSN() string {
	if v := strings.TrimSpace(os.Getenv("BILLING_TEST_DSN")); v != "" {
		return v
	}
	return DefaultDSN
}

// DB returns a migrated pool whose application tables are empty. The test holds
// an exclusive advisory lock on the database until it finishes, so tests are
// serialised even when they call t.Parallel().
//
// The test is skipped (not failed) when no database is reachable, so `go test`
// still works on a machine without the cluster. CI must set
// BILLING_REQUIRE_DB=1 so a missing database fails the build instead of
// silently skipping the only tests that prove the invariants.
func DB(t *testing.T) *store.Pool {
	t.Helper()

	pool, err := sharedPool()
	if err != nil {
		if os.Getenv("BILLING_REQUIRE_DB") == "1" {
			t.Fatalf("database required but unavailable: %v", err)
		}
		t.Skipf("skipping database test: %v", err)
	}

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, testLockID); err != nil {
		conn.Release()
		t.Fatalf("acquire test lock: %v", err)
	}
	if err := Truncate(ctx, conn.Conn()); err != nil {
		conn.Release()
		t.Fatalf("truncate: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanupCtx, `SELECT pg_advisory_unlock($1)`, testLockID); err != nil {
			t.Logf("release test lock: %v", err)
		}
		conn.Release()
	})

	return pool
}

// sharedPool opens and migrates the database once per test binary.
func sharedPool() (*store.Pool, error) {
	openOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		cfg := store.Config{
			URL:              DSN(),
			ConnectTimeout:   5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
		pool, err := store.Open(ctx, cfg)
		if err != nil {
			openErr = err
			return
		}
		mig := pool.Migrator()
		mig.LockTimeout = 30 * time.Second
		if _, err := mig.Up(ctx); err != nil {
			pool.Close()
			openErr = fmt.Errorf("migrate test database: %w", err)
			return
		}
		shared = pool
	})
	return shared, openErr
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
func Truncate(ctx context.Context, conn *pgx.Conn) error {
	tables := make([]string, len(Tables))
	for i, tbl := range Tables {
		tables[i] = pgx.Identifier{tbl}.Sanitize()
	}
	stmt := "TRUNCATE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	// Price/ledger reference rows that tests expect to exist after truncation.
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
	return nil
}

// Tables lists the application tables to truncate, children first.
var Tables = []string{
	"journal_entries",
	"journal_transactions",
	"payment_allocations",
	"refunds",
	"payments",
	"payment_attempts",
	"gateway_events",
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
	"customers",
}

var _ = pgxpool.Pool{}
