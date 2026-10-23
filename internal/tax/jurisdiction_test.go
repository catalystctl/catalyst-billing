package tax_test

// The jurisdiction rules, against a real database and a fake validation service.
//
// Every test here is written so that removing the behaviour it names makes it fail: a rate table
// alone cannot pass `TestReverseChargeRequiresAValidatedVATID`, and an engine that guesses passes
// neither that nor `TestUnvalidatedVATIDRefusesTheSupply`.
//
// The fake validation service is an `httptest` server speaking the VIES SOAP shape, not a stubbed
// interface: the acceptance criterion is that a VAT ID is checked *against a service*, so the test
// asserts what the service received and what happens when it answers badly.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/tax"
)

// ---------------------------------------------------------------- fixtures --

var rulesNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type rulesEnv struct {
	pool   *store.Pool
	rules  *tax.Rules
	store  *commerce.Store
	client string
}

func newRulesEnv(t *testing.T) *rulesEnv {
	t.Helper()
	pool := storetest.DB(t)
	resetJurisdictionSettings(t, pool)
	store := commerce.New(pool)
	store.Now = func() time.Time { return rulesNow }
	rules := tax.NewRules(pool)
	rules.Now = func() time.Time { return rulesNow }
	env := &rulesEnv{pool: pool, rules: rules, store: store}
	// The code-side default for tax.vat_validation_url is the real VIES endpoint. Deleting the row
	// would therefore re-enable it, and a test that forgot to wire its fake would make an outbound
	// call — a suite that needs egress fails on a plane, and worse, it silently validates against
	// production. An explicit empty value keeps the default off in tests; every test that wants a
	// validation wires its own httptest server.
	env.set(t, "vat_validation_url", "")
	return env
}

// resetJurisdictionSettings restores the five keys this package writes. `settings` is reference
// data the harness never truncates (it is configuration, not state a test creates), so a test that
// turns reverse charge on must put it back or the next test inherits it.
func resetJurisdictionSettings(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, name := range [][2]string{
		{"tax", "enabled"},
		{"tax", "seller_country"},
		{"tax", "reverse_charge"},
		{"tax", "vat_validation_url"},
		{"tax", "vat_validation_ttl_hours"},
		{"tax", "nexus_enforced"},
	} {
		if _, err := pool.Exec(ctx,
			`DELETE FROM settings WHERE group_key = $1 AND key = $2`, name[0], name[1]); err != nil {
			t.Fatalf("reset setting %s.%s: %v", name[0], name[1], err)
		}
	}
}

func (e *rulesEnv) set(t *testing.T, key, value string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('tax', $1, $2)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set tax.%s: %v", key, err)
	}
}

// rate inserts one open rate, with the store's clock so the row is in effect for the tests.
func (e *rulesEnv) rate(t *testing.T, country, region, class, rate string, inclusive bool) {
	t.Helper()
	from := rulesNow.Add(-24 * time.Hour)
	_, err := e.store.CreateTaxRate(context.Background(), commerce.TaxRateInput{
		Name:          "Test " + country + region,
		CountryCode:   country,
		RegionCode:    region,
		TaxClass:      class,
		Rate:          rate,
		Inclusive:     &inclusive,
		EffectiveFrom: &from,
	})
	if err != nil {
		t.Fatalf("create rate %s/%s/%s: %v", country, region, class, err)
	}
}

