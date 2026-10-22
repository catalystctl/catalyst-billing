// The commerce surface: coupons, tax rates, currencies and the cart.
//
// Four domains, three audiences, and the split is the security model:
//
//   - `/api/v1/staff/coupons/**` needs coupon:read:all or coupon:write.
//   - `/api/v1/staff/tax-rates/**` needs tax:write; `/api/v1/staff/currencies/**` needs
//     currency:write.
//   - `/api/v1/currencies` is public, because the storefront's currency switcher renders
//     before anyone signs in. It returns only enabled currencies and no configuration that
//     is not already visible in a price.
//   - `/api/v1/portal/cart/**` is the customer's own cart, and the customer is resolved from
//     the *session* — never from a body or a path. The store scopes every query by
//     customer_id, so an identifier substitution is a 404 rather than a data leak.
//
// Every mutation goes through `staffMutation` or `mutation`, so CSRF and idempotency are
// applied by the registrar rather than remembered by a handler.
//
// This file owns the registration entry point and the error mapping; the per-domain files
// own the handlers. That split is what lets four people add four domains without editing one
// shared list.

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("47-commerce", func(r *router) {
		r.s.registerCommerceRoutes(r)
	})
}

// commerceStore returns the commerce store for this server's pool.
//
// The store is a pool reference plus a clock, so constructing one per call is cheaper than
// the mutex that would guard a cache: there is no state to share and therefore nothing that
// a shared instance would protect.
func (s *Server) commerceStore() *commerce.Store {
	return commerce.New(s.pool)
}

// registerCommerceRoutes wires the whole surface. Each domain registers itself, so adding a
// route does not mean editing a list that four people share.
func (s *Server) registerCommerceRoutes(r *router) {
	registerCouponRoutes(r, s)
	registerTaxRoutes(r, s)
	registerCurrencyRoutes(r, s)
	registerCartRoutes(r, s)
}

// withCommerceAudit runs a commerce change and its audit row in one transaction.
//
// The row is written in the same transaction as the change, so a promotion or a tax rate
// cannot commit without a record of who changed it. That is the same rule the money paths
// follow, and it is why this is a transaction rather than a best-effort write afterwards.
func (s *Server) withCommerceAudit(r *http.Request, action, targetType, targetID string,
	changes map[string]any, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		ev := s.auditEvent(r, action, targetType, targetID, audit.OutcomeSuccess).WithChanges(changes)
		return audit.Record(ctx, tx, ev)
	})
}

// commerceError maps a domain error to a stable code.
//
// The codes are the contract the UI branches on: "expired" must render differently from
// "invalid", and both must render differently from "code taken". A generic 422 for all of
// them would make the coupon form unable to explain itself.
func (s *Server) commerceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, commerce.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, commerce.ErrCodeTaken):
		// One code for both a coupon code and a currency code: the client branches on
		// `code_taken` either way, and the detail names the resource.
		writeProblem(w, http.StatusConflict, "code_taken", "that code is already in use")
	case errors.Is(err, commerce.ErrInUse):
		writeProblem(w, http.StatusConflict, "in_use", err.Error())
	case errors.Is(err, commerce.ErrExpired):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_expired", err.Error())
	case errors.Is(err, commerce.ErrExhausted):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_exhausted", err.Error())
	case errors.Is(err, commerce.ErrNotApplicable):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_not_applicable", err.Error())
	case errors.Is(err, commerce.ErrInvalidInput):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	default:
		s.staffError(w, r, err)
	}
}
