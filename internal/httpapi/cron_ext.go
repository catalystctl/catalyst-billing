// The automation screen: what the worker runs, how often, and how to stop it.
//
// The panel could already show cron *history* (cron.go: `cron_runs` and the heartbeat) and it
// could show the two thresholds those runs obey (the `cronjob` settings group). What it could
// not answer is the question an operator asks at 03:00 when something is cancelling orders:
// **what runs on this box, and which switch turns it off**. The job list lived in
// cmd/billing/jobs.go, the switches in the environment, and the only way to read either was
// the source.
//
// # Where this list comes from, and why it is not one list
//
// There are two registration sites, and they are not equally readable:
//
//   - internal/lifecycle registers its jobs through lifecycle.Jobs(). That function is
//     called here, with the live pool, so the names and intervals on this screen are the ones
//     the worker actually schedules — not a copy. A job that appears there and not here would
//     be invisible to the operator, so the test in admin_screens_test.go asserts the two sets
//     match, and a job removed from the catalogue while still registered fails the build.
//   - cmd/billing/jobs.go is package main: it cannot be imported, so its jobs are described
//     here from reading it. The same test parses that file for its `name:`/`interval:` pairs
//     and compares them with this table, which is what stops the copy from drifting.
//
// # Why the kill switches are read-only
//
// They are environment variables, read when the worker starts. There is no endpoint that can
// change one: writing to os.Setenv would change this process and not the worker's, so a
// control here would be a switch that does nothing, which is worse than no switch. So the
// screen reports the switch's name and its state and says plainly that it is an environment
// variable — the operator's next step is a deploy, and the screen says so.
package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/operations"
	"github.com/catalyst/billing/internal/tax"
)

func init() {
	RegisterRoutes("44-operator-automation", registerOperatorAutomation)
}

func registerOperatorAutomation(r *router) {
	// settings:write rather than log:read: reading the list is harmless, but this is the screen
	// an operator uses to decide what to switch off, and the settings permission is the one that
	// already means "may change how this installation behaves".
	r.staffGet("/api/v1/staff/automation", authn.PermSettingsWrite, r.s.handleAutomationJobs)
}

// automationJob is one periodic job, as the screen describes it.
//
// The zero KillSwitch means "there is no switch for this job", which is a fact worth showing:
// an operator looking for the switch that stops renewals needs to learn that there is not one,
// rather than assume they have not found it.
type automationJob struct {
	Name string
	// Interval is the period between ticks.
	Interval time.Duration
	// Summary is one sentence: what the job does.
	Summary string
	// KillSwitch is the environment variable that stops the job. Empty when none exists.
	KillSwitch string
	// SwitchEffect says what the switch actually stops when it stops less than the whole job.
	// Only the dunning job is partial today: its switch stops suspension and resume, and the
	// escalation ladder keeps running, which is the opposite of what "disabled" suggests.
	SwitchEffect string
	// Settings are the setting keys the job reads, so an operator can connect a threshold to
	// the job that obeys it.
	Settings []string
	// Source is the file that registers the job.
	Source string
}

