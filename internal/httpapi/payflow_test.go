package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/portal"
)

// The customer pay flow over HTTP (PAY-17, PAY-18, PAY-19, PAY-13, PAY-14).
//
// internal/credit proves the posting rules and internal/billing proves the allocation. What
// is proven here is the boundary: that a customer can only spend credit on their own
// invoice, that a poll reports settlement from the allocation rather than from the attempt,
// that the pay dialog is offered the gateway the deployment actually has, and that an
// operator's manual payment goes through the same allocation code as a webhook.

// portalFor builds the portal manager for an environment.
func portalFor(env *authEnv) *portal.Manager { return portal.New(env.pool) }

// seedCredit puts credit on a customer's account through the domain path.
func (e *authEnv) seedCredit(t *testing.T, customerID string, amount int64, key string) {
	t.Helper()
	if _, err := credit.New(e.pool).Grant(context.Background(), credit.GrantRequest{
		CustomerID: customerID, AmountMinor: amount, Currency: "USD",
		Reason: "seed", Key: key,
	}); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
}

// seedAttempt inserts a checkout attempt for an invoice, the way checkout.Service would.
func (e *authEnv) seedAttempt(t *testing.T, invoiceID, providerRef, status string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO payment_attempts (invoice_id, gateway, operation_key, status, amount_minor,
			currency, provider_ref, redirect_url)
		VALUES ($1, 'fakegateway', $2, $3, 1200, 'USD', $4, 'https://pay.example.test/x')
		RETURNING id`,
		invoiceID, "checkout:"+invoiceID, status, nullableTestString(providerRef)).Scan(&id); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

func nullableTestString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// seedSettledAllocation records a payment and its allocation for an invoice.
func (e *authEnv) seedSettledAllocation(t *testing.T, invoiceID, providerRef string, amount int64) {
	t.Helper()
	ctx := context.Background()
	var paymentID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status)
		VALUES ('fakegateway', 'acct_test', $1, $2, 'USD', 'succeeded')
		RETURNING id`, providerRef, amount).Scan(&paymentID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, $3, 'USD')`, paymentID, invoiceID, amount); err != nil {
		t.Fatalf("seed allocation: %v", err)
	}
}

// Paying with credit settles the invoice and answers with the new remaining balance.
func TestPayWithCreditOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	tenant := env.createTenant(t, "paycredit@example.test", "12.00")
	cookie, csrf := env.login(t, "paycredit@example.test", "a long enough password")

	// Enough credit to settle the invoice exactly.
	env.seedCredit(t, tenant.customerID, 1200, "pay-with-credit-1")

	rec := env.postJSON(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/pay-with-credit",
		cookie, csrf, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		ApplicationID  string                `json:"application_id"`
		Amount         struct{ Minor int64 } `json:"amount"`
		Remaining      struct{ Minor int64 } `json:"remaining"`
		InvoicePaid    bool                  `json:"invoice_paid"`
		AlreadyApplied bool                  `json:"already_applied"`
		CreditBalance  []struct {
			Minor int64 `json:"minor"`
		} `json:"credit_balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Amount.Minor != 1200 || !body.InvoicePaid || body.Remaining.Minor != 0 {
		t.Fatalf("body = %+v", body)
	}
	if len(body.CreditBalance) != 0 {
		t.Fatalf("credit_balance = %+v, want empty", body.CreditBalance)
	}
	if body.ApplicationID == "" {
		t.Fatal("no application id")
	}

	// A second call with no key derives the same identity, so it spends nothing more and is
	// reported as already applied rather than failing.
	replay := env.postJSON(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/pay-with-credit",
		cookie, csrf, map[string]any{})
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: status %d; body %s", replay.Code, replay.Body)
	}
	var replayBody struct {
		AlreadyApplied bool                  `json:"already_applied"`
		Amount         struct{ Minor int64 } `json:"amount"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayBody); err != nil {
		t.Fatal(err)
	}
	if !replayBody.AlreadyApplied || replayBody.Amount.Minor != 1200 {
		t.Fatalf("replay = %+v", replayBody)
	}
}

// A partial balance leaves the right remainder and does not mark the invoice paid.
func TestPayWithCreditPartialOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	tenant := env.createTenant(t, "partial@example.test", "12.00")
	cookie, csrf := env.login(t, "partial@example.test", "a long enough password")

	env.seedCredit(t, tenant.customerID, 500, "partial-1")

	rec := env.postJSON(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/pay-with-credit",
		cookie, csrf, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Amount      struct{ Minor int64 } `json:"amount"`
		Remaining   struct{ Minor int64 } `json:"remaining"`
		InvoicePaid bool                  `json:"invoice_paid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Amount.Minor != 500 || body.Remaining.Minor != 700 || body.InvoicePaid {
		t.Fatalf("body = %+v", body)
	}

	// The portal's own view of the invoice agrees.
	inv := env.get(t, "/api/v1/portal/invoices/"+tenant.invoiceID, cookie)
	if inv.Code != http.StatusOK {
		t.Fatalf("invoice: %d %s", inv.Code, inv.Body)
	}
	var invBody struct {
		Due  struct{ Minor int64 } `json:"due"`
		Paid struct{ Minor int64 } `json:"paid"`
	}
	if err := json.Unmarshal(inv.Body.Bytes(), &invBody); err != nil {
		t.Fatalf("invoice body: %v\n%s", err, inv.Body)
	}
	if invBody.Due.Minor != 700 || invBody.Paid.Minor != 500 {
		t.Fatalf("the invoice shows due %d / paid %d, want 700/500", invBody.Due.Minor, invBody.Paid.Minor)
	}
}

