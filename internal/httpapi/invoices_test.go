package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The invoice parity surface as an operator meets it: the configured number format, the
// configured payment term, the send-email toggle, editing a draft, debit notes, voiding a
// note, the transactions list and the optional immutability switch.
//
// These run through the real router, the real permission checks and the real database,
// because most of them are properties of that combination: a number format that only the
// service honours is a number format the operator never sees, and an immutability switch that
// the trigger ignores is not a switch.

// setInvoiceSetting writes one invoice setting directly, which is what the settings screen's
// save path does.
func setInvoiceSetting(t *testing.T, env *authEnv, key, value string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', $1, $2)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set invoices.%s: %v", key, err)
	}
}

// reserveCreditNotes moves the credit-note sequence past the numbers the fixtures insert by
// hand. The sequence is package-wide test state, so without this a test's note can collide with
// the fixture's CN-000001 — which is a duplicate key, not a product bug.
func reserveCreditNotes(t *testing.T, env *documentEnv) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO invoice_number_sequences (scope, next_value) VALUES ('credit_note', 5000)
		ON CONFLICT (scope) DO UPDATE SET next_value = 5000`); err != nil {
		t.Fatalf("reserve credit note numbers: %v", err)
	}
}

// createInvoice posts a manual invoice and returns its id.
func createInvoice(t *testing.T, env *documentEnv, body map[string]any) (id string) {
	t.Helper()
	rec := env.postJSON(t, "/api/v1/staff/invoices", env.adminCookie, env.adminCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invoice: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return out.ID
}

// invoiceBody describes a one-line invoice for a customer.
func invoiceBody(customerID string, issue bool) map[string]any {
	return map[string]any{
		"customer_id": customerID,
		"issue_now":   issue,
		"lines": []map[string]any{{
			"description":       "VPS Small",
			"quantity":          1,
			"unit_amount_minor": 1200,
			"currency":          "USD",
		}},
	}
}

// The number format is a setting and the manual issue path must honour it (INV-03). Before
// this, only the proforma conversion read the configured format.
func TestIssueInvoiceUsesTheConfiguredNumberFormat(t *testing.T) {
	env := newDocumentEnv(t)
	setInvoiceSetting(t, env.authEnv, "invoice_number_prefix", "{number}/{year}")
	setInvoiceSetting(t, env.authEnv, "invoice_number_padding", "4")

	id := createInvoice(t, env, invoiceBody(env.alice.customerID, true))
	var number string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(number, '') FROM invoices WHERE id = $1`, id).Scan(&number); err != nil {
		t.Fatalf("read number: %v", err)
	}
	year := time.Now().UTC().Format("2006")
	want := regexp.MustCompile(`^[0-9]{4,}/` + year + `$`)
	if !want.MatchString(number) {
		t.Fatalf("number = %q, want the configured \"{number}/%s\" format", number, year)
	}
}

// The payment term is a setting and the manual create path must honour it (INV-17). The
// hard-coded 30 days was the gap the inventory named.
func TestManualInvoiceUsesTheConfiguredDueDays(t *testing.T) {
	env := newDocumentEnv(t)
	setInvoiceSetting(t, env.authEnv, "due_days", "3")

	id := createInvoice(t, env, invoiceBody(env.alice.customerID, false))
	var dueAt time.Time
	if err := env.pool.QueryRow(context.Background(),
		`SELECT due_at FROM invoices WHERE id = $1`, id).Scan(&dueAt); err != nil {
		t.Fatalf("read due date: %v", err)
	}
	days := time.Until(dueAt.UTC()).Hours() / 24
	if days < 2.5 || days > 3.5 {
		t.Fatalf("due in %.2f days, want 3 (the configured term)", days)
	}
}

// Issuing with send_email off must not queue the customer mail, and must record that choice
// (INV-14, ADM-31). The mail was previously unconditional.
func TestIssueWithSendEmailOffDoesNotQueueMail(t *testing.T) {
	env := newDocumentEnv(t)

	body := invoiceBody(env.alice.customerID, false)
	body["send_email"] = false
	id := createInvoice(t, env, body)

	rec := env.postJSON(t, "/api/v1/staff/invoices/"+id+"/issue",
		env.adminCookie, env.adminCSRF, map[string]any{"send_email": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}

	var queued, suppressed int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FILTER (WHERE status = 'queued'),
		       COUNT(*) FILTER (WHERE status = 'suppressed')
		FROM notification_deliveries WHERE kind = 'invoice_issued'`).
		Scan(&queued, &suppressed); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if queued != 0 {
		t.Fatalf("%d invoice mails were queued with send_email off", queued)
	}
	if suppressed != 1 {
		t.Fatalf("%d suppression rows, want 1: the decision must be recorded, not silent", suppressed)
	}

	// And the default is to send: an ordinary issue queues one message.
	second := createInvoice(t, env, invoiceBody(env.alice.customerID, false))
	rec = env.postJSON(t, "/api/v1/staff/invoices/"+second+"/issue",
		env.adminCookie, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM notification_deliveries
		WHERE kind = 'invoice_issued' AND status = 'queued'`).Scan(&queued); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queued != 1 {
		t.Fatalf("%d queued mails, want 1 for the default send", queued)
	}
}

