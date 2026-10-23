// The tax-rate surface, as the panel's own screens see it.
//
// The rate table's CRUD is in commerce_tax.go: creating, closing and deleting a dated row. This
// file is the other half — where the rows come from, and which of them applies — and it exists as
// its own registrar so the import and jurisdiction surfaces can grow without editing a list four
// domains share.
//
// Two surfaces live here:
//
//   - the staff jurisdiction screen (ADM-07, TAX-02/05/06): the seller's own country, the reverse
//     charge switch, the VAT-ID validation service, and where the installation has sales-tax
//     nexus. Plus the two things an operator cannot get anywhere else: "why would this sale be
//     taxed" and "what did the validation service say".
//
//   - the public disclosure read (TAX-08): the one sentence a storefront may show next to a price
//     — "incl. 19% VAT" — computed from the same rules the invoice uses. It is public because a
//     price is public, and it is computed here rather than in the browser because a percentage
//     derived from a rational is exactly the kind of arithmetic a renderer gets wrong.
//
// The write surface is deliberately thin. The five settings are edited through the existing
// audited settings endpoint (PUT /api/v1/staff/settings, group `tax`), so "who turned reverse
// charge on" is answerable without a second audit path. What is here is the data those settings
// interpret: nexus declarations, VAT-ID checks and the decision preview.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/tax"
	"github.com/jackc/pgx/v5"
)

// init registers the tax-rate import and jurisdiction surfaces.
func init() {
	RegisterRoutes("88-tax-import", func(r *router) { registerTaxImportRoutes(r, r.s) })
	RegisterRoutes("89-tax-jurisdiction", func(r *router) { registerTaxJurisdictionRoutes(r, r.s) })
}

// registerTaxJurisdictionRoutes wires the rules screen and the public disclosure.
func registerTaxJurisdictionRoutes(r *router, s *Server) {
	r.staffGet("/api/v1/staff/tax-jurisdiction", authn.PermTaxWrite, s.handleTaxJurisdiction)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-jurisdiction/nexus", authn.PermTaxWrite,
		s.handleDeclareNexus)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/tax-jurisdiction/nexus/{id}", authn.PermTaxWrite,
		s.handleRetireNexus)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-jurisdiction/validate-vat", authn.PermTaxWrite,
		s.handleValidateVAT)
	r.staffGet("/api/v1/staff/tax-jurisdiction/validations", authn.PermTaxWrite,
		s.handleVATValidationHistory)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-jurisdiction/decide", authn.PermTaxWrite,
		s.handleExplainDecision)

	// Public, because it explains a price that is already public. It never names a customer.
	r.publicGet("/api/v1/tax/disclosure", s.handleTaxDisclosure)
}

// taxJurisdictionError maps the rules engine's errors to problem documents.
func (s *Server) taxJurisdictionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tax.ErrInvalidNexus):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	case errors.Is(err, tax.ErrNexusExists):
		writeProblem(w, http.StatusConflict, "nexus_exists", err.Error())
	case errors.Is(err, tax.ErrNexusNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such nexus declaration")
	case errors.Is(err, tax.ErrRefused):
		var refusal *tax.Refusal
		if errors.As(err, &refusal) {
			writeProblem(w, http.StatusUnprocessableEntity, refusal.Code, refusal.Detail)
			return
		}
		writeProblem(w, http.StatusUnprocessableEntity, "tax_refused", err.Error())
	case errors.Is(err, tax.ErrInvalidSource):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_source", err.Error())
	default:
		s.staffError(w, r, err)
	}
}

// jurisdictionConfigJSON is the wire shape of the five rules settings. It is spelled out rather
// than marshalling tax.Config so the duration is hours and the keys are the setting names an
// operator reads on the settings page.
type jurisdictionConfigJSON struct {
	SellerCountry         string `json:"seller_country"`
	ReverseCharge         bool   `json:"reverse_charge"`
	NexusEnforced         bool   `json:"nexus_enforced"`
	VATValidationURL      string `json:"vat_validation_url"`
	VATValidationTTLHours int    `json:"vat_validation_ttl_hours"`
}

