package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/authn"
)

// Core routes: the ones that existed before the web-UI gate, plus the operational
// endpoints that belong to the server itself rather than to a feature slice.
//
// They are registered through the same table as everything else so that the permission
// coverage test (F3) and the OpenAPI parity test (E1) see them. The routes that used to
// be registered inline in Handler() are here, unchanged in behaviour.

func init() {
	RegisterRoutes("10-core", registerCore)
}

func registerCore(r *router) {
	// Public: liveness and readiness must work without a session, and a webhook arrives
	// with a signature rather than a cookie.
	r.publicGet("/healthz", r.s.handleHealth)
	r.publicGet("/readyz", r.s.handleReady)
	r.publicPost("/webhooks/{provider}", r.s.handleWebhook)

	// Metrics and the operator overview expose money and customer counts, so they
	// require staff authentication.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/metrics", Access: AccessUser,
		StaffOnly: true, Rate: RateRead, Handler: r.s.handleMetrics,
	})
	r.staffGet("/api/v1/ops/overview", authn.PermReportRead, r.s.handleOverview)

	// Authentication. Login is public because it is how a session is obtained; logout
	// requires the session it destroys.
	r.publicPost("/api/v1/auth/login", r.s.handleLogin)
	r.userMutation(http.MethodPost, "/api/v1/auth/logout", false, r.s.handleLogout)
	r.userGet("/api/v1/auth/me", r.s.handleMe)

	// Customer portal. Identity comes from the session, never from the URL, and the
	// cancellation action additionally requires a CSRF token because it changes state.
	r.get("/api/v1/portal/overview", authn.PermCustomerReadSelf, r.s.handlePortalOverview)
	r.get("/api/v1/portal/invoices", authn.PermInvoiceReadSelf, r.s.handlePortalInvoices)
	r.get("/api/v1/portal/invoices/{id}", authn.PermInvoiceReadSelf, r.s.handlePortalInvoice)
	r.get("/api/v1/portal/services", authn.PermServiceReadSelf, r.s.handlePortalServices)
	r.get("/api/v1/portal/services/{id}", authn.PermServiceReadSelf, r.s.handlePortalService)
	r.mutation(http.MethodPost, "/api/v1/portal/services/{id}/cancel",
		authn.PermServiceCancelSelf, r.s.handlePortalCancel)

	// Streaming. Long-lived and therefore exempt from the request budget: a limiter that
	// counts connections would disconnect an idle operator who is behaving correctly.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/events", Access: AccessUser,
		Handler: r.s.handleEvents,
	})

	// The SPA. Registered last in the table and matched last by the mux, so it can never
	// shadow an API route.
	r.publicGet("/", r.s.handleStatic)
}