// A draft can be re-priced through the API, and stops being editable the moment it is issued
// (INV-05).
func TestDraftLinesAreEditableOnlyWhileDraft(t *testing.T) {
	env := newDocumentEnv(t)
	id := createInvoice(t, env, invoiceBody(env.alice.customerID, false))

	rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+id+"/lines",
		env.adminCookie, env.adminCSRF, map[string]any{
			"lines": []map[string]any{
				{"description": "VPS Large", "quantity": 1, "unit_amount_minor": 2400, "currency": "USD"},
				{"description": "Backup", "quantity": 2, "unit_amount_minor": 100, "currency": "USD"},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("update draft: %d %s", rec.Code, rec.Body)
	}
	var updated struct {
		Totals struct {
			Total struct {
				Minor int64 `json:"minor"`
			} `json:"total"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Totals.Total.Minor != 2600 {
		t.Fatalf("total = %d, want 2600", updated.Totals.Total.Minor)
	}

	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+id+"/issue",
		env.adminCookie, env.adminCSRF, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
	rec = env.postJSONPut(t, "/api/v1/staff/invoices/"+id+"/lines",
		env.adminCookie, env.adminCSRF, map[string]any{
			"lines": []map[string]any{
				{"description": "Retroactive discount", "quantity": 1, "unit_amount_minor": 0, "currency": "USD"},
			},
		})
	if rec.Code != http.StatusConflict {
		t.Fatalf("editing an issued invoice: %d, want 409", rec.Code)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "not_draft" {
		t.Fatalf("code = %q, want not_draft", problem.Code)
	}
}

// A debit note is issued from the invoice, numbered from its own series and listed as an
// adjustment note (INV-10, ADM-25).
func TestDebitNoteIsIssuedAndListed(t *testing.T) {
	env := newDocumentEnv(t)

	rec := env.postJSON(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/debit-notes",
		env.adminCookie, env.adminCSRF, map[string]any{
			"amount_minor": 500,
			"reason":       "setup fee was undercharged",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("debit note: %d %s", rec.Code, rec.Body)
	}
	var issued struct {
		ID     string `json:"id"`
		Number string `json:"number"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if issued.Kind != "debit" {
		t.Fatalf("kind = %q, want debit", issued.Kind)
	}
	if !strings.HasPrefix(issued.Number, "DN-") {
		t.Fatalf("number = %q, want the DN- series", issued.Number)
	}

	rec = env.get(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/adjustment-notes", env.adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list notes: %d %s", rec.Code, rec.Body)
	}
	var listed struct {
		Notes []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, n := range listed.Notes {
		if n.ID == issued.ID {
			found = true
			if n.Kind != "debit" || n.Status != "issued" {
				t.Fatalf("listed note = %+v", n)
			}
		}
	}
	if !found {
		t.Fatalf("the debit note is not in the invoice's adjustment notes: %+v", listed.Notes)
	}
}

// Voiding an adjustment note cancels it, and a second void is a conflict rather than a second
// cancellation (ADM-25).
func TestVoidAdjustmentNoteCancelsItOnce(t *testing.T) {
	env := newDocumentEnv(t)

	reserveCreditNotes(t, env)
	rec := env.postJSON(t, "/api/v1/staff/invoices/"+env.bob.invoiceID+"/credit-notes",
		env.adminCookie, env.adminCSRF, map[string]any{
			"amount_minor": 300,
			"reason":       "goodwill",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("credit note: %d %s", rec.Code, rec.Body)
	}
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &note); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec = env.postJSON(t, "/api/v1/staff/credit-notes/"+note.ID+"/void",
		env.adminCookie, env.adminCSRF, map[string]any{"reason": "wrong amount"})
	if rec.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rec.Code, rec.Body)
	}
	var voided struct {
		Status     string `json:"status"`
		VoidReason string `json:"void_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &voided); err != nil {
		t.Fatalf("decode void: %v", err)
	}
	if voided.Status != "void" || voided.VoidReason != "wrong amount" {
		t.Fatalf("voided = %+v", voided)
	}

	rec = env.postJSON(t, "/api/v1/staff/credit-notes/"+note.ID+"/void",
		env.adminCookie, env.adminCSRF, map[string]any{"reason": "again"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second void: %d, want 409", rec.Code)
	}
}

// The invoice's transactions are the payments, credit applications and notes that explain its
// balance (ADM-24). A list that omitted the payment would leave the screen unable to answer
// "how was this paid".
func TestInvoiceTransactionsListThePayment(t *testing.T) {
	env := newDocumentEnv(t)

	var paymentID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status, method, note)
		VALUES ('offline', 'manual', 'cash-1', 1200, 'USD', 'succeeded', 'bank_transfer', 'wire')
		RETURNING id`).Scan(&paymentID); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 1200, 'USD')`, paymentID, env.alice.invoiceID); err != nil {
		t.Fatalf("allocation: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/transactions", env.adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("transactions: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Transactions []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Method string `json:"method"`
			Amount struct {
				Minor int64 `json:"minor"`
			} `json:"amount"`
			RefundURL string `json:"refund_url"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var payment *struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Method string `json:"method"`
		Amount struct {
			Minor int64 `json:"minor"`
		} `json:"amount"`
		RefundURL string `json:"refund_url"`
	}
	for i := range body.Transactions {
		if body.Transactions[i].ID == paymentID {
			payment = &body.Transactions[i]
		}
	}
	if payment == nil {
		t.Fatalf("the payment is not among the transactions: %+v", body.Transactions)
	}
	if payment.Kind != "payment" || payment.Method != "bank_transfer" {
		t.Fatalf("payment transaction = %+v", payment)
	}
	if payment.Amount.Minor != 1200 {
		t.Fatalf("amount = %d, want 1200", payment.Amount.Minor)
	}
	if !strings.HasSuffix(payment.RefundURL, "/refunds") {
		t.Fatalf("refund_url = %q, want the refund endpoint (the only correction ADM-24 allows)",
			payment.RefundURL)
	}
}

