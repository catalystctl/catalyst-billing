package operations

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
)

// What the charge sweep would do, before it does it.
//
// Nobody sensible turns on an unattended charger without seeing what it would take, and "trust me,
// it charges what is due" is not something an operator can check. So the same predicates the sweep
// uses are asked as a question: which invoices, whose card, how much, and — for every invoice that
// is due and *not* on the list — why not.
//
// # Why this cannot charge anything
//
// It never resolves a charger. There is no gateway launch, no attempt row, no provider call: the
// plan is three SELECTs and an amount format. That is a structural property rather than a promise,
// which is why the function takes a provider *name* instead of the factory the sweep takes — a
// preview that could reach a gateway is a preview that can be pointed at the wrong deployment.
//
// # Why the skip list is a second query rather than a filter
//
// The chargeable set comes from dueInvoices, the sweep's own function, so the preview cannot show a
// different batch than the sweep would charge. The refusals are then read separately, with the
// conditions inverted. Inverting a predicate is where drift hides, so a test asserts the invariant
// instead of trusting the text: nothing marked chargeable may be absent from the sweep's list, and
// nothing the sweep would take may be marked skipped.

// ChargeDuePlan is the answer to "what would the sweep charge right now".
type ChargeDuePlan struct {
	// Enabled is `gateway.off_session_enabled`: the permission the sweep needs to run at all.
	Enabled bool `json:"enabled"`
	// KillSwitchSet says whether the operator's process-level switch is on. It is reported
	// separately from Enabled because the two are set in different places, and the difference is
	// what an operator needs at three in the morning: "the setting is on but the switch is off"
	// and "nobody granted permission" are not the same problem.
	KillSwitchSet bool `json:"kill_switch_set"`
	// Reason is set when the sweep would do nothing, and names why.
	Reason string `json:"reason,omitempty"`
	// Provider is the gateway the saved methods must belong to.
	Provider string `json:"provider,omitempty"`
	// RetryWindowHours is how long a declined card is left alone.
	RetryWindowHours int `json:"retry_window_hours"`
	// Batch is the most invoices one run will take.
	Batch int `json:"batch"`
	// Candidates are the invoices the next run would offer to the charge path.
	Candidates []ChargePlanRow `json:"candidates"`
	// Skipped are due invoices that have a saved method and are outside the plan, with the reason.
	Skipped []ChargePlanRow `json:"skipped"`
	// WithoutMethod is how many due invoices have no default saved method at all. They are counted
	// rather than listed: a customer with no card is not this job's business, and listing a hundred
	// of them would bury the twelve it is about to charge.
	WithoutMethod int `json:"without_method"`
	// Truncated says the batch bound cut the candidate list, so the totals describe the next run
	// and not the whole backlog.
	Truncated bool `json:"truncated"`
	// Totals is the amount per currency the next run would attempt, formatted once so two clients
	// cannot disagree about how a currency is spelled.
	Totals []ChargePlanTotal `json:"totals"`
	// GeneratedAt is when this answer was computed.
	GeneratedAt time.Time `json:"generated_at"`
}

// ChargePlanTotal is one currency's share of the next run.
type ChargePlanTotal struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	Minor    int64  `json:"amount_minor"`
	Invoices int    `json:"invoices"`
}

// ChargePlanRow is one invoice in the plan.
type ChargePlanRow struct {
	InvoiceID     string `json:"invoice_id"`
	InvoiceNumber string `json:"invoice_number"`
	CustomerID    string `json:"customer_id"`
	CustomerName  string `json:"customer_name"`
	Currency      string `json:"currency"`
	// Amount is the outstanding balance, not the invoice total: a part-paid invoice is charged
	// what is left, and a preview that showed the total would overstate every one of them.
	Amount       string    `json:"amount"`
	AmountMinor  int64     `json:"amount_minor"`
	DueAt        time.Time `json:"due_at"`
	OverdueDays  int       `json:"overdue_days"`
	MethodID     string    `json:"method_id,omitempty"`
	MethodBrand  string    `json:"method_brand,omitempty"`
	MethodLast4  string    `json:"method_last4,omitempty"`
	AgreementID  string    `json:"agreement_id,omitempty"`
	Chargeable   bool      `json:"chargeable"`
	SkipReason   string    `json:"skip_reason,omitempty"`
	AttemptCount int       `json:"attempt_count"`
	// LastAttempt is the most recent off-session attempt, which is what the retry window reads.
	// The failure *code* is deliberately absent: the panel stores it on the payment method
	// (`last_failure_code`), not on the attempt, and reporting a method's last failure as this
	// invoice's reason would be a guess dressed as a fact.
	LastAttempt *PlanAttempt `json:"last_attempt,omitempty"`
}

