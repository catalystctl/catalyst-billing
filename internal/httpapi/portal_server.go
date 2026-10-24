// The live view of a customer's game server, inside the billing panel.
//
// A customer should be able to see that their server exists, what it is doing, where to connect and
// how to restart it without opening a second product. That is what these four routes are for:
//
//	GET  /api/v1/portal/services/{id}/server        the read: name, state, address, panel link, stats
//	POST /api/v1/portal/services/{id}/server/power  start, stop, restart, kill
//	GET  /api/v1/staff/services/{id}/server         the same read, for an operator
//	POST /api/v1/staff/services/{id}/server/power   the same write, for an operator
//
// # Why this is not a service-operation queue row
//
// Every *billing* consequence of a remote call goes through the queue: create, suspend, terminate
// and resize are fenced, leased and reconciled because a duplicate charges somebody twice. A power
// action has no billing consequence at all — it starts a process the customer already pays for — and
// queueing it would mean pressing "start" and waiting for a worker tick. So it is called in the
// request path, bounded, idempotent by the request's own idempotency key, and audited like the
// extension hook calls it sits beside.
//
// The read is in the request path for the same reason the provisioning-fields read is: a page that
// says "unknown" until a background job runs is a page that is wrong most of the time.
//
// # What a failure does not mean
//
// The panel being down is not a statement about the customer's service. So a read that cannot reach
// the provisioner answers 200 with `available: false` and a sentence, rather than a 5xx that would
// make the whole service page look broken. Only the *write* turns a transport failure into an error
// status, because a write that did not happen must not be reported as done.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("76-service-server", registerServiceServer)
}

func registerServiceServer(r *router) {
	// The customer's own service. The route's permission is the floor; the SQL below is what makes
	// another customer's service id indistinguishable from one that does not exist.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/portal/services/{id}/server",
		Access: AccessPermission, Permission: authn.PermServiceReadSelf,
		Rate: RateRead, Handler: r.s.handlePortalServerView,
	})
	r.add(Route{
		Method: http.MethodPost, Pattern: "/api/v1/portal/services/{id}/server/power",
		Access: AccessPermission, Permission: authn.PermServiceWriteSelf,
		CSRF: true, Idempotent: true, IdempotencyRequired: true,
		Rate: RateMutation, Handler: r.s.handlePortalServerPower,
	})

	// An operator's view of the same thing, so support can answer "is it running" without opening
	// the game panel either.
	r.staffGet("/api/v1/staff/services/{id}/server", authn.PermServiceReadAll, r.s.handleStaffServerView)
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/server/power",
		authn.PermServiceWriteAll, r.s.handleStaffServerPower)
}

// serverReadTimeout bounds one live read: a launch plus one HTTP call to the provider, with room for
// a slow panel. It is the same budget the connection test uses.
const serverReadTimeout = integrationTestTimeout

// serverPowerTimeout bounds one power action.
//
// Longer than the read because the provider may synchronously stop a container before answering
// "accepted", and shorter than the point where a browser gives up, so the customer sees a failure
// rather than an abandoned request.
const serverPowerTimeout = 25 * time.Second

// serverTarget is the service a request names, resolved against the caller's own scope.
type serverTarget struct {
	serviceID  string
	customerID string
	// externalID is the provider's id for this service's resource, empty when it was never
	// provisioned (a pending order, or a product with no server behind it).
	externalID string
	status     string
}

// resolveServerTarget reads the service and its provisioned id, scoped to the caller.
//
// The scoping is the whole access-control story for these routes, so it is one query rather than
// fetch-then-check: a service that belongs to somebody else must be a 404 with no other observable
// difference, and a check written after a fetch is one forgotten `if` away from a cross-account
// read.
func (s *Server) resolveServerTarget(w http.ResponseWriter, r *http.Request, customerScoped bool) (serverTarget, bool) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured", "no database is configured")
		return serverTarget{}, false
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such service")
		return serverTarget{}, false
	}

	var (
		target serverTarget
		err    error
	)
	if customerScoped {
		user, ok := userFrom(r.Context())
		if !ok {
			writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
			return serverTarget{}, false
		}
		err = s.pool.QueryRow(r.Context(), `
			SELECT s.customer_id, COALESCE(s.external_id, ''), s.status
			FROM services s
			JOIN customer_accounts ca ON ca.customer_id = s.customer_id
			WHERE s.id = $1 AND ca.user_id = $2`, id, user.ID).
			Scan(&target.customerID, &target.externalID, &target.status)
	} else {
		err = s.pool.QueryRow(r.Context(), `
			SELECT customer_id, COALESCE(external_id, ''), status
			FROM services WHERE id = $1`, id).
			Scan(&target.customerID, &target.externalID, &target.status)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, http.StatusNotFound, "not_found", "no such service")
		return serverTarget{}, false
	case err != nil:
		s.cfg.Logger.ErrorContext(r.Context(), "resolve service for a server view",
			"service_id", id, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the service could not be read; retry")
		return serverTarget{}, false
	}
	target.serviceID = id
	return target, true
}

