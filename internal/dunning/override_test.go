package dunning_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// holdEnv is a suspendable service plus two staff users.
type holdEnv struct {
	pool       *store.Pool
	engine     *dunning.Engine
	serviceID  string
	alice, bob string
	customerID string
}

// newHoldEnv builds the chain the eligibility check joins along, and posts the receivable the
// check treats as authoritative.
func newHoldEnv(t *testing.T) *holdEnv {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	e := &holdEnv{pool: pool, engine: dunning.NewEngine(pool, dunning.DefaultPolicy())}

	e.alice = e.createUser(t, "alice@example.test")
	e.bob = e.createUser(t, "bob@example.test")

	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('hold@example.test', 'Hold')
		RETURNING id`).Scan(&e.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	var productID, priceVersionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('hold-prod', 'Hold Product') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC', 'plan', 'eu-west') RETURNING id`,
		productID).Scan(&priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	now := time.Now().UTC()
	var subscriptionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4) RETURNING id`,
		e.customerID, priceVersionID, now.AddDate(0, -1, 0), now).Scan(&subscriptionID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, subscription_id, status)
		VALUES ($1, $2, $3, 'active') RETURNING id`,
		e.customerID, productID, subscriptionID).Scan(&e.serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}

	past := now.Add(-30 * 24 * time.Hour)
	var invoiceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, subscription_id, number, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, $2, 'INV-HOLD', 'open', 'USD', 5000, 0, 5000, 0, $3, $3,
		        '{"schema_version":1}'::jsonb) RETURNING id`,
		e.customerID, subscriptionID, past).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO dunning_states (invoice_id, state, step) VALUES ($1, 'exhausted', 3)`,
		invoiceID); err != nil {
		t.Fatalf("dunning state: %v", err)
	}
	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: invoiceID,
			CustomerID: e.customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(5000, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(5000, usd)}},
		})
		return err
	}); err != nil {
		t.Fatalf("post receivable: %v", err)
	}
	return e
}

func (e *holdEnv) createUser(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, email_verified_at) VALUES ($1, 'x', now())
		RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("user: %v", err)
	}
	return id
}

// Without a hold the service is eligible, which is the baseline the override is measured
// against.
func TestEligibleWithoutHold(t *testing.T) {
	e := newHoldEnv(t)
	elig, err := e.engine.Evaluate(context.Background(), e.serviceID)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible: %s", elig.Reason)
	}
}

// A hold blocks the ordinary path, and the engine says so rather than silently refusing.
func TestHoldBlocksSuspension(t *testing.T) {
	e := newHoldEnv(t)
	if _, err := e.engine.AddHold(context.Background(), e.serviceID, "dispute", e.alice, nil); err != nil {
		t.Fatalf("AddHold: %v", err)
	}

	if _, err := e.engine.RequestSuspension(context.Background(), e.serviceID); !errors.Is(err, dunning.ErrNotEligible) {
		t.Fatalf("RequestSuspension error = %v, want ErrNotEligible", err)
	}

	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'suspend'`,
		e.serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a held service enqueued %d suspension(s)", n)
	}

	holdID, held, err := e.engine.ActiveHold(context.Background(), e.serviceID)
	if err != nil || !held || holdID == "" {
		t.Fatalf("ActiveHold = (%q, %v, %v)", holdID, held, err)
	}
}

// An override enqueues the suspension and records both actors, which is what makes "who
// decided this" answerable later.
func TestOverrideHoldRecordsBothActors(t *testing.T) {
	e := newHoldEnv(t)
	if _, err := e.engine.AddHold(context.Background(), e.serviceID, "dispute", e.alice, nil); err != nil {
		t.Fatalf("AddHold: %v", err)
	}

	elig, err := e.engine.OverrideHold(context.Background(), dunning.OverrideRequest{
		ServiceID:   e.serviceID,
		Reason:      "abuse report",
		RequestedBy: e.alice,
		ApprovedBy:  e.bob,
	})
	if err != nil {
		t.Fatalf("OverrideHold: %v", err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible after override: %s", elig.Reason)
	}

	var (
		decision, decidedBy, approvedBy string
		override                        bool
		reason                          string
	)
	if err := e.pool.QueryRow(context.Background(), `
		SELECT decision, COALESCE(decided_by::text, ''), COALESCE(approved_by::text, ''),
		       override_hold, reason
		FROM suspension_decisions WHERE service_id = $1`, e.serviceID).
		Scan(&decision, &decidedBy, &approvedBy, &override, &reason); err != nil {
		t.Fatalf("decision: %v", err)
	}
	if decision != "suspend" || !override {
		t.Fatalf("decision = %q, override = %v", decision, override)
	}
	if decidedBy != e.alice || approvedBy != e.bob {
		t.Fatalf("decided_by = %s, approved_by = %s", decidedBy, approvedBy)
	}
	if reason == "" {
		t.Fatal("the override reason was not recorded")
	}

	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'suspend'`,
		e.serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("suspend operations = %d, want 1", n)
	}
}

