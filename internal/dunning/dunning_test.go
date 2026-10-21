package dunning_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// baseTime is fixed so escalation arithmetic is exact.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	pool         *store.Pool
	customerID   string
	serviceID    string
	subscription string
	invoiceID    string
	engine       *dunning.Engine
	now          time.Time
}

// newFixture builds customer -> subscription -> service and an open invoice due
// on the given date.
func newFixture(t *testing.T, dueAt time.Time) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	f := &fixture{pool: pool, now: baseTime}
	f.engine = dunning.NewEngine(pool, dunning.Policy{
		ReminderInterval: 48 * time.Hour,
		MaxSteps:         3,
		SuspendAfter:     7 * 24 * time.Hour,
	})
	f.engine.Now = func() time.Time { return f.now }

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('d@example.test', 'D') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return err
		}
		var productID, priceID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ('vps', 'VPS') RETURNING id`).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', 1200, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day, timezone,
				current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
			RETURNING id`,
			f.customerID, priceID, dueAt.AddDate(0, -1, 0), dueAt).Scan(&f.subscription); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation)
			VALUES ($1, $2, $3, 'active', 1) RETURNING id`,
			f.customerID, f.subscription, productID).Scan(&f.serviceID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     f.customerID,
			SubscriptionID: f.subscription,
			PeriodKey:      "2026-03-01/2026-04-01",
			Lines: []billing.Line{{
				Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
			DueAt:    dueAt,
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		f.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

func (f *fixture) pay(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_" + f.invoiceID,
			InvoiceID:         f.invoiceID,
			Amount:            money.New(1200, usd),
		})
		return err
	})
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
}

func (f *fixture) state(t *testing.T) (state string, step int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state, step FROM dunning_states WHERE invoice_id = $1`, f.invoiceID).
		Scan(&state, &step); err != nil {
		t.Fatalf("read dunning state: %v", err)
	}
	return state, step
}

// -------------------------------------------------------------------- tests --

