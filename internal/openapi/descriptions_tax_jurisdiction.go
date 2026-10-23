package openapi

// The tax-jurisdiction descriptions: the rules screen, the nexus declarations, the VAT-ID checks,
// the decision preview, and the public disclosure.
//
// These exist because the routes exist — `TestEveryRouteIsDescribed` fails otherwise — but the prose
// carries three facts a generated document could not state, and each one is a way a client author
// gets this surface wrong:
//
//   - The *decision* is not a rate. `kind` says which rule applied, and `reverse_charge` and
//     `no_nexus` both mean "charge nothing" for a reason that a zero rate would misrepresent.
//   - A refusal is an answer, not a failure. The preview returns 200 with `outcome: "refused"` and a
//     stable `code`, because the engine declining to decide is the information the operator asked
//     for. The same refusal read through the cart is a 422 with that code.
//   - The public disclosure names no customer and never changes state. It exists so a storefront
//     can print "incl. 19% VAT" without deriving a percentage from a rational in the browser.

func init() { RegisterDescriptions(func() map[string]Operation { return taxJurisdictionOperations }) }

// taxJurisdictionConfigSchema is the five settings the rules interpret.
func taxJurisdictionConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"seller_country": map[string]any{
				"type": "string",
				"description": "Where the installation is established. Required before reverse charge " +
					"can be decided: whether a supply is cross-border is the first question the rule asks.",
			},
			"reverse_charge": map[string]any{"type": "boolean", "description": "Enables the EU B2B rule."},
			"nexus_enforced": map[string]any{"type": "boolean", "description": "Enables the subnational sales-tax rule."},
			"vat_validation_url": map[string]any{
				"type":        "string",
				"description": "The service a VAT ID is checked against. Empty means no service, which refuses reverse charge rather than guessing.",
			},
			"vat_validation_ttl_hours": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 8760,
				"description": "How long a check is trusted. A cart is re-priced on every view.",
			},
		},
	}
}

// taxNexusSchema is one declaration.
func taxNexusSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "country_code", "jurisdiction", "enabled"},
		"properties": map[string]any{
			"id":           map[string]any{"type": "string", "format": "uuid"},
			"country_code": map[string]any{"type": "string"},
			"region_code":  map[string]any{"type": "string", "description": "Empty means the whole country."},
			"jurisdiction": map[string]any{"type": "string", "description": "`US-CA`, or `US` for a national registration."},
			"note":         map[string]any{"type": "string", "description": "The basis for the declaration. An auditor will ask."},
			"enabled":      map[string]any{"type": "boolean"},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"updated_at":   map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// taxVATValidationSchema is one check of one identifier.
func taxVATValidationSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"country_code", "vat_id", "status", "checked_at"},
		"properties": map[string]any{
			"country_code": map[string]any{"type": "string"},
			"vat_id":       map[string]any{"type": "string", "description": "Normalised: upper case, separators removed, country prefix stripped."},
			"status": map[string]any{
				"type": "string", "enum": []string{"valid", "invalid", "unavailable"},
				"description": "`invalid` is the service saying no; `unavailable` is nobody answering. " +
					"Only `valid` grants reverse charge.",
			},
			"trader_name": map[string]any{"type": "string"},
			"service_url": map[string]any{"type": "string"},
			"detail":      map[string]any{"type": "string"},
			"checked_at":  map[string]any{"type": "string", "format": "date-time"},
			"cached":      map[string]any{"type": "boolean"},
		},
	}
}

// taxDecisionSchema is the engine's conclusion about one supply.
func taxDecisionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "policy", "charges"},
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"rate", "reverse_charge", "no_nexus", "staff_exempt", "no_rate", "no_tax"},
				"description": "`reverse_charge` and `no_nexus` are decisions to charge nothing, not zero rates: " +
					"the reason is recorded and the jurisdiction is named.",
			},
			"policy":       map[string]any{"type": "string", "description": "The rule that decided, recorded in the invoice's tax evidence."},
			"jurisdiction": map[string]any{"type": "string", "description": "`DE/standard`, `US-CA/standard`."},
			"rate":         map[string]any{"type": "string", "description": "An exact rational (`0.19`, `1/5`); empty when nothing is charged."},
			"inclusive":    map[string]any{"type": "boolean"},
			"charges":      map[string]any{"type": "boolean"},
			"reason":       map[string]any{"type": "string"},
			"validation":   taxVATValidationSchema(),
		},
	}
}

// taxRefusalSchema is why the engine would not decide.
func taxRefusalSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "detail"},
		"properties": map[string]any{
			"code": map[string]any{
				"type": "string",
				"enum": []string{"seller_country_missing", "vat_id_unverified", "vat_id_validation_failed"},
			},
			"detail":  map[string]any{"type": "string"},
			"missing": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}