// A customer cannot spend credit on another customer's invoice, and the refusal is
// indistinguishable from a nonexistent invoice.
func TestPayWithCreditRejectsAnotherCustomersInvoice(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))

	alice := env.createTenant(t, "alice-pay@example.test", "12.00")
	bob := env.createTenant(t, "bob-pay@example.test", "25.00")
	cookie, csrf := env.login(t, "alice-pay@example.test", "a long enough password")

	env.seedCredit(t, alice.customerID, 5000, "cross-tenant-http")

	foreign := env.postJSON(t, "/api/v1/portal/invoices/"+bob.invoiceID+"/pay-with-credit",
		cookie, csrf, map[string]any{})
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("alice paying bob's invoice: status %d, want 404; body %s", foreign.Code, foreign.Body)
	}
	missing := env.postJSON(t, "/api/v1/portal/invoices/00000000-0000-0000-0000-000000000000/pay-with-credit",
		cookie, csrf, map[string]any{})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing invoice: status %d, want 404", missing.Code)
	}
	if foreign.Body.String() != missing.Body.String() {
		t.Fatalf("a foreign invoice is distinguishable from a missing one:\n%s\nvs\n%s",
			foreign.Body, missing.Body)
	}

	// Bob's invoice is untouched and Alice's balance is intact.
	var paid int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, bob.invoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 0 {
		t.Fatalf("bob's invoice paid = %d, want 0", paid)
	}
	credits := env.get(t, "/api/v1/portal/credits", cookie)
	if credits.Code != http.StatusOK {
		t.Fatalf("credits: %d %s", credits.Code, credits.Body)
	}
	if !bytesContain(credits.Body.Bytes(), `"minor":5000`) {
		t.Fatalf("alice's credit moved: %s", credits.Body)
	}
}

