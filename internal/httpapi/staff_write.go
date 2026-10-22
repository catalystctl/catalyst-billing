package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// The staff mutation surface (Gate B).
//
// Every handler here follows the same four-step shape, and the shape is the point:
//
//  1. Lock and re-read inside one transaction. A decision made against data read outside the
//     transaction is a decision about data that may already be gone.
//  2. Do the domain work through the service that owns the invariant (the dunning engine for
//     a suspension, the issuer for an invoice). Never reimplement the rule here.
//  3. Write the audit row in the same transaction. An audit row for a rolled-back change is a
//     lie; a change with no audit row is a gap.
//  4. Publish the event *after* commit. An event published inside the transaction tells a
//     client about a change that can still disappear.

func init() {
	RegisterRoutes("50-staff-write", registerStaffWrites)
}

func registerStaffWrites(r *router) {
	// Service lifecycle.
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/suspend",
		authn.PermServiceSuspend, r.s.handleSuspend)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/resume",
		authn.PermServiceSuspend, r.s.handleResume)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/terminate",
		authn.PermServiceTerminate, r.s.handleTerminate)

	// Holds.
	r.staffGet("/api/v1/staff/services/{id}/holds", authn.PermServiceReadAll, r.s.handleListHolds)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/holds",
		authn.PermServiceHold, r.s.handleAddHold)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/holds/{hold_id}",
		authn.PermServiceHold, r.s.handleClearHold)

	// Invoices.
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices",
		authn.PermInvoiceWriteAll, r.s.handleCreateInvoice)
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/issue",
		authn.PermInvoiceWriteAll, r.s.handleIssueInvoice)
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/void",
		authn.PermInvoiceVoid, r.s.handleVoidInvoice)
}

// accepted is the 202 body for an enqueued action.
type accepted struct {
	OperationID string `json:"operation_id"`
	StatusURL   string `json:"status_url"`
}

// acceptedResponse renders the 202 body.
func acceptedResponse(operationID string) accepted {
	return accepted{
		OperationID: operationID,
		StatusURL:   "/api/v1/staff/operations/" + operationID,
	}
}

// requireDunning returns the engine, or writes 503.
func (s *Server) requireDunning(w http.ResponseWriter) bool {
	if s.dunning == nil {
		writeProblem(w, http.StatusServiceUnavailable, "dunning_unavailable",
			"the dunning engine is not configured")
		return false
	}
	return true
}

// -------------------------------------------------------------- suspension --

// suspendRequest is the body of a suspend call.
type suspendRequest struct {
	Reason string `json:"reason"`
	// OverrideHold asks to suspend despite a manual hold. It requires approval_id: the id
	// of an approval a *different* staff actor already granted.
	OverrideHold bool   `json:"override_hold"`
	ApprovalID   string `json:"approval_id"`
	// RequestApproval asks for an approval to be created instead of performing the action.
	// It is how the first operator starts the dual-approval dance: they cannot approve
	// their own request, so the response hands them the id a colleague needs.
	RequestApproval bool `json:"request_approval"`
}