// customer creates a customer with a billing profile, returning the id.
func (e *rulesEnv) customer(t *testing.T, country, region, vatID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO customers (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("jurisdiction-%d@example.test", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO billing_profiles (customer_id, country_code, region, tax_id)
		VALUES ($1, $2, $3, $4)`, id, country, region, vatID); err != nil {
		t.Fatalf("create billing profile: %v", err)
	}
	return id
}

// fakeVIES answers the SOAP checkVat request with a fixed verdict, and counts the calls.
type fakeVIES struct {
	mu      sync.Mutex
	calls   int
	valid   bool
	name    string
	fault   string
	status  int
	lastReq string
	server  *httptest.Server
}

func newFakeVIES(t *testing.T) *fakeVIES {
	t.Helper()
	fake := &fakeVIES{valid: true, name: "Example GmbH", status: http.StatusOK}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(strings.Builder)
		if _, err := copyBody(body, r); err != nil {
			t.Logf("read request: %v", err)
		}
		fake.mu.Lock()
		fake.calls++
		fake.lastReq = body.String()
		valid, name, fault, status := fake.valid, fake.name, fake.fault, fake.status
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.WriteHeader(status)
		if fault != "" {
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><env:Envelope xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"><env:Body><env:Fault><faultcode>env:Server</faultcode><faultstring>%s</faultstring></env:Fault></env:Body></env:Envelope>`, fault)
			return
		}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><env:Envelope xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"><env:Body><checkVatResponse xmlns="urn:ec.europa.eu:taxud:vies:services:checkVat:types"><countryCode>%s</countryCode><vatNumber>%s</vatNumber><valid>%t</valid><name>%s</name></checkVatResponse></env:Body></env:Envelope>`,
			countryFromRequest(body.String()), numberFromRequest(body.String()), valid, name)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func copyBody(dst *strings.Builder, r *http.Request) (int64, error) {
	buf := make([]byte, 8192)
	var n int64
	for {
		read, err := r.Body.Read(buf)
		dst.Write(buf[:read])
		n += int64(read)
		if err != nil {
			if err.Error() == "EOF" {
				return n, nil
			}
			return n, err
		}
	}
}

func countryFromRequest(body string) string {
	start := strings.Index(body, "<urn:countryCode>")
	if start < 0 {
		return ""
	}
	rest := body[start+len("<urn:countryCode>"):]
	return rest[:strings.Index(rest, "<")]
}

func numberFromRequest(body string) string {
	start := strings.Index(body, "<urn:vatNumber>")
	if start < 0 {
		return ""
	}
	rest := body[start+len("<urn:vatNumber>"):]
	return rest[:strings.Index(rest, "<")]
}

func (f *fakeVIES) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// wire points the installation at the fake service.
func (e *rulesEnv) wire(t *testing.T, fake *fakeVIES) {
	t.Helper()
	e.set(t, "vat_validation_url", fake.server.URL)
	e.rules.Validator = nil
}

// -------------------------------------------------------------- pure rules --

// The shape check is what refuses an identifier before a network call, and the destination check is
// what stops a valid foreign identifier from supporting a domestic reverse charge.
func TestVATIDShapeIsCheckedAgainstTheDestination(t *testing.T) {
	cases := []struct {
		name    string
		country string
		raw     string
		number  string
		ok      bool
	}{
		{"german number with prefix", "DE", "DE 123 456 789", "123456789", true},
		{"german number without prefix", "DE", "123456789", "123456789", true},
		{"french number supplied for a german supply", "DE", "FR40303265045", "40303265045", false},
		{"german number of the wrong length", "DE", "DE12345", "12345", false},
		{"greek identifier with the EL prefix", "GR", "EL123456789", "123456789", true},
		{"dutch number", "NL", "NL123456789B01", "123456789B01", true},
		{"empty", "DE", "  ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			number, ok := tax.NormalizeVATID(tc.country, tc.raw)
			if number != tc.number || ok != tc.ok {
				t.Fatalf("NormalizeVATID(%q, %q) = (%q, %v), want (%q, %v)",
					tc.country, tc.raw, number, ok, tc.number, tc.ok)
			}
		})
	}
	if !tax.IsEU("de") {
		t.Fatal("IsEU must be case-insensitive; a lower-case code from a client is the same country")
	}
	if tax.IsEU("US") {
		t.Fatal("the United States is not an EU member state")
	}
}

// ------------------------------------------------------------ reverse charge --

