package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/creditnote"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/refunds"
	"github.com/jackc/pgx/v5"
)

// Money actions over HTTP (Gate D1-D3).
//
// The handlers are thin because the hard parts live in internal/refunds and internal/creditnote: the
// row locks, the intent-then-call sequence, the idempotent postings. What belongs here is the policy
// that is genuinely about the request:
//
//   - Who may do it (a permission, plus the approval for a large amount).
//   - What the client is told when the outcome is unknown (202, not 500, because the refund may have
//     happened).
//   - What is recorded about who asked (an audit row in the same transaction as the domain change
//     where possible).

func init() {
	RegisterRoutes("75-money", registerMoney)
}

func registerMoney(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/payments/{id}/refunds",
		authn.PermPaymentRefund, r.s.handleCreateRefund)
	r.staffGet("/api/v1/staff/refunds/{id}", authn.PermPaymentRefund, r.s.handleGetRefund)

	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/credit-notes",
		authn.PermInvoiceWriteAll, r.s.handleIssueCreditNote)
	r.staffMutation(http.MethodPost, "/api/v1/staff/customers/{id}/credit/apply",
		authn.PermInvoiceWriteAll, r.s.handleApplyCredit)
}

// refundGateway adapts the configured checkout gateway to the refund interface.
//
// A gateway that cannot refund is not an error at startup: the refund endpoint reports that the
// provider does not support refunds, and every other screen keeps working. The assertion happens
// per request so a gateway added later is picked up without a restart.
func (s *Server) refundService() *refunds.Service {
	if s.gateway == nil || s.pool == nil {
		return nil
	}
	gw, ok := s.gateway.(refunds.Gateway)
	if !ok {
		return nil
	}
	threshold := approvals.DefaultRefundThresholdMinor
	if s.approvals != nil {
		threshold = s.approvals.RefundThresholdMinor
	}
	return refunds.New(s.pool, gw, s.provider, s.account, threshold)
}

