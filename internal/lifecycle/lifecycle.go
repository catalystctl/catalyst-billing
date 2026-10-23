// Package lifecycle owns the three rules that run on a timer and change something on their own.
//
// Two settings had been sitting in the table since the settings screen was written and nothing
// had ever read them: `cronjob.order_cancel_days` and `cronjob.terminate_days`. They are
// promises to an operator — "an abandoned order is cleaned up", "a service suspended for too
// long is taken away" — and this package is what makes them true.
//
// The third rule is the pending-price-change queue (`service-upgrades`). It is here for the same
// reason the other two are: `service.Store` implements it and nothing ever called it, so a paid
// upgrade never moved, an unpaid one stayed pending for ever, and a downgrade was owed a credit
// nobody posted. All three are one shape — read persisted state, derive the work, act through the
// package that owns the invariant — and a rule that runs on a timer belongs where the timers are.
//
// # The shape of a job here
//
// Every job is the same three steps, and the order matters:
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
// It also registers two maintenance jobs from internal/operations — charging billing agreements and
// delivering the telemetry payload — because `Jobs` below is the seam the worker already iterates
// and cmd/billing cannot be edited to add them. Those rules are not lifecycle rules; see
// operations_jobs.go for why they are converted here rather than written into this package.
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

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/service"
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

	// The price-change queue: it finishes a change the customer already asked for, and it is
	// guarded by its own switch because it is the job that voids documents.
	jobs = append(jobs, ServiceJobs(pool, logger)...)

	// The maintenance rules from internal/operations: charging the cards customers left on file
	// and delivering the opted-in telemetry payload. They are registered through this function
	// because it is the seam the worker iterates (see operations_jobs.go) — the rules themselves
	// are not lifecycle rules and do not live here.
	jobs = append(jobs, OperationsJobs(pool, logger)...)
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

const (
	// ServiceUpgradesJob applies paid upgrades, credits and applies downgrades, and voids stale
	// upgrade invoices.
	ServiceUpgradesJob = "service-upgrades"
	// ServiceUpgradesInterval is deliberately much shorter than the stale-upgrade window: a
	// customer who pays an upgrade invoice expects the plan to change while they are still looking
	// at the screen.
	ServiceUpgradesInterval = 2 * time.Minute
	// ServiceUpgradesKillSwitch stops the whole job without a deploy.
	ServiceUpgradesKillSwitch = "BILLING_DISABLE_SERVICE_UPGRADES"
)

// ServiceJobs returns the service-price-change jobs, for the worker to schedule.
//
// It is called from Jobs(), which is the list cmd/billing/jobs.go and the automation screen both
// read, so a job added here is scheduled and visible in one step rather than two.
func ServiceJobs(pool *store.Pool, logger *slog.Logger) []Job {
	if pool == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if disabled(ServiceUpgradesKillSwitch) {
		logger.Warn("service upgrade automation is disabled by kill switch")
		return nil
	}

	svc := service.New(pool)
	credits := credit.New(pool)

	return []Job{{
		Name:     ServiceUpgradesJob,
		Interval: ServiceUpgradesInterval,
		Run: func(ctx context.Context) error {
			runServiceUpgrades(ctx, pool, svc, credits, logger)
			return nil
		},
	}}
}

// runServiceUpgrades performs one tick.
//
// It returns no error on purpose. Each step is bounded and each failure is per-row, so a single
// unprocessable row must not be reported as a failed job: the worker would record a red tick every
// two minutes while the rest of the queue drained, and the operator would stop reading them.
func runServiceUpgrades(ctx context.Context, pool *store.Pool, svc *service.Store,
	credits *credit.Service, logger *slog.Logger) {
	// 1. The credit a downgrade owes.
	posted, failed, err := credits.PostPendingDowngradeCredits(ctx, 100)
	if err != nil {
		logger.Error("downgrade credit run failed", "error", err)
	}
	for _, id := range posted {
		logger.Info("downgrade credited", "upgrade_id", id)
	}
	for _, id := range failed {
		logger.Error("a downgrade could not be credited", "upgrade_id", id)
	}

	// 2. Apply what the money allows. Downgrades are applied from their own list because
	// `ApplyPendingUpgrades` handles only the paid-invoice case by design.
	credited, err := credit.PendingDowngrades(ctx, pool, true, 100)
	if err != nil {
		logger.Error("downgrade apply scan failed", "error", err)
	}
	for _, id := range credited {
		if err := svc.ApplyDowngrade(ctx, id); err != nil {
			// A row another worker already applied is the normal race, not an incident worth an
			// error line.
			logger.Debug("downgrade not applied", "upgrade_id", id, "error", err)
			continue
		}
		logger.Info("downgrade applied", "upgrade_id", id)
	}

	applied, err := svc.ApplyPendingUpgrades(ctx)
	if err != nil {
		logger.Error("paid upgrade run failed", "error", err)
	}
	for _, id := range applied {
		logger.Info("upgrade applied", "upgrade_id", id)
	}

	// 3. The upgrades nobody paid for.
	cancelled, err := svc.CancelStaleUpgrades(ctx)
	if err != nil {
		logger.Error("stale upgrade run failed", "error", err)
	}
	for _, id := range cancelled {
		logger.Info("stale upgrade cancelled", "upgrade_id", id)
	}
}
