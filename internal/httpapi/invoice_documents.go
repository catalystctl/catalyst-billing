package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

// Invoice lifecycle mutations that the document surface needs (INV-01, INV-04,
// INV-07, INV-09).
//
//   - A client-visible note (INV-07): free text an operator adds to a document the
//     customer will read, and which is printed only when
//     `invoices.notes_client_visible` is on.
//   - Void with a reason (INV-09): the existing void path already required a reason
//     and recorded it in the audit log, but the invoice itself carried nothing, so a
//     customer downloading the document after it was cancelled saw an ordinary
//     invoice. `invoices.void_reason` now carries it onto the page.
//   - Proforma (INV-04): issue a provisional document, and convert it to a real
//     invoice when it is paid.

func init() {
	RegisterRoutes("86-invoice-lifecycle", registerInvoiceLifecycle)
}

func registerInvoiceLifecycle(r *router) {
	// A note is an operator's words on a customer's document, so writing one is
	// invoice:write:all. Reading it back is part of the invoice read.
	r.staffMutation(http.MethodPut, "/api/v1/staff/invoices/{id}/client-note",
		authn.PermInvoiceWriteAll, r.s.handleSetClientNote)

	// The customer's view of the same facts: the note an operator wrote for them,
	// whether the document is a proforma, why it was voided, and the credit notes
	// issued against it. Without this read a customer is sent a credit note they
	// cannot open — the PDF route exists, but nothing tells them the note exists or
	// what its id is. It is scoped by `customer_accounts.user_id` in the SQL, so a
	// substituted invoice id selects no row.
	r.get("/api/v1/portal/invoices/{id}/document", authn.PermInvoiceReadSelf,
		r.s.handlePortalInvoiceDocument)

	// Voiding is gated on invoice:void, which is deliberately separate from
	// invoice:write:all: a void burns a number.
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/void-reason",
		authn.PermInvoiceVoid, r.s.handleVoidWithReason)

	// A proforma is an issued document, so issuing one is issuance.
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/proforma",
		authn.PermInvoiceWriteAll, r.s.handleIssueProforma)
}

// ---------------------------------------------------------------- client note --

// maxClientNoteLen bounds the note. It is printed on a page, so a note long enough
// to push the totals off it is not a note.
const maxClientNoteLen = 2000

