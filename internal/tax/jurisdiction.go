package tax

// The jurisdiction rules engine.
//
// The rate table answers "how much is tax in DE". It cannot answer "does the German rate apply
// to this sale", and that is a different, prior question. Two cases where the rate table gets the
// first question wrong by construction:
//
//   - A German *business* supplies a validated VAT ID and buys a digital service from a seller in
//     another member state. The rate table's answer — 19% for DE — charges tax the customer is not
//     liable for, and the invoice is not one they can use to reverse-charge it.
//   - A California customer of a seller with no California presence. If 7.25% is in the table, the
//     table charges it; the legally relevant fact is that the seller may not be registered to
//     collect it.
//
// So the rules here are evaluated *before* a rate is applied, and a rule can only reach three
// outcomes:
//
//   - charge a resolved rate (the ordinary case),
//   - charge nothing, with a reason that names the rule (reverse charge, no nexus, staff
//     exemption),
//   - **refuse**, with a stable problem code and the list of facts that are missing.
//
// The third outcome is the important one and the reason this file exists rather than a slightly
// cleverer rate lookup. A tax engine that guesses produces a plausible number on a document that
// a customer cannot appeal. A tax engine that refuses produces a problem code an operator can act
// on. Given that a wrong rate is a legal exposure and a refusal is a support ticket, refusing is
// the safe failure, and the only acceptable default.
//
// # What is code and what is data
//
// The membership of the European Union and the published shape of each member state's VAT ID are
// facts of law, not configuration: they change when a treaty changes, and a version-controlled
// constant with a citation is reviewable in a way a row edited at 2am is not. Everything that is
// genuinely per-installation — the seller's own country, whether reverse charge is offered, which
// service to validate against, whether nexus governs — is a setting, so the existing audited
// settings path owns it (internal/settings/group_tax.go).
//
// The two facts the *customer* supplies — a VAT ID and a billing country — are read from the
// billing profile, and the outcome of every validation is stored append-only
// (`vat_id_validations`), because "reverse charge was applied" is only defensible if the check
// that justified it can be produced.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
)

// ErrRefused is the sentinel every refusal wraps. A caller that only wants "can this be priced at
// all" can check for it; one that wants to tell an operator what to fix unwraps *Refusal.
var ErrRefused = errors.New("tax: the jurisdiction rules cannot decide this supply")

// Refusal codes. Stable identifiers: the panel, the API and the tests branch on these strings,
// and the sentence next to them is allowed to change.
const (
	// CodeSellerCountryMissing is "reverse charge is enabled but the installation has not said
	// where it is established". Without it, cross-border cannot be distinguished from domestic.
	CodeSellerCountryMissing = "seller_country_missing"
	// CodeVATIDUnverified is "a VAT ID was supplied, the destination is in the EU, and no service
	// is configured to check it". Reverse charge is not granted on an unchecked string.
	CodeVATIDUnverified = "vat_id_unverified"
	// CodeVATValidationFailed is "the validation service was asked and did not answer". It is
	// distinct from an *invalid* answer, which is a fact and lets the sale be priced normally.
	CodeVATValidationFailed = "vat_id_validation_failed"
)

// The outcomes a decision can have.
const (
	// DecisionRate charges the resolved rate. Policy is the rate row that decided it.
	DecisionRate = "rate"
	// DecisionReverseCharge charges nothing under the EU B2B reverse-charge rule.
	DecisionReverseCharge = "reverse_charge"
	// DecisionNoNexus charges nothing because the installation declared no nexus in the
	// destination's subdivision.
	DecisionNoNexus = "no_nexus"
	// DecisionStaffExempt charges nothing because a staff member set the exemption flag. It is the
	// pre-existing override and it wins, because a human decision outranks a derived one.
	DecisionStaffExempt = "staff_exempt"
	// DecisionNoRate charges nothing because the rate table has rows but none for this
	// jurisdiction. The evidence says exactly that, which is what makes it auditable.
	DecisionNoRate = "no_rate"
	// DecisionNoTax charges nothing because the rate table is empty. That is the "tax is
	// configured but unused" state, and pricing net is the behaviour the installation had before
	// this engine existed.
	DecisionNoTax = "no_tax"
)

