package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// Offline payments over HTTP.
//
// The panel could refund, credit and void an invoice but had no way to record money arriving
// outside a card gateway, so a customer who pays by bank transfer could never have their
// invoice marked paid. This file is the missing direction: POST a payment against an invoice
// with an amount, a method from a closed set, a reference and an optional note.
//
// The handler is thin on purpose. The rules live in internal/payments (the closed method set,
// the reference as the payment's identity, the overpayment policy) and in internal/billing (the
// allocation, the ledger posting, the invoice transition). What belongs here is the HTTP policy:
//
//   - **Who may do it.** payment:reconcile, the same permission that reads the payment list and
//     asserts a refund. Deliberately *not* invoice:write:all: editing a document is not the same
//     authority as claiming money arrived, and the person who reconciles the bank statement is
//     not always the person who drafts invoices.
//   - **Idempotency.** The route is a staffMutation, so an Idempotency-Key is required and a
//     replay returns the stored response. The reference is a second, independent guard: even
//     after the stored response expires, the unique provider identity absorbs the retry.
//   - **The applier chain.** The payment is recorded through the same applier chain the webhook
//     uses, so a manual payment provisions what it bought and a top-up deposit becomes credit.
//   - **Audit, in the same transaction.** Who recorded it, against which invoice, with which
//     reference, and whether it was a replay.
//
// A note on what this endpoint is not: it does not call a provider, it does not create a checkout
// attempt, and it does not write a provider payment id that belongs to somebody else. The row is
// provider='manual' with the operator's reference, because that is what happened.

func init() {
	RegisterRoutes("37-manual-payments", registerManualPayments)
}

func registerManualPayments(r *router) {
	// The per-invoice spelling. The action belongs to the invoice — it is the invoice that
	// becomes paid — and putting the id in the path is what makes the permission, the audit
	// target and the idempotency digest all name the same record.
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/payments",
		authn.PermPaymentReconcile, r.s.handleRecordManualPayment)

	// The vocabulary, so the form offers the set the server accepts rather than a copy that
	// drifts. It is a read, and it is staff-only because recording an offline payment is.
	r.staffGet("/api/v1/staff/payment-methods/offline",
		authn.PermPaymentReconcile, r.s.handleOfflinePaymentMethods)
}

// manualPaymentRequest is the body of a recorded offline payment.
type manualPaymentRequest struct {
	// AmountMinor is what arrived, in the invoice's currency.
	AmountMinor int64 `json:"amount_minor"`
	// Method is one of bank_transfer, cash, cheque, other.
	Method string `json:"method"`
	// Reference is the bank reference, cheque number or receipt id. It is the payment's
	// identity, so it is required: without it a retry would record the money twice.
	Reference string `json:"reference"`
	// Note is optional free text for the human who reads the payment list later.
	Note string `json:"note"`
	// Currency is optional and, when present, must match the invoice.
	Currency string `json:"currency"`
}