// handleSuspend suspends a service for non-payment, or refuses and says why.
//
// The three outcomes are deliberately distinct, because they need different actions from the
// operator:
//
//   - 202: enqueued. Poll the operation.
//   - 409 hold_in_place: a manual hold is blocking it. Either clear the hold or ask a
//     colleague to approve overriding it.
//   - 409 not_eligible: the customer does not owe enough, or the service is not active. There
//     is nothing to do; suspending would be wrong.
func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	if !s.requireDunning(w) {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req suspendRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	// Is a hold in the way? Asking before acting lets the response name the reason instead
	// of surfacing a generic refusal.
	holdID, held, err := s.dunning.ActiveHold(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	if held {
		if !req.OverrideHold {
			writeProblem(w, http.StatusConflict, "hold_in_place",
				"a manual hold is in place; clear it, or request an override approval")
			return
		}

		// An override needs an approval a different person granted.
		if req.ApprovalID == "" {
			if !req.RequestApproval {
				writeProblem(w, http.StatusConflict, "approval_required",
					"overriding a hold needs an approval from a second staff member")
				return
			}
			s.requestHoldOverrideApproval(w, r, id, holdID, user.ID, req.Reason)
			return
		}

		approval, err := s.approvals.Get(r.Context(), req.ApprovalID)
		if err != nil {
			s.staffError(w, r, err)
			return
		}
		if approval.Action != approvals.ActionHoldOverride || approval.ReferenceID != id {
			writeProblem(w, http.StatusConflict, "approval_mismatch",
				"that approval is for a different action")
			return
		}
		if approval.Status != approvals.StatusApproved {
			writeProblem(w, http.StatusConflict, "approval_required",
				"the approval has not been granted")
			return
		}

		elig, err := s.dunning.WithAuditor(s.dunningAuditor(r, req.Reason)).
			OverrideHold(r.Context(), dunning.OverrideRequest{
				ServiceID:   id,
				Reason:      req.Reason,
				RequestedBy: user.ID,
				ApprovedBy:  approval.ApprovedBy,
			})
		if err != nil {
			s.writeDunningError(w, r, err)
			return
		}
		s.publishServiceEvent(r, id, "service.suspend_requested", map[string]any{
			"service_id": id, "generation": elig.Generation, "override_hold": true,
		})
		s.respondEnqueued(w, r, "suspend", id, elig.Generation)
		return
	}

	elig, err := s.dunning.WithAuditor(s.dunningAuditor(r, req.Reason)).
		RequestSuspension(r.Context(), id)
	if err != nil {
		s.writeDunningError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.suspend_requested", map[string]any{
		"service_id": id, "generation": elig.Generation,
	})
	s.respondEnqueued(w, r, "suspend", id, elig.Generation)
}

// requestHoldOverrideApproval creates a pending approval and answers 409 with its id.
//
// The status is 409 rather than 201 because the requested action did not happen: nothing was
// suspended. Returning 201 would tell the client the suspension is under way.
func (s *Server) requestHoldOverrideApproval(w http.ResponseWriter, r *http.Request,
	serviceID, holdID, requester, reason string) {
	if s.approvals == nil {
		writeProblem(w, http.StatusServiceUnavailable, "approvals_unavailable",
			"approvals are not configured")
		return
	}

	var approval approvals.Approval
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approval, err = s.approvals.Request(ctx, tx, approvals.ActionHoldOverride,
			serviceID, requester, reason)
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
		"detail": "a second staff member must approve overriding the hold",
		"code":   "approval_required",
		"approval": map[string]any{
			"id":           approval.ID,
			"action":       approval.Action,
			"reference_id": approval.ReferenceID,
			"requested_by": approval.RequestedBy,
			"status":       approval.Status,
		},
		"hold_id": holdID,
	})
}

// handleResume resumes a suspended service.
//
// The engine refuses unless the customer owes nothing at all, checked against the subledger.
// That is stricter than "this invoice is paid" on purpose: a second overdue invoice would
// suspend the service again within the hour, and an operator who has to click twice learns to
// distrust the button.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !s.requireDunning(w) {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req suspendRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}

	elig, err := s.dunning.WithAuditor(s.dunningAuditor(r, req.Reason)).
		RequestResume(r.Context(), id)
	if err != nil {
		s.writeDunningError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.resume_requested", map[string]any{
		"service_id": id, "generation": elig.Generation,
	})
	s.respondEnqueued(w, r, "resume", id, elig.Generation)
}

// terminateRequest is the body of a terminate call.
type terminateRequest struct {
	// Confirm must equal the service's external_id or id. It is not a password: it is a
	// speed bump that makes "terminate" impossible to trigger by a mis-click or a replayed
	// request against a different service.
	Confirm string `json:"confirm"`
	Reason  string `json:"reason"`
}

