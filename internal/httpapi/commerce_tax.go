// Staff tax-rate administration.
//
// Tax rates are dated rows, and this surface reflects that: a rate is created effective from
// a date, closed by setting `effective_to`, and never edited once it has taken effect. The
// update handler refuses a rate change on a live row and says to append instead, because a
// rate is the evidence an issued invoice points at.
//
// One permission, `tax:write`, gates every mutation. Reads use the same permission: a rate
// table is not a secret, but it is also not something a billing clerk needs in order to
// price an invoice — the pricer resolves it.

package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/tax"
	"github.com/jackc/pgx/v5"
)

// registerTaxRoutes is the staff tax-rate surface.
func registerTaxRoutes(r *router, s *Server) {
	r.staffGet("/api/v1/staff/tax-rates", authn.PermTaxWrite, s.handleListTaxRates)
	r.staffGet("/api/v1/staff/tax-rates/{id}", authn.PermTaxWrite, s.handleGetTaxRate)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-rates", authn.PermTaxWrite, s.handleCreateTaxRate)
	r.staffMutation(http.MethodPut, "/api/v1/staff/tax-rates/{id}", authn.PermTaxWrite, s.handleUpdateTaxRate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/tax-rates/{id}", authn.PermTaxWrite, s.handleDeleteTaxRate)
}

// taxRateJSON is the wire shape. The rate is a string because it is an exact rational: a
// JSON number would be a float on the way in and on the way out.
func taxRateJSON(t commerce.TaxRate) map[string]any {
	out := map[string]any{
		"id":             t.ID,
		"name":           t.Name,
		"country_code":   t.CountryCode,
		"region_code":    t.RegionCode,
		"tax_class":      t.TaxClass,
		"rate":           t.Rate,
		"inclusive":      t.Inclusive,
		"effective_from": t.EffectiveFrom.UTC(),
		"enabled":        t.Enabled,
		"jurisdiction":   commerce.TaxJurisdiction(t),
		"created_at":     t.CreatedAt.UTC(),
	}
	if t.EffectiveTo != nil {
		out["effective_to"] = t.EffectiveTo.UTC()
	}
	return out
}

// handleListTaxRates lists every rate, current and historic.
func (s *Server) handleListTaxRates(w http.ResponseWriter, r *http.Request) {
	rates, err := s.commerceStore().TaxRates(r.Context())
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rates))
	for _, t := range rates {
		out = append(out, taxRateJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleGetTaxRate reads one rate.
func (s *Server) handleGetTaxRate(w http.ResponseWriter, r *http.Request) {
	t, err := s.commerceStore().TaxRate(r.Context(), r.PathValue("id"))
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, taxRateJSON(t))
}

// handleCreateTaxRate appends a rate.
func (s *Server) handleCreateTaxRate(w http.ResponseWriter, r *http.Request) {
	var in commerce.TaxRateInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var created commerce.TaxRate
	err := s.withCommerceAudit(r, "tax_rate.create", "tax_rate", "",
		map[string]any{"country_code": in.CountryCode, "rate": in.Rate},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = s.commerceStore().CreateTaxRateTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, taxRateJSON(created))
}

// handleUpdateTaxRate edits a rate's window or presentation, and refuses a rate change on a
// live row.
func (s *Server) handleUpdateTaxRate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in commerce.TaxRateInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var updated commerce.TaxRate
	err := s.withCommerceAudit(r, "tax_rate.update", "tax_rate", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = s.commerceStore().UpdateTaxRateTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, taxRateJSON(updated))
}

// handleDeleteTaxRate removes a rate that never took effect.
func (s *Server) handleDeleteTaxRate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.withCommerceAudit(r, "tax_rate.delete", "tax_rate", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.commerceStore().DeleteTaxRateTx(ctx, tx, id)
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// taxPricerFor builds the pricer the cart and checkout use for a customer.
//
// It delegates to the jurisdiction rules engine (internal/tax) rather than resolving a rate here,
// because the question is not only "what is the rate for DE" — it is "does the German rate apply to
// this sale at all". A validated VAT ID on a cross-border EU supply, and a US destination with no
// declared nexus, are both decisions that a rate lookup cannot express.
//
// It returns nil when tax is switched off, and the cart prices net. Nil rather than a zero-rate
// pricer matters: a cart priced by "a pricer that always charges nothing" would record tax evidence
// claiming a rate was resolved, and that evidence would be a lie.
func (s *Server) taxPricerFor(ctx context.Context, customerID string, at time.Time) (billing.Pricer, error) {
	return tax.NewRules(s.pool).PricerForCustomer(ctx, customerID, at)
}
