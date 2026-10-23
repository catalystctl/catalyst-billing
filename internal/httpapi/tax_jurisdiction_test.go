package httpapi_test

// The tax-jurisdiction surface, end to end through the router.
//
// These tests assert the three things an operator and a storefront need and a rate table cannot
// give them: a reverse charge on a validated VAT ID, a refusal (not a guess) when the identifier
// could not be checked, and the exact sentence a price is displayed with.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/tax"
	"github.com/jackc/pgx/v5"
)

// resetJurisdictionSettings restores the keys these tests write, both on the way in and on the way
// out: `settings` is reference data the harness never truncates, so a test that switches reverse
// charge on must put it back for the rest of the package.
func resetJurisdictionSettings(t *testing.T, pool *store.Pool) {
	t.Helper()
	clear := func() {
		for _, key := range []string{"enabled", "seller_country", "reverse_charge",
			"vat_validation_url", "vat_validation_ttl_hours", "nexus_enforced"} {
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM settings WHERE group_key = 'tax' AND key = $1`, key); err != nil {
				t.Fatalf("reset tax.%s: %v", key, err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
}

func setTaxSetting(t *testing.T, pool *store.Pool, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('tax', $1, $2)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set tax.%s: %v", key, err)
	}
}

// fakeVIESEndpoint serves the SOAP checkVat answer the validator expects.
func fakeVIESEndpoint(t *testing.T) (url string, calls *int) {
	t.Helper()
	var (
		mu sync.Mutex
		n  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><env:Envelope xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"><env:Body><checkVatResponse><countryCode>FR</countryCode><vatNumber>40303265045</vatNumber><valid>true</valid><name>Example SARL</name></checkVatResponse></env:Body></env:Envelope>`)
	}))
	t.Cleanup(server.Close)
	return server.URL, &n
}

// addGermanAndFrenchRates creates the two rates the tests resolve against.
func addGermanAndFrenchRates(t *testing.T, env *staffEnv) {
	t.Helper()
	for _, body := range []map[string]any{
		{"name": "German VAT", "country_code": "DE", "rate": "0.19", "inclusive": true},
		{"name": "French VAT", "country_code": "FR", "rate": "0.2"},
	} {
		rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates",
			env.billing, env.billingCSRF, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create rate %v: %d %s", body, rec.Code, rec.Body)
		}
	}
}

