package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/operations"
	"github.com/jackc/pgx/v5"
)

// The failed-job viewer, the cron statistics and the on-demand retry (OPS-09, OPS-13).
//
// The run history existed and answered "what ran recently". These three answer the questions an
// operator actually has at 03:00: which job is failing, how often, and can this one be run again
// without restarting the worker. The tests assert the properties, not the happy path: the failure
// list is only failures, the aggregate reports a stuck run as running rather than healthy, a
// retry leaves a `cron_runs` row and an audit row, and a job the panel cannot construct is
// refused by name instead of 500ing.

// clearJobSwitches removes every kill switch a job list would consult.
//
// The environment is process-wide, so a switch set by another test (or by the developer's shell)
// would silently empty lifecycle.Jobs() and make these tests pass for the wrong reason.
func clearJobSwitches(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		lifecycle.OrderCancelKillSwitch, lifecycle.TerminationKillSwitch,
		lifecycle.ServiceUpgradesKillSwitch, operations.ChargeDueKillSwitch,
		operations.TelemetryKillSwitch,
	} {
		t.Setenv(key, "")
	}
}

type cronFailureRow struct {
	ID         string `json:"id"`
	Job        string `json:"job"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail"`
	DurationMS *int64 `json:"duration_ms"`
}

// The failure list carries failed runs and nothing else.
func TestCronFailuresListsOnlyFailures(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO cron_runs (job, started_at, finished_at, outcome, detail, items) VALUES
			('renewals', now() - interval '10 minutes', now() - interval '9 minutes', 'ok', 'invoiced 3', 3),
			('renewals', now() - interval '5 minutes', now() - interval '4 minutes', 'failed', 'gateway timeout', 0),
			('dunning',  now() - interval '3 minutes', now() - interval '2 minutes', 'failed', 'boom', 0),
			('dunning',  now() - interval '1 minute',  NULL, 'running', '', 0)`); err != nil {
		t.Fatalf("insert runs: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/cron/failures", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []cronFailureRow `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if len(page.Data) != 2 {
		t.Fatalf("failures = %d, want the two failed runs only: %+v", len(page.Data), page.Data)
	}
	for _, row := range page.Data {
		if row.Outcome != "failed" {
			t.Fatalf("the failure list returned a %q run", row.Outcome)
		}
	}
	if page.Data[0].Job != "dunning" {
		t.Fatalf("first row = %+v, want the newest failure", page.Data[0])
	}

	// Filtering by job, and a job with no failures answers an empty page rather than an error.
	rec = env.get(t, "/api/v1/staff/cron/failures?job=renewals", env.support)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Job != "renewals" {
		t.Fatalf("filtered failures = %+v, want the one renewals failure", page.Data)
	}

	// A clerk without log:read cannot read it.
	if rec := env.get(t, "/api/v1/staff/cron/failures", env.billing); rec.Code != http.StatusForbidden {
		t.Fatalf("billing: status %d, want 403", rec.Code)
	}
}

