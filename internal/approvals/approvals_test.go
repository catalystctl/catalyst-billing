package approvals_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

type env struct {
	pool *store.Pool
	svc  *approvals.Service
	alice,
	bob string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	svc, err := approvals.New(pool)
	if err != nil {
		t.Fatalf("approvals.New: %v", err)
	}
	e := &env{pool: pool, svc: svc}
	e.alice = e.createUser(t, "alice@example.test")
	e.bob = e.createUser(t, "bob@example.test")
	return e
}

func (e *env) createUser(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ($1, 'x', now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func (e *env) request(t *testing.T, action, ref, requester, reason string) approvals.Approval {
	t.Helper()
	var a approvals.Approval
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = e.svc.Request(ctx, tx, action, ref, requester, reason)
		return err
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return a
}

const ref = "0f8fad5b-d9cb-469f-a165-70867728950e"

// A high-value action needs a second, different person.
func TestRequestThenApproveByAnotherUser(t *testing.T) {
	e := newEnv(t)
	a := e.request(t, approvals.ActionRefund, ref, e.alice, "customer asked")
	if a.Status != approvals.StatusPending || a.RequestedBy != e.alice {
		t.Fatalf("approval = %+v", a)
	}

	approved, err := e.svc.Approve(context.Background(), a.ID, e.bob)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != approvals.StatusApproved || approved.ApprovedBy != e.bob {
		t.Fatalf("approved = %+v", approved)
	}
	if approved.DecidedAt == nil {
		t.Fatal("an approved request has no decision time")
	}
}

// One person cannot provide both signatures. The service says so, and the database
// refuses it even if the service is bypassed.
func TestSelfApprovalRefused(t *testing.T) {
	e := newEnv(t)
	a := e.request(t, approvals.ActionRefund, ref, e.alice, "")

	if _, err := e.svc.Approve(context.Background(), a.ID, e.alice); !errors.Is(err, approvals.ErrSelfApproval) {
		t.Fatalf("self-approval error = %v, want ErrSelfApproval", err)
	}

	// The constraint is the last line of defence.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE approvals SET approved_by = requested_by WHERE id = $1`, a.ID); err == nil {
		t.Fatal("the database accepted a self-approval")
	}
}

func TestDoubleApprovalRefused(t *testing.T) {
	e := newEnv(t)
	a := e.request(t, approvals.ActionRefund, ref, e.alice, "")
	if _, err := e.svc.Approve(context.Background(), a.ID, e.bob); err != nil {
		t.Fatalf("approve: %v", err)
	}
	carol := e.createUser(t, "carol@example.test")
	if _, err := e.svc.Approve(context.Background(), a.ID, carol); !errors.Is(err, approvals.ErrNotPending) {
		t.Fatalf("second approval error = %v, want ErrNotPending", err)
	}
}

func TestReject(t *testing.T) {
	e := newEnv(t)
	a := e.request(t, approvals.ActionTerminateBatch, ref, e.alice, "")
	rejected, err := e.svc.Reject(context.Background(), a.ID, e.bob, "no ticket reference")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.Status != approvals.StatusRejected {
		t.Fatalf("status = %q", rejected.Status)
	}
	if rejected.Reason == "" || rejected.Reason == a.Reason {
		t.Fatalf("rejection reason was not recorded: %q", rejected.Reason)
	}
}

// Asking twice for the same decision returns the existing pending row rather than
// creating a second one, which is how an action gets approved twice.
func TestRequestIsIdempotentPerReference(t *testing.T) {
	e := newEnv(t)
	first := e.request(t, approvals.ActionRefund, ref, e.alice, "first")
	second := e.request(t, approvals.ActionRefund, ref, e.bob, "second")
	if first.ID != second.ID {
		t.Fatalf("two pending approvals for one action: %s and %s", first.ID, second.ID)
	}

	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM approvals WHERE reference_id = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("approval rows = %d, want 1", n)
	}
}

// The threshold is configuration, and a threshold of zero disables the check
// deliberately rather than by accident.
func TestRefundThreshold(t *testing.T) {
	pool := storetest.DB(t)
	t.Setenv("BILLING_REFUND_APPROVAL_THRESHOLD_MINOR", "50000")
	svc, err := approvals.New(pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.RefundNeedsApproval(49999) {
		t.Fatal("499.99 needed approval")
	}
	if !svc.RefundNeedsApproval(50000) {
		t.Fatal("500.00 did not need approval")
	}

	t.Setenv("BILLING_REFUND_APPROVAL_THRESHOLD_MINOR", "0")
	svc, err = approvals.New(pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.RefundNeedsApproval(1 << 40) {
		t.Fatal("a zero threshold still required approval")
	}
}

// A malformed threshold must not silently fall back: falling back could lower the bar.
func TestMalformedThresholdRefused(t *testing.T) {
	pool := storetest.DB(t)
	t.Setenv("BILLING_REFUND_APPROVAL_THRESHOLD_MINOR", "five hundred")
	if _, err := approvals.New(pool); err == nil {
		t.Fatal("a non-numeric threshold was accepted")
	}
}

func TestPendingForAndGet(t *testing.T) {
	e := newEnv(t)
	a := e.request(t, approvals.ActionHoldOverride, ref, e.alice, "override")

	got, ok, err := e.svc.PendingFor(context.Background(), approvals.ActionHoldOverride, ref)
	if err != nil || !ok {
		t.Fatalf("PendingFor = (%v, %v)", ok, err)
	}
	if got.ID != a.ID {
		t.Fatalf("PendingFor id = %s, want %s", got.ID, a.ID)
	}

	if _, ok, err := e.svc.PendingFor(context.Background(), approvals.ActionRefund, ref); err != nil || ok {
		t.Fatalf("PendingFor(other action) = (%v, %v)", ok, err)
	}

	if _, err := e.svc.Get(context.Background(),
		"11111111-1111-1111-1111-111111111111"); !errors.Is(err, approvals.ErrNotFound) {
		t.Fatalf("Get(unknown) error = %v", err)
	}
}

func TestListFilters(t *testing.T) {
	e := newEnv(t)
	e.request(t, approvals.ActionRefund, ref, e.alice, "")
	e.request(t, approvals.ActionTerminateBatch,
		"22222222-2222-2222-2222-222222222222", e.alice, "")

	all, _, err := e.svc.List(context.Background(), approvals.Filter{}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d, want 2", len(all))
	}

	refunds, _, err := e.svc.List(context.Background(),
		approvals.Filter{Action: approvals.ActionRefund}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(refunds) != 1 || refunds[0].Action != approvals.ActionRefund {
		t.Fatalf("refunds = %+v", refunds)
	}

	pending, _, err := e.svc.List(context.Background(),
		approvals.Filter{Status: approvals.StatusPending}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d", len(pending))
	}
}

func TestUnknownActionRefused(t *testing.T) {
	e := newEnv(t)
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.svc.Request(ctx, tx, "wire_transfer", ref, e.alice, "")
		return err
	})
	if err == nil {
		t.Fatal("an unknown action was accepted")
	}
}
