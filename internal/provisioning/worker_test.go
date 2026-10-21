package provisioning_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// memoryBackend models a provider that remembers what it created independently of
// the adapter process. This is what makes it possible to test the case that
// matters: the adapter restarts and loses its in-process idempotency map, while
// the provider still holds the resource.
type memoryBackend struct {
	resources map[string]string // operation key -> external id
	nextID    atomic.Int64
}

func (b *memoryBackend) create(opKey string) string {
	if id, ok := b.resources[opKey]; ok {
		return id
	}
	id := "srv_" + string(rune('a'+b.nextID.Add(1)%26))
	b.resources[opKey] = id
	return id
}

// adapter is one process's view of the backend. Its idempotency cache is
// deliberately per-instance, exactly like the fixture's.
type adapter struct {
	name    string
	backend *memoryBackend
	cache   map[string]string
	// loseReplies makes Create create the resource and then fail, which is the
	// timeout case.
	loseReplies bool
	// lookupBroken makes Lookup fail, so the core cannot establish the truth.
	lookupBroken bool
	// emptyExternalID makes Create report success without an id.
	emptyExternalID bool
}

func newAdapter(name string, backend *memoryBackend) *adapter {
	return &adapter{name: name, backend: backend, cache: map[string]string{}}
}

func (a *adapter) Name() string           { return a.name }
func (a *adapter) Capabilities() []string { return []string{"create", "lookup"} }

func (a *adapter) Create(_ context.Context, req provisioning.CreateRequest) (extproto.ProvisionCreateResult, error) {
	if id, ok := a.cache[req.OperationKey]; ok {
		return extproto.ProvisionCreateResult{ExternalID: id, Status: "running"}, nil
	}
	id := a.backend.create(req.OperationKey)
	a.cache[req.OperationKey] = id
	if a.loseReplies {
		// The resource exists; the reply does not arrive.
		return extproto.ProvisionCreateResult{}, extproto.ErrTimeout
	}
	if a.emptyExternalID {
		return extproto.ProvisionCreateResult{Status: "running"}, nil
	}
	return extproto.ProvisionCreateResult{ExternalID: id, Status: "running"}, nil
}

func (a *adapter) Lookup(_ context.Context, opKey string) (extproto.ProvisionLookupResult, error) {
	if a.lookupBroken {
		return extproto.ProvisionLookupResult{}, errors.New("lookup unavailable")
	}
	if id, ok := a.backend.resources[opKey]; ok {
		return extproto.ProvisionLookupResult{Found: true, ExternalID: id, Status: "running"}, nil
	}
	return extproto.ProvisionLookupResult{Found: false}, nil
}

func (a *adapter) Inspect(_ context.Context, externalID string) (extproto.ProvisionInspectResult, error) {
	return extproto.ProvisionInspectResult{ExternalID: externalID, Status: "running"}, nil
}

func (a *adapter) Suspend(context.Context, string, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}
func (a *adapter) Resume(context.Context, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}
func (a *adapter) Terminate(context.Context, string, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}

// setup creates a customer, product, service and one queued create operation.
func setup(t *testing.T) (*store.Pool, string, string) {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	var customerID, productID, serviceID, opKey string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('p@example.test', 'P') RETURNING id`,
		).Scan(&customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ('vps', 'VPS') RETURNING id`).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, product_id, status, generation, config)
			VALUES ($1, $2, 'pending', 1, '{"plan_id":"small","region":"eu-west"}')
			RETURNING id`, customerID, productID).Scan(&serviceID); err != nil {
			return err
		}
		opKey = "create:" + serviceID
		_, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'create', $2, 1, 'queued')`, serviceID, opKey)
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return pool, serviceID, opKey
}

