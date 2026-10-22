package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// The invoice-document surface: who may download what, and what a proforma looks
// like before and after it is paid.
//
// These tests use the real database and the real session middleware, because the
// property under test — a customer cannot download another customer's invoice —
// depends on the SQL predicate and on the permission check, and a mock of either
// would prove nothing about the other.

// documentEnv is a server with a portal and two customers who each own an issued
// invoice.
type documentEnv struct {
	*authEnv
	alice, bob         tenantFixture
	aliceCookie        *http.Cookie
	aliceCSRF          string
	adminCookie        *http.Cookie
	adminCSRF          string
	creditNoteForAlice string
}

func newDocumentEnv(t *testing.T) *documentEnv {
	t.Helper()
	env := &documentEnv{authEnv: newAuthEnv(t)}
	env.srv.WithPortal(portal.New(env.pool))

	env.alice = env.createTenant(t, "alice@example.test", "12.34")
	env.bob = env.createTenant(t, "bob@example.test", "99.00")
	env.aliceCookie, env.aliceCSRF = env.login(t, "alice@example.test", "a long enough password")

	env.createUser(t, "billing@example.test", "billing password 1", "billing")
	env.adminCookie, env.adminCSRF = env.login(t, "billing@example.test", "billing password 1")

	// A credit note against Alice's invoice, so the credit-note download has
	// something real to serve.
	var cnID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO credit_notes (number, customer_id, invoice_id, currency, total_minor,
			tax_minor, reason, status, issued_at)
		VALUES ('CN-000001', $1, $2, 'USD', 100, 0, 'goodwill', 'issued', now())
		RETURNING id`, env.alice.customerID, env.alice.invoiceID).Scan(&cnID); err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	env.creditNoteForAlice = cnID
	return env
}

// getDoc performs a GET and returns the recorder.
func (e *authEnv) getDoc(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A customer can download her own invoice, and the response is a PDF attachment.
func TestCustomerDownloadsOwnInvoicePDF(t *testing.T) {
	env := newDocumentEnv(t)

	rec := env.getDoc(t, "/api/v1/portal/invoices/"+env.alice.invoiceID+"/pdf", env.aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want an attachment", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, ".pdf") {
		t.Fatalf("Content-Disposition = %q, want a .pdf filename", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}

	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatalf("the body is not a PDF: %q", body[:min(12, len(body))])
	}
	// The document must carry the number the customer is being asked to pay.
	var number string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(number, '') FROM invoices WHERE id = $1`, env.alice.invoiceID).
		Scan(&number); err != nil {
		t.Fatalf("read number: %v", err)
	}
	if number == "" {
		t.Fatal("the seeded invoice has no number")
	}
	if !bytes.Contains(body, []byte(number)) {
		t.Fatalf("the invoice number %q is not in the PDF", number)
	}
}

// The central property: a customer cannot download another customer's invoice,
// and the refusal is indistinguishable from an id that does not exist.
func TestCustomerCannotDownloadAnotherCustomersInvoice(t *testing.T) {
	env := newDocumentEnv(t)

	foreign := env.getDoc(t, "/api/v1/portal/invoices/"+env.bob.invoiceID+"/pdf", env.aliceCookie)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("downloading a foreign invoice: status %d, want 404 (not 403)", foreign.Code)
	}
	if foreign.Code == http.StatusForbidden {
		t.Fatal("the response distinguishes a foreign invoice from a missing one with 403")
	}

	missing := env.getDoc(t,
		"/api/v1/portal/invoices/00000000-0000-0000-0000-000000000000/pdf", env.aliceCookie)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("a nonexistent invoice: status %d, want 404", missing.Code)
	}
	// The two bodies must be identical, or a probe can enumerate identifiers.
	if foreign.Body.String() != missing.Body.String() {
		t.Fatalf("a foreign invoice is distinguishable from a missing one:\n%s\nvs\n%s",
			foreign.Body, missing.Body)
	}
	// And nothing resembling a PDF may leak in the refusal.
	if bytes.HasPrefix(foreign.Body.Bytes(), []byte("%PDF-")) {
		t.Fatal("the refusal body is a PDF")
	}

	// A malformed identifier is also a 404 that never reaches the database.
	for _, id := range []string{"not-a-uuid", "123"} {
		rec := env.getDoc(t, "/api/v1/portal/invoices/"+id+"/pdf", env.aliceCookie)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", id, rec.Code)
		}
	}
}