// Policies recorded in tax evidence. They are not the same strings as the decision kinds: the
// policy is what an invoice reader sees a year later, so it names the rule, not the code path.
const (
	PolicyTaxRate       = "tax-rate"
	PolicyReverseCharge = "eu-reverse-charge"
	PolicyNoNexus       = "no-sales-tax-nexus"
	PolicyStaffExempt   = "tax-exempt"
	PolicyNoRate        = "no-rate"
	PolicyNoTax         = "no-tax"
)

// Supply is one sale as the rules see it. Everything here is a fact the caller already has.
type Supply struct {
	// CustomerID names the customer, so a validation can be recorded against them. Optional: a
	// quote for a visitor has no customer row.
	CustomerID string
	// Country is the destination — the customer's billing country, ISO 3166-1 alpha-2, upper case.
	Country string
	// Region is the subnational destination (a US state), empty when not supplied.
	Region string
	// TaxClass is the kind of supply: "standard", "reduced", etc. It selects the rate row.
	TaxClass string
	// VATID is the identifier on the customer's billing profile, as typed.
	VATID string
	// StaffExempt and StaffExemptReason are the existing staff override. When set, no rule runs.
	StaffExempt       bool
	StaffExemptReason string
	// At pins the instant the rate is resolved at and the validation is judged fresh against.
	At time.Time
}

// Decision is what the rules concluded.
type Decision struct {
	Kind string
	// Policy is the tax-evidence policy string. It is recorded on the invoice snapshot, so it must
	// name the rule that applied rather than the branch that ran.
	Policy string
	// Jurisdiction is the place of supply as the evidence renders it ("DE/standard", "US-CA").
	Jurisdiction string
	// Rate is the exact rational rate when Kind is DecisionRate, empty otherwise. A zero decision
	// is not "a rate of 0": it is the absence of a rate plus the reason it is absent.
	Rate string
	// Inclusive mirrors the resolved rate row: an EU price is quoted tax-inclusive, a US one is
	// not, and one installation can sell into both.
	Inclusive bool
	// Reason is the sentence recorded as the exemption reason when no tax is charged.
	Reason string
	// Validation is the VAT-ID check the decision used, when it used one.
	Validation *VATValidation
	// Supply echoes what was decided about, so a caller does not have to thread it separately.
	Supply Supply
}

// Charges reports whether the decision adds tax.
func (d Decision) Charges() bool { return d.Kind == DecisionRate && d.Rate != "" }

// Refusal is a supply the rules will not price. It carries what is missing, because "refused" is
// only actionable if the operator is told which fact would let the engine decide.
type Refusal struct {
	Code    string   `json:"code"`
	Detail  string   `json:"detail"`
	Missing []string `json:"missing,omitempty"`
	Supply  Supply   `json:"-"`
}

func (r *Refusal) Error() string {
	if len(r.Missing) == 0 {
		return fmt.Sprintf("tax: %s: %s", r.Code, r.Detail)
	}
	return fmt.Sprintf("tax: %s: %s (missing: %s)", r.Code, r.Detail, strings.Join(r.Missing, ", "))
}

// Unwrap makes errors.Is(err, tax.ErrRefused) the check a caller remembers.
func (r *Refusal) Unwrap() error { return ErrRefused }

func refuse(code, detail string, missing ...string) *Refusal {
	return &Refusal{Code: code, Detail: detail, Missing: missing}
}

// Config is the installation's answer to the questions only it can answer.
type Config struct {
	// SellerCountry is where the installation is established. Empty means "not configured".
	SellerCountry string
	// ReverseCharge enables the EU B2B rule.
	ReverseCharge bool
	// NexusEnforced enables the subnational sales-tax rule.
	NexusEnforced bool
	// VATValidationURL is the endpoint a VAT ID is checked against, empty for none.
	VATValidationURL string
	// VATValidationTTL is how long a check is trusted before it is repeated.
	VATValidationTTL time.Duration
}

// Rules evaluates a supply against the installation's configuration, the rate table, the nexus
// declarations and the VAT-ID validation history.
//
// It holds no per-request state; construct one per call site (it is a pool reference plus a
// clock). The validator is a field rather than a constructor argument so a test can pin the
// service, and so an installation can be served by an adapter other than VIES without a schema
// change.
type Rules struct {
	pool  *store.Pool
	store *commerce.Store
	// Validator is the service a VAT ID is checked against. Nil means "build one from
	// Config.VATValidationURL", which is the VIES adapter.
	Validator VATValidator
	// Now is the clock, injectable so a test can age a validation past its TTL.
	Now func() time.Time
}

