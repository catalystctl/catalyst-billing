// The failed-job viewer, the cron statistics and the on-demand retry (OPS-09, OPS-13).
//
// `cron_runs` and `worker_heartbeat` had writers and two readers — a list of recent runs and one
// heartbeat — and neither answered the question an operator actually has when a job has been
// failing since 03:00: *which job, how often, and can I run it again now?* The run list was
// every job interleaved, so a failing one scrolled off it; there was no aggregate at all; and the
// only way to re-run a failed rule was to restart the worker.
//
// Three answers, and they are deliberately separate routes because they are separate questions:
//
//   - **`GET /cron/failures`** is the same table filtered to `outcome = 'failed'` — one list, one
//     question, and it is paginated the same way the run list is.
//   - **`GET /cron/statistics`** is one aggregate per job over a window: how many runs, how many
//     failures, how long a run takes, and when it last succeeded. A job that is *not* failing but
//     has not run in a day is the failure mode a failure list cannot show, which is why
//     `last_run_at` and `last_success_at` are in the same row.
//   - **`POST /cron/{job}/retry`** runs the rule once, now, and records the attempt in the same
//     `cron_runs` table the worker writes. It re-runs the *rule*, not the worker's schedule.
//
// # What the retry can and cannot do
//
// A job is a closure over a pool, and only the jobs a package exposes can be called from here:
// `lifecycle.Jobs()` — which also carries the two maintenance rules from internal/operations.
// The jobs that live in `cmd/billing` (payments, provisioning, renewals, notifications …) are
// `package main`: they need a gateway client, a provisioner and a mail transport, and they cannot
// be constructed from an HTTP handler. Those answer `job_not_retryable` with that sentence in the
// detail rather than a 500, because "you cannot do this from the panel" is information.
//
// # Why the retry is audited but the run is not
//
// The audit row is the operator's action ("this person re-ran the order-cancellation rule at
// 09:14"), and it is written in its own transaction: the job's work commits or rolls back on its
// own, and an audit row claiming an action is only written when the action was actually attempted.
// The run row carries the outcome, so the audit row does not need to duplicate it.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("43-operator-cron-failures", registerCronFailures)
}

func registerCronFailures(r *router) {
	// log:read, like the rest of the operator log surface: the run history is a log, and support
	// holds that permission because "the renewals did not run" is a support question.
	r.staffGet("/api/v1/staff/cron/failures", authn.PermLogRead, r.s.handleCronFailures)
	r.staffGet("/api/v1/staff/cron/statistics", authn.PermLogRead, r.s.handleCronStatistics)

	// settings:write rather than log:read for the retry: the retry does not read a log, it runs a
	// rule that voids invoices and cancels orders. It is the same permission the automation screen
	// uses to describe those rules, which is the one that already means "may change how this
	// installation behaves".
	r.staffMutation(http.MethodPost, "/api/v1/staff/cron/{job}/retry",
		authn.PermSettingsWrite, r.s.handleCronRetry)
}

// retryTimeout bounds one retry. A job that has hung is exactly the job an operator retries, and
// a request held open by it would be a second hung thing to look at. The run row it leaves behind
// stays `running`, which is the honest record that it did not finish.
const retryTimeout = 60 * time.Second

