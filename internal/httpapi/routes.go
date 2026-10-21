package httpapi

import (
	"net/http"
	"sort"
	"sync"

	"github.com/catalyst/billing/internal/authn"
)

// Route registration.
//
// Every route is registered through this file rather than by calling mux.HandleFunc
// directly, for three reasons that all matter once the surface is large:
//
//  1. The route table is *data*. A test can enumerate it, which is what makes "every
//     staff route declares a permission" (F3), "every error is problem+json" (E4) and
//     "the OpenAPI document matches the router" (E1) provable instead of aspirational.
//  2. The cross-cutting wrappers (authentication, permission, CSRF, idempotency, rate
//     limiting) are applied by the registrar, so a new route cannot accidentally ship
//     without one. Forgetting `requireCSRF` used to be a silent CSRF hole.
//  3. Feature files register themselves through RegisterRoutes in an init function,
//     so two people adding two features do not edit the same list.
//
// Registration order is by name, not by init order: Go does not define the order of
// init calls across files, and a route table that reshuffles between builds would make
// the generated OpenAPI document unstable.

// Access is what a request must satisfy before a route's handler runs.
type Access int

const (
	// AccessPublic needs no session. Health, readiness, the webhook endpoint and the
	// credential entry points are public: an orchestrator has no cookie and a provider
	// has a signature instead.
	AccessPublic Access = iota
	// AccessUser needs a session but no particular permission. This is for "who am I"
	// style routes, where the answer is scoped to the caller by construction.
	AccessUser
	// AccessPermission needs a session and a declared permission.
	AccessPermission
)

// RateClass buckets routes for the per-principal limiter. The classes exist because a
// bulk export and a single read should not share a budget.
type RateClass string

const (
	RateNone     RateClass = ""
	RateRead     RateClass = "read"
	RateMutation RateClass = "mutation"
	RateExport   RateClass = "export"
)

// Route is one entry in the table.
type Route struct {
	Method  string
	Pattern string
	Access  Access
	// Permission is the declared permission. It is empty for public and
	// authenticated-only routes, and required for anything under /api/v1/staff or
	// /api/v1/portal (a test enforces that).
	Permission authn.Permission
	// StaffOnly additionally requires a staff role. It is defence in depth on top of
	// the permission check: no customer role holds a staff permission today, and this
	// makes sure that stays true even if one is added by mistake.
	StaffOnly bool
	// CSRF marks a state-changing route. Set for every non-GET route that runs as a
	// session.
	CSRF bool
	// Idempotent marks a mutation that stores and replays responses by
	// Idempotency-Key.
	Idempotent bool
	// IdempotencyRequired rejects a mutation that does not present a key. It is used
	// for money actions, where a client retry without a key is a double refund.
	IdempotencyRequired bool
	Rate                RateClass
	Handler             http.HandlerFunc
}

// Path renders the route the way the router and the OpenAPI document name it.
func (r Route) Path() string { return r.Method + " " + r.Pattern }

// routeRegistrar is one feature's registration function.
type routeRegistrar struct {
	name     string
	register func(*router)
}

var (
	registrarsMu sync.Mutex
	registrars   []routeRegistrar
)

// RegisterRoutes adds a feature's routes to every Server built from now on.
//
// It is called from init in the feature's own file. The name orders the route table
// deterministically; it has no other meaning.
func RegisterRoutes(name string, fn func(*router)) {
	registrarsMu.Lock()
	defer registrarsMu.Unlock()
	registrars = append(registrars, routeRegistrar{name: name, register: fn})
}

// router is the per-Server route builder.
type router struct {
	s      *Server
	mux    *http.ServeMux
	routes []Route
}

