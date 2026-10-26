package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The failed-operation actions (ADM-18): retry, resolve and delete.
//
// These assert the properties that matter, not the happy path:
//
//   - a retry requeues the same operation key and gives it a fresh attempt budget;
//   - a retry of a fenced operation is refused with a problem, because the worker would reject
//     it anyway;
//   - a resolve demands a reason and leaves both the sentence and an audit row;
//   - a delete only accepts `failed`, so an unresolved unknown outcome cannot be swept away;
//   - each action needs the permission that matches what it does.

func seedOperation(t *testing.T, env *staffEnv, serviceID, kind, status string, generation int64) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status,
			attempts, last_error)
		VALUES ($1, $2, 'op-'||gen_random_uuid(), $3, $4, 5, 'adapter refused')
		RETURNING id`, serviceID, kind, generation, status).Scan(&id); err != nil {
		t.Fatalf("insert operation: %v", err)
	}
	return id
}

func operationState(t *testing.T, env *staffEnv, id string) (status string, attempts int, lastError *string, finished bool) {
	t.Helper()
	ctx := context.Background()
	if err := env.pool.QueryRow(ctx, `
		SELECT status, attempts, last_error, finished_at IS NOT NULL
		FROM service_operations WHERE id = $1`, id).
		Scan(&status, &attempts, &lastError, &finished); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	return status, attempts, lastError, finished
}

func auditCount(t *testing.T, env *staffEnv, action, targetID string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		action, targetID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// A retry requeues the row, resets the attempt budget and leaves an audit row.
func TestOperationRetryRequeuesFailedOperation(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "op-retry@example.test", "Retry Owner")
	serviceID, _ := env.seedService(t, customer, "active")
	id := seedOperation(t, env, serviceID, "create", "failed", 1)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/"+id+"/retry",
		env.admin, env.adminCSRF, map[string]any{"reason": "provider is back"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	status, attempts, _, finished := operationState(t, env, id)
	if status != "queued" || attempts != 0 || finished {
		t.Fatalf("operation = %s attempts=%d finished=%v, want queued/0/false",
			status, attempts, finished)
	}
	if n := auditCount(t, env, "operation.retry", id); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

// A retry of an operation whose service has moved on is refused rather than bounced by the
// worker's fencing check.
func TestOperationRetryRefusesFencedGeneration(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "op-fence@example.test", "Fence Owner")
	serviceID, _ := env.seedService(t, customer, "active")
	// The service is at generation 1 (the default); the operation was queued against 0.
	id := seedOperation(t, env, serviceID, "suspend", "failed", 0)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/"+id+"/retry",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "operation_fenced" {
		t.Fatalf("code = %q, want operation_fenced", code)
	}
	if status, _, _, _ := operationState(t, env, id); status != "failed" {
		t.Fatalf("status = %q, want the row untouched", status)
	}
}

// Retrying is a provisioning decision, so a billing clerk — who may edit a service but may not
// provision it — is refused.
func TestOperationRetryRequiresProvisionPermission(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "op-perm@example.test", "Perm Owner")
	serviceID, _ := env.seedService(t, customer, "active")
	id := seedOperation(t, env, serviceID, "create", "failed", 1)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/"+id+"/retry",
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content type = %q, want problem+json", ct)
	}
}

// Resolve demands a reason, and a successful resolve keeps the sentence and an audit row.
func TestOperationResolveRequiresReasonAndClosesRow(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "op-resolve@example.test", "Resolve Owner")
	serviceID, _ := env.seedService(t, customer, "active")
	id := seedOperation(t, env, serviceID, "create", "needs_review", 1)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/"+id+"/resolve",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status without reason = %d; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_reason" {
		t.Fatalf("code = %q, want invalid_reason", code)
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/"+id+"/resolve",
		env.admin, env.adminCSRF, map[string]any{"reason": "server exists in the provider panel"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	status, _, lastError, finished := operationState(t, env, id)
	if status != "succeeded" || !finished {
		t.Fatalf("operation = %s finished=%v, want succeeded/finished", status, finished)
	}
	if lastError == nil || !strings.Contains(*lastError, "server exists in the provider panel") {
		t.Fatalf("last_error = %v, want the operator's sentence", lastError)
	}
	if n := auditCount(t, env, "operation.resolve", id); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

// Delete accepts a failed row and refuses an unresolved unknown outcome.
func TestOperationDeleteOnlyFailed(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "op-delete@example.test", "Delete Owner")
	serviceID, _ := env.seedService(t, customer, "active")
	unknown := seedOperation(t, env, serviceID, "create", "unknown", 1)
	failed := seedOperation(t, env, serviceID, "create", "failed", 1)

	rec := env.doJSON(t, http.MethodDelete, "/api/v1/staff/operations/"+unknown,
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete unknown status = %d; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "operation_outcome_unknown" {
		t.Fatalf("code = %q, want operation_outcome_unknown", code)
	}

	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/operations/"+failed,
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete failed status = %d; body %s", rec.Code, rec.Body)
	}
	var exists bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM service_operations WHERE id = $1)`, failed).
		Scan(&exists); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if exists {
		t.Fatal("the failed operation was not deleted")
	}
	if n := auditCount(t, env, "operation.delete", failed); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

// A malformed id is a 404 that never reaches the database.
func TestOperationActionUnknownID(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/operations/not-a-uuid/retry",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
}
