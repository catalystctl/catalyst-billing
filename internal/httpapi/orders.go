// Creating an order: "buy this product for this billing account".
//
// This is the entry point the catalog and the storefront were missing. Before it, an
// order could only be produced by a test fixture, so a customer could browse products
// and then had nowhere to go. The handler is deliberately thin: pricing, the
// subscription schedule, the invoice and the ledger posting all live in
// internal/orders, where a test can prove them without an HTTP round trip.
//
// The one rule that is not obvious: `customer_id` is optional, and when it is absent the
// order is placed for the acting staff member's own billing account. A staff member who
// is the billing account must not have to link an account to an account first — see
// billingAccountFor.
//
// The second rule: `custom_price_minor` lets an operator agree a number with one customer.
// The amount is sent as integer minor units and stored as an explicit override on the line,
// together with the reason that justifies it; the catalog is never written, because the
// price list is what every other customer pays. A custom price without a reason is refused.

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/orders"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("46-orders", func(r *router) {
		r.staffMutation(http.MethodPost, "/api/v1/staff/orders",
			authn.PermInvoiceWriteAll, r.s.handleCreateOrder)
	})
}

// createOrderRequest is the body of a purchase.
//
// There is no *catalog* amount and no currency: both are read from the price version, so a
// caller cannot quietly choose what to charge. The one exception is deliberate and loud —
// `custom_price_minor` — because agreeing a price with a customer is a thing an operator
// legitimately does, and it must be recorded rather than done by editing the catalog.
type createOrderRequest struct {
	// CustomerID is the billing account. Empty means "me" (the acting staff member).
	CustomerID string `json:"customer_id"`
	// PriceVersionID is the catalog price being bought. It is required even when the
	// amount is overridden: it names what is bought, and so what gets provisioned.
	PriceVersionID string `json:"price_version_id"`
	// Quantity defaults to 1.
	Quantity int `json:"quantity"`
	// CustomPriceMinor is a negotiated unit price in minor units (500 = 5.00 in USD).
	//
	// A pointer, not an int, because absent and zero are different answers: absent means
	// "charge the catalog price", zero is an amount that cannot be charged and is
	// refused. It replaces the catalog unit price and its setup fee, so the number sent
	// is the whole price of one unit.
	CustomPriceMinor *int64 `json:"custom_price_minor"`
	// CustomPriceReason is why the price differs from the catalog. Mandatory whenever
	// CustomPriceMinor is present.
	CustomPriceReason string `json:"custom_price_reason"`
}

