package openapi

// The catalogue-and-pricing descriptions: duplicating a product and ordering its plans.
//
// Two things are worth stating in the document rather than leaving to a reader of the handler:
//
//   - A duplicated product is a *draft*. The copy is hidden until an operator publishes it, so
//     "duplicate" cannot put a second listing in front of visitors at a price nobody reviewed.
//
//   - A plan order is presentation, not price. It is the only field of a price version this API
//     will UPDATE, and it takes the complete order rather than a single move, because a partial
//     list would leave the unnamed plans wherever they happened to be.
//
// The response schemas are inline, like the other domain files: the component list belongs to
// openapi.go, and a reference to an undefined component resolves to nothing.

func init() { RegisterDescriptions(catalogPlansOperations) }

// planSchema is one plan (one immutable price version) as the panel renders it.
func planSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"id", "version", "name", "currency", "amount", "setup", "interval", "recurring", "sort_order",
		},
		"properties": map[string]any{
			"id":      map[string]any{"type": "string", "format": "uuid"},
			"version": map[string]any{"type": "integer"},
			"name":    map[string]any{"type": "string"},
			"currency": map[string]any{
				"type": "string", "pattern": "^[A-Z0-9]{2,12}$",
			},
			"amount": AmountRef(),
			"setup":  AmountRef(),
			"interval": map[string]any{
				"type": "string",
				"enum": []string{"monthly", "quarterly", "semiannual", "annual", "one_time", "free"},
				"description": "`one_time` is charged once and `free` costs nothing. Neither creates a " +
					"subscription, so neither can renew. Arbitrary N-day periods are not supported.",
			},
			"interval_label": map[string]any{
				"type":        "string",
				"description": "The customer-facing wording (\"per month\", \"one-time\", \"free\").",
			},
			"recurring": map[string]any{
				"type":        "boolean",
				"description": "False for `one_time` and `free`. A false value means no subscription exists behind the purchase.",
			},
			"sort_order":       map[string]any{"type": "integer", "description": "Display position among the product's plans."},
			"anchor_day":       map[string]any{"type": "integer"},
			"anchor_timezone":  map[string]any{"type": "string"},
			"provision_plan":   map[string]any{"type": "string"},
			"provision_region": map[string]any{"type": "string"},
			"created_at":       map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// productSchema is the staff shape of a product. It is repeated here rather than referenced
// because the catalog's own entries predate this file and live in the core map.
func catalogProductSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "format": "uuid"},
			"slug":   map[string]any{"type": "string"},
			"name":   map[string]any{"type": "string"},
			"hidden": map[string]any{"type": "boolean"},
			"description": map[string]any{
				"type": "string",
			},
			"description_format": map[string]any{
				"type": "string",
				"enum": []string{"plain", "html"},
				"description": "`html` descriptions are sanitised on write against an allowlist: no " +
					"script, style, event handler or javascript: URL survives. `plain` is rendered " +
					"as text.",
			},
			"prices": map[string]any{"type": "array", "items": planSchema()},
			"price_versions": map[string]any{
				"type":        "array",
				"items":       planSchema(),
				"description": "Alias of `prices`, kept for clients that predate the rename.",
			},
		},
	}
}

func catalogPlansOperations() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/staff/catalog/products/{id}/duplicate": {
			Summary: "Duplicate a product",
			Description: "Copies the product, its price versions and its config options (including each " +
				"option's price per version) in one transaction, and writes one audit row naming the " +
				"product it was copied from. The name and slug are disambiguated (`-copy`, `-copy-2`, …) " +
				"so the copy can never collide with its source. The copy is hidden: duplicating a product " +
				"produces a draft, and publishing it stays a deliberate act. Price versions keep their " +
				"version numbers, and the copied rows are new rows, so no invoice or subscription that " +
				"points at the source is redirected.",
			Responses: []Response{
				{Status: 201, Description: "The copy", Schema: catalogProductSchema()},
				{Status: 404, Description: "No such product", Schema: ProblemRef()},
				{Status: 409, Description: "The disambiguated slug could not be allocated", Schema: ProblemRef()},
				{Status: 422, Description: "The copy would not be valid", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/catalog/products/{id}/plans/order": {
			Summary: "Set the order of a product's plans",
			Description: "Writes `price_versions.sort_order`, which is the only column of a price version " +
				"this API will UPDATE: reordering changes no amount and no issued document, while an " +
				"amount change is still a new version. The body must name every plan of the product " +
				"exactly once — a partial list is refused rather than leaving the unnamed plans wherever " +
				"they happened to be. The storefront and the product page both read the order from this " +
				"column.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"plan_ids"},
				"properties": map[string]any{
					"plan_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "format": "uuid"},
						"description": "Every price version of the product, in display order.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The plans in their new order", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": planSchema()},
					},
				}},
				{Status: 404, Description: "No such product, or it has no plans", Schema: ProblemRef()},
				{Status: 422, Description: "The list does not name every plan exactly once", Schema: ProblemRef()},
			},
		},
	}
}
