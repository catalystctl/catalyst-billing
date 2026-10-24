// Package operations owns the maintenance rules the worker runs that are neither a pricing
// decision (internal/renewal) nor one of the three destructive lifecycle rules
// (internal/lifecycle): charging the cards customers left on file, and delivering the daily
// telemetry payload an operator opted into.
//
// # Why these two are here and not in cmd/billing
//
// Both are "one scan, derived from persisted state, safe to run twice" jobs, and both used to be
// reachable only by a human pressing a button:
//
//   - `payments.Agreements.ChargeDue` is the *documented* entry point for a cron ("it is the
//     entry point a cron calls after the renewal scan") and no cron called it. An installation
//     with `gateway.off_session_enabled` on and a saved default card therefore issued renewal
//     invoices and escalated them through dunning without ever attempting the charge the
//     customer had authorised.
//   - `updates.Store.Send` delivered a telemetry payload only when an operator pressed "send now"
//     on the updates screen, so `updates.telemetry` promised a daily report that never happened.
//     A promise in a settings label that nothing keeps is worse than no setting.
//
// The worker's own package (cmd/billing) is deliberately not where the rules live: it is the
// loop, and a rule that can be tested without a worker is a rule that gets tested. `Jobs` returns
// the list in the same shape internal/lifecycle uses, and the two are registered through the same
// seam (see lifecycle.OperationsJobs).
//
// # What every job in here must do
//
//   - **Skip, do not fail, when the installation has not configured it.** No gateway and no
//     telemetry endpoint are the normal state of a fresh install. Returning an error there would
//     paint `cron_runs` red every fifteen minutes for a customer who has switched the feature
//     off, and an operator learns to ignore a red row.
//   - **Report what it did, and nothing when it did nothing.** A job that logs "0 charged" every
//     tick is a log nobody reads, which is how a job that charged twelve cards goes unnoticed.
//   - **Bound the batch.** A rule that processes everything holds row locks for as long as the
//     backlog is long, and the customer waiting on a checkout is behind it.
//   - **Never invent an amount or a permission.** The charge path reads the amount from the
//     invoice row, the method from the customer's default, and the mandate from the agreement —
//     all inside internal/payments. This package only decides *which* invoices to offer it.
package operations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
)

// Job is one periodic task.
//
// It mirrors lifecycle.Job rather than importing it: the two packages are independent lists of
// rules, and a shared type in a third package would be a third thing to maintain for no gain. The
// adapter in internal/lifecycle converts one into the other in four lines.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(context.Context) error
}

// Job names. They are the `job` column of `cron_runs`, so they are stable identifiers: renaming
// one splits the operator's run history in two and orphans the failures recorded under the old
// name.
const (
	// ChargeDueJob charges open, past-due invoices that have a default saved method.
	ChargeDueJob = "charge-agreements"
	// TelemetryJob delivers the telemetry payload when an operator has opted in.
	TelemetryJob = "telemetry-send"
)

// Intervals.
//
// The charge sweep is deliberately much shorter than the retry window it obeys: a card that was
// declined is not re-attempted for `cronjob.charge_retry_hours` (see ChargeDue), so a short tick
// makes a *newly* due invoice charge promptly without hammering a declining card. The telemetry
// job's interval is the promise the setting makes ("daily"), and it keeps its own record in
// `telemetry_sends` so a missed day is visible there rather than inferred from the tick.
const (
	ChargeDueInterval = 15 * time.Minute
	TelemetryInterval = 24 * time.Hour
)

// Kill switches, following the worker's own `BILLING_DISABLE_*` convention. They are read through
// disabled so an operator who writes "yes" in one place and "1" in another gets one answer.
const (
	ChargeDueKillSwitch = "BILLING_DISABLE_CHARGE_DUE"
	TelemetryKillSwitch = "BILLING_DISABLE_TELEMETRY"
)

// DefaultChargeRetry is the fallback for `cronjob.charge_retry_hours`.
//
// One day, because the alternative is a declined card being charged every tick: the provider's
// decline is a statement about the card, and repeating the request fifteen minutes later is how a
// customer's bank starts refusing *all* charges from the merchant.
const DefaultChargeRetry = 24 * time.Hour

// Batch bounds one run of either job.
const Batch = 50

