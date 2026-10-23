// The operator actions on one service that the panel was missing (SVC-11 … SVC-22, CT-12).
//
// Five routes, and each one is here for the same reason: Paymenter lets an operator do it and
// this panel did not, so the workaround was SQL.
//
//	POST   /api/v1/staff/services/{id}/recalculate-price        SVC-13
//	POST   /api/v1/staff/services/{id}/cancel-subscription      SVC-14
//	POST   /api/v1/staff/orders/{id}/cancel-subscription        CT-12
//	POST   /api/v1/staff/services/{id}/delete-remote            SVC-12
//	POST   /api/v1/staff/services/{id}/notify                   SVC-12
//	DELETE /api/v1/staff/services/{id}                          SVC-12
//	POST   /api/v1/staff/upgrades/apply                         UPG-05
//	GET|PUT /api/v1/staff/services/{id}/provisioning-fields     UPG-12
//
// Three rules shape the file:
//
//  1. **Nothing here calls a provisioner or a gateway.** The remote actions enqueue a
//     `service_operations` row in the same transaction as the local change, and the fenced,
//     leased, reconciled worker is the only component that talks to an adapter. A handler that
//     called the provisioner directly would reintroduce the double-provision the queue exists
//     to prevent.
//  2. **A refusal names the reason.** Deleting a service with billing history, cancelling a
//     subscription that does not exist and notifying a customer with no address are three
//     different problems with three different remedies, so they carry three different codes.
//  3. **Every action is audited inside its transaction.** The audit row cannot outlive a
//     rollback, which is what makes "who deleted this service" answerable at all.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("70-service-actions", registerServiceActions)
}

func registerServiceActions(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/recalculate-price",
		authn.PermServiceWriteAll, r.s.handleRecalculatePrice)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/cancel-subscription",
		authn.PermServiceWriteAll, r.s.handleCancelServiceSubscription)
	r.staffMutation(http.MethodPost, "/api/v1/staff/orders/{id}/cancel-subscription",
		authn.PermServiceWriteAll, r.s.handleCancelOrderSubscription)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/delete-remote",
		authn.PermServiceWriteAll, r.s.handleDeleteRemoteService)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/notify",
		authn.PermServiceWriteAll, r.s.handleNotifyServiceCustomer)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/services/{id}",
		authn.PermServiceWriteAll, r.s.handleDeleteService)
	r.staffMutation(http.MethodPost, "/api/v1/staff/upgrades/apply",
		authn.PermServiceWriteAll, r.s.handleApplyPendingUpgrades)

	r.staffGet("/api/v1/staff/services/{id}/provisioning-fields",
		authn.PermServiceReadAll, r.s.handleGetProvisioningFields)
	r.staffMutation(http.MethodPut, "/api/v1/staff/services/{id}/provisioning-fields",
		authn.PermServiceWriteAll, r.s.handlePutProvisioningFields)
}

// requireServiceID validates a path identifier before any query sees it.
//
// A malformed id is a 404 rather than a 400: the caller asked for a record that cannot exist,
// and the answer must not distinguish "not a uuid" from "not found".
func requireServiceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return "", false
	}
	return id, true
}

// ---------------------------------------------------------------- recalculate --

// recalculatePriceRequest is the body of a recalculation.
type recalculatePriceRequest struct {
	// Apply is the operator's decision. False is the default because the read is the useful
	// half: an operator who wants the comparison must be able to see it before anything moves.
	Apply  bool   `json:"apply"`
	Reason string `json:"reason"`
}

// handleRecalculatePrice recomputes a service's price from the catalog (SVC-13).
func (s *Server) handleRecalculatePrice(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req recalculatePriceRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).RecalculatePrice(r.Context(), id, user.ID, req.Apply)
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	if req.Apply && res.Changed {
		s.publishServiceEvent(r, id, "service.price_recalculated", map[string]any{
			"service_id":            id,
			"to_price_version_id":   res.To.PriceVersionID,
			"to_total_minor":        res.To.TotalMinor,
			"currency":              res.To.Currency,
			"operation_id":          res.OperationID,
			"from_price_version_id": res.From.PriceVersionID,
		})
	}
	out := map[string]any{
		"service_id":       res.ServiceID,
		"subscription_id":  res.SubscriptionID,
		"applied":          req.Apply && res.Changed,
		"changed":          res.Changed,
		"plan_changed":     res.PlanChanged,
		"repriced_options": res.RepricedOptions,
		"from":             priceSnapshotJSON(res.From),
		"to":               priceSnapshotJSON(res.To),
	}
	if res.OperationID != "" {
		out["operation_id"] = res.OperationID
	}
	writeJSON(w, http.StatusOK, out)
}

