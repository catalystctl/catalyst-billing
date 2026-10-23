package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/register"
	"github.com/jackc/pgx/v5"
)

// The customer checkout endpoint (CT-07, CT-08, SF-20, SF-21).
//
// `POST /api/v1/staff/orders` already prices from the catalog and writes the order,
// its subscription and the invoice in one transaction; what was missing was a path a
// *customer* could reach. This is that path, and it is thin for the same reason the
// staff handler is: the pricing, the stock lock, the coupon and the zero-total
// activation live in internal/register, where a test can prove them without an HTTP
// round trip.
//
// Three rules shape the handler:
//
//  1. **customer_id comes from the session.** The body has no customer field at all,
//     so identifier substitution is not a reachable attack rather than a checked one.
//     The billing account is resolved through `customer_accounts` (written by
//     registration) with the staff fallback internal/portal uses.
//  2. **No amount is accepted.** A line names a price version; the price is read from
//     the catalog inside the transaction. A client cannot choose what to charge.
//  3. **The permission is invoice:pay:self**, not invoice:read:self: placing an order
//     creates an obligation to pay, which is the closest existing capability to it.
//     See the report for the permission the integrator may want to add
//     (`order:create:self`).

func init() {
	RegisterRoutes("21-checkout-customer", registerCustomerCheckout)
}

func registerCustomerCheckout(r *router) {
	r.mutation(http.MethodPost, "/api/v1/portal/orders",
		authn.PermInvoicePaySelf, r.s.handleCustomerCheckout)
}

// customerCheckoutRequest is the body of a purchase.
//
// There is deliberately no customer id, no amount and no currency: all three are
// derived, and a body that could name them would be a body a client could lie in.
//
// # Two accepted spellings, on purpose
//
// The cart was specified to the frontend as `{items, coupon_code, tos_accepted}` and the
// API document describes `{lines, coupon_code, accept_tos}`. Both are accepted rather
// than picking a winner and breaking a caller: `decodeJSON` uses DisallowUnknownFields,
// so an unaccepted alias is a hard 400 on every checkout, which is exactly the kind of
// integration failure that is invisible until someone tries to buy something.
//
// `items` and `tos_accepted` are the canonical names (they are what the frontend is
// written against); `lines` and `accept_tos` are accepted aliases. Sending both is a
// client bug and is refused rather than merged, because "which list did you mean" has no
// safe answer.
type customerCheckoutRequest struct {
	Items []checkoutLineRequest `json:"items"`
	Lines []checkoutLineRequest `json:"lines"`

	// CouponCode is optional; matched case-insensitively.
	CouponCode string `json:"coupon_code"`

	TOSAccepted bool `json:"tos_accepted"`
	AcceptTOS   bool `json:"accept_tos"`
}

// checkoutLineRequest is one requested line.
type checkoutLineRequest struct {
	PriceVersionID string            `json:"price_version_id"`
	Quantity       int               `json:"quantity"`
	Config         map[string]string `json:"config"`
}

// handleCustomerCheckout buys what the request names, for the caller.
func (s *Server) handleCustomerCheckout(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil || s.issuer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "orders_unavailable",
			"ordering is not configured")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}

	var req customerCheckoutRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}

	requested := req.Items
	if len(requested) > 0 && len(req.Lines) > 0 {
		writeProblem(w, http.StatusBadRequest, "ambiguous_cart",
			"send the cart as either `items` or `lines`, not both")
		return
	}
	if len(requested) == 0 {
		requested = req.Lines
	}

	lines := make([]register.CheckoutLine, 0, len(requested))
	for _, l := range requested {
		lines = append(lines, register.CheckoutLine{
			PriceVersionID: l.PriceVersionID,
			Quantity:       l.Quantity,
			Config:         l.Config,
		})
	}

	svc := register.NewCheckoutService(s.pool, s.issuer, s.merchant, s.terms, s.series).
		WithNotify(s.notify).
		WithClock(s.now)

	res, err := svc.Checkout(r.Context(), register.CheckoutRequest{
		UserID:        user.ID,
		Lines:         lines,
		CouponCode:    req.CouponCode,
		AcceptedTOS:   req.TOSAccepted || req.AcceptTOS,
		CorrelationID: correlationID(r.Context()),
		IP:            s.clientIP(r),
	})
	if err != nil {
		s.writeCheckoutError(w, r, err)
		return
	}

	// Both the customer and the staff who own the revenue path may care that an order
	// was placed. It is published after the commit, never inside it.
	s.publishOrderEvents(res)

	// And the customer is told directly, because the live event only reaches a browser that is
	// already open. A customer who closed the tab still deserves the confirmation.
	s.notifyOrderPlaced(r.Context(), res, user.ID, user.Email)

	writeJSON(w, http.StatusCreated, checkoutJSON(res))
}

