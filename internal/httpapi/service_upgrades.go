// Upgrades, downgrades and config-option changes over HTTP (UPG-01…UPG-06, CAT-09, SVC-10).
//
// The pricing lives in internal/service; what lives here is the request policy and the shape
// of the answer. Two details are deliberate:
//
//   - The customer's upgrade options are computed against *their own* service, and the target
//     price is validated against the upgrade graph. A customer cannot move to an unrelated
//     product by guessing a price version id.
//   - The response names the proration explicitly (the fraction of the period that remained
//     and the exact amounts) so a customer who is charged 5.00 for half a month can check the
//     arithmetic instead of trusting it.
package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
)

func init() {
	RegisterRoutes("61-service-upgrades", registerServiceUpgrades)
}

func registerServiceUpgrades(r *router) {
	// Customer.
	r.get("/api/v1/portal/services/{id}/upgrade-options",
		authn.PermServiceUpgradeSelf, r.s.handleUpgradeOptions)
	r.mutation(http.MethodPost, "/api/v1/portal/services/{id}/upgrade",
		authn.PermServiceUpgradeSelf, r.s.handleUpgrade)

	// Staff.
	r.staffMutation(http.MethodPost, "/api/v1/staff/services/{id}/upgrade",
		authn.PermServiceUpgrade, r.s.handleStaffUpgrade)
	r.staffGet("/api/v1/staff/upgrades", authn.PermServiceReadAll, r.s.handleStaffUpgrades)
}

// upgradeRequest is the body of an upgrade call.
type upgradeRequest struct {
	// PriceVersionID is the target price. The product must be reachable from the service's
	// product through product_upgrades.
	PriceVersionID string `json:"price_version_id"`
	Reason         string `json:"reason"`
}

// upgradePriceJSON is one target price with its prorated consequence.
func upgradePriceJSON(p service.UpgradePrice) map[string]any {
	return map[string]any{
		"price_version_id": p.PriceVersionID,
		"version":          p.Version,
		"name":             p.Name,
		"interval":         p.Interval,
		"amount":           amountJSON(p.Amount),
		"setup":            amountJSON(p.Setup),
		"prorated":         amountJSON(p.Prorated),
	}
}

// prorationJSON describes the exact fraction of the period that remained.
//
// The ratio is rendered as a decimal string with the numerator and denominator alongside it,
// so a client can display "12.5 of 30 days" and an auditor can recompute the amount without
// re-running the code.
func prorationJSON(p service.Proration) map[string]any {
	return map[string]any{
		"period_start":   p.PeriodStart.UTC(),
		"period_end":     p.PeriodEnd.UTC(),
		"as_of":          p.AsOf.UTC(),
		"remaining_secs": int64(p.Remaining.Seconds()),
		"total_secs":     int64(p.Total.Seconds()),
		"ratio":          p.Ratio.String(),
	}
}

// upgradeResultJSON is the wire form of an upgrade or downgrade.
func upgradeResultJSON(res service.UpgradeResult) map[string]any {
	out := map[string]any{
		"upgrade_id":            res.UpgradeID,
		"service_id":            res.ServiceID,
		"from_price_version_id": res.FromPriceVersionID,
		"to_price_version_id":   res.ToPriceVersionID,
		"direction":             res.Direction,
		"currency":              res.Currency,
		"prorated_minor":        res.ProratedMinor,
		"status":                res.Status,
		"proration":             prorationJSON(res.Proration),
	}
	if res.InvoiceID != "" {
		out["invoice_id"] = res.InvoiceID
		out["invoice_number"] = res.InvoiceNumber
	}
	if res.CreditPending {
		// Said plainly rather than hidden: the customer is owed money and the credit has not
		// been posted yet. The integrator's credit posting is what closes it.
		out["credit_minor"] = res.CreditMinor
		out["credit_pending"] = true
		out["message"] = "The difference will be credited to your account balance."
	}
	return out
}

// handleUpgradeOptions lists the products reachable from a service's product.
func (s *Server) handleUpgradeOptions(w http.ResponseWriter, r *http.Request) {
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
	// Scope first: a service that is not the caller's must be indistinguishable from one that
	// does not exist, and doing it here means the pricing query never sees a foreign id.
	user, _ := userFrom(r.Context())
	if err := s.serviceStore(nil).ServiceOwnedBy(r.Context(), id, user.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	options, err := s.serviceStore(nil).UpgradeOptions(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(options))
	for _, opt := range options {
		prices := make([]map[string]any, 0, len(opt.Prices))
		for _, p := range opt.Prices {
			prices = append(prices, upgradePriceJSON(p))
		}
		out = append(out, map[string]any{
			"product_id":        opt.ProductID,
			"product_name":      opt.ProductName,
			"product_slug":      opt.ProductSlug,
			"short_description": opt.ShortDescription,
			"prices":            prices,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": out})
}

// handleUpgrade starts an upgrade or downgrade for the caller's own service.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
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

	var req upgradeRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must be a price version id")
		return
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).Upgrade(r.Context(), service.UpgradeRequest{
		ServiceID:      id,
		PriceVersionID: req.PriceVersionID,
		UserID:         user.ID,
		Reason:         req.Reason,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.upgrade_requested", map[string]any{
		"service_id": id, "upgrade_id": res.UpgradeID,
		"invoice_id": res.InvoiceID, "direction": res.Direction,
	})
	writeJSON(w, http.StatusCreated, upgradeResultJSON(res))
}

// handleStaffUpgrade moves any service, on a customer's behalf.
func (s *Server) handleStaffUpgrade(w http.ResponseWriter, r *http.Request) {
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

	var req upgradeRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must be a price version id")
		return
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).UpgradeAsStaff(r.Context(), service.UpgradeRequest{
		ServiceID:      id,
		PriceVersionID: req.PriceVersionID,
		UserID:         user.ID,
		Reason:         req.Reason,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.upgrade_requested", map[string]any{
		"service_id": id, "upgrade_id": res.UpgradeID,
		"invoice_id": res.InvoiceID, "direction": res.Direction,
	})
	writeJSON(w, http.StatusCreated, upgradeResultJSON(res))
}

// handleStaffUpgrades lists the pending-upgrade queue.
//
// Query: paid=true lists only the rows whose invoice has been paid (the ones the worker will
// apply); paid=false lists every pending row, which is what an operator chasing a stuck
// upgrade wants to see.
func (s *Server) handleStaffUpgrades(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	query := r.URL.Query()
	paidOnly := false
	if raw := strings.TrimSpace(query.Get("paid")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_paid", "paid must be true or false")
			return
		}
		paidOnly = parsed
	}
	limit := 100
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = n
	}

	rows, err := s.serviceStore(nil).PendingUpgrades(r.Context(), paidOnly, limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		row := map[string]any{
			"id":                    u.ID,
			"service_id":            u.ServiceID,
			"customer_id":           u.CustomerID,
			"from_price_version_id": u.FromPriceVersionID,
			"to_price_version_id":   u.ToPriceVersionID,
			"prorated_minor":        u.ProratedMinor,
			"currency":              u.Currency,
			"status":                u.Status,
			"created_at":            u.CreatedAt.UTC(),
		}
		if u.InvoiceID != "" {
			row["invoice_id"] = u.InvoiceID
			row["invoice_status"] = u.InvoiceStatus
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"upgrades": out})
}