func serviceState(t *testing.T, pool *store.Pool, serviceID string) (status, externalID string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(external_id, '') FROM services WHERE id = $1`, serviceID).
		Scan(&status, &externalID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	return status, externalID
}

// The reviewer's blocker: a create whose reply is lost, followed by a restart of
// the extension. Reconciliation must not provision a second server.
func TestReconcileAfterExtensionRestartDoesNotDuplicate(t *testing.T) {
	pool, serviceID, opKey := setup(t)
	backend := &memoryBackend{resources: map[string]string{}}

	// First process: creates the resource, loses the reply.
	first := newAdapter("prov-1", backend)
	first.loseReplies = true
	worker := provisioning.NewWorker(pool, first)
	worker.CallTimeout = 200 * time.Millisecond

	results, err := worker.Run(context.Background(), 1)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "unknown" {
		t.Fatalf("first attempt = %+v, want unknown", results)
	}
	created := backend.resources[opKey]
	if created == "" {
		t.Fatal("the backend did not record a resource; the test proves nothing")
	}

	// Second process: a fresh adapter with an empty idempotency cache, which is
	// exactly what a restart produces.
	second := newAdapter("prov-2", backend)
	worker2 := provisioning.NewWorker(pool, second)

	reconciled, err := worker2.Reconcile(context.Background(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(reconciled) != 1 || reconciled[0].Outcome != "succeeded" {
		t.Fatalf("reconcile = %+v, want one success", reconciled)
	}

	// The service points at the resource that already existed.
	status, externalID := serviceState(t, pool, serviceID)
	if status != "active" {
		t.Fatalf("service status = %s, want active", status)
	}
	if externalID != created {
		t.Fatalf("service external id = %q, want the existing %q (a duplicate was provisioned)",
			externalID, created)
	}

	// Exactly one resource exists in the backend.
	var count int
	for range backend.resources {
		count++
	}
	if count != 1 {
		t.Fatalf("%d resources created, want 1", count)
	}
}

// When the adapter cannot answer, the operation must stay in review rather than
// being retried into a duplicate.
func TestReconcileWithoutAnswerStaysInReview(t *testing.T) {
	pool, _, opKey := setup(t)
	backend := &memoryBackend{resources: map[string]string{}}

	first := newAdapter("prov-1", backend)
	first.loseReplies = true
	worker := provisioning.NewWorker(pool, first)
	worker.CallTimeout = 200 * time.Millisecond
	if _, err := worker.Run(context.Background(), 1); err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	// The next process cannot perform lookups at all.
	second := newAdapter("prov-2", backend)
	second.lookupBroken = true
	worker2 := provisioning.NewWorker(pool, second)

	reconciled, err := worker2.Reconcile(context.Background(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(reconciled) != 1 || reconciled[0].Outcome != "needs_review" {
		t.Fatalf("reconcile = %+v, want needs_review", reconciled)
	}
	// No second resource was created.
	if len(backend.resources) != 1 {
		t.Fatalf("%d resources, want 1", len(backend.resources))
	}
	_ = opKey
}

// An operation stranded in 'running' by a worker that died must be recoverable,
// not wedged forever. The claim query treats an expired lease as claimable, so
// recovery does not depend on a separate pass having run.
func TestStrandedOperationIsClaimedAfterLeaseExpiry(t *testing.T) {
	pool, serviceID, _ := setup(t)
	ctx := context.Background()

	// Simulate a crash between claim and finish.
	if _, err := pool.Exec(ctx, `
		UPDATE service_operations
		SET status = 'running', started_at = now() - interval '1 hour', attempts = 1
		WHERE service_id = $1`, serviceID); err != nil {
		t.Fatal(err)
	}

	backend := &memoryBackend{resources: map[string]string{}}
	worker := provisioning.NewWorker(pool, newAdapter("prov", backend))
	worker.LeaseDuration = 5 * time.Minute

	results, err := worker.Run(ctx, 1)
	if err != nil {
		t.Fatalf("run on an expired lease: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "succeeded" {
		t.Fatalf("results = %+v, want one success", results)
	}
	status, _ := serviceState(t, pool, serviceID)
	if status != "active" {
		t.Fatalf("service status = %s, want active", status)
	}
}

// A stranded operation is also recoverable by the explicit reaper, which is what
// an operator or a scheduled sweep uses to make the recovery visible.
func TestReaperRecoversStrandedOperation(t *testing.T) {
	pool, serviceID, _ := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		UPDATE service_operations
		SET status = 'running', started_at = now() - interval '1 hour'
		WHERE service_id = $1`, serviceID); err != nil {
		t.Fatal(err)
	}

	worker := provisioning.NewWorker(pool, newAdapter("prov", &memoryBackend{resources: map[string]string{}}))
	worker.LeaseDuration = 5 * time.Minute

	n, err := worker.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d operations, want 1", n)
	}

	var status, lastError string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(last_error, '') FROM service_operations WHERE service_id = $1`,
		serviceID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "retry_wait" {
		t.Fatalf("status = %s, want retry_wait", status)
	}
	if lastError == "" {
		t.Fatal("the reap did not record why it acted")
	}
}

// A live lease must not be reaped: only genuinely expired work is reclaimed.
func TestReaperLeavesLiveLeaseAlone(t *testing.T) {
	pool, serviceID, _ := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		UPDATE service_operations SET status = 'running', started_at = now() WHERE service_id = $1`,
		serviceID); err != nil {
		t.Fatal(err)
	}

	worker := provisioning.NewWorker(pool, newAdapter("prov", &memoryBackend{resources: map[string]string{}}))
	worker.LeaseDuration = 5 * time.Minute

	n, err := worker.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reaped %d operations with a live lease, want 0", n)
	}
}

// A success with no external id is not a provisioned service: every later
// suspend or terminate would call the adapter with an empty target.
func TestEmptyExternalIDIsNotSuccess(t *testing.T) {
	pool, serviceID, _ := setup(t)

	a := newAdapter("prov", &memoryBackend{resources: map[string]string{}})
	a.emptyExternalID = true
	worker := provisioning.NewWorker(pool, a)

	results, err := worker.Run(context.Background(), 1)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "needs_review" {
		t.Fatalf("results = %+v, want needs_review", results)
	}

	status, externalID := serviceState(t, pool, serviceID)
	if status == "active" {
		t.Fatal("service was marked active without an external id")
	}
	if externalID != "" {
		t.Fatalf("external id = %q, want empty", externalID)
	}
}

// Lookup returning "not found" is authoritative: the create never took effect, so
// retrying with the same key is safe.
func TestReconcileNotFoundRequeues(t *testing.T) {
	pool, serviceID, _ := setup(t)
	ctx := context.Background()

	// Put the operation into unknown without any resource behind it.
	if _, err := pool.Exec(ctx, `
		UPDATE service_operations SET status = 'unknown' WHERE service_id = $1`, serviceID); err != nil {
		t.Fatal(err)
	}

	backend := &memoryBackend{resources: map[string]string{}}
	worker := provisioning.NewWorker(pool, newAdapter("prov", backend))

	reconciled, err := worker.Reconcile(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].Outcome != "retry_wait" {
		t.Fatalf("reconcile = %+v, want retry_wait", reconciled)
	}

	// The retry then provisions it exactly once.
	if _, err := worker.Run(ctx, 1); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(backend.resources) != 1 {
		t.Fatalf("%d resources, want 1", len(backend.resources))
	}
}