// workerJobs is the worker's job list, in registration order.
//
// Registration order is kept because it is the order the worker ticks in, and because the two
// jobs that must run last are last for a reason: `notifications` drains the outbox every other
// job writes to, and the lifecycle rules enqueue operations that the queue worker then has to
// see. A screen that sorted alphabetically would lose that.
var workerJobs = []automationJob{
	{
		Name:       "payments",
		Interval:   5 * time.Second,
		Summary:    "Applies settled gateway payments to invoices, checking each amount against the recorded checkout.",
		KillSwitch: "BILLING_DISABLE_PAYMENTS",
		Source:     "cmd/billing/jobs.go",
	},
	{
		Name:       "provisioning",
		Interval:   5 * time.Second,
		Summary:    "Runs queued service operations, reaps expired leases and reconciles outcomes the provider left unknown.",
		KillSwitch: "BILLING_DISABLE_PROVISIONING",
		Source:     "cmd/billing/jobs.go",
	},
	{
		Name:     "retention",
		Interval: 6 * time.Hour,
		Summary:  "Deletes bookkeeping rows past their retention policy.",
		Source:   "cmd/billing/jobs.go",
	},
	{
		Name:     "ticket-autoclose",
		Interval: 6 * time.Hour,
		Summary:  "Closes tickets nobody replied to once they are older than the auto-close threshold.",
		Settings: []string{"tickets.autoclose_days"},
		Source:   "cmd/billing/jobs.go",
	},
	{
		Name:         "dunning",
		Interval:     60 * time.Second,
		Summary:      "Escalates overdue invoices, then suspends services whose invoice is exhausted and resumes the ones that were paid.",
		KillSwitch:   "BILLING_DISABLE_SUSPENSION",
		SwitchEffect: "stops only the suspend and resume half; the escalation ladder keeps running",
		Source:       "cmd/billing/jobs.go",
	},
	{
		Name:     "renewals",
		Interval: 60 * time.Second,
		Summary:  "Issues an invoice for every subscription whose billing period has ended.",
		Source:   "cmd/billing/jobs.go",
	},
	{
		Name:       "refund-reconcile",
		Interval:   2 * time.Minute,
		Summary:    "Asks the provider again about refunds whose outcome is unknown, using the same operation key.",
		KillSwitch: "BILLING_DISABLE_REFUND_RECONCILE",
		Source:     "cmd/billing/jobs.go",
	},
	{
		Name:       "notifications",
		Interval:   15 * time.Second,
		Summary:    "Drains the durable mail outbox. Every other job enqueues into it, so this one runs last.",
		KillSwitch: "BILLING_DISABLE_NOTIFICATIONS",
		Source:     "cmd/billing/jobs.go",
	},
	{
		Name:     "ticket-piping",
		Interval: time.Minute,
		Summary:  "Polls the support mailbox and turns new mail into ticket replies.",
		Source:   "cmd/billing/piping.go",
	},
	// The two lifecycle rules. Their names, intervals and switches are taken from
	// internal/lifecycle's exported declarations rather than typed again here, so a rename in
	// that package is a compile error rather than a screen that lies.
	{
		Name:       lifecycle.OrderCancelJob,
		Interval:   lifecycle.OrderCancelInterval,
		Summary:    "Cancels orders that were never paid, voiding their invoices through the same open-to-void transition staff use.",
		KillSwitch: lifecycle.OrderCancelKillSwitch,
		Settings:   []string{"cronjob.order_cancel_days"},
		Source:     "internal/lifecycle/lifecycle.go",
	},
	{
		Name:       lifecycle.TerminationJob,
		Interval:   lifecycle.TerminationInterval,
		Summary:    "Terminates services that have been suspended longer than the threshold, through the operation queue so the work stays fenced and auditable.",
		KillSwitch: lifecycle.TerminationKillSwitch,
		Settings:   []string{"cronjob.terminate_days"},
		Source:     "internal/lifecycle/lifecycle.go",
	},
	// The price-change queue (UPG-05, UPG-06, CRD-06). It is the only job an operator can use to
	// answer "why did my upgrade not apply" without reading the database: a paid upgrade that never
	// moved, an invoice nobody paid that kept a row pending, or a downgrade whose credit was never
	// posted are all the same queue with three different next steps.
	{
		Name:       lifecycle.ServiceUpgradesJob,
		Interval:   lifecycle.ServiceUpgradesInterval,
		Summary:    "Finishes pending price changes: credits and applies downgrades, applies paid upgrades, and voids upgrade invoices nobody paid.",
		KillSwitch: lifecycle.ServiceUpgradesKillSwitch,
		Source:     "internal/lifecycle/lifecycle.go",
	},
	// The two maintenance rules from internal/operations, registered through lifecycle.Jobs()
	// because that is the seam the worker iterates. Their names and intervals come from the
	// package that defines them, so a rename is a compile error rather than a screen that lies.
	{
		Name:       operations.ChargeDueJob,
		Interval:   operations.ChargeDueInterval,
		Summary:    "Charges the saved default payment method of every open, past-due invoice, waiting cronjob.charge_retry_hours before offering a declined card again.",
		KillSwitch: operations.ChargeDueKillSwitch,
		Settings:   []string{"gateway.off_session_enabled", "cronjob.charge_retry_hours"},
		Source:     "internal/lifecycle/operations_jobs.go",
	},
	{
		Name:       operations.TelemetryJob,
		Interval:   operations.TelemetryInterval,
		Summary:    "Delivers the anonymous telemetry payload once a day when updates.telemetry is on, and records the send in telemetry_sends.",
		KillSwitch: operations.TelemetryKillSwitch,
		Settings:   []string{"updates.telemetry", "updates.channel"},
		Source:     "internal/lifecycle/operations_jobs.go",
	},
	// The tax-rate refresh. Its interval is the worker's *tick*, not the refresh cadence: the
	// source's own `refresh_interval_hours` (configured on the tax-rates screen) decides whether a
	// tick fetches anything, so an installation that wants a daily refresh is not polled hourly.
	// There is no kill switch: switching the source off on that screen is the brake, and it stops
	// the fetch itself rather than only the scheduler's share of it.
	{
		Name:     tax.JobName,
		Interval: tax.JobInterval,
		Summary:  "Fetches the configured external tax-rate source and applies what it says, recording provenance and never replacing a rate an operator typed.",
		Source:   "cmd/billing/jobs.go",
	},
}