// NewRules builds a rules engine for a pool.
func NewRules(pool *store.Pool) *Rules {
	return &Rules{pool: pool, store: commerce.New(pool)}
}

// Config reads the five jurisdiction settings in one pass.
func (r *Rules) Config(ctx context.Context) (Config, error) {
	s := settings.New(r.pool)
	var (
		cfg Config
		err error
	)
	if cfg.SellerCountry, err = s.String(ctx, "tax", "seller_country"); err != nil {
		return Config{}, fmt.Errorf("tax: read seller country: %w", err)
	}
	cfg.SellerCountry = strings.ToUpper(strings.TrimSpace(cfg.SellerCountry))
	if cfg.ReverseCharge, err = s.Bool(ctx, "tax", "reverse_charge"); err != nil {
		return Config{}, fmt.Errorf("tax: read reverse charge setting: %w", err)
	}
	if cfg.NexusEnforced, err = s.Bool(ctx, "tax", "nexus_enforced"); err != nil {
		return Config{}, fmt.Errorf("tax: read nexus setting: %w", err)
	}
	if cfg.VATValidationURL, err = s.String(ctx, "tax", "vat_validation_url"); err != nil {
		return Config{}, fmt.Errorf("tax: read validation URL: %w", err)
	}
	cfg.VATValidationURL = strings.TrimSpace(cfg.VATValidationURL)
	hours, err := s.Int(ctx, "tax", "vat_validation_ttl_hours")
	if err != nil {
		return Config{}, fmt.Errorf("tax: read validation TTL: %w", err)
	}
	cfg.VATValidationTTL = time.Duration(hours) * time.Hour
	return cfg, nil
}

// Decide evaluates one supply. It returns either a decision or a *Refusal; a database failure is
// the only other outcome, because a rule engine that turns "I could not read the rate table" into
// "no tax" is the failure mode this file exists to prevent.
func (r *Rules) Decide(ctx context.Context, supply Supply) (Decision, error) {
	if supply.At.IsZero() {
		supply.At = r.now()
	}
	supply.Country = strings.ToUpper(strings.TrimSpace(supply.Country))
	supply.Region = strings.ToUpper(strings.TrimSpace(supply.Region))
	if supply.TaxClass == "" {
		supply.TaxClass = commerce.DefaultTaxClass
	}
	supply.VATID = strings.TrimSpace(supply.VATID)

	cfg, err := r.Config(ctx)
	if err != nil {
		return Decision{}, err
	}

	// A staff exemption wins over every derived rule. It is a human asserting a fact (a charity, a
	// diplomatic mission, a government buyer) that no rule can observe, and the existing behaviour
	// of `billing_profiles.tax_exempt` is preserved exactly, including its reason.
	if supply.StaffExempt {
		reason := strings.TrimSpace(supply.StaffExemptReason)
		if reason == "" {
			reason = "tax exempt"
		}
		return Decision{
			Kind:         DecisionStaffExempt,
			Policy:       PolicyStaffExempt,
			Jurisdiction: jurisdictionOf(supply.Country, supply.Region, supply.TaxClass),
			Reason:       reason,
			Supply:       supply,
		}, nil
	}

	rate, err := r.resolve(ctx, supply)
	if err != nil {
		return Decision{}, err
	}
	if rate == nil {
		// Two different absences, and the difference is what an operator needs to see. No rows at
		// all is an installation that has not configured tax; rows that do not cover this place is
		// a gap in a table that is otherwise in use.
		empty, err := r.rateTableEmpty(ctx)
		if err != nil {
			return Decision{}, err
		}
		if empty {
			return Decision{
				Kind:         DecisionNoTax,
				Policy:       PolicyNoTax,
				Jurisdiction: jurisdictionOf(supply.Country, supply.Region, supply.TaxClass),
				Reason:       "no tax rates are configured",
				Supply:       supply,
			}, nil
		}
		return Decision{
			Kind:         DecisionNoRate,
			Policy:       PolicyNoRate,
			Jurisdiction: jurisdictionOf(supply.Country, supply.Region, supply.TaxClass),
			Reason:       "no tax rate is configured for this jurisdiction",
			Supply:       supply,
		}, nil
	}

	// Nexus is checked before the rate is applied, because the question is not "what is the rate"
	// but "is this installation required to collect at all".
	if decision, refused, err := r.applyNexus(ctx, supply, cfg); err != nil || refused != nil || decision != nil {
		if err != nil {
			return Decision{}, err
		}
		if refused != nil {
			return Decision{}, refused
		}
		return *decision, nil
	}

	if decision, refused, err := r.applyReverseCharge(ctx, supply, cfg); err != nil || refused != nil || decision != nil {
		if err != nil {
			return Decision{}, err
		}
		if refused != nil {
			return Decision{}, refused
		}
		return *decision, nil
	}

	return Decision{
		Kind:         DecisionRate,
		Policy:       PolicyTaxRate + ":" + rate.ID,
		Jurisdiction: commerce.TaxJurisdiction(*rate),
		Rate:         rate.Rate,
		Inclusive:    rate.Inclusive,
		Supply:       supply,
	}, nil
}

