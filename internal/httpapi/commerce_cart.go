// The customer's cart.
//
// The customer is resolved from the *session*, never from a path or a body, and the store
// scopes every query by customer_id. That combination is what makes an identifier
// substitution a 404 rather than a data leak — the same rule the portal follows.
//
// Reads need customer:read:self and mutations need customer:write:self, which is the
// permission that already means "may change my own account". The cart is not a new
// privilege; it is a draft of a purchase the customer is already allowed to make.
//
// Every mutation goes through `mutation`, so CSRF and idempotency apply automatically: a
// double-submitted "add to cart" must not add the line twice, and a retried "apply coupon"
// must not apply it twice.

package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/tax"
)

// registerCartRoutes is the portal cart surface.
func registerCartRoutes(r *router, s *Server) {
	r.get("/api/v1/portal/cart", authn.PermCustomerReadSelf, s.handleGetCart)
	r.get("/api/v1/portal/cart/totals", authn.PermCustomerReadSelf, s.handleCartTotals)

	r.mutation(http.MethodPost, "/api/v1/portal/cart/items",
		authn.PermCustomerWriteSelf, s.handleAddCartItem)
	r.mutation(http.MethodPut, "/api/v1/portal/cart/items/{id}",
		authn.PermCustomerWriteSelf, s.handleUpdateCartItem)
	r.mutation(http.MethodDelete, "/api/v1/portal/cart/items/{id}",
		authn.PermCustomerWriteSelf, s.handleRemoveCartItem)
	r.mutation(http.MethodDelete, "/api/v1/portal/cart",
		authn.PermCustomerWriteSelf, s.handleClearCart)
	r.mutation(http.MethodPost, "/api/v1/portal/cart/coupon",
		authn.PermCustomerWriteSelf, s.handleApplyCoupon)
	r.mutation(http.MethodDelete, "/api/v1/portal/cart/coupon",
		authn.PermCustomerWriteSelf, s.handleRemoveCoupon)
}

// cartCustomer resolves the caller's billing customer.
//
// It writes the response itself, so a handler reads as a sequence of guards. An
// unconfigured portal is a 503 rather than a 500 because it is a deployment state, not a
// bug.
func (s *Server) cartCustomer(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable",
			"the portal is not configured")
		return "", false
	}
	user, ok := userFrom(r.Context())
	if !ok || user.ID == "" {
		writeProblem(w, http.StatusUnauthorized, "authentication_required",
			"a session is required")
		return "", false
	}
	customerID, err := s.portal.CustomerIDFor(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, portal.ErrNotFound) {
			// A signed-in user with no billing account. This is the "register then buy"
			// state: the answer is that there is no cart yet, not that the request was
			// forbidden.
			writeProblem(w, http.StatusNotFound, "not_found", "no billing account for this login")
			return "", false
		}
		s.writePortalError(w, r, err)
		return "", false
	}
	return customerID, true
}

// cartError maps a cart error to a stable code.
//
// It exists because `coupon_not_applicable` is the right code for a refused promotion and the
// wrong one for "the cart is in USD and this price is in EUR": a client that branches on the
// code would show the wrong message. The cart's own refusal is therefore `not_applicable`, and
// the coupon-specific codes stay with the coupon surface.
func (s *Server) cartError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, commerce.ErrAlreadyInCart):
		// 409, not 422: the request was well formed and true when it was made; it conflicts with
		// what is already in the cart. The code is its own so the storefront can say "it is
		// already in your cart" and offer the cart, rather than the currency message that
		// `not_applicable` carries.
		writeProblem(w, http.StatusConflict, "already_in_cart", err.Error())
	case errors.Is(err, commerce.ErrNotApplicable):
		writeProblem(w, http.StatusUnprocessableEntity, "not_applicable", err.Error())
	case errors.Is(err, commerce.ErrExpired):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_expired", err.Error())
	case errors.Is(err, commerce.ErrExhausted):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_exhausted", err.Error())
	case errors.Is(err, tax.ErrRefused):
		// The jurisdiction rules will not price this sale. The refusal carries its own stable code
		// and the facts that are missing, which is exactly what the cart needs to show instead of a
		// plausible total that nobody should invoice.
		var refusal *tax.Refusal
		if errors.As(err, &refusal) {
			writeProblem(w, http.StatusUnprocessableEntity, refusal.Code, refusal.Detail)
			return
		}
		writeProblem(w, http.StatusUnprocessableEntity, "tax_refused", err.Error())
	default:
		s.commerceError(w, r, err)
	}
}

// cartJSON is the wire shape of a cart.
func cartJSON(c commerce.Cart) map[string]any {
	items := make([]map[string]any, 0, len(c.Items))
	for _, item := range c.Items {
		items = append(items, map[string]any{
			"id":               item.ID,
			"price_version_id": item.PriceVersionID,
			"product_id":       item.ProductID,
			"product_name":     item.ProductName,
			"price_name":       item.PriceName,
			"quantity":         item.Quantity,
			"config":           item.Config,
			"label":            item.Label,
			"created_at":       item.CreatedAt.UTC(),
		})
	}
	out := map[string]any{
		"id":              c.ID,
		"currency":        c.Currency,
		"coupon_id":       c.CouponID,
		"items":           items,
		"item_count":      len(c.Items),
		"tos_accepted_at": nil,
	}
	if c.TosAcceptedAt != nil {
		out["tos_accepted_at"] = c.TosAcceptedAt.UTC()
	}
	return out
}