// handleCreateRefund refunds part or all of a payment.
func (s *Server) handleCreateRefund(w http.ResponseWriter, r *http.Request) {
	svc := s.refundService()
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "provider_unavailable",
			"the configured payment provider does not support refunds")
		return
	}
	paymentID := r.PathValue("id")
	if !portal.ValidID(paymentID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		// AmountMinor is optional: absent means "refund everything still available", which is what
		// the button on the payment screen does.
		AmountMinor int64  `json:"amount_minor"`
		Reason      string `json:"reason"`
		ApprovalID  string `json:"approval_id"`
		// RequestApproval asks for an approval instead of performing the refund, so the first
		// operator can start the dual-approval flow they cannot complete alone.
		RequestApproval bool `json:"request_approval"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.AmountMinor < 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount cannot be negative")
		return
	}

	user, _ := userFrom(r.Context())

	// Above the threshold with no approval: create or report the pending approval rather than
	// attempting the refund and failing halfway.
	if s.approvals != nil && req.ApprovalID == "" &&
		req.AmountMinor > 0 && s.approvals.RefundNeedsApproval(req.AmountMinor) {
		if !req.RequestApproval {
			writeProblem(w, http.StatusConflict, "approval_required",
				"a refund this size needs a second staff signature")
			return
		}
		s.requestRefundApproval(w, r, paymentID, user.ID, req.Reason, req.AmountMinor)
		return
	}

	// An approval was supplied: it must be for this payment, for this action, and granted.
	if req.ApprovalID != "" {
		if !s.checkRefundApproval(w, r, req.ApprovalID, paymentID) {
			return
		}
	}

	// The request's idempotency key doubles as the provider operation key. The middleware above
	// already replays a stored response, but that row expires; passing the key down means a retry
	// after the window is still recognised by the provider as the same refund rather than as a
	// second one.
	res, err := svc.WithAuditor(s.auditorFor(r, req.Reason)).Create(r.Context(), refunds.Request{
		PaymentID:    paymentID,
		AmountMinor:  req.AmountMinor,
		Reason:       req.Reason,
		RequestedBy:  user.ID,
		ApprovalID:   req.ApprovalID,
		OperationKey: r.Header.Get(idempotencyHeader),
	})
	switch {
	case err == nil:
	case errors.Is(err, refunds.ErrExceedsRefundable):
		writeProblem(w, http.StatusConflict, "exceeds_refundable", err.Error())
		return
	case errors.Is(err, refunds.ErrNotRefundable):
		writeProblem(w, http.StatusConflict, "not_refundable", err.Error())
		return
	case errors.Is(err, refunds.ErrApprovalRequired):
		writeProblem(w, http.StatusConflict, "approval_required", err.Error())
		return
	case errors.Is(err, refunds.ErrProviderRefused):
		// A definitive refusal: nothing was recorded, and the operator may retry with a different
		// amount or investigate with the provider.
		writeProblem(w, http.StatusBadGateway, "provider_refused", err.Error())
		return
	case errors.Is(err, refunds.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "refund failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	// An unknown outcome is a 202 with the refund id: the refund may have happened, so saying 500
	// would invite a retry that could refund twice.
	status := http.StatusCreated
	if res.Outcome == refunds.StatusUnknown {
		status = http.StatusAccepted
	}
	if res.Replayed {
		status = http.StatusOK
	}

	if res.Outcome == refunds.StatusSucceeded {
		s.publish(events.Event{
			Kind:       "refund.succeeded",
			Permission: string(authn.PermPaymentReconcile),
			Data:       map[string]any{"refund_id": res.Refund.ID, "payment_id": paymentID},
		})
	}

	writeJSON(w, status, refundMapFromRefund(res.Refund, res.Outcome, res.Replayed))
}

// refundMapFromRefund renders a refund for the API.
func refundMapFromRefund(rf refunds.Refund, outcome string, replayed bool) map[string]any {
	cur := moneyFor(rf.Currency)
	return map[string]any{
		"id":                 rf.ID,
		"payment_id":         rf.PaymentID,
		"provider_refund_id": nullableString(rf.ProviderRefundID),
		"operation_key":      rf.OperationKey,
		"amount":             amountJSON(money.New(rf.AmountMinor, cur)),
		"status":             rf.Status,
		"outcome":            outcome,
		"reason":             rf.Reason,
		"created_at":         rf.CreatedAt.UTC(),
		"finished_at":        timeOrNil(rf.FinishedAt),
		"replayed":           replayed,
	}
}

// requestRefundApproval creates a pending approval and answers 409 with its id.
func (s *Server) requestRefundApproval(w http.ResponseWriter, r *http.Request,
	paymentID, requester, reason string, amount int64) {
	var approval approvals.Approval
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approval, err = s.approvals.Request(ctx, tx, approvals.ActionRefund, paymentID,
			requester, reason)
		return err
	})
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"type":   "about:blank",
		"title":  "Approval Required",
		"status": http.StatusConflict,
		"detail": "a second staff member must approve this refund",
		"code":   "approval_required",
		"approval": map[string]any{
			"id":           approval.ID,
			"action":       approval.Action,
			"reference_id": approval.ReferenceID,
			"requested_by": approval.RequestedBy,
			"status":       approval.Status,
		},
		"amount_minor": amount,
	})
}

// checkRefundApproval validates a supplied approval.
func (s *Server) checkRefundApproval(w http.ResponseWriter, r *http.Request,
	approvalID, paymentID string) bool {
	if s.approvals == nil {
		writeProblem(w, http.StatusServiceUnavailable, "approvals_unavailable",
			"approvals are not configured")
		return false
	}
	approval, err := s.approvals.Get(r.Context(), approvalID)
	if err != nil {
		if errors.Is(err, approvals.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "approval_not_found", "no such approval")
			return false
		}
		s.staffError(w, r, err)
		return false
	}
	if approval.Action != approvals.ActionRefund || approval.ReferenceID != paymentID {
		writeProblem(w, http.StatusConflict, "approval_mismatch",
			"that approval is for a different payment")
		return false
	}
	if approval.Status != approvals.StatusApproved {
		writeProblem(w, http.StatusConflict, "approval_required",
			"the approval has not been granted")
		return false
	}
	return true
}

// moneyFor resolves a persisted currency code.
//
// A code outside the ISO table is reconstructed at scale 2, which is what the schema's CHECK allows:
// refusing to render a refund because its currency is unusual would be worse than rendering it at the
// default scale.
func moneyFor(code string) money.Currency {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur
	}
	cur, _ := money.NewCurrency(code, 2)
	return cur
}

// handleGetRefund returns one refund.
func (s *Server) handleGetRefund(w http.ResponseWriter, r *http.Request) {
	svc := s.refundService()
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "provider_unavailable",
			"the configured payment provider does not support refunds")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	rf, err := svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, refunds.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		}
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, refundMapFromRefund(rf, rf.Status, false))
}

// ------------------------------------------------------------ credit notes --

// handleIssueCreditNote issues an immutable counter-document.
func (s *Server) handleIssueCreditNote(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	invoiceID := r.PathValue("id")
	if !portal.ValidID(invoiceID) {
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
	if req.Reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "a credit note needs a reason")
		return
	}
	user, _ := userFrom(r.Context())

	note, err := creditnote.New(s.pool).WithAuditor(s.auditorFor(r, req.Reason)).
		Issue(r.Context(), creditnote.IssueRequest{
			InvoiceID:   invoiceID,
			AmountMinor: req.AmountMinor,
			TaxMinor:    req.TaxMinor,
			Reason:      req.Reason,
			IssuedBy:    user.ID,
		})
	switch {
	case err == nil:
	case errors.Is(err, creditnote.ErrExceedsInvoice):
		writeProblem(w, http.StatusConflict, "exceeds_invoice", err.Error())
		return
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
		Kind:       "credit_note.issued",
		Permission: string(authn.PermInvoiceReadAll),
		Data:       map[string]any{"credit_note_id": note.ID, "invoice_id": invoiceID},
	})

	cur := moneyFor(note.Currency)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         note.ID,
		"number":     note.Number,
		"invoice_id": note.InvoiceID,
		"total":      amountJSON(money.New(note.TotalMinor, cur)),
		"tax":        amountJSON(money.New(note.TaxMinor, cur)),
		"reason":     note.Reason,
		"status":     note.Status,
		"issued_at":  timeOrNil(note.IssuedAt),
	})
}

// -------------------------------------------------------- applying credit --

// handleApplyCredit spends customer credit on an invoice.
func (s *Server) handleApplyCredit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID := r.PathValue("id")
	if !portal.ValidID(customerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		InvoiceID   string `json:"invoice_id"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if !portal.ValidID(req.InvoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		return
	}
	if req.AmountMinor < 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount cannot be negative")
		return
	}
	user, _ := userFrom(r.Context())

	app, err := creditnote.New(s.pool).WithAuditor(s.auditorFor(r, "")).Apply(r.Context(), creditnote.ApplyRequest{
		CustomerID:  customerID,
		InvoiceID:   req.InvoiceID,
		AmountMinor: req.AmountMinor,
		AppliedBy:   user.ID,
	})
	switch {
	case err == nil:
	case errors.Is(err, creditnote.ErrInsufficientCredit):
		writeProblem(w, http.StatusConflict, "insufficient_credit", err.Error())
		return
	case errors.Is(err, creditnote.ErrExceedsInvoice):
		writeProblem(w, http.StatusConflict, "exceeds_invoice", err.Error())
		return
	case errors.Is(err, creditnote.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	if app.InvoicePaid {
		s.publish(events.Event{
			Kind:       "invoice.paid",
			Permission: string(authn.PermInvoiceReadAll),
			Data:       map[string]any{"invoice_id": app.InvoiceID, "via": "credit"},
		})
	}

	cur := moneyFor(app.Currency)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           app.ID,
		"customer_id":  app.CustomerID,
		"invoice_id":   app.InvoiceID,
		"amount":       amountJSON(money.New(app.AmountMinor, cur)),
		"invoice_paid": app.InvoicePaid,
		"created_at":   app.CreatedAt.UTC(),
	})
}

// reconcileRefunds is the worker entry point for the refund reconciler.
//
// It is exported so the job runner can call it without depending on the HTTP layer, and it lives
// here so the service construction (which needs the gateway assertion) has one home.
func (s *Server) ReconcileRefunds(ctx context.Context, limit int) (int, int, error) {
	svc := s.refundService()
	if svc == nil {
		return 0, 0, errors.New("httpapi: no refund-capable gateway is configured")
	}
	return svc.Reconcile(ctx, limit)
}
