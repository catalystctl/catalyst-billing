package openapi

// Credit and pay-flow descriptions (P7).
//
// These entries are prose only: the method, path, permission, CSRF and idempotency
// metadata all come from the route table, so a change to a route's guard cannot leave this
// file claiming something else. What is written here is the part a generator cannot know —
// *why* a refusal means what it means, and which field the UI should wait on.
//
// The one distinction worth repeating in every entry: a checkout attempt's status is a
// hint, and `settled` is the fact. Only a webhook-verified allocation settles an invoice,
// which is why the status endpoint exists instead of the UI trusting its own redirect.

func init() { RegisterDescriptions(creditOperations) }

// creditOperations is the prose for customer credit and the customer pay flow.
func creditOperations() map[string]Operation {
	balance := map[string]any{
		"type":     "object",
		"required": []string{"customer_id", "balances", "settings"},
		"properties": map[string]any{
			"customer_id": map[string]any{"type": "string", "format": "uuid"},
			"balances":    map[string]any{"type": "array", "items": AmountRef()},
			"settings": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"enabled":               map[string]any{"type": "boolean"},
					"minimum_deposit_minor": map[string]any{"type": "integer", "format": "int64"},
					"maximum_deposit_minor": map[string]any{"type": "integer", "format": "int64"},
					"maximum_credit_minor":  map[string]any{"type": "integer", "format": "int64"},
					"auto_use":              map[string]any{"type": "boolean"},
				},
			},
		},
	}

	ops := map[string]Operation{
		// ------------------------------------------------------------ credit --

		"GET /api/v1/portal/credits": {
			Summary: "The customer's credit balance and the top-up limits",
			Description: "The balance is derived from the `customer_credit` subledger account, never from a stored column, so it cannot " +
				"disagree with the journal. The settings block is returned with it so the deposit form can show the real minimum and " +
				"maximum instead of a guess, and so a disabled installation can hide the form rather than fail the submission.",
			Responses: []Response{
				{Status: 200, Description: "Balances per currency, and the credits settings", Schema: balance},
				{Status: 404, Description: "The caller has no billing account", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/credits/deposit": {
			Summary: "Start a credit top-up through the gateway",
			Description: "Creates a deposit invoice and opens a gateway session, then returns the redirect URL. The amount is validated " +
				"against `credits.enabled`, `credits.minimum_deposit_minor`, `credits.maximum_deposit_minor` and " +
				"`credits.maximum_credit_minor` *before* anything is created, so a refused top-up leaves no open invoice behind, " +
				"and an unsettled deposit already started counts against the maximum so two top-ups cannot both pass the limit. " +
				"A deposit is an ordinary invoice carrying `snapshot.catalyst_deposit = \"credit\"` and no order: the gateway path, " +
				"its recorded attempt and its amount verification are the existing ones, unchanged, and the operator dashboard's " +
				"paid-not-provisioned check does not mistake it for an undelivered purchase. The credit is granted when the " +
				"settled webhook arrives, not when this call returns.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"amount_minor", "currency"},
				"properties": map[string]any{
					"amount_minor": map[string]any{"type": "integer", "format": "int64", "minimum": 1},
					"currency":     map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "A deposit invoice and the gateway redirect"},
				{Status: 400, Description: "The amount is outside the configured deposit range", Schema: ProblemRef()},
				{Status: 409, Description: "Credit is disabled, or the top-up would exceed the maximum balance", Schema: ProblemRef()},
				{Status: 503, Description: "No payment gateway is configured, or the gateway is unavailable", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/customers/{id}/credit": {
			Summary: "Grant credit to a customer",
			Description: "Issues a numbered credit note and posts an adjustment (debit revenue, credit customer_credit). The number comes " +
				"from the same credit-note series as a correcting note, so a grant cannot take a number a document later repeats. " +
				"A reason is required and reaches the audit row. The request's idempotency key also becomes the note's identity, so " +
				"a replay returns the existing grant instead of minting a second one.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"amount_minor", "currency", "reason"},
				"properties": map[string]any{
					"amount_minor": map[string]any{"type": "integer", "format": "int64", "minimum": 1},
					"currency":     map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
					"reason":       map[string]any{"type": "string", "minLength": 1},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The issued credit note and the balance after it"},
				{Status: 400, Description: "The amount, currency or reason is missing or invalid", Schema: ProblemRef()},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- pay flow --

		"POST /api/v1/portal/invoices/{id}/pay-with-credit": {
			Summary: "Pay an invoice with the customer's credit",
			Description: "Applies `min(available balance, what the invoice owes)` and answers with the new remaining balance. Scoped to the " +
				"session: another customer's invoice is reported as not_found, identically to a nonexistent one. Idempotent — a " +
				"replayed request finds the existing application and spends nothing more, which is what makes a double-click safe.",
			Responses: []Response{
				{Status: 200, Description: "The application, the invoice's new totals and the remaining credit",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"application_id":  map[string]any{"type": "string", "format": "uuid"},
							"invoice_id":      map[string]any{"type": "string", "format": "uuid"},
							"amount":          AmountRef(),
							"total":           AmountRef(),
							"paid":            AmountRef(),
							"remaining":       AmountRef(),
							"invoice_paid":    map[string]any{"type": "boolean"},
							"already_applied": map[string]any{"type": "boolean"},
							"credit_balance":  map[string]any{"type": "array", "items": AmountRef()},
						},
					}},
				{Status: 404, Description: "No such invoice, or it belongs to someone else", Schema: ProblemRef()},
				{Status: 409, Description: "Nothing to apply: no credit, nothing owed, or credit is disabled", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/invoices/{id}/payment-status": {
			Summary: "Poll the local state of an invoice's payment attempt",
			Description: "Reports the newest checkout attempt's status plus two facts the UI can wait on: `settled` (an allocation exists, " +
				"which only a verified webhook creates) and `remaining` (what is still owed). It deliberately does not call the " +
				"provider: a browser polling this endpoint must not become one provider API call per poll, and the local state is " +
				"what decides whether the invoice is paid. A redirect returning to the success URL is not evidence of payment; " +
				"`settled` is.",
			Responses: []Response{
				{Status: 200, Description: "The attempt's status and whether the invoice is settled",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"invoice_id", "status", "settled", "remaining"},
						"properties": map[string]any{
							"invoice_id": map[string]any{"type": "string", "format": "uuid"},
							"status": map[string]any{
								"type": "string",
								"enum": []string{"none", "created", "pending", "requires_action",
									"succeeded", "failed", "cancelled"},
							},
							"settled":   map[string]any{"type": "boolean"},
							"remaining": AmountRef(),
							"paid":      AmountRef(),
							"total":     AmountRef(),
							"attempt": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":           map[string]any{"type": "string", "format": "uuid"},
									"gateway":      map[string]any{"type": "string"},
									"status":       map[string]any{"type": "string"},
									"provider_ref": map[string]any{"type": "string"},
									"amount":       AmountRef(),
									"redirect_url": map[string]any{"type": "string"},
									"created_at":   map[string]any{"type": "string", "format": "date-time"},
									"updated_at":   map[string]any{"type": "string", "format": "date-time"},
								},
							},
						},
					}},
				{Status: 404, Description: "No such invoice, or it belongs to someone else", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/payment-methods": {
			Summary: "The payment gateways this installation offers",
			Description: "Reports the configured gateway with the display metadata the pay dialog needs. This build has exactly one " +
				"gateway — the extension named by `BILLING_GATEWAY_PROVIDER` — so the list has one entry when one is configured and " +
				"is empty when none is. There is no gateway registry to invent: a gateway that does not exist cannot be offered. " +
				"`brand_hint` describes the shape of the flow (a hosted card page), not a logo the panel cannot honestly supply.",
			Responses: []Response{
				{Status: 200, Description: "The configured gateways",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"payment_methods": map[string]any{
								"type": "array",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"id":               map[string]any{"type": "string"},
										"provider":         map[string]any{"type": "string"},
										"name":             map[string]any{"type": "string"},
										"brand_hint":       map[string]any{"type": "string"},
										"methods":          map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
										"currencies":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
										"supports_refunds": map[string]any{"type": "boolean"},
										"enabled":          map[string]any{"type": "boolean"},
									},
								},
							},
						},
					}},
			},
		},
		"POST /api/v1/staff/payments": {
			Summary: "Record a payment that arrived outside the gateway",
			Description: "Creates a payment with provider `manual` and the caller's reference as its provider payment id, then allocates it " +
				"through the same `billing.ApplyPayment` every other path uses: the ledger posting, the invoice transition and the " +
				"idempotency are that function's, not a copy. Presenting the same reference twice finds the existing payment instead " +
				"of recording it again. `fee_minor` (PAY-13) is stored on the payment for reconciliation and is not posted: the ledger " +
				"records the gross the customer paid, and the fee is a cost of collecting it that a report nets off. A reason is " +
				"required and reaches the audit row.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"invoice_id", "amount_minor", "reference", "reason"},
				"properties": map[string]any{
					"invoice_id":   map[string]any{"type": "string", "format": "uuid"},
					"amount_minor": map[string]any{"type": "integer", "format": "int64", "minimum": 1},
					"currency":     map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
					"reference":    map[string]any{"type": "string", "minLength": 1},
					"fee_minor":    map[string]any{"type": "integer", "format": "int64", "minimum": 0},
					"reason":       map[string]any{"type": "string", "minLength": 1},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded payment and the invoice's remaining balance",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"payment_id":       map[string]any{"type": "string", "format": "uuid"},
							"invoice_id":       map[string]any{"type": "string", "format": "uuid"},
							"provider":         map[string]any{"type": "string"},
							"reference":        map[string]any{"type": "string"},
							"amount":           AmountRef(),
							"fee":              AmountRef(),
							"remaining":        AmountRef(),
							"invoice_paid":     map[string]any{"type": "boolean"},
							"already_recorded": map[string]any{"type": "boolean"},
						},
					}},
				{Status: 400, Description: "The amount, reference or reason is missing, or the currency does not match", Schema: ProblemRef()},
				{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
				{Status: 409, Description: "The amount exceeds what is owed, or the reference is already used", Schema: ProblemRef()},
			},
		},
	}
	return ops
}
