package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Offline payments over HTTP.
//
// The invoice screen could issue, credit, refund and void, but there was no way to record money
// *arriving* outside a gateway — so a customer who pays by bank transfer could never have their
// invoice marked paid. These tests prove the path end to end at the boundary the operator uses:
//
//   - a full offline payment settles the invoice;
//   - a partial payment leaves the invoice partly paid with the right remainder;
//   - the same Idempotency-Key applies the money once (the middleware replay), and the same
//     reference with a fresh key also applies it once (the database identity);
//   - an overpayment is refused rather than converted to credit, and nothing is written;
//   - the recorded payment is distinguishable from a gateway payment by its method, both in the
//     payment list and on the invoice;
//   - the guards (permission, closed method set, reference, invoice state) answer specific codes.

// offlinePayment is the response body of the record-payment endpoint.
type offlinePayment struct {
	PaymentID   string                `json:"payment_id"`
	InvoiceID   string                `json:"invoice_id"`
	Provider    string                `json:"provider"`
	Method      string                `json:"method"`
	MethodLabel string                `json:"method_label"`
	Reference   string                `json:"reference"`
	Note        string                `json:"note"`
	Amount      struct{ Minor int64 } `json:"amount"`
	Total       struct{ Minor int64 } `json:"total"`
	Paid        struct{ Minor int64 } `json:"paid"`
	Remaining   struct{ Minor int64 } `json:"remaining"`
	InvoicePaid bool                  `json:"invoice_paid"`
	Already     bool                  `json:"already_recorded"`
}

// decodeOfflinePayment reads the response, failing with the body when it is not the expected shape.
func decodeOfflinePayment(t *testing.T, body []byte) offlinePayment {
	t.Helper()
	var out offlinePayment
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode payment: %v\n%s", err, body)
	}
	return out
}

// invoicePaidMinor reads the invoice's recorded paid amount and status.
func (e *authEnv) invoicePaidMinor(t *testing.T, invoiceID string) (int64, string) {
	t.Helper()
	var (
		paid   int64
		status string
	)
	if err := e.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor, status FROM invoices WHERE id = $1`, invoiceID).
		Scan(&paid, &status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return paid, status
}

// paymentCount counts payments recorded for one reference.
func (e *authEnv) paymentCount(t *testing.T, reference string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payments WHERE provider = 'manual' AND provider_payment_id = $1`,
		reference).Scan(&n); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}

