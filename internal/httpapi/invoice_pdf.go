package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// Invoice documents over HTTP (INV-08, INV-10, INV-11).
//
// A customer is asked to pay a document they cannot download. That is the gap
// these routes close: the same PDF is served to the customer who owes the money
// and to the operator answering a question about it, from one renderer
// (internal/invoicepdf) and one snapshot (the invoice's frozen `snapshot` column).
//
// Three rules shape the handlers:
//
//  1. **The customer's identity is in the SQL, not in a later check.** A query that
//     fetches by id and then compares owners is one forgotten comparison away from
//     an IDOR. These queries join customer_accounts on the session's user, so
//     another customer's invoice cannot be selected at all and the answer is 404,
//     indistinguishable from a nonexistent id.
//  2. **A draft is not a document.** It has no number and was never issued, so
//     there is nothing to print. The portal answers 404 for the same reason it does
//     for a foreign invoice; staff get a 409 that says why, because for staff a
//     draft demonstrably exists.
//  3. **The bytes are the response.** No JSON wrapper, no base64: a browser that
//     follows the link gets a file. The filename comes from the document number and
//     is sanitised, because the number's prefix is operator-configurable.

func init() {
	RegisterRoutes("85-invoice-documents", registerInvoiceDocuments)
}

func registerInvoiceDocuments(r *router) {
	// The customer's own documents. invoice:read:self is the permission the
	// portal's invoice page already relies on, so a customer who can see the
	// amount can also download the document that states it.
	r.get("/api/v1/portal/invoices/{id}/pdf", authn.PermInvoiceReadSelf, r.s.handlePortalInvoicePDF)
	r.get("/api/v1/portal/credit-notes/{id}/pdf", authn.PermInvoiceReadSelf, r.s.handlePortalCreditNotePDF)

	// The same documents for staff, who need them to answer a question about a
	// document the customer is holding.
	r.staffGet("/api/v1/staff/invoices/{id}/pdf", authn.PermInvoiceReadAll, r.s.handleStaffInvoicePDF)
	r.staffGet("/api/v1/staff/credit-notes/{id}/pdf", authn.PermInvoiceReadAll, r.s.handleStaffCreditNotePDF)
}

// ---------------------------------------------------------------- the read --

// documentRow is one invoice as the renderer needs it: the frozen snapshot plus the
// columns that are not in it.
type documentRow struct {
	// InvoiceID is carried for the filename of an unnumbered document, and for
	// nothing else: the renderer works from the snapshot and the columns below.
	InvoiceID  string
	Snapshot   billing.Snapshot
	Number     string
	Status     string
	Currency   string
	IssuedAt   *time.Time
	DueAt      *time.Time
	Proforma   bool
	ClientNote string
	VoidReason string
	BillToText string
}

// invoiceDocumentQuery selects the columns a document needs.
//
// The scoping predicate is appended by the caller, so there is exactly one place
// that knows how to read a document and exactly one place per audience that knows
// who may read it.
const invoiceDocumentQuery = `
	SELECT i.id, i.snapshot, COALESCE(i.number, ''), i.status, i.currency,
	       i.issued_at, i.due_at, i.proforma, i.client_note, i.void_reason,
	       COALESCE(bp.bill_to_text, '')
	FROM invoices i
	LEFT JOIN billing_profiles bp ON bp.customer_id = i.customer_id`

// scanDocument reads one document row.
func scanDocument(row pgx.Row) (documentRow, error) {
	var (
		raw      []byte
		out      documentRow
		issuedAt *time.Time
		dueAt    *time.Time
	)
	if err := row.Scan(&out.InvoiceID, &raw, &out.Number, &out.Status, &out.Currency,
		&issuedAt, &dueAt, &out.Proforma, &out.ClientNote, &out.VoidReason,
		&out.BillToText); err != nil {
		return documentRow{}, err
	}
	if err := json.Unmarshal(raw, &out.Snapshot); err != nil {
		// A snapshot that cannot be parsed is not a customer error: the document
		// was written by this system. Say so without leaking the parse detail.
		return documentRow{}, fmt.Errorf("httpapi: parse invoice snapshot: %w", err)
	}
	out.IssuedAt, out.DueAt = issuedAt, dueAt
	return out, nil
}