// Reasons a run did nothing. They are stable strings so a `cron_runs` row can be grouped by them.
const (
	// ReasonKillSwitch means the operator disabled the job.
	ReasonKillSwitch = "kill_switch"
	// ReasonNotConfigured means the installation has no gateway or no telemetry endpoint.
	ReasonNotConfigured = "not_configured"
	// ReasonDisabled means the feature's own setting is off (off-session charging, telemetry).
	ReasonDisabled = "disabled"
	// ReasonNothingDue means the scan found no work, which is the healthy state.
	ReasonNothingDue = "nothing_due"
	// ReasonLocked means another worker was mid-sweep and this tick stood down.
	//
	// It is a skip, never a failure. Two workers scanning the same invoices is not a correctness
	// problem — `payment_attempts.operation_key` is UNIQUE and inserted before the provider call,
	// so the loser is refused by the database rather than double-charging — but it burns provider
	// rate limit and races the decline-to-dunning transition from two goroutines. The lock removes
	// the noise; the constraint remains the guarantee.
	ReasonLocked = "locked"
	// ReasonSent means the telemetry payload was delivered.
	ReasonSent = "sent"
	// ReasonRecentlySent means a delivery was already recorded inside TelemetryMinimumGap, so this
	// tick did nothing. It is what keeps a restart from becoming a second daily report.
	ReasonRecentlySent = "recently_sent"
)

// Options are the dependencies a job needs that are not the database.
type Options struct {
	// Logger receives what a run did. Nil means slog.Default(), so a caller that forgot one
	// still sees the run rather than swallowing it.
	Logger *slog.Logger
	// Version is the running panel version, carried in the telemetry payload.
	Version string
	// Account is the provider account the settlement is recorded against, as the webhook path
	// records it. Empty means the off-session path is not configured (see payments.ErrNotConfigured).
	Account string
	// ChargerOverride replaces the launch of the configured gateway extension. It exists for
	// tests and for a deployment whose adapter is attached rather than launched, which is the
	// same seam internal/httpapi exposes as WithOffSessionCharging.
	ChargerOverride ChargerFactory
	// Client is the HTTP client the telemetry delivery uses. Nil means one bounded by
	// updates.TelemetryTimeout, which is what the manual send uses.
	Client *http.Client
	// Now is the clock. Zero means time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}

// Jobs returns the maintenance jobs this package provides.
//
// A job whose kill switch is set is not returned at all, which is the worker's convention: a
// disabled subsystem is absent from the list rather than present and silent. The rule re-checks
// the switch for a caller that runs it directly (a retry from the panel, a test).
func Jobs(pool *store.Pool, opts Options) []Job {
	if pool == nil {
		return nil
	}
	opts = opts.withDefaults()

	var jobs []Job
	if disabled(ChargeDueKillSwitch) {
		opts.Logger.Warn("charging billing agreements is disabled by kill switch")
	} else {
		jobs = append(jobs, Job{
			Name:     ChargeDueJob,
			Interval: ChargeDueInterval,
			Run: func(ctx context.Context) error {
				run, err := ChargeDue(ctx, pool, opts)
				logChargeDue(opts.Logger, run, err)
				if err != nil {
					return err
				}
				if run.Reason == ReasonLocked {
					// A skip, not a success: the sweep did not run, and the run history should say
					// so without calling it a failure.
					return Skip("another worker held the sweep")
				}
				return nil
			},
		})
	}

	if disabled(TelemetryKillSwitch) {
		opts.Logger.Warn("telemetry delivery is disabled by kill switch")
	} else {
		jobs = append(jobs, Job{
			Name:     TelemetryJob,
			Interval: TelemetryInterval,
			Run: func(ctx context.Context) error {
				run, err := SendTelemetry(ctx, pool, opts)
				logTelemetry(opts.Logger, run, err)
				return err
			},
		})
	}
	return jobs
}

// disabled reports whether a kill switch is set, using the worker's own spellings.
func disabled(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// logChargeDue writes what a run did, and nothing when it did nothing.
func logChargeDue(logger *slog.Logger, run ChargeDueRun, err error) {
	if err != nil {
		logger.Error("charge due sweep failed", "error", err)
	}
	if run.Reason != "" {
		logger.Info("charge due sweep did nothing", "reason", run.Reason)
	}
	for _, out := range run.Outcomes {
		if out.Status == "skipped" {
			logger.Debug("charge skipped", "invoice_id", out.InvoiceID, "detail", out.Detail)
			continue
		}
		logger.Info("invoice charged from the saved method", "invoice_id", out.InvoiceID,
			"payment_id", out.PaymentID, "status", string(out.Status), "detail", out.Detail)
	}
	if len(run.Outcomes) > 0 {
		logger.Info("charge due sweep complete", "attempted", len(run.Outcomes),
			"settled", run.Settled, "declined", run.Declined,
			"already_attempted", run.AlreadyAttempted)
	}
}

// logTelemetry writes the outcome of a delivery attempt.
func logTelemetry(logger *slog.Logger, run TelemetryRun, err error) {
	if err != nil {
		logger.Error("telemetry delivery failed", "error", err)
	}
	if run.Reason != "" {
		logger.Debug("telemetry not sent", "reason", run.Reason)
	}
	if run.Result != nil {
		logger.Info("telemetry sent", "send_id", run.Result.ID, "status", run.Result.Status,
			"endpoint_host", run.Result.EndpointHost)
	}
}

// joinErr returns nil for an empty slice of errors, so a caller can return it directly.
func joinErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
