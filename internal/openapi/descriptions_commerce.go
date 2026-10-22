package openapi

// The commerce descriptions: coupons, tax rates, currencies and the cart.
//
// The prose here is the part a generated document cannot supply. Each entry states the
// invariant the handler enforces, because "create a coupon" is a summary and "a retired
// coupon keeps its history because the invoices it discounted reference it" is the thing a
// client author needs to know before they build the form.
//
// The response schemas are declared inline rather than as `#/components/schemas`
// references: the component list is built by `openapi.go`, which this domain does not own,
// and a reference to a component nobody defines would be a document that resolves to
// nothing. Inline shapes are honest about what this file can assert.

func init() { RegisterDescriptions(commerceOperations) }

// couponSchema is the promotion shape. A percentage is a string because it is an exact
// rational; a fixed amount is minor units plus the currency it is denominated in.
func couponSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "code", "kind", "applies_to", "enabled"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"code":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"kind":        map[string]any{"type": "string", "enum": []string{"percentage", "fixed"}},
			"percentage": map[string]any{
				"type":        "string",
				"description": "Exact decimal percentage for a percentage coupon; never a float.",
			},
			"amount_minor": map[string]any{
				"type": "integer", "format": "int64",
				"description": "Fixed discount in minor units of `currency`.",
			},
			"currency":          map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"applies_to":        map[string]any{"type": "string", "enum": []string{"price", "setup", "all"}},
			"max_uses":          map[string]any{"type": []string{"integer", "null"}},
			"max_uses_per_user": map[string]any{"type": []string{"integer", "null"}},
			"starts_at":         map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"expires_at":        map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"recurring_cycles":  map[string]any{"type": []string{"integer", "null"}},
			"enabled":           map[string]any{"type": "boolean"},
			"product_ids":       map[string]any{"type": "array", "items": map[string]any{"type": "string", "format": "uuid"}},
			"created_at":        map[string]any{"type": "string", "format": "date-time"},
			"updated_at":        map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// taxRateSchema is one dated jurisdiction rate.
func taxRateSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "country_code", "tax_class", "rate", "inclusive", "effective_from"},
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "format": "uuid"},
			"name": map[string]any{"type": "string"},
			"country_code": map[string]any{
				"type": "string", "description": "ISO 3166-1 alpha-2, or `all` for the wildcard fallback.",
			},
			"region_code":    map[string]any{"type": "string"},
			"tax_class":      map[string]any{"type": "string"},
			"rate":           map[string]any{"type": "string", "description": "Exact rational: \"0.19\", \"1/5\"."},
			"inclusive":      map[string]any{"type": "boolean"},
			"effective_from": map[string]any{"type": "string", "format": "date-time"},
			"effective_to":   map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"enabled":        map[string]any{"type": "boolean"},
			"jurisdiction":   map[string]any{"type": "string", "description": "`DE/standard`, `US-CA/standard` or `all/standard`."},
			"created_at":     map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// currencySchema is one configured currency: presentation only.
func currencySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "name", "scale", "is_default", "enabled"},
		"properties": map[string]any{
			"code":            map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"name":            map[string]any{"type": "string"},
			"scale":           map[string]any{"type": "integer"},
			"symbol":          map[string]any{"type": "string"},
			"symbol_position": map[string]any{"type": "string", "enum": []string{"prefix", "suffix"}},
			"group_separator": map[string]any{"type": "string"},
			"decimal_mark":    map[string]any{"type": "string"},
			"group_size":      map[string]any{"type": "integer"},
			"is_default":      map[string]any{"type": "boolean"},
			"enabled":         map[string]any{"type": "boolean"},
			"sort_order":      map[string]any{"type": "integer"},
			"example": map[string]any{
				"type": "string", "description": "123456 minor units rendered in the configured style.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// cartSchema is the selection: what was chosen, never what it costs.
func cartSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "currency", "items"},
		"properties": map[string]any{
			"id":       map[string]any{"type": "string", "format": "uuid"},
			"currency": map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"coupon_id": map[string]any{
				"type": []string{"string", "null"}, "format": "uuid",
			},
			"item_count":      map[string]any{"type": "integer"},
			"tos_accepted_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"id", "price_version_id", "quantity"},
					"properties": map[string]any{
						"id":               map[string]any{"type": "string", "format": "uuid"},
						"price_version_id": map[string]any{"type": "string", "format": "uuid"},
						"product_id":       map[string]any{"type": "string", "format": "uuid"},
						"product_name":     map[string]any{"type": "string"},
						"price_name":       map[string]any{"type": "string"},
						"quantity":         map[string]any{"type": "integer"},
						"config":           map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"label":            map[string]any{"type": "string"},
						"created_at":       map[string]any{"type": "string", "format": "date-time"},
					},
				},
			},
		},
	}
}