// The disclosure is public and renders the sentence from the exact rational, including the
// difference between a rate inside the price and one added to it.
func TestTaxDisclosureIsPublicAndExact(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	addGermanAndFrenchRates(t, env)
	setTaxSetting(t, env.pool, "enabled", "true")
	setTaxSetting(t, env.pool, "seller_country", "DE")

	rec := env.get(t, "/api/v1/tax/disclosure?country=DE", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disclosure: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		TaxEnabled bool   `json:"tax_enabled"`
		Label      string `json:"label"`
		Decision   struct {
			Kind      string `json:"kind"`
			Rate      string `json:"rate"`
			Inclusive bool   `json:"inclusive"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.TaxEnabled {
		t.Fatal("tax is enabled but the disclosure says it is off")
	}
	if body.Label != "incl. 19% VAT" {
		t.Fatalf("label = %q, want incl. 19%% VAT", body.Label)
	}
	// The rate is the exact rational string, never a float.
	if body.Decision.Rate != "0.19" && body.Decision.Rate != "19/100" {
		t.Fatalf("rate = %q, want the exact rational", body.Decision.Rate)
	}

	// France is exclusive: the sentence says the tax is added.
	rec = env.get(t, "/api/v1/tax/disclosure?country=FR", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Label != "plus 20% VAT" {
		t.Fatalf("label = %q, want plus 20%% VAT", body.Label)
	}

	// A destination with no rate says so rather than inventing a zero.
	rec = env.get(t, "/api/v1/tax/disclosure?country=JP", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Label != "tax is calculated at checkout" {
		t.Fatalf("label for an unconfigured destination = %q", body.Label)
	}
}

// The cart charges nothing for a cross-border EU supply with a validated VAT ID, and the evidence
// names the reverse charge. This is the rule a rate table cannot express.
func TestCartAppliesReverseChargeForAValidatedVATID(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	addGermanAndFrenchRates(t, env)
	viesURL, calls := fakeVIESEndpoint(t)
	setTaxSetting(t, env.pool, "enabled", "true")
	setTaxSetting(t, env.pool, "seller_country", "DE")
	setTaxSetting(t, env.pool, "reverse_charge", "true")
	setTaxSetting(t, env.pool, "vat_validation_url", viesURL)

	env.srv.WithPortal(portal.New(env.pool))
	customerID, _ := env.seedCommerceCustomer(t, "reverse-charge@example.test")
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO billing_profiles (customer_id, country_code, tax_id)
		VALUES ($1, 'FR', 'FR40303265045')`, customerID); err != nil {
		t.Fatalf("billing profile: %v", err)
	}
	cookie, csrf := env.login(t, "reverse-charge@example.test", "customer password 1")

	_, priceID := env.seedCartPrice(t, "reverse-charge-vps", 10000, 0, "combined")
	if rec := env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/items", cookie, csrf,
		map[string]any{"price_version_id": priceID, "quantity": 1}); rec.Code != http.StatusCreated {
		t.Fatalf("add item: %d %s", rec.Code, rec.Body)
	}

	rec := env.get(t, "/api/v1/portal/cart/totals", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("totals: %d %s", rec.Code, rec.Body)
	}
	var totals struct {
		Tax         struct{ Minor int64 } `json:"tax"`
		Total       struct{ Minor int64 } `json:"total"`
		TaxEvidence *struct {
			Rate            string `json:"rate"`
			ExemptionReason string `json:"exemption_reason"`
		} `json:"tax_evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &totals); err != nil {
		t.Fatal(err)
	}
	if totals.Tax.Minor != 0 {
		t.Fatalf("tax = %d, want 0 under reverse charge (body %s)", totals.Tax.Minor, rec.Body)
	}
	if totals.TaxEvidence == nil || !strings.Contains(totals.TaxEvidence.ExemptionReason, "reverse charge") {
		t.Fatalf("tax evidence = %+v, want a reverse-charge reason", totals.TaxEvidence)
	}
	if *calls != 1 {
		t.Fatalf("the validation service was called %d times, want 1", *calls)
	}
}

// When the validation service cannot answer, the cart refuses with a stable code and the fact that
// is missing. It must not charge the French rate to a business entitled to reverse charge, and it
// must not zero the tax either.
func TestCartRefusesWhenTheVATIDCannotBeValidated(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	addGermanAndFrenchRates(t, env)
	// A service that is reachable but answers nothing usable: the honest summary is "no answer".
	faulty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<?xml version="1.0"?><env:Envelope xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"><env:Body><env:Fault><faultcode>env:Server</faultcode><faultstring>MS_UNAVAILABLE</faultstring></env:Fault></env:Body></env:Envelope>`)
	}))
	t.Cleanup(faulty.Close)
	setTaxSetting(t, env.pool, "enabled", "true")
	setTaxSetting(t, env.pool, "seller_country", "DE")
	setTaxSetting(t, env.pool, "reverse_charge", "true")
	setTaxSetting(t, env.pool, "vat_validation_url", faulty.URL)

	env.srv.WithPortal(portal.New(env.pool))
	customerID, _ := env.seedCommerceCustomer(t, "unverifiable@example.test")
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO billing_profiles (customer_id, country_code, tax_id)
		VALUES ($1, 'FR', 'FR40303265045')`, customerID); err != nil {
		t.Fatalf("billing profile: %v", err)
	}
	cookie, _ := env.login(t, "unverifiable@example.test", "customer password 1")

	// One call, because a second render would read the recorded "unavailable" answer from the
	// cache: the code must be the same either way, and this asserts the first attempt's.
	rec := env.get(t, "/api/v1/portal/cart/totals", cookie)
	if code := commerceProblem(t, rec, http.StatusUnprocessableEntity); code != "vat_id_validation_failed" {
		t.Fatalf("code = %q, want vat_id_validation_failed", code)
	}
}

// The staff rules screen: nexus CRUD is permissioned, conflict-aware, audited and reflected in the
// decision preview.
func TestTaxJurisdictionRoutes(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)

	rec := env.get(t, "/api/v1/staff/tax-jurisdiction", env.support)
	if code := commerceProblem(t, rec, http.StatusForbidden); code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", code)
	}

	rec = env.get(t, "/api/v1/staff/tax-jurisdiction", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	var status struct {
		Config struct {
			SellerCountry string `json:"seller_country"`
			ReverseCharge bool   `json:"reverse_charge"`
			NexusEnforced bool   `json:"nexus_enforced"`
		} `json:"config"`
		Nexus          []struct{ Jurisdiction string } `json:"nexus"`
		EUMemberStates []string                        `json:"eu_member_states"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.EUMemberStates) != 27 {
		t.Fatalf("EU member states = %d, want 27", len(status.EUMemberStates))
	}
	if status.Config.ReverseCharge || status.Config.NexusEnforced {
		t.Fatal("the defaults must be off: a new setting must not change what an installation charges")
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-jurisdiction/nexus",
		env.billing, env.billingCSRF, map[string]any{
			"country_code": "US", "region_code": "CA", "note": "warehouse in Reno",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare nexus: %d %s", rec.Code, rec.Body)
	}
	var declared struct {
		ID           string `json:"id"`
		Jurisdiction string `json:"jurisdiction"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &declared); err != nil {
		t.Fatal(err)
	}
	if declared.Jurisdiction != "US-CA" {
		t.Fatalf("jurisdiction = %q, want US-CA", declared.Jurisdiction)
	}

	// A duplicate declaration is a conflict, not a second row.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-jurisdiction/nexus",
		env.billing, env.billingCSRF, map[string]any{"country_code": "US", "region_code": "CA"})
	if code := commerceProblem(t, rec, http.StatusConflict); code != "nexus_exists" {
		t.Fatalf("code = %q, want nexus_exists", code)
	}

	// The declaration is audited in the same transaction, so the trail cannot be missing for a
	// committed assertion about where tax is collected.
	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'tax_jurisdiction.nexus_declare'`).Scan(&audited); err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	if audited != 1 {
		t.Fatalf("audit rows = %d, want 1", audited)
	}

	// The decision preview explains a hypothetical sale: no tax where no nexus is declared once the
	// country is nexus-governed.
	setTaxSetting(t, env.pool, "enabled", "true")
	setTaxSetting(t, env.pool, "nexus_enforced", "true")
	addGermanAndFrenchRates(t, env)
	if rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-rates", env.billing, env.billingCSRF,
		map[string]any{"name": "New York", "country_code": "US", "region_code": "NY", "rate": "0.08"}); rec.Code != http.StatusCreated {
		t.Fatalf("create NY rate: %d %s", rec.Code, rec.Body)
	}
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-jurisdiction/decide",
		env.billing, env.billingCSRF, map[string]any{"country": "US", "region": "NY"})
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	var explained struct {
		Outcome  string `json:"outcome"`
		Decision struct {
			Kind    string `json:"kind"`
			Charges bool   `json:"charges"`
			Reason  string `json:"reason"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &explained); err != nil {
		t.Fatal(err)
	}
	if explained.Decision.Kind != "no_nexus" || explained.Decision.Charges {
		t.Fatalf("decision = %+v, want no_nexus without a charge", explained.Decision)
	}
	if !strings.Contains(explained.Decision.Reason, "US-NY") {
		t.Fatalf("reason = %q, want it to name the jurisdiction", explained.Decision.Reason)
	}

	// Retiring the declaration removes it.
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/tax-jurisdiction/nexus/"+declared.ID,
		env.billing, env.billingCSRF, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("retire nexus: %d %s", rec.Code, rec.Body)
	}
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/tax-jurisdiction/nexus/"+declared.ID,
		env.billing, env.billingCSRF, nil)
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// A malformed VAT ID is refused before a service is asked, and the refusal names the format.
func TestValidateVATRefusesAMalformedIdentifier(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	setTaxSetting(t, env.pool, "vat_validation_url", "http://127.0.0.1:1/never-called")

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-jurisdiction/validate-vat",
		env.billing, env.billingCSRF, map[string]any{"country": "DE", "vat_id": "DE12"})
	if code := commerceProblem(t, rec, http.StatusUnprocessableEntity); code != "vat_id_malformed" {
		t.Fatalf("code = %q, want vat_id_malformed", code)
	}
}

// Time keeps these tests honest about the harness clock: the rate rows are dated with the real
// clock, and a pinned instant would put them in the future.
var _ = time.Now

// The resolving issuer: `Server.IssuerFor` prices an invoice at the customer's own jurisdiction.
//
// This is TAX-02's server half. The shipped server still builds its issuer with the no-tax pricer
// (server.go is not this change's to edit), so this test is what proves the replacement is correct
// before it is placed: the same issuer, given a German customer, records the German rate, evidence
// and tax on the document.
func TestIssuerForPricesAtTheCustomersJurisdiction(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	addGermanAndFrenchRates(t, env)
	setTaxSetting(t, env.pool, "enabled", "true")

	customerID, _ := env.seedCommerceCustomer(t, "german-issuer@example.test")
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO billing_profiles (customer_id, country_code) VALUES ($1, 'DE')`, customerID); err != nil {
		t.Fatalf("billing profile: %v", err)
	}

	var (
		taxMinor int64
		snapJSON []byte
	)
	err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		issuer, err := env.srv.IssuerFor(ctx, customerID)
		if err != nil {
			return err
		}
		id, totals, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, money.MustLookupCurrency("EUR")),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: customerID},
			DueAt:    time.Now().UTC().AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		taxMinor = totals.Tax.Minor()
		return tx.QueryRow(ctx, `SELECT snapshot FROM invoices WHERE id = $1`, id).Scan(&snapJSON)
	})
	if err != nil {
		t.Fatalf("issue through the resolving issuer: %v", err)
	}
	// The German rate is inclusive 19%: tax is the share of the 10000 gross, 1597.
	if taxMinor != 1597 {
		t.Fatalf("tax = %d, want 1597 (the German inclusive rate)", taxMinor)
	}
	var snap struct {
		TaxEvidence struct {
			Policy          string `json:"policy"`
			Jurisdiction    string `json:"jurisdiction"`
			Rate            string `json:"rate"`
			ExemptionReason string `json:"exemption_reason"`
		} `json:"tax_evidence"`
	}
	if err := json.Unmarshal(snapJSON, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.TaxEvidence.Jurisdiction != "DE/standard" {
		t.Fatalf("evidence jurisdiction = %q, want DE/standard", snap.TaxEvidence.Jurisdiction)
	}
	if snap.TaxEvidence.Rate != "0.19" && snap.TaxEvidence.Rate != "19/100" {
		t.Fatalf("evidence rate = %q", snap.TaxEvidence.Rate)
	}

	// With tax switched off the issuer charges nothing and records the shipped no-tax policy: the
	// default behaviour an installation without rates must keep.
	setTaxSetting(t, env.pool, "enabled", "false")
	err = env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		issuer, err := env.srv.IssuerFor(ctx, customerID)
		if err != nil {
			return err
		}
		_, totals, snap, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, money.MustLookupCurrency("EUR")),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: customerID},
			DueAt:    time.Now().UTC().AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		if totals.Tax.Minor() != 0 {
			t.Fatalf("tax with the switch off = %d, want 0", totals.Tax.Minor())
		}
		if snap.TaxEvidence.Policy != "no-tax" {
			t.Fatalf("policy with the switch off = %q, want no-tax", snap.TaxEvidence.Policy)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("issue with tax off: %v", err)
	}
}

