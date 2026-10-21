package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/portal"
)

// Checkout over HTTP (Gate A0).
//
// internal/checkout was complete and tested but unreachable: the customer "Pay" button had
// no endpoint, so nothing could pay. This is the route, and it is deliberately thin — the
// amount, the idempotency of a second click, and the recorded expectation all live in the
// service, where the tests already prove them.
//
// The one rule worth restating here: the request body is empty. The amount comes from the
// invoice under a row lock inside Service.Start. A route that accepted an amount would be a
// route that lets a customer choose what to pay.

func init() {
	RegisterRoutes("30-checkout", registerCheckout)
}

func registerCheckout(r *router) {
	r.mutation(http.MethodPost, "/api/v1/portal/invoices/{id}/checkout",
		authn.PermInvoicePaySelf, r.s.handlePortalCheckout)
}

// checkoutService builds the checkout service for the configured gateway.
//
// It returns nil when no gateway is configured, which the handler reports as 503 once per
// request rather than logging per call: an unconfigured gateway is a deployment state, not
// an incident.
func (s *Server) checkoutService() *checkout.Service {
	if s.gateway == nil || s.pool == nil {
		return nil
	}
	return checkout.New(s.pool, s.gateway, s.account, s.checkoutSuccessURL, s.checkoutCancelURL)
}

func (s *Server) handlePortalCheckout(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	id := r.PathValue("id")
	// A malformed id is a 404 that never reaches the database, so the response cannot differ
	// between "malformed" and "someone else's invoice".
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	// Ownership is checked with the same scoped query the portal uses, so another
	// customer's invoice is indistinguishable from a nonexistent one.
	if _, err := s.portal.Invoice(r.Context(), user.ID, id); err != nil {
		s.writePortalError(w, r, err)
		return
	}

	// The gateway is checked last: whether an invoice exists is not a property of the
	// deployment, so a 404 must not become a 503 because a gateway is unconfigured.
	svc := s.checkoutService()
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable",
			"no payment gateway is configured")
		return
	}

	session, err := svc.Start(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, checkout.ErrInvoiceSettled):
			writeProblem(w, http.StatusConflict, "already_settled", "this invoice is already paid")
		case errors.Is(err, checkout.ErrInvoiceNotPayable):
			writeProblem(w, http.StatusPaymentRequired, "not_payable",
				"this invoice cannot be paid")
		case errors.Is(err, checkout.ErrCurrencyMismatch):
			writeProblem(w, http.StatusPaymentRequired, "not_payable",
				"the gateway does not accept this invoice's currency")
		default:
			// A gateway failure is our problem, not the customer's: say so, and let them
			// retry rather than concluding the invoice is broken.
			s.cfg.Logger.ErrorContext(r.Context(), "checkout failed",
				"invoice_id", id, "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable",
				"the payment gateway is unavailable; try again")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"attempt_id":   session.AttemptID,
		"invoice_id":   session.InvoiceID,
		"redirect_url": session.URL,
		"amount":       amountJSON(session.Amount),
		// Reused is true when the customer clicked twice and got the existing session
		// back. It is a 200, not an error: nothing went wrong, and the customer must be
		// sent to the same place.
		"reused": session.Reused,
	})
}