// The same rule for credit notes: a note belongs to one customer.
func TestCustomerCannotDownloadAnotherCustomersCreditNote(t *testing.T) {
	env := newDocumentEnv(t)

	// Alice's own note works.
	rec := env.getDoc(t, "/api/v1/portal/credit-notes/"+env.creditNoteForAlice+"/pdf", env.aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("own credit note: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatal("the credit note is not a PDF")
	}

	// Bob cannot reach it, and gets the same answer as for a missing one.
	foreign := env.getDoc(t, "/api/v1/portal/credit-notes/"+env.creditNoteForAlice+"/pdf", env.bobUserCookie(t))
	missing := env.getDoc(t,
		"/api/v1/portal/credit-notes/00000000-0000-0000-0000-000000000000/pdf", env.bobUserCookie(t))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign credit note: status %d, want 404", foreign.Code)
	}
	if foreign.Body.String() != missing.Body.String() {
		t.Fatal("a foreign credit note is distinguishable from a missing one")
	}
}

// bobUserCookie signs Bob in once per call site without storing it on the fixture.
func (e *documentEnv) bobUserCookie(t *testing.T) *http.Cookie {
	t.Helper()
	cookie, _ := e.login(t, "bob@example.test", "a long enough password")
	return cookie
}

// Staff can download any invoice, including one they do not own.
func TestStaffCanDownloadAnyInvoicePDF(t *testing.T) {
	env := newDocumentEnv(t)

	for _, path := range []string{
		"/api/v1/staff/invoices/" + env.alice.invoiceID + "/pdf",
		"/api/v1/staff/invoices/" + env.bob.invoiceID + "/pdf",
		"/api/v1/staff/credit-notes/" + env.creditNoteForAlice + "/pdf",
	} {
		rec := env.getDoc(t, path, env.adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d; body %s", path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
			t.Fatalf("%s: Content-Type = %q", path, got)
		}
		if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
			t.Fatalf("%s: not a PDF", path)
		}
	}

	// A support agent has invoice:read:all too.
	env.createUser(t, "support@example.test", "support password 1", "support")
	supportCookie, _ := env.login(t, "support@example.test", "support password 1")
	if rec := env.getDoc(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/pdf", supportCookie); rec.Code != http.StatusOK {
		t.Fatalf("support: status %d, want 200", rec.Code)
	}

	// An unauthenticated caller gets the envelope, not a document.
	anon := env.getDoc(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/pdf", nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", anon.Code)
	}
}

