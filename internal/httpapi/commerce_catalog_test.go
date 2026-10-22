package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
)

// The commerce routes are exercised through the real router, because the properties that
// matter are properties of the *surface*: a customer cannot reach a staff route, a mutation
// without a CSRF token is refused, and every failure is problem+json with a stable code. A
// handler unit test would prove none of that.

// commerceProblem asserts the error envelope and returns the stable code.
func commerceProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) string {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, wantStatus, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, rec.Body)
	}
	if body.Code == "" {
		t.Fatalf("error body has no code: %s", rec.Body)
	}
	if body.Status != rec.Code {
		t.Fatalf("body status %d != HTTP status %d", body.Status, rec.Code)
	}
	if body.Type == "" || body.Title == "" {
		t.Fatalf("incomplete problem document: %s", rec.Body)
	}
	return body.Code
}

// doJSON performs a mutation with a JSON body, a CSRF token and an idempotency key.
func (e *authEnv) doJSON(t *testing.T, method, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "commerce-"+method+"-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// resetCommerceCurrencies restores the seeded currency rows, and schedules the same restore for
// when the test ends.
//
// `currencies` is reference data the test harness does not truncate (a fresh installation must
// be able to take a payment without configuration), so a test that adds one leaks into the
// next. Restoring on the way in makes the test independent of what ran before it; restoring on
// the way out stops this package's tests from leaking into the *other* tests in this shared
// package, which is the half that is easy to forget.
func resetCommerceCurrencies(t *testing.T, pool *store.Pool) {
	t.Helper()
	t.Cleanup(func() { restoreCommerceCurrencies(t, pool) })
	restoreCommerceCurrencies(t, pool)
}

// restoreCommerceCurrencies writes the seed back without scheduling anything.
func restoreCommerceCurrencies(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM currencies`); err != nil {
		t.Fatalf("clear currencies: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO currencies (code, name, scale, symbol, symbol_position, is_default, sort_order) VALUES
			('USD', 'US Dollar',        2, '$',  'prefix', true,  10),
			('EUR', 'Euro',             2, '€',  'prefix', false, 20),
			('GBP', 'Pound Sterling',   2, '£',  'prefix', false, 30),
			('CAD', 'Canadian Dollar',  2, 'CA$','prefix', false, 40),
			('AUD', 'Australian Dollar',2, 'A$', 'prefix', false, 50),
			('JPY', 'Japanese Yen',     0, '¥',  'prefix', false, 60)`); err != nil {
		t.Fatalf("restore currencies: %v", err)
	}
}

// seedCommerceCustomer creates a customer with a login and returns both ids.
func (e *authEnv) seedCommerceCustomer(t *testing.T, email string) (customerID, userID string) {
	t.Helper()
	return e.seedCustomer(t, email, "Commerce Customer")
}

// -------------------------------------------------------------- coupons --

// Creating a coupon is a staff mutation: a customer must be refused, and the refusal must be
// the problem envelope rather than a bare 403.
func TestCouponCreateRequiresPermission(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCommerceCustomer(t, "couponcustomer@example.test")
	customerCookie, customerCSRF := env.login(t, "couponcustomer@example.test", "customer password 1")

	body := map[string]any{
		"code": "TEN", "kind": "percentage", "percentage": "10",
	}
	// A customer holds no coupon:write.
	// The staff guard runs outermost, so a customer is told the area is for staff rather than
	// which permission was missing. Both refusals are the same envelope.
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		customerCookie, customerCSRF, body)
	if code := commerceProblem(t, rec, http.StatusForbidden); code != "staff_required" {
		t.Fatalf("code = %q, want staff_required", code)
	}

	// Support holds no coupon permission, so this is a real separation rather than "any staff
	// member". The code is `forbidden`, not `staff_required`: support *is* staff, and the
	// refusal is about the permission.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.support, env.supportCSRF, body)
	if code := commerceProblem(t, rec, http.StatusForbidden); code != "forbidden" {
		t.Fatalf("support create: code = %q, want forbidden", code)
	}

	// The billing role does, and the happy path works.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.billing, env.billingCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("billing create: status %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID         string `json:"id"`
		Code       string `json:"code"`
		Kind       string `json:"kind"`
		Percentage string `json:"percentage"`
		Enabled    bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if created.ID == "" || created.Code != "TEN" || created.Percentage != "10" || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}

	// The list is readable with coupon:read:all, and shows the new promotion.
	rec = env.get(t, "/api/v1/staff/coupons", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Data)
	}

	// And a duplicate code is a conflict with a stable code.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.billing, env.billingCSRF, body)
	if code := commerceProblem(t, rec, http.StatusConflict); code != "code_taken" {
		t.Fatalf("duplicate code = %q, want code_taken", code)
	}
}