// cartTotalsSchema is the priced cart. Every money field is the minor/currency/scale triple.
func cartTotalsSchema() map[string]any {
	amount := AmountRef()
	return map[string]any{
		"type":     "object",
		"required": []string{"currency", "lines", "subtotal", "discount", "tax", "total"},
		"properties": map[string]any{
			"currency":   map[string]any{"type": "string"},
			"item_count": map[string]any{"type": "integer"},
			"subtotal":   amount,
			"discount":   amount,
			"tax":        amount,
			"total":      amount,
			"lines": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"item_id":          map[string]any{"type": "string", "format": "uuid"},
						"price_version_id": map[string]any{"type": "string", "format": "uuid"},
						"product_id":       map[string]any{"type": "string", "format": "uuid"},
						"description":      map[string]any{"type": "string"},
						"quantity":         map[string]any{"type": "integer"},
						"unit":             amount,
						"setup":            amount,
						"line":             amount,
						"setup_line":       amount,
						"discount":         amount,
						"net":              amount,
						"tax":              amount,
						"total":            amount,
						"tax_rate":         map[string]any{"type": "string"},
						"tax_evidence": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"jurisdiction":     map[string]any{"type": "string"},
								"rate":             map[string]any{"type": "string"},
								"inclusive":        map[string]any{"type": "boolean"},
								"exemption_reason": map[string]any{"type": "string"},
							},
						},
					},
				},
			},
			"coupon": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"coupon_id":        map[string]any{"type": "string", "format": "uuid"},
					"code":             map[string]any{"type": "string"},
					"applies_to":       map[string]any{"type": "string"},
					"discount":         amount,
					"recurring_cycles": map[string]any{"type": []string{"integer", "null"}},
				},
			},
			"coupon_problem": map[string]any{
				"type":        "string",
				"description": "Why an attached coupon is not reducing anything (expired, exhausted, not applicable).",
			},
		},
	}
}