// One person cannot be both signatures.
func TestOverrideRefusesSelfApproval(t *testing.T) {
	e := newHoldEnv(t)
	if _, err := e.engine.AddHold(context.Background(), e.serviceID, "dispute", e.alice, nil); err != nil {
		t.Fatalf("AddHold: %v", err)
	}
	_, err := e.engine.OverrideHold(context.Background(), dunning.OverrideRequest{
		ServiceID:   e.serviceID,
		Reason:      "because",
		RequestedBy: e.alice,
		ApprovedBy:  e.alice,
	})
	if err == nil {
		t.Fatal("a self-approved override was accepted")
	}

	// The database refuses it too, so a bug in the service cannot produce one.
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO suspension_decisions (service_id, decision, reason, generation,
			decided_by, approved_by, override_hold)
		VALUES ($1, 'suspend', 'x', 1, $2, $2, true)`, e.serviceID, e.alice); err == nil {
		t.Fatal("the database accepted a self-approved override")
	}
}

// An override with no hold in place is refused: there is nothing to override, and recording
// one would be a lie.
func TestOverrideWithoutHoldRefused(t *testing.T) {
	e := newHoldEnv(t)
	_, err := e.engine.OverrideHold(context.Background(), dunning.OverrideRequest{
		ServiceID:   e.serviceID,
		Reason:      "no reason to be here",
		RequestedBy: e.alice,
		ApprovedBy:  e.bob,
	})
	if !errors.Is(err, dunning.ErrNotEligible) {
		t.Fatalf("error = %v, want ErrNotEligible", err)
	}
}

// An expired hold is not a hold: the override has nothing to override either.
func TestExpiredHoldIsNotActive(t *testing.T) {
	e := newHoldEnv(t)
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := e.engine.AddHold(context.Background(), e.serviceID, "temporary", e.alice, &past); err != nil {
		t.Fatalf("AddHold: %v", err)
	}

	if _, held, err := e.engine.ActiveHold(context.Background(), e.serviceID); err != nil || held {
		t.Fatalf("ActiveHold = (%v, %v), want not held", held, err)
	}
	// The ordinary path works again.
	if _, err := e.engine.RequestSuspension(context.Background(), e.serviceID); err != nil {
		t.Fatalf("RequestSuspension after the hold expired: %v", err)
	}
}

// An override needs a reason and two distinct actors.
func TestOverrideValidation(t *testing.T) {
	e := newHoldEnv(t)
	cases := map[string]dunning.OverrideRequest{
		"no reason":    {ServiceID: e.serviceID, RequestedBy: e.alice, ApprovedBy: e.bob},
		"no approver":  {ServiceID: e.serviceID, Reason: "x", RequestedBy: e.alice},
		"no requester": {ServiceID: e.serviceID, Reason: "x", ApprovedBy: e.bob},
		"no service":   {Reason: "x", RequestedBy: e.alice, ApprovedBy: e.bob},
	}
	for name, req := range cases {
		if _, err := e.engine.OverrideHold(context.Background(), req); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// OperationIDFor reads back the id the enqueue path created, so the caller can return a status
// URL.
func TestOperationIDFor(t *testing.T) {
	e := newHoldEnv(t)
	elig, err := e.engine.RequestSuspension(context.Background(), e.serviceID)
	if err != nil {
		t.Fatalf("RequestSuspension: %v", err)
	}
	id, err := e.engine.OperationIDFor(context.Background(), "suspend", e.serviceID, elig.Generation)
	if err != nil {
		t.Fatalf("OperationIDFor: %v", err)
	}
	if id == "" {
		t.Fatal("no operation id")
	}
	if _, err := e.engine.OperationIDFor(context.Background(), "resume", e.serviceID, 999); err == nil {
		t.Fatal("a nonexistent operation was found")
	}
}

// failingAuditor makes the audit write fail, so the test can prove the operation was not enqueued.
type failingAuditor struct{}

func (failingAuditor) RecordAction(context.Context, pgx.Tx, string, string, int64, string) error {
	return errors.New("audit write refused")
}

// recordingAuditor captures what it was asked to record.
type recordingAuditor struct {
	actions     []string
	serviceIDs  []string
	generations []int64
}

func (r *recordingAuditor) RecordAction(_ context.Context, _ pgx.Tx, action, serviceID string,
	generation int64, _ string) error {
	r.actions = append(r.actions, action)
	r.serviceIDs = append(r.serviceIDs, serviceID)
	r.generations = append(r.generations, generation)
	return nil
}

// The audit row commits with the operation, not after it: a failing audit write must leave no
// operation behind, or the record and the change could disagree.
func TestAuditCommitsWithTheOperation(t *testing.T) {
	e := newHoldEnv(t)
	_, err := e.engine.WithAuditor(failingAuditor{}).RequestSuspension(context.Background(), e.serviceID)
	if err == nil {
		t.Fatal("the suspension succeeded despite a failing audit write")
	}

	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'suspend'`,
		e.serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d suspension(s) survived a rolled-back audit write", n)
	}
}

// The auditor sees the action, the service and the generation.
func TestAuditorReceivesTheAction(t *testing.T) {
	e := newHoldEnv(t)
	rec := &recordingAuditor{}
	elig, err := e.engine.WithAuditor(rec).RequestSuspension(context.Background(), e.serviceID)
	if err != nil {
		t.Fatalf("RequestSuspension: %v", err)
	}
	if len(rec.actions) != 1 || rec.actions[0] != "service.suspend" {
		t.Fatalf("audited actions = %v", rec.actions)
	}
	if rec.serviceIDs[0] != e.serviceID || rec.generations[0] != elig.Generation {
		t.Fatalf("audited (%s, %d), want (%s, %d)",
			rec.serviceIDs[0], rec.generations[0], e.serviceID, elig.Generation)
	}

	// WithAuditor returns a copy: a second call on the original engine must not be audited by the
	// recorder that was bound to the first.
	if _, err := e.engine.RequestResume(context.Background(), e.serviceID); err != nil {
		// The resume may legitimately refuse; what matters is that the recorder saw nothing.
		_ = err
	}
	if len(rec.actions) != 1 {
		t.Fatalf("the original engine was audited too: %v", rec.actions)
	}
}
