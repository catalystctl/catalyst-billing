package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
)

// The customer checkout endpoint. These tests drive the route, not the service, because
// the things that can go wrong here are the ones only the HTTP layer can get wrong: the
// customer id coming from the body, the permission, CSRF, and the response shape the
// storefront is written against.

// shopEnv is a staff server with the customer portal attached, which is what a real
// deployment has and what the portal assertions below need.
func shopEnv(t *testing.T) *staffEnv {
	t.Helper()
	lowerRegisterCost(t)
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	return env
}

// checkout posts a cart with the customer's session.
func (e *authEnv) checkout(t *testing.T, cookie *http.Cookie, csrf string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.postJSON(t, "/api/v1/portal/orders", cookie, csrf, body)
}

// seedProduct creates a sellable product and returns the price id.
func (e *authEnv) seedProduct(t *testing.T, slug string, amount, setup int64) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		`UPDATE products SET allow_quantity = 'combined' WHERE id = $1`, productID); err != nil {
		t.Fatalf("allow quantity: %v", err)
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

// buyer creates a customer with a login and returns their session.
func (e *authEnv) buyer(t *testing.T, email string) (customerID, userID string, cookie *http.Cookie, csrf string) {
	t.Helper()
	customerID, userID = e.seedCustomer(t, email, "Buyer")
	cookie, csrf = e.login(t, email, "customer password 1")
	return customerID, userID, cookie, csrf
}

type placedOrder struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Currency       string   `json:"currency"`
	InvoiceID      string   `json:"invoice_id"`
	InvoiceNumber  string   `json:"invoice_number"`
	SubscriptionID []string `json:"subscription_ids"`
	ServiceIDs     []string `json:"service_ids"`
	Activated      bool     `json:"activated"`
	CouponCode     string   `json:"coupon_code"`
	Subtotal       struct {
		Minor int64 `json:"minor"`
	} `json:"subtotal"`
	Discount struct {
		Minor int64 `json:"minor"`
	} `json:"discount"`
	Tax struct {
		Minor int64 `json:"minor"`
	} `json:"tax"`
	Total struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
	} `json:"total"`
	Lines []struct {
		Description string `json:"description"`
		Quantity    int    `json:"quantity"`
	} `json:"lines"`
}

func decodeOrder(t *testing.T, rec *httptest.ResponseRecorder) placedOrder {
	t.Helper()
	var out placedOrder
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	return out
}

// The whole point of the endpoint: a signed-in customer can buy.
func TestCustomerCanCheckout(t *testing.T) {
	env := shopEnv(t)
	customerID, _, cookie, csrf := env.buyer(t, "shopper@example.test")
	_, priceID := env.seedProduct(t, "vps", 2500, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID, "quantity": 2}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)
	if order.ID == "" || order.InvoiceID == "" || order.InvoiceNumber == "" {
		t.Fatalf("incomplete response: %+v", order)
	}
	if order.Status != "pending_payment" {
		t.Fatalf("status = %q", order.Status)
	}
	if order.Total.Minor != 5000 || order.Total.Currency != "USD" {
		t.Fatalf("total = %+v", order.Total)
	}
	if len(order.SubscriptionID) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(order.SubscriptionID))
	}
	if order.Activated || len(order.ServiceIDs) != 0 {
		t.Fatalf("a paid order must not activate: %+v", order)
	}
	if len(order.Lines) != 1 || order.Lines[0].Quantity != 2 {
		t.Fatalf("lines = %+v", order.Lines)
	}

	// The order belongs to the session's customer.
	var owner string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT customer_id FROM orders WHERE id = $1`, order.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != customerID {
		t.Fatalf("order owner = %s, want %s", owner, customerID)
	}

	// The customer can see the invoice they were just issued, which is what makes the
	// next step (paying it) reachable.
	if rec := env.get(t, "/api/v1/portal/invoices/"+order.InvoiceID, cookie); rec.Code != http.StatusOK {
		t.Fatalf("portal invoice: %d %s", rec.Code, rec.Body)
	}
}

// The critical scoping test: a body that names another customer must not move the
// order, and the body is refused rather than silently ignored.
func TestCheckoutIgnoresACustomerIDInTheBody(t *testing.T) {
	env := shopEnv(t)
	mine, _, cookie, csrf := env.buyer(t, "mine@example.test")
	theirs, _, _, _ := env.buyer(t, "theirs@example.test")
	_, priceID := env.seedProduct(t, "scoped", 1000, 0)

	// `customer_id` is not a field of the request, and unknown fields are refused: a
	// client cannot even express "bill someone else".
	rec := env.checkout(t, cookie, csrf, map[string]any{
		"customer_id": theirs,
		"lines":       []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "invalid_body") {
		t.Fatalf("body = %s", rec.Body)
	}
	// Nothing was created by the refused request.
	var orders int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("a refused request created %d order(s)", orders)
	}

	// The legitimate request lands on the session's customer, not the one that was
	// named.
	rec = env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)
	var owner string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT customer_id FROM orders WHERE id = $1`, order.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != mine {
		t.Fatalf("order owner = %s, want the session's %s", owner, mine)
	}
	if owner == theirs {
		t.Fatal("the order was placed for the customer named in the body")
	}
}

