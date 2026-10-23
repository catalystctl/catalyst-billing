package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Choosing a plan *and* its options as one change (UPG-02).
//
// The arithmetic is proven in internal/service's plan-change tests. What is pinned here is the
// customer-facing contract the portal screen depends on: the target plan's options are priced on
// that plan's own price version, the change is one request with one prorated amount, another
// customer's service is a 404, and a plan the server did not offer is a 404 rather than a pricing
// query with an arbitrary id in it.

// targetOption is the shape of one option in a plan-options response.
type targetOption struct {
	ConfigOptionID string `json:"config_option_id"`
	Name           string `json:"name"`
	Value          string `json:"value"`
	Upgradable     bool   `json:"upgradable"`
	Choices        []struct {
		Choice      string `json:"choice"`
		AmountMinor int64  `json:"amount_minor"`
	} `json:"choices"`
}

// seedPlanOption attaches an option to a product and prices it on one price version.
func (e *authEnv) seedPlanOption(t *testing.T, productID, priceVersionID, name string,
	choices map[string]int64) string {
	t.Helper()
	var optionID string
	encoded, err := json.Marshal(orderedChoices(choices))
	if err != nil {
		t.Fatalf("encode choices: %v", err)
	}
	if err := e.pool.QueryRow(t.Context(), `
		INSERT INTO config_options (product_id, name, option_type, choices, sort_order)
		VALUES ($1, $2, 'dropdown', $3::jsonb, 1) RETURNING id`,
		productID, name, string(encoded)).Scan(&optionID); err != nil {
		t.Fatalf("config option: %v", err)
	}
	for choice, amount := range choices {
		if _, err := e.pool.Exec(t.Context(), `
			INSERT INTO config_option_prices
				(config_option_id, price_version_id, choice, amount_minor)
			VALUES ($1, $2, $3, $4)`, optionID, priceVersionID, choice, amount); err != nil {
			t.Fatalf("config option price: %v", err)
		}
	}
	return optionID
}

