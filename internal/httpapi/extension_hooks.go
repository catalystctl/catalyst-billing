package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// Extension hooks over HTTP (E4.7 / GRW-05).
//
// An extension contributes UI as *data*: a navigation item, and an action on a service.
// It ships no code to the browser, so the panel never runs a third party's JavaScript in
// an operator's session. The whole surface is therefore two questions and one call:
//
//	GET  /api/v1/extensions/hooks                      what may this caller see?
//	POST /api/v1/staff/services/{id}/hooks/{ext}/{act}  call one hook, as staff
//	POST /api/v1/portal/services/{id}/hooks/{ext}/{act} call one hook, on your own service
//
// The safety argument is that the call is not arbitrary: the extension declared a method
// in ui.describe, the core refused the declaration unless that method was advertised in
// the handshake, and the core refused any permission it does not itself declare. So the
// worst a hook can ask for is a capability the caller does not hold — which is refused
// here, and audited.
//
// Two properties the handlers must not lose:
//
//   - The permission is the extension's declared one, checked against the caller, not a
//     permission on the route. A route-level permission would have to be the loosest one
//     any hook needs, which would let a hook declared as service:terminate be called by
//     someone holding only service:write:self.
//   - A customer's service id is resolved in SQL against their own account, so a wrong id
//     is a 404 rather than a 403 that confirms the service exists.

func init() {
	RegisterRoutes("75-extension-hooks", registerExtensionHooks)
}

func registerExtensionHooks(r *router) {
	// Readable by every signed-in caller: the answer is filtered to the permissions the
	// caller holds, so a customer sees the portal entries an extension contributes and
	// not the operator ones.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/extensions/hooks",
		Access: AccessUser, Rate: RateRead, Handler: r.s.handleExtensionHooks,
	})

	// Staff: the permission is the action's declared one, resolved in the handler, so the
	// route is dynamic rather than declaring a permission that would be wrong for some
	// hook. It is still staff-only, CSRF-protected and requires an idempotency key.
	r.staffMutationDynamic(http.MethodPost,
		"/api/v1/staff/services/{id}/hooks/{extension}/{action}", r.s.handleStaffHookCall)

	// Customer: their own service. service:write:self is the baseline permission for
	// changing one's own service; the declared permission is checked again inside the
	// handler, so a hook that asks for more than that is refused even though the route
	// let the caller through.
	r.add(Route{
		Method: http.MethodPost, Pattern: "/api/v1/portal/services/{id}/hooks/{extension}/{action}",
		Access: AccessPermission, Permission: authn.PermServiceWriteSelf,
		CSRF: true, Idempotent: true, IdempotencyRequired: true,
		Rate: RateMutation, Handler: r.s.handleCustomerHookCall,
	})
}

// hookEventBuffer is how many broker events may be queued between the broker and the
// extension fan-out. It is a buffer, not a bound on latency: the forwarder only ever does
// non-blocking sends, so it drains immediately and this depth is never the thing that
// makes a request wait.
const hookEventBuffer = 256

// hookCallTimeout bounds one service action. A hook that runs a reboot may legitimately
// take a while; it must not hold a request open indefinitely.
const hookCallTimeout = 15 * time.Second

// hookAttachment is one server's view of the installed extensions.
type hookAttachment struct {
	registry *extproto.Hooks
	stop     context.CancelFunc
}

// attachments holds one hook attachment per Server.
//
// The natural home for this is a field on Server, but Server is declared in server.go,
// which this feature does not own: a parallel slice of work is editing that file, and a
// field added there would be a merge conflict in the one place a mistake is invisible.
// Keying by the server keeps the dependency in this file. A Server lives for the life of
// the process and there is one per process in production, so nothing accumulates.
var (
	hooksMu     sync.Mutex
	attachments = map[*Server]*hookAttachment{}
)

// WithExtensionHooks attaches the registry of installed extensions and starts forwarding
// committed lifecycle events to them.
//
// Wiring it is what turns event emission on. Without it the hook endpoints still answer
// (with nothing installed), and no event is ever delivered — which is the correct
// behaviour for a deployment with no extensions.
func (s *Server) WithExtensionHooks(reg *extproto.Hooks) *Server {
	if reg == nil {
		return s
	}
	att := &hookAttachment{registry: reg}
	if s.events != nil {
		sub := s.events.Subscribe(hookEventBuffer)
		ctx, cancel := context.WithCancel(context.Background())
		att.stop = cancel
		go forwardLifecycleEvents(ctx, sub, reg)
	}

	hooksMu.Lock()
	previous := attachments[s]
	attachments[s] = att
	hooksMu.Unlock()
	if previous != nil && previous.stop != nil {
		previous.stop()
	}
	return s
}