// PlanAttempt is the last recorded attempt against an invoice, as the plan shows it.
type PlanAttempt struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

// Skip reasons. Stable strings, like the run reasons, so a client can localise them and a test can
// assert the exact one.
const (
	// SkipRetryWindow means a previous attempt for this invoice is still inside the window. The
	// card said no, and the provider is not asked again until the window closes.
	SkipRetryWindow = "retry_window"
	// SkipCheckoutOpen means the customer has a checkout in flight, so charging now would be the
	// second charge for one invoice.
	SkipCheckoutOpen = "checkout_open"
	// SkipNoMandate means a default method exists but no active agreement covers it: a token
	// without permission is not a token this panel may charge.
	SkipNoMandate = "no_mandate"
)

// PlanChargeDue computes the plan. `provider` is the gateway name whose methods may be charged,
// and an empty value means the installation has configured no gateway.
func PlanChargeDue(ctx context.Context, pool *store.Pool, provider string, opts Options) (ChargeDuePlan, error) {
	opts = opts.withDefaults()
	plan := ChargeDuePlan{
		Provider:      provider,
		Batch:         Batch,
		GeneratedAt:   opts.Now(),
		Candidates:    []ChargePlanRow{},
		Skipped:       []ChargePlanRow{},
		KillSwitchSet: disabled(ChargeDueKillSwitch),
	}

	values := settings.New(pool)
	enabled, err := values.Bool(ctx, "gateway", "off_session_enabled")
	if err != nil {
		return ChargeDuePlan{}, fmt.Errorf("operations: read gateway.off_session_enabled: %w", err)
	}
	plan.Enabled = enabled

	retry := DefaultChargeRetry
	if hours, err := values.Int(ctx, settings.GroupCronjob, settings.KeyChargeRetryHours); err == nil && hours > 0 {
		retry = time.Duration(hours) * time.Hour
	}
	plan.RetryWindowHours = int(retry / time.Hour)

	switch {
	case plan.KillSwitchSet:
		plan.Reason = ReasonKillSwitch
	case !enabled:
		plan.Reason = ReasonDisabled
	case provider == "":
		plan.Reason = ReasonNotConfigured
	}

	// The plan is still computed when the sweep would refuse to run: "the switch is off and it
	// would have taken 412.00 across 9 invoices" is the sentence that makes the switch a decision.
	if provider == "" {
		return plan, nil
	}

	retryBefore := opts.Now().Add(-retry)
	ids, err := dueInvoices(ctx, pool, provider, retryBefore, Batch)
	if err != nil {
		return ChargeDuePlan{}, err
	}
	plan.Truncated = len(ids) == Batch

	rows, err := planRows(ctx, pool, ids, provider, retryBefore)
	if err != nil {
		return ChargeDuePlan{}, err
	}
	// The candidate list is exactly what the sweep would take. If the annotation on one of those
	// rows names a refusal the sweep did not apply, the two queries disagree; the row is shown as
	// skipped rather than quietly advertised as a charge, and a test asserts the two never
	// disagree in the first place.
	for _, row := range rows {
		row.Chargeable = row.SkipReason == ""
		if row.Chargeable {
			plan.Candidates = append(plan.Candidates, row)
			continue
		}
		plan.Skipped = append(plan.Skipped, row)
	}

	skipped, err := refusedReason(ctx, pool, provider, retryBefore, ids)
	if err != nil {
		return ChargeDuePlan{}, err
	}
	for i := range skipped {
		skipped[i].Chargeable = false
	}
	plan.Skipped = append(plan.Skipped, skipped...)
	sort.Slice(plan.Skipped, func(i, j int) bool {
		return plan.Skipped[i].DueAt.Before(plan.Skipped[j].DueAt)
	})

	without, err := countWithoutMethod(ctx, pool, provider)
	if err != nil {
		return ChargeDuePlan{}, err
	}
	plan.WithoutMethod = without
	plan.Totals = planTotals(plan.Candidates)
	return plan, nil
}

