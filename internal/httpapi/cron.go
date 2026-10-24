// Cron run history and the worker heartbeat (OPS-08, ADM-22, OPS-16).
//
// `internal/ops` answers "what is broken right now". This answers a different question:
// "did the renewals actually run, and when". Those are not the same thing — a job that has
// not run for two days produces no exceptions, because it produced no invoices either —
// and an operator who cannot tell "nothing was due" from "the worker is dead" will find
// out from a customer.
//
// So there are two tables and two readers:
//
//   - `cron_runs` is one row per job run, with its outcome and how many items it touched.
//     The worker records it through RecordRun, which returns a handle the job closes when
//     it finishes; that is what makes a run that panicked visible as `running` past its
//     window rather than invisible.
//   - `worker_heartbeat` is one row per worker, upserted on every tick through Beat. The
//     reader reports it with `stale` derived from the age, because a heartbeat row that
//     exists is not the same as a worker that is alive: a process killed nine hours ago
//     leaves a row behind forever.
//
// Both writers take a pool and a context and nothing else, because the worker lives in
// package main and must be able to record without depending on the HTTP layer.

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("43-operator-cron", registerOperatorCron)
}

func registerOperatorCron(r *router) {
	r.staffGet("/api/v1/staff/cron", authn.PermLogRead, r.s.handleCronRuns)
	r.staffGet("/api/v1/staff/cron/heartbeat", authn.PermLogRead, r.s.handleHeartbeat)
}

// HeartbeatStaleAfter is how long a heartbeat may be silent before the worker is reported
// as stale.
//
// It is a constant rather than a setting because it is a property of the loop, not of the
// installation: the worker ticks every few seconds, so two minutes of silence is a dead
// worker or a blocked database, and either is worth showing. A configurable threshold
// would let a broken deployment hide behind a large number.
const HeartbeatStaleAfter = 2 * time.Minute

// heartbeatID is the row key used before heartbeats were per-instance.
//
// It is kept only so the migration that removed it, and any test asserting on the legacy shape, can
// name it. No code writes this id any more: a singleton row is what made two workers overwrite each
// other, so the panel could never say how many were running.
const heartbeatID = "worker"