// extensionHooks returns the server's registry, creating an empty one on first use so the
// read endpoint works on a server that was never wired to an extension host.
func (s *Server) extensionHooks() *extproto.Hooks {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	if att, ok := attachments[s]; ok {
		return att.registry
	}
	reg := NewExtensionHooks()
	attachments[s] = &hookAttachment{registry: reg}
	return reg
}

// NewExtensionHooks builds a hook registry wired to this core's permission table.
//
// The permission predicate is the whole safety argument, so it is provided here rather than
// left to the caller: a registry built with a permissive check would let a hook declare a
// capability the panel does not have and cannot enforce. An extension host should build one
// of these at boot, call Register for each launched extension, and hand it to
// Server.WithExtensionHooks.
func NewExtensionHooks() *extproto.Hooks { return extproto.NewHooks(declaredPermission) }

// declaredPermission reports whether p is a permission the core declares. It is the check
// that stops a hook inventing a capability.
func declaredPermission(p string) bool {
	for _, declared := range authn.AllPermissions() {
		if string(declared) == p {
			return true
		}
	}
	return false
}

// forwardLifecycleEvents bridges the in-process broker to the extension fan-out.
//
// The broker's contract is what makes this safe: it is published to *after* a transaction
// commits, so an extension is never told about a change that can still roll back. The
// forwarder does one non-blocking send per event and never calls an extension, so a
// checkout that publishes an order event cannot be delayed by anything on the far side.
func forwardLifecycleEvents(ctx context.Context, sub *events.Subscription, reg *extproto.Hooks) {
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				// The broker dropped this subscriber (it fell behind). Reconnecting
				// would re-read nothing: the durable record is the database, and the
				// extension is told about what happened from now on.
				return
			}
			if !extproto.IsLifecycleKind(ev.Kind) {
				continue
			}
			reg.Emit(extproto.Event{
				Kind:       ev.Kind,
				ID:         ev.ID,
				CustomerID: ev.CustomerID,
				Permission: ev.Permission,
				Data:       ev.Data,
				At:         ev.At,
			})
		}
	}
}

// ------------------------------------------------------------------- reading --

// hookNavJSON is the wire form of one navigation item.
func hookNavJSON(extension string, item extproto.NavItem) map[string]any {
	return map[string]any{
		"extension":  extension,
		"id":         item.ID,
		"label":      item.Label,
		"path":       item.Path,
		"icon":       item.Icon,
		"permission": item.Permission,
	}
}

// hookActionJSON is the wire form of one service action.
func hookActionJSON(extension string, action extproto.ServiceAction) map[string]any {
	out := map[string]any{
		"extension":  extension,
		"id":         action.ID,
		"label":      action.Label,
		"method":     action.Method,
		"permission": action.Permission,
	}
	if action.Confirm != "" {
		out["confirm"] = action.Confirm
	}
	return out
}

// handleExtensionHooks lists the hooks the caller may use.
//
// The filtering is per item, not per extension: one extension may contribute an operator
// nav item and a customer action, and a caller should see exactly the half they can act
// on. A permission the caller does not hold hides the item rather than showing it and
// failing on click, which is the difference between a menu and a trap.
func (s *Server) handleExtensionHooks(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	perms := s.callerPermissions(user)

	nav := make([]map[string]any, 0, 4)
	actions := make([]map[string]any, 0, 4)
	for _, ext := range s.extensionHooks().Extensions() {
		for _, item := range ext.NavItems {
			if !holdsPermission(perms, item.Permission) {
				continue
			}
			nav = append(nav, hookNavJSON(ext.Name, item))
		}
		for _, action := range ext.ServiceActions {
			if !holdsPermission(perms, action.Permission) {
				continue
			}
			actions = append(actions, hookActionJSON(ext.Name, action))
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"nav_items":       nav,
		"service_actions": actions,
	})
}

// callerPermissions resolves the caller's permission set once per request.
//
// A server without an authorizer yields an empty set: nothing is shown rather than
// everything, because "authorization is not configured" must never read as "you may".
func (s *Server) callerPermissions(user authn.User) map[string]struct{} {
	if s.auth == nil || s.auth.Authz == nil {
		return nil
	}
	granted := s.auth.Authz.Permissions(user)
	out := make(map[string]struct{}, len(granted))
	for p := range granted {
		out[string(p)] = struct{}{}
	}
	return out
}