// A refusal refuses the invoice too, not just the cart: `IssuerFor` returns the *tax.Refusal and no
// issuer, so there is no path from "the rules could not decide" to a document charged at zero.
//
// The failure this guards against is the expensive one: silently charging 0% VAT because a VAT ID
// could not be checked turns a bug into a tax liability, and an issuer that fell back to
// FixedTaxPricer would do exactly that.
func TestIssuerForRefusesAnUnverifiableSupply(t *testing.T) {
	env := newStaffEnv(t)
	resetJurisdictionSettings(t, env.pool)
	addGermanAndFrenchRates(t, env)
	setTaxSetting(t, env.pool, "enabled", "true")
	setTaxSetting(t, env.pool, "seller_country", "DE")
	setTaxSetting(t, env.pool, "reverse_charge", "true")
	// No validation service: the identifier cannot be checked, so reverse charge cannot be granted
	// and the destination rate must not be charged either.
	setTaxSetting(t, env.pool, "vat_validation_url", "")

	customerID, _ := env.seedCommerceCustomer(t, "unverifiable-issuer@example.test")
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO billing_profiles (customer_id, country_code, tax_id)
		VALUES ($1, 'FR', 'FR40303265045')`, customerID); err != nil {
		t.Fatalf("billing profile: %v", err)
	}

	issuer, err := env.srv.IssuerFor(context.Background(), customerID)
	var refusal *tax.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *tax.Refusal", err)
	}
	if issuer != nil {
		t.Fatal("an issuer was returned for a refused supply, which invites a zero-tax document")
	}
	if refusal.Code != tax.CodeVATIDUnverified {
		t.Fatalf("code = %q, want %s", refusal.Code, tax.CodeVATIDUnverified)
	}
	if !errors.Is(err, tax.ErrRefused) {
		t.Fatal("the refusal must be recognisable with errors.Is(err, tax.ErrRefused)")
	}
	if len(refusal.Missing) == 0 || !strings.Contains(strings.Join(refusal.Missing, ","), "vat_validation_url") {
		t.Fatalf("missing = %v, want it to name tax.vat_validation_url", refusal.Missing)
	}

	// And nothing was written: a refusal creates no invoice and no draft.
	var invoices int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d, want 0", invoices)
	}
}
