package openapi

// The catalog's merchandising write: stock, the per-customer limit, the quantity policy and the
// featured flag.
//
// It is documented separately from the product PUT because the split is the point. The PUT
// replaces the whole metadata record, and its other callers (the description editor, the
// hide/unhide toggle) know nothing about stock — so folding `stock` into it would mean every
// description save silently restocked a sold-out product. One narrow endpoint that writes exactly
// these four fields is what lets the panel offer them.
//
// Two properties a client has to be able to rely on, and the document states both:
//
//   - **`stock: null` means unlimited and `stock: 0` means sold out.** They are different answers
//     and a visitor is told which one it is; a document that called zero "no stock configured"
//     would make the storefront's "Sold out" look like a bug.
//   - **`allow_quantity: ""` leaves the stored policy alone.** Everything else is written as sent,
//     including a null stock, because clearing stock back to unlimited has to be expressible.

func init() { RegisterDescriptions(catalogMerchandisingOperations) }

func catalogMerchandisingOperations() map[string]Operation {
	return map[string]Operation{
		"PUT /api/v1/staff/catalog/products/{id}/merchandising": {
			Summary: "Set a product's merchandising fields",
			Description: "Writes stock, the per-customer limit, the quantity policy and the featured flag in one " +
				"request. `stock: null` is unlimited and `stock: 0` is sold out; a negative count is refused with " +
				"`invalid_input` rather than stored. `allow_quantity` is one of `disabled` (one per order), " +
				"`separated` (quantity N is N independent services) or `combined` (quantity N is one service with " +
				"N units); an empty value leaves the stored policy untouched. `featured` promotes the product on " +
				"the storefront — it is not a visibility grant, because every public read applies the " +
				"hidden/category/priced predicates first, so a featured hidden product stays hidden. The write is " +
				"audited in the same transaction as the change.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"stock": map[string]any{
						"type":        []string{"integer", "null"},
						"minimum":     0,
						"description": "Units available. Null is unlimited; 0 marks the product sold out.",
					},
					"per_user_limit": map[string]any{
						"type":        []string{"integer", "null"},
						"minimum":     1,
						"description": "How many one customer may hold. Null is no limit.",
					},
					"allow_quantity": map[string]any{
						"type":        "string",
						"enum":        []string{"", "disabled", "separated", "combined"},
						"description": "The quantity policy; empty leaves the stored one alone.",
					},
					"featured": map[string]any{
						"type":        "boolean",
						"description": "Show the product in the storefront's featured row.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The product after the change"},
				{Status: 404, Description: "No such product", Schema: ProblemRef()},
				{Status: 422, Description: "A stock, limit or policy the catalog refuses (`invalid_input`)", Schema: ProblemRef()},
			},
		},
	}
}