// loadPortalInvoiceDocument reads an invoice the caller's own customer owns.
//
// The ownership predicate is `ca.user_id = $2`, joined in SQL: a substituted
// identifier selects no row and is therefore a 404 that is byte-identical to the
// 404 for an id that never existed.
func (s *Server) loadPortalInvoiceDocument(ctx context.Context, userID, invoiceID string) (documentRow, error) {
	if s.pool == nil {
		return documentRow{}, errDocumentUnavailable
	}
	return scanDocument(s.pool.QueryRow(ctx, invoiceDocumentQuery+`
		JOIN customer_accounts ca ON ca.customer_id = i.customer_id
		WHERE i.id = $1 AND ca.user_id = $2 AND i.status <> 'draft'`, invoiceID, userID))
}

// loadStaffInvoiceDocument reads any issued invoice.
func (s *Server) loadStaffInvoiceDocument(ctx context.Context, invoiceID string) (documentRow, error) {
	if s.pool == nil {
		return documentRow{}, errDocumentUnavailable
	}
	return scanDocument(s.pool.QueryRow(ctx, invoiceDocumentQuery+`
		WHERE i.id = $1 AND i.status <> 'draft'`, invoiceID))
}

// ------------------------------------------------------------------- PDFs --

// handlePortalInvoicePDF serves the caller's own invoice.
func (s *Server) handlePortalInvoicePDF(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in to download this document")
		return
	}
	id := r.PathValue("id")
	// A malformed id is a 404 before the database is touched, so the response
	// cannot differ between "malformed" and "someone else's".
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	doc, err := s.loadPortalInvoiceDocument(r.Context(), user.ID, id)
	if err != nil {
		s.writeDocumentError(w, r, err, false)
		return
	}
	s.serveInvoiceDocument(w, r, doc)
}

// handleStaffInvoicePDF serves any issued invoice.
func (s *Server) handleStaffInvoicePDF(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	doc, err := s.loadStaffInvoiceDocument(r.Context(), id)
	if err != nil {
		s.writeDocumentError(w, r, err, true)
		return
	}
	s.serveInvoiceDocument(w, r, doc)
}

// serveInvoiceDocument renders and writes one invoice PDF.
func (s *Server) serveInvoiceDocument(w http.ResponseWriter, r *http.Request, doc documentRow) {
	// The note policy belongs here, not in the renderer: `invoices.notes_client_visible`
	// is an installation-wide decision about what a *client* may see, and a renderer
	// that consulted settings would print a different document for staff and for the
	// customer from the same bytes.
	note := doc.ClientNote
	if note != "" && !s.clientNotesVisible(r.Context()) {
		note = ""
	}

	meta := invoicepdf.Meta{
		Number:     doc.Number,
		Status:     doc.Status,
		IssuedAt:   doc.IssuedAt,
		DueAt:      doc.DueAt,
		Proforma:   doc.Proforma,
		ClientNote: note,
		VoidReason: doc.VoidReason,
		BillToText: doc.BillToText,
	}
	// The tenant's brand is an overlay on the layout, never a change to it: the columns, the
	// totals and the tax evidence are the product of a frozen snapshot, and a theme must not
	// be able to move a fact on a document a customer is asked to pay. An unset brand leaves
	// the bytes identical, so a branded installation and an unbranded one differ only in the
	// colour, the name and the mark.
	document := invoicepdf.FromInvoice(doc.Snapshot, meta)
	document.Brand = s.invoiceBrand(r.Context())
	pdf, err := invoicepdf.Render(document)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "render invoice pdf", "error", err)
		writeProblem(w, http.StatusInternalServerError, "render_failed",
			"the document could not be rendered")
		return
	}

	name := doc.Number
	if name == "" {
		// A proforma has no number, so it is named for what it is. The id suffix
		// keeps two proformas from downloading as the same file without putting a
		// whole UUID in front of the customer.
		name = "proforma-" + invoicepdf.SafeFilename(doc.InvoiceID)
	}
	writePDF(w, invoicepdf.SafeFilename(name)+".pdf", pdf)
}