// publishCartEvent announces one committed cart change (CT-13).
//
// Three properties, and they are the same three the rest of this server's events have:
//
//   - It is called after the store returned, which is after its transaction committed. An
//     event announced from inside would describe a cart that can still roll back, and an
//     extension that reacted to it would have reacted to nothing.
//   - It cannot fail the request. `Server.publish` is a non-blocking hand-off to the broker,
//     which drops a slow subscriber and closes its stream rather than blocking the customer's
//     add-to-cart. The event is best-effort; the cart write is not.
//   - It carries the customer id, so the same event reaches that customer's own browser and
//     the staff permission that may watch carts — the filter in events.go does the scoping, not
//     this call.
//
// The kind is decided by the caller rather than derived here: only the add-to-cart path knows
// whether the line it just added was the first one, and "created" and "updated" mean different
// things to an extension that counts abandoned carts.
func (s *Server) publishCartEvent(kind, customerID string, cart commerce.Cart, extra map[string]any) {
	data := map[string]any{
		"cart_id":     cart.ID,
		"customer_id": customerID,
		"item_count":  len(cart.Items),
		"currency":    cart.Currency,
	}
	for key, value := range extra {
		data[key] = value
	}
	s.publish(events.Event{
		Kind:       kind,
		CustomerID: customerID,
		Permission: string(authn.PermCustomerReadSelf),
		Data:       data,
	})
}

// totalsJSON is the wire shape of a priced cart. Money is always the minor/currency/scale
// triple, never a float and never a decimal string.
func totalsJSON(t commerce.Totals) map[string]any {
	lines := make([]map[string]any, 0, len(t.Lines))
	for _, lt := range t.Lines {
		line := map[string]any{
			"item_id":          lt.ItemID,
			"price_version_id": lt.PriceVersionID,
			"product_id":       lt.ProductID,
			"description":      lt.Description,
			"quantity":         lt.Quantity,
			"unit":             amountJSON(lt.Unit),
			"setup":            amountJSON(lt.Setup),
			"line":             amountJSON(lt.Line),
			"setup_line":       amountJSON(lt.SetupLine),
			"discount":         amountJSON(lt.Discount),
			"net":              amountJSON(lt.Net),
			"tax":              amountJSON(lt.Tax),
			"total":            amountJSON(lt.Total),
			"tax_rate":         lt.TaxRate,
		}
		if lt.TaxEvidence != nil {
			line["tax_evidence"] = map[string]any{
				"jurisdiction":     lt.TaxEvidence.Jurisdiction,
				"rate":             lt.TaxEvidence.Rate,
				"inclusive":        lt.TaxEvidence.Inclusive,
				"exemption_reason": lt.TaxEvidence.ExemptionReason,
			}
		}
		lines = append(lines, line)
	}
	out := map[string]any{
		"currency":   t.Currency,
		"lines":      lines,
		"subtotal":   amountJSON(t.Subtotal),
		"discount":   amountJSON(t.Discount),
		"tax":        amountJSON(t.Tax),
		"total":      amountJSON(t.Total),
		"item_count": len(t.Lines),
	}
	// The cart-level tax explanation, lifted from the first line that has one. The storefront and
	// the cart both render "incl. 19% VAT" above the total, and re-deriving that from a per-line
	// object would mean two renderers guessing the same thing differently.
	for _, lt := range t.Lines {
		if lt.TaxEvidence == nil {
			continue
		}
		out["tax_evidence"] = map[string]any{
			"jurisdiction":     lt.TaxEvidence.Jurisdiction,
			"rate":             lt.TaxEvidence.Rate,
			"inclusive":        lt.TaxEvidence.Inclusive,
			"exemption_reason": lt.TaxEvidence.ExemptionReason,
		}
		break
	}
	if t.Coupon != nil {
		out["coupon"] = map[string]any{
			"coupon_id":        t.Coupon.CouponID,
			"code":             t.Coupon.Code,
			"applies_to":       t.Coupon.AppliesTo,
			"discount":         amountJSON(t.Coupon.Total),
			"recurring_cycles": t.Coupon.RecurringCycles,
		}
	}
	if t.CouponProblem != "" {
		// The refusal is shown on the cart rather than raised: the customer can remove the
		// code and still check out, and hiding the reason would leave them guessing.
		out["coupon_problem"] = t.CouponProblem
	}
	return out
}

// handleGetCart returns the caller's cart, creating an empty one if needed.
func (s *Server) handleGetCart(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	currency := r.URL.Query().Get("currency")
	cart, err := s.commerceStore().Cart(r.Context(), customerID, currency)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cartJSON(cart))
}