// Paying with no credit is a 409 with a stable code, not a silent success.
func TestPayWithCreditWithNoBalance(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	tenant := env.createTenant(t, "nobalance@example.test", "12.00")
	cookie, csrf := env.login(t, "nobalance@example.test", "a long enough password")

	rec := env.postJSON(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/pay-with-credit",
		cookie, csrf, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "nothing_to_apply") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// The status endpoint reports an unsettled attempt as unsettled, then settled once the
// allocation the webhook would write exists.
func TestPaymentStatusPollsLocalState(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	tenant := env.createTenant(t, "poll@example.test", "12.00")
	cookie, _ := env.login(t, "poll@example.test", "a long enough password")

	// No attempt yet: the invoice is simply unpaid.
	rec := env.get(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/payment-status", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Status    string                `json:"status"`
		Settled   bool                  `json:"settled"`
		Remaining struct{ Minor int64 } `json:"remaining"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "none" || body.Settled || body.Remaining.Minor != 1200 {
		t.Fatalf("before an attempt: %+v", body)
	}

	// A pending attempt is still not settlement: a redirect is not proof of payment.
	env.seedAttempt(t, tenant.invoiceID, "pi_poll_1", "pending")
	rec = env.get(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/payment-status", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "pending" || body.Settled {
		t.Fatalf("pending attempt: %+v", body)
	}

	// The allocation the webhook writes is what makes it settled.
	env.seedSettledAllocation(t, tenant.invoiceID, "pi_poll_1", 1200)
	rec = env.get(t, "/api/v1/portal/invoices/"+tenant.invoiceID+"/payment-status", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Settled {
		t.Fatalf("after the allocation: %+v", body)
	}

	// Another customer's invoice is a 404.
	other := env.createTenant(t, "poll-other@example.test", "3.00")
	if rec := env.get(t, "/api/v1/portal/invoices/"+other.invoiceID+"/payment-status", cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign invoice: status %d, want 404", rec.Code)
	}
}

// The pay dialog is offered exactly the gateway the installation has.
func TestPaymentMethodsReportsConfiguredGateway(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env)).WithGateway(httpGateway{name: "fakegateway"})
	env.createTenant(t, "methods@example.test", "4.00")
	cookie, _ := env.login(t, "methods@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/payment-methods", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		PaymentMethods []struct {
			ID        string `json:"id"`
			Provider  string `json:"provider"`
			Name      string `json:"name"`
			BrandHint string `json:"brand_hint"`
			Enabled   bool   `json:"enabled"`
		} `json:"payment_methods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(body.PaymentMethods) != 1 {
		t.Fatalf("payment_methods = %+v, want exactly the configured gateway", body.PaymentMethods)
	}
	if body.PaymentMethods[0].Provider != "fakegateway" || !body.PaymentMethods[0].Enabled {
		t.Fatalf("gateway = %+v", body.PaymentMethods[0])
	}
	if body.PaymentMethods[0].Name == "" || body.PaymentMethods[0].BrandHint == "" {
		t.Fatalf("gateway has no display metadata: %+v", body.PaymentMethods[0])
	}
}

// Without a gateway the list is empty rather than an error: the pay dialog then offers only
// "pay with credit", which is the truth about the deployment.
func TestPaymentMethodsWithoutGateway(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	env.createTenant(t, "methods-bare@example.test", "4.00")
	cookie, _ := env.login(t, "methods-bare@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/payment-methods", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if !bytesContain(rec.Body.Bytes(), `"payment_methods":[]`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

// An operator records a manual payment; it allocates, posts and settles the invoice.
func TestStaffRecordManualPayment(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "manual@example.test", "Manual Pay")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)

	rec := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id":   invoiceID,
		"amount_minor": 4000,
		"currency":     "USD",
		"reference":    "BANK-REF-0001",
		"fee_minor":    120,
		"reason":       "bank transfer received",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var body struct {
		PaymentID string                `json:"payment_id"`
		Provider  string                `json:"provider"`
		Reference string                `json:"reference"`
		Amount    struct{ Minor int64 } `json:"amount"`
		Fee       struct{ Minor int64 } `json:"fee"`
		Remaining struct{ Minor int64 } `json:"remaining"`
		Paid      bool                  `json:"invoice_paid"`
		Already   bool                  `json:"already_recorded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Provider != "manual" || body.Reference != "BANK-REF-0001" {
		t.Fatalf("body = %+v", body)
	}
	if body.Amount.Minor != 4000 || body.Fee.Minor != 120 || body.Remaining.Minor != 6000 {
		t.Fatalf("body = %+v", body)
	}
	if body.Paid {
		t.Fatal("a partial payment marked the invoice paid")
	}

	// The fee is stored on the payment (PAY-13).
	var fee int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT fee_minor FROM payments WHERE id = $1`, body.PaymentID).Scan(&fee); err != nil {
		t.Fatal(err)
	}
	if fee != 120 {
		t.Fatalf("fee_minor = %d, want 120", fee)
	}

	// The allocation exists, so the ledger moved through billing.ApplyPayment.
	var allocated int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT amount_minor FROM payment_allocations WHERE payment_id = $1`,
		body.PaymentID).Scan(&allocated); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if allocated != 4000 {
		t.Fatalf("allocated = %d, want 4000", allocated)
	}
	var paidMinor int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, invoiceID).Scan(&paidMinor); err != nil {
		t.Fatal(err)
	}
	if paidMinor != 4000 {
		t.Fatalf("invoice paid = %d, want 4000", paidMinor)
	}

	// A replay with the same reference finds the payment instead of recording it twice.
	replay := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id":   invoiceID,
		"amount_minor": 4000,
		"currency":     "USD",
		"reference":    "BANK-REF-0001",
		"reason":       "bank transfer received",
	})
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay: status %d; body %s", replay.Code, replay.Body)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Already {
		t.Fatalf("the replay was not reported as already recorded: %+v", body)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, invoiceID).Scan(&paidMinor); err != nil {
		t.Fatal(err)
	}
	if paidMinor != 4000 {
		t.Fatalf("invoice paid after a replay = %d, want 4000", paidMinor)
	}
}

// Recording a payment needs payment:reconcile, a reference and a reason.
func TestStaffRecordManualPaymentGuards(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "manual-guard@example.test", "Guard")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 5000, 0, nil)

	body := map[string]any{
		"invoice_id": invoiceID, "amount_minor": 1000,
		"reference": "BANK-2", "reason": "cheque",
	}

	// Support can read invoices but may not assert that money arrived.
	forbidden := e.postJSON(t, "/api/v1/staff/payments", e.support, e.supportCSRF, body)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403; body %s", forbidden.Code, forbidden.Body)
	}

	noRef := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id": invoiceID, "amount_minor": 1000, "reason": "cheque",
	})
	if noRef.Code != http.StatusBadRequest {
		t.Fatalf("no reference: status %d, want 400; body %s", noRef.Code, noRef.Body)
	}
	if !containsCode(noRef.Body.Bytes(), "reference_required") {
		t.Fatalf("body = %s", noRef.Body)
	}

	noReason := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id": invoiceID, "amount_minor": 1000, "reference": "BANK-3",
	})
	if noReason.Code != http.StatusBadRequest {
		t.Fatalf("no reason: status %d, want 400; body %s", noReason.Code, noReason.Body)
	}

	// An overpayment is refused: a manual payment is not a way to mint credit.
	over := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id": invoiceID, "amount_minor": 9000, "reference": "BANK-4", "reason": "too much",
	})
	if over.Code != http.StatusConflict {
		t.Fatalf("overpayment: status %d, want 409; body %s", over.Code, over.Body)
	}
	if !containsCode(over.Body.Bytes(), "overpayment") {
		t.Fatalf("body = %s", over.Body)
	}
}

// A manual payment recorded against a deposit invoice credits the customer, because the
// handler allocates through the same routing the webhook uses. Without it, an operator
// recording a bank transfer for a top-up would leave the deposit paid and the balance empty.
func TestStaffRecordManualPaymentAgainstDeposit(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "manual-deposit@example.test", "Deposit")

	// Build the deposit the way a customer's top-up does: no order, and the snapshot key
	// that marks it.
	var invoiceID string
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor,
			tax_minor, total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-DEP-1', 'open', 'USD', 3000, 0, 3000, 0, now(),
		        now() + interval '7 days',
		        '{"schema_version":1,"catalyst_deposit":"credit"}'::jsonb)
		RETURNING id`, customerID).Scan(&invoiceID); err != nil {
		t.Fatal(err)
	}

	rec := e.postJSON(t, "/api/v1/staff/payments", e.billing, e.billingCSRF, map[string]any{
		"invoice_id":   invoiceID,
		"amount_minor": 3000,
		"currency":     "USD",
		"reference":    "BANK-DEP-1",
		"reason":       "bank transfer for account credit",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	// The balance is what proves the routing: the invoice is paid either way.
	var balance int64
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0) FROM journal_entries
		WHERE account_code = 'customer_credit' AND customer_id = $1`, customerID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 3000 {
		t.Fatalf("credit balance = %d, want 3000", balance)
	}

	// And the ledger still balances per currency.
	rows, err := e.pool.Query(ctx, `
		SELECT currency, SUM(debit_minor) - SUM(credit_minor)
		FROM journal_entries GROUP BY currency`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			delta int64
		)
		if err := rows.Scan(&code, &delta); err != nil {
			t.Fatal(err)
		}
		if delta != 0 {
			t.Fatalf("the ledger is unbalanced in %s by %d", code, delta)
		}
	}
}