// jurisdictionStatusResponse is the screen's whole read.
type jurisdictionStatusResponse struct {
	Config jurisdictionConfigJSON `json:"config"`
	// Nexus is every declaration, retired ones included, so the screen can show that the
	// installation used to collect somewhere and stopped.
	Nexus []tax.Nexus `json:"nexus"`
	// EUMemberStates is the closed list the reverse-charge rule applies to. It is served rather
	// than hard-coded in the screen so the page cannot disagree with the engine about membership.
	EUMemberStates []string `json:"eu_member_states"`
	// Gaps names the jurisdictions that have an open rate but no nexus declaration, for a country
	// whose tax is nexus-governed. These are the places where a rate exists and will not be
	// charged, which is the most expensive kind of quiet misconfiguration.
	Gaps []string `json:"nexus_gaps"`
	// Warnings are the configuration contradictions the engine cannot resolve on its own. They are
	// prose for a human; nothing branches on them.
	Warnings []string `json:"warnings"`
}

// handleTaxJurisdiction reads the rules configuration and its consequences.
func (s *Server) handleTaxJurisdiction(w http.ResponseWriter, r *http.Request) {
	rules := tax.NewRules(s.pool)
	cfg, err := rules.Config(r.Context())
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	nexus, err := rules.NexusList(r.Context())
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	out := jurisdictionStatusResponse{
		Config: jurisdictionConfigJSON{
			SellerCountry:    cfg.SellerCountry,
			ReverseCharge:    cfg.ReverseCharge,
			NexusEnforced:    cfg.NexusEnforced,
			VATValidationURL: cfg.VATValidationURL,
			// Hours rather than a duration: a JSON duration is nanoseconds, which no screen
			// should have to divide.
			VATValidationTTLHours: int(cfg.VATValidationTTL.Hours()),
		},
		Nexus:          nexus,
		EUMemberStates: tax.EUCountries(),
		Gaps:           []string{},
		Warnings:       []string{},
	}

	if cfg.ReverseCharge && cfg.SellerCountry == "" {
		out.Warnings = append(out.Warnings,
			"Reverse charge is switched on, but the seller country is not set. No EU sale can be decided until it is: whether a supply is cross-border is the first question the rule asks.")
	}
	if cfg.ReverseCharge && cfg.VATValidationURL == "" {
		out.Warnings = append(out.Warnings,
			"Reverse charge is switched on without a VAT-ID validation service. A VAT ID that cannot be checked refuses the sale rather than granting a zero rate.")
	}
	if cfg.NexusEnforced {
		if len(nexus) == 0 {
			out.Warnings = append(out.Warnings,
				"Nexus enforcement is switched on but no jurisdiction is declared. A country with no declaration anywhere is priced from the rate table as before; declare the states the installation has nexus in to stop collecting elsewhere.")
		}
		// A rate for a jurisdiction with no declaration is a rate that will never be charged.
		countries := make([]string, 0, 4)
		for _, n := range nexus {
			if !stringInSlice(countries, n.CountryCode) {
				countries = append(countries, n.CountryCode)
			}
		}
		for _, country := range countries {
			gaps, err := rules.NexusGaps(r.Context(), country)
			if err != nil {
				s.taxJurisdictionError(w, r, err)
				return
			}
			out.Gaps = append(out.Gaps, gaps...)
		}
		if len(out.Gaps) > 0 {
			out.Warnings = append(out.Warnings,
				"These jurisdictions have a rate but no nexus declaration, so tax is not collected there: "+strings.Join(out.Gaps, ", ")+".")
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeclareNexus records where the installation may collect subnational tax.
func (s *Server) handleDeclareNexus(w http.ResponseWriter, r *http.Request) {
	var in tax.NexusInput
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	actorID := actingUserID(r)
	rules := tax.NewRules(s.pool)
	var declared tax.Nexus
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		declared, err = rules.DeclareNexus(ctx, tx, in, actorID)
		if err != nil {
			return err
		}
		// A nexus declaration is the installation asserting where it may collect tax. That is a
		// legal position, not a preference, so who declared it and why is part of the record.
		return audit.Record(ctx, tx, s.auditEvent(r, "tax_jurisdiction.nexus_declare", "tax_nexus",
			declared.ID, audit.OutcomeSuccess).WithChanges(map[string]any{
			"jurisdiction": declared.Jurisdiction(),
			"note":         declared.Note,
		}))
	})
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, nexusJSON(declared))
}

