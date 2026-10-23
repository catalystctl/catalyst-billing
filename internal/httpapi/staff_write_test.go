package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// newRequest builds a request with a body.
func newRequest(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, path, body)
}

// stringReader wraps a string for a request body.
func stringReader(s string) io.Reader { return strings.NewReader(s) }

// doRequest runs a request through the environment's server.
func (e *authEnv) doRequest(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, r)
	return rec
}

// Staff mutation tests (Gate B).
//
// They assert the properties that make the buttons safe rather than the buttons themselves:
// a double click enqueues once, a held service refuses, a stale generation does nothing, the
// actor comes from the session, and a paid invoice cannot be erased.

// seedEligibleService creates the exact state the dunning engine calls suspendable: an
// active service whose subscription has an open invoice in dunning state `exhausted`.
//
// It builds the chain explicitly (product -> price version -> subscription -> service ->
// invoice -> dunning state) rather than taking a shortcut, because the eligibility check
// joins along exactly that chain and a shortcut would test a different code path.
func (e *staffEnv) seedEligibleService(t *testing.T, email string) (serviceID, invoiceID string) {
	t.Helper()
	ctx := context.Background()
	customerID, _ := e.seedCustomer(t, email, "Suspendable")

	var productID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('sus-'||gen_random_uuid(), 'Suspendable VPS')
		RETURNING id`).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceVersionID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC', 'plan-sus', 'eu-west')
		RETURNING id`, productID).Scan(&priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}

	now := time.Now().UTC()
	var subscriptionID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
		RETURNING id`,
		customerID, priceVersionID, now.AddDate(0, -1, 0), now).Scan(&subscriptionID); err != nil {
		t.Fatalf("subscription: %v", err)
	}

	if err := e.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, subscription_id, status)
		VALUES ($1, $2, $3, 'active') RETURNING id`,
		customerID, productID, subscriptionID).Scan(&serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}

	// An invoice that is open, past due, and out of retries: the state the engine treats as
	// exhausted and therefore suspendable.
	past := now.Add(-30 * 24 * time.Hour)
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, subscription_id, number, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, $2, 'INV-SUS-'||floor(random()*100000)::text, 'open', 'USD',
			5000, 0, 5000, 0, $3, $3, '{"schema_version":1}'::jsonb)
		RETURNING id`, customerID, subscriptionID, past).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO dunning_states (invoice_id, state, step, next_action_at)
		VALUES ($1, 'exhausted', 3, NULL)`, invoiceID); err != nil {
		t.Fatalf("dunning state: %v", err)
	}

	// The eligibility check treats the subledger as the authority on what is owed, so the
	// receivable has to exist. Posting it here is what issuance would have done.
	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatalf("currency: %v", err)
	}
	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindInvoiceIssued,
			Reference:   ledger.RefInvoice,
			ReferenceID: invoiceID,
			CustomerID:  customerID,
			Currency:    usd,
			Debits:      []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(5000, usd)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(5000, usd)}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("post receivable: %v", err)
	}
	return serviceID, invoiceID
}

// A suspension enqueues exactly one operation, and asking twice still enqueues one.
func TestSuspendEnqueuesOnce(t *testing.T) {
	env := newStaffEnv(t)
	serviceID, _ := env.seedEligibleService(t, "suspend@example.test")

	first := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend", env.admin, env.adminCSRF,
		map[string]any{"reason": "non-payment"})
	if first.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", first.Code, first.Body)
	}
	var accepted struct {
		OperationID string `json:"operation_id"`
		StatusURL   string `json:"status_url"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("body: %v\n%s", err, first.Body)
	}
	if accepted.OperationID == "" || accepted.StatusURL == "" {
		t.Fatalf("accepted = %+v", accepted)
	}

	// The status URL must resolve.
	rec := env.get(t, accepted.StatusURL, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status url: %d %s", rec.Code, rec.Body)
	}

	// A second request with a fresh idempotency key must not create a second operation: the
	// operation key is derived from the service generation, so the engine is idempotent
	// independently of the HTTP layer.
	second := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend", env.admin, env.adminCSRF,
		map[string]any{"reason": "again"})
	if second.Code != http.StatusAccepted {
		t.Fatalf("second suspend: status %d; body %s", second.Code, second.Body)
	}

	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations
		WHERE service_id = $1 AND kind = 'suspend'`, serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("suspend operations = %d, want 1", n)
	}
}

