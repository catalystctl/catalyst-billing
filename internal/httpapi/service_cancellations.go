// Cancellation requests over HTTP (CAN-01…CAN-06, SVC-09).
//
// Two audiences, and the split is the workflow:
//
//   - The customer files a request. It is scoped in SQL to their own customer account, so
//     substituting another customer's service id returns 404 rather than acting on it.
//   - Staff read the queue and decide. An immediate cancellation is only *executed* here,
//     which is what makes it a decision rather than a button a customer can press.
//
// The handlers are thin because the state machine lives in internal/service: what belongs
// here is the request policy — who may ask, what the response says, and which event is
// published after the transaction commits.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("60-service-cancellations", registerServiceCancellations)
}

// serviceStore returns the lifecycle store bound to this request's actor.
//
// It is constructed per call rather than cached: the value is a pool handle, a clock and an
// auditor, and the auditor is per-request. Caching it would need a field and a mutex on the
// shared Server for no measurable gain.
func (s *Server) serviceStore(r *http.Request) *service.Store {
	store := service.New(s.pool)
	if r != nil {
		store = store.WithAuditor(s.auditorFor(r, ""))
	}
	if s.issuer != nil {
		store.Issuer = s.issuer
	}
	store.Merchant = s.merchant
	store.Terms = s.terms
	store.Series = s.series
	return store
}

func registerServiceCancellations(r *router) {
	// Customer. The permission is service:cancel:self, which every customer holds and which
	// is scoped to their own record by the query.
	r.mutation(http.MethodPost, "/api/v1/portal/services/{id}/cancellation",
		authn.PermServiceCancelSelf, r.s.handleRequestCancellation)
	r.get("/api/v1/portal/services/{id}/cancellation",
		authn.PermServiceReadSelf, r.s.handlePortalCancellation)

	// Staff queue.
	r.staffGet("/api/v1/staff/cancellations", authn.PermServiceReadAll, r.s.handleStaffCancellations)
	r.staffMutation(http.MethodPost, "/api/v1/staff/cancellations/{id}/execute",
		authn.PermServiceTerminate, r.s.handleExecuteCancellation)
	r.staffMutation(http.MethodPost, "/api/v1/staff/cancellations/{id}/reject",
		authn.PermServiceSuspend, r.s.handleRejectCancellation)
	// A staff-initiated request on a customer's behalf. It takes the same body as the portal
	// route so the two cannot drift, and it is gated on service:upgrade because moving a
	// service's lifecycle on a customer's behalf is the same kind of decision.
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/cancellation",
		authn.PermServiceUpgrade, r.s.handleStaffRequestCancellation)
}

// cancellationRequest is the body of a cancellation request.
type cancellationRequest struct {
	// Reason is free text, bounded by the same 255 characters the column allows.
	Reason string `json:"reason"`
	// Type is "immediate" or "end_of_period".
	Type string `json:"type"`
}

// cancellationJSON is the wire form of a cancellation request.
func cancellationJSON(c service.Cancellation) map[string]any {
	out := map[string]any{
		"id":         c.ID,
		"service_id": c.ServiceID,
		"type":       c.Type,
		"status":     c.Status,
		"reason":     c.Reason,
		"created_at": c.CreatedAt.UTC(),
	}
	if c.RequestedBy != "" {
		out["requested_by"] = c.RequestedBy
	}
	if c.EffectiveAt != nil {
		out["effective_at"] = c.EffectiveAt.UTC()
	}
	if c.ExecutedAt != nil {
		out["executed_at"] = c.ExecutedAt.UTC()
	}
	if c.CustomerID != "" {
		out["customer"] = map[string]any{
			"id":    c.CustomerID,
			"name":  c.CustomerName,
			"email": c.CustomerEmail,
		}
	}
	if c.ProductName != "" {
		out["product_name"] = c.ProductName
		out["service_status"] = c.ServiceStatus
	}
	return out
}