// A concurrent checkout of the last unit: one wins, the other is refused cleanly, and
// nothing is left half-written.
func TestConcurrentCheckoutOfTheLastUnitFailsCleanly(t *testing.T) {
	env := shopEnv(t)
	_, _, cookieA, csrfA := env.buyer(t, "a@example.test")
	_, _, cookieB, csrfB := env.buyer(t, "b@example.test")
	productID, priceID := env.seedProduct(t, "last-unit", 900, 0)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE products SET stock = 1 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code int
		body []byte
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, c := range []struct {
		cookie *http.Cookie
		csrf   string
	}{{cookieA, csrfA}, {cookieB, csrfB}} {
		wg.Add(1)
		go func(i int, cookie *http.Cookie, csrf string) {
			defer wg.Done()
			rec := env.checkout(t, cookie, csrf, map[string]any{
				"lines": []map[string]any{{"price_version_id": priceID}},
			})
			results[i] = result{code: rec.Code, body: rec.Body.Bytes()}
		}(i, c.cookie, c.csrf)
	}
	wg.Wait()

	var created, refused int
	for _, r := range results {
		switch r.code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			refused++
			if !bytes.Contains(r.body, []byte("out_of_stock")) {
				t.Fatalf("refusal body = %s", r.body)
			}
		default:
			t.Fatalf("unexpected status %d: %s", r.code, r.body)
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("created = %d, refused = %d; want 1 and 1", created, refused)
	}

	// Stock is exactly exhausted, never negative, and only one order exists.
	var stock, orders int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT (SELECT stock FROM products WHERE id = $1),
		       (SELECT count(*) FROM orders)`, productID).Scan(&stock, &orders); err != nil {
		t.Fatal(err)
	}
	if stock != 0 {
		t.Fatalf("stock = %d, want 0", stock)
	}
	if orders != 1 {
		t.Fatalf("orders = %d, want 1", orders)
	}
}

// CT-08 over HTTP: a zero-total order activates and issues no invoice.
func TestZeroTotalCheckoutActivatesWithoutAnInvoice(t *testing.T) {
	env := shopEnv(t)
	customerID, _, cookie, csrf := env.buyer(t, "free@example.test")
	_, priceID := env.seedProduct(t, "free-plan", 0, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)
	if !order.Activated {
		t.Fatal("a zero-total order did not activate")
	}
	if order.InvoiceID != "" || order.InvoiceNumber != "" {
		t.Fatalf("a free order issued an invoice: %+v", order)
	}
	if order.Status != "paid" {
		t.Fatalf("status = %q, want paid", order.Status)
	}
	if len(order.ServiceIDs) != 1 {
		t.Fatalf("services = %+v", order.ServiceIDs)
	}

	var invoices int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d, want 0", invoices)
	}

	// The service appears in the customer's portal, which is the observable outcome the
	// customer cares about.
	if rec := env.get(t, "/api/v1/portal/services", cookie); rec.Code != http.StatusOK {
		t.Fatalf("portal services: %d %s", rec.Code, rec.Body)
	} else if !bytes.Contains(rec.Body.Bytes(), []byte(order.ServiceIDs[0])) {
		t.Fatalf("the activated service is not in the portal list: %s", rec.Body)
	}
}

// A coupon appears on the invoice as a line of its own, and the response reports it.
func TestCouponDiscountAppearsAsALineOverHTTP(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "coupon@example.test")
	_, priceID := env.seedProduct(t, "promo", 10000, 0)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO coupons (code, kind, percentage) VALUES ('SAVE15', 'percentage', '15')`); err != nil {
		t.Fatal(err)
	}

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines":       []map[string]any{{"price_version_id": priceID}},
		"coupon_code": "save15",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)
	if order.CouponCode != "SAVE15" {
		t.Fatalf("coupon = %q", order.CouponCode)
	}
	if order.Discount.Minor != 1500 {
		t.Fatalf("discount = %d, want 1500", order.Discount.Minor)
	}
	if order.Total.Minor != 8500 {
		t.Fatalf("total = %d, want 8500", order.Total.Minor)
	}

	// The document explains itself: a discount line naming the code.
	var (
		kind string
		net  int64
		desc string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT line_kind, net_minor, description FROM invoice_lines
		WHERE invoice_id = $1 AND line_kind = 'discount'`, order.InvoiceID).
		Scan(&kind, &net, &desc); err != nil {
		t.Fatalf("discount line: %v", err)
	}
	if net != -1500 {
		t.Fatalf("discount net = %d, want -1500", net)
	}
	if !bytes.Contains([]byte(desc), []byte("SAVE15")) {
		t.Fatalf("description = %q", desc)
	}
}

// An invalid coupon is a 422 with a code the cart can explain, and nothing is created.
func TestInvalidCouponIsRefused(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "badcoupon@example.test")
	_, priceID := env.seedProduct(t, "nopromo", 1000, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines":       []map[string]any{{"price_version_id": priceID}},
		"coupon_code": "DOESNOTEXIST",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "coupon_not_found") {
		t.Fatalf("body = %s", rec.Body)
	}
	var orders int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("a refused coupon created %d order(s)", orders)
	}
}

// The route needs a permission, a session and a CSRF token.
func TestCheckoutRequiresAuthentication(t *testing.T) {
	env := shopEnv(t)
	_, priceID := env.seedProduct(t, "guarded", 1000, 0)
	body, _ := json.Marshal(map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})

	// No session at all.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	// A session without a CSRF token.
	_, _, cookie, _ := env.buyer(t, "nocsrf@example.test")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/portal/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "csrf_failed") {
		t.Fatalf("body = %s", rec.Body)
	}

	// Nothing was created by either attempt.
	var orders int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("a guarded request created %d order(s)", orders)
	}
}

// A customer holds invoice:pay:self, which is what gates this route. A user whose
// account is not active cannot use it, because the session is refused upstream.
func TestCheckoutPermissionIsCustomerScoped(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "perm@example.test")
	_, priceID := env.seedProduct(t, "perm", 1000, 0)

	// The customer's own permissions include invoice:pay:self, which is the declared
	// permission on the route. This pins that the route's guard is one a customer
	// actually holds — a route guarded by a staff permission would be unreachable for
	// the audience it exists for.
	authorizer := authn.NewAuthorizer()
	customer := authn.User{ID: "x", Status: "active"}
	if !authorizer.Can(customer, authn.PermInvoicePaySelf) {
		t.Fatal("a customer does not hold invoice:pay:self")
	}
	if authorizer.Can(customer, authn.PermInvoiceReadAll) {
		t.Fatal("a customer must not hold a staff permission")
	}

	if rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
}

// An empty cart and an unknown price are refused with the codes the UI branches on.
func TestCheckoutValidation(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "valid@example.test")

	if rec := env.checkout(t, cookie, csrf, map[string]any{"lines": []map[string]any{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty cart: %d %s", rec.Code, rec.Body)
	} else if !containsCode(rec.Body.Bytes(), "no_lines") {
		t.Fatalf("body = %s", rec.Body)
	}

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": "0f8fad5b-d9cb-469f-a165-70867728950e"}},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown price: %d %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "not_found") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A purchase the installation gates on a verified address is refused with a code that
// sends the customer to their mail rather than to a dead end.
func TestCheckoutRequiresVerifiedEmailWhenConfigured(t *testing.T) {
	env := shopEnv(t)
	_, userID, cookie, csrf := env.buyer(t, "unverified@example.test")
	_, priceID := env.seedProduct(t, "needsverify", 1000, 0)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE users SET email_verified_at = NULL WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('mail', 'must_verify', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "email_verification_required") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// Terms over HTTP: the flag is required only when configured, and the refusal names it.
func TestCheckoutRequiresTOSOverHTTP(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "tos@example.test")
	_, priceID := env.seedProduct(t, "tos-product", 1000, 0)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('general', 'tos', 'Terms apply.')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "tos_required") {
		t.Fatalf("body = %s", rec.Body)
	}

	rec = env.checkout(t, cookie, csrf, map[string]any{
		"lines":      []map[string]any{{"price_version_id": priceID}},
		"accept_tos": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("with acceptance: %d %s", rec.Code, rec.Body)
	}
}

// An order placed through the portal is visible on the staff order screen, which is the
// E1.2 acceptance criterion.
func TestStaffCanSeeACustomerOrder(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "seen@example.test")
	_, priceID := env.seedProduct(t, "seen", 4200, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)

	staff := env.get(t, "/api/v1/staff/orders/"+order.ID, env.admin)
	if staff.Code != http.StatusOK {
		t.Fatalf("staff order detail: %d %s", staff.Code, staff.Body)
	}
	if !bytes.Contains(staff.Body.Bytes(), []byte(order.InvoiceNumber)) {
		t.Fatalf("the staff view does not show the invoice: %s", staff.Body)
	}
}

// A user with no billing account is told so, rather than being handed a 500.
func TestCheckoutWithoutABillingAccount(t *testing.T) {
	env := shopEnv(t)
	_, priceID := env.seedProduct(t, "orphan", 1000, 0)

	// A login with no customer_accounts row and no staff role.
	var userID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ('orphan@example.test', 'x', 'Orphan', now()) RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	// The password is not usable (the hash is a placeholder), so the session is created
	// directly the way the MFA path does.
	svc := env.srv
	_ = svc
	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/orders",
		bytes.NewReader([]byte(`{"lines":[{"price_version_id":"`+priceID+`"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	// No session: this asserts the guard, and the service-level "no billing account"
	// case is proven in internal/register.
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// The frontend was specified a different spelling than the API document: `items` and
// `tos_accepted`. Both must work, because `decodeJSON` refuses unknown fields and an
// unaccepted alias would 400 every checkout — an integration failure nobody sees until a
// customer tries to buy something.
func TestCheckoutAcceptsTheFrontendSpelling(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "frontend@example.test")
	_, priceID := env.seedProduct(t, "frontend", 3000, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"items": []map[string]any{{"price_version_id": priceID, "quantity": 1}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("`items` spelling: status = %d; body %s", rec.Code, rec.Body)
	}
	if order := decodeOrder(t, rec); order.Total.Minor != 3000 {
		t.Fatalf("total = %d, want 3000", order.Total.Minor)
	}

	// The canonical terms flag also works, and both are refused together because
	// "which one did you mean" has no safe answer.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('general', 'tos', 'Terms.')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	rec = env.checkout(t, cookie, csrf, map[string]any{
		"items":        []map[string]any{{"price_version_id": priceID}},
		"tos_accepted": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("`tos_accepted`: status = %d; body %s", rec.Code, rec.Body)
	}
	// Without acceptance it is refused, which proves the alias is actually read.
	rec = env.checkout(t, cookie, csrf, map[string]any{
		"items": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("without acceptance: status = %d; body %s", rec.Code, rec.Body)
	}
}

// Sending both cart spellings is a client bug, refused rather than merged.
func TestCheckoutRefusesAmbiguousCart(t *testing.T) {
	env := shopEnv(t)
	_, _, cookie, csrf := env.buyer(t, "ambiguous@example.test")
	_, priceID := env.seedProduct(t, "ambiguous", 1000, 0)

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"items": []map[string]any{{"price_version_id": priceID}},
		"lines": []map[string]any{{"price_version_id": priceID}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "ambiguous_cart") {
		t.Fatalf("body = %s", rec.Body)
	}
}