// handlePortalCreditNotePDF serves a credit note the caller's customer owns.
func (s *Server) handlePortalCreditNotePDF(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in to download this document")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	cn, err := s.loadCreditNoteDocument(r.Context(), user.ID, id)
	if err != nil {
		s.writeDocumentError(w, r, err, false)
		return
	}
	s.serveCreditNoteDocument(w, r, cn)
}

// handleStaffCreditNotePDF serves any credit note.
func (s *Server) handleStaffCreditNotePDF(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	cn, err := s.loadCreditNoteDocument(r.Context(), "", id)
	if err != nil {
		s.writeDocumentError(w, r, err, true)
		return
	}
	s.serveCreditNoteDocument(w, r, cn)
}

// creditNoteRow is one credit note plus the invoice snapshot it corrects.
type creditNoteRow struct {
	Number     string
	InvoiceID  string
	Currency   string
	TotalMinor int64
	TaxMinor   int64
	Reason     string
	Status     string
	IssuedAt   *time.Time
	CustomerID string
	Customer   billing.CustomerDetails
	// Base is the snapshot of the corrected invoice. It supplies the letterhead.
	// A goodwill credit has no invoice, so Base is zero and the letterhead is
	// built from the customer record and the installation's settings.
	Base       billing.Snapshot
	HasInvoice bool
}

// loadCreditNoteDocument reads one credit note, scoped to userID when it is not
// empty.
func (s *Server) loadCreditNoteDocument(ctx context.Context, userID, id string) (creditNoteRow, error) {
	if s.pool == nil {
		return creditNoteRow{}, errDocumentUnavailable
	}
	query := `
		SELECT COALESCE(cn.number, ''), COALESCE(cn.invoice_id::text, ''), cn.currency,
		       cn.total_minor, cn.tax_minor, cn.reason, cn.status, cn.issued_at,
		       cn.customer_id,
		       COALESCE(i.snapshot, '{}'::jsonb)
		FROM credit_notes cn
		LEFT JOIN invoices i ON i.id = cn.invoice_id`
	args := []any{id}
	if userID != "" {
		// The ownership predicate is the join, so another customer's note is not
		// selected rather than selected-and-refused.
		query += ` JOIN customer_accounts ca ON ca.customer_id = cn.customer_id`
		query += ` WHERE cn.id = $1 AND ca.user_id = $2`
		args = append(args, userID)
	} else {
		query += ` WHERE cn.id = $1`
	}

	var (
		out      creditNoteRow
		rawBase  []byte
		issuedAt *time.Time
	)
	if err := s.pool.QueryRow(ctx, query, args...).Scan(
		&out.Number, &out.InvoiceID, &out.Currency, &out.TotalMinor, &out.TaxMinor,
		&out.Reason, &out.Status, &issuedAt, &out.CustomerID, &rawBase); err != nil {
		return creditNoteRow{}, err
	}
	out.IssuedAt = issuedAt
	if out.InvoiceID != "" {
		if err := json.Unmarshal(rawBase, &out.Base); err != nil {
			return creditNoteRow{}, fmt.Errorf("httpapi: parse credit note invoice snapshot: %w", err)
		}
		out.HasInvoice = true
	}

	// The customer block is needed for a note that corrects nothing.
	var name, email string
	if err := s.pool.QueryRow(ctx,
		`SELECT name, email FROM customers WHERE id = $1`, out.CustomerID).
		Scan(&name, &email); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return creditNoteRow{}, fmt.Errorf("httpapi: load credit note customer: %w", err)
	}
	out.Customer = billing.CustomerDetails{ID: out.CustomerID, Name: name, Email: email}
	return out, nil
}