// handleTerminate terminates a service.
//
// Termination is not automatically reconcilable: if the adapter is called and the process
// dies, the resource may or may not be gone. That is why it is a queued operation with a
// confirmation, not a direct call.
func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req terminateRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}

	var (
		externalID string
		status     string
		generation int64
		opID       string
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(external_id, ''), status, generation
			FROM services WHERE id = $1 FOR UPDATE`, id).
			Scan(&externalID, &status, &generation); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return staffNotFound{}
			}
			return fmt.Errorf("staff: lock service: %w", err)
		}

		// The operator must name what they are destroying. Accepting either identifier keeps
		// the check usable whether the UI shows the internal id or the provider's.
		if req.Confirm != id && (externalID == "" || req.Confirm != externalID) {
			return errConfirmationMismatch
		}
		if status == "terminated" {
			return errAlreadyTerminated
		}

		key := fmt.Sprintf("terminate:%s:%d", id, generation)
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'terminate', $2, $3, 'queued')
			ON CONFLICT (operation_key) DO UPDATE SET operation_key = EXCLUDED.operation_key
			RETURNING id`, id, key, generation).Scan(&opID); err != nil {
			return fmt.Errorf("staff: enqueue termination: %w", err)
		}

		return audit.Record(ctx, tx, s.auditEvent(r, "service.terminate", "service", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"reason":      req.Reason,
			"generation":  generation,
			"external_id": externalID,
		}))
	})
	switch {
	case err == nil:
	case errors.As(err, new(staffNotFound)):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, errConfirmationMismatch):
		writeProblem(w, http.StatusBadRequest, "confirmation_mismatch",
			"confirm must equal the service's external_id or id")
		return
	case errors.Is(err, errAlreadyTerminated):
		writeProblem(w, http.StatusConflict, "already_terminated", "this service is already terminated")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.publishServiceEvent(r, id, "service.terminate_requested", map[string]any{
		"service_id": id, "generation": generation,
	})
	writeJSON(w, http.StatusAccepted, acceptedResponse(opID))
}

// staffNotFound marks a missing row so the transaction wrapper can map it.
type staffNotFound struct{}

func (staffNotFound) Error() string { return "staff: not found" }

var (
	errConfirmationMismatch = errors.New("staff: confirmation mismatch")
	errAlreadyTerminated    = errors.New("staff: already terminated")
)

// respondEnqueued reads the operation id back by key and answers 202.
func (s *Server) respondEnqueued(w http.ResponseWriter, r *http.Request, kind, serviceID string, generation int64) {
	opID, err := s.dunning.OperationIDFor(r.Context(), kind, serviceID, generation)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, acceptedResponse(opID))
}

// writeDunningError maps a dunning refusal to a response.
func (s *Server) writeDunningError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, dunning.ErrNotEligible):
		writeProblem(w, http.StatusConflict, "not_eligible", err.Error())
	case errors.Is(err, dunning.ErrHoldInPlace):
		writeProblem(w, http.StatusConflict, "hold_in_place", err.Error())
	default:
		s.staffError(w, r, err)
	}
}

// dunningAuditor returns an auditor that writes the audit row inside the engine's transaction.
//
// This is what makes F1 true for the service actions: the row that says who suspended a service
// commits with the operation, so there is no window in which the operation exists and its record
// does not.
func (s *Server) dunningAuditor(r *http.Request, reason string) dunning.Auditor {
	return s.auditorFor(r, reason)
}

