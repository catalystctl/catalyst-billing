package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The document surface as a *screen* needs it.
//
// The PDFs and the mutations were already served and tested (invoice_pdf_test.go): an operator
// could issue a credit note and could not download it, could write a client note and could not
// see it, and a customer was sent a credit note with nothing telling them it existed. These
// tests cover the two reads that close those gaps — the staff invoice payload's document state
// and `GET /api/v1/portal/invoices/{id}/document` — because a document nobody can reach is a
// document that may as well not have been rendered.

// portalDocument is the shape `handlePortalInvoiceDocument` returns.
type portalDocument struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Proforma     bool   `json:"proforma"`
	ClientNote   string `json:"client_note"`
	NotesVisible bool   `json:"notes_visible"`
	VoidReason   string `json:"void_reason"`
	CreditNotes  []struct {
		ID     string `json:"id"`
		Number string `json:"number"`
		Total  struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"total"`
		Reason   string `json:"reason"`
		Status   string `json:"status"`
		IssuedAt string `json:"issued_at"`
	} `json:"credit_notes"`
}

// getPortalDocument reads one invoice's document state as the given session.
func getPortalDocument(t *testing.T, env *documentEnv, invoiceID string, cookie *http.Cookie) portalDocument {
	t.Helper()
	rec := env.getDoc(t, "/api/v1/portal/invoices/"+invoiceID+"/document", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("portal document: status %d, body %s", rec.Code, rec.Body)
	}
	var doc portalDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return doc
}

// A customer can see the credit notes against their invoice, and the id that read reports is
// the id the PDF is served under. That link is the whole point: the download existed and was
// unreachable because nothing named the document.
func TestPortalDocumentListsTheCreditNotesItCanDownload(t *testing.T) {
	env := newDocumentEnv(t)

	doc := getPortalDocument(t, env, env.alice.invoiceID, env.aliceCookie)
	if doc.ID != env.alice.invoiceID {
		t.Fatalf("id = %q, want %q", doc.ID, env.alice.invoiceID)
	}
	if len(doc.CreditNotes) != 1 {
		t.Fatalf("credit_notes = %+v, want the one issued against this invoice", doc.CreditNotes)
	}
	note := doc.CreditNotes[0]
	if note.ID != env.creditNoteForAlice {
		t.Fatalf("credit note id = %q, want %q", note.ID, env.creditNoteForAlice)
	}
	if note.Number != "CN-000001" {
		t.Fatalf("credit note number = %q", note.Number)
	}
	if note.Total.Minor != 100 || note.Total.Currency != "USD" {
		t.Fatalf("credit note total = %+v, want 100 USD", note.Total)
	}
	if note.IssuedAt == "" {
		t.Fatal("an issued credit note has no issue date")
	}

	// The id from that read is the id the download is keyed on.
	pdf := env.getDoc(t, "/api/v1/portal/credit-notes/"+note.ID+"/pdf", env.aliceCookie)
	if pdf.Code != http.StatusOK {
		t.Fatalf("downloading the credit note the page named: %d %s", pdf.Code, pdf.Body)
	}
	if got := pdf.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", got)
	}
}

// Another customer's invoice, an invoice that never existed, and a draft are one answer: 404.
// The ownership predicate is in the SQL, so the three cannot be told apart.
func TestPortalDocumentIsScopedToTheCaller(t *testing.T) {
	env := newDocumentEnv(t)

	if rec := env.getDoc(t, "/api/v1/portal/invoices/"+env.alice.invoiceID+"/document",
		env.bobUserCookie(t)); rec.Code != http.StatusNotFound {
		t.Fatalf("another customer's document: status %d, want 404", rec.Code)
	}
	if rec := env.getDoc(t, "/api/v1/portal/invoices/00000000-0000-0000-0000-000000000000/document",
		env.aliceCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("a nonexistent document: status %d, want 404", rec.Code)
	}
	if rec := env.getDoc(t, "/api/v1/portal/invoices/not-a-uuid/document",
		env.aliceCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("a malformed id: status %d, want 404", rec.Code)
	}
	if rec := env.getDoc(t, "/api/v1/portal/invoices/"+env.alice.invoiceID+"/document", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous read: status %d, want 401", rec.Code)
	}

	// A draft has no document to describe.
	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if rec := env.getDoc(t, "/api/v1/portal/invoices/"+draftID+"/document",
		env.aliceCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("a draft: status %d, want 404", rec.Code)
	}
}

// The note a customer reads is the note the document prints: present while the installation
// prints client notes, absent — though still stored — when it does not.
func TestPortalDocumentCarriesTheClientNoteOnlyWhenItIsPrinted(t *testing.T) {
	env := newDocumentEnv(t)
	note := "The setup fee was charged twice. This invoice carries the correction."

	if rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/client-note",
		env.adminCookie, env.adminCSRF, map[string]any{"note": note}); rec.Code != http.StatusOK {
		t.Fatalf("set note: %d %s", rec.Code, rec.Body)
	}

	doc := getPortalDocument(t, env, env.alice.invoiceID, env.aliceCookie)
	if doc.ClientNote != note {
		t.Fatalf("client_note = %q, want %q", doc.ClientNote, note)
	}
	if !doc.NotesVisible {
		t.Fatal("notes_visible = false with the seeded setting on")
	}

	// Turning the setting off is a decision about what a customer reads, so the note leaves the
	// customer's view. It is not deleted: the row still carries it, and the operator's screen
	// says so.
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE settings SET value = 'false'
		WHERE group_key = 'invoices' AND key = 'notes_client_visible'`); err != nil {
		t.Fatalf("disable notes: %v", err)
	}
	doc = getPortalDocument(t, env, env.alice.invoiceID, env.aliceCookie)
	if doc.ClientNote != "" {
		t.Fatalf("client_note = %q with notes disabled, want it withheld", doc.ClientNote)
	}
	if doc.NotesVisible {
		t.Fatal("notes_visible = true with the setting off")
	}
	var stored string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(client_note, '') FROM invoices WHERE id = $1`, env.alice.invoiceID).
		Scan(&stored); err != nil {
		t.Fatalf("read note: %v", err)
	}
	if stored != note {
		t.Fatalf("stored note = %q, want it kept", stored)
	}
}

// The staff invoice payload carries the document's lifecycle state, so the screen can say that
// a document is a proforma, show the note it just wrote, and explain a void.
func TestStaffInvoiceCarriesTheDocumentState(t *testing.T) {
	env := newDocumentEnv(t)

	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	read := func() struct {
		Proforma           bool   `json:"proforma"`
		ClientNote         string `json:"client_note"`
		VoidReason         string `json:"void_reason"`
		NotesClientVisible bool   `json:"notes_client_visible"`
		Number             string `json:"number"`
		Status             string `json:"status"`
	} {
		t.Helper()
		rec := env.get(t, "/api/v1/staff/invoices/"+draftID, env.adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("staff invoice: %d %s", rec.Code, rec.Body)
		}
		var body struct {
			Invoice struct {
				Proforma           bool   `json:"proforma"`
				ClientNote         string `json:"client_note"`
				VoidReason         string `json:"void_reason"`
				NotesClientVisible bool   `json:"notes_client_visible"`
				Number             string `json:"number"`
				Status             string `json:"status"`
			} `json:"invoice"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode invoice: %v", err)
		}
		return body.Invoice
	}

	before := read()
	if before.Proforma {
		t.Fatal("a draft reports itself as a proforma")
	}
	if before.ClientNote != "" {
		t.Fatalf("client_note = %q on a fresh draft", before.ClientNote)
	}
	if !before.NotesClientVisible {
		t.Fatal("notes_client_visible = false with the seeded setting on")
	}

	// Issuing a proforma is a state the screen has to be able to show: the document is now open
	// and payable, and still has no number.
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+draftID+"/proforma",
		env.adminCookie, env.adminCSRF, map[string]any{"due_in_days": 7}); rec.Code != http.StatusOK {
		t.Fatalf("issue proforma: %d %s", rec.Code, rec.Body)
	}
	after := read()
	if !after.Proforma {
		t.Fatal("the issued proforma is not reported as one")
	}
	if after.Number != "" {
		t.Fatalf("the proforma carries number %q", after.Number)
	}
	if after.Status != "open" {
		t.Fatalf("status = %q, want open: a proforma is payable", after.Status)
	}

	// The customer's own read agrees with the operator's, which is what makes the badge on the
	// two screens the same fact rather than two guesses.
	doc := getPortalDocument(t, env, draftID, env.aliceCookie)
	if !doc.Proforma {
		t.Fatal("the customer's document does not report the proforma")
	}

	// The note reaches the staff payload too: a screen that cannot show what it wrote is a
	// screen an operator has to guess at.
	note := "Payable now; the number is assigned when it is paid."
	if rec := env.postJSONPut(t, "/api/v1/staff/invoices/"+draftID+"/client-note",
		env.adminCookie, env.adminCSRF, map[string]any{"note": note}); rec.Code != http.StatusOK {
		t.Fatalf("set note: %d %s", rec.Code, rec.Body)
	}
	if got := read().ClientNote; got != note {
		t.Fatalf("client_note = %q, want %q", got, note)
	}
}