// orderedChoices keeps the JSON array stable so a test failure is readable.
func orderedChoices(in map[string]int64) []string {
	out := make([]string, 0, len(in))
	for choice := range in {
		out = append(out, choice)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// The target plan's options are listed with the amounts that plan charges, and the current plan's
// option is not offered as if it belonged to the new plan.
func TestPortalPlanOptionsPricesTheTargetPlan(t *testing.T) {
	// The target product's option must be priced on the target price version, which is what makes
	// "8 GB costs 8.00 on this plan" true rather than an average of the catalogue.
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "plan-options@example.test", "10.00")
	targetProduct, targetPrice := env.seedUpgradeTarget(t, tenant.serviceID, 2000)
	option := env.seedPlanOption(t, targetProduct, targetPrice, "Memory",
		map[string]int64{"2 GB": 0, "4 GB": 800})
	cookie, _ := env.login(t, "plan-options@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+
		"/plan-options?price_version_id="+targetPrice, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan options: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		ServiceID      string         `json:"service_id"`
		PriceVersionID string         `json:"price_version_id"`
		Options        []targetOption `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("plan options body: %v\n%s", err, rec.Body)
	}
	if len(body.Options) != 1 || body.Options[0].ConfigOptionID != option {
		t.Fatalf("options = %s", rec.Body)
	}
	var fourGB int64 = -1
	for _, choice := range body.Options[0].Choices {
		if choice.Choice == "4 GB" {
			fourGB = choice.AmountMinor
		}
	}
	if fourGB != 800 {
		t.Fatalf("4 GB on the target plan = %d, want 800 (the target version's price)", fourGB)
	}

	// A plan the service cannot move to is a 404: the target list is the server's answer, and a
	// price id outside it must not be priced.
	_, ownPrice := env.planFacts(t, tenant.serviceID)
	if rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+
		"/plan-options?price_version_id="+ownPrice, cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("the service's own plan: status %d, want 404; body %s", rec.Code, rec.Body)
	}
}

// The customer changes plan and options in one request and gets one prorated amount covering
// both, which is the whole point of the route.
func TestPortalChangesPlanAndOptionsInOneRequest(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "plan-change@example.test", "10.00")
	// The customer already pays for an option on the old plan.
	oldOption := env.seedConfigOption(t, tenant.serviceID)

	targetProduct, targetPrice := env.seedUpgradeTarget(t, tenant.serviceID, 2000)
	newOption := env.seedPlanOption(t, targetProduct, targetPrice, "Memory",
		map[string]int64{"2 GB": 0, "4 GB": 800})
	cookie, csrf := env.login(t, "plan-change@example.test", "a long enough password")

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/change-plan", cookie, csrf,
		freshKey("plan-change"), map[string]any{
			"price_version_id": targetPrice,
			"config_options": []map[string]string{
				{"config_option_id": newOption, "value": "4 GB"},
			},
			"reason": "needs more memory",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("change plan: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeUpgrade(t, rec)

	// (2000 + 800) - (1000 + 0) for the whole remaining period; the fixture is a full month.
	if body.ProratedMinor != 1800 {
		t.Fatalf("prorated = %d, want 1800 (the plan and the option together)", body.ProratedMinor)
	}
	if body.Direction != "upgrade" || body.InvoiceID == "" {
		t.Fatalf("result = %+v, want an upgrade with an invoice", body)
	}
	var total int64
	if err := env.pool.QueryRow(t.Context(),
		`SELECT total_minor FROM invoices WHERE id = $1`, body.InvoiceID).Scan(&total); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if total != 1800 {
		t.Fatalf("invoice total = %d, want 1800", total)
	}

	// The chosen options wait on the upgrade row and the live selection is untouched until the
	// invoice is paid — the service is still running the old plan (and its old option).
	var pending int
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM service_upgrade_configs WHERE upgrade_id = $1`, body.UpgradeID).
		Scan(&pending); err != nil {
		t.Fatalf("count chosen options: %v", err)
	}
	if pending != 1 {
		t.Fatalf("chosen options = %d, want 1", pending)
	}
	var liveOption string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT config_option_id::text FROM service_configs WHERE service_id = $1`,
		tenant.serviceID).Scan(&liveOption); err != nil {
		t.Fatalf("read live selection: %v", err)
	}
	if liveOption != oldOption {
		t.Fatalf("the live selection moved to %s before the invoice was paid", liveOption)
	}
}

// Another customer's service is a 404 on both the read and the write, and nothing is written.
func TestPortalPlanChangeIsScopedToTheCustomer(t *testing.T) {
	env := newStaffEnv(t)
	alice := env.createTenant(t, "plan-scope-alice@example.test", "10.00")
	_, targetPrice := env.seedUpgradeTarget(t, alice.serviceID, 2000)
	env.createTenant(t, "plan-scope-bob@example.test", "10.00")
	bobCookie, bobCSRF := env.login(t, "plan-scope-bob@example.test", "a long enough password")

	if rec := env.get(t, "/api/v1/portal/services/"+alice.serviceID+
		"/plan-options?price_version_id="+targetPrice, bobCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("plan options as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+alice.serviceID+"/change-plan", bobCookie, bobCSRF,
		freshKey("plan-scope"), map[string]any{"price_version_id": targetPrice})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("change plan as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	var upgrades int
	if err := env.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM service_upgrades WHERE service_id = $1`, alice.serviceID).
		Scan(&upgrades); err != nil {
		t.Fatalf("count upgrades: %v", err)
	}
	if upgrades != 0 {
		t.Fatalf("%d upgrade rows written for a foreign request", upgrades)
	}
}

// A missing or malformed target is a problem document with a stable code, not a panic or a
// silently ignored field.
func TestPortalPlanChangeRequiresATargetPlan(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "plan-invalid@example.test", "10.00")
	cookie, csrf := env.login(t, "plan-invalid@example.test", "a long enough password")

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/change-plan", cookie, csrf,
		freshKey("plan-invalid"), map[string]any{"reason": "no plan named"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_price_version" {
		t.Fatalf("problem code = %q, want invalid_price_version", code)
	}
}