// priceSnapshotJSON renders one side of a recalculation.
//
// Every amount is the API's `{minor, currency, scale, display}` shape rather than a bare
// integer, so a screen renders it with the same money component every other amount uses. A
// currency outside ISO 4217 cannot be displayed and is left as units plus the code rather than
// guessed at.
func priceSnapshotJSON(p service.PriceSnapshot) map[string]any {
	cur, known := money.LookupCurrency(p.Currency)
	amount := func(minor int64) map[string]any {
		if !known {
			return map[string]any{"minor": minor, "currency": p.Currency}
		}
		return amountJSON(money.New(minor, cur))
	}
	return map[string]any{
		"price_version_id": p.PriceVersionID,
		"version":          p.Version,
		"name":             p.Name,
		"interval":         p.Interval,
		"currency":         p.Currency,
		"plan":             amount(p.PlanMinor),
		"options":          amount(p.OptionsMinor),
		"setup":            amount(p.SetupMinor),
		"total":            amount(p.TotalMinor),
		"plan_id":          p.PlanID,
		"region":           p.Region,
	}
}

// ------------------------------------------------------- cancel a subscription --

// externalSubscriptionRequest is the body of a subscription cancellation.
type externalSubscriptionRequest struct {
	// SubscriptionID is the provider's id. Empty uses the one already stored on the service.
	SubscriptionID string `json:"subscription_id"`
	Reason         string `json:"reason"`
}

// handleCancelServiceSubscription cancels one service's provider-side subscription (SVC-14).
func (s *Server) handleCancelServiceSubscription(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req externalSubscriptionRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).CancelExternalSubscription(r.Context(), service.ExternalSubscriptionRequest{
		ServiceID:      id,
		SubscriptionID: req.SubscriptionID,
		Reason:         req.Reason,
		ActorID:        user.ID,
	})
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.subscription_cancel_requested", map[string]any{
		"service_id": id, "operation_id": res.OperationID,
		"external_subscription_id": res.SubscriptionID,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"service_id":               id,
		"external_subscription_id": res.SubscriptionID,
		"operation_id":             res.OperationID,
		"queued":                   true,
		"status_url":               "/api/v1/staff/operations/" + res.OperationID,
	})
}

// handleCancelOrderSubscription cancels every provider-side subscription of an order (CT-12).
func (s *Server) handleCancelOrderSubscription(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	orderID, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req externalSubscriptionRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	queued, failures, err := s.serviceStore(r).CancelOrderExternalSubscriptions(
		r.Context(), orderID, user.ID, req.Reason)
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(queued))
	for _, q := range queued {
		rows = append(rows, map[string]any{
			"service_id":               q.ServiceID,
			"external_subscription_id": q.SubscriptionID,
			"operation_id":             q.OperationID,
		})
		s.publishServiceEvent(r, q.ServiceID, "service.subscription_cancel_requested", map[string]any{
			"service_id": q.ServiceID, "operation_id": q.OperationID,
			"external_subscription_id": q.SubscriptionID,
		})
	}
	out := map[string]any{"order_id": orderID, "queued": rows, "count": len(rows)}
	if len(failures) > 0 {
		// Reported rather than hidden: a partial success under a green tick is how a customer
		// keeps getting charged for one of their three services.
		out["failures"] = failures
	}
	writeJSON(w, http.StatusAccepted, out)
}

// ----------------------------------------------------------- delete remote --

// handleDeleteRemoteService destroys the provider-side resource and leaves the service (SVC-12).
//
// It is deliberately not the terminate route. Terminate ends the service; this removes the
// server an operator no longer wants managed while the customer keeps what they bought, which is
// what "delete remote" means in the panel this replaces.
func (s *Server) handleDeleteRemoteService(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	var (
		operationID string
		externalID  string
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var (
			status     string
			generation int64
		)
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(external_id, ''), status, generation
			FROM services WHERE id = $1 FOR UPDATE`, id).Scan(&externalID, &status, &generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return service.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}
		if externalID == "" {
			return errNoRemoteResource
		}
		payload, err := json.Marshal(map[string]string{"reason": req.Reason})
		if err != nil {
			return fmt.Errorf("service: encode delete_remote payload: %w", err)
		}
		key := fmt.Sprintf("delete_remote:%s:%d", id, generation)
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status, payload)
			VALUES ($1, 'delete_remote', $2, $3, 'queued', $4::jsonb)
			ON CONFLICT (operation_key) DO UPDATE SET payload = EXCLUDED.payload
			RETURNING id`, id, key, generation, payload).Scan(&operationID); err != nil {
			return fmt.Errorf("service: enqueue delete_remote operation: %w", err)
		}
		// The request and the outcome are two audit rows with two actions: "somebody asked for
		// this" and "the provider did it" are different facts, and a single name would make the
		// trail unable to say which happened.
		return s.auditorFor(r, req.Reason).RecordAction(ctx, tx, "service.delete_remote_requested",
			"service", id, map[string]any{
				"operation_id": operationID,
				"external_id":  externalID,
				"actor":        user.ID,
			})
	})
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.delete_remote", map[string]any{
		"service_id": id, "operation_id": operationID,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"service_id":        id,
		"operation_id":      operationID,
		"external_id":       externalID,
		"queued":            true,
		"status_url":        "/api/v1/staff/operations/" + operationID,
		"service_untouched": true,
	})
}

