package httpapi

// Creating a service for an existing customer, from the operator panel.
//
// The panel could suspend, resume, terminate, upgrade and cancel a service, and it could not add
// one, so an operator adding a service to a customer outside checkout needed SQL. This is that
// route.
//
// It is the dangerous half of the management surface, so the shape of the response matters as much
// as the shape of the request: the response says whether a subscription was created (what the
// renewal engine will act on) and states plainly that no invoice was raised. An operator who is not
// told that would assume a bill went out.
//
// Everything that decides whether a product may be sold is done by internal/customer calling the
// same helpers the customer checkout calls — stock, the per-user limit, the quantity policy and the
// config-option pricing. Nothing here re-implements a rule, and nothing here writes an invoice: a
// manual creation is a delivery, and billing for a service is the separate action
// `POST /api/v1/staff/invoices` with a product line, which fulfilment provisions when it settles.

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/customer"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("47-services-staff", registerStaffServiceCreate)
}

func registerStaffServiceCreate(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/services",
		authn.PermServiceWriteAll, r.s.handleCreateService)
}

// createServiceRequest is the body of a hand-made service.
type createServiceRequest struct {
	CustomerID string `json:"customer_id"`
	// PriceVersionID names what is being given. The price is read from the catalog, never from the
	// body, so a client cannot choose what the service is worth.
	PriceVersionID string `json:"price_version_id"`
	// Quantity defaults to 1.
	Quantity int `json:"quantity"`
	// Config is the chosen config options as {config_option_id: value}. It is validated and priced
	// against the price version, exactly as a purchase's selection is.
	Config map[string]string `json:"config"`
	// Reason is required. A service with no order behind it is exactly what an auditor asks about,
	// so the answer is captured when it is still known rather than reconstructed later.
	Reason string `json:"reason"`
	// Comp asks for a service nothing will ever invoice: no subscription is created, so the
	// renewal engine has no row to find. It is the honest way to express "this customer does not
	// pay for this", rather than a price of zero that would bill zero every period for ever.
	Comp bool `json:"comp"`
}

// handleCreateService adds a service to an existing customer.
func (s *Server) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	in := customer.ServiceInput{
		CustomerID:     req.CustomerID,
		PriceVersionID: req.PriceVersionID,
		Quantity:       req.Quantity,
		Config:         req.Config,
		Reason:         req.Reason,
		Comp:           req.Comp,
		ActorID:        user.ID,
		CorrelationID:  correlationID(r.Context()),
		IP:             s.clientIP(r),
	}

	var result customer.ServiceResult
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = s.customerStore().CreateService(ctx, tx, in)
		return err
	})
	if err != nil {
		s.writeServiceCreateError(w, r, err)
		return
	}

	// The subscription is echoed because it is the thing the renewal engine will act on: an
	// operator who has just handed someone a service needs to see when the first invoice will be
	// raised, or that none will be.
	out := map[string]any{
		"service": map[string]any{
			"id":          result.ServiceID,
			"customer_id": result.CustomerID,
			"product_id":  result.ProductID,
			"status":      result.Status,
			"origin":      "manual",
			"reason":      result.OriginReason,
			"quantity":    result.Quantity,
			"created_at":  result.CreatedAt.UTC(),
		},
		"price_version_id": result.PriceVersionID,
		"subscription":     nil,
		"config":           serviceSelectionJSON(result.Options),
		// Stated rather than implied: this endpoint never raises a bill, and an operator who is
		// not told so would assume one went out.
		"invoiced": false,
		"comped":   result.Comped,
	}
	if result.SubscriptionID != "" {
		out["subscription"] = map[string]any{
			"id":                   result.SubscriptionID,
			"status":               "active",
			"interval":             result.Interval,
			"current_period_start": result.PeriodStart.UTC(),
			"current_period_end":   result.PeriodEnd.UTC(),
			"next_billing_at":      result.NextBillAt.UTC(),
		}
	}

	s.publish(events.Event{Kind: "service.created",
		Permission: string(authn.PermServiceReadAll),
		CustomerID: result.CustomerID,
		Data: map[string]any{
			"service_id": result.ServiceID,
			"product_id": result.ProductID,
			"comped":     result.Comped,
		}})
	writeJSON(w, http.StatusCreated, out)
}

// serviceSelectionJSON renders a priced config selection.
func serviceSelectionJSON(options []service.SelectionContribution) []map[string]any {
	out := make([]map[string]any, 0, len(options))
	for _, o := range options {
		out = append(out, map[string]any{
			"config_option_id": o.ConfigOptionID,
			"name":             o.Name,
			"env_variable":     o.EnvVariable,
			"value":            o.Value,
			"amount":           amountJSON(o.Amount),
			"setup":            amountJSON(o.Setup),
		})
	}
	return out
}

// writeServiceCreateError maps a refusal onto a stable code.
//
// Every refusal here is something the operator can act on, and they are all different actions:
// pick another customer (404), restock the product (409 out_of_stock), raise the customer's limit
// (409 per_user_limit), choose different options (400 invalid_config), say why (400
// reason_required). Collapsing them into one code would be a support ticket per incident.
func (s *Server) writeServiceCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, customer.ErrCustomerNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
	case errors.Is(err, customer.ErrPriceNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such price version")
	case errors.Is(err, customer.ErrReasonRequired):
		writeProblem(w, http.StatusBadRequest, "reason_required",
			"a manually created service needs a reason, because there is no order behind it")
	case errors.Is(err, customer.ErrInvalidQuantity):
		writeProblem(w, http.StatusBadRequest, "invalid_quantity",
			"quantity must be between 1 and 1000")
	case errors.Is(err, customer.ErrQuantityNotAllowed):
		writeProblem(w, http.StatusBadRequest, "quantity_not_allowed",
			"this product cannot be given in a quantity greater than one")
	case errors.Is(err, customer.ErrOutOfStock):
		writeProblem(w, http.StatusConflict, "out_of_stock", "this product has no stock left")
	case errors.Is(err, customer.ErrPerUserLimit):
		writeProblem(w, http.StatusConflict, "per_user_limit",
			"the customer already holds the maximum number of this product")
	case errors.Is(err, customer.ErrInvalidConfig):
		writeProblem(w, http.StatusBadRequest, "invalid_config", err.Error())
	case errors.Is(err, customer.ErrInvalidSchedule):
		writeProblem(w, http.StatusConflict, "invalid_schedule",
			"this price cannot be turned into a billing period")
	default:
		s.staffError(w, r, err)
	}
}