// A void document carries its reason into both reads, so the screen explains the void and the
// customer is not left with an unexplained cancellation.
func TestStaffAndPortalReadsCarryTheVoidReason(t *testing.T) {
	env := newDocumentEnv(t)

	var invoiceID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-VOID-READ-1', 'open', 'USD', 1000, 0, 1000, now(),
		        now() + interval '7 days', $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&invoiceID); err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	reason := "raised against the wrong account"
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+invoiceID+"/void-reason",
		env.adminCookie, env.adminCSRF, map[string]any{"reason": reason}); rec.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rec.Code, rec.Body)
	}

	rec := env.get(t, "/api/v1/staff/invoices/"+invoiceID, env.adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff invoice: %d %s", rec.Code, rec.Body)
	}
	var staffBody struct {
		Invoice struct {
			VoidReason string `json:"void_reason"`
			Status     string `json:"status"`
		} `json:"invoice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &staffBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if staffBody.Invoice.VoidReason != reason {
		t.Fatalf("staff void_reason = %q, want %q", staffBody.Invoice.VoidReason, reason)
	}

	doc := getPortalDocument(t, env, invoiceID, env.aliceCookie)
	if doc.VoidReason != reason {
		t.Fatalf("portal void_reason = %q, want %q", doc.VoidReason, reason)
	}
	if doc.Status != "void" {
		t.Fatalf("portal status = %q, want void", doc.Status)
	}
}

// A staff read of an invoice that does not exist is a 404 with the problem+json shape, not a
// 500 from a missing row.
func TestStaffInvoiceDocumentStateMissingInvoice(t *testing.T) {
	env := newDocumentEnv(t)

	rec := env.get(t, "/api/v1/staff/invoices/00000000-0000-0000-0000-000000000000", env.adminCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}
}

// The credit note list is scoped to the invoice it corrects: a note against another invoice of
// the same customer does not appear here.
func TestPortalDocumentListsOnlyThisInvoicesCreditNotes(t *testing.T) {
	env := newDocumentEnv(t)

	// A second, unrelated credit note for Alice, correcting nothing (a goodwill grant).
	var otherID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO credit_notes (number, customer_id, invoice_id, currency, total_minor,
			tax_minor, reason, status, issued_at)
		VALUES ('CN-000002', $1, $2, 'USD', 500, 0, 'goodwill', 'issued', now())
		RETURNING id`, env.alice.customerID, env.bob.invoiceID).Scan(&otherID); err != nil {
		t.Fatalf("create other credit note: %v", err)
	}

	doc := getPortalDocument(t, env, env.alice.invoiceID, env.aliceCookie)
	if len(doc.CreditNotes) != 1 || doc.CreditNotes[0].ID != env.creditNoteForAlice {
		t.Fatalf("credit_notes = %+v, want only the note against this invoice", doc.CreditNotes)
	}

	// It is still Alice's document, so it is still readable by her — just not on this invoice.
	if rec := env.getDoc(t, "/api/v1/portal/credit-notes/"+otherID+"/pdf",
		env.aliceCookie); rec.Code != http.StatusOK {
		t.Fatalf("downloading a note against another invoice: %d %s", rec.Code, rec.Body)
	}
}

