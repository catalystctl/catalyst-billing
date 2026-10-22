// Package lifecycle owns the two rules that end something on a timer.
//
// Two settings have been sitting in the table since the settings screen was written and
// nothing has ever read them: `cronjob.order_cancel_days` and `cronjob.terminate_days`. They
// are promises to an operator — "an abandoned order is cleaned up", "a service suspended for
// too long is taken away" — and this package is what makes them true.
//
// # The shape of a job here
//
// Both jobs are the same three steps, and the order matters:
//
//  1. Read the threshold through internal/settings. A threshold that is missing falls back to
//     the code default; a threshold that is *zero or negative* means "this rule is off", not
//     "everything is overdue". That distinction is the difference between a misconfigured
//     installation doing nothing and a misconfigured installation cancelling every order it
//     has.
//  2. Scan for a bounded batch of ids from persisted state. Nothing is remembered between
//     runs: the work is derived from the rows themselves, so a restarted worker, a second
//     worker, or a run at 03:00 instead of 02:00 all produce the same answer.
//  3. Do one row per transaction, and decide again *inside* that transaction against locked
//     rows. A scan is a hint; the transaction is the decision.
//
// # What these jobs must never do
//
//   - Never write a destructive state directly. Termination goes through
//     `service_operations` so it inherits the queue's leases, fencing token and
//     reconciliation. A service row updated by a cron is a service nobody can account for.
//   - Never delete or edit an issued invoice. A cancelled order's invoice is *voided* through
//     the same `open -> void` transition the staff handler uses, so the number stays burned
//     and the trigger in migration 0008 still has the last word.
//   - Never override a human. A manual hold (`service_holds`) outranks every automated rule,
//     and the check is repeated inside the transaction that enqueues the work.
//   - Never act on a customer who has paid. The order path re-reads the invoice's paid amount
//     and refuses to cancel; the termination path asks whether the customer still owes
//     anything at all, which is the same question dunning's resume rule asks in reverse.
//
// The integrator wires this package in cmd/billing/jobs.go with one line per job:
//
//	for _, job := range lifecycle.Jobs(pool, logger) {
//	    jobs = append(jobs, jobSpec{name: job.Name, interval: job.Interval, run: job.Run})
//	}
package lifecycle

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
)

// Job is one periodic task.
//
// It is declared here rather than reusing the worker's own type so this package does not
// depend on cmd/billing: the rule and the runner that calls it are separable, and a test
// drives the rule without a worker.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(context.Context) error
}

// Job names. They are stable strings: a cron run is recorded under one of them, so renaming
// one splits the operator's history in two.
const (
	// OrderCancelJob cancels orders that were never paid.
	OrderCancelJob = "order-cancel"
	// TerminationJob terminates services that have been suspended too long.
	TerminationJob = "terminate-suspended"
)

// Intervals.
//
// Both rules are measured in days, so a tick every half hour is already far more often than
// the rule can change its answer. It is frequent enough that an operator who lowers the
// threshold sees the effect in the same session, and cheap: the scan is an index scan that
// returns nothing on a healthy installation.
const (
	OrderCancelInterval = 30 * time.Minute
	TerminationInterval = 30 * time.Minute
)

// Kill switches.
//
// They follow the convention the rest of the worker uses (`BILLING_DISABLE_*`): an operator's
// emergency brake, settable without a deploy, for when a rule is doing something wrong. A
// switched-off job does nothing at all — it does not fall back to a default threshold.
const (
	OrderCancelKillSwitch = "BILLING_DISABLE_ORDER_CANCEL"
	TerminationKillSwitch = "BILLING_DISABLE_TERMINATION"
)

// Settings the jobs read. They are the two keys the 0020 migration seeded into the
// `cronjob` group, and they are read through the typed accessor so a missing row is the
// code-side default rather than a zero.
const (
	settingsGroup          = "cronjob"
	orderCancelDaysSetting = "order_cancel_days"
	terminateDaysSetting   = "terminate_days"
)

// DefaultBatch bounds one run.
//
// A cron that processes "everything" is a cron that one day holds thousands of row locks in
// one transaction. A batch drains a backlog over a few ticks and keeps each transaction
// short enough that a customer's payment is never blocked behind it.
const DefaultBatch = 50

// Reasons a run did nothing. They are stable strings so a log line or a cron_runs row can be
// grouped by them.
const (
	// ReasonKillSwitch means the operator disabled the job.
	ReasonKillSwitch = "kill_switch"
	// ReasonThresholdDisabled means the threshold is zero or negative: the rule is off.
	// This is deliberately not "act immediately".
	ReasonThresholdDisabled = "threshold_disabled"
	// ReasonThresholdUnreadable means the setting could not be read or parsed. The job does
	// nothing rather than guessing a number.
	ReasonThresholdUnreadable = "threshold_unreadable"
)

// Runner executes the rules. It holds a pool, the settings accessor and the notification
// outbox, and nothing else.
type Runner struct {
	pool     *store.Pool
	settings *settings.Store
	queue    *notify.Queue
	// holds is the dunning engine, used only for its definition of an active manual hold.
	// The termination rule asks it rather than re-deriving "is this service held" from a
	// query of its own.
	holds *dunning.Engine

	// Now is the clock. Tests set it; production uses UTC so a comparison against a
	// timestamptz column is unambiguous.
	Now func() time.Time

	// Batch overrides DefaultBatch. Zero means the default.
	Batch int
}