// handleSetClientNote sets or clears `invoices.client_note`.
//
// The note is stored even when client notes are disabled installation-wide: the
// setting controls what a *customer* sees, not what an operator may record. A note
// written while the setting is off becomes visible when it is turned on, which is
// what an operator who wrote it would expect.
func (s *Server) handleSetClientNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		Note string `json:"note"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > maxClientNoteLen {
		writeProblem(w, http.StatusBadRequest, "note_too_long",
			"the note is longer than the limit")
		return
	}

	var previous string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status, client_note FROM invoices WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDocumentNotFound
			}
			return err
		}
		// A note on a void document would be printed next to the void banner and
		// read as a correction to it. Refusing keeps the void statement the last
		// word on the page.
		if status == "void" {
			return errInvoiceVoid
		}
		// INV-05: an issued document is immutable by default. The database trigger
		// deliberately leaves `client_note` writable, because a note is an annotation
		// rather than a money fact — but a customer holding a PDF must not be able to
		// be shown a different annotation later. An installation that wants the soft
		// edit sets `invoices.immutable` off, and then this is allowed.
		if status != "draft" && settings.InvoiceImmutable(ctx, tx) {
			return errDocumentImmutable
		}
		if _, err := tx.Exec(ctx,
			`UPDATE invoices SET client_note = $2 WHERE id = $1`, id, note); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.client_note", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"before": previous,
			"after":  note,
		}))
	})
	switch {
	case err == nil:
	case errors.Is(err, errDocumentNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, errInvoiceVoid):
		writeProblem(w, http.StatusConflict, "invoice_void",
			"a void document cannot carry a client note")
		return
	case errors.Is(err, errDocumentImmutable):
		writeProblem(w, http.StatusConflict, "invoice_immutable",
			"an issued invoice cannot be annotated; turn invoices.immutable off to allow it")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.client_note", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          id,
		"client_note": note,
	})
}

var (
	errDocumentNotFound  = errors.New("httpapi: document not found")
	errInvoiceVoid       = errors.New("httpapi: invoice is void")
	errDocumentImmutable = errors.New("httpapi: document is immutable")
)

// ------------------------------------------------ document state on the reads --

// invoiceDocumentState is what a screen needs about a document beyond its money:
// whether it is provisional, what the operator wrote for the customer, and why it
// was voided.
//
// These columns are printed on the document (`invoicepdf.Meta`) but were absent from
// the staff invoice payload, so the screen could not say "this is a proforma", could
// not show the note it had just written, and could not explain a void.
type invoiceDocumentState struct {
	Proforma   bool
	ClientNote string
	VoidReason string
}

// loadInvoiceDocumentState reads the document columns for one invoice.
func (s *Server) loadInvoiceDocumentState(ctx context.Context, invoiceID string) (invoiceDocumentState, error) {
	var out invoiceDocumentState
	err := s.pool.QueryRow(ctx, `
		SELECT proforma, COALESCE(client_note, ''), COALESCE(void_reason, '')
		FROM invoices WHERE id = $1`, invoiceID).
		Scan(&out.Proforma, &out.ClientNote, &out.VoidReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return invoiceDocumentState{}, errDocumentNotFound
	}
	if err != nil {
		return invoiceDocumentState{}, fmt.Errorf("httpapi: read invoice document state: %w", err)
	}
	return out, nil
}

// handlePortalInvoiceDocument serves the customer's own view of a document's
// lifecycle facts.
//
// Three rules, the same three the PDF handlers follow:
//
//  1. The ownership predicate is the join, not a later comparison, so another
//     customer's invoice selects no row and answers 404 byte-identically to an id
//     that never existed.
//  2. A draft has no document, so it is 404 for the same reason its PDF is: there is
//     nothing issued to describe.
//  3. The client note is included only when `invoices.notes_client_visible` is on. The
//     note is stored either way — the setting decides what a customer reads, and a
//     read that ignored it would show a note the document does not print.
func (s *Server) handlePortalInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in to read this document")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var state invoiceDocumentState
	var status string
	err := s.pool.QueryRow(r.Context(), `
		SELECT i.status, i.proforma, COALESCE(i.client_note, ''), COALESCE(i.void_reason, '')
		FROM invoices i
		JOIN customer_accounts ca ON ca.customer_id = i.customer_id
		WHERE i.id = $1 AND ca.user_id = $2 AND i.status <> 'draft'`, id, user.ID).
		Scan(&status, &state.Proforma, &state.ClientNote, &state.VoidReason)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read portal invoice document", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
		return
	}

	notes, err := s.portalInvoiceCreditNotes(r.Context(), user.ID, id)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read portal invoice credit notes", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
		return
	}

	note := state.ClientNote
	visible := s.clientNotesVisible(r.Context())
	if !visible {
		note = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"status":        status,
		"proforma":      state.Proforma,
		"client_note":   note,
		"notes_visible": visible,
		"void_reason":   state.VoidReason,
		"credit_notes":  notes,
	})
}

// portalInvoiceCreditNotes lists the credit notes issued against one of the caller's
// invoices, so the portal can offer the download it serves.
//
// A credit note is a document in its own right, so this is a list of documents rather
// than of amounts: the number is what the customer quotes, and the id is what the PDF
// route needs. The ownership predicate is again the join.
func (s *Server) portalInvoiceCreditNotes(ctx context.Context, userID, invoiceID string) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cn.id, COALESCE(cn.number, ''), cn.currency, cn.total_minor, cn.tax_minor,
		       cn.reason, cn.status, cn.kind, COALESCE(cn.void_reason, ''), cn.issued_at
		FROM credit_notes cn
		JOIN customer_accounts ca ON ca.customer_id = cn.customer_id
		WHERE cn.invoice_id = $1 AND ca.user_id = $2
		ORDER BY cn.created_at DESC, cn.id DESC`, invoiceID, userID)
	if err != nil {
		return nil, fmt.Errorf("httpapi: list invoice credit notes: %w", err)
	}
	defer rows.Close()

	out := make([]map[string]any, 0, 4)
	for rows.Next() {
		var (
			id, number, currency, reason, status, kind, voidReason string
			totalMinor, taxMinor                                   int64
			issuedAt                                               *time.Time
		)
		if err := rows.Scan(&id, &number, &currency, &totalMinor, &taxMinor,
			&reason, &status, &kind, &voidReason, &issuedAt); err != nil {
			return nil, fmt.Errorf("httpapi: scan invoice credit note: %w", err)
		}
		out = append(out, map[string]any{
			"id":          id,
			"number":      number,
			"kind":        kind,
			"total":       amountJSON(money.New(totalMinor, moneyFor(currency))),
			"tax":         amountJSON(money.New(taxMinor, moneyFor(currency))),
			"reason":      reason,
			"status":      status,
			"void_reason": voidReason,
			"issued_at":   timeOrNil(issuedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: iterate invoice credit notes: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------- void reason --

// handleVoidWithReason voids an invoice and records why.
//
// This is the same transition as POST /void (which already required a reason for the
// audit log) with the reason additionally stored on the row, so the reason travels
// with the document instead of only with the operator's audit trail. A customer who
// downloads a voided invoice now reads the explanation on it.
//
// A paid invoice still cannot be voided: the money moved, and the correction is a
// credit note. The check is against `amount_paid_minor` as well as the status,
// because a partially paid invoice is just as impossible to erase.
func (s *Server) handleVoidWithReason(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "voiding needs a reason")
		return
	}
	if len(reason) > 255 {
		// The column is free text, but a reason that fills a page is a note. The
		// limit matches service_cancellations.reason, so one operator convention
		// covers both.
		writeProblem(w, http.StatusBadRequest, "reason_too_long",
			"the reason is longer than the limit")
		return
	}

	var previous string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var (
			status string
			paid   int64
		)
		if err := tx.QueryRow(ctx,
			`SELECT status, amount_paid_minor FROM invoices WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &paid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDocumentNotFound
			}
			return err
		}
		if status == "paid" || paid > 0 {
			return errNotVoidable
		}
		if status == "void" {
			return errAlreadyVoid
		}
		previous = status

		// One statement sets both the status and the reason, so a reader can never
		// see a void invoice whose reason has not been written yet. The 0008 trigger
		// validates the transition itself.
		if _, err := tx.Exec(ctx,
			`UPDATE invoices SET status = 'void', void_reason = $2 WHERE id = $1`,
			id, reason); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.void", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"reason":      reason,
			"previous":    previous,
			"amount_paid": paid,
		}))
	})
	switch {
	case err == nil:
	case errors.Is(err, errDocumentNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, errNotVoidable):
		writeProblem(w, http.StatusConflict, "not_voidable",
			"a paid invoice cannot be voided; issue a credit note instead")
		return
	case errors.Is(err, errAlreadyVoid):
		writeProblem(w, http.StatusConflict, "already_void", "this invoice is already void")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.void", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": id, "reason": reason}})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          id,
		"status":      "void",
		"void_reason": reason,
	})
}

// ----------------------------------------------------------------- proforma --

// handleIssueProforma issues a draft as a provisional, unnumbered document.
func (s *Server) handleIssueProforma(w http.ResponseWriter, r *http.Request) {
	if s.issuer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "issuer_unavailable",
			"invoice issuance is not configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		DueInDays int `json:"due_in_days"`
	}
	// An empty body is the common case: the due date defaults to the installation's
	// terms, and a client that sends nothing should not be refused. ContentLength is
	// -1 for a chunked request, which may still carry a body, so only a definite
	// zero skips decoding.
	if r.ContentLength != 0 {
		if !s.decodeJSON(w, r, &req, 4<<10) {
			return
		}
	}
	dueInDays := req.DueInDays
	if dueInDays <= 0 {
		dueInDays = s.dueDays(r.Context())
	}

	dueAt := s.now().AddDate(0, 0, dueInDays)
	var state invoicepdf.ProformaState
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := invoicepdf.IssueProforma(ctx, tx, id, dueAt); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, s.auditEvent(r, "invoice.proforma_issue", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"due_at": dueAt.UTC(),
		})); err != nil {
			return err
		}
		read, err := invoicepdf.ReadProformaState(ctx, tx, id)
		if err != nil {
			return err
		}
		state = read
		return nil
	})
	switch {
	case err == nil:
	case errors.Is(err, invoicepdf.ErrProformaNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, invoicepdf.ErrProformaNumbered):
		writeProblem(w, http.StatusConflict, "already_issued",
			"this invoice already has a number")
		return
	case errors.Is(err, billing.ErrInvoiceNotDraft):
		writeProblem(w, http.StatusConflict, "not_draft",
			"only a draft invoice can be issued")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.proforma_issued", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"status":   state.Status,
		"proforma": state.Proforma,
		// The number is explicitly null: the absence is the point of a proforma,
		// and a client that reads the field must see that it is absent rather than
		// missing.
		"number": nil,
		"due_at": dueAt.UTC(),
	})
}

// dueDays reads the configured payment terms (INV-17).
//
// The parse and the fallback live in the settings package so the manual create path, the
// checkout path and this one cannot disagree about what "7 days" means. Seven is both
// Paymenter's default and the interval billing.Issuer.Issue applies when an invoice has no due
// date.
func (s *Server) dueDays(ctx context.Context) int {
	return settings.InvoiceDueDays(ctx, s.pool)
}

// clientNotesVisible reports whether client-visible notes are enabled.
//
// The default is *on* when the setting is absent, matching the seeded value: an
// operator who has never opened the settings page should see the note they typed
// appear on the document, not silently disappear.
func (s *Server) clientNotesVisible(ctx context.Context) bool {
	var raw string
	err := s.pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'invoices' AND key = 'notes_client_visible'`).
		Scan(&raw)
	if err != nil {
		return true
	}
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// companyName reads the merchant name from settings, used for a goodwill credit
// note that has no invoice to inherit a letterhead from.
func (s *Server) companyName(ctx context.Context) string {
	if s.merchant.Name != "" {
		return s.merchant.Name
	}
	var raw string
	if err := s.pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'general' AND key = 'company_name'`).
		Scan(&raw); err != nil {
		return ""
	}
	return raw
}