// handleRecordManualPayment records money that arrived outside the gateway.
func (s *Server) handleRecordManualPayment(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}

	invoiceID := r.PathValue("id")
	if !portal.ValidID(invoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		return
	}

	var req manualPaymentRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}

	method, err := payments.ParseOfflineMethod(req.Method)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_method",
			"the method must be one of bank_transfer, cash, cheque or other")
		return
	}

	// A malformed currency is a client bug, not a payment; refusing it before the transaction
	// keeps the error about the request rather than about the money.
	if code := strings.TrimSpace(req.Currency); code != "" {
		if _, ok := money.LookupCurrency(code); !ok {
			writeProblem(w, http.StatusBadRequest, "invalid_currency", "unknown currency")
			return
		}
	}

	mp := payments.ManualPayment{
		InvoiceID:   invoiceID,
		AmountMinor: req.AmountMinor,
		Currency:    req.Currency,
		Method:      method,
		Reference:   req.Reference,
		Note:        strings.TrimSpace(req.Note),
	}
	// Validate before opening a transaction, so an incomplete body costs one round trip and
	// produces one precise code instead of a rollback.
	if err := mp.Validate(); err != nil {
		s.writeManualPaymentProblem(w, err)
		return
	}

	// The applier chain, not billing.ApplyPayment directly. Two things must happen after the
	// allocation and neither belongs in this handler:
	//
	//   - fulfilment enqueues provisioning for anything the paid invoice bought, so recording a
	//     bank transfer for a server actually delivers the server;
	//   - credit routing turns a settled *deposit* invoice into a balance, so a top-up paid by
	//     transfer credits the customer instead of leaving the deposit paid and the balance empty.
	//
	// Both are the same code the webhook uses. The notifier is attached for the same reason: a
	// customer whose transfer was recorded deserves the same "your invoice is paid" message as
	// one whose card settled, and the outbox write belongs in the settlement transaction.
	next := fulfilment.NewApplier(s.pool)
	if s.notify != nil {
		next = next.WithNotifier(s.notify)
	}
	applier := credit.NewDepositApplier(s.pool, next)

	user, _ := userFrom(r.Context())

	var res payments.ManualResult
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = payments.RecordManual(ctx, tx, applier, mp)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "payment.record", "payment", res.PaymentID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"invoice_id":      res.InvoiceID,
			"amount":          res.AmountMinor,
			"currency":        res.Currency,
			"method":          string(res.Method),
			"reference":       res.Reference,
			"note":            res.Note,
			"duplicate":       res.Duplicate,
			"recorded_by":     user.ID,
			"invoice_paid":    res.InvoicePaid,
			"invoice_total":   res.InvoiceTotalMinor,
			"invoice_paid_to": res.InvoicePaidMinor,
		}))
	})
	if err != nil {
		s.writeManualPaymentProblem(w, err)
		return
	}

	// Published after commit: an event inside the transaction would announce money that can
	// still roll back.
	if res.InvoicePaid && !res.Duplicate {
		s.publish(events.Event{
			Kind:       "invoice.paid",
			Permission: string(authn.PermInvoiceReadAll),
			Data:       map[string]any{"invoice_id": res.InvoiceID, "via": "manual"},
		})
	}

	cur := moneyFor(res.Currency)
	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_id":       res.PaymentID,
		"invoice_id":       res.InvoiceID,
		"provider":         payments.ManualProvider,
		"method":           string(res.Method),
		"method_label":     res.Method.Label(),
		"reference":        res.Reference,
		"note":             res.Note,
		"amount":           amountJSON(money.New(res.AmountMinor, cur)),
		"total":            amountJSON(money.New(res.InvoiceTotalMinor, cur)),
		"paid":             amountJSON(money.New(res.InvoicePaidMinor, cur)),
		"remaining":        amountJSON(money.New(res.RemainingMinor, cur)),
		"invoice_paid":     res.InvoicePaid,
		"already_recorded": res.Duplicate,
	})
}

// handleOfflinePaymentMethods lists the closed set the endpoint accepts.
//
// The frontend could hardcode four strings, and for four stable strings that is almost fine.
// It is served anyway so the label ("Bank transfer") and the value ("bank_transfer") are
// defined once, next to the CHECK constraint that enforces them.
func (s *Server) handleOfflinePaymentMethods(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, len(payments.OfflineMethods()))
	for _, m := range payments.OfflineMethods() {
		out = append(out, map[string]any{"value": string(m), "label": m.Label()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"methods": out})
}

// writeManualPaymentProblem maps a domain error to a problem+json response.
//
// The codes are the client's contract, so they are specific: an operator needs to know whether
// the amount was too large (fix the amount) or the reference was already used (find the existing
// payment), because those are different next steps.
func (s *Server) writeManualPaymentProblem(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, payments.ErrInvalidOfflineMethod):
		writeProblem(w, http.StatusBadRequest, "invalid_method",
			"the method must be one of bank_transfer, cash, cheque or other")
	case errors.Is(err, payments.ErrInvalidManualPayment):
		writeProblem(w, http.StatusBadRequest, "invalid_payment", err.Error())
	case errors.Is(err, billing.ErrOverpayment):
		// Refused, not converted to credit. See payments.RecordManual for why.
		writeProblem(w, http.StatusConflict, "overpayment", err.Error())
	case errors.Is(err, payments.ErrReferenceConflict):
		writeProblem(w, http.StatusConflict, "reference_conflict", err.Error())
	case errors.Is(err, billing.ErrPaymentReused):
		writeProblem(w, http.StatusConflict, "reference_reused", err.Error())
	case errors.Is(err, billing.ErrCurrencyMismatch):
		writeProblem(w, http.StatusBadRequest, "currency_mismatch",
			"the payment currency does not match the invoice")
	case errors.Is(err, billing.ErrInvoiceNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
	case errors.Is(err, billing.ErrInvalidInvoice):
		writeProblem(w, http.StatusBadRequest, "invalid_payment", err.Error())
	case errors.Is(err, billing.ErrInvoiceNotDraft):
		// A draft has not been issued, so there is nothing to pay yet. The sentence is the
		// operator's next step rather than the internal error name.
		writeProblem(w, http.StatusConflict, "not_issued",
			"this invoice is a draft; issue it before recording a payment")
	default:
		// An unmapped failure is a bug or an outage, not something the operator can fix by
		// changing the form. Log it so the cause is recoverable, and tell the client only that
		// it failed.
		s.cfg.Logger.Error("record manual payment failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the payment could not be recorded")
	}
}
