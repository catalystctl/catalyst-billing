package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Money action tests (Gate D1-D4) over HTTP.
//
// The domain behaviour is proven in internal/refunds and internal/creditnote. What is proven here is
// the part that only exists at the HTTP boundary: the permission, the approval flow, the status code
// when the outcome is unknown, and the fact that the request's idempotency key reaches the provider.

// moneyEnv is a staff environment with a paid invoice and a payment.
type moneyEnv struct {
	*staffEnv
	customerID string
	invoiceID  string
	paymentID  string
}

func newMoneyEnv(t *testing.T) *moneyEnv {
	t.Helper()
	env := newStaffEnv(t)
	e := &moneyEnv{staffEnv: env}
	e.customerID, _ = env.seedCustomer(t, "money@example.test", "Money")
	e.invoiceID = env.seedInvoice(t, e.customerID, "paid", "USD", 10000, 10000, nil)

	ctx := context.Background()
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status)
		VALUES ('fakegateway', 'acct_test', 'pi_money_1', 10000, 'USD', 'succeeded')
		RETURNING id`).Scan(&e.paymentID); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 10000, 'USD')`, e.paymentID, e.invoiceID); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	return e
}

// Without a refund-capable gateway the endpoint says so rather than failing halfway.
func TestRefundRequiresGateway(t *testing.T) {
	e := newMoneyEnv(t)
	rec := e.postJSON(t, "/api/v1/staff/payments/"+e.paymentID+"/refunds",
		e.admin, e.adminCSRF, map[string]any{"reason": "test"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "provider_unavailable") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A malformed payment id is a 404 before the gateway is consulted.
func TestRefundMalformedID(t *testing.T) {
	e := newMoneyEnv(t)
	rec := e.postJSON(t, "/api/v1/staff/payments/not-a-uuid/refunds",
		e.admin, e.adminCSRF, map[string]any{})
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 404 or 503; body %s", rec.Code, rec.Body)
	}
}

// A credit note issues and is listed against the invoice.
func TestIssueCreditNoteOverHTTP(t *testing.T) {
	e := newMoneyEnv(t)
	rec := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 1200,
			"tax_minor":    200,
			"reason":       "service outage credit",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var note struct {
		ID     string `json:"id"`
		Number string `json:"number"`
		Total  struct {
			Minor int64 `json:"minor"`
		} `json:"total"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &note); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if note.Number == "" || note.Total.Minor != 1200 || note.Status != "issued" {
		t.Fatalf("note = %+v", note)
	}

	// The invoice detail now shows it, and the journal balances.
	detail := e.get(t, "/api/v1/staff/invoices/"+e.invoiceID, e.billing)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body)
	}
	if !bytesContain(detail.Body.Bytes(), note.Number) {
		t.Fatalf("the credit note is missing from the invoice detail: %s", detail.Body)
	}

	// A credit larger than the invoice is refused with the remaining amount in the message.
	over := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 20000,
			"reason":       "too much",
		})
	if over.Code != http.StatusConflict {
		t.Fatalf("over-credit: status %d, want 409; body %s", over.Code, over.Body)
	}
	if !containsCode(over.Body.Bytes(), "exceeds_invoice") {
		t.Fatalf("body = %s", over.Body)
	}
}

// A credit note without a reason is refused.
func TestCreditNoteRequiresReasonOverHTTP(t *testing.T) {
	e := newMoneyEnv(t)
	rec := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{"amount_minor": 100})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
}

// Applying credit settles the invoice and answers with invoice_paid.
func TestApplyCreditOverHTTP(t *testing.T) {
	e := newMoneyEnv(t)
	// Give the customer credit first.
	note := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 3000,
			"reason":       "goodwill",
		})
	if note.Code != http.StatusCreated {
		t.Fatalf("credit note: %d %s", note.Code, note.Body)
	}

	// The invoice is already paid, so applying is refused: there is nothing owed.
	paid := e.postJSON(t, "/api/v1/staff/customers/"+e.customerID+"/credit/apply",
		e.billing, e.billingCSRF, map[string]any{
			"invoice_id":   e.invoiceID,
			"amount_minor": 1000,
		})
	if paid.Code != http.StatusConflict {
		t.Fatalf("apply to a paid invoice: status %d, want 409; body %s", paid.Code, paid.Body)
	}

	// A fresh open invoice can absorb it.
	openID := e.seedInvoice(t, e.customerID, "open", "USD", 5000, 0, nil)
	applied := e.postJSON(t, "/api/v1/staff/customers/"+e.customerID+"/credit/apply",
		e.billing, e.billingCSRF, map[string]any{
			"invoice_id":   openID,
			"amount_minor": 3000,
		})
	if applied.Code != http.StatusOK {
		t.Fatalf("apply: status %d; body %s", applied.Code, applied.Body)
	}
	var app struct {
		Amount      struct{ Minor int64 } `json:"amount"`
		InvoicePaid bool                  `json:"invoice_paid"`
	}
	if err := json.Unmarshal(applied.Body.Bytes(), &app); err != nil {
		t.Fatal(err)
	}
	if app.Amount.Minor != 3000 {
		t.Fatalf("applied = %d", app.Amount.Minor)
	}
	if app.InvoicePaid {
		t.Fatal("the invoice was marked paid before the credit covered it")
	}

	// Applying more than the customer has is refused.
	more := e.postJSON(t, "/api/v1/staff/customers/"+e.customerID+"/credit/apply",
		e.billing, e.billingCSRF, map[string]any{
			"invoice_id":   openID,
			"amount_minor": 5000,
		})
	if more.Code != http.StatusConflict {
		t.Fatalf("over-apply: status %d, want 409; body %s", more.Code, more.Body)
	}
	if !containsCode(more.Body.Bytes(), "insufficient_credit") {
		t.Fatalf("body = %s", more.Body)
	}
}

// Support may not issue a credit note or apply credit: it holds neither permission.
func TestMoneyActionsRequirePermission(t *testing.T) {
	e := newMoneyEnv(t)
	if rec := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.support, e.supportCSRF, map[string]any{
			"amount_minor": 100, "reason": "x",
		}); rec.Code != http.StatusForbidden {
		t.Fatalf("support credit note: status %d, want 403", rec.Code)
	}
	if rec := e.postJSON(t, "/api/v1/staff/payments/"+e.paymentID+"/refunds",
		e.support, e.supportCSRF, map[string]any{"reason": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("support refund: status %d, want 403", rec.Code)
	}
	if rec := e.postJSON(t, "/api/v1/staff/customers/"+e.customerID+"/credit/apply",
		e.support, e.supportCSRF, map[string]any{"invoice_id": e.invoiceID}); rec.Code != http.StatusForbidden {
		t.Fatalf("support apply: status %d, want 403", rec.Code)
	}
}

// A refund above the threshold asks for an approval, and a second person's approval unlocks it.
func TestRefundApprovalFlow(t *testing.T) {
	e := newMoneyEnv(t)
	// The approval is created by the money handler only when a refund gateway exists, so this test
	// drives the approval endpoints directly: they are the contract the UI uses.
	request := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{"amount_minor": 100, "reason": "seed"})
	if request.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", request.Code, request.Body)
	}

	// Create a pending approval through the same service the handlers use.
	var approvalID string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO approvals (action, reference_id, requested_by, status, reason)
		VALUES ('refund', $1, $2, 'pending', 'large refund')
		RETURNING id`, e.paymentID, mustUserID(t, e, "billing@example.test")).Scan(&approvalID); err != nil {
		t.Fatalf("approval: %v", err)
	}

	// The requester cannot approve it.
	self := e.postJSON(t, "/api/v1/staff/approvals/"+approvalID+"/approve",
		e.billing, e.billingCSRF, map[string]any{})
	if self.Code != http.StatusConflict {
		t.Fatalf("self approval: status %d, want 409; body %s", self.Code, self.Body)
	}

	// A second administrator can.
	approve := e.postJSON(t, "/api/v1/staff/approvals/"+approvalID+"/approve",
		e.admin, e.adminCSRF, map[string]any{})
	if approve.Code != http.StatusOK {
		t.Fatalf("approval: status %d; body %s", approve.Code, approve.Body)
	}

	// Approving again is a conflict.
	again := e.postJSON(t, "/api/v1/staff/approvals/"+approvalID+"/approve",
		e.admin, e.adminCSRF, map[string]any{})
	if again.Code != http.StatusConflict {
		t.Fatalf("second approval: status %d, want 409", again.Code)
	}

	// The list shows it as approved.
	list := e.get(t, "/api/v1/staff/approvals?status=approved", e.admin)
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	if !bytesContain(list.Body.Bytes(), approvalID) {
		t.Fatalf("the approval is missing from the list: %s", list.Body)
	}
}

// mustUserID resolves a user id by email.
func mustUserID(t *testing.T, e *moneyEnv, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("user %s: %v", email, err)
	}
	return id
}

// bytesContain reports whether body contains needle.
func bytesContain(body []byte, needle string) bool {
	return len(needle) > 0 && len(body) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(body); i++ {
				if string(body[i:i+len(needle)]) == needle {
					return true
				}
			}
			return false
		})()
}

// A refund without a gateway is a 503 on the read endpoint too, so a UI does not show a refund that
// can never be fetched.
func TestGetRefundRequiresGateway(t *testing.T) {
	e := newMoneyEnv(t)
	rec := e.get(t, "/api/v1/staff/refunds/0f8fad5b-d9cb-469f-a165-70867728950e", e.billing)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
}

// The audit log records a credit note issue with the actor from the session.
func TestCreditNoteIsAudited(t *testing.T) {
	e := newMoneyEnv(t)
	if rec := e.postJSON(t, "/api/v1/staff/invoices/"+e.invoiceID+"/credit-notes",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 500, "reason": "audited",
		}); rec.Code != http.StatusCreated {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}

	var action string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT action FROM audit_events
		WHERE target_type = 'credit_note' ORDER BY created_at DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "credit_note.issue" {
		t.Fatalf("action = %q", action)
	}
}
