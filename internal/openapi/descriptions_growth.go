package openapi

// The growth descriptions: the affiliate programme and Discord notifications.
//
// Two things are worth stating in the document rather than leaving to the reader of the
// handler:
//
//   - A commission rate is an exact rational string ("0.1", "10%", "1/10"), never a float,
//     and a commission is minor units in the order's currency. A client that formats a rate
//     as a float and sends it back will be refused, which is the point.
//
//   - The Discord webhook URL is write-only. The read reports `webhook_configured`, never
//     the URL: a settings page that echoes a credential back puts it in every proxy log and
//     browser cache between here and the operator.
//
// Response schemas are inline for the same reason the commerce file's are: the component
// list belongs to openapi.go, and a reference to an undefined component resolves to
// nothing.

func init() { RegisterDescriptions(growthOperations) }

// affiliateSchema is one affiliate with its per-currency balances.
func affiliateSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "user_id", "code", "commission_rate", "payout_method", "enabled"},
		"properties": map[string]any{
			"id":      map[string]any{"type": "string", "format": "uuid"},
			"user_id": map[string]any{"type": "string", "format": "uuid"},
			"email":   map[string]any{"type": "string", "format": "email"},
			"code": map[string]any{
				"type":        "string",
				"description": "Upper-case referral code. It appears in the referral link as `?ref=<code>`.",
			},
			"commission_rate": map[string]any{
				"type": "string",
				"description": "Exact rational: `0.1`, `10%` or `1/10`. Stored canonically as `1/10` " +
					"so a later recomputation lands on the same minor units. Never a float.",
			},
			"payout_method": map[string]any{
				"type": "string",
				"enum": []string{"credit", "paypal", "bank_transfer", "manual"},
			},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "Whether the affiliate can earn. Kept for compatibility; `status` is the field to branch on.",
			},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"active", "suspended", "deleted"},
				"description": "Derived server-side from the state that actually gates earning, so a client cannot " +
					"disagree with what the server will do. `suspended` means the code no longer attributes and no " +
					"further commission accrues; `deleted` means the payee is archived and everything they earned " +
					"is still owed.",
			},
			"suspended_at": map[string]any{
				"type": []string{"string", "null"}, "format": "date-time",
				"description": "When the suspension was recorded. Null on an affiliate that was never suspended, and " +
					"on one disabled before suspensions were recorded.",
			},
			"suspended_reason": map[string]any{"type": "string"},
			"deleted_at": map[string]any{
				"type": []string{"string", "null"}, "format": "date-time",
				"description": "When the affiliate was archived. Once set it is never cleared: a deletion is not a " +
					"longer suspension.",
			},
			"deleted_reason": map[string]any{"type": "string"},
			"referred_count": map[string]any{"type": "integer"},
			"balances": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"currency", "owed", "paid"},
					"properties": map[string]any{
						"currency": map[string]any{"type": "string"},
						"owed":     AmountRef(),
						"paid":     AmountRef(),
					},
				},
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// payoutLineSchema is one row of the payout report.
func payoutLineSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"affiliate_id", "code", "currency", "owed", "lifetime_paid", "pending_commissions",
		},
		"properties": map[string]any{
			"affiliate_id":  map[string]any{"type": "string", "format": "uuid"},
			"code":          map[string]any{"type": "string"},
			"user_id":       map[string]any{"type": "string", "format": "uuid"},
			"email":         map[string]any{"type": "string", "format": "email"},
			"currency":      map[string]any{"type": "string"},
			"owed":          AmountRef(),
			"lifetime_paid": AmountRef(),
			"deleted": map[string]any{
				"type": "boolean",
				"description": "The payee has been archived but is still owed money. The line is still payable: a " +
					"debt does not expire because the payee left the programme.",
			},
			"pending_commissions": map[string]any{
				"type":        "integer",
				"description": "Commission rows that make up `owed`, so a reconciler can compare summary with detail.",
			},
		},
	}
}

// referralSchema is one attribution or one commission.
func referralSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "affiliate_id", "customer_id", "status", "attributed_at"},
		"properties": map[string]any{
			"id":           map[string]any{"type": "string", "format": "uuid"},
			"affiliate_id": map[string]any{"type": "string", "format": "uuid"},
			"customer_id":  map[string]any{"type": "string", "format": "uuid"},
			"order_id": map[string]any{
				"type":        "string",
				"format":      "uuid",
				"description": "Present only on a commission row. An attribution row (written at signup) has no order.",
			},
			"commission": AmountRef(),
			"rate": map[string]any{
				"type":        "string",
				"description": "The rate captured when the commission was recorded, so a later rate change cannot rewrite it.",
			},
			"status":        map[string]any{"type": "string", "enum": []string{"pending", "approved", "paid"}},
			"attributed_at": map[string]any{"type": "string", "format": "date-time"},
			"paid_at":       map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
		},
	}
}