// publishServiceEvent sends an event scoped to the customer who owns the service.
func (s *Server) publishServiceEvent(r *http.Request, serviceID, kind string, data map[string]any) {
	if s.events == nil {
		return
	}
	var customerID string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT customer_id FROM services WHERE id = $1`, serviceID).Scan(&customerID); err != nil {
		customerID = ""
	}
	s.publish(events.Event{
		Kind:       kind,
		CustomerID: customerID,
		Permission: string(authn.PermServiceReadAll),
		Data:       data,
	})
}

// ------------------------------------------------------------------- holds --

func (s *Server) handleListHolds(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	holds, err := s.staffService().Holds(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(holds))
	for _, h := range holds {
		out = append(out, holdMap(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// addHoldRequest is the body of a hold.
type addHoldRequest struct {
	Reason string `json:"reason"`
	// ExpiresInSeconds makes the hold temporary. Zero or absent means it lasts until a human
	// clears it, which is the safe default: a hold that expires on its own is a hold nobody
	// notices has lapsed.
	ExpiresInSeconds int `json:"expires_in_seconds"`
	// CreatedBy is accepted in the body only so a client that sends it can be told it is
	// ignored. The session is the only source of the actor.
	CreatedBy string `json:"created_by"`
}

func (s *Server) handleAddHold(w http.ResponseWriter, r *http.Request) {
	if !s.requireDunning(w) {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req addHoldRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if req.Reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "a hold needs a reason")
		return
	}

	user, _ := userFrom(r.Context())
	var expiresAt *time.Time
	if req.ExpiresInSeconds > 0 {
		when := time.Now().UTC().Add(time.Duration(req.ExpiresInSeconds) * time.Second)
		expiresAt = &when
	}

	var holdID string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("staff: check service: %w", err)
		}
		if !exists {
			return staffNotFound{}
		}
		// created_by comes from the session. A body field is ignored even when present, so a
		// client cannot attribute a hold to someone else.
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_holds (service_id, reason, created_by, expires_at)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			id, req.Reason, user.ID, expiresAt).Scan(&holdID); err != nil {
			return fmt.Errorf("staff: add hold: %w", err)
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "service.hold_add", "service", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"hold_id":    holdID,
			"reason":     req.Reason,
			"expires_at": expiresAt,
		}))
	})
	if err != nil {
		if errors.As(err, new(staffNotFound)) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		}
		s.staffError(w, r, err)
		return
	}

	s.publishServiceEvent(r, id, "service.hold_added", map[string]any{
		"service_id": id, "hold_id": holdID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         holdID,
		"service_id": id,
		"reason":     req.Reason,
		"expires_at": expiresAt,
		"created_by": user.ID,
	})
}

func (s *Server) handleClearHold(w http.ResponseWriter, r *http.Request) {
	holdID := r.PathValue("hold_id")
	if !portal.ValidID(holdID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var serviceID string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			DELETE FROM service_holds WHERE id = $1 RETURNING service_id`, holdID).
			Scan(&serviceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return staffNotFound{}
			}
			return fmt.Errorf("staff: clear hold: %w", err)
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "service.hold_clear", "service", serviceID,
			audit.OutcomeSuccess).WithChanges(map[string]any{"hold_id": holdID}))
	})
	if err != nil {
		if errors.As(err, new(staffNotFound)) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		}
		s.staffError(w, r, err)
		return
	}

	s.publishServiceEvent(r, serviceID, "service.hold_cleared", map[string]any{
		"service_id": serviceID, "hold_id": holdID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- invoices --

// invoiceLineRequest is one line of a manual invoice.
type invoiceLineRequest struct {
	Description string `json:"description"`
	Quantity    int    `json:"quantity"`
	// UnitAmount is in minor units. It is a price, not a total: the server computes every
	// total with the configured pricer, so a client cannot choose what to charge.
	UnitAmount int64  `json:"unit_amount_minor"`
	Currency   string `json:"currency"`
	// ProductID makes the line sell a product. When the invoice is paid, fulfilment provisions
	// one service per such line — this is what makes a manual invoice deliver something, since
	// it has no order behind it to describe what was bought.
	//
	// Omit it for a line that provisions nothing: support time, a prorated difference, a
	// late fee. Most lines are that.
	ProductID string `json:"product_id,omitempty"`
	// PriceVersionID pins which price was used, so the provision plan and region are
	// reproducible. Optional: with no version, fulfilment reads the plan from the product's
	// current version while the amount charged stays exactly what was typed.
	PriceVersionID string `json:"price_version_id,omitempty"`
}

// createInvoiceRequest is the body of a manual invoice.
type createInvoiceRequest struct {
	CustomerID string `json:"customer_id"`
	// IssueNow drafts and issues in one transaction, which is what the "create and send"
	// button does.
	IssueNow bool `json:"issue_now"`
	// DueInDays defaults to the merchant's configured terms.
	DueInDays int                  `json:"due_in_days"`
	Lines     []invoiceLineRequest `json:"lines"`
	Memo      string               `json:"memo"`
}

func (s *Server) handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	if s.issuer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "issuer_unavailable",
			"invoice issuance is not configured")
		return
	}
	var req createInvoiceRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if !portal.ValidID(req.CustomerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	if len(req.Lines) == 0 {
		writeProblem(w, http.StatusBadRequest, "no_lines", "an invoice needs at least one line")
		return
	}

	// Every line must share one currency: an invoice is a single-currency document, and a
	// mixed-currency invoice would need an exchange rate the product does not have.
	currency := req.Lines[0].Currency
	for _, line := range req.Lines {
		if line.Currency != currency {
			writeProblem(w, http.StatusBadRequest, "mixed_currency",
				"all lines of an invoice must use the same currency")
			return
		}
		if line.Quantity <= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid_quantity",
				"quantity must be positive")
			return
		}
		if line.Description == "" {
			writeProblem(w, http.StatusBadRequest, "invalid_line", "every line needs a description")
			return
		}
	}
	cur, err := staffCurrency(currency)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", "unknown currency")
		return
	}

	dueInDays := req.DueInDays
	if dueInDays <= 0 {
		dueInDays = 30
	}

	var (
		invoiceID string
		number    string
		status    string
	)
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		lines := make([]billing.Line, 0, len(req.Lines))
		for _, line := range req.Lines {
			var productID, versionID *string
			if line.ProductID != "" {
				if !portal.ValidID(line.ProductID) {
					return staffNotFound{}
				}
				productID = &line.ProductID
			}
			if line.PriceVersionID != "" {
				if !portal.ValidID(line.PriceVersionID) {
					return staffNotFound{}
				}
				versionID = &line.PriceVersionID
			}
			lines = append(lines, billing.Line{
				Description: line.Description,
				// Quantity is expressed in thousandths, which is the pricer's unit.
				QuantityMilli:  int64(line.Quantity) * 1000,
				UnitPrice:      money.New(line.UnitAmount, cur),
				ProductID:      productID,
				PriceVersionID: versionID,
			})
		}

		customer := billing.CustomerDetails{ID: req.CustomerID}
		if err := billing.FillCustomer(ctx, tx, &customer); err != nil {
			if errors.Is(err, billing.ErrInvalidProfile) {
				return staffNotFound{}
			}
			return err
		}

		issueReq := billing.IssueRequest{
			CustomerID: req.CustomerID,
			Lines:      lines,
			Merchant:   s.merchant,
			Customer:   customer,
			Terms:      s.terms,
			DueAt:      s.now().AddDate(0, 0, dueInDays),
			Series:     s.series,
		}

		id, _, _, err := s.issuer.CreateDraft(ctx, tx, issueReq)
		if err != nil {
			return err
		}
		invoiceID, status = id, "draft"

		if req.IssueNow {
			issued, err := s.issuer.Issue(ctx, tx, id, s.series)
			if err != nil {
				return err
			}
			number, status = issued.Number, "open"
			// Queue the notification in the same transaction: a crash between issuing and
			// mailing must not lose the mail, and a retry must not send it twice.
			if err := s.queueInvoiceNotification(ctx, tx, id, issued.Number,
				issued.Totals.Total, issueReq.DueAt); err != nil {
				return err
			}
		}

		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.create", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"customer_id": req.CustomerID,
			"lines":       len(req.Lines),
			"issue_now":   req.IssueNow,
			"currency":    currency,
		}))
	})
	if err != nil {
		if errors.As(err, new(staffNotFound)) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
			return
		}
		if errors.Is(err, billing.ErrInvalidInvoice) {
			writeProblem(w, http.StatusBadRequest, "invalid_invoice", err.Error())
			return
		}
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.created", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": invoiceID, "number": number, "status": status}})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     invoiceID,
		"number": number,
		"status": status,
	})
}

