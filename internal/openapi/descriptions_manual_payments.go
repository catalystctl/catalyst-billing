package openapi

// Offline payments: recording money that arrived without a gateway.
//
// The prose here exists because the two endpoints are easy to misread as "create a payment".
// They do not create anything at the provider: they record a fact that already happened — a
// bank transfer that landed, a cheque that cleared, cash in a drawer — and allocate it to an
// invoice through the same subledger path a webhook settlement uses.
//
// The parts a client has to get right:
//
//   - `reference` is the payment's identity, not a memo. It is required, it is unique per
//     manual payment, and re-sending it is how a retry stays safe after the Idempotency-Key
//     response has expired.
//   - `method` is a closed set. A value outside it is a 400 rather than a recorded guess.
//   - an overpayment is refused with 409 `overpayment`; it is never silently turned into
//     customer credit, because granting credit is its own audited action.

func init() { RegisterDescriptions(manualPaymentOperations) }

func manualPaymentOperations() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/staff/invoices/{id}/payments": {
			Summary: "Record a payment that arrived outside a gateway",
			Description: "Records a bank transfer, a cash payment, a cheque or another offline payment against " +
				"one invoice, and allocates it.\n\n" +
				"The invoice's paid amount and status follow the allocation: an amount below the outstanding " +
				"balance leaves the invoice partly paid with a smaller balance, and an amount that settles it " +
				"marks the invoice paid and runs the same fulfilment the webhook runs (provisioning for what " +
				"the invoice bought, and credit for a top-up deposit).\n\n" +
				"**The payment is not a gateway payment.** The row is written with `provider: \"manual\"`, the " +
				"operator's `reference` as the payment id, and the `method` beside it, so a reconciliation report " +
				"can tell a cheque from a card charge without guessing from a reference prefix.\n\n" +
				"**Idempotency works twice.** The route requires an `Idempotency-Key`, so a replay returns the " +
				"stored response; and the `reference` is the payment's identity in the database, so even a retry " +
				"with a fresh key applies the money once and answers `already_recorded: true`.\n\n" +
				"**An overpayment is refused** with `409 overpayment`, not converted to credit. An operator who " +
				"received more than the invoice owed records the invoice amount here and grants the remainder as " +
				"credit as a separate, visible action.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"amount_minor", "method", "reference"},
				"properties": map[string]any{
					"amount_minor": map[string]any{
						"type":    "integer",
						"format":  "int64",
						"minimum": 1,
						"description": "What actually arrived, in the invoice's currency, in minor units. Never " +
							"defaulted from the invoice: a partial payment is the normal case.",
					},
					"method": map[string]any{
						"type": "string",
						"enum": []string{"bank_transfer", "cash", "cheque", "other"},
						"description": "How the money arrived. `other` keeps the set closed without forcing a " +
							"wrong value.",
					},
					"reference": map[string]any{
						"type":      "string",
						"maxLength": 200,
						"description": "The bank reference, cheque number or receipt id. It is the payment's " +
							"identity: recording the same reference twice applies the money once.",
					},
					"note": map[string]any{
						"type":        "string",
						"maxLength":   500,
						"description": "Optional free text for the operator reading the payment list later.",
					},
					"currency": map[string]any{
						"type":        "string",
						"pattern":     "^[A-Z0-9]{2,12}$",
						"description": "Optional. When present it must match the invoice's currency.",
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded payment and the invoice's new balance", Schema: map[string]any{
					"type":     "object",
					"required": []string{"payment_id", "invoice_id", "method", "amount", "remaining", "invoice_paid"},
					"properties": map[string]any{
						"payment_id":       map[string]any{"type": "string", "format": "uuid"},
						"invoice_id":       map[string]any{"type": "string", "format": "uuid"},
						"provider":         map[string]any{"type": "string", "enum": []string{"manual"}},
						"method":           map[string]any{"type": "string"},
						"method_label":     map[string]any{"type": "string"},
						"reference":        map[string]any{"type": "string"},
						"note":             map[string]any{"type": "string"},
						"amount":           AmountRef(),
						"total":            AmountRef(),
						"paid":             AmountRef(),
						"remaining":        AmountRef(),
						"invoice_paid":     map[string]any{"type": "boolean"},
						"already_recorded": map[string]any{"type": "boolean"},
					},
				}},
				{Status: 400, Description: "invalid_method, invalid_payment, invalid_currency or currency_mismatch", Schema: ProblemRef()},
				{Status: 404, Description: "not_found", Schema: ProblemRef()},
				{Status: 409, Description: "overpayment, reference_reused, reference_conflict or not_issued", Schema: ProblemRef()},
				{Status: 422, Description: "idempotency_key_reuse", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/payment-methods/offline": {
			Summary: "The offline payment methods",
			Description: "The closed set of methods the record-payment endpoint accepts, with the label to " +
				"show a human. It is served so the form does not keep a second copy of the vocabulary that " +
				"could drift from the database's CHECK constraint.",
			Responses: []Response{
				{Status: 200, Description: "The methods", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"methods": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"value": map[string]any{"type": "string"},
									"label": map[string]any{"type": "string"},
								},
							},
						},
					},
				}},
			},
		},
	}
}