// handleCartTotals re-prices the cart from the catalog.
func (s *Server) handleCartTotals(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	totals, err := s.cartTotals(r, customerID)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, totalsJSON(totals))
}

// cartTotals prices the cart, attaching the tax pricer when tax is configured.
func (s *Server) cartTotals(r *http.Request, customerID string) (commerce.Totals, error) {
	pricer, err := s.taxPricerFor(r.Context(), customerID, time.Now().UTC())
	if err != nil {
		return commerce.Totals{}, err
	}
	if pricer == nil {
		// No tax configured: price net, which is the honest default.
		return s.commerceStore().Totals(r.Context(), customerID, nil)
	}
	return s.commerceStore().Totals(r.Context(), customerID, pricer)
}

// addItemBody is the add-to-cart body.
type addItemBody struct {
	PriceVersionID string            `json:"price_version_id"`
	Quantity       int               `json:"quantity"`
	Config         map[string]string `json:"config"`
	Label          string            `json:"label"`
}

// handleAddCartItem adds a selection.
func (s *Server) handleAddCartItem(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	var body addItemBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	if !portal.ValidID(body.PriceVersionID) {
		// A malformed id never reaches the database and is indistinguishable from a missing
		// one, which is what the storefront needs to show "unavailable" without leaking
		// whether an id exists.
		writeProblem(w, http.StatusNotFound, "not_found", "no such price")
		return
	}
	currency := r.URL.Query().Get("currency")
	cart, err := s.commerceStore().AddItem(r.Context(), customerID, currency, commerce.AddItemRequest{
		PriceVersionID: body.PriceVersionID,
		Quantity:       body.Quantity,
		Config:         body.Config,
		Label:          body.Label,
	})
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	// The store reports whether this call brought the cart into existence; every other change
	// is an update. An extension that runs an abandoned-cart campaign cares about the
	// difference, and counting lines cannot answer it — a second unit of the same product is
	// an update on a one-line cart.
	kind := extproto.EventCartUpdated
	if cart.Created {
		kind = extproto.EventCartCreated
	}
	s.publishCartEvent(kind, customerID, cart, map[string]any{
		"price_version_id": body.PriceVersionID,
		"quantity":         body.Quantity,
	})
	writeJSON(w, http.StatusCreated, cartJSON(cart))
}

// quantityBody is the update-quantity body.
type quantityBody struct {
	Quantity int `json:"quantity"`
}

// handleUpdateCartItem sets a line's quantity.
func (s *Server) handleUpdateCartItem(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such cart line")
		return
	}
	var body quantityBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	cart, err := s.commerceStore().UpdateQuantity(r.Context(), customerID, id, body.Quantity)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	s.publishCartEvent(extproto.EventCartUpdated, customerID, cart, map[string]any{
		"item_id":  id,
		"quantity": body.Quantity,
	})
	writeJSON(w, http.StatusOK, cartJSON(cart))
}

// handleRemoveCartItem deletes one line.
func (s *Server) handleRemoveCartItem(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such cart line")
		return
	}
	cart, err := s.commerceStore().RemoveItem(r.Context(), customerID, id)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	s.publishCartEvent(extproto.EventCartUpdated, customerID, cart, map[string]any{"item_id": id})
	writeJSON(w, http.StatusOK, cartJSON(cart))
}

// handleClearCart empties the cart but keeps its currency.
func (s *Server) handleClearCart(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	cart, err := s.commerceStore().Clear(r.Context(), customerID)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	// An emptied cart is still an update, not an abandonment: the customer is standing in
	// front of the screen. Abandonment is a fact about time, and it is emitted by the job that
	// can see time passing.
	s.publishCartEvent(extproto.EventCartUpdated, customerID, cart, map[string]any{"cleared": true})
	writeJSON(w, http.StatusOK, cartJSON(cart))
}

// couponBody is the apply-coupon body.
type couponBody struct {
	Code string `json:"code"`
}

// handleApplyCoupon attaches a coupon and returns the live total.
func (s *Server) handleApplyCoupon(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	var body couponBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	cart, err := s.commerceStore().ApplyCoupon(r.Context(), customerID, body.Code)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	s.publishCartEvent(extproto.EventCartUpdated, customerID, cart, map[string]any{"coupon_code": body.Code})
	// The response carries both the cart and the live total, because "apply a coupon" is
	// only useful to a UI if it can show the new number without a second request.
	totals, err := s.cartTotals(r, customerID)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cart":   cartJSON(cart),
		"totals": totalsJSON(totals),
	})
}

// handleRemoveCoupon detaches the coupon.
func (s *Server) handleRemoveCoupon(w http.ResponseWriter, r *http.Request) {
	customerID, ok := s.cartCustomer(w, r)
	if !ok {
		return
	}
	cart, err := s.commerceStore().RemoveCoupon(r.Context(), customerID)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	s.publishCartEvent(extproto.EventCartUpdated, customerID, cart, map[string]any{"coupon_removed": true})
	totals, err := s.cartTotals(r, customerID)
	if err != nil {
		s.cartError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cart":   cartJSON(cart),
		"totals": totalsJSON(totals),
	})
}