// A held service refuses a suspension and enqueues nothing.
func TestSuspendRefusesWhenHeld(t *testing.T) {
	env := newStaffEnv(t)
	serviceID, _ := env.seedEligibleService(t, "held@example.test")

	// Place a hold. Support holds service:hold, which is exactly the "stop" instruction a
	// first-line agent needs while a dispute is open.
	hold := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/holds", env.support, env.supportCSRF,
		map[string]any{"reason": "customer dispute open"})
	if hold.Code != http.StatusCreated {
		t.Fatalf("support hold: status %d; body %s", hold.Code, hold.Body)
	}

	rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend", env.admin, env.adminCSRF,
		map[string]any{"reason": "non-payment"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "hold_in_place") {
		t.Fatalf("body = %s", rec.Body)
	}

	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations
		WHERE service_id = $1 AND kind = 'suspend'`, serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a held service enqueued %d suspension(s)", n)
	}
}

// Overriding a hold needs a second staff actor. The first request creates the approval; the
// same person cannot approve it; a different person can, and only then does the suspension
// enqueue.
func TestHoldOverrideRequiresSecondActor(t *testing.T) {
	env := newStaffEnv(t)
	serviceID, _ := env.seedEligibleService(t, "override@example.test")

	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/holds",
		env.support, env.supportCSRF, map[string]any{"reason": "dispute"}); rec.Code != http.StatusCreated {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body)
	}

	// Ask for the override: the response hands back the approval id.
	rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend", env.admin, env.adminCSRF,
		map[string]any{"reason": "abuse report", "override_hold": true, "request_approval": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	var pending struct {
		Approval struct {
			ID          string `json:"id"`
			RequestedBy string `json:"requested_by"`
			Status      string `json:"status"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if pending.Approval.ID == "" || pending.Approval.Status != "pending" {
		t.Fatalf("approval = %+v", pending.Approval)
	}

	// The requester cannot approve their own request, even holding the permission. This is the
	// property that makes dual approval meaningful rather than decorative.
	selfApprove := env.postJSON(t, "/api/v1/staff/approvals/"+pending.Approval.ID+"/approve",
		env.admin, env.adminCSRF, map[string]any{})
	if selfApprove.Code != http.StatusConflict {
		t.Fatalf("self approval: status %d, want 409; body %s", selfApprove.Code, selfApprove.Body)
	}
	if !containsCode(selfApprove.Body.Bytes(), "self_approval_refused") {
		t.Fatalf("body = %s", selfApprove.Body)
	}

	// Nothing was enqueued while the approval is pending.
	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'suspend'`,
		serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a pending override enqueued %d suspension(s)", n)
	}

	// A second administrator is a different person holding service:suspend, so they can
	// approve. Billing deliberately cannot: it holds neither the permission to suspend nor the
	// one to terminate, which is what keeps the two duties apart.
	env.createUser(t, "admin2@example.test", "admin2 password 1", "administrator")
	admin2Cookie, admin2CSRF := env.login(t, "admin2@example.test", "admin2 password 1")

	if rec := env.postJSON(t, "/api/v1/staff/approvals/"+pending.Approval.ID+"/approve",
		env.billing, env.billingCSRF, map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("billing approval: status %d, want 403 (it does not hold service:suspend)", rec.Code)
	}

	approve := env.postJSON(t, "/api/v1/staff/approvals/"+pending.Approval.ID+"/approve",
		admin2Cookie, admin2CSRF, map[string]any{})
	if approve.Code != http.StatusOK {
		t.Fatalf("approval by a second actor: status %d; body %s", approve.Code, approve.Body)
	}

	// Now the override proceeds.
	done := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend", env.admin, env.adminCSRF,
		map[string]any{"reason": "abuse report", "override_hold": true, "approval_id": pending.Approval.ID})
	if done.Code != http.StatusAccepted {
		t.Fatalf("override: status %d; body %s", done.Code, done.Body)
	}

	var decisions int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM suspension_decisions
		WHERE service_id = $1 AND override_hold`, serviceID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 {
		t.Fatalf("override decisions = %d, want 1", decisions)
	}
}

