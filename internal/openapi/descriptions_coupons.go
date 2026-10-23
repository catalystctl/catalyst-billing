package openapi

// The coupon renewal descriptions.
//
// One route, and the prose is the point: the wire shape carries three numbers — `cycles_applied`,
// `cycles_total` and `remaining` — and a client that recomputes the third from the first two will
// eventually disagree with the renewal engine about how many cycles a promotion has left. The
// server computes it from the same two columns the engine reads, so the answer is the engine's.

func init() { RegisterDescriptions(func() map[string]Operation { return couponRenewalOperations }) }

// couponSubscriptionSchema is one subscription carrying a coupon.
func couponSubscriptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"subscription_id", "customer_id", "status", "cycles_applied"},
		"properties": map[string]any{
			"subscription_id": map[string]any{"type": "string", "format": "uuid"},
			"service_id":      map[string]any{"type": "string", "format": "uuid", "description": "Present once the service exists."},
			"customer_id":     map[string]any{"type": "string", "format": "uuid"},
			"customer_email":  map[string]any{"type": "string"},
			"product_name":    map[string]any{"type": "string"},
			"status":          map[string]any{"type": "string"},
			"cycles_applied":  map[string]any{"type": "integer", "description": "How many invoices this coupon has already discounted."},
			"cycles_total": map[string]any{
				"type":        []string{"integer", "null"},
				"description": "The coupon's `recurring_cycles`; null means every cycle.",
			},
			"remaining": map[string]any{
				"type": []string{"integer", "null"},
				"description": "`cycles_total - cycles_applied`, floored at zero, or null for a lifetime coupon. " +
					"Computed by the server so a screen cannot derive a different answer from the same two columns.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

var couponRenewalOperations = map[string]Operation{
	"GET /api/v1/staff/coupons/{id}/subscriptions": {
		Summary: "The subscriptions a coupon discounts on renewal",
		Description: "A coupon has two lives: it discounts the invoice that used it, and then it discounts renewals " +
			"for as many cycles as it was configured for. The redemption list shows the first; this shows the second. " +
			"`remaining` is null for a lifetime coupon and zero once the window is used up, which are different facts " +
			"that a client must not collapse into one number.",
		Responses: []Response{
			{Status: 200, Description: "Every subscription carrying the coupon, newest first", Schema: map[string]any{
				"type":     "object",
				"required": []string{"data"},
				"properties": map[string]any{
					"data": map[string]any{"type": "array", "items": couponSubscriptionSchema()},
				},
			}},
			{Status: 404, Description: "The id is not a coupon id", Schema: ProblemRef()},
			{Status: 403, Description: "The caller does not hold coupon:read:all", Schema: ProblemRef()},
		},
	},
}
