package httpapi

// The invoice surface the parity inventory still owed: editing a draft (INV-05), debit
// notes and the void of an adjustment note (INV-10, ADM-25), the transactions behind an
// invoice (ADM-24), a due-date change, and the configured number format read at issue time
// (INV-03).
//
// # The one rule everything here obeys
//
// An issued invoice is frozen by a database trigger (`0021_proforma.sql`). None of these
// handlers rewrites one, and none of them is a workaround for the trigger:
//
//   - the draft editor refuses a row that is not a draft, and the trigger is the backstop if
//     this code is ever wrong;
//   - a correction to an issued document is a credit note or a debit note, which is a second
//     numbered document that points at the first;
//   - a due date can be *extended* on an open document only when the installation has chosen
//     to allow the soft edits (INV-05, `invoices.immutable`), and never on a paid or void one.
//
// # Why the number format is read here
//
// `invoicepdf.LoadNumbering` already knew how to parse a configured prefix, padding and date
// tokens; what was missing was an issue site that used it. Reading it inside the issuing
// transaction means the format cannot change between the draft and the number, and the same
// helper is used by every path this package owns.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/creditnote"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("87-invoice-adjustments", registerInvoiceAdjustments)
}

func registerInvoiceAdjustments(r *router) {
	// Transactions are the payments, applications and adjustment notes that explain how an
	// invoice got from "issued" to "paid". Reading them is part of reading the invoice.
	r.staffGet("/api/v1/staff/invoices/{id}/transactions", authn.PermInvoiceReadAll,
		r.s.handleInvoiceTransactions)
	// The adjustment notes are listed separately because a note is a document, not a payment:
	// it has a number, a PDF and a lifecycle of its own.
	r.staffGet("/api/v1/staff/invoices/{id}/adjustment-notes", authn.PermInvoiceReadAll,
		r.s.handleInvoiceAdjustmentNotes)

	// A debit note charges more. It is a money mutation, so it needs an idempotency key like
	// every other one.
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/debit-notes",
		authn.PermInvoiceWriteAll, r.s.handleIssueDebitNote)
	// Voiding a note burns a number, exactly like voiding an invoice, so it is gated on
	// invoice:void rather than invoice:write:all.
	r.staffMutation(http.MethodPost, "/api/v1/staff/credit-notes/{id}/void",
		authn.PermInvoiceVoid, r.s.handleVoidAdjustmentNote)

	// A draft is a worksheet: its lines and its due date are editable until it has a number.
	r.staffMutation(http.MethodPut, "/api/v1/staff/invoices/{id}/lines",
		authn.PermInvoiceWriteAll, r.s.handleUpdateDraftLines)
	r.staffMutation(http.MethodPut, "/api/v1/staff/invoices/{id}/due-date",
		authn.PermInvoiceWriteAll, r.s.handleExtendDueDate)
}

// invoiceSeries resolves the configured number format inside the issuing transaction.
//
// A read failure is returned rather than swallowed: the caller is already inside a
// transaction that cannot commit if the database is unreachable, and falling back to the
// compiled-in series would number a document in a format the operator did not choose.
func (s *Server) invoiceSeries(ctx context.Context, tx pgx.Tx) (billing.NumberSeries, error) {
	numbering, err := invoicepdf.LoadNumbering(ctx, tx)
	if err != nil {
		return billing.NumberSeries{}, err
	}
	return numbering.InvoiceSeries(s.now())
}

// adjustSeries resolves the configured number format for an adjustment note.
//
// Credit and debit notes have their own series (INV-10, INV-18). They are read through the
// same loader as the invoice format so all three formats have one definition.
func (s *Server) adjustSeries(ctx context.Context, tx pgx.Tx, kind string) (creditnote.Series, error) {
	numbering, err := invoicepdf.LoadNumbering(ctx, tx)
	if err != nil {
		return creditnote.Series{}, err
	}
	fallback, err := creditnote.SeriesFor(kind)
	if err != nil {
		return creditnote.Series{}, err
	}
	var configured billing.NumberSeries
	if kind == creditnote.KindDebit {
		configured, err = numbering.DebitNoteSeries(s.now())
	} else {
		configured, err = numbering.CreditNoteSeries(s.now())
	}
	if err != nil {
		return creditnote.Series{}, err
	}
	// A pattern this package cannot express (an unknown placeholder) keeps the configured
	// series' scope but falls back to the default run; the caller has already validated the
	// pattern when it was saved, so this is defence in depth rather than a normal path.
	if configured.Scope == "" {
		return fallback, nil
	}
	return creditnote.Series{
		Scope:  configured.Scope,
		Prefix: configured.Prefix,
		Suffix: configured.Suffix,
		Width:  configured.Width,
	}, nil
}