// handleRequestCancellation records a customer's request.
//
// The response states the consequence plainly rather than echoing a status code: an
// end-of-period request keeps the service running until a date, and an immediate request is a
// *pending* decision that staff will act on. A customer who is told "cancelled" and then finds
// the service running (or gone) has been lied to by the UI.
func (s *Server) handleRequestCancellation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req cancellationRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	out, err := s.serviceStore(r).RequestCancellation(r.Context(), service.CancellationRequest{
		ServiceID: id,
		UserID:    user.ID,
		Reason:    req.Reason,
		Type:      req.Type,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	s.publishServiceEvent(r, id, "service.cancellation_requested", map[string]any{
		"service_id": id, "cancellation_id": out.ID, "type": out.Type, "status": out.Status,
	})

	// Tell the customer their request was received, and when it takes effect. A cancellation is
	// the one action where silence is worst: a customer who asked to stop paying needs to know
	// the request landed, and an end-of-period request that says nothing leaves them unsure
	// whether the next invoice is coming. Best-effort — the request itself is already recorded,
	// and failing the response because a mail could not be queued would be the wrong trade.
	s.notifyCancellationReceived(r.Context(), id, out)
	// And tell the staff who own the queue. The customer's mail says "we have your request";
	// without this, nobody knows a decision is waiting, which is how an immediate cancellation
	// sits pending until the customer chases it (CAN-04).
	s.notifyStaffCancellationReceived(r.Context(), out)

	body := cancellationJSON(out)
	if out.Status == service.StatusScheduled && out.EffectiveAt != nil {
		body["message"] = "Your service stays active until the end of the period you have paid for."
	} else {
		body["message"] = "Your request has been recorded and will be reviewed."
	}
	writeJSON(w, http.StatusCreated, body)
}

// handleStaffRequestCancellation records a request on a customer's behalf.
func (s *Server) handleStaffRequestCancellation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req cancellationRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	out, err := s.serviceStore(r).RequestCancellationAsStaff(r.Context(), service.CancellationRequest{
		ServiceID: id,
		UserID:    user.ID,
		Reason:    req.Reason,
		Type:      req.Type,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.cancellation_requested", map[string]any{
		"service_id": id, "cancellation_id": out.ID, "type": out.Type, "status": out.Status,
	})
	writeJSON(w, http.StatusCreated, cancellationJSON(out))
}

// handlePortalCancellation reports the open request for a service, if any.
func (s *Server) handlePortalCancellation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	// The scope check is in SQL and does not depend on the portal being configured: a
	// service that is not the caller's is indistinguishable from one that does not exist.
	user, _ := userFrom(r.Context())
	if err := s.serviceStore(nil).ServiceOwnedBy(r.Context(), id, user.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	c, err := s.serviceStore(nil).CancellationForService(r.Context(), id, user.ID)
	if errors.Is(err, service.ErrNotFound) {
		// No open request is not an error: it is the normal state.
		writeJSON(w, http.StatusOK, map[string]any{"cancellation": nil})
		return
	}
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancellation": cancellationJSON(c)})
}

// handleStaffCancellations lists the queue.
//
// Query: status (pending/scheduled/executed/cancelled/rejected; empty means the open queue),
// service_id, limit.
func (s *Server) handleStaffCancellations(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	query := r.URL.Query()
	limit := 50
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = n
	}

	status := strings.TrimSpace(query.Get("status"))
	switch status {
	case "", service.StatusPending, service.StatusScheduled, service.StatusExecuted,
		service.StatusCancelled, service.StatusRejected:
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_status",
			"status must be one of pending, scheduled, executed, cancelled, rejected")
		return
	}

	serviceID := strings.TrimSpace(query.Get("service_id"))
	if serviceID != "" && !portal.ValidID(serviceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	rows, err := s.serviceStore(nil).Cancellations(r.Context(), service.CancellationFilter{
		Status:    status,
		ServiceID: serviceID,
		Limit:     limit,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, cancellationJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cancellations": out,
		"statuses": []string{
			service.StatusPending, service.StatusScheduled, service.StatusExecuted,
			service.StatusCancelled, service.StatusRejected,
		},
	})
}

