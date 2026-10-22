// Viewing a customer's portal as a staff member.
//
// The portal's own endpoints are scoped to the caller: identity comes from the session and the
// customer is derived from it, never from the URL. That is the right rule for a customer and
// the wrong one for support: an operator answering "why does my invoice say that" needs to see
// exactly what the customer sees, and the only alternative is asking them to read it out.
//
// So this file adds a *read-only* staff view of the same data. Three properties make it safe:
//
//   - It is a separate route, not a flag on the portal route. `?customer_id=` on
//     /api/v1/portal/overview would mean the portal endpoint could be asked for somebody
//     else's data, and one forgotten permission check would be a data breach. A distinct path
//     cannot be reached by a customer at all, because the permission is different.
//   - It requires customer:read:all. That is the permission that already means "may see any
//     customer", so there is no new privilege to reason about.
//   - It is read-only. Nothing here can pay, cancel, or change billing details: those stay on
//     the customer's own endpoints, where the identity is theirs. Impersonation that can act is
//     a much larger decision than impersonation that can look.
//
// Every view is audited, because "who looked at this customer's account" is a question a
// support team has to be able to answer.

package httpapi

import (
	"context"
	"net/http"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("85-staff-portal-view", func(r *router) {
		r.s.registerStaffPortalView(r)
	})
}

func (s *Server) registerStaffPortalView(r *router) {
	// Each endpoint carries the permission its *data* needs, not the one the page is named
	// after. Infrastructure holds customer:read:all but deliberately not invoice:read:all —
	// gating the invoice list on the customer permission would hand that role every invoice in
	// the system through a screen that was supposed to be a convenience.
	r.staffGet("/api/v1/staff/customers/{id}/portal", authn.PermCustomerReadAll, s.handleStaffPortalOverview)
	r.staffGet("/api/v1/staff/customers/{id}/portal/invoices", authn.PermInvoiceReadAll, s.handleStaffPortalInvoices)
	r.staffGet("/api/v1/staff/customers/{id}/portal/services", authn.PermServiceReadAll, s.handleStaffPortalServices)
	r.staffGet("/api/v1/staff/customers/{id}/portal/billing-profile", authn.PermCustomerReadAll, s.handleStaffPortalProfile)
}

// portalCustomer resolves the customer named in the path, or writes a 404.
//
// The id is validated as a UUID before it reaches the query, so a malformed path cannot turn
// into a database error that reads as a 500.
func (s *Server) portalCustomer(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return "", false
	}
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return "", false
	}
	return id, true
}

func (s *Server) handleStaffPortalOverview(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.portalCustomer(w, r)
	if !ok {
		return
	}
	overview, err := s.portal.OverviewForCustomer(r.Context(), customerID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	s.auditCustomerView(r, customerID, "overview")

	// The overview is a bundle: it carries the balance, the invoices and the services. A role
	// can hold customer:read:all without holding invoice:read:all or service:read:all, so the
	// sections are removed rather than the whole page being refused — an operator who may see
	// the customer but not their billing should see the customer, not an error.
	//
	// Removing a section is the only safe direction here: an absent key cannot be read as an
	// empty list by a client that does not check, because there is nothing to iterate.
	body := portalOverviewJSON(overview)
	if !s.can(r, authn.PermInvoiceReadAll) {
		delete(body, "open_invoices")
		delete(body, "recent_invoices")
		delete(body, "balance")
	}
	if !s.can(r, authn.PermServiceReadAll) {
		delete(body, "services")
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleStaffPortalInvoices(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.portalCustomer(w, r)
	if !ok {
		return
	}
	invoices, err := s.portal.InvoicesForCustomer(r.Context(), customerID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	s.auditCustomerView(r, customerID, "invoices")
	out := make([]map[string]any, 0, len(invoices))
	for _, inv := range invoices {
		out = append(out, invoiceJSON(inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": out})
}

func (s *Server) handleStaffPortalServices(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.portalCustomer(w, r)
	if !ok {
		return
	}
	services, err := s.portal.ServicesForCustomer(r.Context(), customerID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	s.auditCustomerView(r, customerID, "services")
	out := make([]map[string]any, 0, len(services))
	for _, svc := range services {
		out = append(out, serviceJSON(svc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

func (s *Server) handleStaffPortalProfile(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.portalCustomer(w, r)
	if !ok {
		return
	}
	// The profile is a billing record, not a portal aggregate: load it the same way the
	// customer's own endpoint does, so the two cannot disagree about what a profile is.
	profile, err := billing.LoadProfile(r.Context(), s.pool, customerID)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	s.auditCustomerView(r, customerID, "billing_profile")
	writeJSON(w, http.StatusOK, profileResponse(profile))
}

// can reports whether the caller holds a permission.
func (s *Server) can(r *http.Request, p authn.Permission) bool {
	if s.auth == nil || s.auth.Authz == nil {
		return false
	}
	user, ok := userFrom(r.Context())
	if !ok {
		return false
	}
	return s.auth.Authz.Can(user, p)
}

// auditCustomerView records that a staff member looked at a customer's account.
//
// Not because looking is suspicious, but because a support conversation that cannot be tied to
// who read the account is one where "I never saw that" has no answer. The reason is passed as a
// short label so the log says which view was opened, not merely that something was.
func (s *Server) auditCustomerView(r *http.Request, customerID, view string) {
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		ev := s.auditEvent(r, "customer.portal_view", "customer", customerID, audit.OutcomeSuccess).
			WithChanges(map[string]any{"view": view})
		return audit.Record(ctx, tx, ev)
	}); err != nil {
		// A failed audit write must not deny a read the caller is entitled to; the read is not
		// a state change, so there is nothing to roll back.
		s.cfg.Logger.ErrorContext(r.Context(), "audit portal view", "error", err)
	}
}