var taxJurisdictionOperations = map[string]Operation{
	"GET /api/v1/staff/tax-jurisdiction": {
		Summary: "The jurisdiction rules and their consequences",
		Description: "The five rules settings, every nexus declaration, the EU membership list the reverse-charge " +
			"rule applies to, and the configuration contradictions the engine cannot resolve on its own. " +
			"`nexus_gaps` names the jurisdictions that have an open rate but no declaration, which is the most " +
			"expensive kind of quiet misconfiguration: a number is configured and tax is not collected.",
		Responses: []Response{
			{Status: 200, Description: "The rules configuration", Schema: map[string]any{
				"type":     "object",
				"required": []string{"config", "nexus", "eu_member_states"},
				"properties": map[string]any{
					"config":           taxJurisdictionConfigSchema(),
					"nexus":            map[string]any{"type": "array", "items": taxNexusSchema()},
					"eu_member_states": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"nexus_gaps":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"warnings":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			}},
			{Status: 403, Description: "The caller does not hold tax:write", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/tax-jurisdiction/nexus": {
		Summary: "Declare sales-tax nexus",
		Description: "Declaring any jurisdiction in a country makes that country's tax nexus-governed: once it is " +
			"enforced, tax is collected only where a declaration matches. That is deliberately an explicit act, per " +
			"country, because declaring one state must not silently stop collection in the other forty-nine. " +
			"Recorded in the audit log with the note, in the same transaction.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"country_code"},
			"properties": map[string]any{
				"country_code": map[string]any{"type": "string", "description": "ISO 3166-1 alpha-2, upper case."},
				"region_code":  map[string]any{"type": "string", "description": "A compact subdivision code; empty for a national registration."},
				"note":         map[string]any{"type": "string", "description": "The basis for the declaration."},
				"enabled":      map[string]any{"type": "boolean"},
			},
		},
		Responses: []Response{
			{Status: 201, Description: "The declaration", Schema: taxNexusSchema()},
			{Status: 409, Description: "That jurisdiction is already declared", Schema: ProblemRef()},
			{Status: 422, Description: "The country or region code is not acceptable", Schema: ProblemRef()},
		},
	},
	"DELETE /api/v1/staff/tax-jurisdiction/nexus/{id}": {
		Summary: "Remove a nexus declaration",
		Description: "Nothing points at the declaration: an invoice records the decision the rules made, not the row, " +
			"so removing a declaration that was wrong does not rewrite a document. The audit row keeps the fact that " +
			"it existed.",
		Responses: []Response{
			{Status: 204, Description: "Removed"},
			{Status: 404, Description: "No such declaration", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/tax-jurisdiction/validate-vat": {
		Summary: "Check a VAT ID against the configured service",
		Description: "Asks the configured service and records the answer, whichever it is: `valid`, `invalid` (the " +
			"service said no) or `unavailable` (nobody answered). The record is append-only, because a reverse-charge " +
			"treatment on an issued invoice must be explainable by the check that justified it. A malformed identifier " +
			"is refused before the service is called.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"country", "vat_id"},
			"properties": map[string]any{
				"country":     map[string]any{"type": "string"},
				"vat_id":      map[string]any{"type": "string"},
				"customer_id": map[string]any{"type": "string", "format": "uuid", "description": "Records the check against the customer."},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The answer, recorded", Schema: taxVATValidationSchema()},
			{Status: 422, Description: "The identifier does not match the country's published format", Schema: ProblemRef()},
			{Status: 502, Description: "The validation service did not answer; the answer is recorded as unavailable", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/tax-jurisdiction/validations": {
		Summary: "The check history for a VAT ID",
		Description: "Newest first, at most twenty. The history is what explains why a sale was or was not reverse-charged " +
			"on a given day.",
		Responses: []Response{
			{Status: 200, Description: "The recorded checks", Schema: map[string]any{
				"type":     "object",
				"required": []string{"data"},
				"properties": map[string]any{
					"data": map[string]any{"type": "array", "items": taxVATValidationSchema()},
				},
			}},
			{Status: 422, Description: "The identifier does not match the country's published format", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/tax-jurisdiction/decide": {
		Summary: "Explain what the rules would do with a sale",
		Description: "A dry run: it decides about a hypothetical supply and records the decision in the audit log, " +
			"charging nothing and creating no invoice. It is a POST because deciding a VAT-ID supply asks the validation " +
			"service and appends the answer to the history. A refusal is an answer and arrives with 200 and " +
			"`outcome: \"refused\"`; the same refusal read through the cart is a 422 carrying the same code.",
		RequestSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"customer_id": map[string]any{"type": "string", "format": "uuid", "description": "When set, the destination and VAT ID come from the billing profile."},
				"country":     map[string]any{"type": "string"},
				"region":      map[string]any{"type": "string"},
				"vat_id":      map[string]any{"type": "string"},
				"tax_class":   map[string]any{"type": "string"},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The decision, or the refusal", Schema: map[string]any{
				"type":     "object",
				"required": []string{"outcome"},
				"properties": map[string]any{
					"outcome":  map[string]any{"type": "string", "enum": []string{"decided", "refused"}},
					"decision": taxDecisionSchema(),
					"refusal":  taxRefusalSchema(),
					"supply":   map[string]any{"type": "object"},
				},
			}},
		},
	},
	"GET /api/v1/tax/disclosure": {
		Summary: "How a price is displayed with tax, for a destination",
		Description: "Public, because it explains a price that is already public, and it names no customer. The " +
			"`country` is the destination the visitor has told the shop; when it is absent the installation's own " +
			"country is used, which is the only market whose price can be shown before anybody has said where they are. " +
			"`label` is the sentence a page may print (`incl. 19% VAT`, `plus 7.25% sales tax`, `VAT reverse charge`), " +
			"computed from the exact rational so a renderer never derives a percentage itself.",
		Responses: []Response{
			{Status: 200, Description: "The disclosure", Schema: map[string]any{
				"type":     "object",
				"required": []string{"tax_enabled", "configured", "label"},
				"properties": map[string]any{
					"tax_enabled": map[string]any{"type": "boolean"},
					"configured":  map[string]any{"type": "boolean"},
					"country":     map[string]any{"type": "string"},
					"region":      map[string]any{"type": "string"},
					"tax_class":   map[string]any{"type": "string"},
					"outcome":     map[string]any{"type": "string", "enum": []string{"decided", "refused"}},
					"label":       map[string]any{"type": "string"},
					"decision":    taxDecisionSchema(),
					"refusal":     taxRefusalSchema(),
				},
			}},
		},
	},
}