// cronRunRow is one run in the history.
type cronRunRow struct {
	ID         string     `json:"id"`
	Job        string     `json:"job"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Outcome    string     `json:"outcome"`
	Detail     string     `json:"detail"`
	Items      int        `json:"items"`
	// DurationMS is derived, and null while the run is still going.
	DurationMS *int64 `json:"duration_ms"`
}

// handleCronRuns lists recent runs, newest first.
//
// Query: job (exact name), outcome (running|ok|failed|skipped), limit, cursor.
func (s *Server) handleCronRuns(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.logPageParams(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	args := []any{}
	where := []string{"true"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconvItoa(len(args))))
	}
	if job := strings.TrimSpace(q.Get("job")); job != "" {
		add("job = ?", job)
	}
	if outcome := strings.TrimSpace(q.Get("outcome")); outcome != "" {
		add("outcome = ?", outcome)
	}
	if cursor != "" {
		when, id, err := decodeLogCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, "(started_at, id) < ($"+strconvItoa(len(args)-1)+", $"+strconvItoa(len(args))+")")
	}
	args = append(args, limit+1)

	stmt := `
		SELECT id, job, started_at, finished_at, outcome, detail, items
		FROM cron_runs
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY started_at DESC, id DESC
		LIMIT $` + strconvItoa(len(args))

	rows, err := s.pool.Query(r.Context(), stmt, args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "cron runs failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]cronRunRow, 0, limit)
	for rows.Next() {
		var row cronRunRow
		if err := rows.Scan(&row.ID, &row.Job, &row.StartedAt, &row.FinishedAt,
			&row.Outcome, &row.Detail, &row.Items); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		if row.FinishedAt != nil {
			ms := row.FinishedAt.Sub(row.StartedAt).Milliseconds()
			row.DurationMS = &ms
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		c := paginate.Encode(last.StartedAt.UTC().Format(time.RFC3339Nano), last.ID)
		next = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, Page[cronRunRow]{Data: out, NextCursor: next})
}

// heartbeatJSON is the heartbeat response.
type heartbeatJSON struct {
	// ID is this worker's instance id — one row per process, so the row count is the worker count.
	// It is not the host name: two workers can share a host, and the same worker restarted is a
	// new instance.
	ID string `json:"id"`
	// BeatAt is when the worker last reported.
	BeatAt time.Time `json:"beat_at"`
	// AgeSeconds is how long ago that was, derived at read time.
	AgeSeconds int64 `json:"age_seconds"`
	// Jobs is how many jobs the worker is running.
	Jobs int `json:"jobs"`
	// Version is the build the worker is running, so a mixed-version fleet is visible.
	Version string `json:"version"`
	// Stale is true when the beat is older than HeartbeatStaleAfter. It is the only field
	// an operator needs to look at, and deriving it here means the client cannot get the
	// comparison wrong.
	Stale bool `json:"stale"`
	// StaleAfterSeconds documents the threshold the server used.
	StaleAfterSeconds int64 `json:"stale_after_seconds"`
}

// handleHeartbeat reports the worker heartbeat.
//
// A missing row is not a 404: "the worker has never started" is exactly the answer an
// operator needs, and a 404 renders as an error page rather than as that sentence.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	now := time.Now().UTC()
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, beat_at, jobs, version FROM worker_heartbeat
		ORDER BY beat_at DESC, id`)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "heartbeat failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	instances := make([]heartbeatJSON, 0, 2)
	for rows.Next() {
		var (
			id      string
			beatAt  time.Time
			jobs    int
			version string
		)
		if err := rows.Scan(&id, &beatAt, &jobs, &version); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "read heartbeat row", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		age := now.Sub(beatAt)
		if age < 0 {
			// A clock that is ahead of ours is not stale; clamping avoids a negative age in the
			// response, which a client would render as "in the future".
			age = 0
		}
		instances = append(instances, heartbeatJSON{
			ID:                id,
			BeatAt:            beatAt.UTC(),
			AgeSeconds:        int64(age.Seconds()),
			Jobs:              jobs,
			Version:           version,
			Stale:             age > HeartbeatStaleAfter,
			StaleAfterSeconds: int64(HeartbeatStaleAfter.Seconds()),
		})
	}
	if err := rows.Err(); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read heartbeats", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	if len(instances) == 0 {
		// A worker that has never run leaves no row. Reporting that as an empty heartbeat
		// is honest and renders; a 500 would say the panel is broken.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"heartbeat":           nil,
			"present":             false,
			"stale":               true,
			"stale_after_seconds": int64(HeartbeatStaleAfter.Seconds()),
			"detail":              "no worker has reported a heartbeat",
			"workers":             0,
			"instances":           instances,
		})
		return
	}

	// `heartbeat` stays the newest instance so every existing client keeps rendering one card, and
	// `workers` is the fact those clients could not previously express. The headline is stale when
	// the NEWEST beat is stale: if the freshest row is old, nothing is beating.
	newest := instances[0]
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"heartbeat": newest,
		"present":   true,
		"stale":     newest.Stale,
		"workers":   len(instances),
		"instances": instances,
	})
}

// ------------------------------------------------------------------- writers --