// taxForNote returns the tax share of an adjustment.
//
// The invoice's snapshot records the rate that was actually charged, so the tax share of a
// note is that rate applied to the amount. This is not a new tax decision: it is splitting an
// amount that already contains tax. A request may state a tax share explicitly for the case
// where an operator is correcting a specific taxed line; otherwise the snapshot's rate is
// used, which is the answer that matches the document being corrected.
//
// The arithmetic is exact: a rational rate applied with the same rounding mode the invoice was
// priced with. An inclusive rate needs rate/(1+rate), which is computed in `big.Rat` so a
// repeating decimal never enters the document.
func taxForNote(snap billing.Snapshot, cur money.Currency, amountMinor, requested int64) (int64, error) {
	if requested > 0 {
		if requested > amountMinor {
			return 0, fmt.Errorf("%w: the tax share exceeds the amount", billing.ErrInvalidInvoice)
		}
		return requested, nil
	}
	rate := strings.TrimSpace(snap.TaxEvidence.Rate)
	if rate == "" || amountMinor <= 0 {
		return 0, nil
	}
	r, err := money.ParseRatio(rate)
	if err != nil || r.IsNegative() {
		return 0, nil
	}
	applied := r
	if strings.EqualFold(snap.TaxEvidence.Mode, "inclusive") && !r.IsZero() {
		denominator := new(big.Rat).Add(big.NewRat(1, 1), r.Rat())
		ratio, rerr := money.RatioFromRat(new(big.Rat).Quo(r.Rat(), denominator))
		if rerr != nil {
			return 0, nil
		}
		applied = ratio
	}
	share, aerr := money.ApplyRate(money.New(amountMinor, cur), applied, money.RoundHalfUp)
	if aerr != nil {
		return 0, aerr
	}
	return share.Minor(), nil
}

// ------------------------------------------------------------- transactions --

