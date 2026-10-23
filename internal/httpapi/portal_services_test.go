package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Managing a service from the portal (UPG-01…UPG-11, SVC-02…SVC-08).
//
// The three mutations a customer can make to their own service — request an upgrade, change a
// config option, edit the properties the operator made visible — were served and unreachable: no
// screen called any of them, so every plan change in the product was a support ticket. These
// tests pin the properties the screen depends on, not the happy path:
//
//   - the request reaches the staff queue, which is what makes it a request rather than a click
//     into the void;
//   - the price the customer is shown comes from the same row the charge is computed from;
//   - another customer's service identifier is a 404 on every one of them, and nothing is written;
//   - every mutation is audited inside its own transaction and replays safely under one
//     Idempotency-Key, because a retried upgrade that issued a second invoice would be a real
//     charge for a change the customer made once.

// portalMutation sends a mutation with an explicit idempotency key, so a test can replay one.
func (e *authEnv) portalMutation(t *testing.T, method, path string, cookie *http.Cookie, csrf,
	key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// freshKey is a unique idempotency key per call site.
func freshKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// planFacts are the two ids a service's pricing hangs from: the product it was sold on and the
// price version its subscription renews at.
func (e *authEnv) planFacts(t *testing.T, serviceID string) (productID, priceVersionID string) {
	t.Helper()
	if err := e.pool.QueryRow(t.Context(), `
		SELECT s.product_id, COALESCE(sub.price_version_id::text, '')
		FROM services s
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.id = $1`, serviceID).Scan(&productID, &priceVersionID); err != nil {
		t.Fatalf("plan facts for %s: %v", serviceID, err)
	}
	if productID == "" || priceVersionID == "" {
		t.Fatalf("the fixture service %s has no product or no schedule", serviceID)
	}
	return productID, priceVersionID
}

// seedUpgradeTarget adds a reachable product with one price, and returns that price's id.
//
// The edge is what makes the target legal: `Upgrade` refuses a price version of a product the
// upgrade graph does not connect, which is the rule that stops a customer moving to an unrelated
// product by guessing an id.
func (e *authEnv) seedUpgradeTarget(t *testing.T, serviceID string, amountMinor int64) (productID, priceID string) {
	t.Helper()
	fromProduct, _ := e.planFacts(t, serviceID)
	if err := e.pool.QueryRow(t.Context(), `
		INSERT INTO products (slug, name) VALUES ($1, 'VPS Medium') RETURNING id`,
		"upgrade-"+serviceID[:8]).Scan(&productID); err != nil {
		t.Fatalf("seed upgrade product: %v", err)
	}
	if err := e.pool.QueryRow(t.Context(), `
		INSERT INTO price_versions
			(product_id, version, name, currency, amount_minor, interval, anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Medium', 'USD', $2, 'monthly', 1, 'UTC') RETURNING id`,
		productID, amountMinor).Scan(&priceID); err != nil {
		t.Fatalf("seed upgrade price: %v", err)
	}
	if _, err := e.pool.Exec(t.Context(), `
		INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)`,
		fromProduct, productID); err != nil {
		t.Fatalf("seed upgrade edge: %v", err)
	}
	return productID, priceID
}

// seedConfigOption adds an upgradable dropdown to the service's product, prices both choices on
// the subscription's price version, and stores the cheap one as the current selection.
func (e *authEnv) seedConfigOption(t *testing.T, serviceID string) (optionID string) {
	t.Helper()
	productID, priceVersionID := e.planFacts(t, serviceID)
	if err := e.pool.QueryRow(t.Context(), `
		INSERT INTO config_options
			(product_id, name, description, option_type, choices, env_variable, required,
			 hidden, upgradable, sort_order)
		VALUES ($1, 'Memory', 'How much RAM the box has', 'dropdown', '["2 GB","4 GB"]'::jsonb,
		        'MEMORY', false, false, true, 1)
		RETURNING id`, productID).Scan(&optionID); err != nil {
		t.Fatalf("seed config option: %v", err)
	}
	if _, err := e.pool.Exec(t.Context(), `
		INSERT INTO config_option_prices
			(config_option_id, price_version_id, choice, amount_minor, setup_minor)
		VALUES ($1, $2, '2 GB', 0, 0), ($1, $2, '4 GB', 500, 0)`,
		optionID, priceVersionID); err != nil {
		t.Fatalf("seed config option prices: %v", err)
	}
	if _, err := e.pool.Exec(t.Context(), `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
		VALUES ($1, $2, '2 GB', 0, 0)`, serviceID, optionID); err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	return optionID
}

// auditCount counts the audit rows for one action on one service.
func (e *authEnv) auditCount(t *testing.T, action, serviceID string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		action, serviceID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// upgradeBody is the part of an upgrade or config-change response these tests read.
type upgradeBody struct {
	UpgradeID     string `json:"upgrade_id"`
	ServiceID     string `json:"service_id"`
	Direction     string `json:"direction"`
	Currency      string `json:"currency"`
	ProratedMinor int64  `json:"prorated_minor"`
	Status        string `json:"status"`
	InvoiceID     string `json:"invoice_id"`
	InvoiceNumber string `json:"invoice_number"`
	CreditMinor   int64  `json:"credit_minor"`
	CreditPending bool   `json:"credit_pending"`
	Proration     struct {
		RemainingSecs int64  `json:"remaining_secs"`
		TotalSecs     int64  `json:"total_secs"`
		Ratio         string `json:"ratio"`
	} `json:"proration"`
}

func decodeUpgrade(t *testing.T, rec *httptest.ResponseRecorder) upgradeBody {
	t.Helper()
	var body upgradeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("upgrade body: %v\n%s", err, rec.Body)
	}
	return body
}

// A customer sees the plans they may move to, asks for one, and the request reaches the queue an
// operator works from.
//
// The proration is asserted rather than assumed: the fixture period is a whole month, so almost
// all of it remains, and the charge for a 20.00 plan from a 12.00 one is the 8.00 difference —
// stated with the fraction alongside it so the arithmetic can be checked without re-running the
// code.
func TestPortalRequestsAnUpgradeAndItReachesTheStaffQueue(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "upgrade-alice@example.test", "12.00")
	_, targetPrice := env.seedUpgradeTarget(t, tenant.serviceID, 2000)
	cookie, csrf := env.login(t, "upgrade-alice@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/upgrade-options", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("options: status %d; body %s", rec.Code, rec.Body)
	}
	var options struct {
		Options []struct {
			ProductID   string `json:"product_id"`
			ProductName string `json:"product_name"`
			Prices      []struct {
				PriceVersionID string `json:"price_version_id"`
				Amount         struct {
					Minor int64 `json:"minor"`
				} `json:"amount"`
				Prorated struct {
					Minor int64 `json:"minor"`
				} `json:"prorated"`
			} `json:"prices"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &options); err != nil {
		t.Fatalf("options body: %v\n%s", err, rec.Body)
	}
	if len(options.Options) != 1 || len(options.Options[0].Prices) != 1 {
		t.Fatalf("upgrade options = %s", rec.Body)
	}
	if got := options.Options[0].Prices[0]; got.PriceVersionID != targetPrice ||
		got.Amount.Minor != 2000 || got.Prorated.Minor != 800 {
		t.Fatalf("the target price or its proration is wrong: %+v", got)
	}

	rec = env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/upgrade", cookie, csrf,
		freshKey("upgrade"), map[string]any{
			"price_version_id": targetPrice,
			"reason":           "the site outgrew the small box",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upgrade: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeUpgrade(t, rec)
	if body.Direction != "upgrade" || body.ProratedMinor != 800 || body.Currency != "USD" {
		t.Fatalf("upgrade result = %+v", body)
	}
	if body.InvoiceID == "" || body.InvoiceNumber == "" {
		t.Fatalf("an upgrade issued no invoice: %+v", body)
	}
	if body.Proration.TotalSecs <= 0 || body.Proration.Ratio == "" {
		t.Fatalf("the response does not name the proration: %+v", body.Proration)
	}

	// The request is a *request*: the row is pending, and the subscription still renews at the
	// old price until the invoice is paid.
	var status, currentPrice string
	if err := env.pool.QueryRow(t.Context(), `
		SELECT u.status, sub.price_version_id
		FROM service_upgrades u
		JOIN services s ON s.id = u.service_id
		JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE u.id = $1`, body.UpgradeID).Scan(&status, &currentPrice); err != nil {
		t.Fatalf("read upgrade row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("upgrade status = %q, want pending", status)
	}
	if currentPrice == targetPrice {
		t.Fatal("the subscription was re-priced before the invoice was paid")
	}

	// The staff queue is the other half of "the customer can ask": an operator has to be able to
	// see what was asked for, or the request is a row nobody reads.
	rec = env.get(t, "/api/v1/staff/upgrades?paid=false", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff queue: status %d; body %s", rec.Code, rec.Body)
	}
	var queue struct {
		Upgrades []struct {
			ID        string `json:"id"`
			ServiceID string `json:"service_id"`
			Status    string `json:"status"`
			InvoiceID string `json:"invoice_id"`
		} `json:"upgrades"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("queue body: %v\n%s", err, rec.Body)
	}
	found := false
	for _, row := range queue.Upgrades {
		if row.ID == body.UpgradeID {
			found = true
			if row.ServiceID != tenant.serviceID || row.Status != "pending" || row.InvoiceID != body.InvoiceID {
				t.Fatalf("the queued row disagrees with the response: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("the customer's upgrade is not in the staff queue: %s", rec.Body)
	}

	// The audit row is written inside the same transaction as the change, so a change that is
	// visible without its record cannot exist.
	if n := env.auditCount(t, "service.upgrade", tenant.serviceID); n != 1 {
		t.Fatalf("audit rows for service.upgrade = %d, want 1", n)
	}
}

// A service the caller does not own answers 404 on both upgrade reads and writes, and nothing is
// written: the pricing query never sees a foreign id, so there is no partial answer to leak.
func TestPortalUpgradeIsScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.createTenant(t, "upgrade-scope-alice@example.test", "12.00")
	_, targetPrice := env.seedUpgradeTarget(t, alice.serviceID, 2000)
	env.createTenant(t, "upgrade-scope-bob@example.test", "12.00")
	bobCookie, bobCSRF := env.login(t, "upgrade-scope-bob@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+alice.serviceID+"/upgrade-options", bobCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("options as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	rec = env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+alice.serviceID+"/upgrade", bobCookie, bobCSRF,
		freshKey("upgrade-foreign"), map[string]any{"price_version_id": targetPrice})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upgrade as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	var upgrades int
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM service_upgrades WHERE service_id = $1`, alice.serviceID).
		Scan(&upgrades); err != nil {
		t.Fatalf("count upgrades: %v", err)
	}
	if upgrades != 0 {
		t.Fatalf("a refused upgrade wrote %d rows", upgrades)
	}
}

// The change form is priced from the row the charge comes from.
//
// `GET .../config-options` exists for exactly this: the selection read names the current value and
// no alternative's price, so without it a customer would commit to a change before learning what
// it costs. The amounts asserted here are the `config_option_prices` rows the mutation reads.
func TestPortalConfigOptionsArePricedOnTheServicesPlan(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "config-read@example.test", "12.00")
	optionID := env.seedConfigOption(t, tenant.serviceID)
	cookie, _ := env.login(t, "config-read@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/config-options", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("config options: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		ServiceID   string `json:"service_id"`
		Currency    string `json:"currency"`
		CanChange   bool   `json:"can_change"`
		BlockReason string `json:"change_blocked_reason"`
		Options     []struct {
			ConfigOptionID string `json:"config_option_id"`
			Name           string `json:"name"`
			Type           string `json:"type"`
			Upgradable     bool   `json:"upgradable"`
			Value          string `json:"value"`
			Choices        []struct {
				Choice      string `json:"choice"`
				AmountMinor int64  `json:"amount_minor"`
			} `json:"choices"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("config options body: %v\n%s", err, rec.Body)
	}
	if body.ServiceID != tenant.serviceID || body.Currency != "USD" {
		t.Fatalf("config options header = %+v", body)
	}
	// The page offers the change forms on this flag alone, so it must be true for a live service
	// and carry no excuse.
	if !body.CanChange || body.BlockReason != "" {
		t.Fatalf("a live service is not offered its options: can_change=%v reason=%q",
			body.CanChange, body.BlockReason)
	}
	if len(body.Options) != 1 {
		t.Fatalf("options = %s", rec.Body)
	}
	option := body.Options[0]
	if option.ConfigOptionID != optionID || option.Name != "Memory" || option.Type != "dropdown" {
		t.Fatalf("option = %+v", option)
	}
	if !option.Upgradable {
		t.Fatal("the option is not offered as changeable")
	}
	if option.Value != "2 GB" {
		t.Fatalf("current value = %q, want the stored selection", option.Value)
	}
	prices := map[string]int64{}
	for _, c := range option.Choices {
		prices[c.Choice] = c.AmountMinor
	}
	if prices["2 GB"] != 0 || prices["4 GB"] != 500 {
		t.Fatalf("priced choices = %v", prices)
	}
}

// A hidden option is not offered, because an operator attached it to the plan without selling it.
func TestPortalConfigOptionsOmitHiddenOptions(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "config-hidden@example.test", "12.00")
	productID, _ := env.planFacts(t, tenant.serviceID)
	if _, err := env.pool.Exec(t.Context(), `
		INSERT INTO config_options (product_id, name, option_type, choices, hidden, sort_order)
		VALUES ($1, 'Internal note', 'text', '[]'::jsonb, true, 9)`, productID); err != nil {
		t.Fatalf("seed hidden option: %v", err)
	}
	cookie, _ := env.login(t, "config-hidden@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/config-options", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("config options: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	for _, o := range body.Options {
		if o.Name == "Internal note" {
			t.Fatalf("a hidden option was offered: %s", rec.Body)
		}
	}
}

// A customer changes an option and pays the prorated difference, exactly as an upgrade would.
func TestPortalChangesAConfigOption(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "config-change@example.test", "12.00")
	optionID := env.seedConfigOption(t, tenant.serviceID)
	cookie, csrf := env.login(t, "config-change@example.test", "a long enough password")

	rec := env.portalMutation(t, http.MethodPut,
		"/api/v1/portal/services/"+tenant.serviceID+"/config", cookie, csrf,
		freshKey("config"), map[string]any{
			"config_option_id": optionID,
			"value":            "4 GB",
			"reason":           "the database needs the headroom",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("change config: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeUpgrade(t, rec)
	if body.ProratedMinor != 500 || body.Currency != "USD" || body.Direction != "upgrade" {
		t.Fatalf("config change result = %+v", body)
	}
	if body.InvoiceID == "" {
		t.Fatalf("a price increase issued no invoice: %+v", body)
	}

	// The stored selection carries the amount it contributed, which is what the next renewal
	// prices from.
	var value string
	var amountMinor int64
	if err := env.pool.QueryRow(t.Context(), `
		SELECT value, amount_minor FROM service_configs
		WHERE service_id = $1 AND config_option_id = $2`, tenant.serviceID, optionID).
		Scan(&value, &amountMinor); err != nil {
		t.Fatalf("read selection: %v", err)
	}
	if value != "4 GB" || amountMinor != 500 {
		t.Fatalf("stored selection = %q / %d", value, amountMinor)
	}

	if n := env.auditCount(t, "service.config_change", tenant.serviceID); n != 1 {
		t.Fatalf("audit rows for service.config_change = %d, want 1", n)
	}

	// An option the operator marked fixed is refused: `upgradable` is a decision, not a hint.
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE config_options SET upgradable = false WHERE id = $1`, optionID); err != nil {
		t.Fatalf("freeze option: %v", err)
	}
	rec = env.portalMutation(t, http.MethodPut,
		"/api/v1/portal/services/"+tenant.serviceID+"/config", cookie, csrf,
		freshKey("config-frozen"), map[string]any{
			"config_option_id": optionID,
			"value":            "2 GB",
		})
	if rec.Code != http.StatusConflict {
		t.Fatalf("frozen option: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "option_not_upgradable" {
		t.Fatalf("code = %q, want option_not_upgradable", code)
	}
}

// A config change on another customer's service is a 404, on the read and on the write.
func TestPortalConfigChangeIsScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.createTenant(t, "config-scope-alice@example.test", "12.00")
	optionID := env.seedConfigOption(t, alice.serviceID)
	env.createTenant(t, "config-scope-bob@example.test", "12.00")
	bobCookie, bobCSRF := env.login(t, "config-scope-bob@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+alice.serviceID+"/config-options", bobCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	rec = env.portalMutation(t, http.MethodPut,
		"/api/v1/portal/services/"+alice.serviceID+"/config", bobCookie, bobCSRF,
		freshKey("config-foreign"), map[string]any{
			"config_option_id": optionID,
			"value":            "4 GB",
		})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("write as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	var value string
	if err := env.pool.QueryRow(t.Context(), `
		SELECT value FROM service_configs
		WHERE service_id = $1 AND config_option_id = $2`, alice.serviceID, optionID).
		Scan(&value); err != nil {
		t.Fatalf("read selection: %v", err)
	}
	if value != "2 GB" {
		t.Fatalf("a refused change wrote the selection: %q", value)
	}
}

// One idempotency key, one change.
//
// A customer whose connection drops mid-request retries; the retry must not issue a second
// invoice, a second upgrade row or a second audit row. The replay is answered from the stored
// response, so the client sees the same answer it would have seen the first time.
func TestPortalServiceMutationsReplayUnderOneKey(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "idem@example.test", "12.00")
	_, targetPrice := env.seedUpgradeTarget(t, tenant.serviceID, 2000)
	cookie, csrf := env.login(t, "idem@example.test", "a long enough password")

	key := freshKey("idem-upgrade")
	path := "/api/v1/portal/services/" + tenant.serviceID + "/upgrade"
	body := map[string]any{"price_version_id": targetPrice}

	first := env.portalMutation(t, http.MethodPost, path, cookie, csrf, key, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first call: status %d; body %s", first.Code, first.Body)
	}
	second := env.portalMutation(t, http.MethodPost, path, cookie, csrf, key, body)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay: status %d; body %s", second.Code, second.Body)
	}
	// The replay is the stored response, and the storage column is jsonb: the bytes come back
	// with the keys in the database's order, so the assertion is over the values. What must not
	// differ is the identity of the change — a second upgrade id would be a second charge.
	if firstBody, secondBody := decodeUpgrade(t, first), decodeUpgrade(t, second); firstBody.UpgradeID != secondBody.UpgradeID ||
		firstBody.InvoiceID != secondBody.InvoiceID ||
		firstBody.ProratedMinor != secondBody.ProratedMinor {
		t.Fatalf("the replay answered a different change:\n%+v\n%+v", firstBody, secondBody)
	}
	if got := second.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Fatalf("Idempotent-Replayed = %q, want true", got)
	}

	var upgrades, invoices int
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM service_upgrades WHERE service_id = $1`, tenant.serviceID).
		Scan(&upgrades); err != nil {
		t.Fatalf("count upgrades: %v", err)
	}
	if upgrades != 1 {
		t.Fatalf("a replayed upgrade wrote %d rows", upgrades)
	}
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, tenant.customerID).
		Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	// The fixture's own invoice plus the one the upgrade issued.
	if invoices != 2 {
		t.Fatalf("invoices = %d, want the fixture's plus one prorated charge", invoices)
	}
	if n := env.auditCount(t, "service.upgrade", tenant.serviceID); n != 1 {
		t.Fatalf("audit rows for service.upgrade = %d, want 1", n)
	}
}

// A service the server would refuse to re-price says so on the options read, instead of offering a
// form that comes back 409.
//
// The page renders the plan and option forms on `can_change` alone, so this is the field that keeps
// the promise the screen makes. A service with no billing schedule is the case that matters: it is
// a real state (a service provisioned from a manual invoice), and the upgrade handler refuses it.
func TestPortalConfigOptionsSayWhenAChangeIsNotPossible(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "config-noschedule@example.test", "12.00")
	cookie, _ := env.login(t, "config-noschedule@example.test", "a long enough password")

	// Detach the schedule: a service sold from a manual invoice has no subscription.
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE services SET subscription_id = NULL WHERE id = $1`, tenant.serviceID); err != nil {
		t.Fatalf("detach schedule: %v", err)
	}

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/config-options", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("config options: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		CanChange   bool   `json:"can_change"`
		BlockReason string `json:"change_blocked_reason"`
		Options     []any  `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.CanChange {
		t.Fatal("a service with no billing schedule was offered a re-price")
	}
	if !strings.Contains(body.BlockReason, "billing schedule") {
		t.Fatalf("the refusal does not name the cause: %q", body.BlockReason)
	}
	if body.Options == nil {
		t.Fatal("options is null rather than an empty array; a client cannot iterate it")
	}
}
