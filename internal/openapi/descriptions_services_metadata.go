package openapi

// Service metadata descriptions (E2.6: SVC-02, SVC-03, SVC-04, SVC-07, SVC-08).
//
// The prose lives in its own file for the same reason the route table does: two people adding
// two domains must not edit one block. The parity test compares this map against the router in
// both directions, so a route added without an entry here fails, and an entry whose route is
// gone fails too.
//
// Four conventions apply to every route in this file:
//
//   - A customer's route resolves the customer from the session and scopes in SQL, so a
//     substituted service identifier returns 404 rather than 403.
//   - `label` and `description` are what a human wrote, and both may be empty. The fields to
//     display are `effective_label` and `effective_description`, which fall back to the
//     product's name and to a description derived from the product and the selected options,
//     so a service is never blank.
//   - An absent field means "leave it alone"; an empty string clears it. That is what keeps a
//     customer's rename from blanking the description an operator wrote.
//   - A custom property is a definition plus a value. The definition (its label, its order,
//     whether it prints on an invoice, whether the customer sees it) is installation
//     configuration; the value is a short sanitised string on the service.

func init() { RegisterDescriptions(serviceMetadataOperations) }

// servicePropertySchema is one property with its definition attached.
func servicePropertySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key", "label", "value"},
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9_]{0,63}$`},
			"label": map[string]any{"type": "string", "description": "What a human reads. Never the key."},
			"description": map[string]any{
				"type": "string", "description": "What the property is for, shown on the staff screen.",
			},
			"value": map[string]any{
				"type": "string", "maxLength": 200,
				"description": "Sanitised on write: one line, no control characters, capped at 200 characters.",
			},
			"show_on_invoice": map[string]any{
				"type": "boolean",
				"description": "Whether the definition lets this property print on an invoice or a credit " +
					"note. The decision belongs to the definition, not to a global setting: a rack number " +
					"belongs on the document and an internal asset tag does not.",
			},
			"customer_visible": map[string]any{
				"type":        "boolean",
				"description": "Whether the customer sees the value on their own service page. Independent of show_on_invoice: an internal note may be printed for the operator and still be invisible to the customer.",
			},
			"sort_order": map[string]any{"type": "integer"},
		},
	}
}

// servicePropertyDefinitionSchema is a configured property definition.
func servicePropertyDefinitionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key", "label", "show_on_invoice", "customer_visible"},
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9_]{0,63}$`},
			"label": map[string]any{"type": "string", "maxLength": 120},
			"description": map[string]any{
				"type": "string", "maxLength": 500,
			},
			"show_on_invoice": map[string]any{
				"type": "boolean",
				"description": "Print this property on an invoice or a credit note when the service carries a " +
					"value for it. Nothing is printed by default.",
			},
			"customer_visible": map[string]any{
				"type":        "boolean",
				"description": "Show the value on the customer's own service page.",
			},
			"sort_order": map[string]any{"type": "integer"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// servicePlanSchema is the plan a service renews at: the immutable catalog row, with the price
// as integer minor units and an explicit currency.
func servicePlanSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"price_version_id", "name", "interval", "currency", "amount_minor"},
		"properties": map[string]any{
			"price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"name":             map[string]any{"type": "string"},
			// The billing period, from the one schema the catalog uses: an enum here went
			// stale the moment a day-count period was added, and a document that tells an
			// integrator `every_45_days` is invalid is worse than one that says nothing.
			"interval":     intervalSchema(),
			"currency":     map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"amount_minor": map[string]any{"type": "integer", "format": "int64"},
			"setup_minor":  map[string]any{"type": "integer", "format": "int64"},
			"amount":       AmountRef(),
		},
	}
}