// handleInvoiceTransactions lists the money that moved against one invoice.
//
// It is a read, deliberately: ADM-24 asked for the invoice's transactions resource with
// create/edit/delete/refund, and the honest answer for this product is that a payment is
// immutable. What an operator needs on this screen is the list with its refunds and the
// ability to start a refund, which exists at POST /staff/payments/{id}/refunds. Editing or
// deleting a payment would rewrite the subledger, and the ledger's whole design is that
// corrections are reversals.
func (s *Server) handleInvoiceTransactions(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var exists bool
	if err := s.pool.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM invoices WHERE id = $1)`, id).Scan(&exists); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "check invoice for transactions", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	if !exists {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	out := make([]map[string]any, 0, 8)

	// Payments allocated to this invoice, with what has already been refunded.
	rows, err := s.pool.Query(r.Context(), `
		SELECT p.id, a.amount_minor, p.currency, p.received_at, p.status,
		       p.provider, p.provider_payment_id, COALESCE(p.method, ''), COALESCE(p.note, ''),
		       COALESCE((
		           SELECT SUM(r.amount_minor) FROM refunds r
		           WHERE r.payment_id = p.id AND r.status IN ('created', 'pending', 'succeeded')
		       ), 0)
		FROM payment_allocations a
		JOIN payments p ON p.id = a.payment_id
		WHERE a.invoice_id = $1
		ORDER BY p.received_at, p.id`, id)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "list invoice payments", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	for rows.Next() {
		var (
			txID, currency, status, provider, providerID, method, note string
			amountMinor, refundedMinor                                 int64
			receivedAt                                                 time.Time
		)
		if err := rows.Scan(&txID, &amountMinor, &currency, &receivedAt, &status,
			&provider, &providerID, &method, &note, &refundedMinor); err != nil {
			rows.Close()
			s.cfg.Logger.ErrorContext(r.Context(), "scan invoice payment", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, map[string]any{
			"id":          txID,
			"kind":        "payment",
			"amount":      amountJSON(money.New(amountMinor, moneyFor(currency))),
			"occurred_at": receivedAt.UTC(),
			"status":      status,
			"provider":    provider,
			"reference":   providerID,
			"method":      method,
			"note":        note,
			"refundable":  status != "refunded" && refundedMinor < amountMinor,
			"refunded":    amountJSON(money.New(refundedMinor, moneyFor(currency))),
			"refund_url":  "/api/v1/staff/payments/" + txID + "/refunds",
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.cfg.Logger.ErrorContext(r.Context(), "iterate invoice payments", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	rows.Close()

	// Credit applied to this invoice. It is its own kind because it is not a payment: no
	// gateway took the money, the customer's own balance did.
	apps, err := s.pool.Query(r.Context(), `
		SELECT id, amount_minor, currency, created_at FROM credit_applications
		WHERE invoice_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "list credit applications", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	for apps.Next() {
		var (
			txID, currency string
			amountMinor    int64
			createdAt      time.Time
		)
		if err := apps.Scan(&txID, &amountMinor, &currency, &createdAt); err != nil {
			apps.Close()
			s.cfg.Logger.ErrorContext(r.Context(), "scan credit application", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, map[string]any{
			"id":          txID,
			"kind":        "credit_application",
			"amount":      amountJSON(money.New(amountMinor, moneyFor(currency))),
			"occurred_at": createdAt.UTC(),
			"status":      "applied",
		})
	}
	if err := apps.Err(); err != nil {
		apps.Close()
		s.cfg.Logger.ErrorContext(r.Context(), "iterate credit applications", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	apps.Close()

	// Adjustment notes are money too, in the direction their kind says.
	notes, err := s.pool.Query(r.Context(), `
		SELECT id, COALESCE(number, ''), kind, total_minor, currency, status, issued_at
		FROM credit_notes WHERE invoice_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "list invoice adjustment notes", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	for notes.Next() {
		var (
			txID, number, kind, currency, status string
			amountMinor                          int64
			issuedAt                             *time.Time
		)
		if err := notes.Scan(&txID, &number, &kind, &amountMinor, &currency, &status, &issuedAt); err != nil {
			notes.Close()
			s.cfg.Logger.ErrorContext(r.Context(), "scan adjustment note", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		signed := amountMinor
		if kind == creditnote.KindCredit {
			signed = -amountMinor
		}
		out = append(out, map[string]any{
			"id":          txID,
			"kind":        "adjustment_note",
			"adjustment":  kind,
			"number":      number,
			"amount":      amountJSON(money.New(signed, moneyFor(currency))),
			"occurred_at": timeOrNil(issuedAt),
			"status":      status,
		})
	}
	if err := notes.Err(); err != nil {
		notes.Close()
		s.cfg.Logger.ErrorContext(r.Context(), "iterate adjustment notes", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	notes.Close()

	// Newest first is what a screen wants; the SQL above orders deterministically within a
	// source so a re-read does not shuffle identical timestamps.
	sort.SliceStable(out, func(i, j int) bool {
		return occurredAt(out[i]).After(occurredAt(out[j]))
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"invoice_id":   id,
		"transactions": out,
	})
}

// occurredAt reads the timestamp back out of a transaction map.
func occurredAt(t map[string]any) time.Time {
	switch v := t["occurred_at"].(type) {
	case time.Time:
		return v
	default:
		return time.Time{}
	}
}

// --------------------------------------------------------- adjustment notes --

// handleInvoiceAdjustmentNotes lists the credit and debit notes against one invoice.
func (s *Server) handleInvoiceAdjustmentNotes(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	notes, _, err := creditnote.New(s.pool).List(r.Context(), id, 100, "")
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		out = append(out, map[string]any{
			"id":          n.ID,
			"number":      n.Number,
			"kind":        n.Kind,
			"status":      n.Status,
			"amount":      amountJSON(money.New(n.TotalMinor, moneyFor(n.Currency))),
			"tax":         amountJSON(money.New(n.TaxMinor, moneyFor(n.Currency))),
			"reason":      n.Reason,
			"issued_at":   timeOrNil(n.IssuedAt),
			"void_reason": n.VoidReason,
			"voided_at":   timeOrNil(n.VoidedAt),
			"voidable":    n.Status == creditnote.StatusIssued,
			"pdf_url":     "/api/v1/staff/credit-notes/" + n.ID + "/pdf",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": id, "notes": out})
}

// handleIssueDebitNote charges more against an invoice.
//
// This is the counterpart of the credit note, and the reason it is a separate document rather
// than an edit: an issued invoice is evidence, so a shop that under-charged issues a debit
// note that says so. The invoice's frozen total does not move.
func (s *Server) handleIssueDebitNote(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		TaxMinor    int64  `json:"tax_minor"`
		Reason      string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount must be positive")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "a debit note needs a reason")
		return
	}
	user, _ := userFrom(r.Context())

	var (
		series   creditnote.Series
		taxMinor int64
		currency string
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// The tax share is derived from the corrected invoice's own snapshot rather than from
		// today's tax table: the note corrects that document, so it carries that document's
		// rate. This read is a pre-flight check so a bad number is a 400 rather than a
		// half-written note; creditnote.Issue re-reads and locks the invoice in its own
		// transaction, which is where the authoritative status check happens.
		var snap billing.Snapshot
		if err := tx.QueryRow(r.Context(), `
			SELECT snapshot, currency FROM invoices
			WHERE id = $1 AND status NOT IN ('draft', 'void')`,
			id).Scan(&snap, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDocumentNotFound
			}
			return err
		}
		tax, terr := taxForNote(snap, moneyFor(currency), req.AmountMinor, req.TaxMinor)
		if terr != nil {
			return terr
		}
		taxMinor = tax
		resolved, serr := s.adjustSeries(ctx, tx, creditnote.KindDebit)
		if serr != nil {
			return serr
		}
		series = resolved
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errDocumentNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		case errors.Is(err, billing.ErrInvalidInvoice):
			writeProblem(w, http.StatusBadRequest, "invalid_amount", err.Error())
		default:
			s.staffError(w, r, err)
		}
		return
	}

	note, err := creditnote.New(s.pool).WithAuditor(s.auditorFor(r, reason)).
		Issue(r.Context(), creditnote.IssueRequest{
			InvoiceID:   id,
			AmountMinor: req.AmountMinor,
			TaxMinor:    taxMinor,
			Reason:      reason,
			IssuedBy:    user.ID,
			Kind:        creditnote.KindDebit,
			Series:      &series,
		})
	switch {
	case err == nil:
	case errors.Is(err, creditnote.ErrNotIssuable):
		writeProblem(w, http.StatusConflict, "not_issuable", err.Error())
		return
	case errors.Is(err, creditnote.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{
		Kind:       "debit_note.issued",
		Permission: string(authn.PermInvoiceReadAll),
		Data:       map[string]any{"credit_note_id": note.ID, "invoice_id": id},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      note.ID,
		"number":  note.Number,
		"kind":    note.Kind,
		"status":  note.Status,
		"amount":  amountJSON(money.New(note.TotalMinor, moneyFor(note.Currency))),
		"tax":     amountJSON(money.New(note.TaxMinor, moneyFor(note.Currency))),
		"reason":  note.Reason,
		"pdf_url": "/api/v1/staff/credit-notes/" + note.ID + "/pdf",
	})
}

// handleVoidAdjustmentNote cancels a credit or debit note.
func (s *Server) handleVoidAdjustmentNote(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "voiding a note needs a reason")
		return
	}
	if len(reason) > 255 {
		writeProblem(w, http.StatusBadRequest, "reason_too_long",
			"the reason is longer than the limit")
		return
	}
	user, _ := userFrom(r.Context())

	note, err := creditnote.New(s.pool).WithAuditor(s.auditorFor(r, reason)).
		Void(r.Context(), creditnote.VoidRequest{ID: id, Reason: reason, VoidedBy: user.ID})
	switch {
	case err == nil:
	case errors.Is(err, creditnote.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, creditnote.ErrAlreadyVoid):
		writeProblem(w, http.StatusConflict, "already_void", "this note is already void")
		return
	case errors.Is(err, creditnote.ErrNotVoidable):
		writeProblem(w, http.StatusConflict, "not_voidable",
			"a note that has already been applied cannot be voided; issue a debit note instead")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{
		Kind:       "credit_note.void",
		Permission: string(authn.PermInvoiceReadAll),
		Data:       map[string]any{"credit_note_id": note.ID, "invoice_id": note.InvoiceID},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          note.ID,
		"number":      note.Number,
		"kind":        note.Kind,
		"status":      note.Status,
		"void_reason": note.VoidReason,
		"voided_at":   timeOrNil(note.VoidedAt),
	})
}

// --------------------------------------------------------------- draft edit --

// handleUpdateDraftLines rewrites a draft invoice's lines (INV-05).
//
// The request is the complete document, not a patch: a draft line has no identity a client
// can safely address, and "here is the invoice" is the operation an operator is performing.
// The pricing runs through the same issuer path as creation, so the preview the screen showed
// and the totals stored here cannot disagree.
func (s *Server) handleUpdateDraftLines(w http.ResponseWriter, r *http.Request) {
	if s.issuer == nil || s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "issuer_unavailable",
			"invoice issuance is not configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req createInvoiceRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if len(req.Lines) == 0 {
		writeProblem(w, http.StatusBadRequest, "no_lines", "an invoice needs at least one line")
		return
	}
	currency := req.Lines[0].Currency
	for _, line := range req.Lines {
		switch {
		case line.Currency != currency:
			writeProblem(w, http.StatusBadRequest, "mixed_currency",
				"all lines of an invoice must use the same currency")
			return
		case line.Quantity <= 0:
			writeProblem(w, http.StatusBadRequest, "invalid_quantity",
				"quantity must be positive")
			return
		case line.Description == "":
			writeProblem(w, http.StatusBadRequest, "invalid_line",
				"every line needs a description")
			return
		}
	}
	cur, err := staffCurrency(currency)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", "unknown currency")
		return
	}
	lines := make([]billing.Line, 0, len(req.Lines))
	for _, line := range req.Lines {
		var productID, versionID *string
		if line.ProductID != "" {
			if !portal.ValidID(line.ProductID) {
				writeProblem(w, http.StatusNotFound, "not_found", "no such product")
				return
			}
			productID = &line.ProductID
		}
		if line.PriceVersionID != "" {
			if !portal.ValidID(line.PriceVersionID) {
				writeProblem(w, http.StatusNotFound, "not_found", "no such price version")
				return
			}
			versionID = &line.PriceVersionID
		}
		lines = append(lines, billing.Line{
			Description:    line.Description,
			QuantityMilli:  int64(line.Quantity) * 1000,
			UnitPrice:      money.New(line.UnitAmount, cur),
			ProductID:      productID,
			PriceVersionID: versionID,
		})
	}

	var (
		totals billing.Totals
		snap   billing.Snapshot
	)
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// The customer is re-read rather than trusted from the body: the billed party is the
		// account the draft belongs to, and letting a request replace it would be a way to
		// move a document to another customer without a credit note.
		var customerID string
		if err := tx.QueryRow(r.Context(),
			`SELECT customer_id FROM invoices WHERE id = $1`, id).Scan(&customerID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDocumentNotFound
			}
			return err
		}
		customer := billing.CustomerDetails{ID: customerID}
		if err := billing.FillCustomer(ctx, tx, &customer); err != nil {
			return err
		}
		// A due date is only replaced when the request asked for one: "edit the lines" must not
		// quietly move the payment term of a document already being negotiated.
		var dueAt *time.Time
		if req.DueInDays > 0 {
			dueAt = dueAtFromRequest(req.DueInDays, s.now(), settings.InvoiceDueDays(ctx, tx))
		}
		updated, updatedSnap, uerr := s.issuer.UpdateDraft(ctx, tx, id, billing.DraftUpdate{
			Lines:    lines,
			Customer: customer,
			Terms:    s.terms,
			Merchant: s.merchant,
			DueAt:    dueAt,
		})
		if uerr != nil {
			return uerr
		}
		totals, snap = updated, updatedSnap
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.draft_update", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"lines": len(lines),
			"total": totals.Total.Minor(),
		}))
	})
	switch {
	case err == nil:
	case errors.Is(err, errDocumentNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, billing.ErrInvoiceNotDraft):
		writeProblem(w, http.StatusConflict, "not_draft",
			"only a draft invoice can be edited; correct an issued document with a credit note")
		return
	case errors.Is(err, billing.ErrInvalidInvoice), errors.Is(err, billing.ErrCurrencyMismatch):
		writeProblem(w, http.StatusBadRequest, "invalid_invoice", err.Error())
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.updated", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": "draft",
		"totals": map[string]any{
			"subtotal": amountJSON(totals.Subtotal),
			"tax":      amountJSON(totals.Tax),
			"total":    amountJSON(totals.Total),
		},
		"currency": snap.Currency,
	})
}

// dueAtFromRequest resolves an optional due-in-days override.
func dueAtFromRequest(days int, now time.Time, configured int) *time.Time {
	if days <= 0 {
		days = configured
	}
	t := now.AddDate(0, 0, days)
	return &t
}

// ---------------------------------------------------------------- due date --

// handleExtendDueDate moves an invoice's due date later.
//
// The database leaves `due_at` writable after issue, and dunning depends on it, so an
// operator negotiating terms needs to move it. Two limits keep that from being a way to make a
// document disagree with itself: a date can only move later (moving it earlier would make an
// invoice retroactively overdue and restart reminders), and when `invoices.immutable` is on —
// the default — an issued document may not move at all. That setting is INV-05's
// "optional immutable invoices": the soft edits exist for an installation that wants them.
func (s *Server) handleExtendDueDate(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req struct {
		DueAt     *time.Time `json:"due_at"`
		DueInDays int        `json:"due_in_days"`
	}
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}

	var (
		previous *time.Time
		next     time.Time
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var status string
		var paid int64
		if err := tx.QueryRow(r.Context(), `
			SELECT status, amount_paid_minor, due_at FROM invoices WHERE id = $1 FOR UPDATE`,
			id).Scan(&status, &paid, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDocumentNotFound
			}
			return err
		}
		if status == "paid" || status == "void" || status == "refunded" || paid > 0 {
			return errImmutableDocument
		}
		if status != "draft" && settings.InvoiceImmutable(ctx, tx) {
			return errImmutableDocument
		}
		switch {
		case req.DueAt != nil:
			next = req.DueAt.UTC()
		case req.DueInDays > 0:
			next = s.now().AddDate(0, 0, req.DueInDays)
		default:
			return errDueDateRequired
		}
		if previous != nil && !next.After(*previous) {
			return errDueDateEarlier
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE invoices SET due_at = $2 WHERE id = $1`, id, next); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.due_date", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"before": previous,
			"after":  next,
		}))
	})
	switch {
	case err == nil:
	case errors.Is(err, errDocumentNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, errImmutableDocument):
		writeProblem(w, http.StatusConflict, "invoice_immutable",
			"this document's due date cannot change; issue a credit note and a new invoice")
		return
	case errors.Is(err, errDueDateRequired):
		writeProblem(w, http.StatusBadRequest, "due_date_required",
			"send due_at or due_in_days")
		return
	case errors.Is(err, errDueDateEarlier):
		writeProblem(w, http.StatusConflict, "due_date_earlier",
			"a due date can only move later")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "due_at": next.UTC()})
}

