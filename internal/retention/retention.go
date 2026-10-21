// Package retention deletes bookkeeping rows that have outlived their usefulness.
//
// It exists because "we log every login attempt" and "we never delete anything" are
// the same sentence in production: the tables grow without bound, and an attacker can
// drive that growth deliberately. Deleting old rows on a schedule turns an unbounded
// leak into a bounded cost.
//
// What it must never do: delete anything financial or evidentiary before its
// retention period. The policies live in the database, so changing one is a reviewed
// data change rather than a code deploy, and the sweep refuses to touch a table that
// has no policy.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/store"
)

// Result reports one table's sweep.
type Result struct {
	Table   string
	Deleted int64
	KeepFor time.Duration
}

// Sweeper deletes expired rows.
type Sweeper struct {
	pool *store.Pool
	// Batch bounds how many rows one statement deletes, so a large backlog does not
	// hold a long lock on a busy table.
	Batch int
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs a Sweeper.
func New(pool *store.Pool) *Sweeper {
	return &Sweeper{pool: pool, Batch: 5000, Now: func() time.Time { return time.Now().UTC() }}
}

// allowedTables is the closed set the sweeper may touch.
//
// The table name is interpolated into the DELETE because PostgreSQL cannot take an
// identifier as a parameter. A closed set makes that safe: a name not on this list is
// never interpolated, so the retention table cannot be used to point the sweeper at
// invoices or journal entries.
var allowedTables = map[string]bool{
	"login_attempts": true,
	"auth_events":    true,
	"gateway_events": true,
}

// allowedColumns is the closed set of age columns.
var allowedColumns = map[string]bool{
	"created_at":  true,
	"received_at": true,
}

// Run sweeps every table that has a policy.
func (s *Sweeper) Run(ctx context.Context) ([]Result, error) {
	type policy struct {
		table  string
		keep   time.Duration
		column string
	}

	rows, err := s.pool.Query(ctx, `
		SELECT table_name, extract(epoch FROM keep_for)::bigint, age_column
		FROM retention_policies
		ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("retention: read policies: %w", err)
	}
	var policies []policy
	for rows.Next() {
		var (
			p    policy
			secs int64
		)
		if err := rows.Scan(&p.table, &secs, &p.column); err != nil {
			rows.Close()
			return nil, fmt.Errorf("retention: scan policy: %w", err)
		}
		p.keep = time.Duration(secs) * time.Second
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("retention: read policies: %w", err)
	}
	rows.Close()

	// Validate the whole policy set before deleting anything. A single unsafe policy
	// must abort the sweep entirely: validating per table would delete from the safe
	// tables first and then stop, which is a half-applied policy change on live data.
	for _, p := range policies {
		if !allowedTables[p.table] {
			return nil, fmt.Errorf(
				"retention: policy names table %q which the sweeper may not delete from", p.table)
		}
		if !allowedColumns[p.column] {
			return nil, fmt.Errorf(
				"retention: policy for %q names unsupported age column %q", p.table, p.column)
		}
		if p.keep <= 0 {
			return nil, fmt.Errorf("retention: policy for %q has a non-positive retention", p.table)
		}
	}

	var out []Result
	for _, p := range policies {
		cutoff := s.Now().Add(-p.keep)
		// Both the table and the column come from the closed sets above, so the
		// interpolation cannot carry user input.
		stmt := fmt.Sprintf(`
			DELETE FROM %s
			WHERE ctid IN (
				SELECT ctid FROM %s WHERE %s < $1 LIMIT $2
			)`, p.table, p.table, p.column)

		tag, err := s.pool.Exec(ctx, stmt, cutoff, s.Batch)
		if err != nil {
			return out, fmt.Errorf("retention: sweep %s: %w", p.table, err)
		}
		out = append(out, Result{Table: p.table, Deleted: tag.RowsAffected(), KeepFor: p.keep})
	}
	return out, nil
}
