package tax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// The history, the provenance and the schedule.
//
// A run is a record, not a log line: "did the refresh run, and what did it change" is a question an
// operator asks when a customer disputes a price, and the answer has to outlive the worker's log
// rotation. `tax_import_runs` keeps the counts and the full per-row report.

// Runs returns recent imports, newest first.
func (i *Importer) Runs(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := i.pool.Query(ctx, `
		SELECT id, source_url, format, trigger, COALESCE(actor_id::text, ''), fetched_at,
		       started_at, finished_at, outcome, detail, rows_total, created, updated,
		       unchanged, protected, rejected, report
		  FROM tax_import_runs
		 ORDER BY started_at DESC, id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("tax: list import runs: %w", err)
	}
	defer rows.Close()
	out := make([]Run, 0, limit)
	for rows.Next() {
		var (
			run    Run
			report []byte
		)
		if err := rows.Scan(&run.ID, &run.SourceURL, &run.Format, &run.Trigger, &run.ActorID,
			&run.FetchedAt, &run.StartedAt, &run.FinishedAt, &run.Outcome, &run.Detail,
			&run.Counts.Rows, &run.Counts.Created, &run.Counts.Updated, &run.Counts.Unchanged,
			&run.Counts.Protected, &run.Counts.Rejected, &report); err != nil {
			return nil, fmt.Errorf("tax: scan import run: %w", err)
		}
		if len(report) > 0 {
			var preview Preview
			if err := json.Unmarshal(report, &preview); err == nil {
				run.Preview = &preview
			}
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Provenance returns every rate's origin.
//
// It is a separate read rather than a field on the rate list because the rate list is
// `internal/commerce`'s projection, shared with the pricer and the staff form; provenance is this
// package's fact. The screen joins them by id, and the join is by id rather than by jurisdiction so
// a closed rate's origin survives the row that replaced it.
func (i *Importer) Provenance(ctx context.Context) ([]Provenance, error) {
	rows, err := i.pool.Query(ctx, `
		SELECT id, origin, source, fetched_at
		  FROM tax_rates
		 ORDER BY country_code, region_code, tax_class, effective_from DESC`)
	if err != nil {
		return nil, fmt.Errorf("tax: list rate provenance: %w", err)
	}
	defer rows.Close()
	out := make([]Provenance, 0, 32)
	for rows.Next() {
		var p Provenance
		if err := rows.Scan(&p.RateID, &p.Origin, &p.Source, &p.FetchedAt); err != nil {
			return nil, fmt.Errorf("tax: scan rate provenance: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ schedule --

// retryAfterFailure is how long the worker waits after a failed run before trying again.
//
// It is shorter than a refresh interval on purpose: a source that was briefly down should be
// retried in the same day, and a *working* source should be polled at the operator's cadence. A
// single interval for both would either hammer a broken source or leave a rate table a day stale
// after one 502.
const retryAfterFailure = 15 * time.Minute

// due reports whether the scheduled refresh should fetch now.
//
// The rule is derived from the run history rather than from a mutable "next refresh" column,
// because a restarted worker must not lose the schedule and two workers must not both think they
// are due. The latest run decides: a successful one waits for the configured interval, a failed one
// waits retryAfterFailure, and no run at all is due immediately.
func (i *Importer) due(ctx context.Context, src Source) (bool, error) {
	var (
		startedAt time.Time
		outcome   string
	)
	err := i.pool.QueryRow(ctx, `
		SELECT started_at, outcome
		  FROM tax_import_runs
		 ORDER BY started_at DESC, id DESC
		 LIMIT 1`).Scan(&startedAt, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("tax: read last import run: %w", err)
	}
	wait := src.RefreshInterval
	if wait <= 0 {
		wait = DefaultRefreshInterval
	}
	if outcome == OutcomeRunFailed {
		wait = retryAfterFailure
	}
	return !i.now().Before(startedAt.Add(wait)), nil
}

// Refresh is the scheduled entry point.
//
// It returns nil — and does nothing — when there is no source, when the source is switched off, or
// when the last run is younger than the configured interval. Those are not failures: a fresh
// installation has no source, and a worker that reported "failed" every tick because of it would
// fill the cron history with noise that hides a real failure.
//
// It never overwrites a manual rate. A cron job cannot ask the operator for permission, so it does
// not have any: the protection is `OverwriteManual = false`, and the run report names every
// jurisdiction it left alone.
func (i *Importer) Refresh(ctx context.Context) (*Result, error) {
	src, err := i.Source(ctx)
	if errors.Is(err, ErrNoSource) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !src.Enabled {
		return nil, nil
	}
	due, err := i.due(ctx, src)
	if err != nil {
		return nil, err
	}
	if !due {
		return nil, nil
	}
	return i.Apply(ctx, Options{Trigger: TriggerScheduled})
}

// RefreshJob returns the run function the worker registers.
//
// The pool is resolved per call rather than captured, so the job works in a worker that was started
// before the pool was handed over, and a test can point it at its own database.
func RefreshJob(pool *store.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		result, err := New(pool).Refresh(ctx)
		if err != nil {
			if logger != nil {
				logger.Warn("tax rate refresh failed", "error", err)
			}
			return err
		}
		if result != nil && logger != nil {
			counts := result.Run.Counts
			logger.Info("tax rates refreshed",
				"source", result.Run.SourceURL,
				"created", counts.Created, "updated", counts.Updated,
				"unchanged", counts.Unchanged, "protected", counts.Protected,
				"rejected", counts.Rejected)
		}
		return nil
	}
}

// JobName and JobInterval are the worker's registration, named here so the automation screen and
// cmd/billing/jobs.go cannot disagree about them.
const (
	// JobName is the job's name in `cron_runs` and on the automation screen.
	JobName = "tax-rate-refresh"
	// JobInterval is how often the worker *ticks*. Whether a tick fetches is decided by the
	// source's own refresh interval (see Refresh), because "how stale may a rate be" is the
	// installation's business decision while the tick is a property of the loop.
	JobInterval = time.Hour
)
