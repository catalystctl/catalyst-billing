package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
)

// Customer-facing endpoints.
//
// Every handler resolves the customer from the *session*, never from a path or
// query parameter, and the manager's queries are scoped in SQL. That combination is
// what makes identifier substitution useless rather than merely detected.

// portalManager returns the portal manager, or nil when it is not configured.
func (s *Server) portalManager() *portal.Manager { return s.portal }

// amountJSON is the wire form of a money amount: minor units plus currency.
func amountJSON(a money.Amount) map[string]any {
	return map[string]any{
		"minor":    a.Minor(),
		"currency": a.Currency().Code(),
		"scale":    a.Currency().Scale(),
		"display":  a.Format(),
	}
}

func invoiceJSON(inv portal.Invoice) map[string]any {
	out := map[string]any{
		"id":       inv.ID,
		"number":   inv.Number,
		"status":   inv.Status,
		"currency": inv.Currency,
		"total":    amountJSON(inv.Total),
		"paid":     amountJSON(inv.Paid),
		"due":      amountJSON(inv.Due),
		"overdue":  inv.Overdue,
		"payable":  inv.Payable,
	}
	if inv.IssuedAt != nil {
		out["issued_at"] = inv.IssuedAt.UTC()
	}
	if inv.DueAt != nil {
		out["due_at"] = inv.DueAt.UTC()
	}
	return out
}

func serviceJSON(svc portal.Service) map[string]any {
	out := map[string]any{
		"id":      svc.ID,
		"name":    svc.Name,
		"status":  svc.Status,
		"actions": svc.Actions,
	}
	if svc.NextBill != nil {
		out["next_bill_at"] = svc.NextBill.UTC()
	}
	return out
}

// handlePortalOverview serves the customer landing page.
func (s *Server) handlePortalOverview(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	overview, err := s.portal.Overview(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, portalOverviewJSON(overview))
}

// portalOverviewJSON is the one description of a customer's portal landing page.
//
// Shared by the customer's own endpoint and the staff read-only view: two functions that build
// the same object would drift, and the whole point of the staff view is that an operator sees
// what the customer sees.
func portalOverviewJSON(overview portal.Overview) map[string]any {
	outstanding := make([]map[string]any, 0, len(overview.Balance.Outstanding))
	for _, a := range overview.Balance.Outstanding {
		outstanding = append(outstanding, amountJSON(a))
	}
	credit := make([]map[string]any, 0, len(overview.Balance.Credit))
	for _, a := range overview.Balance.Credit {
		credit = append(credit, amountJSON(a))
	}
	services := make([]map[string]any, 0, len(overview.Upcoming))
	for _, svc := range overview.Upcoming {
		services = append(services, serviceJSON(svc))
	}
	open := make([]map[string]any, 0, len(overview.OpenInvoices))
	for _, inv := range overview.OpenInvoices {
		open = append(open, invoiceJSON(inv))
	}
	recent := make([]map[string]any, 0, len(overview.RecentInvoices))
	for _, inv := range overview.RecentInvoices {
		recent = append(recent, invoiceJSON(inv))
	}

	return map[string]any{
		"customer": map[string]any{
			"id":    overview.Customer.ID,
			"name":  overview.Customer.Name,
			"email": overview.Customer.Email,
		},
		"balance": map[string]any{
			"outstanding": outstanding,
			"credit":      credit,
		},
		"services":        services,
		"open_invoices":   open,
		"recent_invoices": recent,
	}
}

// handlePortalInvoices lists the customer's invoices.
func (s *Server) handlePortalInvoices(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	invoices, err := s.portal.Invoices(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(invoices))
	for _, inv := range invoices {
		out = append(out, invoiceJSON(inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": out})
}

// handlePortalInvoice serves one invoice.
func (s *Server) handlePortalInvoice(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	inv, err := s.portal.Invoice(r.Context(), user.ID, id)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invoiceJSON(inv))
}

// handlePortalServices lists the customer's services.
func (s *Server) handlePortalServices(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	services, err := s.portal.Services(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(services))
	for _, svc := range services {
		out = append(out, serviceJSON(svc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// handlePortalService serves one service.
func (s *Server) handlePortalService(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	svc, err := s.portal.Service(r.Context(), user.ID, id)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceJSON(svc))
}

// handlePortalCancel schedules cancellation at the end of the paid period.
func (s *Server) handlePortalCancel(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	endsAt, err := s.portal.RequestCancellation(r.Context(), user.ID, id)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "cancellation_scheduled",
		// State the consequence plainly: the service keeps running until this date.
		"ends_at": endsAt.UTC(),
		"message": "Your service stays active until the end of the period you have paid for.",
	})
}

// writePortalError maps a portal error to a response.
//
// A record that belongs to someone else is reported exactly like a record that does
// not exist, so the endpoint cannot be used to discover identifiers.
func (s *Server) writePortalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, portal.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, authn.ErrForbidden):
		writeProblem(w, http.StatusForbidden, "forbidden", "you do not have access to this action")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "portal request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}