// handlePortalServerView serves the customer's live server card.
func (s *Server) handlePortalServerView(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveServerTarget(w, r, true)
	if !ok {
		return
	}
	s.writeServerView(w, r, target)
}

// handleStaffServerView serves the operator's live server card.
func (s *Server) handleStaffServerView(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveServerTarget(w, r, false)
	if !ok {
		return
	}
	s.writeServerView(w, r, target)
}

// writeServerView is the one description of a live server, shared by both callers so a customer and
// an operator cannot be told different things about the same server.
func (s *Server) writeServerView(w http.ResponseWriter, r *http.Request, target serverTarget) {
	w.Header().Set("Cache-Control", "no-store")

	out := map[string]any{
		"service_id":     target.serviceID,
		"service_status": target.status,
		// provisioned is false for a service that has no resource yet. It is not an error and the
		// page renders a sentence rather than a failure.
		"provisioned": target.externalID != "",
		"available":   false,
	}
	if target.externalID == "" {
		out["detail"] = "This service has no server yet. It appears here once it has been provisioned."
		writeJSON(w, http.StatusOK, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverReadTimeout)
	defer cancel()
	view, err := provisioning.ReadResource(ctx, s.pool, serverReadTimeout, target.externalID)
	out["provider"] = view.Provider
	out["extension"] = view.Extension
	switch {
	case err != nil:
		// The read failed. That is not a statement about the service, so it is a 200 whose card says
		// why rather than a 5xx that would take the whole page down.
		s.cfg.Logger.WarnContext(r.Context(), "read the live server state",
			"service_id", target.serviceID, "provider", view.Provider, "error", err)
		out["detail"] = serverProblemSentence(err)
		writeJSON(w, http.StatusOK, out)
		return
	case !view.Supported:
		// The configured adapter has no live view. Saying so is better than an empty card that looks
		// like a bug in the panel.
		out["detail"] = "The " + view.Provider + " integration cannot report a server's live state."
		writeJSON(w, http.StatusOK, out)
		return
	}

	res := view.Result
	out["available"] = true
	out["name"] = res.Name
	out["state"] = res.State
	out["status"] = string(res.Status)
	out["address"] = res.Address()
	out["host"] = res.Host
	out["port"] = res.Port
	out["panel_url"] = res.PanelURL
	out["can_power"] = powerActionStrings(res.CanPower)
	if res.PlanID != "" {
		out["plan_id"] = res.PlanID
	}
	if res.Region != "" {
		out["region"] = res.Region
	}
	if !res.ObservedAt.IsZero() {
		out["observed_at"] = res.ObservedAt.UTC()
	}
	if res.Stats != nil {
		out["stats"] = statsJSON(*res.Stats)
	}
	writeJSON(w, http.StatusOK, out)
}

// statsJSON renders the metrics. Every field is optional, and the page renders a dash for a missing
// one: "0 MB" and "no figure" are different statements and only one of them is true.
func statsJSON(stats extproto.ResourceStats) map[string]any {
	out := map[string]any{
		"cpu_percent":      stats.CPUPercent,
		"memory_used_mb":   stats.MemoryUsedMB,
		"memory_limit_mb":  stats.MemoryLimitMB,
		"disk_used_mb":     stats.DiskUsedMB,
		"disk_limit_mb":    stats.DiskLimitMB,
		"network_rx_bytes": stats.NetworkRxBytes,
		"network_tx_bytes": stats.NetworkTxBytes,
	}
	if !stats.ObservedAt.IsZero() {
		out["observed_at"] = stats.ObservedAt.UTC()
	}
	return out
}

// powerActionStrings renders the action set for the wire.
func powerActionStrings(actions []extproto.PowerAction) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, string(action))
	}
	return out
}

// serverProblemSentence turns a provider failure into a sentence a customer can act on.
//
// The provider's own text is not repeated here: it is written for an operator, it can name internal
// hosts, and "catalystctl: launch …: fork/exec: no such file" on a customer's page is noise that
// sounds like their server is broken when it is the billing deployment that is.
func serverProblemSentence(err error) string {
	switch {
	case errors.Is(err, provisioning.ErrNotConfigured):
		return "Server control is not set up yet. Please contact support."
	case errors.Is(err, extproto.ErrTimeout):
		return "The server panel did not answer in time. Your service is unaffected; try again in a moment."
	case errors.Is(err, extproto.ErrNotFound):
		return "The server panel does not recognise this service. Please contact support."
	default:
		return "The server panel could not be reached just now. Your service is unaffected; try again in a moment."
	}
}