// handleRetireNexus removes a declaration.
func (s *Server) handleRetireNexus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rules := tax.NewRules(s.pool)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := rules.RetireNexus(ctx, tx, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "tax_jurisdiction.nexus_retire", "tax_nexus",
			id, audit.OutcomeSuccess))
	})
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// vatValidationRequest is the body of the two VAT-ID reads.
type vatValidationRequest struct {
	Country string `json:"country"`
	VATID   string `json:"vat_id"`
	// CustomerID, when set, records the check against the customer whose profile holds the
	// identifier. It is optional: an operator may check an identifier before saving it.
	CustomerID string `json:"customer_id"`
}

// handleValidateVAT asks the configured service about an identifier, records the answer, and
// returns it. The answer is recorded whatever it is — "we asked and got nothing" is the fact that
// explains a refusal later.
func (s *Server) handleValidateVAT(w http.ResponseWriter, r *http.Request) {
	var in vatValidationRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	rules := tax.NewRules(s.pool)
	cfg, err := rules.Config(r.Context())
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	country := countryUpper(in.Country)
	number, shaped := tax.NormalizeVATID(country, in.VATID)
	if !shaped {
		writeProblem(w, http.StatusUnprocessableEntity, "vat_id_malformed",
			"that identifier does not match the format the country publishes, so no service was asked")
		return
	}
	validation, err := rules.ValidateVATID(r.Context(), in.CustomerID, country, number, cfg)
	if err != nil {
		var unavailable *tax.ValidationUnavailable
		if errors.As(err, &unavailable) {
			// The refusal is the honest answer, and the recorded row explains it.
			writeProblem(w, http.StatusBadGateway, "vat_validation_unavailable", unavailable.Detail)
			return
		}
		s.taxJurisdictionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, validationJSON(validation))
}

