package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/retention"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

var now = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func newSweeper(t *testing.T) (*retention.Sweeper, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	s := retention.New(pool)
	s.Now = func() time.Time { return now }
	return s, pool
}

func insertAuthEvent(t *testing.T, pool *store.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auth_events (event, created_at) VALUES ('login_failed', $1)`, at); err != nil {
		t.Fatalf("insert auth event: %v", err)
	}
}

func count(t *testing.T, pool *store.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Rows past their retention period are deleted; rows inside it are not.
func TestSweepRespectsRetention(t *testing.T) {
	s, pool := newSweeper(t)

	// auth_events are kept 180 days.
	insertAuthEvent(t, pool, now.AddDate(0, 0, -200)) // expired
	insertAuthEvent(t, pool, now.AddDate(0, 0, -179)) // kept
	insertAuthEvent(t, pool, now)                     // kept

	results, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var swept bool
	for _, r := range results {
		if r.Table == "auth_events" {
			swept = true
			if r.Deleted != 1 {
				t.Fatalf("deleted %d auth events, want 1", r.Deleted)
			}
			if r.KeepFor != 180*24*time.Hour {
				t.Fatalf("keep_for = %s, want 180 days", r.KeepFor)
			}
		}
	}
	if !swept {
		t.Fatal("auth_events was not swept")
	}
	if got := count(t, pool, "auth_events"); got != 2 {
		t.Fatalf("%d auth events remain, want 2", got)
	}
}

// The boundary is exclusive: a row exactly at the cutoff is kept.
func TestSweepBoundary(t *testing.T) {
	s, pool := newSweeper(t)
	cutoff := now.Add(-24 * time.Hour) // login_attempts are kept one day

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO login_attempts (bucket, subject, succeeded, created_at)
		VALUES ('account', 'a@example.test', false, $1)`, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO login_attempts (bucket, subject, succeeded, created_at)
		VALUES ('account', 'b@example.test', false, $1)`, cutoff.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var subjects []string
	rows, err := pool.Query(context.Background(),
		`SELECT subject FROM login_attempts ORDER BY subject`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, s)
	}
	if len(subjects) != 1 || subjects[0] != "a@example.test" {
		t.Fatalf("remaining attempts = %v, want only the row exactly at the cutoff", subjects)
	}
}

// Financial records must never be touched, whatever a policy says.
func TestSweeperRefusesUnauthorisedTables(t *testing.T) {
	s, pool := newSweeper(t)
	ctx := context.Background()

	// Point a policy at the invoice table.
	if _, err := pool.Exec(ctx, `
		INSERT INTO retention_policies (table_name, keep_for, age_column)
		VALUES ('invoices', interval '1 day', 'created_at')`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Run(ctx); err == nil {
		t.Fatal("the sweeper accepted a policy naming invoices")
	}

	// And nothing was deleted from the auth tables either: the sweep stops rather
	// than half-applying an unsafe policy set.
	insertAuthEvent(t, pool, now.AddDate(0, 0, -400))
	if _, err := s.Run(ctx); err == nil {
		t.Fatal("second sweep succeeded despite the unsafe policy")
	}
	if got := count(t, pool, "auth_events"); got != 1 {
		t.Fatalf("%d auth events remain, want 1 (nothing should have been swept)", got)
	}
}

func TestSweeperRefusesBadColumn(t *testing.T) {
	s, pool := newSweeper(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		UPDATE retention_policies SET age_column = 'id' WHERE table_name = 'auth_events'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx); err == nil {
		t.Fatal("the sweeper accepted an unsupported age column")
	}
}

// A large backlog is deleted in bounded batches, so the sweep cannot hold a long
// lock on a busy table.
func TestSweepIsBatched(t *testing.T) {
	s, pool := newSweeper(t)
	ctx := context.Background()
	s.Batch = 10

	for i := 0; i < 35; i++ {
		insertAuthEvent(t, pool, now.AddDate(0, 0, -200))
	}

	results, err := s.Run(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var deleted int64
	for _, r := range results {
		if r.Table == "auth_events" {
			deleted = r.Deleted
		}
	}
	if deleted != 10 {
		t.Fatalf("deleted %d in one sweep, want the batch size 10", deleted)
	}
	if got := count(t, pool, "auth_events"); got != 25 {
		t.Fatalf("%d remain, want 25 (the rest wait for the next sweep)", got)
	}
}