// The statistics aggregate per job: counts, the last outcome, and the detail that explains it —
// with the credential redaction applied again on the way out.
func TestCronStatisticsAggregatePerJob(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO cron_runs (job, started_at, finished_at, outcome, detail, items) VALUES
			('renewals', now() - interval '30 minutes', now() - interval '29 minutes', 'ok', 'invoiced 3', 3),
			('renewals', now() - interval '20 minutes', now() - interval '19 minutes', 'ok', 'invoiced 1', 1),
			('renewals', now() - interval '10 minutes', now() - interval '9 minutes', 'failed', 'password=hunter2 refused', 0),
			('dunning',  now() - interval '8 minutes',  NULL, 'running', '', 0),
			('tickets',  now() - interval '7 minutes',  now() - interval '7 minutes', 'skipped', 'threshold disabled', 0)`); err != nil {
		t.Fatalf("insert runs: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/cron/statistics", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		WindowDays int `json:"window_days"`
		Data       []struct {
			Job           string `json:"job"`
			Runs          int    `json:"runs"`
			Failures      int    `json:"failures"`
			Running       int    `json:"running"`
			Skipped       int    `json:"skipped"`
			AvgDurationMS int64  `json:"avg_duration_ms"`
			LastOutcome   string `json:"last_outcome"`
			LastDetail    string `json:"last_detail"`
			LastSuccessAt any    `json:"last_success_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if body.WindowDays != 7 {
		t.Fatalf("window = %d days, want the default 7", body.WindowDays)
	}
	if len(body.Data) != 3 {
		t.Fatalf("jobs = %d, want 3", len(body.Data))
	}

	byJob := map[string]int{}
	for i, row := range body.Data {
		byJob[row.Job] = i
	}
	renewals := body.Data[byJob["renewals"]]
	if renewals.Runs != 3 || renewals.Failures != 1 {
		t.Fatalf("renewals = %+v, want 3 runs and 1 failure", renewals)
	}
	if renewals.LastOutcome != "failed" {
		t.Fatalf("renewals last outcome = %q, want failed", renewals.LastOutcome)
	}
	if renewals.LastSuccessAt == nil {
		t.Fatal("renewals has no last_success_at although two runs succeeded")
	}
	if renewals.AvgDurationMS != 60000 {
		t.Fatalf("renewals avg duration = %dms, want 60000", renewals.AvgDurationMS)
	}
	if strings.Contains(renewals.LastDetail, "hunter2") {
		t.Fatalf("the statistics detail leaked a credential: %q", renewals.LastDetail)
	}
	if !strings.Contains(renewals.LastDetail, "[redacted]") {
		t.Fatalf("the statistics detail was not redacted: %q", renewals.LastDetail)
	}

	// A run that has not finished is reported as running, not as a success and not as a failure.
	dunning := body.Data[byJob["dunning"]]
	if dunning.Running != 1 || dunning.Failures != 0 || dunning.LastOutcome != "running" {
		t.Fatalf("dunning = %+v, want one running run", dunning)
	}
	tickets := body.Data[byJob["tickets"]]
	if tickets.Skipped != 1 {
		t.Fatalf("tickets = %+v, want one skipped run", tickets)
	}

	// The window is a parameter and is validated.
	if rec := env.get(t, "/api/v1/staff/cron/statistics?days=0", env.support); rec.Code != http.StatusBadRequest {
		t.Fatalf("days=0: status %d, want 400", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/cron/statistics?days=30", env.support); rec.Code != http.StatusOK {
		t.Fatalf("days=30: status %d, want 200", rec.Code)
	}
}

// A retry actually runs the rule, records a run row and an audit row, and the outcome is in the
// body.
func TestCronRetryRunsTheJobAndRecordsIt(t *testing.T) {
	clearJobSwitches(t)
	env := newStaffEnv(t)
	ctx := context.Background()

	rec := env.postJSONWithKey(t, "/api/v1/staff/cron/"+lifecycle.OrderCancelJob+"/retry",
		env.admin, env.adminCSRF, map[string]any{}, "retry-order-cancel")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: status %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Job     string `json:"job"`
		RunID   string `json:"run_id"`
		Outcome string `json:"outcome"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if out.Job != lifecycle.OrderCancelJob || out.Outcome != "ok" || out.RunID == "" {
		t.Fatalf("retry response = %+v, want an ok run with an id", out)
	}

	// The run is in the same history the worker writes, so an operator sees it without a second
	// screen.
	var (
		outcome string
		runID   string
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT id, outcome FROM cron_runs WHERE job = $1 ORDER BY started_at DESC LIMIT 1`,
		lifecycle.OrderCancelJob).Scan(&runID, &outcome); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runID != out.RunID || outcome != "ok" {
		t.Fatalf("recorded run = %s/%s, want %s/ok", runID, outcome, out.RunID)
	}

	// The operator's action is audited, naming the rule and the run.
	var (
		action  string
		changes map[string]any
	)
	if err := env.pool.QueryRow(ctx, `
		SELECT action, changes FROM audit_events
		WHERE action = 'cron.retry' AND changes->>'job' = $1
		ORDER BY created_at DESC LIMIT 1`, lifecycle.OrderCancelJob).Scan(&action, &changes); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if changes["run_id"] != out.RunID {
		t.Fatalf("audit changes = %v, want run_id %s", changes, out.RunID)
	}

	// A support agent holds log:read but not settings:write, so the retry is not theirs.
	if rec := env.postJSONWithKey(t, "/api/v1/staff/cron/"+lifecycle.OrderCancelJob+"/retry",
		env.support, env.supportCSRF, map[string]any{}, "retry-as-support"); rec.Code != http.StatusForbidden {
		t.Fatalf("support retry: status %d, want 403", rec.Code)
	}
}

// A job the panel cannot construct is refused by name, not 500'd: renewals live in the worker
// process and need a gateway, a provisioner and a mail transport.
func TestCronRetryRefusesAWorkerOnlyJob(t *testing.T) {
	clearJobSwitches(t)
	env := newStaffEnv(t)

	rec := env.postJSONWithKey(t, "/api/v1/staff/cron/renewals/retry",
		env.admin, env.adminCSRF, map[string]any{}, "retry-renewals")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if problem.Code != "job_not_retryable" {
		t.Fatalf("code = %q, want job_not_retryable", problem.Code)
	}
	if !strings.Contains(problem.Detail, "worker") {
		t.Fatalf("detail = %q, want it to say where the job lives", problem.Detail)
	}
}