// The whole point of TAX-05: a validated VAT ID on a cross-border EU supply is charged nothing, and
// the evidence says why. A rate table alone charges 20%.
func TestReverseChargeRequiresAValidatedVATID(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	env.wire(t, fake)
	env.set(t, "enabled", "true")
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	customerID := env.customer(t, "FR", "", "FR40303265045")
	decision, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != tax.DecisionReverseCharge {
		t.Fatalf("decision = %q, want reverse_charge", decision.Kind)
	}
	if decision.Rate != "" {
		t.Fatalf("rate = %q; a reverse charge is the absence of a rate, not a zero rate", decision.Rate)
	}
	if !strings.Contains(decision.Reason, "FR40303265045") {
		t.Fatalf("reason does not name the validated identifier: %q", decision.Reason)
	}
	if decision.Validation == nil || decision.Validation.Status != tax.VATStatusValid {
		t.Fatalf("validation = %+v, want a valid answer", decision.Validation)
	}
	if fake.count() != 1 {
		t.Fatalf("the validation service was called %d times, want once", fake.count())
	}
	if !strings.Contains(fake.lastReq, "<urn:countryCode>FR</urn:countryCode>") {
		t.Fatalf("the service was asked about the wrong country: %s", fake.lastReq)
	}

	// And the pricer charges nothing on a real amount, with the reason recorded.
	pricer, err := env.rules.PricerForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("pricer: %v", err)
	}
	cur := money.MustLookupCurrency("EUR")
	_, totals, evidence, err := pricer.Price(context.Background(),
		[]billing.Line{{Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur)}}, cur)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Tax.Minor() != 0 || totals.Total.Minor() != 10000 {
		t.Fatalf("totals = tax %d total %d, want 0 tax on 10000", totals.Tax.Minor(), totals.Total.Minor())
	}
	if evidence.Policy != tax.PolicyReverseCharge || evidence.ExemptionReason == "" {
		t.Fatalf("evidence = %+v, want the reverse-charge policy and a reason", evidence)
	}
}

// A VAT ID the service refuses does not establish a business, so the destination rate applies. This
// is a definite answer, not a guess — and it must not be confused with "we could not ask".
func TestInvalidVATIDIsChargedTheDestinationRate(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	fake.valid = false
	env.wire(t, fake)
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	customerID := env.customer(t, "FR", "", "FR40303265045")
	decision, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != tax.DecisionRate || decision.Rate != "0.2" {
		t.Fatalf("decision = %+v, want the French rate", decision)
	}
}