// handleVATValidationHistory lists the checks for one identifier.
func (s *Server) handleVATValidationHistory(w http.ResponseWriter, r *http.Request) {
	country := countryUpper(r.URL.Query().Get("country"))
	number, shaped := tax.NormalizeVATID(country, r.URL.Query().Get("vat_id"))
	if !shaped {
		writeProblem(w, http.StatusUnprocessableEntity, "vat_id_malformed",
			"that identifier does not match the format the country publishes")
		return
	}
	history, err := tax.NewRules(s.pool).ValidationHistory(r.Context(), country, number, 20)
	if err != nil {
		s.taxJurisdictionError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(history))
	for _, v := range history {
		out = append(out, validationJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// decisionRequest is a hypothetical sale to explain.
type decisionRequest struct {
	CustomerID string `json:"customer_id"`
	Country    string `json:"country"`
	Region     string `json:"region"`
	VATID      string `json:"vat_id"`
	TaxClass   string `json:"tax_class"`
}

// handleExplainDecision answers "why would this sale be taxed".
//
// It is a POST because it is not a pure read: deciding a VAT-ID supply asks the validation service
// and appends the answer to the history. Registering it as a staff mutation is what gives it the
// same Idempotency-Key and audit treatment as the action it performs. A refusal is *not* an HTTP
// error here — it is the answer to the question — so it is returned with 200 and an `outcome` the
// screen branches on.
func (s *Server) handleExplainDecision(w http.ResponseWriter, r *http.Request) {
	var in decisionRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	rules := tax.NewRules(s.pool)
	ctx := r.Context()

	var (
		decision tax.Decision
		err      error
	)
	if in.CustomerID != "" {
		decision, err = rules.DecideForCustomer(ctx, in.CustomerID, time.Now().UTC())
	} else {
		decision, err = rules.Decide(ctx, tax.Supply{
			Country:  countryUpper(in.Country),
			Region:   in.Region,
			VATID:    in.VATID,
			TaxClass: in.TaxClass,
			At:       time.Now().UTC(),
		})
	}
	var refusal *tax.Refusal
	if err != nil && !errors.As(err, &refusal) {
		s.taxJurisdictionError(w, r, err)
		return
	}

	out := map[string]any{
		"outcome": "refused",
		"supply": map[string]any{
			"customer_id": in.CustomerID, "country": countryUpper(in.Country),
			"region": in.Region, "vat_id": in.VATID,
		},
	}
	if refusal != nil {
		out["refusal"] = refusalJSON(refusal)
	} else {
		out["outcome"] = "decided"
		out["decision"] = decisionJSON(decision)
	}
	changes := map[string]any{
		"customer_id": in.CustomerID,
		"country":     countryUpper(in.Country),
		"region":      in.Region,
		"outcome":     out["outcome"],
	}
	if refusal != nil {
		changes["refusal_code"] = refusal.Code
	} else {
		changes["decision_kind"] = decision.Kind
		changes["policy"] = decision.Policy
	}
	// The preview is recorded too: "an operator checked this sale and the engine said reverse
	// charge" is exactly the question asked when a zero-rated invoice is audited.
	if err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "tax_jurisdiction.decide", "tax_supply",
			"", audit.OutcomeSuccess).WithChanges(changes))
	}); err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTaxDisclosure is the sentence a storefront may print next to a price.
//
// The country is the destination the visitor has told the shop; when it is absent the installation's
// own country is used, because that is the only market whose price can be shown before anybody has
// said where they are. The rate and the wording are computed from the same decision an invoice uses,
// so a page cannot advertise a percentage the checkout then disagrees with.
func (s *Server) handleTaxDisclosure(w http.ResponseWriter, r *http.Request) {
	rules := tax.NewRules(s.pool)
	ctx := r.Context()
	enabled, err := rules.TaxEnabled(ctx)
	if err != nil {
		// A disclosure is a display aid. A storefront that cannot read the rules should print prices
		// without a tax sentence rather than fail the page.
		writeJSON(w, http.StatusOK, map[string]any{
			"tax_enabled": false, "configured": false,
			"label": "Tax is calculated at checkout",
		})
		return
	}
	cfg, err := rules.Config(ctx)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	country := countryUpper(r.URL.Query().Get("country"))
	if country == "" {
		country = cfg.SellerCountry
	}
	region := r.URL.Query().Get("region")
	class := r.URL.Query().Get("class")

	out := map[string]any{
		"tax_enabled": enabled,
		"country":     country,
		"region":      region,
		"tax_class":   classOrDefault(class),
		"configured":  country != "",
	}
	if !enabled || country == "" {
		out["label"] = "Tax is calculated at checkout"
		writeJSON(w, http.StatusOK, out)
		return
	}

	decision, err := rules.Decide(ctx, tax.Supply{
		Country: country, Region: region, TaxClass: class, At: time.Now().UTC(),
	})
	var refusal *tax.Refusal
	if err != nil && !errors.As(err, &refusal) {
		s.staffError(w, r, err)
		return
	}
	if refusal != nil {
		// The shop cannot price this destination. The disclosure says so rather than showing a
		// percentage the checkout will refuse.
		out["outcome"] = "refused"
		out["refusal"] = refusalJSON(refusal)
		out["label"] = "Tax cannot be determined for this destination"
		writeJSON(w, http.StatusOK, out)
		return
	}
	label, err := tax.DisclosureLabel(decision)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out["outcome"] = "decided"
	out["decision"] = decisionJSON(decision)
	out["label"] = label
	writeJSON(w, http.StatusOK, out)
}