// A mutation without a CSRF token is refused, which is the property the registrar applies.
func TestCommerceMutationRequiresCSRF(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.billing, "", map[string]any{"code": "NOCSRF", "kind": "percentage", "percentage": "10"})
	if code := commerceProblem(t, rec, http.StatusForbidden); code != "csrf_failed" {
		t.Fatalf("code = %q, want csrf_failed", code)
	}
}

// Retiring a coupon is an update, never a delete, and a retired coupon reads as missing to a
// customer validating it.
func TestCouponRetireIsAnUpdate(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.billing, env.billingCSRF, map[string]any{
			"code": "RETIREME", "kind": "percentage", "percentage": "10",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/coupons/"+created.ID,
		env.billing, env.billingCSRF, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", rec.Code, rec.Body)
	}
	var retired struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &retired); err != nil {
		t.Fatal(err)
	}
	if retired.Enabled {
		t.Fatal("the coupon is still enabled")
	}
	// The row still exists: a delete would have made it 404.
	if rec := env.get(t, "/api/v1/staff/coupons/"+created.ID, env.billing); rec.Code != http.StatusOK {
		t.Fatalf("read retired coupon: %d %s", rec.Code, rec.Body)
	}
	// An unknown id is a 404 with the envelope.
	rec = env.get(t, "/api/v1/staff/coupons/11111111-1111-1111-1111-111111111111", env.billing)
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// ------------------------------------------------------------- tax rates --

func TestTaxRateRoutes(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCommerceCustomer(t, "taxcustomer@example.test")
	customerCookie, customerCSRF := env.login(t, "taxcustomer@example.test", "customer password 1")

	body := map[string]any{
		"name": "Germany VAT", "country_code": "DE", "rate": "0.19",
	}
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates",
		customerCookie, customerCSRF, body)
	commerceProblem(t, rec, http.StatusForbidden)

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates",
		env.billing, env.billingCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID           string `json:"id"`
		Rate         string `json:"rate"`
		Jurisdiction string `json:"jurisdiction"`
		Inclusive    bool   `json:"inclusive"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Rate != "19/100" && created.Rate != "0.19" {
		t.Fatalf("rate = %q", created.Rate)
	}
	if created.Jurisdiction != "DE/standard" {
		t.Fatalf("jurisdiction = %q, want DE/standard", created.Jurisdiction)
	}

	// A float rate is refused: the rate is an exact rational, and a JSON number would be a
	// float on the way in.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates",
		env.billing, env.billingCSRF, map[string]any{
			"name": "Bad", "country_code": "FR", "rate": "0.2.1",
		})
	if code := commerceProblem(t, rec, http.StatusUnprocessableEntity); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}

	// A second open-ended rate for the same jurisdiction is a conflict.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates",
		env.billing, env.billingCSRF, map[string]any{
			"name": "Germany again", "country_code": "DE", "rate": "0.21",
		})
	if code := commerceProblem(t, rec, http.StatusConflict); code != "in_use" {
		t.Fatalf("code = %q, want in_use", code)
	}

	rec = env.get(t, "/api/v1/staff/tax-rates", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	rec = env.get(t, "/api/v1/staff/tax-rates/"+created.ID, env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}

	// Deleting a rate that has taken effect is refused: an invoice's evidence points at it.
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/tax-rates/"+created.ID,
		env.billing, env.billingCSRF, nil)
	if code := commerceProblem(t, rec, http.StatusConflict); code != "in_use" {
		t.Fatalf("code = %q, want in_use", code)
	}
}

// ------------------------------------------------------------- currencies --

// The public switcher is reachable without a session and returns only enabled currencies.
func TestPublicCurrenciesAreUnauthenticated(t *testing.T) {
	env := newAuthEnv(t)
	resetCommerceCurrencies(t, env.pool)
	rec := env.get(t, "/api/v1/currencies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			Code      string `json:"code"`
			IsDefault bool   `json:"is_default"`
			Example   string `json:"example"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) == 0 {
		t.Fatal("the seeded currencies are missing")
	}
	if body.Data[0].Code != "USD" || !body.Data[0].IsDefault {
		t.Fatalf("first currency = %+v, want the default USD first", body.Data[0])
	}
	if body.Data[0].Example != "$1,234.56" {
		t.Fatalf("example = %q, want \"$1,234.56\"", body.Data[0].Example)
	}
}