// A proforma that is converted stops being provisional in both reads, so the badge on the
// screen follows the document rather than a cached impression of it.
func TestProformaStateClearsOnConversionInBothReads(t *testing.T) {
	env := newDocumentEnv(t)

	var draftID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, $2)
		RETURNING id`, env.alice.customerID, snapshotJSON(t)).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if rec := env.postJSON(t, "/api/v1/staff/invoices/"+draftID+"/proforma",
		env.adminCookie, env.adminCSRF, map[string]any{"due_in_days": 7}); rec.Code != http.StatusOK {
		t.Fatalf("issue proforma: %d %s", rec.Code, rec.Body)
	}
	if !getPortalDocument(t, env, draftID, env.aliceCookie).Proforma {
		t.Fatal("the proforma is not provisional after issuing it")
	}

	// The conversion the payment path performs, through the shipped function.
	if err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := convertProforma(ctx, tx, draftID)
		return err
	}); err != nil {
		t.Fatalf("convert: %v", err)
	}

	doc := getPortalDocument(t, env, draftID, env.aliceCookie)
	if doc.Proforma {
		t.Fatal("the converted invoice still reports itself as a proforma")
	}
	rec := env.get(t, "/api/v1/staff/invoices/"+draftID, env.adminCookie)
	var body struct {
		Invoice struct {
			Proforma bool   `json:"proforma"`
			Number   string `json:"number"`
		} `json:"invoice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Invoice.Proforma {
		t.Fatal("the staff read still reports a proforma after conversion")
	}
	if body.Invoice.Number == "" {
		t.Fatal("the converted invoice has no number")
	}
}