// IssuerFor builds an invoice issuer whose pricer resolves this customer's jurisdiction.
//
// The server's default issuer prices with `FixedTaxPricer{Policy: "no-tax"}`, and that default is
// correct for an installation that has configured no rates: no tax line, no error. It is not a
// placeholder. But it is also not enough once tax *is* configured, because the rate depends on
// facts about the customer — their billing country, their VAT ID — and `billing.Pricer.Price` is
// not given a customer. So the resolving pricer is built per customer and wrapped in an issuer,
// which is what this method is for.
//
// A caller that issues an invoice for a known customer should use this instead of `s.issuer`. The
// wire-up is reported rather than applied here because the shared issuer is constructed in
// server.go and consumed by four other files (checkout, orders, the manual invoice and the renewal
// worker); changing all of them is one change, and it is not this file's.
//
// It returns the no-tax issuer when tax is switched off, so a caller never has to branch on nil.
func (s *Server) IssuerFor(ctx context.Context, customerID string) (*billing.Issuer, error) {
	pricer, err := tax.NewRules(s.pool).PricerForCustomer(ctx, customerID, s.now())
	if err != nil {
		return nil, err
	}
	if pricer == nil {
		return billing.NewIssuer(billing.FixedTaxPricer{Policy: "no-tax", Jurisdiction: "none"}), nil
	}
	return billing.NewIssuer(pricer), nil
}

// ------------------------------------------------------------------ shapes --

func nexusJSON(n tax.Nexus) map[string]any {
	out := map[string]any{
		"id":           n.ID,
		"country_code": n.CountryCode,
		"region_code":  n.RegionCode,
		"jurisdiction": n.Jurisdiction(),
		"note":         n.Note,
		"enabled":      n.Enabled,
		"created_at":   n.CreatedAt.UTC(),
		"updated_at":   n.UpdatedAt.UTC(),
	}
	if n.CreatedBy != "" {
		out["created_by"] = n.CreatedBy
	}
	return out
}

func validationJSON(v tax.VATValidation) map[string]any {
	out := map[string]any{
		"country_code": v.CountryCode,
		"vat_id":       v.VATID,
		"status":       v.Status,
		"checked_at":   v.CheckedAt.UTC(),
		"cached":       v.Cached,
	}
	if v.TraderName != "" {
		out["trader_name"] = v.TraderName
	}
	if v.ServiceURL != "" {
		out["service_url"] = v.ServiceURL
	}
	if v.Detail != "" {
		out["detail"] = v.Detail
	}
	return out
}

func decisionJSON(d tax.Decision) map[string]any {
	out := map[string]any{
		"kind":         d.Kind,
		"policy":       d.Policy,
		"jurisdiction": d.Jurisdiction,
		"rate":         d.Rate,
		"inclusive":    d.Inclusive,
		"charges":      d.Charges(),
	}
	if d.Reason != "" {
		out["reason"] = d.Reason
	}
	if d.Validation != nil {
		out["validation"] = validationJSON(*d.Validation)
	}
	return out
}

func refusalJSON(refusal *tax.Refusal) map[string]any {
	out := map[string]any{
		"code":   refusal.Code,
		"detail": refusal.Detail,
	}
	if len(refusal.Missing) > 0 {
		out["missing"] = refusal.Missing
	}
	return out
}

// ----------------------------------------------------------------- helpers --

// actingUserID is the signed-in staff user, or empty when the request has no identity.
func actingUserID(r *http.Request) string {
	if user, ok := userFrom(r.Context()); ok {
		return user.ID
	}
	return ""
}

// countryUpper normalises a country query or body value. It is lenient on purpose: a storefront
// passing "de" is a client spelling, not a different country.
func countryUpper(raw string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
}

func classOrDefault(class string) string {
	if class == "" {
		return "standard"
	}
	return class
}

func stringInSlice(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