// writeCheckoutError maps a checkout failure to one stable code.
//
// Every branch names something the customer can act on: a coupon that expired is
// "use another code", stock that ran out is "choose something else", a missing
// permission is "sign in". A single "checkout failed" would be true and useless.
func (s *Server) writeCheckoutError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, register.ErrNoLines):
		writeProblem(w, http.StatusBadRequest, "no_lines", "at least one product is required")
	case errors.Is(err, register.ErrTooManyLines):
		writeProblem(w, http.StatusBadRequest, "too_many_lines", "an order can have at most 50 lines")
	case errors.Is(err, register.ErrNoCustomer):
		writeProblem(w, http.StatusForbidden, "no_billing_account",
			"this account has no billing account; finish registration or contact the operator")
	case errors.Is(err, register.ErrPriceNotFound):
		// A malformed id and an unknown one are the same answer: a probing request must
		// not learn that a price exists.
		writeProblem(w, http.StatusNotFound, "not_found", "no such price")
	case errors.Is(err, register.ErrInvalidQuantity):
		writeProblem(w, http.StatusBadRequest, "invalid_quantity",
			"quantity must be between 1 and 1000")
	case errors.Is(err, register.ErrQuantityNotAllowed):
		writeProblem(w, http.StatusBadRequest, "quantity_not_allowed",
			"this product is sold one at a time")
	case errors.Is(err, register.ErrMixedCurrency):
		writeProblem(w, http.StatusBadRequest, "mixed_currency",
			"all products in one order must use the same currency")
	case errors.Is(err, register.ErrOutOfStock):
		// 409, not 400: the request was well formed and was true when it was made. The
		// conflict is with the state of the shop.
		writeProblem(w, http.StatusConflict, "out_of_stock",
			"there is not enough stock left for this product")
	case errors.Is(err, register.ErrUserLimitReached):
		writeProblem(w, http.StatusConflict, "user_limit_reached",
			"you already hold the maximum of this product")
	case errors.Is(err, register.ErrInvalidSchedule):
		writeProblem(w, http.StatusBadRequest, "invalid_price",
			"this price cannot be billed: it has no valid schedule")
	case errors.Is(err, register.ErrInvalidConfig):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_config", err.Error())
	case errors.Is(err, register.ErrTOSRequired):
		writeProblem(w, http.StatusUnprocessableEntity, "tos_required",
			"the terms of service must be accepted")
	case errors.Is(err, register.ErrEmailVerificationRequired):
		writeProblem(w, http.StatusForbidden, "email_verification_required",
			"confirm your email address before buying")

	case errors.Is(err, register.ErrCouponNotFound):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_not_found", "that coupon code is not valid")
	case errors.Is(err, register.ErrCouponDisabled):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_disabled", "that coupon is no longer available")
	case errors.Is(err, register.ErrCouponNotStarted):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_not_started", "that coupon is not active yet")
	case errors.Is(err, register.ErrCouponExpired):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_expired", "that coupon has expired")
	case errors.Is(err, register.ErrCouponExhausted):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_exhausted", "that coupon has been fully redeemed")
	case errors.Is(err, register.ErrCouponUsed):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_already_used", "you have already used that coupon")
	case errors.Is(err, register.ErrCouponNotForYou):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_not_applicable",
			"that coupon does not apply to these products")
	case errors.Is(err, register.ErrCouponWrongMoney):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_currency_mismatch",
			"that coupon is issued in a different currency")
	case errors.Is(err, register.ErrCouponNotEligible), errors.Is(err, register.ErrCouponNoDiscount):
		writeProblem(w, http.StatusUnprocessableEntity, "coupon_no_discount",
			"that coupon gives no discount on this order")

	default:
		s.cfg.Logger.ErrorContext(r.Context(), "checkout failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// checkoutJSON is the wire shape of a placed order.
//
// It is the contract the storefront and the cart need, so it is written once, here,
// and documented in internal/openapi/descriptions_purchase.go. The rule that matters
// for a client: `invoice_id` is empty exactly when `activated` is true, because a free
// order has nothing to pay and therefore no document.
func checkoutJSON(res register.CheckoutResult) map[string]any {
	lines := make([]map[string]any, 0, len(res.Lines))
	for _, l := range res.Lines {
		lines = append(lines, map[string]any{
			"description": l.Description,
			"quantity":    l.Quantity,
			"unit":        amountJSON(l.Unit),
			"net":         amountJSON(l.Net),
			"tax":         amountJSON(l.Tax),
			"total":       amountJSON(l.Total),
		})
	}
	subscriptions := res.SubscriptionIDs
	if subscriptions == nil {
		subscriptions = []string{}
	}
	services := res.ServiceIDs
	if services == nil {
		services = []string{}
	}
	return map[string]any{
		"id":               res.OrderID,
		"status":           res.Status,
		"currency":         res.Currency,
		"subtotal":         amountJSON(res.Subtotal),
		"discount":         amountJSON(res.Discount),
		"tax":              amountJSON(res.Tax),
		"total":            amountJSON(res.Total),
		"invoice_id":       res.InvoiceID,
		"invoice_number":   res.InvoiceNumber,
		"subscription_ids": subscriptions,
		"service_ids":      services,
		"activated":        res.Activated,
		"coupon_code":      res.CouponCode,
		"lines":            lines,
	}
}

// publishOrderEvents tells the live views that an order exists.
//
// One event, scoped to the customer *and* carrying the staff permission: the SSE
// filter delivers it to that customer and to staff who hold invoice:read:all, so the
// portal refreshes itself and an operator watching orders sees it too. It is published
// after the transaction committed, never inside it.
func (s *Server) publishOrderEvents(res register.CheckoutResult) {
	data := map[string]any{
		"order_id":       res.OrderID,
		"invoice_id":     res.InvoiceID,
		"invoice_number": res.InvoiceNumber,
		"total_minor":    res.Total.Minor(),
		"currency":       res.Currency,
		"activated":      res.Activated,
	}
	if res.Activated {
		data["service_ids"] = res.ServiceIDs
	}
	s.publish(events.Event{
		Kind:       "order.created",
		CustomerID: res.CustomerID,
		Permission: string(authn.PermInvoiceReadAll),
		Data:       data,
	})
	// CT-13: the cart stopped being a draft. It is a separate kind from order.created because
	// an extension that runs cart recovery has to stop counting this customer, and it can only
	// do that if "the cart was bought" is its own fact.
	s.publish(events.Event{
		Kind:       extproto.EventCartChecked,
		CustomerID: res.CustomerID,
		Permission: string(authn.PermCustomerReadSelf),
		Data: map[string]any{
			"order_id":    res.OrderID,
			"customer_id": res.CustomerID,
			"total_minor": res.Total.Minor(),
			"currency":    res.Currency,
		},
	})
}

// notifyOrderPlaced tells a customer their order was received.
//
// It runs on its own transaction, after the order committed, and is deliberately best-effort:
// the order is the durable fact, and a mail that cannot be queued must not fail a checkout the
// customer already completed. Dedupe is the order id, so a double-clicked Place order button
// produces one message.
func (s *Server) notifyOrderPlaced(ctx context.Context, res register.CheckoutResult, userID, email string) {
	if s.pool == nil || s.notify == nil || res.OrderID == "" {
		return
	}

	total := res.Total.Format()
	_ = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.OrderCreated(ctx, tx, s.notify, notify.OrderCreatedInput{
			UserID:   userID,
			Email:    email,
			OrderID:  res.OrderID,
			Number:   res.InvoiceNumber,
			Total:    total,
			OrderURL: "/portal",
		})
		return err
	})
}