// resolve finds the rate row that applies, or nil when none does.
func (r *Rules) resolve(ctx context.Context, supply Supply) (*commerce.TaxRate, error) {
	if supply.Country == "" {
		return nil, nil
	}
	rate, err := r.store.ResolveRate(ctx, supply.Country, supply.Region, supply.TaxClass, supply.At)
	if errors.Is(err, commerce.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tax: resolve rate for %s: %w", jurisdictionOf(supply.Country, supply.Region, supply.TaxClass), err)
	}
	return &rate, nil
}

// rateTableEmpty answers "has this installation ever configured a rate", which is the question
// that distinguishes "tax is off in effect" from "this destination is missing".
func (r *Rules) rateTableEmpty(ctx context.Context) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tax_rates)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("tax: inspect rate table: %w", err)
	}
	return !exists, nil
}

// applyNexus runs the subnational sales-tax rule.
//
// It applies only to a destination country the installation has made a nexus declaration in. That
// is the design decision worth stating: the presence of *any* declaration for a country is the
// operator telling the panel "this country's tax is nexus-governed", and only then does the
// absence of a declaration for a particular subdivision mean "do not collect". Without that rule
// an installation that declares one US state would silently stop collecting in the other forty-
// nine, which is not what declaring one state means.
//
// A declared destination falls through to the rate table; an undeclared one in a nexus-governed
// country is priced at zero with the reason recorded. That is a determination, not a guess: the
// operator has stated where the installation has nexus, and the absence of a declaration is the
// statement that it does not have it there.
func (r *Rules) applyNexus(ctx context.Context, supply Supply, cfg Config) (*Decision, *Refusal, error) {
	if !cfg.NexusEnforced || supply.Country == "" {
		return nil, nil, nil
	}
	countryWide, err := r.hasNexus(ctx, supply.Country, "")
	if err != nil {
		return nil, nil, err
	}
	regionDeclared := false
	if supply.Region != "" {
		if regionDeclared, err = r.hasNexus(ctx, supply.Country, supply.Region); err != nil {
			return nil, nil, err
		}
	}
	if countryWide || regionDeclared {
		return nil, nil, nil
	}
	governed, err := r.nexusGoverns(ctx, supply.Country)
	if err != nil {
		return nil, nil, err
	}
	if !governed {
		// No declaration in this country at all: nexus is not the rule the operator is using here,
		// so the ordinary rate applies and this rule stays out of the way.
		return nil, nil, nil
	}
	return &Decision{
		Kind:         DecisionNoNexus,
		Policy:       PolicyNoNexus,
		Jurisdiction: jurisdictionOf(supply.Country, supply.Region, supply.TaxClass),
		Reason: fmt.Sprintf(
			"no sales-tax nexus is declared for %s; tax is not collected there",
			jurisdictionOf(supply.Country, supply.Region, supply.TaxClass)),
		Supply: supply,
	}, nil, nil
}