// An unavailable service means nobody knows. Reverse charge is not granted, and the sale is refused
// with a code and the setting that is missing rather than priced at a guess.
func TestUnvalidatedVATIDRefusesTheSupply(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	fake.fault = "MS_UNAVAILABLE"
	fake.status = http.StatusInternalServerError
	env.wire(t, fake)
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	customerID := env.customer(t, "FR", "", "FR40303265045")
	_, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	var refusal *tax.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *tax.Refusal", err)
	}
	if refusal.Code != tax.CodeVATValidationFailed {
		t.Fatalf("code = %q, want %s", refusal.Code, tax.CodeVATValidationFailed)
	}
	if len(refusal.Missing) == 0 {
		t.Fatal("a refusal must say what is missing, or the operator cannot act on it")
	}
	if !errors.Is(err, tax.ErrRefused) {
		t.Fatal("a refusal must be recognisable with errors.Is(err, tax.ErrRefused)")
	}

	// The failed check is recorded, because "we asked and got nothing" is the fact that explains
	// the refusal a week later.
	var status string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT status FROM vat_id_validations WHERE country_code = 'FR' AND vat_id = '40303265045'`).
		Scan(&status); err != nil {
		t.Fatalf("read recorded validation: %v", err)
	}
	if status != tax.VATStatusUnavailable {
		t.Fatalf("recorded status = %q, want unavailable", status)
	}
}

// Reverse charge cannot be decided without knowing where the installation is established: an empty
// seller country makes "cross-border" unanswerable.
func TestReverseChargeNeedsASellerCountry(t *testing.T) {
	env := newRulesEnv(t)
	env.wire(t, newFakeVIES(t))
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	customerID := env.customer(t, "FR", "", "FR40303265045")
	_, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	var refusal *tax.Refusal
	if !errors.As(err, &refusal) || refusal.Code != tax.CodeSellerCountryMissing {
		t.Fatalf("err = %v, want refusal %s", err, tax.CodeSellerCountryMissing)
	}
}

// A domestic supply is never a reverse charge, however convincing the identifier is.
func TestDomesticSupplyIsNeverReverseCharged(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	env.wire(t, fake)
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "DE", "", "standard", "0.19", true)

	customerID := env.customer(t, "DE", "", "DE123456789")
	decision, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != tax.DecisionRate || decision.Rate != "0.19" || !decision.Inclusive {
		t.Fatalf("decision = %+v, want the domestic inclusive rate", decision)
	}
	if fake.count() != 0 {
		t.Fatalf("a domestic sale called the validation service %d times; it did not need to", fake.count())
	}
}

// A validation inside the TTL is reused, so a cart re-price does not call an external service on
// every render — and the stored row refuses to be rewritten.
func TestValidationIsCachedAndAppendOnly(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	env.wire(t, fake)
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	ctx := context.Background()
	customerID := env.customer(t, "FR", "", "FR40303265045")
	for i := 0; i < 3; i++ {
		if _, err := env.rules.DecideForCustomer(ctx, customerID, rulesNow); err != nil {
			t.Fatalf("decide %d: %v", i, err)
		}
	}
	if fake.count() != 1 {
		t.Fatalf("the validation service was called %d times, want 1 (the rest must be cached)", fake.count())
	}

	// The TTL passed: the answer is asked again rather than trusted for ever.
	env.rules.Now = func() time.Time { return rulesNow.Add(48 * time.Hour) }
	if _, err := env.rules.Decide(ctx, tax.Supply{
		Country: "FR", VATID: "FR40303265045", At: rulesNow.Add(48 * time.Hour),
	}); err != nil {
		t.Fatalf("decide after TTL: %v", err)
	}
	if fake.count() != 2 {
		t.Fatalf("calls after the TTL = %d, want 2", fake.count())
	}

	// Append-only: a validation is evidence an invoice points at, so rewriting one is refused by
	// the database rather than by convention.
	if _, err := env.pool.Exec(ctx, `UPDATE vat_id_validations SET status = 'invalid'`); err == nil {
		t.Fatal("updating a stored validation was allowed; the table must be append-only")
	}
}

// ------------------------------------------------------------------ nexus --

// A country with a nexus declaration is taxed where the declaration matches, and nowhere else.
func TestNexusDecidesWhereSalesTaxIsCollected(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "nexus_enforced", "true")
	env.rate(t, "US", "CA", "standard", "0.0725", false)
	env.rate(t, "US", "NY", "standard", "0.08", false)

	ctx := context.Background()
	california := env.customer(t, "US", "CA", "")

	// Declared nowhere yet: the country is not nexus-governed, so the rate table applies as before.
	decision, err := env.rules.DecideForCustomer(ctx, california, rulesNow)
	if err != nil {
		t.Fatalf("decide before any declaration: %v", err)
	}
	if decision.Kind != tax.DecisionRate || decision.Rate != "0.0725" {
		t.Fatalf("decision = %+v, want the California rate", decision)
	}

	// Declaring California turns the country nexus-governed: California is charged, New York is not.
	if _, err := env.rules.DeclareNexus(ctx, env.pool, tax.NexusInput{
		CountryCode: "US", RegionCode: "CA", Note: "warehouse in Reno",
	}, ""); err != nil {
		t.Fatalf("declare nexus: %v", err)
	}
	decision, err = env.rules.DecideForCustomer(ctx, california, rulesNow)
	if err != nil {
		t.Fatalf("decide in California: %v", err)
	}
	if decision.Kind != tax.DecisionRate || decision.Rate != "0.0725" {
		t.Fatalf("decision = %+v, want the California rate", decision)
	}
	newYork := env.customer(t, "US", "NY", "")
	decision, err = env.rules.DecideForCustomer(ctx, newYork, rulesNow)
	if err != nil {
		t.Fatalf("decide in New York: %v", err)
	}
	if decision.Kind != tax.DecisionNoNexus {
		t.Fatalf("decision = %+v, want no_nexus", decision)
	}
	if decision.Rate != "" {
		t.Fatalf("rate = %q; no-nexus charges nothing rather than a zero rate", decision.Rate)
	}
	if !strings.Contains(decision.Reason, "US-NY") {
		t.Fatalf("reason does not name the jurisdiction: %q", decision.Reason)
	}

	// A second declaration for the same jurisdiction is a conflict, not a second row.
	_, err = env.rules.DeclareNexus(ctx, env.pool, tax.NexusInput{CountryCode: "US", RegionCode: "CA"}, "")
	if !errors.Is(err, tax.ErrNexusExists) {
		t.Fatalf("duplicate declaration: err = %v, want ErrNexusExists", err)
	}
}

// --------------------------------------------------------- the other states --

// An empty rate table prices net, which is the behaviour the product shipped with. It must not
// become a refusal or a zero-rate line.
func TestEmptyRateTableChargesNoTax(t *testing.T) {
	env := newRulesEnv(t)
	customerID := env.customer(t, "DE", "", "")

	pricer, err := env.rules.PricerForSupply(context.Background(), tax.Supply{
		Country: "DE", At: rulesNow,
	})
	if err != nil {
		t.Fatalf("pricer: %v", err)
	}
	cur := money.MustLookupCurrency("EUR")
	_, totals, evidence, err := pricer.Price(context.Background(),
		[]billing.Line{{Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur)}}, cur)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Tax.Minor() != 0 || totals.Total.Minor() != 10000 {
		t.Fatalf("totals = tax %d total %d, want no tax", totals.Tax.Minor(), totals.Total.Minor())
	}
	if evidence.Policy != tax.PolicyNoTax {
		t.Fatalf("policy = %q, want %q", evidence.Policy, tax.PolicyNoTax)
	}

	// The same installation with tax switched *on* and no rates: a pricer exists, it charges
	// nothing, and it says so as no-tax. This is the upgrade case the orchestrator asked about —
	// an installation that never configured a rate must not start failing at checkout.
	env.set(t, "enabled", "true")
	emptyPricer, err := env.rules.PricerForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("pricer with tax on and no rates: %v", err)
	}
	if emptyPricer == nil {
		t.Fatal("tax is switched on, so a pricer must exist even when the table is empty")
	}
	_, emptyTotals, emptyEvidence, err := emptyPricer.Price(context.Background(),
		[]billing.Line{{Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur)}}, cur)
	if err != nil {
		t.Fatalf("price with tax on and no rates: %v", err)
	}
	if emptyTotals.Tax.Minor() != 0 || emptyTotals.Total.Minor() != 10000 {
		t.Fatalf("empty table totals = tax %d total %d, want no tax and no failure",
			emptyTotals.Tax.Minor(), emptyTotals.Total.Minor())
	}
	if emptyEvidence.Policy != tax.PolicyNoTax || emptyEvidence.Rate != "0" {
		t.Fatalf("empty table evidence = %+v, want no-tax and rate 0", emptyEvidence)
	}

	// Tax being switched off must yield no pricer at all: a pricer that charges nothing would
	// write evidence claiming a rate was resolved.
	env.set(t, "enabled", "false")
	off, err := env.rules.PricerForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("pricer with tax off: %v", err)
	}
	if off != nil {
		t.Fatalf("tax is switched off but PricerForCustomer returned %T", off)
	}
}

// A rate table in use, with no row for the destination, charges nothing and records exactly that —
// it is a gap an operator can see, not a guessed rate.
func TestUnconfiguredDestinationRecordsNoRate(t *testing.T) {
	env := newRulesEnv(t)
	env.rate(t, "DE", "", "standard", "0.19", true)
	customerID := env.customer(t, "US", "CA", "")

	decision, err := env.rules.DecideForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != tax.DecisionNoRate || decision.Policy != tax.PolicyNoRate {
		t.Fatalf("decision = %+v, want no_rate", decision)
	}
}

// A staff exemption is a human assertion and outranks every derived rule, including reverse charge.
func TestStaffExemptionWinsAndKeepsItsReason(t *testing.T) {
	env := newRulesEnv(t)
	fake := newFakeVIES(t)
	env.wire(t, fake)
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.rate(t, "FR", "", "standard", "0.2", false)

	ctx := context.Background()
	customerID := env.customer(t, "FR", "", "FR40303265045")
	if _, err := env.pool.Exec(ctx,
		`UPDATE billing_profiles SET tax_exempt = true, tax_exemption_reason = 'diplomatic mission' WHERE customer_id = $1`,
		customerID); err != nil {
		t.Fatalf("set exemption: %v", err)
	}
	decision, err := env.rules.DecideForCustomer(ctx, customerID, rulesNow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != tax.DecisionStaffExempt || decision.Reason != "diplomatic mission" {
		t.Fatalf("decision = %+v, want the staff exemption and its reason", decision)
	}
	if fake.count() != 0 {
		t.Fatal("an exempt customer must not trigger a VAT-ID validation")
	}
}

// A destination rate is charged at the exact rational the table holds, with the evidence naming the
// row that decided it.
func TestResolvedRateIsChargedExactly(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	env.rate(t, "DE", "", "standard", "0.19", true)
	customerID := env.customer(t, "DE", "", "")

	pricer, err := env.rules.PricerForCustomer(context.Background(), customerID, rulesNow)
	if err != nil {
		t.Fatalf("pricer: %v", err)
	}
	cur := money.MustLookupCurrency("EUR")
	_, totals, evidence, err := pricer.Price(context.Background(),
		[]billing.Line{{Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur)}}, cur)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// Inclusive 19% on a 10000 gross: tax 10000*19/119 = 1596.63…, rounded once to 1597.
	if totals.Tax.Minor() != 1597 || totals.Total.Minor() != 10000 {
		t.Fatalf("totals = tax %d total %d, want 1597 of 10000", totals.Tax.Minor(), totals.Total.Minor())
	}
	if evidence.Rate != "19/100" && evidence.Rate != "0.19" {
		t.Fatalf("evidence rate = %q, want the exact rational", evidence.Rate)
	}
	if evidence.Jurisdiction != "DE/standard" {
		t.Fatalf("evidence jurisdiction = %q, want DE/standard", evidence.Jurisdiction)
	}
	if !strings.HasPrefix(evidence.Policy, tax.PolicyTaxRate) {
		t.Fatalf("evidence policy = %q, want %s:<id>", evidence.Policy, tax.PolicyTaxRate)
	}
}

// ---------------------------------------------------------------- wording --

// The disclosure wording is computed from the exact rational, because a browser multiplying by 100
// prints 7.249999999999999 for the California rate.
func TestDisclosureLabelRendersTheExactRate(t *testing.T) {
	cases := []struct {
		decision tax.Decision
		want     string
	}{
		{tax.Decision{Kind: tax.DecisionRate, Rate: "0.19", Inclusive: true, Jurisdiction: "DE/standard"}, "incl. 19% VAT"},
		{tax.Decision{Kind: tax.DecisionRate, Rate: "0.0725", Jurisdiction: "US-CA/standard"}, "plus 7.25% sales tax"},
		{tax.Decision{Kind: tax.DecisionReverseCharge}, "VAT reverse charge"},
		{tax.Decision{Kind: tax.DecisionNoNexus}, "no sales tax"},
		{tax.Decision{Kind: tax.DecisionStaffExempt}, "tax exempt"},
		{tax.Decision{Kind: tax.DecisionNoRate}, "tax is calculated at checkout"},
	}
	for _, tc := range cases {
		label, err := tax.DisclosureLabel(tc.decision)
		if err != nil {
			t.Fatalf("label for %+v: %v", tc.decision, err)
		}
		if label != tc.want {
			t.Fatalf("label = %q, want %q", label, tc.want)
		}
	}

	// A rate with no terminating decimal is shown as its fraction next to a rounded display, never
	// as the rounded display alone.
	label, err := tax.PercentLabel("1/3")
	if err != nil {
		t.Fatalf("percent label: %v", err)
	}
	if !strings.HasPrefix(label, "100/3%") || !strings.Contains(label, "33.33") {
		t.Fatalf("label for 1/3 = %q", label)
	}
}