// add wraps a route with the cross-cutting middleware its declaration implies and
// registers it.
func (r *router) add(rt Route) {
	h := rt.Handler
	if rt.Access == AccessPermission {
		h = r.s.requirePermission(rt.Permission, h)
	}
	if rt.StaffOnly {
		h = r.s.requireStaff(h)
	}
	if rt.CSRF {
		h = r.s.requireCSRF(h)
	}
	if rt.Idempotent {
		h = r.s.withIdempotency(rt, h)
	}
	if rt.Rate != RateNone {
		h = r.s.withRateLimit(rt, h)
	}
	r.mux.HandleFunc(rt.Method+" "+rt.Pattern, h)
	r.routes = append(r.routes, rt)
}

// publicGet registers an unauthenticated read.
func (r *router) publicGet(pattern string, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodGet, Pattern: pattern, Access: AccessPublic, Handler: h})
}

// publicPost registers an unauthenticated mutation. CSRF is not applied because there
// is no session to bind a token to; these handlers enforce same-origin themselves.
func (r *router) publicPost(pattern string, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodPost, Pattern: pattern, Access: AccessPublic, Handler: h})
}

// userGet registers a read that needs a session but no permission.
func (r *router) userGet(pattern string, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodGet, Pattern: pattern, Access: AccessUser,
		Rate: RateRead, Handler: h})
}

// userMutation registers a state-changing route that needs a session but no
// permission (a user acting on their own account).
func (r *router) userMutation(method, pattern string, idempotent bool, h http.HandlerFunc) {
	r.add(Route{Method: method, Pattern: pattern, Access: AccessUser, CSRF: true,
		Idempotent: idempotent, Rate: RateMutation, Handler: h})
}

// get registers a permissioned read.
func (r *router) get(pattern string, p authn.Permission, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodGet, Pattern: pattern, Access: AccessPermission,
		Permission: p, Rate: RateRead, Handler: h})
}

// staffGet registers a permissioned staff read.
func (r *router) staffGet(pattern string, p authn.Permission, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodGet, Pattern: pattern, Access: AccessPermission,
		Permission: p, StaffOnly: true, Rate: RateRead, Handler: h})
}

// staffExport registers a staff read that is expensive by nature (a full ledger or
// audit dump) and therefore gets its own, tighter budget.
func (r *router) staffExport(pattern string, p authn.Permission, h http.HandlerFunc) {
	r.add(Route{Method: http.MethodGet, Pattern: pattern, Access: AccessPermission,
		Permission: p, StaffOnly: true, Rate: RateExport, Handler: h})
}

// staffMutation registers a permissioned staff mutation with a required idempotency
// key. Every staff mutation requires one: a retried mutation is how money moves twice.
func (r *router) staffMutation(method, pattern string, p authn.Permission, h http.HandlerFunc) {
	r.add(Route{Method: method, Pattern: pattern, Access: AccessPermission,
		Permission: p, StaffOnly: true, CSRF: true,
		Idempotent: true, IdempotencyRequired: true,
		Rate: RateMutation, Handler: h})
}

// mutation registers a permissioned mutation where an idempotency key is accepted but
// not required (a customer-facing action, where the domain operation is already keyed).
func (r *router) mutation(method, pattern string, p authn.Permission, h http.HandlerFunc) {
	r.add(Route{Method: method, Pattern: pattern, Access: AccessPermission,
		Permission: p, CSRF: true, Idempotent: true,
		Rate: RateMutation, Handler: h})
}

// buildRoutes constructs the mux and the route table.
func (s *Server) buildRoutes() (*http.ServeMux, []Route) {
	registrarsMu.Lock()
	regs := make([]routeRegistrar, len(registrars))
	copy(regs, registrars)
	registrarsMu.Unlock()

	sort.Slice(regs, func(i, j int) bool { return regs[i].name < regs[j].name })

	r := &router{s: s, mux: http.NewServeMux()}
	for _, reg := range regs {
		reg.register(r)
	}

	s.routesMu.Lock()
	s.routes = r.routes
	s.routesMu.Unlock()
	return r.mux, r.routes
}

// Routes returns the route table of the most recently built handler.
//
// Tests use it to prove properties of the whole surface (permission coverage, error
// shape, OpenAPI parity). It is not used at request time.
func (s *Server) Routes() []Route {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	return out
}