// handleExecuteCancellation executes a pending immediate request.
//
// The response is 202 with the operation id, because the *destructive* part is the queued
// terminate: the request is recorded as executed, and the service disappears when the worker
// has spoken to the provider. Answering 200 with "terminated" would claim an effect that has
// not happened.
func (s *Server) handleExecuteCancellation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
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
	// An empty body is allowed: the request already carries the customer's reason, and an
	// operator executing it has nothing to add.
	if r.ContentLength != 0 {
		if !s.decodeJSON(w, r, &req, 16<<10) {
			return
		}
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).ExecuteCancellation(r.Context(), id, user.ID, req.Reason)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	s.publishServiceEvent(r, res.ServiceID, "service.cancellation_executed", map[string]any{
		"service_id": res.ServiceID, "cancellation_id": res.CancellationID,
		"operation_id": res.OperationID,
	})

	if res.AlreadyTerminated || res.OperationID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"cancellation_id":    res.CancellationID,
			"service_id":         res.ServiceID,
			"status":             service.StatusExecuted,
			"already_terminated": true,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"cancellation_id": res.CancellationID,
		"service_id":      res.ServiceID,
		"status":          service.StatusExecuted,
		"operation_id":    res.OperationID,
		"status_url":      "/api/v1/staff/operations/" + res.OperationID,
	})
}

// handleRejectCancellation rejects a request and clears any schedule it set.
func (s *Server) handleRejectCancellation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
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
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		// Rejecting a customer's request without a reason leaves the next agent (and the
		// customer) with no explanation, which is how a queue turns into a black hole.
		writeProblem(w, http.StatusBadRequest, "reason_required", "rejecting a request needs a reason")
		return
	}
	user, _ := userFrom(r.Context())

	out, err := s.serviceStore(r).RejectCancellation(r.Context(), id, user.ID, req.Reason)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.publishServiceEvent(r, out.ServiceID, "service.cancellation_rejected", map[string]any{
		"service_id": out.ServiceID, "cancellation_id": out.ID,
	})
	writeJSON(w, http.StatusOK, cancellationJSON(out))
}