// creditBalance sums the customer's credit account, which an overpayment must not touch.
func (e *authEnv) creditBalance(t *testing.T, customerID string) int64 {
	t.Helper()
	var balance int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0) FROM journal_entries
		WHERE account_code = 'customer_credit' AND customer_id = $1`, customerID).
		Scan(&balance); err != nil {
		t.Fatalf("read credit balance: %v", err)
	}
	return balance
}

// A bank transfer for the whole amount marks the invoice paid, and the payment is visibly
// offline: provider manual, method bank_transfer, and the operator's reference and note.
func TestRecordOfflinePaymentMarksInvoicePaid(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-full@example.test", "Offline Full")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)

	rec := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 10000,
			"method":       "bank_transfer",
			"reference":    "SEPA-2024-0001",
			"note":         "Received on the statement of the 3rd",
		}, "offline-payment-full-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	body := decodeOfflinePayment(t, rec.Body.Bytes())
	if body.PaymentID == "" {
		t.Fatal("no payment id")
	}
	if body.Provider != "manual" || body.Method != "bank_transfer" || body.MethodLabel != "Bank transfer" {
		t.Fatalf("body = %+v", body)
	}
	if body.Reference != "SEPA-2024-0001" || body.Note != "Received on the statement of the 3rd" {
		t.Fatalf("reference/note = %q/%q", body.Reference, body.Note)
	}
	if body.Amount.Minor != 10000 || body.Remaining.Minor != 0 || body.Paid.Minor != 10000 {
		t.Fatalf("amounts = %+v", body)
	}
	if !body.InvoicePaid || body.Already {
		t.Fatalf("paid/already = %v/%v", body.InvoicePaid, body.Already)
	}

	paid, status := e.invoicePaidMinor(t, invoiceID)
	if paid != 10000 || status != "paid" {
		t.Fatalf("invoice = %d %q, want 10000 paid", paid, status)
	}

	// The method and note are on the payment row, and the allocation exists — the money moved
	// through the same allocation path a webhook uses.
	var (
		method string
		note   string
		amount int64
	)
	if err := e.pool.QueryRow(context.Background(), `
		SELECT p.method, p.note, pa.amount_minor
		  FROM payments p JOIN payment_allocations pa ON pa.payment_id = p.id
		 WHERE p.id = $1`, body.PaymentID).Scan(&method, &note, &amount); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if method != "bank_transfer" || note == "" || amount != 10000 {
		t.Fatalf("stored payment = %q/%q/%d", method, note, amount)
	}

	// The payment list carries the method, so an offline payment is distinguishable from a
	// gateway payment without reading its reference.
	list := e.get(t, "/api/v1/staff/payments?provider=manual", e.billing)
	if list.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", list.Code, list.Body)
	}
	var page struct {
		Data []struct {
			ID       string  `json:"id"`
			Provider string  `json:"provider"`
			Method   *string `json:"method"`
			Note     *string `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("list body: %v\n%s", err, list.Body)
	}
	if len(page.Data) != 1 {
		t.Fatalf("list has %d manual payments, want 1; body %s", len(page.Data), list.Body)
	}
	row := page.Data[0]
	if row.ID != body.PaymentID || row.Provider != "manual" || row.Method == nil || *row.Method != "bank_transfer" {
		t.Fatalf("list row = %+v", row)
	}

	// And the invoice detail repeats it, which is what the invoice screen renders.
	detail := e.get(t, "/api/v1/staff/invoices/"+invoiceID, e.billing)
	if detail.Code != http.StatusOK {
		t.Fatalf("invoice: status %d; body %s", detail.Code, detail.Body)
	}
	if !strings.Contains(detail.Body.String(), `"method":"bank_transfer"`) {
		t.Fatalf("invoice detail does not carry the method: %s", detail.Body)
	}
}

// A partial payment leaves the invoice partly paid and the remainder exact, and a second payment
// for the remainder settles it. This is the bank-transfer case the panel could not express: the
// customer sends less than the invoice, and the balance has to follow.
func TestRecordOfflinePaymentPartial(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-partial@example.test", "Offline Partial")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)

	rec := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 4000,
			"method":       "cash",
			"reference":    "CASH-RECEIPT-77",
		}, "offline-payment-partial-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	body := decodeOfflinePayment(t, rec.Body.Bytes())
	if body.InvoicePaid {
		t.Fatal("a partial payment marked the invoice paid")
	}
	if body.Paid.Minor != 4000 || body.Remaining.Minor != 6000 {
		t.Fatalf("paid/remaining = %d/%d, want 4000/6000", body.Paid.Minor, body.Remaining.Minor)
	}

	paid, status := e.invoicePaidMinor(t, invoiceID)
	if paid != 4000 || status != "open" {
		t.Fatalf("invoice = %d %q, want 4000 open", paid, status)
	}

	// The rest, by cheque. The invoice settles and the balance is zero.
	rest := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 6000,
			"method":       "cheque",
			"reference":    "CHEQUE-4411",
		}, "offline-payment-partial-2")
	if rest.Code != http.StatusCreated {
		t.Fatalf("second: status %d; body %s", rest.Code, rest.Body)
	}
	second := decodeOfflinePayment(t, rest.Body.Bytes())
	if !second.InvoicePaid || second.Remaining.Minor != 0 || second.Paid.Minor != 10000 {
		t.Fatalf("second = %+v", second)
	}

	paid, status = e.invoicePaidMinor(t, invoiceID)
	if paid != 10000 || status != "paid" {
		t.Fatalf("invoice = %d %q, want 10000 paid", paid, status)
	}
}

