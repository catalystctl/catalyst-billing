package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Posting the credit a downgrade owes over HTTP (CRD-06, UPG-04, CRD-10).
//
// The economic event is proven in internal/credit; what these tests pin is the *money path over
// HTTP*: that the operator's route posts the credit and applies the change, that a replay is a
// repost and not a second note, and that the guards a money mutation needs are actually applied.

// downgradeBody is the response of a posted downgrade credit.
type downgradeBody struct {
	UpgradeID     string `json:"upgrade_id"`
	ServiceID     string `json:"service_id"`
	CreditNoteID  string `json:"credit_note_id"`
	Number        string `json:"number"`
	Currency      string `json:"currency"`
	AmountMinor   int64  `json:"amount_minor"`
	AlreadyPosted bool   `json:"already_posted"`
	Applied       bool   `json:"applied"`
}

// seedDowngrade records a pending downgrade row on a service and returns its id and amount.
func (e *authEnv) seedDowngrade(t *testing.T, serviceID string, creditMinor int64) string {
	t.Helper()
	productID, priceVersionID := e.planFacts(t, serviceID)
	var upgradeID string
	if err := e.pool.QueryRow(t.Context(), `
		INSERT INTO service_upgrades
			(service_id, from_price_version_id, to_price_version_id, prorated_minor, currency, status)
		VALUES ($1, $2, $2, $3, 'USD', 'pending')
		RETURNING id`, serviceID, priceVersionID, -creditMinor).Scan(&upgradeID); err != nil {
		t.Fatalf("seed downgrade (%s): %v", productID, err)
	}
	return upgradeID
}

// The operator posts the credit and the service moves: the note exists, the balance holds it, the
// subscription takes the cheaper price, and the row is applied.
func TestStaffPostsADowngradeCreditAndItIsApplied(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "downgrade-credit@example.test", "20.00")
	upgradeID := env.seedDowngrade(t, tenant.serviceID, 825)

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/staff/upgrades/"+upgradeID+"/credit", env.billing, env.billingCSRF,
		freshKey("downgrade-credit"), map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post downgrade credit: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeDowngrade(t, rec)
	if body.AmountMinor != 825 || body.Number == "" || body.CreditNoteID == "" {
		t.Fatalf("the response does not name the note: %+v", body)
	}
	if body.AlreadyPosted {
		t.Fatal("the first posting reported itself as a replay")
	}
	if !body.Applied {
		t.Fatal("the downgrade was not applied after its credit was posted")
	}

	var (
		status string
		noteID *string
	)
	if err := env.pool.QueryRow(t.Context(), `
		SELECT status, credit_note_id::text FROM service_upgrades WHERE id = $1`, upgradeID).
		Scan(&status, &noteID); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if noteID == nil || *noteID != body.CreditNoteID {
		t.Fatalf("credit_note_id = %v, want %s", noteID, body.CreditNoteID)
	}
	if status != "applied" {
		t.Fatalf("upgrade status = %q, want applied", status)
	}

	// The balance is the ledger's sum, so it is the evidence the money moved.
	var balance int64
	if err := env.pool.QueryRow(t.Context(), `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = 'customer_credit'`, tenant.customerID).
		Scan(&balance); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 825 {
		t.Fatalf("credit balance = %d, want 825", balance)
	}

	// The audit row is written in the posting's own transaction.
	if n := env.auditCount(t, "service.downgrade_credited", tenant.serviceID); n != 1 {
		t.Fatalf("audit rows for service.downgrade_credited = %d, want 1", n)
	}
}

// A second call with a *fresh* idempotency key is a replay at the domain layer: the same note
// comes back, no second note is issued, and the balance is unchanged. This is the case the
// wrapper cannot catch — a worker retrying after a restart has no memory of the first key.
func TestStaffDowngradeCreditReplayDoesNotMintASecondNote(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "downgrade-replay@example.test", "20.00")
	upgradeID := env.seedDowngrade(t, tenant.serviceID, 500)
	path := "/api/v1/staff/upgrades/" + upgradeID + "/credit"

	first := env.portalMutation(t, http.MethodPost, path, env.billing, env.billingCSRF,
		freshKey("dc-first"), map[string]any{})
	if first.Code != http.StatusCreated {
		t.Fatalf("first post: status %d; body %s", first.Code, first.Body)
	}
	firstBody := decodeDowngrade(t, first)

	second := env.portalMutation(t, http.MethodPost, path, env.billing, env.billingCSRF,
		freshKey("dc-second"), map[string]any{})
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d, want 200; body %s", second.Code, second.Body)
	}
	secondBody := decodeDowngrade(t, second)
	if !secondBody.AlreadyPosted || secondBody.Number != firstBody.Number {
		t.Fatalf("replay = %+v, want the original note %s already posted", secondBody, firstBody.Number)
	}

	var notes int
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM credit_notes WHERE customer_id = $1`, tenant.customerID).
		Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 1 {
		t.Fatalf("credit notes = %d, want 1", notes)
	}
}

// The route moves money, so it needs credit:write, CSRF and an Idempotency-Key. A role without
// credit:write is refused before the handler runs.
func TestStaffDowngradeCreditGuards(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "downgrade-guards@example.test", "20.00")
	upgradeID := env.seedDowngrade(t, tenant.serviceID, 500)
	path := "/api/v1/staff/upgrades/" + upgradeID + "/credit"

	// Support may read the queue and not post money.
	if rec := env.portalMutation(t, http.MethodPost, path, env.support, env.supportCSRF,
		freshKey("dc-support"), map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("support posting a credit: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// No key means the retry story is untellable.
	if rec := env.portalMutation(t, http.MethodPost, path, env.billing, env.billingCSRF,
		"", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("without an idempotency key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	// No CSRF token is not a permission problem, it is a forgery problem.
	if rec := env.portalMutation(t, http.MethodPost, path, env.billing, "",
		freshKey("dc-csrf"), map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// An unknown id is a 404, and it does not post anything.
	if rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/staff/upgrades/11111111-1111-1111-1111-111111111111/credit",
		env.billing, env.billingCSRF, freshKey("dc-missing"), map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown upgrade: status %d, want 404; body %s", rec.Code, rec.Body)
	}
}

// An *upgrade* row owes nothing — its money is an invoice the customer pays — so asking for a
// credit on it is refused with a stable code rather than a refund.
func TestStaffDowngradeCreditRefusesAnUpgradeRow(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "downgrade-not@example.test", "20.00")
	upgradeID := env.seedDowngrade(t, tenant.serviceID, -700) // positive prorated = a charge
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE service_upgrades SET prorated_minor = 700 WHERE id = $1`, upgradeID); err != nil {
		t.Fatalf("make it an upgrade: %v", err)
	}

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/staff/upgrades/"+upgradeID+"/credit", env.billing, env.billingCSRF,
		freshKey("dc-upgrade"), map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_a_downgrade" {
		t.Fatalf("problem code = %q, want not_a_downgrade", code)
	}
}

func decodeDowngrade(t *testing.T, rec *httptest.ResponseRecorder) downgradeBody {
	t.Helper()
	var body downgradeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("downgrade body: %v\n%s", err, rec.Body)
	}
	return body
}