// errNoRemoteResource is a service that was never provisioned, so there is nothing remote to
// delete. It is not an error the operator caused, so it gets its own code.
var errNoRemoteResource = errors.New("this service has no remote resource")

// --------------------------------------------------------------- delete --

// handleDeleteService removes a service that has no history (SVC-12).
//
// The condition is the whole safety argument: a service with invoices, operations, upgrades,
// cancellations, holds or tickets behind it is a record of things that happened, and deleting it
// would orphan or cascade away part of the ledger. Such a service is terminated, not deleted, and
// the refusal says so.
func (s *Server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	// The reason travels in a DELETE body when a client sends one, and in the query otherwise.
	// It is not required: a delete of a service with no history is reversible by re-creating it,
	// and demanding prose for that would train operators to type ".".
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if !s.decodeJSON(w, r, &req, 16<<10) {
			return
		}
	}
	user, _ := userFrom(r.Context())

	var blockers []string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, id).
			Scan(&exists); err != nil {
			return fmt.Errorf("service: read service: %w", err)
		}
		if !exists {
			return service.ErrNotFound
		}
		found, err := serviceBlockers(ctx, tx, id)
		if err != nil {
			return err
		}
		blockers = found
		if len(blockers) > 0 {
			return errServiceHasHistory
		}
		// The dependent rows cascade or are checked above, so this is one statement.
		if _, err := tx.Exec(ctx, `DELETE FROM services WHERE id = $1`, id); err != nil {
			return fmt.Errorf("service: delete service: %w", err)
		}
		return s.auditorFor(r, req.Reason).RecordAction(ctx, tx, "service.delete", "service",
			id, map[string]any{"actor": user.ID, "reason": req.Reason})
	})
	if err != nil {
		if errors.Is(err, errServiceHasHistory) {
			writeProblem(w, http.StatusConflict, "service_has_history",
				"this service has "+strings.Join(blockers, ", ")+" behind it; terminate it instead, "+
					"which keeps the history")
			return
		}
		s.writeServiceActionError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.deleted", map[string]any{"service_id": id})
	writeJSON(w, http.StatusOK, map[string]any{
		"service_id": id,
		"deleted":    true,
	})
}

// errServiceHasHistory is a service with records behind it. Terminating keeps those records.
var errServiceHasHistory = errors.New("this service has billing history and must be terminated, not deleted")

// serviceBlockers lists the records that make a service undeletable.
//
// Each entry is a human sentence rather than a table name: the operator is being told what to do
// instead, and "invoices" alone does not explain that the remedy is a termination.
func serviceBlockers(ctx context.Context, tx pgx.Tx, serviceID string) ([]string, error) {
	checks := []struct {
		label string
		sql   string
	}{
		{"invoices", `SELECT EXISTS (
			SELECT 1 FROM invoices i
			JOIN services s ON s.subscription_id = i.subscription_id
			WHERE s.id = $1)`},
		{"provisioning operations", `SELECT EXISTS (
			SELECT 1 FROM service_operations WHERE service_id = $1)`},
		{"upgrades", `SELECT EXISTS (SELECT 1 FROM service_upgrades WHERE service_id = $1)`},
		{"cancellations", `SELECT EXISTS (SELECT 1 FROM service_cancellations WHERE service_id = $1)`},
		{"holds", `SELECT EXISTS (SELECT 1 FROM service_holds WHERE service_id = $1)`},
		{"tickets", `SELECT EXISTS (SELECT 1 FROM tickets WHERE service_id = $1)`},
		{"suspension decisions", `SELECT EXISTS (SELECT 1 FROM suspension_decisions WHERE service_id = $1)`},
	}
	var out []string
	for _, check := range checks {
		var exists bool
		if err := tx.QueryRow(ctx, check.sql, serviceID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("service: check %s: %w", check.label, err)
		}
		if exists {
			out = append(out, check.label)
		}
	}
	return out, nil
}