// The immutability switch refuses the soft edits the trigger leaves open, and only when an
// operator has turned it on (INV-05).
func TestImmutableSettingRefusesClientNoteOnIssuedInvoice(t *testing.T) {
	env := newDocumentEnv(t)

	// Default: the note is allowed, which is the behaviour that existed before the setting.
	if rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/client-note",
		env.adminCookie, env.adminCSRF, map[string]any{"note": "payable now"}); rec.Code != http.StatusOK {
		t.Fatalf("note with immutable off: %d %s", rec.Code, rec.Body)
	}

	setInvoiceSetting(t, env.authEnv, "immutable", "true")
	rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/client-note",
		env.adminCookie, env.adminCSRF, map[string]any{"note": "changed later"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("note with immutable on: %d, want 409", rec.Code)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "invoice_immutable" {
		t.Fatalf("code = %q, want invoice_immutable", problem.Code)
	}
}

// The credit-note number format is a setting too, and the issue path must read it (INV-18).
// The row named the hard-coded CN- and width; a settings screen that changes nothing is worse
// than no settings screen.
func TestCreditNoteUsesTheConfiguredNumberFormat(t *testing.T) {
	env := newDocumentEnv(t)
	reserveCreditNotes(t, env)
	setInvoiceSetting(t, env.authEnv, "credit_note_prefix", "{year}/CN-")
	setInvoiceSetting(t, env.authEnv, "credit_note_padding", "3")

	rec := env.postJSON(t, "/api/v1/staff/invoices/"+env.bob.invoiceID+"/credit-notes",
		env.adminCookie, env.adminCSRF, map[string]any{
			"amount_minor": 100,
			"reason":       "goodwill",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("credit note: %d %s", rec.Code, rec.Body)
	}
	var note struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &note); err != nil {
		t.Fatalf("decode: %v", err)
	}
	year := time.Now().UTC().Format("2006")
	if !strings.HasPrefix(note.Number, year+"/CN-") {
		t.Fatalf("number = %q, want the configured %s/CN- prefix", note.Number, year)
	}
	// The configured padding is three, so the value is zero-padded to at least three digits.
	digits := strings.TrimPrefix(note.Number, year+"/CN-")
	if len(digits) < 3 {
		t.Fatalf("number = %q, want a three-digit sequence", note.Number)
	}
}

// A due date can be extended on a draft, is refused once the document is immutable, and can
// never move earlier — moving it earlier would make an invoice retroactively overdue and
// restart dunning (INV-05).
func TestDueDateCanOnlyMoveLaterAndOnlyWhenAllowed(t *testing.T) {
	env := newDocumentEnv(t)

	// A draft may move, and the instant is stored.
	id := createInvoice(t, env, invoiceBody(env.alice.customerID, false))
	target := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second)
	rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+id+"/due-date",
		env.adminCookie, env.adminCSRF, map[string]any{"due_at": target})
	if rec.Code != http.StatusOK {
		t.Fatalf("extending a draft: %d %s", rec.Code, rec.Body)
	}

	// Never earlier: a due date that moves backwards would restart reminders against a
	// document that was already sent.
	rec = env.postJSONPut(t, "/api/v1/staff/invoices/"+id+"/due-date",
		env.adminCookie, env.adminCSRF,
		map[string]any{"due_at": target.AddDate(0, 0, -2)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("moving a due date earlier: %d, want 409", rec.Code)
	}

	// An issued document with the immutable switch on refuses even a later date.
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+id+"/issue",
		env.adminCookie, env.adminCSRF, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
	setInvoiceSetting(t, env.authEnv, "immutable", "true")
	rec = env.postJSONPut(t, "/api/v1/staff/invoices/"+id+"/due-date",
		env.adminCookie, env.adminCSRF, map[string]any{"due_in_days": 30})
	if rec.Code != http.StatusConflict {
		t.Fatalf("extending an immutable document: %d, want 409", rec.Code)
	}
}
