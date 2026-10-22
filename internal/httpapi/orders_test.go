package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/catalyst/billing/internal/portal"
)

// Buying a product is the path the panel was missing: the catalog could be browsed and
// priced, but there was no way to turn a price into a payable order. These tests drive the
// route, not the service, because the thing that was missing was the route.

// seedPrice creates a sellable product with one price and returns the price id.
func (e *authEnv) seedPrice(t *testing.T, slug string, amount, setup int64) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, 1, 'USD', $2, $3, 'monthly', 1, 'UTC', 'Monthly')
		RETURNING id`, productID, amount, setup).Scan(&priceID); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return productID, priceID
}

type createdOrder struct {
	ID             string `json:"id"`
	InvoiceID      string `json:"invoice_id"`
	Number         string `json:"number"`
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status"`
	Total          struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
	} `json:"total"`
}

func TestStaffCanBuyAProductForACustomer(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "buyer@example.test", "Buyer")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceID,
		"quantity":         2,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var created createdOrder
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if created.ID == "" || created.InvoiceID == "" || created.SubscriptionID == "" {
		t.Fatalf("incomplete response: %+v", created)
	}
	if created.Status != "pending_payment" {
		t.Fatalf("status = %q", created.Status)
	}
	if created.Total.Minor != 2400 || created.Total.Currency != "USD" {
		t.Fatalf("total = %+v", created.Total)
	}
	if created.Number == "" {
		t.Fatal("the invoice was not issued")
	}

	// The order belongs to the customer that was named, not to the caller.
	var orderCustomer string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT customer_id FROM orders WHERE id = $1`, created.ID).Scan(&orderCustomer); err != nil {
		t.Fatal(err)
	}
	if orderCustomer != customerID {
		t.Fatalf("order customer = %s, want %s", orderCustomer, customerID)
	}
}

// The point of the whole change: a staff member is the billing account, so buying for
// themselves must not require a customer_accounts row linking them to an account.
func TestStaffCanBuyForThemselvesWithoutALinkedAccount(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	// Precondition: the administrator is a user, and has no customer record at all.
	var before int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM customers WHERE lower(email) = 'admin@example.test'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("precondition: %d customer rows already exist", before)
	}

	rec := env.postJSON(t, "/api/v1/staff/orders", env.admin, env.adminCSRF, map[string]any{
		"price_version_id": priceID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	// The billing account was created from the login…
	var customerID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM customers WHERE lower(email) = 'admin@example.test'`).Scan(&customerID); err != nil {
		t.Fatalf("billing account was not created: %v", err)
	}
	// …and no customer_accounts link was written, because the operator is the account.
	var links int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM customer_accounts ca
		JOIN users u ON u.id = ca.user_id
		WHERE lower(u.email) = 'admin@example.test'`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Fatalf("customer_accounts rows = %d, want 0", links)
	}

	// The portal resolves the staff member to that billing account anyway, so they can see
	// and pay the invoice they just created.
	overview := env.get(t, "/api/v1/portal/overview", env.admin)
	if overview.Code != http.StatusOK {
		t.Fatalf("portal overview: status %d; body %s", overview.Code, overview.Body)
	}
	var body struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview body: %v\n%s", err, overview.Body)
	}
	if body.Customer.ID != customerID {
		t.Fatalf("portal customer = %q, want %q", body.Customer.ID, customerID)
	}
}

func TestCreateOrderRejectsUnknownRecords(t *testing.T) {
	env := newStaffEnv(t)
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"price_version_id": "11111111-1111-1111-1111-111111111111",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown price: status = %d; body %s", rec.Code, rec.Body)
	}

	rec = env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":      "22222222-2222-2222-2222-222222222222",
		"price_version_id": priceID,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown customer: status = %d; body %s", rec.Code, rec.Body)
	}

	// A malformed id never reaches the database and is indistinguishable from a missing one.
	rec = env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"price_version_id": "not-a-uuid",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed price: status = %d; body %s", rec.Code, rec.Body)
	}
}

func TestCreateOrderRejectsInvalidQuantity(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "buyer@example.test", "Buyer")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceID,
		"quantity":         0,
	})
	// Zero means "one", so this succeeds; the invalid case is a negative quantity.
	if rec.Code != http.StatusCreated {
		t.Fatalf("zero quantity: status = %d; body %s", rec.Code, rec.Body)
	}

	rec = env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceID,
		"quantity":         -1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative quantity: status = %d; body %s", rec.Code, rec.Body)
	}
}

// Support may read orders but may not create one: creating an order issues an invoice.
func TestCreateOrderRequiresInvoiceWrite(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "buyer@example.test", "Buyer")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.support, env.supportCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceID,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support: status = %d; body %s", rec.Code, rec.Body)
	}
}

// --------------------------------------------------------------- custom price --

// customOrder is the part of the create response the custom-price tests care about.
type customOrder struct {
	ID               string `json:"id"`
	InvoiceID        string `json:"invoice_id"`
	ManualPrice      bool   `json:"manual_price"`
	CustomPriceMinor int64  `json:"custom_price_minor"`
	Total            struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
	} `json:"total"`
}

// Selling at a negotiated price is the operator's version of the catalog: one customer, one
// number, agreed. The amount is minor units, the line records that it was manual and why, and
// the catalog — the price list everybody else pays — is untouched.
func TestStaffCanCreateAnOrderAtACustomPrice(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	customerID, _ := env.seedCustomer(t, "negotiated@example.test", "Negotiated")
	productID, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":         customerID,
		"price_version_id":    priceID,
		"quantity":            2,
		"custom_price_minor":  450,
		"custom_price_reason": "  retention price agreed with the customer  ",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var created customOrder
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if !created.ManualPrice || created.CustomPriceMinor != 450 {
		t.Fatalf("response = manual %v custom %d, want true/450", created.ManualPrice, created.CustomPriceMinor)
	}
	// 2 x 4.50: the negotiated price is the whole price of one unit.
	if created.Total.Minor != 900 || created.Total.Currency != "USD" {
		t.Fatalf("total = %+v, want 900 USD", created.Total)
	}

	// The invoice line is the record of what was charged: the override and the reason are on
	// it, so a later reader can tell it from a catalog price without joining the order.
	var (
		unit     int64
		manual   bool
		reason   *string
		invPrice *string
	)
	if err := env.pool.QueryRow(ctx, `
		SELECT unit_minor, manual_price, manual_price_reason, price_version_id
		  FROM invoice_lines WHERE invoice_id = $1 AND position = 1`, created.InvoiceID).
		Scan(&unit, &manual, &reason, &invPrice); err != nil {
		t.Fatalf("read invoice line: %v", err)
	}
	if unit != 450 {
		t.Fatalf("invoice unit = %d, want 450", unit)
	}
	if !manual || reason == nil || *reason != "retention price agreed with the customer" {
		t.Fatalf("invoice line manual=%v reason=%v", manual, reason)
	}
	// The version is still recorded: it says what was sold, not what it cost.
	if invPrice == nil || *invPrice != priceID {
		t.Fatalf("invoice line price version = %v, want %s", invPrice, priceID)
	}

	// The order item agrees with the line.
	var itemManual bool
	var itemReason *string
	if err := env.pool.QueryRow(ctx, `
		SELECT manual_price, manual_price_reason FROM order_items WHERE order_id = $1`,
		created.ID).Scan(&itemManual, &itemReason); err != nil {
		t.Fatalf("read order item: %v", err)
	}
	if !itemManual || itemReason == nil || *itemReason != "retention price agreed with the customer" {
		t.Fatalf("order item manual=%v reason=%v", itemManual, itemReason)
	}

	// The catalog is exactly what it was: same amount, same single version, same product name.
	var amount int64
	var versions int
	var name string
	if err := env.pool.QueryRow(ctx, `
		SELECT pv.amount_minor,
		       (SELECT count(*) FROM price_versions WHERE product_id = $2),
		       (SELECT name FROM products WHERE id = $2)
		  FROM price_versions pv WHERE pv.id = $1`, priceID, productID).
		Scan(&amount, &versions, &name); err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	if amount != 1200 || versions != 1 {
		t.Fatalf("catalog changed: amount %d, versions %d, want 1200/1", amount, versions)
	}

	// The audit row carries the amount and the reason: "who changed the price, to what and
	// why" is the question it exists to answer.
	var changes []byte
	if err := env.pool.QueryRow(ctx, `
		SELECT changes FROM audit_events
		 WHERE target_id = $1 AND action = 'order.create'
		 ORDER BY created_at DESC LIMIT 1`, created.ID).Scan(&changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var diff map[string]any
	if err := json.Unmarshal(changes, &diff); err != nil {
		t.Fatalf("changes: %v\n%s", err, changes)
	}
	if diff["custom_price_minor"] != float64(450) ||
		diff["custom_price_reason"] != "retention price agreed with the customer" {
		t.Fatalf("audit changes = %v", diff)
	}
}

// A custom price without a reason is refused, and the refusal writes nothing. The message is
// a 400 with a stable code, because the screen branches on the code, not the prose.
func TestCreateOrderRefusesACustomPriceWithoutAReason(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	customerID, _ := env.seedCustomer(t, "noreason@example.test", "No Reason")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	for name, body := range map[string]map[string]any{
		"absent": {
			"customer_id":        customerID,
			"price_version_id":   priceID,
			"custom_price_minor": 450,
		},
		"blank": {
			"customer_id":         customerID,
			"price_version_id":    priceID,
			"custom_price_minor":  450,
			"custom_price_reason": "   ",
		},
	} {
		rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s reason: status = %d; body %s", name, rec.Code, rec.Body)
		}
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s reason: body: %v\n%s", name, err, rec.Body)
		}
		if problem.Code != "custom_price_reason_required" {
			t.Fatalf("%s reason: code = %q, want custom_price_reason_required", name, problem.Code)
		}
	}

	// Nothing was created by either attempt.
	var orders int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("orders = %d after refused orders, want 0", orders)
	}
}

// The amount has to be a positive number of minor units: zero is not a discount and a
// negative amount is a refund, which is a different operation with its own endpoint.
func TestCreateOrderRefusesAnUnchargeableCustomPrice(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "cheapskate@example.test", "Cheapskate")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	for name, minor := range map[string]int{"zero": 0, "negative": -450} {
		rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
			"customer_id":         customerID,
			"price_version_id":    priceID,
			"custom_price_minor":  minor,
			"custom_price_reason": "should not be accepted",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d; body %s", name, rec.Code, rec.Body)
		}
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: body: %v\n%s", name, err, rec.Body)
		}
		if problem.Code != "invalid_custom_price" {
			t.Fatalf("%s: code = %q, want invalid_custom_price", name, problem.Code)
		}
	}
}

// A reason with no amount is a client bug, and the dangerous kind: if it were ignored the order
// would be charged at the catalog price while the operator believes it was discounted. It is
// refused instead.
func TestCreateOrderRefusesAReasonWithoutAnAmount(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "halfsent@example.test", "Half Sent")
	_, priceID := env.seedPrice(t, "vps", 1200, 0)

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":         customerID,
		"price_version_id":    priceID,
		"custom_price_reason": "meant to send an amount too",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if problem.Code != "custom_price_required" {
		t.Fatalf("code = %q, want custom_price_required", problem.Code)
	}
}

// The override is not a way around the catalog's identity: a price version that does not
// exist is still a 404, custom price or not.
func TestCustomPriceStillRequiresAKnownPriceVersion(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "ghost@example.test", "Ghost")

	rec := env.postJSON(t, "/api/v1/staff/orders", env.billing, env.billingCSRF, map[string]any{
		"customer_id":         customerID,
		"price_version_id":    "11111111-1111-1111-1111-111111111111",
		"custom_price_minor":  450,
		"custom_price_reason": "a discount on nothing",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
}