// --------------------------------------------------------------- notify --

// notifyRequest is the body of a staff message to a customer.
type notifyRequest struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// handleNotifyServiceCustomer sends the customer a message about their service (SVC-12).
//
// The panel could not send one at all, so an operator who needed to tell a customer something
// about a service had to use a mail client — outside the audit trail and outside the customer's
// inbox in the portal. This writes the outbox row and the portal notification together.
//
// The dedupe key is deliberately empty. Every other event in the outbox is keyed so a retried
// business event cannot send twice; this one is a message a human explicitly asked for, and
// suppressing the second copy would make the button lie.
func (s *Server) handleNotifyServiceCustomer(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req notifyRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	subject := strings.TrimSpace(req.Subject)
	body := strings.TrimSpace(req.Body)
	if subject == "" || len(subject) > 200 {
		writeProblem(w, http.StatusBadRequest, "invalid_subject",
			"subject is required and must be at most 200 characters")
		return
	}
	if body == "" || len(body) > 8000 {
		writeProblem(w, http.StatusBadRequest, "invalid_body",
			"body is required and must be at most 8000 characters")
		return
	}

	var (
		email     string
		customer  string
		delivery  string
		productID string
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT c.email, c.name, s.product_id
			FROM services s JOIN customers c ON c.id = s.customer_id
			WHERE s.id = $1`, id).Scan(&email, &customer, &productID)
		if errors.Is(err, pgx.ErrNoRows) {
			return service.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: read service customer: %w", err)
		}
		if strings.TrimSpace(email) == "" {
			return errNoCustomerEmail
		}
		rowID, _, err := s.notify.Enqueue(ctx, tx, notify.Message{
			Kind:    "staff_service_message",
			To:      email,
			Subject: subject,
			Body:    body,
		})
		if err != nil {
			return fmt.Errorf("service: enqueue customer message: %w", err)
		}
		delivery = rowID
		return s.auditorFor(r, "").RecordAction(ctx, tx, "service.notify", "service", id,
			map[string]any{
				"to":          email,
				"subject":     subject,
				"delivery_id": rowID,
			})
	})
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"service_id":  id,
		"customer":    customer,
		"to":          email,
		"delivery_id": delivery,
		"queued":      true,
	})
}

// errNoCustomerEmail is a customer record with no address, which is a data problem rather than a
// mail failure.
var errNoCustomerEmail = errors.New("the customer has no email address")

// ------------------------------------------------------- pending upgrades --

// handleApplyPendingUpgrades applies every paid upgrade now (UPG-05).
//
// The schedule for this is a job (see internal/service.ApplyPendingUpgrades); this route exists
// because a paid upgrade that has not been applied is a customer running on the old size, and an
// operator needs a way to fix it without waiting for a tick or reaching for SQL. Both call the
// same function, so the manual path and the scheduled one cannot disagree.
func (s *Server) handleApplyPendingUpgrades(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	applied, err := s.serviceStore(r).ApplyPendingUpgrades(r.Context())
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"count":   len(applied),
	})
}

// ----------------------------------------------------- provisioning fields --

// handleGetProvisioningFields reports the adapter's declared inputs and this service's values
// (UPG-12).
func (s *Server) handleGetProvisioningFields(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	values, err := s.storedProvisioningFields(r.Context(), id)
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	declared, readErr := provisioning.ReadProvisionerFields(r.Context(), s.pool, integrationTestTimeout)
	out := map[string]any{
		"service_id":   id,
		"provider":     declared.Provider,
		"extension":    declared.Extension,
		"supported":    declared.Supported,
		"fields":       provisioningFieldJSON(declared.Fields),
		"values":       values,
		"configured":   declared.Provider != "",
		"capabilities": nonNilStrings(declared.Capabilities),
	}
	if readErr != nil && !errors.Is(readErr, provisioning.ErrNotConfigured) {
		// A provisioner that cannot be reached still leaves the stored values readable: the screen
		// must not go blank because a panel is down. The failure travels alongside so the screen
		// can say why the declaration is missing rather than implying it declared nothing.
		out["ok"] = false
		out["detail"] = readErr.Error()
	} else {
		out["ok"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutProvisioningFields stores the adapter's inputs on a service (UPG-12).
//
// The keys are checked against what the extension declared, because an adapter's create call
// reads `metadata[key]` and an unknown key would be stored and then never sent. A required field
// that is missing is refused here rather than after the purchase, which is the difference between
// a form that works and a provisioning failure an hour later.
func (s *Server) handlePutProvisioningFields(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := requireServiceID(w, r)
	if !ok {
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	if req.Values == nil {
		req.Values = map[string]string{}
	}

	declared, err := provisioning.ReadProvisionerFields(r.Context(), s.pool, integrationTestTimeout)
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	byKey := make(map[string]extproto.ProvisionField, len(declared.Fields))
	for _, field := range declared.Fields {
		byKey[field.Key] = field
	}
	for key := range req.Values {
		if _, ok := byKey[key]; !ok {
			writeProblem(w, http.StatusBadRequest, "unknown_field",
				fmt.Sprintf("the %s adapter does not declare a field named %q", declared.Provider, key))
			return
		}
	}
	for _, field := range declared.Fields {
		if field.Required && strings.TrimSpace(req.Values[field.Key]) == "" {
			writeProblem(w, http.StatusBadRequest, "required_field",
				fmt.Sprintf("%s is required by the %s adapter", field.Label, declared.Provider))
			return
		}
	}

	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, id).
			Scan(&exists); err != nil {
			return fmt.Errorf("service: read service: %w", err)
		}
		if !exists {
			return service.ErrNotFound
		}
		// The values live in `services.config.fields` rather than in custom_properties: they are
		// the provisioner's own inputs, sent to the adapter verbatim, and rendering them as
		// service metadata (or letting a customer edit them) would be a different feature.
		encoded, err := json.Marshal(map[string]any{"fields": req.Values})
		if err != nil {
			return fmt.Errorf("service: encode provisioning fields: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE services
			SET config = jsonb_set(config, '{fields}', COALESCE($2::jsonb -> 'fields', '{}'::jsonb), true),
			    updated_at = now()
			WHERE id = $1`, id, encoded); err != nil {
			return fmt.Errorf("service: store provisioning fields: %w", err)
		}
		return s.auditorFor(r, "").RecordAction(ctx, tx, "service.provisioning_fields", "service",
			id, map[string]any{"provider": declared.Provider, "fields": sortedKeys(req.Values)})
	})
	if err != nil {
		s.writeServiceActionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service_id": id,
		"provider":   declared.Provider,
		"values":     req.Values,
		"saved":      true,
	})
}