func TestCurrencyRoutes(t *testing.T) {
	env := newStaffEnv(t)
	resetCommerceCurrencies(t, env.pool)
	env.seedCommerceCustomer(t, "currencycustomer@example.test")
	customerCookie, customerCSRF := env.login(t, "currencycustomer@example.test", "customer password 1")

	// A customer cannot reach the staff surface at all.
	rec := env.get(t, "/api/v1/staff/currencies", customerCookie)
	commerceProblem(t, rec, http.StatusForbidden)

	rec = env.get(t, "/api/v1/staff/currencies", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	// Creating a currency is a staff mutation; a customer is refused.
	body := map[string]any{
		"code": "CHF", "name": "Swiss Franc", "scale": 2, "symbol": "CHF",
	}
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/currencies",
		customerCookie, customerCSRF, body)
	commerceProblem(t, rec, http.StatusForbidden)

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/currencies",
		env.billing, env.billingCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Code    string `json:"code"`
		Example string `json:"example"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Code != "CHF" || created.Example != "CHF1,234.56" {
		t.Fatalf("created = %+v", created)
	}

	// A duplicate code is a conflict.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/currencies",
		env.billing, env.billingCSRF, body)
	if code := commerceProblem(t, rec, http.StatusConflict); code != "code_taken" {
		t.Fatalf("code = %q, want code_taken", code)
	}

	// Disabling the default is refused: a storefront with no default has no currency to
	// create a price in.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/currencies/USD",
		env.billing, env.billingCSRF, map[string]any{"enabled": false})
	if code := commerceProblem(t, rec, http.StatusConflict); code != "in_use" {
		t.Fatalf("code = %q, want in_use", code)
	}
}

// ------------------------------------------------------------------ cart --

// seedCartPrice creates a product with one price and a quantity policy, returning both ids.
func (e *authEnv) seedCartPrice(t *testing.T, slug string, amount, setup int64, policy string) (productID, priceID string) {
	t.Helper()
	productID, priceID = e.seedPrice(t, slug, amount, setup)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE products SET allow_quantity = $2 WHERE id = $1`, productID, policy); err != nil {
		t.Fatalf("set allow_quantity: %v", err)
	}
	return productID, priceID
}

// The cart is the caller's own: another customer's line id is a 404, and the happy path
// returns the priced total.
func TestPortalCartHappyPathAndScoping(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	env.seedCommerceCustomer(t, "alice-cart@example.test")
	aliceCookie, aliceCSRF := env.login(t, "alice-cart@example.test", "customer password 1")
	env.seedCommerceCustomer(t, "bob-cart@example.test")
	bobCookie, _ := env.login(t, "bob-cart@example.test", "customer password 1")

	_, priceID := env.seedCartPrice(t, "cart-vps", 1000, 500, "combined")

	// An empty cart is created on first read.
	rec := env.get(t, "/api/v1/portal/cart", aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("get cart: %d %s", rec.Code, rec.Body)
	}
	var cart struct {
		ID       string `json:"id"`
		Currency string `json:"currency"`
		Items    []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cart); err != nil {
		t.Fatal(err)
	}
	if cart.ID == "" || cart.Currency != "USD" || len(cart.Items) != 0 {
		t.Fatalf("empty cart = %+v", cart)
	}

	// Add an item.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/items",
		aliceCookie, aliceCSRF, map[string]any{
			"price_version_id": priceID, "quantity": 2,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add item: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cart); err != nil {
		t.Fatal(err)
	}
	if len(cart.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(cart.Items))
	}
	itemID := cart.Items[0].ID

	// The total re-prices from the catalog: 2 * (1000 + 500) = 3000.
	rec = env.get(t, "/api/v1/portal/cart/totals", aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("totals: %d %s", rec.Code, rec.Body)
	}
	var totals struct {
		Total struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"total"`
		Subtotal struct {
			Minor int64 `json:"minor"`
		} `json:"subtotal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &totals); err != nil {
		t.Fatal(err)
	}
	if totals.Total.Minor != 3000 || totals.Total.Currency != "USD" {
		t.Fatalf("total = %+v, want 3000 USD", totals.Total)
	}

	// Bob cannot touch Alice's line: the scoping is in SQL, so the answer is a 404 rather
	// than a 403 that would confirm the id exists.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/portal/cart/items/"+itemID,
		bobCookie, "", map[string]any{"quantity": 5})
	// No CSRF token means the wrapper refuses first; send a real one for Bob.
	bobCookie2, bobCSRF := env.login(t, "bob-cart@example.test", "customer password 1")
	rec = env.doJSON(t, http.MethodPut, "/api/v1/portal/cart/items/"+itemID,
		bobCookie2, bobCSRF, map[string]any{"quantity": 5})
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("cross-customer update: code = %q, want not_found", code)
	}

	// Update the quantity as the owner.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/portal/cart/items/"+itemID,
		aliceCookie, aliceCSRF, map[string]any{"quantity": 3})
	if rec.Code != http.StatusOK {
		t.Fatalf("update quantity: %d %s", rec.Code, rec.Body)
	}
	rec = env.get(t, "/api/v1/portal/cart/totals", aliceCookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &totals); err != nil {
		t.Fatal(err)
	}
	if totals.Total.Minor != 4500 {
		t.Fatalf("total at quantity 3 = %d, want 4500", totals.Total.Minor)
	}

	// Remove the line, then confirm the cart is empty.
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/portal/cart/items/"+itemID,
		aliceCookie, aliceCSRF, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	rec = env.get(t, "/api/v1/portal/cart", aliceCookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &cart); err != nil {
		t.Fatal(err)
	}
	if len(cart.Items) != 0 {
		t.Fatalf("items after remove = %d, want 0", len(cart.Items))
	}
}