// handleAutomationJobs serves the worker's job list and each job's kill switch state.
func (s *Server) handleAutomationJobs(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	// The live registration, not a copy of it. lifecycle.Jobs() omits a job whose switch is set,
	// which is exactly how the disabled state is detected: a job missing from the worker's own
	// list is a job the worker does not schedule.
	registered := make(map[string]lifecycle.Job, len(workerJobs))
	for _, job := range lifecycle.Jobs(s.pool, s.cfg.Logger) {
		registered[job.Name] = job
	}

	out := make([]map[string]any, 0, len(workerJobs))
	for _, job := range workerJobs {
		interval := job.Interval
		var disabled bool
		if strings.HasPrefix(job.Source, "internal/lifecycle/") {
			live, ok := registered[job.Name]
			disabled = !ok
			if ok && live.Interval > 0 {
				interval = live.Interval
			}
		} else if job.KillSwitch != "" {
			// Mirrors the worker's own predicate (killSwitch in cmd/billing/jobs.go): the same
			// three spellings, trimmed. Reporting a switch as off when the worker honours it
			// would be the dangerous direction, so the spellings are the worker's, not a
			// more generous guess.
			disabled = envSwitchSet(job.KillSwitch)
		}

		row := map[string]any{
			"name":             job.Name,
			"interval_seconds": int64(interval / time.Second),
			"interval":         humanInterval(interval),
			"summary":          job.Summary,
			"disabled":         disabled,
			"source":           job.Source,
		}
		if job.KillSwitch != "" {
			row["kill_switch"] = job.KillSwitch
			row["kill_switch_set"] = disabled
		} else {
			// Null rather than absent: the screen has to render "there is no switch for this
			// job", and an absent field is indistinguishable from a client that forgot to send
			// one.
			row["kill_switch"] = nil
			row["kill_switch_set"] = false
		}
		if job.SwitchEffect != "" {
			row["switch_effect"] = job.SwitchEffect
		}
		if len(job.Settings) > 0 {
			row["settings"] = job.Settings
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": out,
		// Said once, in the response, so the screen cannot render the state without the
		// explanation: these are environment variables and this screen cannot change them.
		"kill_switches": map[string]any{
			"mechanism": "environment",
			"read_only": true,
			"note": "A kill switch is an environment variable read when the worker starts. " +
				"Changing one is a restart, not a click, so this screen reports their state and " +
				"does not offer a control that would change nothing.",
		},
		// The embedded-worker switch is not a per-job switch and is not read the same way: when
		// it is set the worker runs inside the web process rather than beside it. Reported
		// separately so it cannot be mistaken for a job's brake.
		"worker": map[string]any{
			"embedded_switch": "BILLING_EMBEDDED_WORKER",
			"embedded":        envSwitchSet("BILLING_EMBEDDED_WORKER"),
		},
	})
}

// envSwitchSet reports whether a kill switch is on, using the worker's own spelling rules.
func envSwitchSet(key string) bool {
	switch strings.TrimSpace(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// humanInterval renders a period the way an operator says it: "every 5 seconds", "every 6
// hours".
//
// A period that is not a whole number of its unit falls back to Go's duration string rather
// than rounding, because rounding is how "every 90 seconds" becomes "every 2 minutes" on a
// screen an operator is using to reason about load.
func humanInterval(d time.Duration) string {
	switch {
	case d <= 0:
		return "not scheduled"
	case d%time.Hour == 0:
		return "every " + plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return "every " + plural(int(d/time.Minute), "minute")
	case d%time.Second == 0:
		return "every " + plural(int(d/time.Second), "second")
	default:
		return "every " + d.String()
	}
}

// plural renders "1 hour" / "2 hours".
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