// handleCronFailures lists failed runs, newest first.
//
// Query: job (exact name), from (RFC3339), limit, cursor.
//
// A failed run whose row still says `running` is not listed: it has not failed, it has not
// finished, and the statistics row reports it as `running` so a stuck job is visible there
// instead of being guessed at here.
func (s *Server) handleCronFailures(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.logPageParams(w, r)
	if !ok {
		return
	}

	args := []any{}
	where := []string{"outcome = 'failed'"}
	if job := strings.TrimSpace(r.URL.Query().Get("job")); job != "" {
		args = append(args, job)
		where = append(where, "job = $"+strconv.Itoa(len(args)))
	}
	from, ok := s.logTimeParam(w, r, "from")
	if !ok {
		return
	}
	if from != nil {
		args = append(args, *from)
		where = append(where, "started_at >= $"+strconv.Itoa(len(args)))
	}
	if cursor != "" {
		when, id, err := decodeLogCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, "(started_at, id) < ($"+strconv.Itoa(len(args)-1)+
			", $"+strconv.Itoa(len(args))+")")
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(r.Context(), `
		SELECT id, job, started_at, finished_at, outcome, detail, items
		FROM cron_runs
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY started_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "cron failures failed", "error", err)
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

// cronJobStats is one job's aggregate over the window.
type cronJobStats struct {
	Job string `json:"job"`
	// Runs is every run started in the window, including one still going.
	Runs     int `json:"runs"`
	Failures int `json:"failures"`
	Running  int `json:"running"`
	// Skipped is a run that decided there was nothing to do; it is neither a success nor a
	// failure and counting it as either would hide a rule that is switched off by a threshold.
	Skipped int `json:"skipped"`
	// AvgDurationMS and MaxDurationMS are derived from finished runs only: a run still going has
	// no duration, and counting it as zero would flatter the average.
	AvgDurationMS int64 `json:"avg_duration_ms"`
	MaxDurationMS int64 `json:"max_duration_ms"`
	// LastRunAt, LastSuccessAt and LastFailureAt are the three timestamps an operator reads to
	// tell "failing" from "not running at all".
	LastRunAt     *time.Time `json:"last_run_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastFailureAt *time.Time `json:"last_failure_at"`
	// LastOutcome is the outcome of the most recent run.
	LastOutcome string `json:"last_outcome"`
	// LastDetail is that run's detail, redacted on the way in and again on the way out.
	LastDetail string `json:"last_detail"`
}

// handleCronStatistics aggregates the run history per job over a window.
//
// Query: days (1..90, default 7). A window rather than "all time" because the question is "is it
// healthy now", and a job that failed four hundred times last quarter and not since is healthy.
func (s *Server) handleCronStatistics(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	days := 7
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 90 {
			writeProblem(w, http.StatusBadRequest, "invalid_window", "days must be between 1 and 90")
			return
		}
		days = parsed
	}
	since := time.Now().UTC().AddDate(0, 0, -days)

	rows, err := s.pool.Query(r.Context(), `
		-- The CTE is named "recent", not "window": WINDOW is a reserved word in PostgreSQL and the
		-- query fails at parse time with a message that says nothing about the cause.
		WITH recent AS (
		    SELECT * FROM cron_runs WHERE started_at >= $1
		), latest AS (
		    SELECT DISTINCT ON (job) job, outcome, detail
		    FROM recent ORDER BY job, started_at DESC, id DESC
		)
		SELECT w.job,
		       count(*) AS runs,
		       count(*) FILTER (WHERE w.outcome = 'failed')  AS failures,
		       count(*) FILTER (WHERE w.outcome = 'running') AS running,
		       count(*) FILTER (WHERE w.outcome = 'skipped') AS skipped,
		       COALESCE(avg(EXTRACT(EPOCH FROM (w.finished_at - w.started_at)) * 1000)
		                FILTER (WHERE w.finished_at IS NOT NULL), 0)::bigint AS avg_ms,
		       COALESCE(max(EXTRACT(EPOCH FROM (w.finished_at - w.started_at)) * 1000)
		                FILTER (WHERE w.finished_at IS NOT NULL), 0)::bigint AS max_ms,
		       max(w.started_at) AS last_run_at,
		       max(w.finished_at) FILTER (WHERE w.outcome = 'ok')   AS last_ok_at,
		       max(w.finished_at) FILTER (WHERE w.outcome = 'failed') AS last_failed_at,
		       l.outcome, l.detail
		FROM recent w
		JOIN latest l ON l.job = w.job
		GROUP BY w.job, l.outcome, l.detail
		ORDER BY w.job`, since)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "cron statistics failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]cronJobStats, 0, 16)
	for rows.Next() {
		var (
			stat   cronJobStats
			detail string
		)
		if err := rows.Scan(&stat.Job, &stat.Runs, &stat.Failures, &stat.Running, &stat.Skipped,
			&stat.AvgDurationMS, &stat.MaxDurationMS, &stat.LastRunAt, &stat.LastSuccessAt,
			&stat.LastFailureAt, &stat.LastOutcome, &detail); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		stat.LastDetail = redactLogText(detail)
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"window_days": days,
		"since":       since,
		"data":        out,
		// Said here rather than inferred by the client: a job that is registered but has never
		// run produces no row in this list, and an operator must not read "not listed" as "not
		// scheduled".
		"note": "only jobs that have run inside the window are listed; the automation screen lists every registered job",
	})
}