// serviceMetadataSchema is the answer to "what is this service?".
func serviceMetadataSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"service_id", "label", "effective_label", "effective_description", "plan"},
		"properties": map[string]any{
			"service_id": map[string]any{"type": "string", "format": "uuid"},
			"status":     map[string]any{"type": "string"},
			"customer": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "format": "uuid"},
					"email": map[string]any{"type": "string", "format": "email"},
				},
			},
			"product": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string", "format": "uuid"},
					"name": map[string]any{"type": "string"},
				},
			},
			"plan":     servicePlanSchema(),
			"quantity": map[string]any{"type": "integer", "minimum": 1},
			"order_id": map[string]any{
				"type": "string", "format": "uuid",
				"description": "The order the service was sold on. Absent for a service provisioned from a " +
					"manual invoice, which has no order. Staff only.",
			},
			"label": map[string]any{
				"type": "string", "maxLength": 120,
				"description": "What the customer or an operator wrote. Empty when nobody has named the service.",
			},
			"description": map[string]any{
				"type": "string", "maxLength": 2000,
				"description": "What a human wrote. Empty means nobody wrote one; see effective_description.",
			},
			"effective_label": map[string]any{
				"type": "string",
				"description": "What to display: `label` when it is set, the product's name (with the " +
					"quantity when there is more than one) otherwise. Never empty.",
			},
			"effective_description": map[string]any{
				"type": "string",
				"description": "What to display: `description` when it is set, `derived_description` " +
					"otherwise. Never empty, which is what stops a service page from being blank.",
			},
			"derived_description": map[string]any{
				"type": "string",
				"description": "Built from the product, the plan, the selected config options and the billing " +
					"cycle. It describes facts, never prices: the price is on the invoice, and a description " +
					"that repeated it would go stale the first time the catalog changed.",
			},
			"custom_properties": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "The values keyed by property key, filtered to what this audience may see. A " +
					"customer never receives a value whose definition is not customer_visible.",
			},
			"properties": map[string]any{
				"type": "array", "items": servicePropertySchema(),
				"description": "The same values with their definitions attached, in definition order.",
			},
			"config": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"config_option_id": map[string]any{"type": "string", "format": "uuid"},
						"name":             map[string]any{"type": "string"},
						"type":             map[string]any{"type": "string"},
						"value":            map[string]any{"type": "string"},
						"amount_minor":     map[string]any{"type": "integer", "format": "int64"},
						"setup_minor":      map[string]any{"type": "integer", "format": "int64"},
					},
				},
				"description": "The stored option selection, which is what the description is derived from.",
			},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// serviceMetadataUpdateSchema is the body of a metadata edit.