// queueInvoiceNotification enqueues the "your invoice is ready" message.
//
// The message is suppressed rather than sent when the customer has no verified address:
// "we did not send it" and "we failed to send it" are different answers to a support ticket,
// and only one of them is a bug.
func (s *Server) queueInvoiceNotification(ctx context.Context, tx pgx.Tx, invoiceID, number string,
	total money.Amount, dueAt time.Time) error {
	if s.notify == nil {
		return nil
	}
	var (
		email    string
		verified bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT c.email, EXISTS (
			SELECT 1 FROM customer_accounts ca
			JOIN users u ON u.id = ca.user_id
			WHERE ca.customer_id = c.id AND u.email_verified_at IS NOT NULL)
		FROM invoices i JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1`, invoiceID).Scan(&email, &verified); err != nil {
		return fmt.Errorf("staff: read invoice customer: %w", err)
	}

	msg := notify.InvoiceIssuedMessage(email, number, total.String(),
		dueAt.UTC().Format("2006-01-02"), invoiceID)
	if !verified {
		return s.notify.Suppress(ctx, tx, msg, "the customer has no verified address")
	}
	if _, _, err := s.notify.Enqueue(ctx, tx, msg); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleIssueInvoice(w http.ResponseWriter, r *http.Request) {
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

	var (
		number string
		total  money.Amount
		dueAt  time.Time
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1 FOR UPDATE`, id).
			Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return staffNotFound{}
			}
			return fmt.Errorf("staff: lock invoice: %w", err)
		}
		if status != "draft" {
			return errNotDraft
		}

		issued, err := s.issuer.Issue(ctx, tx, id, s.series)
		if err != nil {
			return err
		}
		number, total = issued.Number, issued.Totals.Total
		var due time.Time
		if err := tx.QueryRow(ctx, `SELECT due_at FROM invoices WHERE id = $1`, id).Scan(&due); err != nil {
			return fmt.Errorf("staff: read invoice due date: %w", err)
		}
		dueAt = due
		if err := s.queueInvoiceNotification(ctx, tx, id, issued.Number, issued.Totals.Total, due); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.issue", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"number": issued.Number,
		}))
	})
	if err != nil {
		switch {
		case errors.As(err, new(staffNotFound)):
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		case errors.Is(err, errNotDraft):
			writeProblem(w, http.StatusConflict, "not_draft", "only a draft invoice can be issued")
			return
		}
		s.staffError(w, r, err)
		return
	}

	s.publish(events.Event{Kind: "invoice.issued", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": id, "number": number}})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"number": number,
		"status": "open",
		"total":  amountJSON(total),
		"due_at": dueAt.UTC(),
	})
}

