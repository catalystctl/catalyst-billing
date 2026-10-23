// Coupon detail: what a promotion is doing to the subscriptions that carry it.
//
// The staff coupon CRUD lives in commerce_coupon.go. This file is the *reviewal* half, and it
// exists because of one question that screen could not answer: a coupon has two lives — it
// discounts the order that used it, and then it discounts renewals for as many cycles as it was
// configured for. The redemption list shows the first. Nothing showed the second, so a "first three
// cycles" promotion looked identical to a lifetime one, and the only way to find out how many
// cycles a subscription had left was to read the database.
//
// It also owns the renewal decision's read path: `GET /api/v1/staff/coupons/{id}/subscriptions`
// returns every subscription carrying the coupon with its applied and remaining cycles, and
// `coupon.SubscriptionsCarrying` computes the remainder from the two stored numbers so the screen
// cannot invent a third answer.
//
// A read, so it needs `coupon:read:all` and nothing else — the person who runs a promotion is the
// person who needs to know what it is still costing.

package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/coupon"
	"github.com/catalyst/billing/internal/portal"
)

func init() {
	RegisterRoutes("90-coupon-renewals", func(r *router) { registerCouponRenewalRoutes(r, r.s) })
}

// registerCouponRenewalRoutes wires the promotion's subscription view.
func registerCouponRenewalRoutes(r *router, s *Server) {
	r.staffGet("/api/v1/staff/coupons/{id}/subscriptions", authn.PermCouponReadAll,
		s.handleCouponSubscriptions)
}

// handleCouponSubscriptions lists the subscriptions a coupon is attached to.
func (s *Server) handleCouponSubscriptions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A malformed id never reaches the query: it cannot name a coupon, and the database would answer
	// with a cast error rather than a 404.
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such coupon")
		return
	}
	subs, err := coupon.SubscriptionsCarrying(r.Context(), s.pool, id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": subs})
}