// New constructs a Runner.
func New(pool *store.Pool) *Runner {
	return &Runner{
		pool:     pool,
		settings: settings.New(pool),
		queue:    notify.NewQueue(pool),
		holds:    dunning.NewEngine(pool, dunning.DefaultPolicy()),
		Now:      func() time.Time { return time.Now().UTC() },
	}
}

// WithClock returns a copy of the runner that reads a different clock.
//
// A copy, not a mutation: the runner is shared by both jobs, and a test that moved the clock
// on the shared value would move it for the other job too.
func (r *Runner) WithClock(now func() time.Time) *Runner {
	clone := *r
	clone.Now = now
	return &clone
}

// now reads the clock with the UTC normalisation every comparison relies on.
func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

// batch is the run's bound.
func (r *Runner) batch() int {
	if r.Batch <= 0 {
		return DefaultBatch
	}
	return r.Batch
}

// threshold reads one day count.
//
// A parse failure is reported as ReasonThresholdUnreadable rather than as an error that
// aborts the run: the job's answer is "do nothing", which is what the caller needs, and the
// caller still gets the reason to log. An unreadable threshold must never be silently
// replaced by a default, because the default is a *number of days* and acting on the wrong
// one is acting on customers.
func (r *Runner) threshold(ctx context.Context, key string) (int, string, error) {
	days, err := r.settings.Int(ctx, settingsGroup, key)
	if err != nil {
		// The typed accessor only fails when the key is unknown or the stored text is not an
		// integer. Both are configuration bugs; neither is a reason to cancel anything.
		return 0, ReasonThresholdUnreadable, nil
	}
	if days <= 0 {
		return 0, ReasonThresholdDisabled, nil
	}
	return int(days), "", nil
}

// cutoff is the instant before which something is "older than days".
//
// Calendar arithmetic, not 24-hour arithmetic: "14 days" means the same wall-clock time
// fourteen days ago, which is what an operator means and what the ticket auto-close rule
// already does. A DST shift therefore does not move the boundary by an hour.
func cutoff(now time.Time, days int) time.Time {
	return now.UTC().AddDate(0, 0, -days)
}

// disabled reports whether a kill switch is set.
//
// It accepts the same spellings the worker's killSwitch does, because an operator who writes
// "yes" in one place and "1" in another must not get two different answers.
func disabled(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Jobs returns the periodic jobs this package provides.
//
// A job whose kill switch is set is not returned at all: the worker's own convention is that
// a disabled subsystem is absent from the list, so nothing schedules it, logs a tick, or
// shows up as a failing job. The rule itself re-checks the switch (see CancelUnpaidOrders),
// which is what makes the switch effective for a caller that runs the rule directly.
func Jobs(pool *store.Pool, logger *slog.Logger) []Job {
	if pool == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	runner := New(pool)

	var jobs []Job
	if disabled(OrderCancelKillSwitch) {
		logger.Warn("order cancellation is disabled by kill switch")
	} else {
		jobs = append(jobs, Job{
			Name:     OrderCancelJob,
			Interval: OrderCancelInterval,
			Run: func(ctx context.Context) error {
				run, err := runner.CancelUnpaidOrders(ctx)
				logOrderCancel(logger, run, err)
				return err
			},
		})
	}

	if disabled(TerminationKillSwitch) {
		logger.Warn("termination of suspended services is disabled by kill switch")
	} else {
		jobs = append(jobs, Job{
			Name:     TerminationJob,
			Interval: TerminationInterval,
			Run: func(ctx context.Context) error {
				run, err := runner.TerminateSuspended(ctx)
				logTermination(logger, run, err)
				return err
			},
		})
	}
	return jobs
}

// logOrderCancel writes what a run did, and nothing when it did nothing.
//
// A job that logs "0 cancelled" every half hour is a log nobody reads, which is how a job
// that cancelled four hundred orders goes unnoticed.
func logOrderCancel(logger *slog.Logger, run OrderCancelRun, err error) {
	if err != nil {
		logger.Error("order cancellation failed", "error", err)
	}
	if run.Reason != "" {
		logger.Info("order cancellation did nothing", "reason", run.Reason)
	}
	for _, o := range run.Cancelled {
		logger.Info("order cancelled", "order_id", o.OrderID,
			"customer_id", o.CustomerID, "invoices_voided", len(o.InvoiceIDs),
			"stock_restored", len(o.Stock), "days", run.Days)
	}
	for _, failure := range run.Failures {
		logger.Error("order cancellation failed for one order", "detail", failure)
	}
}

// logTermination is the same discipline for the other job.
func logTermination(logger *slog.Logger, run TerminationRun, err error) {
	if err != nil {
		logger.Error("termination run failed", "error", err)
	}
	if run.Reason != "" {
		logger.Info("termination did nothing", "reason", run.Reason)
	}
	for _, s := range run.Terminated {
		logger.Warn("service terminated through the queue", "service_id", s.ServiceID,
			"customer_id", s.CustomerID, "operation_id", s.OperationID,
			"invoices_voided", len(s.InvoiceIDs), "days", run.Days)
	}
	for _, failure := range run.Failures {
		logger.Error("termination failed for one service", "detail", failure)
	}
}