// retryableJobs resolves the jobs this process can run, by name.
//
// It is lifecycle.Jobs(), which also carries the maintenance rules from internal/operations. The
// same function the worker iterates is the only honest source: a job that is not in it is not
// registered, and running something the worker does not schedule would be a second list.
func (s *Server) retryableJobs() map[string]lifecycle.Job {
	out := make(map[string]lifecycle.Job, 8)
	for _, job := range lifecycle.Jobs(s.pool, s.cfg.Logger) {
		out[job.Name] = job
	}
	return out
}

// handleCronRetry runs one job now and records the attempt.
//
// The response is always 200 for a job that ran, even when the job failed: the request succeeded
// in running it, and the outcome belongs in the body and in `cron_runs`. A caller that wants a
// failure signal reads `outcome`.
func (s *Server) handleCronRetry(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	name := strings.TrimSpace(r.PathValue("job"))
	if name == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_job", "a job name is required")
		return
	}

	jobs := s.retryableJobs()
	job, ok := jobs[name]
	if !ok {
		// Two different refusals, one route. A name that exists in cmd/billing is a job this
		// process cannot construct, and saying so is more useful than "not found".
		writeProblem(w, http.StatusUnprocessableEntity, "job_not_retryable",
			"the panel cannot re-run "+name+" itself: it is registered in the worker process, "+
				"which is the only place it can be built. Restart the worker, or wait for its next tick.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), retryTimeout)
	defer cancel()

	// The run is recorded with the same two writers the worker uses, so a retry is
	// indistinguishable in the history from a scheduled run — except that it exists at a
	// different time, which is exactly what the operator asked for.
	runID, startErr := StartRun(ctx, s.pool, job.Name)
	if startErr != nil {
		s.cfg.Logger.ErrorContext(ctx, "record retry run failed", "job", job.Name, "error", startErr)
	}

	started := time.Now().UTC()
	runErr := job.Run(ctx)
	duration := time.Since(started)

	outcome := "ok"
	detail := ""
	if runErr != nil {
		outcome = "failed"
		detail = runErr.Error()
		if errors.Is(runErr, context.DeadlineExceeded) {
			detail = "the retry timed out after " + retryTimeout.String() + "; it may still be running"
		}
	}
	if err := FinishRun(ctx, s.pool, runID, outcome, detail, 0); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "finish retry run failed", "job", job.Name, "error", err)
	}

	// The audit row names the operator and the rule. It is written after the run so that a
	// request that died before running anything leaves no claim that it ran something.
	// The job's name is in `changes`, not in `target_id`: the audit table's target is a uuid and
	// a job name is not one, so a row keyed by "order-cancel" would be an insert error at the
	// moment an operator needed the record.
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "cron.retry", "cron_job", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"job":         job.Name,
			"run_id":      runID,
			"outcome":     outcome,
			"duration_ms": duration.Milliseconds(),
		}))
	}); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit retry failed", "job", job.Name, "error", err)
	}

	if runErr != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "cron retry failed", "job", job.Name, "error", runErr)
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"job":         job.Name,
		"run_id":      runID,
		"outcome":     outcome,
		"detail":      redactLogText(detail),
		"duration_ms": duration.Milliseconds(),
	})
}