// discordConfigSchema is the Discord configuration, with the secret withheld.
func discordConfigSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"enabled", "events", "webhook_configured"},
		"properties": map[string]any{
			"enabled": map[string]any{"type": "boolean"},
			"events": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The selected event keys. An empty array means nothing is posted.",
			},
			"webhook_configured": map[string]any{
				"type": "boolean",
				"description": "Whether a webhook URL is stored. The URL itself is never returned: " +
					"it is a credential, and a settings read that echoes it is a leak.",
			},
			"available_events": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Every event key the notifier understands, for rendering the selection.",
			},
		},
	}
}

func growthOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ affiliates --
		"GET /api/v1/staff/affiliates": {
			Summary: "List affiliates and what each is owed",
			Description: "Every affiliate, newest first, with per-currency balances and the number of " +
				"customers they referred. Balances are a materialised total maintained in the same " +
				"transaction as each commission, so this list does not sum history to answer " +
				"\"what do we owe\".",
			Responses: []Response{
				{Status: 200, Description: "The affiliates", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": affiliateSchema()},
					},
				}},
			},
		},
		"POST /api/v1/staff/affiliates": {
			Summary: "Enrol a customer as an affiliate",
			Description: "Fixes the commission rate at which money will later leave the business, which " +
				"is why it requires credit:write rather than a reporting permission. The code is " +
				"upper-cased and must be unique; the rate is an exact rational (`0.1`, `10%`, `1/10`) " +
				"and is stored canonically. One affiliate per user, one code per affiliate.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"user_id", "code", "commission_rate"},
				"properties": map[string]any{
					"user_id": map[string]any{"type": "string", "format": "uuid"},
					"code": map[string]any{
						"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{2,31}$",
					},
					"commission_rate": map[string]any{
						"type":        "string",
						"description": "Exact rational in [0, 1]: `0.1`, `10%` or `1/10`. A float is not accepted.",
					},
					"payout_method": map[string]any{
						"type":    "string",
						"enum":    []string{"credit", "paypal", "bank_transfer", "manual"},
						"default": "credit",
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The affiliate", Schema: affiliateSchema()},
				{Status: 409, Description: "The code or the user is already an affiliate", Schema: ProblemRef()},
				{Status: 422, Description: "The rate or the code is not valid", Schema: ProblemRef()},
			},
		},
		// ------------------------------------------------- the controls (0053) --
		//
		// Four routes, one permission (credit:write) and one shape: they answer with the
		// affiliate as it now stands, because the operator's next question is always "what
		// state is it in now".
		"PATCH /api/v1/staff/affiliates/{id}": {
			Summary: "Set an affiliate's commission rate",
			Description: "Sets a custom per-affiliate rate. It is an exact rational (`10%`, `0.1`, `1/10`), " +
				"stored canonically and bounded at 100% inclusive; anything above 1 is refused rather than " +
				"clamped, because a commission larger than the order pays out money that was never taken. " +
				"The change applies to future commissions only: every commission row captures the rate that " +
				"produced it, so a rate change cannot restate what a past order earned.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"commission_rate"},
				"properties": map[string]any{
					"commission_rate": map[string]any{
						"type": "string",
						"description": "Exact rational in [0, 1]: `0.1`, `10%` or `1/10`. A float is not accepted, " +
							"and neither is a rate above 100%.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The affiliate with its new rate", Schema: affiliateSchema()},
				{Status: 404, Description: "No such affiliate", Schema: ProblemRef()},
				{Status: 409, Description: "The affiliate is deleted", Schema: ProblemRef()},
				{Status: 422, Description: "The rate is not a rational in [0, 1]", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/affiliates/{id}/suspend": {
			Summary: "Suspend (ban) an affiliate",
			Description: "Stops the affiliate earning: their referral code no longer attributes a signup and no " +
				"further commission is recorded for their referrals. What they have already earned is untouched " +
				"and remains payable — a ban is not a confiscation — and a commission on an order paid while " +
				"suspended is refused rather than recorded. A reason is required, and is stored with the " +
				"operator and the instant. Suspending an already-suspended affiliate is a no-op that keeps the " +
				"first record.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"reason"},
				"properties": map[string]any{
					"reason": map[string]any{
						"type": "string", "maxLength": 500,
						"description": "Why. Required: a ban the next operator cannot understand is one they cannot review.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The suspended affiliate", Schema: affiliateSchema()},
				{Status: 404, Description: "No such affiliate", Schema: ProblemRef()},
				{Status: 409, Description: "The affiliate is deleted", Schema: ProblemRef()},
				{Status: 422, Description: "The reason is missing or too long", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/affiliates/{id}/reinstate": {
			Summary: "Reinstate a suspended affiliate",
			Description: "Puts a suspended affiliate back into the programme with the rate they had. Idempotent: " +
				"reinstating an affiliate who is already active succeeds and changes nothing. A deleted " +
				"affiliate cannot be reinstated — deletion ends the record rather than lengthening the " +
				"suspension.",
			Responses: []Response{
				{Status: 200, Description: "The reinstated affiliate", Schema: affiliateSchema()},
				{Status: 404, Description: "No such affiliate", Schema: ProblemRef()},
				{Status: 409, Description: "The affiliate is deleted", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/affiliates/{id}": {
			Summary: "Delete (archive) an affiliate",
			Description: "Takes the affiliate out of the programme and out of the default list, and keeps the " +
				"ledger: every commission row they earned, every balance they are owed and every payout already " +
				"made stay readable, and the payout report keeps naming them for as long as their balance is " +
				"non-zero. A deleted affiliate with unpaid commission is a debt, so deletion is an archive rather " +
				"than a removal — the commission rows reference the affiliate and are never cascaded away. A " +
				"reason is required. Deleting twice is a no-op.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"reason"},
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 500},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The archived affiliate", Schema: affiliateSchema()},
				{Status: 404, Description: "No such affiliate", Schema: ProblemRef()},
				{Status: 422, Description: "The reason is missing or too long", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/affiliates/payouts": {
			Summary: "The payout report",
			Description: "Every affiliate with an outstanding balance, per currency, plus the totals per " +
				"currency. A balance is per currency because commission is earned in the order's " +
				"currency and there is no conversion rate in this system, so summing across currencies " +
				"would be meaningless.",
			Responses: []Response{
				{Status: 200, Description: "The report", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{
							"type": "array", "items": payoutLineSchema(),
						},
						"totals": map[string]any{
							"type":        "array",
							"items":       AmountRef(),
							"description": "The sum of `owed` per currency.",
						},
					},
				}},
			},
		},
		"POST /api/v1/staff/affiliates/{id}/payout": {
			Summary: "Pay out one affiliate's balance in one currency",
			Description: "Marks the pending commissions paid and moves the balance to lifetime paid, in " +
				"one transaction. A second call finds a zero balance and returns 409 `nothing_to_pay`, " +
				"so a double-click pays once even without the idempotency layer. Audited with the " +
				"operator as actor.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"currency"},
				"properties": map[string]any{
					"currency": map[string]any{
						"type":        "string",
						"pattern":     "^[A-Za-z0-9]{2,12}$",
						"description": "The currency of the balance to settle.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "Settled", Schema: map[string]any{
					"type":     "object",
					"required": []string{"affiliate_id", "currency", "amount", "commissions"},
					"properties": map[string]any{
						"affiliate_id": map[string]any{"type": "string", "format": "uuid"},
						"currency":     map[string]any{"type": "string"},
						"amount":       AmountRef(),
						"commissions":  map[string]any{"type": "integer"},
					},
				}},
				{Status: 409, Description: "Nothing outstanding in that currency", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------------- portal --
		"GET /api/v1/portal/affiliate": {
			Summary: "The caller's own affiliate code and earnings",
			Description: "Resolved from the session, never from the URL, and the referral rows are fetched " +
				"by that affiliate id, so a caller can only see their own. A customer who is not an " +
				"affiliate gets 404 `not_an_affiliate`: enrolment is not implied by having an account.",
			Responses: []Response{
				{Status: 200, Description: "The caller's affiliate view", Schema: map[string]any{
					"type":     "object",
					"required": []string{"code", "commission_rate", "balances", "referrals"},
					"properties": map[string]any{
						"code":            map[string]any{"type": "string"},
						"commission_rate": map[string]any{"type": "string"},
						"payout_method": map[string]any{
							"type": "string",
							"enum": []string{"credit", "paypal", "bank_transfer", "manual"},
						},
						"enabled":        map[string]any{"type": "boolean"},
						"referred_count": map[string]any{"type": "integer"},
						"balances":       affiliateSchema()["properties"].(map[string]any)["balances"],
						"referrals": map[string]any{
							"type": "array", "items": referralSchema(),
						},
					},
				}},
				{Status: 404, Description: "This account is not an affiliate", Schema: ProblemRef()},
			},
		},

		// --------------------------------------------------------------- discord --
		"GET /api/v1/staff/discord": {
			Summary: "The Discord notification configuration",
			Description: "Returns the enabled flag, the selected events and whether a webhook is stored. " +
				"The webhook URL is a credential and is never returned; `webhook_configured` is the " +
				"only thing a settings screen needs in order to render \"configured\".",
			Responses: []Response{
				{Status: 200, Description: "The configuration", Schema: discordConfigSchema()},
			},
		},
		"PUT /api/v1/staff/discord": {
			Summary: "Update the Discord notification configuration",
			Description: "The webhook URL is write-only: omitting it keeps the stored credential, because " +
				"the operator cannot see the old value to send it back. `clear_webhook` removes it " +
				"explicitly. Unknown event keys are dropped rather than stored, so the selection can " +
				"never name an event the notifier does not understand. Audited without the URL.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"enabled"},
				"properties": map[string]any{
					"enabled": map[string]any{"type": "boolean"},
					"webhook_url": map[string]any{
						"type":        "string",
						"format":      "uri",
						"writeOnly":   true,
						"description": "Discord webhook URL. Stored as a secret; never returned.",
					},
					"events": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Event keys to post. An empty array posts nothing.",
					},
					"clear_webhook": map[string]any{
						"type":        "boolean",
						"description": "Remove the stored URL. Separate from an empty `webhook_url` so saving the event selection cannot delete the secret by accident.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The updated configuration", Schema: discordConfigSchema()},
				{Status: 422, Description: "The configuration could not be saved", Schema: ProblemRef()},
			},
		},
	}
}