// customPriceOf turns the request's override into the domain's, keeping "absent" distinct
// from "zero".
//
// The reason is trimmed here as well as in the domain, so the audit row records exactly the
// text the database does rather than the padding the caller happened to send.
func (req createOrderRequest) customPriceOf() *orders.CustomPrice {
	if req.CustomPriceMinor == nil {
		return nil
	}
	return &orders.CustomPrice{
		UnitMinor: *req.CustomPriceMinor,
		Reason:    strings.TrimSpace(req.CustomPriceReason),
	}
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if s.issuer == nil || s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "orders_unavailable",
			"ordering is not configured")
		return
	}
	var req createOrderRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	// A malformed id is a 404 that never reaches the database, so "no such price" and
	// "not a uuid" cannot be told apart by a probe.
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such price")
		return
	}
	if req.CustomerID != "" && !portal.ValidID(req.CustomerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	// A reason without an amount is a client that meant to send both. Dropping the reason
	// would charge the catalog price while the operator believes the line was discounted,
	// which is the one failure a manual price must never have, so it is refused instead.
	if req.CustomPriceMinor == nil && strings.TrimSpace(req.CustomPriceReason) != "" {
		writeProblem(w, http.StatusBadRequest, "custom_price_required",
			"custom_price_reason was sent without custom_price_minor")
		return
	}

	user, _ := userFrom(r.Context())
	svc := orders.New(s.issuer)
	now := s.now()
	customPrice := req.customPriceOf()

	var out orders.Result
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		customerID := req.CustomerID
		if customerID == "" {
			var err error
			customerID, err = s.billingAccountFor(ctx, tx, user)
			if err != nil {
				return err
			}
		}
		res, err := svc.Create(ctx, tx, orders.Request{
			CustomerID:     customerID,
			PriceVersionID: req.PriceVersionID,
			Quantity:       req.Quantity,
			CustomPrice:    customPrice,
			Merchant:       s.merchant,
			Terms:          s.terms,
			Series:         s.series,
			// Terms of sale: the document is payable on issue. The renewal engine owns
			// the lead time for *renewal* invoices, which is a different decision.
			DueAt: now.AddDate(0, 0, 30),
			Now:   now,
		})
		if err != nil {
			return err
		}
		out = res
		changes := map[string]any{
			"customer_id":      customerID,
			"price_version_id": req.PriceVersionID,
			"quantity":         req.Quantity,
			"invoice_id":       res.InvoiceID,
			"total_minor":      res.Total.Minor(),
			"manual_price":     res.ManualPrice,
		}
		if customPrice != nil {
			// The reason belongs in the audit record: "who changed the price, to what and
			// why" is the question an audit log exists to answer.
			changes["custom_price_minor"] = customPrice.UnitMinor
			changes["custom_price_reason"] = customPrice.Reason
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "order.create", "order", res.OrderID,
			audit.OutcomeSuccess).WithChanges(changes))
	})
	if err != nil {
		switch {
		case errors.Is(err, orders.ErrPriceNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "no such price")
		case errors.Is(err, orders.ErrCustomerNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		case errors.Is(err, orders.ErrInvalidQuantity):
			writeProblem(w, http.StatusBadRequest, "invalid_quantity",
				"quantity must be between 1 and 1000")
		case errors.Is(err, orders.ErrCustomPriceReasonRequired):
			writeProblem(w, http.StatusBadRequest, "custom_price_reason_required",
				"a custom price needs a reason: say what the price is for")
		case errors.Is(err, orders.ErrCustomPriceReasonTooLong):
			writeProblem(w, http.StatusBadRequest, "invalid_custom_price_reason",
				"the reason for a custom price must be at most 500 characters")
		case errors.Is(err, orders.ErrInvalidCustomPrice):
			writeProblem(w, http.StatusBadRequest, "invalid_custom_price",
				"a custom price must be a positive amount in minor units")
		case errors.Is(err, orders.ErrInvalidSchedule):
			writeProblem(w, http.StatusBadRequest, "invalid_price",
				"this price cannot be billed: it has no valid schedule")
		case errors.Is(err, billing.ErrInvalidInvoice):
			writeProblem(w, http.StatusBadRequest, "invalid_order", err.Error())
		default:
			s.staffError(w, r, err)
		}
		return
	}

	s.publish(events.Event{Kind: "order.created", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"order_id": out.OrderID, "invoice_id": out.InvoiceID}})
	body := map[string]any{
		"id":              out.OrderID,
		"invoice_id":      out.InvoiceID,
		"number":          out.InvoiceNumber,
		"subscription_id": out.SubscriptionID,
		"status":          out.Status,
		"total":           amountJSON(out.Total),
		// manual_price tells the caller the line is not priced from the catalog, so a
		// screen can label it without re-reading the invoice.
		"manual_price": out.ManualPrice,
	}
	if out.ManualPrice {
		body["custom_price_minor"] = out.UnitMinor
	}
	writeJSON(w, http.StatusCreated, body)
}

// billingAccountFor resolves the acting staff member's own billing account.
//
// The customers row is matched by the login's email and created from the login if it
// does not exist. No `customer_accounts` row is written: that table attaches a
// *customer login* to a billing account, and this user is already authenticated as the
// operator. Writing a link would be recording that the account is someone other than
// themselves, which is exactly the thing the operator should not have to say. The
// portal resolves staff the same way (portal.Manager.CustomerIDFor), so the panel and
// the portal agree on who the billing account is.
func (s *Server) billingAccountFor(ctx context.Context, tx pgx.Tx, u authn.User) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		SELECT c.id FROM customers c
		JOIN users uu ON lower(c.email) = lower(uu.email)
		WHERE uu.id = $1
		ORDER BY c.created_at
		LIMIT 1`, u.ID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("orders: resolve billing account: %w", err)
	}

	name := u.DisplayName
	if name == "" {
		name = u.Email
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
		RETURNING id`, u.Email, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("orders: create billing account: %w", err)
	}

	// Lost a race with a concurrent purchase for the same identity: read the row the
	// other transaction created.
	if err := tx.QueryRow(ctx, `
		SELECT id FROM customers WHERE lower(email) = lower($1)`, u.Email).Scan(&id); err != nil {
		return "", fmt.Errorf("orders: read billing account: %w", err)
	}
	return id, nil
}