var (
	errImmutableDocument = errors.New("httpapi: document is immutable")
	errDueDateRequired   = errors.New("httpapi: due date required")
	errDueDateEarlier    = errors.New("httpapi: due date moves earlier")
)

// issueMailsCustomer resolves the per-issue "send email" toggle (INV-14).
//
// A nil requested value means "the installation default", which is what every client that has
// never heard of the setting sends. The setting itself is read inside the transaction so the
// decision and the document commit together.
func (s *Server) issueMailsCustomer(ctx context.Context, tx pgx.Tx, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	return settings.EmailOnIssue(ctx, tx)
}

// suppressInvoiceNotification records that the operator chose not to mail the customer.
//
// A suppression row rather than silence, using the same dedupe key as the send would: "we did
// not send it because the toggle was off" is an answer a support agent can give, and if the
// mail had already gone out the row is a no-op rather than a contradiction.
func (s *Server) suppressInvoiceNotification(ctx context.Context, tx pgx.Tx, invoiceID, number string) error {
	if s.notify == nil {
		return nil
	}
	var email string
	if err := tx.QueryRow(ctx, `
		SELECT c.email FROM invoices i JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1`, invoiceID).Scan(&email); err != nil {
		return fmt.Errorf("staff: read invoice customer: %w", err)
	}
	return s.notify.Suppress(ctx, tx,
		notify.InvoiceIssuedMessage(email, number, "", "", invoiceID), "send_email was off")
}