// applyReverseCharge runs the EU B2B rule.
func (r *Rules) applyReverseCharge(ctx context.Context, supply Supply, cfg Config) (*Decision, *Refusal, error) {
	if !cfg.ReverseCharge {
		return nil, nil, nil
	}
	if !IsEU(supply.Country) {
		return nil, nil, nil
	}
	// A domestic supply is never a reverse charge, whatever the customer's VAT ID says.
	if cfg.SellerCountry != "" && cfg.SellerCountry == supply.Country {
		return nil, nil, nil
	}
	number, shaped := NormalizeVATID(supply.Country, supply.VATID)
	if supply.VATID == "" {
		return nil, nil, nil
	}
	if cfg.SellerCountry == "" {
		return nil, refuse(CodeSellerCountryMissing,
			"reverse charge is enabled and the destination is in the EU, but the installation has not declared the country it is established in, so cross-border cannot be established",
			"tax.seller_country"), nil
	}
	if !IsEU(cfg.SellerCountry) {
		// A seller outside the EU does not apply the intra-EU reverse-charge rule; the sale is
		// priced by the ordinary destination rules.
		return nil, nil, nil
	}
	if !shaped {
		// The identifier is not even the shape the destination state publishes. That is a fact,
		// not a failure to check: the customer cannot reverse-charge with it, so the sale is a
		// consumer sale and carries the destination rate.
		return nil, nil, nil
	}

	validation, err := r.ValidateVATID(ctx, supply.CustomerID, supply.Country, number, cfg)
	if err != nil {
		var failed *ValidationUnavailable
		if !errors.As(err, &failed) {
			return nil, nil, err
		}
		// The row was recorded before the error was returned, so the switch below reports it with
		// the same code a cached "unavailable" answer gets. A code that changed between the first
		// attempt and the next cart render would be a code nobody could branch on.
	}
	switch validation.Status {
	case VATStatusValid:
		return &Decision{
			Kind:         DecisionReverseCharge,
			Policy:       PolicyReverseCharge,
			Jurisdiction: jurisdictionOf(supply.Country, supply.Region, supply.TaxClass),
			Reason: fmt.Sprintf(
				"reverse charge — VAT ID %s%s validated %s by %s; VAT is accounted for by the recipient",
				validation.CountryCode, validation.VATID,
				validation.CheckedAt.UTC().Format("2006-01-02"),
				validation.ServiceURL),
			Validation: &validation,
			Supply:     supply,
		}, nil, nil
	case VATStatusInvalid:
		// The service answered no. The identifier does not establish a business, so the sale is
		// taxed at the destination rate — a definite answer, not a guess.
		return nil, nil, nil
	default:
		// Unavailable is not an answer. Reverse charge cannot be granted on it, and charging the
		// destination rate to a business that is entitled to reverse charge is the wrong invoice
		// this rule exists to prevent.
		code := CodeVATValidationFailed
		missing := "a reachable VAT-ID validation service"
		if validation.ServiceURL == "" {
			code = CodeVATIDUnverified
			missing = "tax.vat_validation_url"
		}
		return nil, refuse(code,
			"a VAT ID was supplied but could not be validated: "+validation.Detail, missing), nil
	}
}

// ---------------------------------------------------------------- EU facts --

// euMemberStates is the European Union as of the 2024 accession (27 member states).
//
// It is a constant with a citation rather than a table because it is a fact about the EU, not
// about this installation: it changes when a treaty changes, and the change belongs in a code
// review. The key is the ISO 3166-1 alpha-2 country code; Greece's VAT prefix is EL, so that
// spelling is accepted separately in NormalizeVATID.
var euMemberStates = map[string]string{
	"AT": "Austria", "BE": "Belgium", "BG": "Bulgaria", "HR": "Croatia",
	"CY": "Cyprus", "CZ": "Czechia", "DK": "Denmark", "EE": "Estonia",
	"FI": "Finland", "FR": "France", "DE": "Germany", "GR": "Greece",
	"HU": "Hungary", "IE": "Ireland", "IT": "Italy", "LV": "Latvia",
	"LT": "Lithuania", "LU": "Luxembourg", "MT": "Malta", "NL": "Netherlands",
	"PL": "Poland", "PT": "Portugal", "RO": "Romania", "SK": "Slovakia",
	"SI": "Slovenia", "ES": "Spain", "SE": "Sweden",
}

// IsEU reports whether a country is a European Union member state.
func IsEU(country string) bool {
	_, ok := euMemberStates[strings.ToUpper(strings.TrimSpace(country))]
	return ok
}