// handlePortalServerPower performs one power action for a customer.
func (s *Server) handlePortalServerPower(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveServerTarget(w, r, true)
	if !ok {
		return
	}
	s.powerServer(w, r, target, "service.power")
}

// handleStaffServerPower performs one power action for an operator.
func (s *Server) handleStaffServerPower(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveServerTarget(w, r, false)
	if !ok {
		return
	}
	s.powerServer(w, r, target, "service.power_staff")
}

// powerServer validates, calls and audits one power action.
func (s *Server) powerServer(w http.ResponseWriter, r *http.Request, target serverTarget, auditAction string) {
	var body struct {
		Action string `json:"action"`
	}
	if !s.decodeJSON(w, r, &body, 4<<10) {
		return
	}
	action, ok := extproto.ParsePowerAction(strings.TrimSpace(body.Action))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_action",
			"action must be one of start, stop, restart, kill")
		return
	}
	if target.externalID == "" {
		writeProblem(w, http.StatusConflict, "not_provisioned",
			"this service has no server yet, so it cannot be powered on or off")
		return
	}

	// The request's idempotency key is the extension call's operation key, so a retried press and a
	// replayed response describe the same act rather than two.
	operationKey := "power:" + target.serviceID + ":" + string(action)
	if key := strings.TrimSpace(r.Header.Get(idempotencyHeader)); key != "" {
		operationKey += ":" + key
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverPowerTimeout)
	defer cancel()
	view, err := provisioning.ControlPower(ctx, s.pool, serverPowerTimeout, provisioning.PowerRequest{
		OperationKey: operationKey,
		ExternalID:   target.externalID,
		Action:       action,
	})
	if err != nil {
		s.writePowerError(w, r, err, target, action)
		return
	}

	// The remote effect has happened, so the audit row is written now. There is no local transaction
	// to join — the change lives in the provider — and a successful power action nobody can attribute
	// is exactly what the append-only log exists to prevent.
	event := s.auditEvent(r, auditAction, "service", target.serviceID, audit.OutcomeSuccess).
		WithChanges(map[string]any{
			"action":      string(action),
			"provider":    view.Provider,
			"customer_id": target.customerID,
			"changed":     view.Result.Changed,
		})
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, event)
	}); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "record a power action in the audit log",
			"service_id", target.serviceID, "action", string(action), "error", err)
		writeProblem(w, http.StatusInternalServerError, "audit_unavailable",
			"the action was performed but could not be recorded")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service_id": target.serviceID,
		"action":     string(action),
		"state":      view.Result.State,
		"status":     string(view.Result.Status),
		"changed":    view.Result.Changed,
		"provider":   view.Provider,
	})
}

// writePowerError maps a power failure onto a stable code.
//
// The distinction that matters to a customer: "this cannot be done" (the provider refused, and a
// retry will not help) versus "we could not ask" (retry). Reporting the second as the first teaches
// people the button is broken; reporting the first as the second invites a retry that will never
// work.
func (s *Server) writePowerError(w http.ResponseWriter, r *http.Request, err error,
	target serverTarget, action extproto.PowerAction) {
	log := func() {
		s.cfg.Logger.WarnContext(r.Context(), "server power action failed",
			"service_id", target.serviceID, "action", string(action), "error", err)
	}
	switch {
	case errors.Is(err, provisioning.ErrInvalidPowerRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_action", "that power action is not supported")
	case errors.Is(err, provisioning.ErrNotConfigured):
		writeProblem(w, http.StatusConflict, "provider_not_configured",
			"server control is not configured on this installation")
	case errors.Is(err, provisioning.ErrUnsupportedOperation):
		writeProblem(w, http.StatusConflict, "power_unsupported",
			"the configured server provider does not support power control")
	case errors.Is(err, extproto.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "server_not_found",
			"the server panel does not recognise this service; contact support")
	case errors.Is(err, extproto.ErrRejected):
		writeProblem(w, http.StatusUnprocessableEntity, "power_rejected", boundedDetail(err.Error()))
	case errors.Is(err, extproto.ErrTimeout):
		log()
		writeProblem(w, http.StatusGatewayTimeout, "power_timeout",
			"the server panel did not answer in time; try again in a moment")
	default:
		log()
		writeProblem(w, http.StatusBadGateway, "power_failed",
			"the server panel could not perform this action; try again in a moment")
	}
}