// A lifecycle job that is switched off is refused by name, because the operator can fix that —
// unlike a job that lives in the worker process, which no switch will make runnable from here.
func TestCronRetryDistinguishesDisabledFromWorkerOnly(t *testing.T) {
	env := newStaffEnv(t)
	// A switch that is set *after* the environment was captured, so the job exists in the process
	// but not in lifecycle.Jobs().
	t.Setenv(lifecycle.OrderCancelKillSwitch, "1")

	rec := env.postJSONWithKey(t, "/api/v1/staff/cron/"+lifecycle.OrderCancelJob+"/retry",
		env.admin, env.adminCSRF, map[string]any{}, "retry-disabled")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if problem.Code != "job_disabled" {
		t.Fatalf("code = %q, want job_disabled", problem.Code)
	}
	if !strings.Contains(problem.Detail, "kill switch") {
		t.Fatalf("detail = %q, want it to name the switch", problem.Detail)
	}
}

// The charge plan is a read that cannot charge (OPS-01).
//
// It exists because nobody enables an unattended charger without seeing what it would take. The
// test asserts the three properties that make it safe to hand to an operator: it reports the same
// invoices the sweep would offer, it answers even when the job is switched off, and it never
// resolves a gateway — so asking the question cannot move money.
func TestChargePlanNamesWhatTheSweepWouldTake(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	customerID, _ := env.seedCustomer(t, "plan@example.test", "Plan Customer")
	var methodID, invoiceID string
	if err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO payment_methods (customer_id, provider, provider_token, brand, last4, is_default)
			VALUES ($1, $2, 'pm_plan_TOKEN', 'visa', '4242', true) RETURNING id`,
			customerID, "stripe").Scan(&methodID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO billing_agreements (customer_id, payment_method_id, provider, status, consent_at)
			VALUES ($1, $2, 'stripe', 'active', now())`, customerID, methodID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
				total_minor, amount_paid_minor, issued_at, due_at, snapshot)
			VALUES ($1, 'INV-PLAN-1', 'open', 'USD', 2500, 0, 2500, 0, now(), now() - interval '2 days',
			        '{"schema_version":1}'::jsonb)
			RETURNING id`, customerID).Scan(&invoiceID)
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// Charging is off, which is the state an operator is in when they are deciding.
	rec := env.get(t, "/api/v1/staff/payments/charge-due", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var plan struct {
		Enabled    bool   `json:"enabled"`
		Reason     string `json:"reason"`
		Provider   string `json:"provider"`
		Candidates []struct {
			InvoiceID    string `json:"invoice_id"`
			AmountMinor  int64  `json:"amount_minor"`
			Amount       string `json:"amount"`
			MethodLast4  string `json:"method_last4"`
			AgreementID  string `json:"agreement_id"`
			CustomerName string `json:"customer_name"`
		} `json:"candidates"`
		Totals []struct {
			Currency string `json:"currency"`
			Minor    int64  `json:"amount_minor"`
			Invoices int    `json:"invoices"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if plan.Enabled || plan.Reason != operations.ReasonDisabled {
		t.Fatalf("enabled=%v reason=%q, want the disabled setting reported", plan.Enabled, plan.Reason)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].InvoiceID != invoiceID {
		t.Fatalf("candidates = %+v, want the overdue invoice", plan.Candidates)
	}
	row := plan.Candidates[0]
	if row.AmountMinor != 2500 || row.Amount != "25.00 USD" {
		t.Fatalf("amount = %d (%q), want 2500 (25.00 USD)", row.AmountMinor, row.Amount)
	}
	if row.MethodLast4 != "4242" || row.AgreementID == "" {
		t.Fatalf("the plan does not name the card and its mandate: %+v", row)
	}
	if len(plan.Totals) != 1 || plan.Totals[0].Minor != 2500 || plan.Totals[0].Invoices != 1 {
		t.Fatalf("totals = %+v, want one currency at 2500", plan.Totals)
	}
	// Nothing was charged by asking: the invoice is still open and no attempt was recorded.
	var status string
	var attempts int
	if err := env.pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1`, invoiceID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM payment_attempts WHERE invoice_id = $1`, invoiceID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if status != "open" || attempts != 0 {
		t.Fatalf("asking for the plan changed state: status=%s attempts=%d", status, attempts)
	}
}

// A support agent cannot enumerate the cards the panel is about to charge.
func TestChargePlanRequiresPaymentReconcile(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/payments/charge-due", env.support)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a support agent; body %s", rec.Code, rec.Body)
	}
	rec = env.get(t, "/api/v1/staff/payments/charge-due", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a session", rec.Code)
	}
}
