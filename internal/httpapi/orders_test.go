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