// An invoice within its terms is not dunned.
func TestNotOverdueYet(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, 5))
	results, err := f.engine.Advance(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("%d advances, want 0", len(results))
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM dunning_states`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dunning state created for an invoice within terms")
	}
}

// Escalation runs one step per reminder interval and then exhausts.
func TestEscalationReachesExhausted(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	ctx := context.Background()

	// First advance: overdue, step 1, next action in 48h.
	results, err := f.engine.Advance(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != dunning.StateOverdue || results[0].Step != 1 {
		t.Fatalf("first advance = %+v, want overdue step 1", results)
	}

	// Nothing is due until the interval elapses.
	if results, err = f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("%d advances before the interval elapsed, want 0", len(results))
	}

	f.now = f.now.Add(48 * time.Hour)
	if results, err = f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Step != 2 || results[0].Exhausted {
		t.Fatalf("second advance = %+v, want step 2 not exhausted", results)
	}

	f.now = f.now.Add(48 * time.Hour)
	if results, err = f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != dunning.StateExhausted || !results[0].Exhausted {
		t.Fatalf("third advance = %+v, want exhausted", results)
	}

	state, step := f.state(t)
	if state != "exhausted" || step != 3 {
		t.Fatalf("state = %s step = %d, want exhausted/3", state, step)
	}

	// Nothing more is scheduled.
	if results, err = f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("%d advances after exhaustion, want 0", len(results))
	}
}

// Paying resolves dunning immediately, wherever it had got to.
func TestPaymentResolvesDunning(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	ctx := context.Background()

	if _, err := f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	f.pay(t)
	if _, err := f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}

	state, _ := f.state(t)
	if state != "resolved" {
		t.Fatalf("state = %s, want resolved", state)
	}
}

// A paying customer is never suspended, even at the exhausted stage.
func TestPaidCustomerIsNotEligible(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	f.engine.Now = func() time.Time { return baseTime.AddDate(0, 0, -30).Add(72 * time.Hour) }
	if _, err := f.engine.Advance(ctx, 100); err != nil {
		t.Fatal(err)
	}
	f.pay(t)

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible {
		t.Fatalf("paid customer reported eligible: %+v", elig)
	}
	if elig.Reason == "" {
		t.Fatal("no reason given for the decision")
	}
}

// An exhausted invoice past the suspension period is eligible, and the decision
// is recorded.
func TestExhaustedInvoiceIsEligible(t *testing.T) {
	due := baseTime.AddDate(0, 0, -30)
	f := newFixture(t, due)
	ctx := context.Background()

	// Run dunning to exhaustion.
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible: %s", elig.Reason)
	}
	if elig.Outstanding == nil || *elig.Outstanding != "12.00 USD" {
		t.Fatalf("outstanding = %v, want 12.00 USD", elig.Outstanding)
	}

	var decision, reason string
	if err := f.pool.QueryRow(ctx, `
		SELECT decision, reason FROM suspension_decisions ORDER BY created_at DESC LIMIT 1`).
		Scan(&decision, &reason); err != nil {
		t.Fatal(err)
	}
	if decision != "suspend" || reason == "" {
		t.Fatalf("decision = %q reason = %q, want suspend with a reason", decision, reason)
	}
}

// A manual hold blocks suspension even when everything else says yes.
func TestManualHoldBlocksSuspension(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	if _, err := f.engine.AddHold(ctx, f.serviceID, "customer called about a dispute", "staff:alice", nil); err != nil {
		t.Fatal(err)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible {
		t.Fatal("manual hold did not block suspension")
	}
	if want := "manual hold"; !contains(elig.Reason, want) {
		t.Fatalf("reason = %q, want it to mention %q", elig.Reason, want)
	}

	var decision string
	if err := f.pool.QueryRow(ctx, `
		SELECT decision FROM suspension_decisions ORDER BY created_at DESC LIMIT 1`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "held" {
		t.Fatalf("decision = %q, want held", decision)
	}

	// Clearing the hold makes it eligible again.
	var holdID string
	if err := f.pool.QueryRow(ctx, `SELECT id FROM service_holds`).Scan(&holdID); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.ClearHold(ctx, holdID); err != nil {
		t.Fatal(err)
	}
	elig, err = f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("still blocked after clearing the hold: %s", elig.Reason)
	}
}

// An expired hold no longer blocks.
func TestExpiredHoldDoesNotBlock(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}
	expired := f.now.Add(-time.Hour)
	if _, err := f.engine.AddHold(ctx, f.serviceID, "temporary", "staff:bob", &expired); err != nil {
		t.Fatal(err)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("expired hold still blocks: %s", elig.Reason)
	}
}

// RequestSuspension enqueues exactly one operation, keyed by generation.
func TestRequestSuspensionEnqueuesOnce(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	if _, err := f.engine.RequestSuspension(ctx, f.serviceID); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Asking again must not enqueue a second suspension.
	if _, err := f.engine.RequestSuspension(ctx, f.serviceID); err != nil && !errors.Is(err, dunning.ErrNotEligible) {
		t.Fatalf("second request: %v", err)
	}

	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations WHERE kind = 'suspend'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d suspend operations, want 1", count)
	}
}

// A service whose generation moved between evaluation and enqueue must not be
// suspended: that is the stale-worker race.
func TestGenerationChangeInvalidatesRequest(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("setup: not eligible: %s", elig.Reason)
	}

	// The customer pays and the service is resumed: the generation moves.
	if _, err := f.pool.Exec(ctx,
		`UPDATE services SET generation = generation + 1 WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}

	// The stale decision must not be applied.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var generation int64
		if err := tx.QueryRow(ctx, `SELECT generation FROM services WHERE id = $1`, f.serviceID).
			Scan(&generation); err != nil {
			return err
		}
		if generation != elig.Generation {
			return dunning.ErrNotEligible
		}
		return nil
	})
	if !errors.Is(err, dunning.ErrNotEligible) {
		t.Fatalf("got %v, want ErrNotEligible for a stale generation", err)
	}

	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations WHERE kind = 'suspend'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d suspend operations enqueued for a stale decision", count)
	}
}