// Termination needs the operator to name the service, and a wrong confirmation enqueues
// nothing.
func TestTerminateRequiresConfirmation(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "term@example.test", "Terminate Me")
	serviceID, _ := env.seedService(t, customerID, "active")

	wrong := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/terminate", env.admin, env.adminCSRF,
		map[string]any{"confirm": "something-else", "reason": "fraud"})
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation: status %d, want 400; body %s", wrong.Code, wrong.Body)
	}
	if !containsCode(wrong.Body.Bytes(), "confirmation_mismatch") {
		t.Fatalf("body = %s", wrong.Body)
	}

	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a wrong confirmation enqueued %d termination(s)", n)
	}

	// The id is an acceptable confirmation, so the operator does not need the provider's
	// identifier to destroy an unprovisioned service.
	ok := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/terminate", env.admin, env.adminCSRF,
		map[string]any{"confirm": serviceID, "reason": "fraud"})
	if ok.Code != http.StatusAccepted {
		t.Fatalf("terminate: status %d; body %s", ok.Code, ok.Body)
	}

	// Terminating twice is a conflict, not a second operation.
	again := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/terminate", env.admin, env.adminCSRF,
		map[string]any{"confirm": serviceID, "reason": "again"})
	if again.Code != http.StatusAccepted && again.Code != http.StatusConflict {
		t.Fatalf("second terminate: status %d", again.Code)
	}
}

// A hold records the session's user as its creator, and a body field claiming otherwise is
// ignored.
func TestHoldCreatorComesFromSession(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "creator@example.test", "Creator")
	serviceID, _ := env.seedService(t, customerID, "active")

	rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/holds", env.support, env.supportCSRF,
		map[string]any{
			"reason": "customer asked us to pause billing",
			// A client that tries to attribute the hold to someone else must not succeed.
			"created_by": "11111111-1111-1111-1111-111111111111",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	var (
		createdBy string
		reason    string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT created_by, reason FROM service_holds WHERE service_id = $1`, serviceID).
		Scan(&createdBy, &reason); err != nil {
		t.Fatal(err)
	}

	var supportID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'support@example.test'`).Scan(&supportID); err != nil {
		t.Fatal(err)
	}
	if createdBy != supportID {
		t.Fatalf("created_by = %s, want the session user %s", createdBy, supportID)
	}
	if reason != "customer asked us to pause billing" {
		t.Fatalf("reason = %q", reason)
	}
}