// commerceOperations is the prose for the P1 commerce surface.
func commerceOperations() map[string]Operation {
	coupon := couponSchema()
	taxRate := taxRateSchema()
	currency := currencySchema()
	cart := cartSchema()
	totals := cartTotalsSchema()

	return map[string]Operation{
		// ------------------------------------------------------------- coupons --
		"GET /api/v1/staff/coupons": {
			Summary: "List promotions",
			Description: "Every coupon, retired ones included, newest first, each with the products it is " +
				"restricted to. Requires coupon:read:all: a promotion list is a list of every price the shop " +
				"has discounted.",
			Responses: []Response{
				{Status: 200, Description: "The coupons", Schema: PageSchema(coupon)},
			},
		},
		"GET /api/v1/staff/coupons/{id}": {
			Summary:     "One promotion",
			Description: "Includes `product_ids`, which is empty when the coupon applies to every product.",
			Responses: []Response{
				{Status: 200, Description: "The coupon", Schema: coupon},
				{Status: 404, Description: "No such coupon", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/coupons/{id}/redemptions": {
			Summary: "Who used a promotion, and what it cost",
			Description: "One row per recorded use, with the discount actually granted in the invoice's " +
				"currency. This is the report that answers \"what did this promotion cost\" without re-running " +
				"the pricing.",
			Responses: []Response{
				{Status: 200, Description: "The redemptions"},
				{Status: 404, Description: "No such coupon", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/coupons": {
			Summary: "Create a promotion",
			Description: "A percentage coupon carries `percentage` as an exact decimal string (\"10\", \"12.5\") " +
				"and no currency; a fixed coupon carries `amount_minor` plus the `currency` it is denominated " +
				"in. A float is never accepted, because 0.1 is not representable and a discount is money. " +
				"`applies_to` is price, setup or all; `recurring_cycles` is the first-N-cycles rule (null is " +
				"lifetime, 1 is the first invoice only).",
			Responses: []Response{
				{Status: 201, Description: "The coupon", Schema: coupon},
				{Status: 409, Description: "That code is already in use", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/coupons/{id}": {
			Summary: "Edit or retire a promotion",
			Description: "Retiring is `enabled: false`, never a delete: the invoices the coupon discounted " +
				"reference it, and an invoice whose discount cannot be explained is not a valid document. A " +
				"retired coupon refuses new redemptions and keeps its history.",
			Responses: []Response{
				{Status: 200, Description: "The coupon", Schema: coupon},
				{Status: 404, Description: "No such coupon", Schema: ProblemRef()},
				{Status: 409, Description: "That code is already in use", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- tax rates --
		"GET /api/v1/staff/tax-rates": {
			Summary: "List jurisdiction tax rates",
			Description: "Every rate, current and historic, newest effective date first. Rates are dated rows: " +
				"a rate change appends a new row instead of rewriting a filed period. `all` as a country code " +
				"is the wildcard fallback.",
			Responses: []Response{
				{Status: 200, Description: "The rates", Schema: PageSchema(taxRate)},
			},
		},
		"GET /api/v1/staff/tax-rates/{id}": {
			Summary:     "One tax rate",
			Description: "`jurisdiction` renders the row as `DE/standard` or `US-CA/standard` for evidence.",
			Responses: []Response{
				{Status: 200, Description: "The rate", Schema: taxRate},
				{Status: 404, Description: "No such rate", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/tax-rates": {
			Summary: "Append a tax rate",
			Description: "`rate` is an exact rational string (\"0.19\", \"19%\", \"1/5\"); a float is never " +
				"accepted. `effective_from` defaults to now and `effective_to` is null for an open-ended rate. " +
				"Only one open-ended enabled rate may exist per (country, region, class), so a change means " +
				"closing the old row first. `inclusive` defaults to the installation's configured tax mode.",
			Responses: []Response{
				{Status: 201, Description: "The rate", Schema: taxRate},
				{Status: 409, Description: "An open-ended rate for that jurisdiction already exists", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/tax-rates/{id}": {
			Summary: "Edit a rate's window or presentation",
			Description: "The arithmetic is refused once the rate has taken effect: changing a live rate would " +
				"rewrite what a past period was taxed at. Close the row and append a new one instead.",
			Responses: []Response{
				{Status: 200, Description: "The rate", Schema: taxRate},
				{Status: 404, Description: "No such rate", Schema: ProblemRef()},
				{Status: 409, Description: "The rate is in effect, or another open-ended rate exists", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/tax-rates/{id}": {
			Summary:     "Delete a rate that never took effect",
			Description: "Refused once the rate has applied: an issued invoice's evidence points at it.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 409, Description: "The rate has taken effect; disable it instead", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- currencies --
		"GET /api/v1/currencies": {
			Summary: "The currencies offered at checkout",
			Description: "Public, because the storefront's currency switcher renders before anyone signs in. " +
				"Only enabled currencies, default first. `example` renders 123456 minor units in the " +
				"configured style, so the switcher can preview the format. No exchange rate is implied: prices " +
				"are explicit per currency.",
			Responses: []Response{
				{Status: 200, Description: "The currencies", Schema: PageSchema(currency)},
			},
		},
		"GET /api/v1/staff/currencies": {
			Summary:     "List every configured currency",
			Description: "Disabled currencies included, because they keep the history their price rows and invoices reference.",
			Responses: []Response{
				{Status: 200, Description: "The currencies", Schema: PageSchema(currency)},
			},
		},
		"POST /api/v1/staff/currencies": {
			Summary: "Add a currency",
			Description: "Supplies presentation only — symbol, position, grouping, scale — because amounts stay " +
				"integer minor units. `scale` defaults to the ISO 4217 exponent for the code. Setting " +
				"`is_default` demotes the incumbent in the same transaction, so \"exactly one default\" holds at " +
				"every commit.",
			Responses: []Response{
				{Status: 201, Description: "The currency", Schema: currency},
				{Status: 409, Description: "That code already exists", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/currencies/{code}": {
			Summary: "Edit a currency's presentation or default state",
			Description: "The code and the scale are immutable: changing the exponent would reinterpret every " +
				"stored minor amount. The default currency cannot be disabled; making another currency the " +
				"default promotes it in the same transaction.",
			Responses: []Response{
				{Status: 200, Description: "The currency", Schema: currency},
				{Status: 404, Description: "No such currency", Schema: ProblemRef()},
				{Status: 409, Description: "The change would leave no default, or the scale was changed", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/currencies/{code}": {
			Summary: "Delete a currency that has never been used",
			Description: "Refused while any price version or invoice names it. The supported way to stop " +
				"offering a currency is `enabled: false`.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 409, Description: "Prices or invoices use it, or it is the default", Schema: ProblemRef()},
			},
		},

		// --------------------------------------------------------------- cart --
		"GET /api/v1/portal/cart": {
			Summary: "The caller's cart",
			Description: "Created empty on first read, with the default currency. The customer is resolved " +
				"from the session, never from the URL: a cart id is not a capability. A cart's currency is " +
				"fixed once it holds an item, because there is no exchange rate to convert with.",
			Responses: []Response{
				{Status: 200, Description: "The cart", Schema: cart},
			},
		},
		"GET /api/v1/portal/cart/totals": {
			Summary: "Price the cart from the catalog",
			Description: "Every number is read from `price_versions` at the moment of the call, so a price " +
				"change while the cart sat open is a non-event. Tax is applied when the installation has it " +
				"enabled and the customer has a billing country; otherwise the cart is quoted net. A coupon " +
				"that no longer applies is reported in `coupon_problem` rather than silently dropped.",
			Responses: []Response{
				{Status: 200, Description: "The priced cart", Schema: totals},
			},
		},
		"POST /api/v1/portal/cart/items": {
			Summary: "Add a selection to the cart",
			Description: "The price version must exist and belong to a product whose `allow_quantity` policy " +
				"permits the quantity: `disabled` refuses more than one, `separated`/`combined` allow up to " +
				"1000. An identical line (same price version and config) merges into one line with a larger " +
				"quantity.",
			Responses: []Response{
				{Status: 201, Description: "The cart", Schema: cart},
				{Status: 404, Description: "No such price", Schema: ProblemRef()},
				{Status: 422, Description: "The quantity is not allowed, or the cart is in another currency", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/portal/cart/items/{id}": {
			Summary: "Set a line's quantity",
			Description: "Quantity must be between 1 and 1000. Zero is not a delete: removing a line is its own " +
				"operation, so a client that sends 0 by mistake gets a validation error rather than a silently " +
				"emptied cart. Another customer's line id is a 404.",
			Responses: []Response{
				{Status: 200, Description: "The cart", Schema: cart},
				{Status: 404, Description: "No such line", Schema: ProblemRef()},
				{Status: 422, Description: "The quantity is not allowed", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/portal/cart/items/{id}": {
			Summary:     "Remove a line",
			Description: "Scoped to the caller's cart in SQL, so another customer's line id is a 404.",
			Responses: []Response{
				{Status: 200, Description: "The cart", Schema: cart},
				{Status: 404, Description: "No such line", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/portal/cart": {
			Summary: "Empty the cart",
			Description: "Removes every line and detaches the coupon, but keeps the cart and its currency: " +
				"\"clear\" is not \"delete my cart\", and the next add should not have to re-pick a currency.",
			Responses: []Response{
				{Status: 200, Description: "The empty cart", Schema: cart},
			},
		},
		"POST /api/v1/portal/cart/coupon": {
			Summary: "Apply a promotion to the cart",
			Description: "Validated against the cart's own contents before it is attached, so \"apply\" cannot " +
				"succeed on a code the totals would then refuse. Returns the cart and the live total in one " +
				"response, because applying a coupon is only useful if the UI can show the new number.",
			Responses: []Response{
				{Status: 200, Description: "The cart and its priced total", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cart":   cart,
						"totals": totals,
					},
				}},
				{Status: 404, Description: "No such code", Schema: ProblemRef()},
				{Status: 422, Description: "The code is expired, exhausted or not applicable", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/portal/cart/coupon": {
			Summary:     "Remove the promotion from the cart",
			Description: "Idempotent: removing a coupon from a cart that has none is the state the caller asked for.",
			Responses: []Response{
				{Status: 200, Description: "The cart and its priced total", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cart":   cart,
						"totals": totals,
					},
				}},
			},
		},
	}
}
