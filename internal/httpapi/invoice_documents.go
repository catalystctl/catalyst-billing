package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/portal"
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
	errDocumentNotFound = errors.New("httpapi: document not found")
	errInvoiceVoid      = errors.New("httpapi: invoice is void")
)

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

// dueDays reads the configured payment terms.
//
// It falls back to seven days, which is both Paymenter's default and the interval
// billing.Issuer.Issue applies when an invoice has no due date.
func (s *Server) dueDays(ctx context.Context) int {
	const fallback = 7
	var raw string
	if err := s.pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'invoices' AND key = 'due_days'`).
		Scan(&raw); err != nil {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 || n > 3650 {
		return fallback
	}
	return n
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