// planRows loads the presentation of the invoices the sweep would take.
func planRows(ctx context.Context, pool *store.Pool, ids []string, provider string, retryBefore time.Time) ([]ChargePlanRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT i.id::text, i.number, i.customer_id::text,
		       COALESCE(NULLIF(c.name, ''), c.email, ''),
		       i.currency, i.total_minor - i.amount_paid_minor, i.due_at,
		       COALESCE(pm.id::text, ''), COALESCE(pm.brand, ''), COALESCE(pm.last4, ''),
		       COALESCE(ag.id::text, ''),
		       COALESCE(last.status, ''), last.created_at,
		       COALESCE(last.attempts, 0),
		       -- Which refusal, if any, applies. The conditions are the sweep's, in the negative.
		       CASE
		           WHEN EXISTS (
		               SELECT 1 FROM payment_attempts pa
		               WHERE pa.invoice_id = i.id
		                 AND pa.status IN ('created', 'pending', 'requires_action')
		                 AND pa.operation_key NOT LIKE 'offsession:%') THEN 'checkout_open'
		           WHEN last.created_at > $3 THEN 'retry_window'
		           WHEN ag.id IS NULL THEN 'no_mandate'
		           ELSE ''
		       END
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		JOIN payment_methods pm
		  ON pm.customer_id = i.customer_id AND pm.is_default AND pm.provider = $1
		LEFT JOIN billing_agreements ag
		  ON ag.payment_method_id = pm.id AND ag.status = 'active'
		LEFT JOIN LATERAL (
		    SELECT pa.status, pa.created_at,
		           count(*) OVER () AS attempts
		    FROM payment_attempts pa
		    WHERE pa.invoice_id = i.id AND pa.operation_key LIKE 'offsession:%'
		    ORDER BY pa.created_at DESC
		    LIMIT 1
		) last ON TRUE
		WHERE i.id = ANY($2::uuid[])
		ORDER BY i.due_at`, provider, ids, retryBefore)
	if err != nil {
		return nil, fmt.Errorf("operations: load the charge plan rows: %w", err)
	}
	defer rows.Close()

	var out []ChargePlanRow
	for rows.Next() {
		var (
			row          ChargePlanRow
			lastAt       *time.Time
			lastStatus   string
			attemptCount int
		)
		if err := rows.Scan(&row.InvoiceID, &row.InvoiceNumber, &row.CustomerID, &row.CustomerName,
			&row.Currency, &row.AmountMinor, &row.DueAt, &row.MethodID, &row.MethodBrand,
			&row.MethodLast4, &row.AgreementID, &lastStatus, &lastAt, &attemptCount,
			&row.SkipReason); err != nil {
			return nil, fmt.Errorf("operations: scan the charge plan row: %w", err)
		}
		out = append(out, finishPlanRow(row, lastStatus, lastAt, attemptCount))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operations: read the charge plan rows: %w", err)
	}
	return out, nil
}

// refusedReason lists the due invoices with a saved method that the sweep leaves for a reason.
func refusedReason(ctx context.Context, pool *store.Pool, provider string, retryBefore time.Time, taken []string) ([]ChargePlanRow, error) {
	// An empty "already taken" list must be an empty array, not NULL: `id <> ALL(NULL)` is NULL,
	// which would drop every row and make the skip list empty exactly when everything is chargeable.
	if taken == nil {
		taken = []string{}
	}
	rows, err := pool.Query(ctx, `
		SELECT i.id::text, i.number, i.customer_id::text,
		       COALESCE(NULLIF(c.name, ''), c.email, ''),
		       i.currency, i.total_minor - i.amount_paid_minor, i.due_at,
		       pm.id::text, COALESCE(pm.brand, ''), COALESCE(pm.last4, ''),
		       COALESCE(last.status, ''), last.created_at,
		       COALESCE(last.attempts, 0),
		       CASE
		           WHEN EXISTS (
		               SELECT 1 FROM payment_attempts pa
		               WHERE pa.invoice_id = i.id
		                 AND pa.status IN ('created', 'pending', 'requires_action')
		                 AND pa.operation_key NOT LIKE 'offsession:%') THEN 'checkout_open'
		           ELSE 'retry_window'
		       END
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		JOIN payment_methods pm
		  ON pm.customer_id = i.customer_id AND pm.is_default AND pm.provider = $1
		LEFT JOIN LATERAL (
		    SELECT pa.status, pa.created_at,
		           count(*) OVER () AS attempts
		    FROM payment_attempts pa
		    WHERE pa.invoice_id = i.id AND pa.operation_key LIKE 'offsession:%'
		    ORDER BY pa.created_at DESC
		    LIMIT 1
		) last ON TRUE
		WHERE i.status = 'open'
		  AND i.amount_paid_minor < i.total_minor
		  AND i.due_at IS NOT NULL
		  AND i.due_at <= now()
		  AND i.id <> ALL(COALESCE($2::uuid[], '{}'::uuid[]))
		  AND (
		      last.created_at > $3
		      OR EXISTS (
		          SELECT 1 FROM payment_attempts pa
		          WHERE pa.invoice_id = i.id
		            AND pa.status IN ('created', 'pending', 'requires_action')
		            AND pa.operation_key NOT LIKE 'offsession:%'))
		ORDER BY i.due_at
		LIMIT $4`, provider, taken, retryBefore, Batch)
	if err != nil {
		return nil, fmt.Errorf("operations: load the refused invoices: %w", err)
	}
	defer rows.Close()

	var out []ChargePlanRow
	for rows.Next() {
		var (
			row          ChargePlanRow
			lastAt       *time.Time
			lastStatus   string
			attemptCount int
		)
		if err := rows.Scan(&row.InvoiceID, &row.InvoiceNumber, &row.CustomerID, &row.CustomerName,
			&row.Currency, &row.AmountMinor, &row.DueAt, &row.MethodID, &row.MethodBrand,
			&row.MethodLast4, &lastStatus, &lastAt, &attemptCount, &row.SkipReason); err != nil {
			return nil, fmt.Errorf("operations: scan the refused invoice: %w", err)
		}
		out = append(out, finishPlanRow(row, lastStatus, lastAt, attemptCount))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operations: read the refused invoices: %w", err)
	}
	return out, nil
}

// countWithoutMethod counts due invoices this job cannot help with.
func countWithoutMethod(ctx context.Context, pool *store.Pool, provider string) (int, error) {
	var count int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM invoices i
		WHERE i.status = 'open'
		  AND i.amount_paid_minor < i.total_minor
		  AND i.due_at IS NOT NULL
		  AND i.due_at <= now()
		  AND NOT EXISTS (
		      SELECT 1 FROM payment_methods pm
		      WHERE pm.customer_id = i.customer_id AND pm.is_default AND pm.provider = $1)`,
		provider).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("operations: count invoices without a saved method: %w", err)
	}
	return count, nil
}