// holdsPermission reports whether the set contains p. An empty set holds nothing.
func holdsPermission(perms map[string]struct{}, p string) bool {
	if p == "" {
		return false
	}
	_, ok := perms[p]
	return ok
}

// ------------------------------------------------------------------ calling --

// handleStaffHookCall runs a declared action against any service.
func (s *Server) handleStaffHookCall(w http.ResponseWriter, r *http.Request) {
	s.callHook(w, r, false)
}

// handleCustomerHookCall runs a declared action against the caller's own service.
func (s *Server) handleCustomerHookCall(w http.ResponseWriter, r *http.Request) {
	s.callHook(w, r, true)
}

// callHook is the one place a hook is invoked, so the guard, the scope, the audit row and
// the error mapping are the same for the operator and the customer path.
func (s *Server) callHook(w http.ResponseWriter, r *http.Request, customerScoped bool) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}

	extension := r.PathValue("extension")
	actionID := r.PathValue("action")
	serviceID := r.PathValue("id")

	registry := s.extensionHooks()
	action, ok := registry.Action(extension, actionID)
	if !ok {
		// One 404 for "no such extension" and "no such action": a caller has no business
		// learning which extensions are installed by probing.
		writeProblem(w, http.StatusNotFound, "hook_not_found",
			"no installed extension offers that action")
		return
	}

	// The declared permission is the guard. The route's own permission (for the portal
	// route) is a floor, not the decision: the extension asked for something specific and
	// the caller must hold exactly that.
	if s.auth == nil || s.auth.Authz == nil {
		writeProblem(w, http.StatusForbidden, "forbidden", "authorization is not configured")
		return
	}
	if err := s.auth.Authz.Require(user, authn.Permission(action.Permission)); err != nil {
		s.recordHookAudit(r, audit.OutcomeDenied, extension, action, serviceID, "")
		writeProblem(w, http.StatusForbidden, "forbidden",
			"you do not have access to this action")
		return
	}
	// A key narrows its owner's permissions and never widens them. The route wrapper
	// applies this to the route's own permission; the declared one is checked here for
	// the same reason, or a read-only key could reboot a service.
	if key, ok := apiKeyFrom(r.Context()); ok && !key.Allows(action.Permission) {
		s.recordHookAudit(r, audit.OutcomeDenied, extension, action, serviceID, "")
		writeProblem(w, http.StatusForbidden, "forbidden", "you do not have access to this action")
		return
	}

	customerID, ok := s.hookServiceOwner(w, r, serviceID, user, customerScoped)
	if !ok {
		return
	}

	// The route wrapper has already insisted on the header; reading it here is what makes
	// the *extension* call idempotent as well, so a retried click returns the first
	// result instead of rebooting twice.
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))

	ctx, cancel := context.WithTimeout(r.Context(), hookCallTimeout)
	defer cancel()
	result, err := registry.Call(ctx, extension, actionID, serviceID, customerID, key)
	if err != nil {
		s.writeHookError(w, r, err, extension, action, serviceID)
		return
	}

	// The remote effect has happened, so the audit row is written now. It is a separate
	// transaction because there is no local transaction to join: the change lives in the
	// extension. Failing to write it fails the response, because a hook call nobody can
	// attribute is exactly what an append-only log exists to prevent.
	if !s.recordHookAudit(r, audit.OutcomeSuccess, extension, action, serviceID, customerID) {
		writeProblem(w, http.StatusInternalServerError, "audit_unavailable",
			"the action was performed but could not be recorded")
		return
	}

	body := map[string]any{
		"extension":  extension,
		"action":     actionID,
		"method":     action.Method,
		"service_id": serviceID,
	}
	if result.Message != "" {
		body["message"] = result.Message
	}
	if result.Data != nil {
		body["data"] = result.Data
	}
	writeJSON(w, http.StatusOK, body)
}

// hookServiceOwner resolves the service and reports whether the caller may act on it.
//
// For a customer the lookup is scoped in SQL to their own account, so another customer's
// service id is indistinguishable from one that does not exist. For staff the service
// must simply exist. The customer id is returned because the extension is told whose
// service it is acting on, and because the audit row records it.
func (s *Server) hookServiceOwner(w http.ResponseWriter, r *http.Request, serviceID string,
	user authn.User, customerScoped bool) (string, bool) {
	if !portal.ValidID(serviceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such service")
		return "", false
	}

	var customerID string
	var err error
	if customerScoped {
		err = s.pool.QueryRow(r.Context(), `
			SELECT s.customer_id
			FROM services s
			JOIN customer_accounts ca ON ca.customer_id = s.customer_id
			WHERE s.id = $1 AND ca.user_id = $2`, serviceID, user.ID).Scan(&customerID)
	} else {
		err = s.pool.QueryRow(r.Context(), `
			SELECT customer_id FROM services WHERE id = $1`, serviceID).Scan(&customerID)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, http.StatusNotFound, "not_found", "no such service")
		return "", false
	case err != nil:
		s.cfg.Logger.ErrorContext(r.Context(), "resolve service for a hook call",
			"service_id", serviceID, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the service could not be read; retry")
		return "", false
	}
	return customerID, true
}