// serveCreditNoteDocument renders and writes one credit note PDF.
func (s *Server) serveCreditNoteDocument(w http.ResponseWriter, r *http.Request, cn creditNoteRow) {
	base := cn.Base
	if !cn.HasInvoice {
		// A goodwill credit has no invoice to inherit a letterhead from, so the
		// merchant block comes from settings and the customer block from the
		// customer record. The alternative — refusing to print the note — would
		// leave the customer holding credit they cannot document.
		base = billing.Snapshot{
			Merchant: s.merchant,
			Customer: cn.Customer,
			Currency: cn.Currency,
		}
		if base.Merchant.Name == "" {
			base.Merchant.Name = s.companyName(r.Context())
		}
	}

	invoiceNumber := ""
	if cn.InvoiceID != "" && s.pool != nil {
		if err := s.pool.QueryRow(r.Context(),
			`SELECT COALESCE(number, '') FROM invoices WHERE id = $1`, cn.InvoiceID).
			Scan(&invoiceNumber); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.cfg.Logger.ErrorContext(r.Context(), "read credit note invoice number", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
	}

	pdf, err := invoicepdf.Render(withBrand(invoicepdf.FromCreditNote(base, invoicepdf.CreditNoteData{
		Number:        cn.Number,
		InvoiceNumber: invoiceNumber,
		IssuedAt:      cn.IssuedAt,
		Currency:      cn.Currency,
		TotalMinor:    cn.TotalMinor,
		TaxMinor:      cn.TaxMinor,
		Reason:        cn.Reason,
		Status:        cn.Status,
	}), s.invoiceBrand(r.Context())))
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "render credit note pdf", "error", err)
		writeProblem(w, http.StatusInternalServerError, "render_failed",
			"the document could not be rendered")
		return
	}
	writePDF(w, invoicepdf.SafeFilename(cn.Number)+".pdf", pdf)
}

// withBrand attaches the tenant's brand to a document.
//
// It is a helper rather than two lines inline because the credit-note path builds its
// document inside a function call, and a brand that reached the invoice but not the credit
// note would be a document a customer receives unbranded and cannot explain.
func withBrand(doc invoicepdf.Document, b invoicepdf.Brand) invoicepdf.Document {
	doc.Brand = b
	return doc
}

// writePDF writes the bytes as a download.//
// The content type is exactly "application/pdf": a browser that receives
// "text/html" for a PDF renders nothing and offers no download, and a proxy that
// sniffs could serve the file as something executable. X-Content-Type-Options and
// a fixed disposition close both.
func writePDF(w http.ResponseWriter, filename string, body []byte) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// A document is derived from an immutable snapshot, but it also carries a status
	// and a void reason that change. No caching keeps a voided invoice from being
	// served from a browser cache as payable.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		// The client went away mid-download. Nothing to report to the client, and
		// nothing was persisted, so there is nothing to roll back.
		return
	}
}

// writeDocumentError maps a document read error to a response.
//
// staff distinguishes "no such record" from "not issued yet" because for an
// operator the difference is actionable: a draft can be issued, and a typo cannot.
// The portal deliberately does not: it answers 404 for both, so a customer cannot
// probe which identifiers exist.
func (s *Server) writeDocumentError(w http.ResponseWriter, r *http.Request, err error, staff bool) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if staff {
			// The row exists but is a draft, or does not exist. One more read
			// distinguishes them, and it is worth it for an operator.
			if s.documentExists(r.Context(), r.PathValue("id")) {
				writeProblem(w, http.StatusConflict, "not_issued",
					"this document has not been issued yet, so there is nothing to print")
				return
			}
		}
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, errDocumentUnavailable):
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"documents are unavailable")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "load invoice document",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// errDocumentUnavailable is returned when there is no database to read a document
// from, which is a deployment state rather than a client error.
var errDocumentUnavailable = errors.New("httpapi: document storage is unavailable")

// documentExists reports whether an invoice id is a real invoice.
func (s *Server) documentExists(ctx context.Context, id string) bool {
	if s.pool == nil || !portal.ValidID(id) {
		return false
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM invoices WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false
	}
	return exists
}