// finishPlanRow fills the derived fields one row shares between the two queries.
func finishPlanRow(row ChargePlanRow, lastStatus string, lastAt *time.Time, attempts int) ChargePlanRow {
	row.Amount = formatPlanAmount(row.AmountMinor, row.Currency)
	row.AttemptCount = attempts
	if lastStatus != "" && lastAt != nil {
		row.LastAttempt = &PlanAttempt{Status: lastStatus, At: lastAt.UTC()}
	}
	return row
}

// planTotals sums the plan per currency, biggest first so the largest exposure is the first line.
func planTotals(rows []ChargePlanRow) []ChargePlanTotal {
	type sum struct {
		minor int64
		count int
	}
	byCurrency := map[string]*sum{}
	for _, row := range rows {
		cur, ok := byCurrency[row.Currency]
		if !ok {
			cur = &sum{}
			byCurrency[row.Currency] = cur
		}
		cur.minor += row.AmountMinor
		cur.count++
	}
	out := make([]ChargePlanTotal, 0, len(byCurrency))
	for code, s := range byCurrency {
		out = append(out, ChargePlanTotal{
			Currency: code,
			Amount:   formatPlanAmount(s.minor, code),
			Minor:    s.minor,
			Invoices: s.count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minor != out[j].Minor {
			return out[i].Minor > out[j].Minor
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// formatPlanAmount renders an amount the way every other money response does.
//
// An unknown currency code is formatted as a bare number rather than refused: a plan that fails to
// render because one invoice is priced in a currency this build does not know would hide the other
// eleven.
func formatPlanAmount(minor int64, code string) string {
	cur, ok := money.LookupCurrency(code)
	if !ok {
		// A code this build does not know is rendered at scale 2 rather than refused: the schema's
		// own assumption for an unknown code, and a plan that failed to render because of one
		// invoice would hide the other eleven.
		cur, _ = money.NewCurrency(code, 2)
	}
	return money.New(minor, cur).Format()
}