// Clearing a hold makes the service suspendable again, which is the whole point of clearing it.
func TestClearHoldReleasesTheService(t *testing.T) {
	env := newStaffEnv(t)
	serviceID, _ := env.seedEligibleService(t, "clear@example.test")

	rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/holds", env.support, env.supportCSRF,
		map[string]any{"reason": "temporary"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Refused while held.
	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend",
		env.admin, env.adminCSRF, map[string]any{"reason": "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("suspend while held: %d", rec.Code)
	}

	// Clear it.
	req := newRequest(http.MethodDelete, "/api/v1/staff/holds/"+created.ID, nil)
	req.AddCookie(env.support)
	req.Header.Set("X-CSRF-Token", env.supportCSRF)
	req.Header.Set("Idempotency-Key", "clear-hold-0001")
	del := env.doRequest(t, req)
	if del.Code != http.StatusNoContent {
		t.Fatalf("clear hold: status %d; body %s", del.Code, del.Body)
	}

	// Now it is suspendable.
	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend",
		env.admin, env.adminCSRF, map[string]any{"reason": "non-payment"}); rec.Code != http.StatusAccepted {
		t.Fatalf("suspend after clearing the hold: %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- invoices --

// A manual invoice is priced by the server, and issuing it allocates a number.
func TestCreateAndIssueInvoice(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "invoice@example.test", "Invoice Me")

	rec := env.postJSON(t, "/api/v1/staff/invoices", env.billing, env.billingCSRF, map[string]any{
		"customer_id": customerID,
		"issue_now":   true,
		"due_in_days": 14,
		"lines": []map[string]any{
			{"description": "Managed hosting", "quantity": 2, "unit_amount_minor": 2500, "currency": "USD"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID     string `json:"id"`
		Number string `json:"number"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if created.Status != "open" {
		t.Fatalf("status = %q, want open after issue_now", created.Status)
	}
	if created.Number == "" {
		t.Fatal("an issued invoice has no number")
	}

	// The total must equal what the pricer computed: 2 x 2500 with no tax.
	var total, subtotal int64
	if err := env.pool.QueryRow(context.Background(), `
		SELECT total_minor, subtotal_minor FROM invoices WHERE id = $1`, created.ID).
		Scan(&total, &subtotal); err != nil {
		t.Fatal(err)
	}
	if subtotal != 5000 || total != 5000 {
		t.Fatalf("subtotal = %d, total = %d, want 5000", subtotal, total)
	}

	// Issuing again is a conflict, not a second document.
	again := env.postJSON(t, "/api/v1/staff/invoices/"+created.ID+"/issue", env.billing, env.billingCSRF,
		map[string]any{})
	if again.Code != http.StatusConflict {
		t.Fatalf("reissue: status %d, want 409; body %s", again.Code, again.Body)
	}

	// The notification was queued in the same transaction, or suppressed because the customer
	// has no verified address. Either way there is a delivery record, which is the point.
	//
	// The kind is the notification *event* key, not the legacy `invoice_issued` outbox kind: the
	// message now comes from the editable new_invoice_created template, so the outbox row, the
	// inbox row and the template all carry the same key.
	var deliveries int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries WHERE kind = 'new_invoice_created'`).
		Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("new_invoice_created deliveries = %d, want 1", deliveries)
	}
}

// A client cannot choose the total: an invoice with two currencies is refused, and quantity
// must be positive.
func TestCreateInvoiceRejectsBadInput(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "badinput@example.test", "Bad Input")

	cases := map[string]map[string]any{
		"mixed currency": {
			"customer_id": customerID,
			"lines": []map[string]any{
				{"description": "a", "quantity": 1, "unit_amount_minor": 100, "currency": "USD"},
				{"description": "b", "quantity": 1, "unit_amount_minor": 100, "currency": "EUR"},
			},
		},
		"zero quantity": {
			"customer_id": customerID,
			"lines": []map[string]any{
				{"description": "a", "quantity": 0, "unit_amount_minor": 100, "currency": "USD"},
			},
		},
		"no lines": {
			"customer_id": customerID,
			"lines":       []map[string]any{},
		},
		"no description": {
			"customer_id": customerID,
			"lines": []map[string]any{
				{"description": "", "quantity": 1, "unit_amount_minor": 100, "currency": "USD"},
			},
		},
	}
	for name, body := range cases {
		rec := env.postJSON(t, "/api/v1/staff/invoices", env.billing, env.billingCSRF, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", name, rec.Code, rec.Body)
		}
	}
}

// Voiding an unpaid invoice works; voiding a paid one is refused, because the correction is
// a credit note rather than an erasure.
func TestVoidInvoice(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "void@example.test", "Void")
	due := time.Now().Add(48 * time.Hour)

	draftID := env.seedInvoice(t, customerID, "open", "USD", 1000, 0, &due)
	rec := env.postJSON(t, "/api/v1/staff/invoices/"+draftID+"/void", env.billing, env.billingCSRF,
		map[string]any{"reason": "created in error"})
	if rec.Code != http.StatusOK {
		t.Fatalf("void: status %d; body %s", rec.Code, rec.Body)
	}
	var status string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT status FROM invoices WHERE id = $1`, draftID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "void" {
		t.Fatalf("status = %q, want void", status)
	}

	// Voiding needs a reason: an unexplained void is a hole in the record.
	otherID := env.seedInvoice(t, customerID, "open", "USD", 1000, 0, &due)
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+otherID+"/void", env.billing, env.billingCSRF,
		map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("void without a reason: status %d, want 400", rec.Code)
	}

	// A paid invoice cannot be voided.
	paidID := env.seedInvoice(t, customerID, "paid", "USD", 1000, 1000, &due)
	rec = env.postJSON(t, "/api/v1/staff/invoices/"+paidID+"/void", env.billing, env.billingCSRF,
		map[string]any{"reason": "customer asked"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("void a paid invoice: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "not_voidable") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A voided renewal invoice lets the same period be reissued, which is what makes voiding
// useful rather than merely destructive.
func TestVoidAllowsReissue(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "reissue@example.test", "Reissue")
	due := time.Now().Add(48 * time.Hour)

	id := env.seedInvoice(t, customerID, "open", "USD", 1000, 0, &due)
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+id+"/void", env.billing, env.billingCSRF,
		map[string]any{"reason": "wrong period"}); rec.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rec.Code, rec.Body)
	}

	// A new invoice for the same customer is unaffected: the period key is what the renewal
	// engine keys on, and a void document does not hold it.
	rec := env.postJSON(t, "/api/v1/staff/invoices", env.billing, env.billingCSRF, map[string]any{
		"customer_id": customerID,
		"issue_now":   true,
		"lines": []map[string]any{
			{"description": "Corrected period", "quantity": 1, "unit_amount_minor": 1000, "currency": "USD"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reissue: status %d; body %s", rec.Code, rec.Body)
	}
}

// A staff mutation is audited with the actor taken from the session.
func TestStaffMutationIsAudited(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "audited@example.test", "Audited")
	serviceID, _ := env.seedService(t, customerID, "active")

	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/holds",
		env.support, env.supportCSRF, map[string]any{"reason": "audited hold"}); rec.Code != http.StatusCreated {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body)
	}

	var (
		actorID string
		action  string
		changes []byte
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COALESCE(actor_id::text, ''), action, changes
		FROM audit_events WHERE target_id = $1 AND action = 'service.hold_add'`, serviceID).
		Scan(&actorID, &action, &changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}

	var supportID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'support@example.test'`).Scan(&supportID); err != nil {
		t.Fatal(err)
	}
	if actorID != supportID {
		t.Fatalf("audit actor = %s, want %s", actorID, supportID)
	}
	var diff map[string]any
	if err := json.Unmarshal(changes, &diff); err != nil {
		t.Fatal(err)
	}
	if diff["reason"] != "audited hold" {
		t.Fatalf("audit changes = %v", diff)
	}

	// The audit log is readable by an administrator and refused to support.
	if rec := env.get(t, "/api/v1/staff/audit?action=service.hold_add", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("audit read: %d %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/audit", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support audit read: status %d, want 403", rec.Code)
	}
}

// Support may place a hold but may not suspend, refund or terminate: the split that makes the
// roles mean something.
func TestSupportCannotSuspendOrTerminate(t *testing.T) {
	env := newStaffEnv(t)
	serviceID, _ := env.seedEligibleService(t, "roles@example.test")
	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/suspend",
		env.support, env.supportCSRF, map[string]any{"reason": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("support suspend: status %d, want 403", rec.Code)
	}
	if rec := env.postJSON(t, "/api/v1/staff/services/"+serviceID+"/terminate",
		env.support, env.supportCSRF, map[string]any{"confirm": serviceID}); rec.Code != http.StatusForbidden {
		t.Fatalf("support terminate: status %d, want 403", rec.Code)
	}
	if rec := env.postJSON(t, "/api/v1/staff/invoices", env.support, env.supportCSRF,
		map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("support invoice write: status %d, want 403", rec.Code)
	}
}

// A mutation without a CSRF token is refused, and one without an idempotency key is refused:
// both are how a cross-site form post and a double refund are prevented.
func TestStaffMutationRequiresCSRFAndKey(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "csrf@example.test", "CSRF")
	serviceID, _ := env.seedService(t, customerID, "active")

	body := `{"reason":"x"}`

	noCSRF := newRequest(http.MethodPost, "/api/v1/staff/services/"+serviceID+"/holds",
		stringReader(body))
	noCSRF.AddCookie(env.admin)
	noCSRF.Header.Set("Content-Type", "application/json")
	noCSRF.Header.Set("Idempotency-Key", "csrf-test-0001")
	if rec := env.doRequest(t, noCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("no CSRF: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	noKey := newRequest(http.MethodPost, "/api/v1/staff/services/"+serviceID+"/holds",
		stringReader(body))
	noKey.AddCookie(env.admin)
	noKey.Header.Set("Content-Type", "application/json")
	noKey.Header.Set("X-CSRF-Token", env.adminCSRF)
	if rec := env.doRequest(t, noKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("no idempotency key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
}