// Recording the same payment twice with the same Idempotency-Key applies it once: the second
// request is answered from the stored response, the invoice is not credited twice, and there is
// still one payment row.
func TestRecordOfflinePaymentIdempotencyKey(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-idem@example.test", "Offline Idem")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)

	payload := map[string]any{
		"amount_minor": 2500,
		"method":       "bank_transfer",
		"reference":    "SEPA-IDEM-1",
		"note":         "double click",
	}

	first := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, payload, "offline-idem-key-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status %d; body %s", first.Code, first.Body)
	}
	firstBody := decodeOfflinePayment(t, first.Body.Bytes())

	// The same key and the same body: the middleware replays the stored response verbatim.
	second := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, payload, "offline-idem-key-1")
	if second.Code != http.StatusCreated {
		t.Fatalf("replay: status %d; body %s", second.Code, second.Body)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay was not marked: headers %v", second.Header())
	}
	secondBody := decodeOfflinePayment(t, second.Body.Bytes())
	if secondBody.PaymentID != firstBody.PaymentID {
		t.Fatalf("replay returned a different payment: %s then %s",
			firstBody.PaymentID, secondBody.PaymentID)
	}

	paid, _ := e.invoicePaidMinor(t, invoiceID)
	if paid != 2500 {
		t.Fatalf("invoice paid after the replay = %d, want 2500 (applied once)", paid)
	}
	if n := e.paymentCount(t, "SEPA-IDEM-1"); n != 1 {
		t.Fatalf("%d payment rows for one reference, want 1", n)
	}

	// A fresh key with the same reference is a different HTTP request but the same payment: the
	// database identity absorbs it, so it is reported as already recorded and moves no money.
	third := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, payload, "offline-idem-key-2")
	if third.Code != http.StatusCreated {
		t.Fatalf("fresh key: status %d; body %s", third.Code, third.Body)
	}
	thirdBody := decodeOfflinePayment(t, third.Body.Bytes())
	if !thirdBody.Already || thirdBody.PaymentID != firstBody.PaymentID {
		t.Fatalf("fresh key = %+v", thirdBody)
	}
	if paid, _ := e.invoicePaidMinor(t, invoiceID); paid != 2500 {
		t.Fatalf("invoice paid after the fresh-key retry = %d, want 2500", paid)
	}
	if n := e.paymentCount(t, "SEPA-IDEM-1"); n != 1 {
		t.Fatalf("%d payment rows after the retry, want 1", n)
	}
}

// An overpayment is refused, not converted to credit. This is the documented policy: granting
// credit is its own audited action, and a payment endpoint that silently creates a liability
// makes the two indistinguishable in the ledger. The test also proves nothing was written.
func TestRecordOfflinePaymentOverpaymentRefused(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-over@example.test", "Offline Over")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 5000, 0, nil)

	rec := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 7500,
			"method":       "cash",
			"reference":    "CASH-TOO-MUCH",
		}, "offline-overpayment-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "overpayment") {
		t.Fatalf("body = %s", rec.Body)
	}

	// Nothing was recorded: no payment row, no allocation, no invoice movement, and — the point
	// of the policy — no credit balance was minted from the excess.
	if n := e.paymentCount(t, "CASH-TOO-MUCH"); n != 0 {
		t.Fatalf("%d payment rows after a refused overpayment, want 0", n)
	}
	if paid, status := e.invoicePaidMinor(t, invoiceID); paid != 0 || status != "open" {
		t.Fatalf("invoice = %d %q after a refused overpayment", paid, status)
	}
	if balance := e.creditBalance(t, customerID); balance != 0 {
		t.Fatalf("credit balance = %d after a refused overpayment, want 0", balance)
	}

	// The exact amount is accepted afterwards, which proves the refusal was about the amount and
	// not about the request.
	ok := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/payments",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 5000,
			"method":       "cash",
			"reference":    "CASH-EXACT",
		}, "offline-overpayment-2")
	if ok.Code != http.StatusCreated {
		t.Fatalf("exact amount: status %d; body %s", ok.Code, ok.Body)
	}
	if paid, status := e.invoicePaidMinor(t, invoiceID); paid != 5000 || status != "paid" {
		t.Fatalf("invoice = %d %q, want 5000 paid", paid, status)
	}
}