func serviceMetadataUpdateSchema(staff bool) map[string]any {
	properties := map[string]any{
		"label": map[string]any{
			"type": "string", "maxLength": 120,
			"description": "Replaces the label. An empty string clears it, which restores the product's name.",
		},
		"description": map[string]any{
			"type": "string", "maxLength": 2000,
			"description": "Replaces the description. An empty string clears it, which restores the derived " +
				"description. Newlines and tabs are allowed; other control characters are a 400.",
		},
	}
	if staff {
		properties["custom_properties"] = map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string", "maxLength": 200},
			"description": "A partial edit of the property values: a key that is named is set (or, with an " +
				"empty string, unset) and a key that is not named is left alone. A key with no definition " +
				"is a 400: a value nobody can label is a value nobody can explain.",
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
}

// serviceMetadataOperations is the prose for the service-metadata routes.
func serviceMetadataOperations() map[string]Operation {
	return map[string]Operation{

		// ------------------------------------------------------------ customer --

		"GET /api/v1/portal/services/{id}/metadata": {
			Summary: "What this service is, for the customer who owns it",
			Description: "The label, the description to display, the plan, the quantity and the properties the " +
				"operator made visible to the customer. `effective_label` and `effective_description` are " +
				"never empty: a service that nobody has named shows the product's name, and one nobody has " +
				"described shows a description derived from the product and the selected config options. " +
				"The service is resolved through the caller's customer account in SQL, so another " +
				"customer's identifier returns 404 — the same answer as an id that does not exist.",
			Responses: []Response{
				{Status: 200, Description: "The service's metadata", Schema: serviceMetadataSchema()},
				{Status: 404, Description: "No such service, or it belongs to another customer",
					Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/portal/services/{id}/metadata": {
			Summary: "Rename or describe your own service",
			Description: "The customer's own label and description, and nothing else: a body that names " +
				"`custom_properties` is refused with 403 rather than silently ignored, because the property " +
				"record is the operator's account of the installation. An absent field is left alone, so " +
				"renaming a service does not blank the description an operator wrote. A label is collapsed " +
				"to one line; a description keeps its newlines. Another customer's identifier is a 404, and " +
				"nothing is written.",
			RequestSchema: serviceMetadataUpdateSchema(false),
			Responses: []Response{
				{Status: 200, Description: "The updated metadata", Schema: serviceMetadataSchema()},
				{Status: 400, Description: "The label or the description is not acceptable", Schema: ProblemRef()},
				{Status: 403, Description: "The body tried to set a custom property", Schema: ProblemRef()},
				{Status: 404, Description: "No such service, or it belongs to another customer",
					Schema: ProblemRef()},
			},
		},

		// --------------------------------------------------------------- staff --

		"GET /api/v1/staff/services/{id}/metadata": {
			Summary: "What this service is, for a support agent",
			Description: "Everything a support agent needs to answer \"what is this service?\" without opening " +
				"four screens: the customer, the product, the plan and its price (integer minor units plus " +
				"an explicit currency), the quantity, the order it was sold on, the label, the description " +
				"and every custom property including the ones the customer cannot see. The plan and the " +
				"price come through the subscription's price version, which is the immutable catalog row the " +
				"service renews at, so the answer cannot disagree with the next renewal invoice.",
			Responses: []Response{
				{Status: 200, Description: "The service's metadata", Schema: serviceMetadataSchema()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/services/{id}/metadata": {
			Summary: "Edit a service's label, description and custom properties",
			Description: "An absent field is left alone; an empty string clears it. `custom_properties` is a " +
				"partial edit of the values, applied under a row lock so two operators cannot merge into one " +
				"another, and validated in full before anything is written: a request naming one defined " +
				"property and one undefined key changes nothing. Every value is sanitised on write — one " +
				"line, no control characters, at most 200 characters — because a value that reaches a " +
				"document is printed, and the database refuses a control character even if this path is " +
				"bypassed.",
			RequestSchema: serviceMetadataUpdateSchema(true),
			Responses: []Response{
				{Status: 200, Description: "The updated metadata", Schema: serviceMetadataSchema()},
				{Status: 400, Description: "An invalid label, description, property key or property value",
					Schema: ProblemRef()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/service-properties": {
			Summary: "The configured service properties",
			Description: "The definitions an operator has created, in display order. A definition supplies the " +
				"label a human reads, the order it prints in, whether it may appear on an invoice " +
				"(`show_on_invoice`) and whether the customer may see it (`customer_visible`). Nothing is " +
				"defined on a fresh installation, which is why every service's `custom_properties` starts " +
				"empty.",
			Responses: []Response{
				{Status: 200, Description: "The definitions",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"properties"},
						"properties": map[string]any{
							"properties": map[string]any{
								"type": "array", "items": servicePropertyDefinitionSchema(),
							},
						},
					}},
			},
		},
		"PUT /api/v1/staff/service-properties/{key}": {
			Summary: "Create or edit a service property definition",
			Description: "An upsert on the key, because the key is the identity: every service's " +
				"`custom_properties` object is written against it, so re-keying a definition would orphan " +
				"every value. Renaming the label is the supported edit. `show_on_invoice` is the mechanism " +
				"behind SVC-08's \"show the properties on the invoice\": a property is printed exactly when " +
				"its definition says so and the service carries a value, and `customer_visible` is a " +
				"separate question with a separate answer.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"label"},
				"properties": map[string]any{
					"label":            map[string]any{"type": "string", "maxLength": 120},
					"description":      map[string]any{"type": "string", "maxLength": 500},
					"show_on_invoice":  map[string]any{"type": "boolean"},
					"customer_visible": map[string]any{"type": "boolean"},
					"sort_order":       map[string]any{"type": "integer"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The saved definition", Schema: servicePropertyDefinitionSchema()},
				{Status: 400, Description: "The key or the label is not acceptable", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/service-properties/{key}": {
			Summary: "Delete a service property definition",
			Description: "Refused with 409 `property_in_use` while any service still carries a value for the " +
				"key: deleting the definition would leave a value nobody can label or print. Clear the " +
				"values first, or rename the label instead of re-keying the definition.",
			Responses: []Response{
				{Status: 200, Description: "The definition is deleted"},
				{Status: 404, Description: "No such definition", Schema: ProblemRef()},
				{Status: 409, Description: "A service still carries this property", Schema: ProblemRef()},
			},
		},
	}
}