// EUCountries lists the member states in code order, for a screen that has to offer them.
func EUCountries() []string {
	out := make([]string, 0, len(euMemberStates))
	for code := range euMemberStates {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// vatIDShapes is the national part of each member state's VAT identifier, as published for VIES.
//
// These are *shape* checks, not validity: a number can match its state's format and still belong
// to nobody. They exist so an obviously malformed identifier is refused before a network call, and
// so the refusal can say which format was expected. Only a service answer can grant reverse
// charge, which is why applyReverseCharge asks one even after the shape matches.
var vatIDShapes = map[string]*regexp.Regexp{
	"AT": regexp.MustCompile(`^U[0-9]{8}$`),
	"BE": regexp.MustCompile(`^[0-9]{10}$`),
	"BG": regexp.MustCompile(`^[0-9]{9,10}$`),
	"HR": regexp.MustCompile(`^[0-9]{11}$`),
	"CY": regexp.MustCompile(`^[0-9]{8}[A-Z]$`),
	"CZ": regexp.MustCompile(`^[0-9]{8,10}$`),
	"DK": regexp.MustCompile(`^[0-9]{8}$`),
	"EE": regexp.MustCompile(`^[0-9]{9}$`),
	"FI": regexp.MustCompile(`^[0-9]{8}$`),
	"FR": regexp.MustCompile(`^[A-Z0-9]{2}[0-9]{9}$`),
	"DE": regexp.MustCompile(`^[0-9]{9}$`),
	"GR": regexp.MustCompile(`^[0-9]{9}$`),
	"HU": regexp.MustCompile(`^[0-9]{8}$`),
	"IE": regexp.MustCompile(`^([0-9]{7}[A-Z]{1,2}|[0-9][A-Z+*][0-9]{5}[A-Z]|[0-9]{7}[A-Z]{2})$`),
	"IT": regexp.MustCompile(`^[0-9]{11}$`),
	"LV": regexp.MustCompile(`^[0-9]{11}$`),
	"LT": regexp.MustCompile(`^([0-9]{9}|[0-9]{12})$`),
	"LU": regexp.MustCompile(`^[0-9]{8}$`),
	"MT": regexp.MustCompile(`^[0-9]{8}$`),
	"NL": regexp.MustCompile(`^[0-9]{9}B[0-9]{2}$`),
	"PL": regexp.MustCompile(`^[0-9]{10}$`),
	"PT": regexp.MustCompile(`^[0-9]{9}$`),
	"RO": regexp.MustCompile(`^[0-9]{2,10}$`),
	"SK": regexp.MustCompile(`^[0-9]{10}$`),
	"SI": regexp.MustCompile(`^[0-9]{8}$`),
	"ES": regexp.MustCompile(`^([A-Z][0-9]{7}[A-Z0-9]|[0-9]{9})$`),
	"SE": regexp.MustCompile(`^[0-9]{12}$`),
}

// NormalizeVATID strips the separators people type, removes the country prefix and reports whether
// what is left is a plausible identifier *for the destination*.
//
// The destination check is the point. A French number supplied for a German supply is a perfectly
// valid French identifier and the wrong identifier for that sale: it does not establish that the
// customer is a business in Germany, so it cannot support a German reverse charge. Treating it as
// merely "an identifier" would grant a zero rate on a fact about a different country.
func NormalizeVATID(country, raw string) (number string, ok bool) {
	country = strings.ToUpper(strings.TrimSpace(country))
	cleaned := strings.ToUpper(raw)
	cleaned = strings.NewReplacer(" ", "", "-", "", ".", "", "/", "", "_", "").Replace(cleaned)
	if cleaned == "" {
		return "", false
	}
	if len(cleaned) > 2 {
		prefix := cleaned[:2]
		if prefix == "EL" {
			prefix = "GR"
		}
		if _, isMember := euMemberStates[prefix]; isMember {
			if prefix != country {
				// The identifier belongs to another member state; it is not this sale's.
				return cleaned[2:], false
			}
			cleaned = cleaned[2:]
		}
	}
	shape, known := vatIDShapes[country]
	if !known {
		return cleaned, false
	}
	return cleaned, shape.MatchString(cleaned)
}

// VATIDCountry returns the member state an identifier's prefix names, or "" when it names none.
func VATIDCountry(raw string) string {
	cleaned := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", ".", "").Replace(strings.TrimSpace(raw)))
	if len(cleaned) < 3 {
		return ""
	}
	prefix := cleaned[:2]
	if prefix == "EL" {
		return "GR"
	}
	if _, ok := euMemberStates[prefix]; ok {
		return prefix
	}
	return ""
}

// ---------------------------------------------------------------- helpers --

// jurisdictionOf renders a place of supply for evidence: "DE/standard", "US-CA/standard".
func jurisdictionOf(country, region, class string) string {
	if country == "" {
		return ""
	}
	if class == "" {
		class = commerce.DefaultTaxClass
	}
	if region != "" {
		return country + "-" + region + "/" + class
	}
	return country + "/" + class
}

func (r *Rules) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