// A draft is not a document. Staff get an actionable 409; a customer gets the same
// 404 as for a foreign invoice.
func TestDraftInvoiceHasNoPDF(t *testing.T) {
	env := newDocumentEnv(t)

	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, '{"schema_version":1}'::jsonb)
		RETURNING id`, env.alice.customerID).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	rec := env.getDoc(t, "/api/v1/staff/invoices/"+draftID+"/pdf", env.adminCookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("staff downloading a draft: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "not_issued" {
		t.Fatalf("body = %s", rec.Body)
	}

	if rec := env.getDoc(t, "/api/v1/portal/invoices/"+draftID+"/pdf", env.aliceCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("customer downloading a draft: status %d, want 404", rec.Code)
	}
}

// A proforma has no number until it is paid, and the document says so. The
// conversion is the same one the payment path performs, and it must assign the
// number in a single UPDATE.
func TestProformaHasNoNumberUntilPaid(t *testing.T) {
	env := newDocumentEnv(t)

	// A draft to issue as a proforma.
	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	rec := env.postJSON(t, "/api/v1/staff/invoices/"+draftID+"/proforma",
		env.adminCookie, env.adminCSRF, map[string]any{"due_in_days": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("issue proforma: %d %s", rec.Code, rec.Body)
	}
	var issued struct {
		Status   string  `json:"status"`
		Proforma bool    `json:"proforma"`
		Number   *string `json:"number"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !issued.Proforma {
		t.Fatal("the response does not report a proforma")
	}
	if issued.Number != nil {
		t.Fatalf("a proforma was issued with number %q", *issued.Number)
	}
	if issued.Status != "open" {
		t.Fatalf("status = %q, want open: a proforma is payable", issued.Status)
	}

	// The database agrees: no number, and the row is provisional.
	var (
		number   *string
		proforma bool
	)
	if err := env.pool.QueryRow(context.Background(),
		`SELECT number, proforma FROM invoices WHERE id = $1`, draftID).
		Scan(&number, &proforma); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if number != nil || !proforma {
		t.Fatalf("stored number = %v, proforma = %v; want NULL and true", number, proforma)
	}

	// The customer can download it, and the document states that it is not a tax
	// invoice with an unassigned number.
	pdf := env.getDoc(t, "/api/v1/portal/invoices/"+draftID+"/pdf", env.aliceCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("downloading the proforma: %d %s", pdf.Code, pdf.Body)
	}
	body := pdf.Body.Bytes()
	if !bytes.Contains(body, []byte("PROFORMA INVOICE")) {
		t.Fatal("the proforma PDF is not titled as one")
	}
	if !bytes.Contains(body, []byte("not yet assigned")) {
		t.Fatal("the proforma PDF does not say the number is unassigned")
	}
	if !bytes.Contains(body, []byte("not a tax invoice")) {
		t.Fatal("the proforma PDF does not say it is not a tax invoice")
	}
	// The filename must not be a UUID or an empty string.
	disposition := pdf.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "proforma") {
		t.Fatalf("Content-Disposition = %q, want a proforma filename", disposition)
	}

	// Pay it: this is the path the payment applier takes after settlement.
	if err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE invoices SET amount_paid_minor = total_minor, status = 'paid' WHERE id = $1`,
			draftID)
		return err
	}); err != nil {
		t.Fatalf("pay: %v", err)
	}

	// Conversion, in one UPDATE that clears the flag and assigns the number.
	var assigned string
	if err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		conv, err := convertProforma(ctx, tx, draftID)
		assigned = conv
		return err
	}); err != nil {
		t.Fatalf("convert proforma: %v", err)
	}
	if assigned == "" {
		t.Fatal("conversion assigned no number")
	}

	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(number, ''), proforma FROM invoices WHERE id = $1`, draftID).
		Scan(&number, &proforma); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if number == nil || *number != assigned {
		t.Fatalf("stored number = %v, want %q", number, assigned)
	}
	if proforma {
		t.Fatal("the invoice is still provisional after conversion")
	}

	// The document now carries the number.
	pdf = env.getDoc(t, "/api/v1/portal/invoices/"+draftID+"/pdf", env.aliceCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("downloading the converted invoice: %d", pdf.Code)
	}
	if !bytes.Contains(pdf.Body.Bytes(), []byte(assigned)) {
		t.Fatalf("the assigned number %q is not on the converted document", assigned)
	}
	if bytes.Contains(pdf.Body.Bytes(), []byte("PROFORMA INVOICE")) {
		t.Fatal("the converted document still says it is a proforma")
	}
}

// convertProforma performs the conversion the payment path performs, through the
// shipped function rather than a hand-written UPDATE, so the assertion is about
// the code that will run in production.
func convertProforma(ctx context.Context, tx pgx.Tx, invoiceID string) (string, error) {
	conv, err := invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
	if err != nil {
		return "", err
	}
	return conv.Number, nil
}