// storedProvisioningFields reads the values already stored on a service.
func (s *Server) storedProvisioningFields(ctx context.Context, serviceID string) (map[string]string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT config FROM services WHERE id = $1`, serviceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, service.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("service: read service config: %w", err)
	}
	var config struct {
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return map[string]string{}, nil
	}
	if config.Fields == nil {
		return map[string]string{}, nil
	}
	return config.Fields, nil
}

// provisioningFieldJSON renders a declared field for the wire.
func provisioningFieldJSON(fields []extproto.ProvisionField) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		row := map[string]any{
			"key":      f.Key,
			"label":    f.Label,
			"type":     string(f.Type),
			"required": f.Required,
			"secret":   f.Secret,
		}
		if len(f.Choices) > 0 {
			row["choices"] = f.Choices
		}
		if f.Hint != "" {
			row["hint"] = f.Hint
		}
		out = append(out, row)
	}
	return out
}

// nonNilStrings renders an absent list as an empty one, so a client can iterate it.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// sortedKeys is the audit trail's view of which fields were written.
func sortedKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// writeServiceActionError maps this file's refusals onto stable codes.
func (s *Server) writeServiceActionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, errServiceHasHistory):
		writeProblem(w, http.StatusConflict, "service_has_history",
			"this service has invoices, operations or other records behind it; terminate it instead, "+
				"which keeps the history")
	case errors.Is(err, errNoRemoteResource):
		writeProblem(w, http.StatusConflict, "no_remote_resource",
			"this service was never provisioned, so there is nothing remote to delete")
	case errors.Is(err, errNoCustomerEmail):
		writeProblem(w, http.StatusConflict, "no_email_address",
			"the customer has no email address, so nothing can be sent")
	case errors.Is(err, service.ErrNoExternalSubscription):
		writeProblem(w, http.StatusConflict, "no_external_subscription",
			"this service has no external subscription id; supply one, or the customer is billed by this panel alone")
	case errors.Is(err, service.ErrNotCancellable):
		writeProblem(w, http.StatusConflict, "not_cancellable", err.Error())
	case errors.Is(err, provisioning.ErrNotConfigured):
		writeProblem(w, http.StatusUnprocessableEntity, "provider_required",
			"no provisioning server is configured yet, so it has no fields to declare")
	case errors.Is(err, service.ErrInvalidInput):
		writeProblem(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, service.ErrNotUpgradable):
		writeProblem(w, http.StatusConflict, "not_recalculable", err.Error())
	default:
		s.staffError(w, r, err)
	}
}