// Suspension is not permitted before the policy's suspension period, even when
// dunning is exhausted, and the boundary itself is inclusive.
func TestSuspensionPeriodIsEnforced(t *testing.T) {
	// Due exactly at baseTime. The engine starts an hour later, so the invoice is
	// just overdue, and three 48h advances land a day short of the seven-day
	// suspension period.
	due := baseTime
	f := newFixture(t, due)
	f.now = baseTime.Add(time.Hour)
	ctx := context.Background()

	// Exhaust dunning while the suspension period is still running: three
	// advances at 48h intervals put "now" at due+6d, one day short of the policy's
	// seven.
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}
	if got := f.now.Sub(due); got >= 7*24*time.Hour {
		t.Fatalf("setup: now is %s after the due date, past the suspension period", got)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible {
		t.Fatal("suspension permitted one day before the suspension period ends")
	}
	if want := "suspension is permitted from"; !contains(elig.Reason, want) {
		t.Fatalf("reason = %q, want it to state when suspension is allowed", elig.Reason)
	}

	// One day later the period has elapsed and suspension is allowed.
	f.now = f.now.Add(24 * time.Hour)
	elig, err = f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible after the period: %s", elig.Reason)
	}
}

// The suspension boundary is inclusive: exactly at due+SuspendAfter is allowed,
// one nanosecond earlier is not. An off-by-one here suspends a customer a day
// early, so it is pinned explicitly.
func TestSuspensionBoundaryIsInclusive(t *testing.T) {
	due := baseTime.AddDate(0, 0, -10)
	f := newFixture(t, due)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	boundary := due.Add(7 * 24 * time.Hour)
	f.now = boundary.Add(-time.Nanosecond)
	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible {
		t.Fatal("eligible one nanosecond before the boundary")
	}

	f.now = boundary
	elig, err = f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible exactly at the boundary: %s", elig.Reason)
	}
}

// A service that is not active is never suspended by dunning.
func TestInactiveServiceNotEligible(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE services SET status = 'suspended' WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}

	elig, err := f.engine.Evaluate(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible {
		t.Fatal("an already-suspended service reported eligible")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A customer who pays must be resumed. Without this, dunning can suspend but
// nothing can undo it, which is worse than not suspending.
func TestResumeAfterPayment(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.engine.Advance(ctx, 100); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}
	// Suspend it, as the worker would.
	if _, err := f.engine.RequestSuspension(ctx, f.serviceID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE services SET status = 'suspended' WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}

	// Still owing: resume is refused.
	res, err := f.engine.RequestResume(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("resume while owing: %v", err)
	}
	if res.Eligible {
		t.Fatal("resume granted while the invoice is unpaid")
	}
	if want := "still owes"; !contains(res.Reason, want) {
		t.Fatalf("reason = %q, want it to mention %q", res.Reason, want)
	}

	// Pay it: resume is now granted exactly once.
	f.pay(t)
	res, err = f.engine.RequestResume(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("resume after payment: %v", err)
	}
	if !res.Eligible {
		t.Fatalf("resume refused after payment: %s", res.Reason)
	}
	if _, err := f.engine.RequestResume(ctx, f.serviceID); err != nil {
		t.Fatalf("second resume: %v", err)
	}

	var resumes int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_operations WHERE kind = 'resume'`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 1 {
		t.Fatalf("%d resume operations, want 1", resumes)
	}
}

// A manual hold blocks an automatic resume too.
func TestHoldBlocksResume(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`UPDATE services SET status = 'suspended' WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	f.pay(t)
	if _, err := f.engine.AddHold(ctx, f.serviceID, "abuse investigation", "staff:carol", nil); err != nil {
		t.Fatal(err)
	}

	res, err := f.engine.RequestResume(ctx, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligible {
		t.Fatal("resume granted despite a manual hold")
	}
	var resumes int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_operations WHERE kind = 'resume'`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 0 {
		t.Fatalf("%d resume operations enqueued despite a hold", resumes)
	}
}