// The guards: a permission, a closed method set, a required reference, and the invoice's state.
func TestRecordOfflinePaymentGuards(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-guard@example.test", "Offline Guard")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 5000, 0, nil)
	path := "/api/v1/staff/invoices/" + invoiceID + "/payments"

	valid := func(over map[string]any) map[string]any {
		body := map[string]any{
			"amount_minor": 1000,
			"method":       "bank_transfer",
			"reference":    "GUARD-1",
		}
		for k, v := range over {
			body[k] = v
		}
		return body
	}

	// Support can read invoices but may not assert that money arrived.
	forbidden := e.postJSONWithKey(t, path, e.support, e.supportCSRF, valid(nil), "guard-support-1")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403; body %s", forbidden.Code, forbidden.Body)
	}

	// The method set is closed, so a made-up channel is refused rather than stored.
	badMethod := e.postJSONWithKey(t, path, e.billing, e.billingCSRF,
		valid(map[string]any{"method": "carrier_pigeon"}), "guard-method-1")
	if badMethod.Code != http.StatusBadRequest || !containsCode(badMethod.Body.Bytes(), "invalid_method") {
		t.Fatalf("bad method: status %d; body %s", badMethod.Code, badMethod.Body)
	}

	// A reference is the payment's identity, so it is required.
	noRef := e.postJSONWithKey(t, path, e.billing, e.billingCSRF,
		valid(map[string]any{"reference": "  "}), "guard-ref-1")
	if noRef.Code != http.StatusBadRequest || !containsCode(noRef.Body.Bytes(), "invalid_payment") {
		t.Fatalf("no reference: status %d; body %s", noRef.Code, noRef.Body)
	}

	// Zero and negative amounts are not payments.
	for i, amount := range []int64{0, -100} {
		bad := e.postJSONWithKey(t, path, e.billing, e.billingCSRF,
			valid(map[string]any{"amount_minor": amount}), "guard-amount-"+string(rune('a'+i)))
		if bad.Code != http.StatusBadRequest || !containsCode(bad.Body.Bytes(), "invalid_payment") {
			t.Fatalf("amount %d: status %d; body %s", amount, bad.Code, bad.Body)
		}
	}

	// A currency that is not ISO is a client bug; one that disagrees with the invoice is a
	// mismatch. Both are refused before any money moves.
	unknown := e.postJSONWithKey(t, path, e.billing, e.billingCSRF,
		valid(map[string]any{"currency": "not-a-currency"}), "guard-currency-1")
	if unknown.Code != http.StatusBadRequest || !containsCode(unknown.Body.Bytes(), "invalid_currency") {
		t.Fatalf("unknown currency: status %d; body %s", unknown.Code, unknown.Body)
	}
	mismatch := e.postJSONWithKey(t, path, e.billing, e.billingCSRF,
		valid(map[string]any{"currency": "EUR"}), "guard-currency-2")
	if mismatch.Code != http.StatusBadRequest || !containsCode(mismatch.Body.Bytes(), "currency_mismatch") {
		t.Fatalf("currency mismatch: status %d; body %s", mismatch.Code, mismatch.Body)
	}

	// A draft has not been issued, so there is nothing to pay yet.
	draftID := e.seedInvoice(t, customerID, "draft", "USD", 5000, 0, nil)
	draft := e.postJSONWithKey(t, "/api/v1/staff/invoices/"+draftID+"/payments",
		e.billing, e.billingCSRF, valid(nil), "guard-draft-1")
	if draft.Code != http.StatusConflict || !containsCode(draft.Body.Bytes(), "not_issued") {
		t.Fatalf("draft: status %d; body %s", draft.Code, draft.Body)
	}

	// A malformed invoice id is a 404 that never reaches the database.
	badID := e.postJSONWithKey(t, "/api/v1/staff/invoices/not-a-uuid/payments",
		e.billing, e.billingCSRF, valid(nil), "guard-id-1")
	if badID.Code != http.StatusNotFound {
		t.Fatalf("bad id: status %d; body %s", badID.Code, badID.Body)
	}

	// A missing invoice is a 404 as well.
	missing := e.postJSONWithKey(t,
		"/api/v1/staff/invoices/00000000-0000-0000-0000-000000000000/payments",
		e.billing, e.billingCSRF, valid(nil), "guard-missing-1")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing invoice: status %d; body %s", missing.Code, missing.Body)
	}

	// The guards did not record anything.
	if n := e.paymentCount(t, "GUARD-1"); n != 0 {
		t.Fatalf("%d payments recorded by refused requests, want 0", n)
	}
	if paid, _ := e.invoicePaidMinor(t, invoiceID); paid != 0 {
		t.Fatalf("invoice paid = %d after refused requests, want 0", paid)
	}
}