// Applying a coupon returns the live total, and an unknown code is a 404 with the envelope.
func TestPortalCartApplyCoupon(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.seedCommerceCustomer(t, "cart-coupon@example.test")
	cookie, csrf := env.login(t, "cart-coupon@example.test", "customer password 1")

	_, priceID := env.seedCartPrice(t, "cart-coupon-vps", 1000, 0, "combined")
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons",
		env.billing, env.billingCSRF, map[string]any{
			"code": "SAVE10", "kind": "percentage", "percentage": "10",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create coupon: %d %s", rec.Code, rec.Body)
	}
	if rec := env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/items",
		cookie, csrf, map[string]any{"price_version_id": priceID, "quantity": 1}); rec.Code != http.StatusCreated {
		t.Fatalf("add item: %d %s", rec.Code, rec.Body)
	}

	// An unknown code is a 404, not a 500.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/coupon",
		cookie, csrf, map[string]any{"code": "NOPE"})
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("unknown code = %q, want not_found", code)
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/coupon",
		cookie, csrf, map[string]any{"code": "SAVE10"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var applied struct {
		Totals struct {
			Discount struct {
				Minor int64 `json:"minor"`
			} `json:"discount"`
			Total struct {
				Minor int64 `json:"minor"`
			} `json:"total"`
			Coupon struct {
				Code string `json:"code"`
			} `json:"coupon"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Totals.Discount.Minor != 100 || applied.Totals.Total.Minor != 900 {
		t.Fatalf("totals = %+v, want discount 100 total 900", applied.Totals)
	}
	if applied.Totals.Coupon.Code != "SAVE10" {
		t.Fatalf("coupon = %q", applied.Totals.Coupon.Code)
	}

	// Removing it restores the total.
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/portal/cart/coupon",
		cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove coupon: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Totals.Total.Minor != 1000 {
		t.Fatalf("total after remove = %d, want 1000", applied.Totals.Total.Minor)
	}
}

// A customer without a billing account gets a 404 rather than a 500: "no cart yet" is a state,
// not a failure.
func TestPortalCartWithoutBillingAccount(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.createUser(t, "nolink@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "nolink@example.test", "customer password 1")

	rec := env.get(t, "/api/v1/portal/cart", cookie)
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// The cart routes are guarded: an anonymous caller is refused, and a portal that is not
// configured answers 503 rather than panicking.
func TestPortalCartRequiresSessionAndPortal(t *testing.T) {
	env := newAuthEnv(t)
	// No WithPortal: the route still exists, and it must refuse cleanly.
	rec := env.get(t, "/api/v1/portal/cart", nil)
	if code := commerceProblem(t, rec, http.StatusUnauthorized); code != "authentication_required" {
		t.Fatalf("anonymous code = %q, want authentication_required", code)
	}

	env.seedCommerceCustomer(t, "noportal@example.test")
	cookie, _ := env.login(t, "noportal@example.test", "customer password 1")
	rec = env.get(t, "/api/v1/portal/cart", cookie)
	if code := commerceProblem(t, rec, http.StatusServiceUnavailable); code != "portal_unavailable" {
		t.Fatalf("unconfigured portal code = %q, want portal_unavailable", code)
	}
}