var errNotDraft = errors.New("staff: invoice is not a draft")

func (s *Server) handleVoidInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if req.Reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "voiding needs a reason")
		return
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var (
			status string
			paid   int64
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, amount_paid_minor FROM invoices WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &paid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return staffNotFound{}
			}
			return fmt.Errorf("staff: lock invoice: %w", err)
		}
		if status == "paid" || paid > 0 {
			// A paid invoice cannot become void: the money moved, and the correction is a
			// credit note, not an erasure.
			return errNotVoidable
		}
		if status == "void" {
			return errAlreadyVoid
		}

		// The trigger in 0008 permits draft/open -> void only; the update relies on it rather
		// than duplicating the rule.
		if _, err := tx.Exec(ctx, `UPDATE invoices SET status = 'void' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("staff: void invoice: %w", err)
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "invoice.void", "invoice", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"reason":      req.Reason,
			"previous":    status,
			"amount_paid": paid,
		}))
	})
	switch {
	case err == nil:
	case errors.As(err, new(staffNotFound)):
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
		Data: map[string]any{"invoice_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "void"})
}

var (
	errNotVoidable = errors.New("staff: invoice is not voidable")
	errAlreadyVoid = errors.New("staff: invoice is already void")
)

// staffCurrency resolves a currency code for a request body.
func staffCurrency(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