// writeHookError maps an extension failure onto a stable code.
//
// The distinction the panel needs is "the extension said no" (the operator can act on it)
// versus "the extension is broken" (an operator has to look at it). Both must be
// problem+json with a code a client can branch on, and neither may leak the extension's
// internals into the response body beyond what the extension itself said.
func (s *Server) writeHookError(w http.ResponseWriter, r *http.Request, err error,
	extension string, action extproto.ServiceAction, serviceID string) {
	switch {
	case errors.Is(err, extproto.ErrHookNotFound):
		writeProblem(w, http.StatusNotFound, "hook_not_found", "no installed extension offers that action")
	case errors.Is(err, extproto.ErrRejected):
		writeProblem(w, http.StatusUnprocessableEntity, "hook_rejected",
			"the extension refused this action: "+boundedDetail(err.Error()))
	case errors.Is(err, extproto.ErrTimeout):
		writeProblem(w, http.StatusGatewayTimeout, "hook_timeout",
			"the extension did not answer in time")
	case errors.Is(err, extproto.ErrCrashed), errors.Is(err, extproto.ErrClosed):
		s.cfg.Logger.WarnContext(r.Context(), "extension crashed during a hook call",
			"extension", extension, "method", action.Method, "service_id", serviceID, "error", err)
		writeProblem(w, http.StatusBadGateway, "extension_unavailable",
			"the extension is not running")
	case errors.Is(err, extproto.ErrNotRunning), errors.Is(err, extproto.ErrDisabled):
		writeProblem(w, http.StatusServiceUnavailable, "extension_unavailable",
			"the extension is not available")
	case errors.Is(err, extproto.ErrInvalidParams):
		// The core built a payload the protocol forbids; that is our bug, not the
		// caller's, so it is a 500 and it is logged.
		s.cfg.Logger.ErrorContext(r.Context(), "hook call was refused by the protocol",
			"extension", extension, "method", action.Method, "error", err)
		writeProblem(w, http.StatusInternalServerError, "hook_failed",
			"the action could not be performed")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "hook call failed",
			"extension", extension, "method", action.Method, "service_id", serviceID, "error", err)
		writeProblem(w, http.StatusBadGateway, "hook_failed",
			"the extension could not perform this action")
	}
}

// boundedDetail trims an extension-supplied message before it reaches a response body.
// The text is rendered as text by the client, but an unbounded string in a problem
// document is still an unbounded string.
func boundedDetail(msg string) string {
	const max = 200
	msg = strings.TrimSpace(msg)
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

// recordHookAudit writes the audit row for a hook call.
//
// It returns false when the row could not be written, so the caller can refuse to report
// success for an action nobody can attribute.
func (s *Server) recordHookAudit(r *http.Request, outcome string, extension string,
	action extproto.ServiceAction, serviceID, customerID string) bool {
	if s.pool == nil {
		return false
	}
	changes := map[string]any{
		"extension": extension,
		"action":    action.ID,
		"method":    action.Method,
	}
	if customerID != "" {
		changes["customer_id"] = customerID
	}
	// A path identifier that is not a UUID cannot name a record, and handing it to the
	// insert would produce a cast error instead of an audit row.
	target := serviceID
	if !portal.ValidID(target) {
		target = ""
	}
	// The event is built here rather than through requestAuditor because the outcome is
	// the point: that helper records every action it is given as a success, which is right
	// for a handler whose failure path returns before it audits, and wrong here.
	event := s.auditEvent(r, "extension.hook_call", "service", target, outcome).WithChanges(changes)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		// A denied call is refused whatever happens to the audit row: telling the caller
		// "the extension is unavailable" because the audit insert failed would leak more
		// than it protects. A successful call, by contrast, must not be reported as done
		// when it left no trace.
		s.cfg.Logger.ErrorContext(r.Context(), "record hook audit",
			"extension", extension, "service_id", serviceID, "outcome", outcome, "error", err)
		return outcome != audit.OutcomeSuccess
	}
	return true
}