// writeServiceError maps a lifecycle error to a problem document.
//
// The codes are stable because the UI branches on them: "already_requested" opens the
// existing request, "not_cancellable" hides the button, "not_pending" refreshes the queue.
func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, service.ErrInvalidInput):
		writeProblem(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, service.ErrNotCancellable):
		writeProblem(w, http.StatusConflict, "not_cancellable", err.Error())
	case errors.Is(err, service.ErrAlreadyRequested):
		writeProblem(w, http.StatusConflict, "already_requested",
			"a cancellation request is already open for this service")
	case errors.Is(err, service.ErrNotPending):
		writeProblem(w, http.StatusConflict, "not_pending", err.Error())
	case errors.Is(err, service.ErrNotUpgradable):
		writeProblem(w, http.StatusConflict, "not_upgradable", err.Error())
	case errors.Is(err, service.ErrCurrencyMismatch):
		writeProblem(w, http.StatusConflict, "currency_mismatch", err.Error())
	case errors.Is(err, service.ErrIntervalMismatch):
		writeProblem(w, http.StatusConflict, "interval_mismatch", err.Error())
	case errors.Is(err, service.ErrOptionNotAvailable):
		writeProblem(w, http.StatusBadRequest, "option_not_available", err.Error())
	case errors.Is(err, service.ErrOptionNotUpgradable):
		writeProblem(w, http.StatusConflict, "option_not_upgradable", err.Error())
	case errors.Is(err, service.ErrOutOfStock):
		writeProblem(w, http.StatusConflict, "out_of_stock", err.Error())
	case errors.Is(err, service.ErrPerUserLimit):
		writeProblem(w, http.StatusConflict, "per_user_limit", err.Error())
	case errors.Is(err, service.ErrNoInvoice):
		writeProblem(w, http.StatusConflict, "invoice_not_paid", err.Error())
	case errors.Is(err, service.ErrCreditNotPosted):
		writeProblem(w, http.StatusConflict, "credit_not_posted", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "service lifecycle request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// notifyCancellationReceived tells a customer their cancellation request was recorded.
//
// It is best-effort and runs on its own transaction: the request is already committed by the
// time this is called, and a customer whose mail is misconfigured must still be able to cancel.
// A failure is swallowed deliberately — the request is the durable fact, and the notification is
// a courtesy that must not turn into a refusal.
//
// The dedupe key is the service id, so asking twice for the same service writes one message; a
// second request on the same service is the same news, not new news.
func (s *Server) notifyCancellationReceived(ctx context.Context, serviceID string, out service.Cancellation) {
	if s.pool == nil || s.notify == nil {
		return
	}

	var (
		email string
		name  string
		user  *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.email, COALESCE(NULLIF(p.name, ''), s.product_id::text),
		       (SELECT ca.user_id::text FROM customer_accounts ca WHERE ca.customer_id = c.id LIMIT 1)
		  FROM services s
		  JOIN customers c ON c.id = s.customer_id
		  JOIN products p  ON p.id = s.product_id
		 WHERE s.id = $1`, serviceID).Scan(&email, &name, &user)
	if err != nil {
		return
	}

	in := notify.CancellationReceivedInput{
		Email:       email,
		ServiceID:   serviceID,
		ServiceName: name,
		ServiceURL:  "/portal/services/" + serviceID,
	}
	if user != nil {
		in.UserID = *user
	}
	if out.EffectiveAt != nil {
		in.CancelsAt = *out.EffectiveAt
	}

	_ = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.CancellationReceived(ctx, tx, s.notify, in)
		return err
	})
}

// notifyStaffCancellationReceived puts the request in the inbox of every operator who can act on
// it (CAN-04).
//
// # Why the inbox rather than a mail
//
// A cancellation needs a *decision*, and the decision is the queue. There is no configured staff
// mailbox in this product — `general.system_email_address` is the no-reply sender, and mailing a
// queue to it would be mailing the operator's own outbox. An inbox row is where the panel already
// tells a person "something is waiting for you", it is readable by any signed-in user (every role
// holds the customer-facing permissions its own inbox needs), and it has a dedupe key, so the
// same request cannot notify twice.
//
// # Why the recipients are resolved by permission and not by role name
//
// "Who can act on a cancellation" is `service:read:all`. Asking the permission means an operator
// on a custom role gets the notification the day the role is given the permission, and stops
// getting it the day it is taken away — a list of role names would be a copy of the matrix that
// drifts from it.
//
// It is best-effort and post-commit, exactly like the customer's mail: the request is the durable
// fact, and a notification is a courtesy that must not be able to refuse a cancellation.
func (s *Server) notifyStaffCancellationReceived(ctx context.Context, out service.Cancellation) {
	if s.pool == nil || out.ID == "" {
		return
	}

	var (
		product  string
		customer string
		email    string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT p.name, c.name, c.email
		FROM services s
		JOIN products p ON p.id = s.product_id
		JOIN customers c ON c.id = s.customer_id
		WHERE s.id = $1`, out.ServiceID).Scan(&product, &customer, &email); err != nil {
		return
	}
	if customer == "" {
		customer = email
	}

	when := "at the end of the period"
	if out.Type == service.CancelImmediate {
		when = "immediately, which needs a decision"
	}
	body := customer + " asked to cancel " + product + " " + when + "."
	if out.Reason != "" {
		body += " Their reason: " + out.Reason
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.id::text
		FROM users u
		JOIN staff_roles sr ON sr.user_id = u.id
		JOIN role_permissions rp ON rp.role_id = sr.role_id
		WHERE rp.permission = $1 AND u.status = 'active'
		ORDER BY 1`, string(authn.PermServiceReadAll))
	if err != nil {
		return
	}
	var recipients []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		recipients = append(recipients, id)
	}
	rows.Close()

	for _, userID := range recipients {
		// One row per request per operator: a second request on the same service is a new row
		// (new id), while a retried delivery of this one is the same row.
		if _, err := notify.Create(ctx, s.pool, notify.Notification{
			UserID:    userID,
			EventKey:  notify.EventServiceCancellationReceived,
			Title:     "Cancellation requested",
			Body:      body,
			URL:       "/cancellations",
			DedupeKey: "service_cancellation:" + out.ID,
		}); err != nil {
			// A failed inbox write must not fail the request that already committed.
			continue
		}
	}
}