// A void invoice prints its reason, and a paid invoice cannot be voided even
// through the reason-carrying route.
func TestVoidWithReasonAppearsOnTheDocument(t *testing.T) {
	env := newDocumentEnv(t)

	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-VOID-1', 'open', 'USD', 1000, 0, 1000, now(), now() + interval '7 days', $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&draftID); err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	rec := env.postJSON(t, "/api/v1/staff/invoices/"+draftID+"/void-reason",
		env.adminCookie, env.adminCSRF, map[string]any{"reason": "raised against the wrong account"})
	if rec.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rec.Code, rec.Body)
	}

	// The reason is on the row, so it travels with the document.
	var reason string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT void_reason FROM invoices WHERE id = $1`, draftID).Scan(&reason); err != nil {
		t.Fatalf("read reason: %v", err)
	}
	if reason != "raised against the wrong account" {
		t.Fatalf("void_reason = %q", reason)
	}

	pdf := env.getDoc(t, "/api/v1/staff/invoices/"+draftID+"/pdf", env.adminCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("download voided: %d %s", pdf.Code, pdf.Body)
	}
	if !bytes.Contains(pdf.Body.Bytes(), []byte("raised against the wrong account")) {
		t.Fatal("the void reason is not printed on the document")
	}

	// A paid invoice is still not voidable through this route.
	paidID := env.alice.invoiceID
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE invoices SET amount_paid_minor = total_minor, status = 'paid' WHERE id = $1`,
		paidID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	rec = env.postJSON(t, "/api/v1/staff/invoices/"+paidID+"/void-reason",
		env.adminCookie, env.adminCSRF, map[string]any{"reason": "trying to erase it"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("voiding a paid invoice: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "not_voidable" {
		t.Fatalf("body = %s", rec.Body)
	}
}

// The client note reaches the customer's document only when the installation
// shows client notes, and the setting is what decides.
func TestClientNoteVisibilityFollowsTheSetting(t *testing.T) {
	env := newDocumentEnv(t)

	note := "Thank you for paying promptly."
	rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/client-note",
		env.adminCookie, env.adminCSRF, map[string]any{"note": note})
	if rec.Code != http.StatusOK {
		t.Fatalf("set note: %d %s", rec.Code, rec.Body)
	}

	// With the seeded setting (true) the note is on the customer's copy.
	pdf := env.getDoc(t, "/api/v1/portal/invoices/"+env.alice.invoiceID+"/pdf", env.aliceCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("download: %d %s", pdf.Code, pdf.Body)
	}
	if !bytes.Contains(pdf.Body.Bytes(), []byte(note)) {
		t.Fatal("the client note is not on the customer's document")
	}

	// Turning the setting off hides it from the customer but keeps it stored.
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE settings SET value = 'false'
		WHERE group_key = 'invoices' AND key = 'notes_client_visible'`); err != nil {
		t.Fatalf("disable notes: %v", err)
	}
	pdf = env.getDoc(t, "/api/v1/portal/invoices/"+env.alice.invoiceID+"/pdf", env.aliceCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("download: %d", pdf.Code)
	}
	if bytes.Contains(pdf.Body.Bytes(), []byte(note)) {
		t.Fatal("the client note is printed while client notes are disabled")
	}
	var stored string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT client_note FROM invoices WHERE id = $1`, env.alice.invoiceID).Scan(&stored); err != nil {
		t.Fatalf("read note: %v", err)
	}
	if stored != note {
		t.Fatalf("the note was not stored while hidden: %q", stored)
	}
}

// postJSONPut is postJSON for a PUT, which the note route uses.
func (e *authEnv) postJSONPut(t *testing.T, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytesReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "test-put-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// snapshotJSON is a minimal but complete invoice snapshot, so a hand-inserted
// invoice renders instead of producing an empty document.
func snapshotJSON(t *testing.T) []byte {
	t.Helper()
	cur := money.MustLookupCurrency("USD")
	snap := billing.Snapshot{
		SchemaVersion: 1,
		Merchant:      billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
		Customer:      billing.CustomerDetails{Name: "Ada Lovelace", Email: "alice@example.test"},
		Lines: []billing.LineResult{{
			Description: "Monthly hosting", QuantityMilli: 1000, UnitMinor: 1000,
			Net: money.New(1000, cur), Tax: money.New(0, cur), Total: money.New(1000, cur),
			TaxRate: "0",
		}},
		Totals:   billing.Totals{Subtotal: money.New(1000, cur), Tax: money.New(0, cur), Total: money.New(1000, cur)},
		Currency: "USD",
		Terms:    "Payable within 7 days.",
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	return encoded
}