// RecordRun writes one cron_runs row and returns its id.
//
//	func RecordRun(ctx context.Context, pool *store.Pool, job, outcome, detail string, items int) (string, error)
//
// It is the simple form: a job that has already finished records what happened. A job that
// wants its running state to be visible uses StartRun and FinishRun instead, because a row
// that only appears on success cannot answer "is it stuck".
func RecordRun(ctx context.Context, pool *store.Pool, job, outcome, detail string, items int) (string, error) {
	if pool == nil {
		return "", nil
	}
	outcome = normalizeOutcome(outcome)
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO cron_runs (job, outcome, detail, items, finished_at)
		VALUES ($1, $2, $3, $4, CASE WHEN $2 = 'running' THEN NULL ELSE now() END)
		RETURNING id`,
		truncate(strings.TrimSpace(job), 120), outcome,
		truncate(redactLogText(detail), 4000), items).Scan(&id)
	if err != nil {
		return "", err
	}
	if outcome == "failed" {
		alertCronFailure(ctx, pool, id, job, detail)
	}
	return id, nil
}

// StartRun records that a job has begun, and returns its id.
//
//	func StartRun(ctx context.Context, pool *store.Pool, job string) (string, error)
//
// A failure to record is returned but should not stop the job: the work is more important
// than its bookkeeping. The id is what FinishRun updates, so a run that panics leaves a
// `running` row that is visible in the viewer.
func StartRun(ctx context.Context, pool *store.Pool, job string) (string, error) {
	return RecordRun(ctx, pool, job, "running", "", 0)
}

// FinishRun closes a run opened by StartRun.
//
//	func FinishRun(ctx context.Context, pool *store.Pool, id, outcome, detail string, items int) error
//
// It updates only a row that is still `running`, so a run closed twice (a deferred call and
// an explicit one) cannot overwrite the first outcome.
func FinishRun(ctx context.Context, pool *store.Pool, id, outcome, detail string, items int) error {
	if pool == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	outcome = normalizeOutcome(outcome)
	if outcome == "running" {
		outcome = "ok"
	}
	tag, err := pool.Exec(ctx, `
		UPDATE cron_runs
		SET outcome = $2, detail = $3, items = $4, finished_at = now()
		WHERE id = $1 AND outcome = 'running'`,
		id, outcome, truncate(redactLogText(detail), 4000), items)
	if err != nil {
		return err
	}
	// Only when the update actually closed the run: a second close is a no-op there, and alerting
	// on a row this call did not change would mail about a failure that was already reported.
	if outcome == "failed" && tag.RowsAffected() > 0 {
		alertCronFailure(ctx, pool, id, "", detail)
	}
	return err
}

// alertCronFailure queues the operator's "a scheduled job failed" mail (NTF-09).
//
// It runs after the run row is written, in its own transaction, and never returns an error to the
// caller: a failure to record the run must not be compounded by a failure to announce it, and a
// job runner that dies because its alert could not be queued is a worse outage than the one being
// reported. The dedupe key is the run id, so RecordRun and FinishRun reporting the same failure
// produce one message.
func alertCronFailure(ctx context.Context, pool *store.Pool, runID, job, detail string) {
	if pool == nil || strings.TrimSpace(runID) == "" {
		return
	}
	queue := notify.NewQueue(pool)
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		in := notify.CronFailureInput{
			Job:      job,
			RunID:    runID,
			Detail:   detail,
			FailedAt: time.Now().UTC(),
		}
		if in.Job == "" {
			// FinishRun knows only the row, so the job name is read from it rather than left
			// blank: a subject that says "Scheduled job failed: unknown job" is not actionable.
			_ = tx.QueryRow(ctx, `SELECT job FROM cron_runs WHERE id = $1`, runID).Scan(&in.Job)
		}
		_, err := notify.SendCronFailure(ctx, tx, queue, in)
		return err
	})
	if err != nil {
		// Best-effort, like the Discord mirror: the run row is the durable record, and the alert is
		// the convenience. It is logged so an operator who did not receive it can see why.
		slog.Default().Warn("cron failure alert was not queued", "run", runID, "error", err)
	}
}

// normalizeOutcome folds an unknown outcome to "failed".
//
// The column is a closed CHECK set. Folding rather than failing means a job that reports
// "timeout" is recorded as failed and visible, instead of losing its row to a constraint
// error the caller would probably swallow.
func normalizeOutcome(outcome string) string {
	switch strings.TrimSpace(outcome) {
	case "running", "ok", "failed", "skipped":
		return strings.TrimSpace(outcome)
	default:
		return "failed"
	}
}

// Beat upserts this worker's heartbeat.
//
//	func Beat(ctx context.Context, pool *store.Pool, instanceID string, jobs int, version string) error
//
// `instanceID` identifies the PROCESS, not the installation: one row per worker, so the reader's row
// count is the number of workers actually running. Before this was per-instance the table had a
// single row keyed 'worker', and two workers — `BILLING_EMBEDDED_WORKER=1` plus a separate
// `billing worker` is the easy way to get two — silently overwrote each other. The panel could show
// that *a* worker was alive and could never show that there were two, which is exactly the
// misconfiguration worth seeing.
//
// Rows older than 24 hours are pruned in the same statement. Instances accumulate on every restart,
// and a table that only grows turns "how many workers are running" into "how many have ever run".
// The prune is an index range scan that normally finds nothing, which is why it can afford to run on
// every tick rather than on a timer of its own.
func Beat(ctx context.Context, pool *store.Pool, instanceID string, jobs int, version string) error {
	if pool == nil {
		return nil
	}
	if strings.TrimSpace(instanceID) == "" {
		// An empty id would collide with every other instance that also failed to supply one, which
		// is the singleton behaviour this change removed. Refusing is better than silently
		// re-creating the bug.
		return fmt.Errorf("httpapi: heartbeat requires an instance id")
	}
	_, err := pool.Exec(ctx, `
		WITH pruned AS (
		    DELETE FROM worker_heartbeat WHERE beat_at < now() - interval '24 hours'
		)
		INSERT INTO worker_heartbeat (id, beat_at, jobs, version)
		VALUES ($1, now(), $2, $3)
		ON CONFLICT (id) DO UPDATE
			SET beat_at = now(), jobs = EXCLUDED.jobs, version = EXCLUDED.version`,
		truncate(strings.TrimSpace(instanceID), 120), jobs, truncate(strings.TrimSpace(version), 120))
	return err
}

// isNoRows reports whether err is pgx's "no rows" sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// strconvItoa is strconv.Itoa, named so the SQL builders read uniformly.
func strconvItoa(n int) string { return strconv.Itoa(n) }
