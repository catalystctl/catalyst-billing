package openapi

// The dashboard-widget and affiliate-payout descriptions (E5.6, GRW-03).
//
// Three operations, and the prose carries the two decisions a client cannot infer from the
// schema:
//
//   - A dashboard figure that could not be read is `null` and is named in `unavailable`,
//     while a period with nothing in it is `0`. A client must render those differently: a
//     dash for the first, a zero for the second. Collapsing them would make an outage look
//     like a quiet month, which is the most misleading thing a dashboard can do.
//
//   - A payout is an append-only record, and the report is what an operator reads before
//     making one. The three figures in it are required to satisfy
//     `earned - paid == outstanding`, and `reconciles` states whether they did when the
//     report was read — a `false` there is a defect in the data, not in the report.
//
// Response schemas are inline for the same reason the other feature files' are: the
// component list belongs to openapi.go, and a reference to an undefined component resolves to
// nothing.

func init() { RegisterDescriptions(dashboardExtOperations) }

// widgetAmountSchema is one per-currency money figure.
func widgetAmountSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"currency", "scale", "minor", "display"},
		"properties": map[string]any{
			"currency": map[string]any{"type": "string"},
			"scale":    map[string]any{"type": "integer"},
			"minor": map[string]any{
				"type":        "integer",
				"format":      "int64",
				"description": "Integer minor units. Never a float, never a decimal string.",
			},
			"display": map[string]any{"type": "string"},
		},
	}
}

// dashboardQueueSchema is the paid-not-provisioned queue.
func dashboardQueueSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "count", "items", "truncated"},
		"properties": map[string]any{
			"code":     map[string]any{"type": "string", "enum": []string{"paid_not_provisioned"}},
			"title":    map[string]any{"type": "string"},
			"detail":   map[string]any{"type": "string"},
			"action":   map[string]any{"type": "string"},
			"severity": map[string]any{"type": "string", "enum": []string{"critical", "warning", "info"}},
			"count": map[string]any{
				"type":        "integer",
				"description": "Every affected invoice, not just the rows below.",
			},
			"oldest_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"money": map[string]any{
				"type":        "array",
				"items":       widgetAmountSchema(),
				"description": "The amount at stake per currency. There is no cross-currency total.",
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"invoice_id", "total"},
					"properties": map[string]any{
						"invoice_id": map[string]any{"type": "string", "format": "uuid"},
						"number":     map[string]any{"type": "string"},
						"customer_id": map[string]any{
							"type": "string", "format": "uuid",
						},
						"email":     map[string]any{"type": "string", "format": "email"},
						"order_id":  map[string]any{"type": "string", "format": "uuid"},
						"total":     AmountRef(),
						"issued_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
					},
				},
			},
			"truncated": map[string]any{
				"type":        "boolean",
				"description": "True when `count` exceeds the rows returned.",
			},
		},
	}
}

// payoutRowSchema is one row that makes up the difference between earned and paid.
func payoutRowSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "id", "amount", "status", "occurred_at"},
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"commission", "renewal", "payout"},
				"description": "`commission` is earned on an order, `renewal` on a renewal invoice, " +
					"`payout` is money that left.",
			},
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"invoice_id": map[string]any{"type": "string", "format": "uuid"},
			"order_id":   map[string]any{"type": "string", "format": "uuid"},
			"amount":     AmountRef(),
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{"pending", "approved", "paid", "recorded"},
				"description": "A commission's status, or `recorded` for a payout row, which has no lifecycle.",
			},
			"rate": map[string]any{
				"type":        "string",
				"description": "The canonical rational captured when the commission was recorded.",
			},
			"occurred_at": map[string]any{"type": "string", "format": "date-time"},
			"paid_at":     map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"payout_id": map[string]any{
				"type":        "string",
				"format":      "uuid",
				"description": "The payout that settled this commission. Absent while it is still owed.",
			},
			"renewal_index": map[string]any{
				"type":        "integer",
				"description": "1 for the first renewal of a referred customer, 2 for the second, and so on.",
			},
		},
	}
}

// reconciliationSchema is one affiliate's books in one currency.
func reconciliationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"affiliate_id", "currency", "earned", "paid", "outstanding", "reconciles", "rows",
		},
		"properties": map[string]any{
			"affiliate_id": map[string]any{"type": "string", "format": "uuid"},
			"code":         map[string]any{"type": "string"},
			"email":        map[string]any{"type": "string", "format": "email"},
			"currency":     map[string]any{"type": "string"},
			"earned": map[string]any{
				"allOf":       []any{AmountRef()},
				"description": "Every commission ever recorded, in this currency.",
			},
			"paid":        AmountRef(),
			"outstanding": AmountRef(),
			"payouts":     map[string]any{"type": "integer"},
			"reconciles": map[string]any{
				"type": "boolean",
				"description": "Whether `earned - paid == outstanding` held when the report was read. " +
					"A false here is a defect in the data, and the rows below are how to find it.",
			},
			"rows": map[string]any{
				"type": "array", "items": payoutRowSchema(),
				"description": "The commission and payout rows inside the period, newest first. " +
					"The totals above are all-time; the period only scopes these rows.",
			},
		},
	}
}

func dashboardExtOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------- dashboard --
		"GET /api/v1/staff/dashboard/widgets": {
			Summary: "Dashboard widgets over a selectable period",
			Description: "Revenue from the ledger, the customer counts, the support queue, the overdue " +
				"position and the paid-not-provisioned queue, over a period the caller chooses.\n\n" +
				"`period` is `7d`, `30d`, `90d` or `custom`; a custom period needs `from` and `to` as " +
				"RFC3339 timestamps or `YYYY-MM-DD` dates, and may not span more than 366 days. A " +
				"date-only `to` means the end of that day, so a range cannot silently exclude the day " +
				"the operator asked for.\n\n" +
				"Revenue is read from `journal_entries` on the revenue account, not from invoice " +
				"totals, so a voided invoice is not revenue and a credit note reduces it. It is per " +
				"currency and has no cross-currency total, because there is no exchange rate in this " +
				"system.\n\n" +
				"A figure that could not be read is `null` and is named in `unavailable`; a period " +
				"with nothing in it is `0`. The response is still 200 in both cases: the dashboard is " +
				"the first screen an operator opens and it has to render when one number cannot be " +
				"read.",
			Responses: []Response{
				{Status: 200, Description: "The widget set", Schema: map[string]any{
					"type": "object",
					"required": []string{
						"period", "from", "to", "days", "generated_at", "available", "revenue",
					},
					"properties": map[string]any{
						"period": map[string]any{
							"type": "string", "enum": []string{"7d", "30d", "90d", "custom"},
						},
						"from":         map[string]any{"type": "string", "format": "date-time"},
						"to":           map[string]any{"type": "string", "format": "date-time"},
						"days":         map[string]any{"type": "integer"},
						"generated_at": map[string]any{"type": "string", "format": "date-time"},
						"available": map[string]any{
							"type":        "boolean",
							"description": "False when at least one figure could not be read.",
						},
						"unavailable": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "The JSON keys of the widgets that failed, so a screen can mark them.",
						},
						"revenue": map[string]any{
							"type":        "array",
							"items":       widgetAmountSchema(),
							"description": "Net revenue per currency, from the ledger.",
						},
						"active_customers": map[string]any{
							"type": []string{"integer", "null"},
							"description": "Customers with a live service, an invoice issued or a payment " +
								"received in the period.",
						},
						"new_customers": map[string]any{
							"type":        []string{"integer", "null"},
							"description": "Customer records created in the period.",
						},
						"open_tickets": map[string]any{
							"type": []string{"integer", "null"},
							"description": "The support queue depth now. A queue is a current state, so it " +
								"does not move when the period changes.",
						},
						"tickets_opened": map[string]any{
							"type":        []string{"integer", "null"},
							"description": "Tickets opened in the period.",
						},
						"overdue_invoices": map[string]any{
							"type": []string{"object", "null"},
							"properties": map[string]any{
								"count":         map[string]any{"type": "integer"},
								"oldest_due_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
								"money": map[string]any{
									"type":        "array",
									"items":       widgetAmountSchema(),
									"description": "What is still owed on each overdue invoice, per currency.",
								},
							},
							"description": "The overdue position now, not a period total.",
						},
						"paid_not_provisioned": map[string]any{
							"allOf":       []any{dashboardQueueSchema()},
							"description": "The exception queue internal/ops alerts on: settled invoices whose order bought nothing that is running.",
						},
					},
				}},
				{Status: 400, Description: "The period is not one this endpoint accepts", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------------- affiliate payouts --
		"GET /api/v1/staff/affiliates/{id}/payouts": {
			Summary: "Reconcile one affiliate's earnings over a period",
			Description: "What the affiliate has earned, what has already been paid, what is still owed, " +
				"and the rows that make up the difference: every commission with the order or renewal " +
				"invoice it came from and the payout that settled it, and every payout with its method " +
				"and reference.\n\n" +
				"The totals are all-time, so the period scopes only the rows: a window that predates the " +
				"commissions still states what is owed. `reconciles` reports whether " +
				"`earned - paid == outstanding` held when the report was read.\n\n" +
				"`currency` narrows the report to one currency. Omit it for every currency the affiliate " +
				"has touched — an affiliate with earnings in two currencies has two balances and two " +
				"payouts, because summing them would be arithmetic without an exchange rate.",
			Responses: []Response{
				{Status: 200, Description: "The reconciliation", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data", "history", "period"},
					"properties": map[string]any{
						"data": map[string]any{
							"type": "array", "items": reconciliationSchema(),
						},
						"history": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"required": []string{
									"id", "affiliate_id", "currency", "amount", "method", "created_at",
								},
								"properties": map[string]any{
									"id":           map[string]any{"type": "string", "format": "uuid"},
									"affiliate_id": map[string]any{"type": "string", "format": "uuid"},
									"currency":     map[string]any{"type": "string"},
									"amount":       AmountRef(),
									"method": map[string]any{
										"type": "string",
										"enum": []string{"credit", "paypal", "bank_transfer", "manual"},
									},
									"reference":   map[string]any{"type": "string"},
									"note":        map[string]any{"type": "string"},
									"created_by":  map[string]any{"type": "string", "format": "uuid"},
									"period_from": map[string]any{"type": "string", "format": "date-time"},
									"period_to":   map[string]any{"type": "string", "format": "date-time"},
									"created_at":  map[string]any{"type": "string", "format": "date-time"},
								},
							},
							"description": "Recorded payouts, newest first.",
						},
						"period": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"from": map[string]any{"type": "string", "format": "date-time"},
								"to":   map[string]any{"type": "string", "format": "date-time"},
							},
						},
					},
				}},
				{Status: 400, Description: "The period is not one this endpoint accepts", Schema: ProblemRef()},
				{Status: 404, Description: "No such affiliate", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/affiliates/{id}/payouts": {
			Summary: "Record a payout",
			Description: "Settles the affiliate's whole outstanding balance in one currency and records " +
				"it as an append-only row that links every commission it settled.\n\n" +
				"It never restates what a commission was worth: the amount on a commission row is the " +
				"fact the payout pays, not a figure the payout recalculates. After a payout the " +
				"reconciliation reads `earned == paid` and `outstanding == 0`.\n\n" +
				"It refuses when the materialised balance and the commission rows disagree (409 " +
				"`balance_mismatch`) — paying either figure would move money nobody can account for " +
				"afterwards. A second call with nothing outstanding is 409 `nothing_to_pay`, so a " +
				"double-click pays once even before the Idempotency-Key layer answers.\n\n" +
				"The payout row and its audit event commit in one transaction: if the audit write " +
				"fails, the money does not move.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"currency"},
				"properties": map[string]any{
					"currency": map[string]any{
						"type":        "string",
						"pattern":     "^[A-Za-z0-9]{2,12}$",
						"description": "The currency of the balance to settle. A payout never mixes currencies.",
					},
					"method": map[string]any{
						"type":        "string",
						"enum":        []string{"credit", "paypal", "bank_transfer", "manual"},
						"description": "How the money left. Defaults to the affiliate's configured method.",
					},
					"reference": map[string]any{
						"type":        "string",
						"description": "A provider or bank reference, quoted when reconciling a statement.",
					},
					"note": map[string]any{"type": "string"},
					"from": map[string]any{
						"type":        "string",
						"description": "The start of the window the operator was reviewing. Recorded on the payout, not used to filter what is settled.",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "The end of that window.",
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded payout", Schema: map[string]any{
					"type": "object",
					"required": []string{
						"id", "affiliate_id", "currency", "amount", "method", "commissions", "created_at",
					},
					"properties": map[string]any{
						"id":           map[string]any{"type": "string", "format": "uuid"},
						"affiliate_id": map[string]any{"type": "string", "format": "uuid"},
						"currency":     map[string]any{"type": "string"},
						"amount":       AmountRef(),
						"method": map[string]any{
							"type": "string",
							"enum": []string{"credit", "paypal", "bank_transfer", "manual"},
						},
						"reference": map[string]any{"type": "string"},
						"commissions": map[string]any{
							"type":        "integer",
							"description": "How many commission rows the payout settled, of both kinds.",
						},
						"order_commissions":   map[string]any{"type": "integer"},
						"renewal_commissions": map[string]any{"type": "integer"},
						"created_at":          map[string]any{"type": "string", "format": "date-time"},
					},
				}},
				{Status: 409, Description: "Nothing outstanding, or the balance disagrees with its rows", Schema: ProblemRef()},
				{Status: 422, Description: "The currency or the method is not valid", Schema: ProblemRef()},
			},
		},
	}
}