// The same reference presented with a different method is a contradiction, not a correction: one
// payment arrived exactly one way. It is refused rather than silently overwriting the first
// recording, which is what a reconciler would later rely on.
func TestRecordOfflinePaymentReferenceConflict(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "offline-conflict@example.test", "Offline Conflict")
	invoiceID := e.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)
	path := "/api/v1/staff/invoices/" + invoiceID + "/payments"

	first := e.postJSONWithKey(t, path, e.billing, e.billingCSRF, map[string]any{
		"amount_minor": 3000, "method": "bank_transfer", "reference": "DUP-REF-1",
	}, "conflict-first")
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status %d; body %s", first.Code, first.Body)
	}

	second := e.postJSONWithKey(t, path, e.billing, e.billingCSRF, map[string]any{
		"amount_minor": 3000, "method": "cash", "reference": "DUP-REF-1",
	}, "conflict-second")
	if second.Code != http.StatusConflict {
		t.Fatalf("conflict: status %d; body %s", second.Code, second.Body)
	}
	if !containsCode(second.Body.Bytes(), "reference_conflict") {
		t.Fatalf("body = %s", second.Body)
	}

	// The first recording stands.
	var method string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT method FROM payments WHERE provider = 'manual' AND provider_payment_id = 'DUP-REF-1'`).
		Scan(&method); err != nil {
		t.Fatal(err)
	}
	if method != "bank_transfer" {
		t.Fatalf("method = %q, want bank_transfer", method)
	}
	if paid, _ := e.invoicePaidMinor(t, invoiceID); paid != 3000 {
		t.Fatalf("invoice paid = %d, want 3000", paid)
	}
}

// The vocabulary is served, so the form does not keep a second copy that can drift.
func TestOfflinePaymentMethods(t *testing.T) {
	e := newStaffEnv(t)

	rec := e.get(t, "/api/v1/staff/payment-methods/offline", e.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Methods []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	want := []string{"bank_transfer", "cash", "cheque", "other"}
	if len(body.Methods) != len(want) {
		t.Fatalf("methods = %+v", body.Methods)
	}
	for i, value := range want {
		if body.Methods[i].Value != value || body.Methods[i].Label == "" {
			t.Fatalf("method %d = %+v, want %s", i, body.Methods[i], value)
		}
	}

	// Reading the vocabulary still needs the reconciliation permission, because it is part of
	// the same staff surface.
	if denied := e.get(t, "/api/v1/staff/payment-methods/offline", e.support); denied.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403; body %s", denied.Code, denied.Body)
	}
}
