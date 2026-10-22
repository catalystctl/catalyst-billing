// Staff coupon administration.
//
// A coupon is configuration with a history, so this surface is CRUD *without* delete:
// retiring a coupon is `enabled: false`, because the invoices it discounted reference it and
// an invoice whose discount cannot be explained is not a valid document.
//
// Reads need coupon:read:all (a promotion list is a list of every price the shop has
// discounted); writes need coupon:write. Both are separate from settings:write because the
// person who runs a promotion is not necessarily the person who configures tax or a gateway.

package httpapi

import (
	"context"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/jackc/pgx/v5"
)

// registerCouponRoutes is the staff coupon CRUD.
func registerCouponRoutes(r *router, s *Server) {
	r.staffGet("/api/v1/staff/coupons", authn.PermCouponReadAll, s.handleListCoupons)
	r.staffGet("/api/v1/staff/coupons/{id}", authn.PermCouponReadAll, s.handleGetCoupon)
	r.staffGet("/api/v1/staff/coupons/{id}/redemptions", authn.PermCouponReadAll, s.handleListRedemptions)

	r.staffMutation(http.MethodPost, "/api/v1/staff/coupons", authn.PermCouponWrite, s.handleCreateCoupon)
	r.staffMutation(http.MethodPut, "/api/v1/staff/coupons/{id}", authn.PermCouponWrite, s.handleUpdateCoupon)
}

// couponJSON is the wire shape. It is built here rather than by marshalling the struct so
// the percentage is a string (never a float) and a nullable limit is `null` rather than
// absent.
func couponJSON(c commerce.Coupon) map[string]any {
	out := map[string]any{
		"id":                c.ID,
		"code":              c.Code,
		"description":       c.Description,
		"kind":              c.Kind,
		"applies_to":        c.AppliesTo,
		"enabled":           c.Enabled,
		"product_ids":       c.ProductIDs,
		"max_uses":          c.MaxUses,
		"max_uses_per_user": c.MaxUsesPerUser,
		"recurring_cycles":  c.RecurringCycles,
		"created_at":        c.CreatedAt.UTC(),
		"updated_at":        c.UpdatedAt.UTC(),
	}
	if c.Kind == commerce.CouponPercentage {
		out["percentage"] = c.Percentage
	} else {
		out["amount_minor"] = c.AmountMinor
		out["currency"] = c.Currency
	}
	if c.StartsAt != nil {
		out["starts_at"] = c.StartsAt.UTC()
	}
	if c.ExpiresAt != nil {
		out["expires_at"] = c.ExpiresAt.UTC()
	}
	return out
}

// handleListCoupons lists every coupon, retired ones included.
func (s *Server) handleListCoupons(w http.ResponseWriter, r *http.Request) {
	coupons, err := s.commerceStore().Coupons(r.Context())
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(coupons))
	for _, c := range coupons {
		out = append(out, couponJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleGetCoupon reads one coupon.
func (s *Server) handleGetCoupon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.commerceStore().Coupon(r.Context(), id)
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, couponJSON(c))
}

// handleListRedemptions lists who used a coupon. It is the relation manager Paymenter shows
// on the coupon resource, and the report that answers "what did this promotion cost".
func (s *Server) handleListRedemptions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	redemptions, err := s.commerceStore().Redemptions(r.Context(), id)
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(redemptions))
	for _, red := range redemptions {
		out = append(out, map[string]any{
			"id":              red.ID,
			"coupon_id":       red.CouponID,
			"customer_id":     red.CustomerID,
			"invoice_id":      red.InvoiceID,
			"subscription_id": red.SubscriptionID,
			"discount":        s.priceAmount(red.DiscountMinor, red.Currency),
			"created_at":      red.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleCreateCoupon creates a promotion.
func (s *Server) handleCreateCoupon(w http.ResponseWriter, r *http.Request) {
	var in commerce.CouponInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var created commerce.Coupon
	err := s.withCommerceAudit(r, "coupon.create", "coupon", "",
		map[string]any{"code": in.Code},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = s.commerceStore().CreateCouponTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, couponJSON(created))
}

// handleUpdateCoupon edits or retires a promotion.
func (s *Server) handleUpdateCoupon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in commerce.CouponInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var updated commerce.Coupon
	err := s.withCommerceAudit(r, "coupon.update", "coupon", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = s.commerceStore().UpdateCouponTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, couponJSON(updated))
}
