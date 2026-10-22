package openapi

// The staff credit screen's reads (CRD-07, E5.2).
//
// These three entries describe the reading half of staff credit: who holds a balance, the
// grants that have been made, and one grant with its evidence. The writing half — the grant
// itself — is described with the rest of the credit surface in descriptions_credits.go,
// because it is the same feature and a reader looking for "how do I grant credit" should not
// have to find it in two files.
//
// The distinction the prose keeps making: a **balance** is derived from the `customer_credit`
// subledger and is per currency, while a **grant** is a document — a numbered credit note —
// with an audit row written inside the transaction that issued it. A screen that showed one
// without the other would be either unverifiable (a number with no document) or unexplained
// (a document with no effect).
//
// Reading requires `credit:read:all`, which is deliberately not `credit:write`: seeing what a
// customer is owed is an audit question, and support holds it without being able to move money.

func init() { RegisterDescriptions(operatorCreditOperations) }

// operatorCreditOperations is the prose for the staff credit reads and the debit write.
func operatorCreditOperations() map[string]Operation {
	creditCustomer := map[string]any{
		"type":     "object",
		"required": []string{"id", "email", "name", "created_at", "balances"},
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "format": "uuid"},
			"email": map[string]any{"type": "string"},
			"name":  map[string]any{"type": "string"},
			// One entry per currency with a positive balance. An empty array means nothing is
			// on account, never that the read failed.
			"balances":   map[string]any{"type": "array", "items": AmountRef()},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}

	grant := map[string]any{
		"type": "object",
		"required": []string{"id", "number", "customer_id", "amount", "reason", "status",
			"created_at"},
		"properties": map[string]any{
			"id":               map[string]any{"type": "string", "format": "uuid"},
			"number":           map[string]any{"type": "string", "description": "the credit note number, from the installation's credit-note series"},
			"customer_id":      map[string]any{"type": "string", "format": "uuid"},
			"customer_email":   map[string]any{"type": "string"},
			"amount":           AmountRef(),
			"reason":           map[string]any{"type": "string"},
			"status":           map[string]any{"type": "string", "enum": []string{"draft", "issued", "applied", "void"}},
			"kind":             map[string]any{"type": "string", "description": "the note's frozen snapshot kind: grant, refund, correction"},
			"invoice_id":       map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
			"created_at":       map[string]any{"type": "string", "format": "date-time"},
			"created_by":       map[string]any{"type": "string", "description": "the operator's user id, empty for a system grant"},
			"created_by_email": map[string]any{"type": "string"},
		},
	}

	auditRow := map[string]any{
		"type":     "object",
		"required": []string{"id", "action", "outcome", "created_at"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "integer", "format": "int64"},
			"action":      map[string]any{"type": "string", "description": "a dotted verb, e.g. credit.grant"},
			"outcome":     map[string]any{"type": "string", "enum": []string{"success", "failure", "denied"}},
			"actor_id":    map[string]any{"type": "string"},
			"actor_email": map[string]any{"type": "string"},
			"created_at":  map[string]any{"type": "string", "format": "date-time"},
			"changes":     map[string]any{"type": "object", "description": "the redacted diff, as stored"},
		},
	}

	return map[string]Operation{
		"POST /api/v1/staff/customers/{id}/credit/debit": {
			Summary: "Take credit away from a customer",
			Description: "Deducts from the customer's credit balance and records it as an append-only journal entry of kind " +
				"`credit_debit` (debit `customer_credit`, credit `revenue`) — never an edit of an existing entry, so the balance " +
				"stays the sum of its entries. `reason` is required: it becomes the memo on the customer-credit entry, which is what " +
				"the customer's own credit history renders, and it is copied into the audit row written inside the same " +
				"transaction. A debit larger than the balance is **refused** with `insufficient_credit` (409) rather than clamped, so " +
				"the balance can never go negative and the operator's intent and the books cannot silently differ. The " +
				"`Idempotency-Key` is required and becomes the entry's identity: a double-submit replays the original deduction " +
				"instead of taking the credit twice, and the same key presented with different content is refused with " +
				"`idempotency_key_reused`. Requires `credit:write` — this is a money movement, not a read.",
			Responses: []Response{
				{Status: 201, Description: "The deduction, the balance it left and its audit row", Schema: map[string]any{
					"type":     "object",
					"required": []string{"id", "customer_id", "amount", "balance", "reason", "kind", "replayed"},
					"properties": map[string]any{
						"id":          map[string]any{"type": "string", "format": "uuid", "description": "the journal entry's reference id, derived from the Idempotency-Key"},
						"customer_id": map[string]any{"type": "string", "format": "uuid"},
						"amount":      AmountRef(),
						"balance":     AmountRef(),
						"reason":      map[string]any{"type": "string"},
						"kind":        map[string]any{"type": "string", "enum": []string{"credit_debit"}},
						"replayed":    map[string]any{"type": "boolean"},
						"audit":       auditRow,
					},
				}},
				{Status: 400, Description: "The amount is not positive, the reason is missing, or the currency is unknown", Schema: ProblemRef()},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
				{Status: 409, Description: "`insufficient_credit` (the balance cannot support the debit) or `idempotency_key_reused`", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/credit/debits": {
			Summary: "Staff credit deductions, newest first",
			Description: "Every `credit_debit` posting, read from the journal rather than from a table of deductions: the deduction " +
				"*is* the posting, and a list assembled from a second table could disagree with the balance it is meant to explain. " +
				"Each row carries the reason (the entry's memo, the same text the customer sees) and the operator from the audit row " +
				"written with the posting, which is empty rather than absent when that row cannot be found. Filter with " +
				"`customer_id` for one account's history.",
			Responses: []Response{
				{Status: 200, Description: "The deductions", Schema: map[string]any{
					"type":     "object",
					"required": []string{"debits"},
					"properties": map[string]any{
						"debits": map[string]any{"type": "array", "items": map[string]any{
							"type":     "object",
							"required": []string{"id", "customer_id", "amount", "reason", "created_at"},
							"properties": map[string]any{
								"id":               map[string]any{"type": "string", "format": "uuid"},
								"customer_id":      map[string]any{"type": "string", "format": "uuid"},
								"customer_email":   map[string]any{"type": "string"},
								"amount":           AmountRef(),
								"reason":           map[string]any{"type": "string"},
								"created_at":       map[string]any{"type": "string", "format": "date-time"},
								"created_by":       map[string]any{"type": "string"},
								"created_by_email": map[string]any{"type": "string"},
							},
						}},
					},
				}},
				{Status: 400, Description: "`limit` is out of range", Schema: ProblemRef()},
				{Status: 404, Description: "`customer_id` is not a customer id", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/credit/customers": {
			Summary: "Customers with their credit balances",
			Description: "One row per customer, with the balance **per currency** — never a total, because the product has no exchange " +
				"rate and a single number would be arithmetic on unrelated quantities. The balance is derived from the " +
				"`customer_credit` subledger account, the same source the customer's own portal reads, so the number an operator " +
				"quotes is the number the books contain. `q` matches the email or the name and needs two characters: a " +
				"one-character term matches nearly every row, and refusing it is better than a request that looks broken because " +
				"it is slow. `with_credit=true` lists only customers who hold something, which is the list an operator starts an " +
				"audit from. Keyset-paginated, so a customer created while the list is being scrolled is neither skipped nor " +
				"duplicated.",
			Responses: []Response{
				{Status: 200, Description: "A page of customers with their per-currency credit", Schema: PageSchema(creditCustomer)},
				{Status: 400, Description: "The search term, sort, direction or cursor is invalid", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/credit/grants": {
			Summary: "Goodwill credit grants, newest first",
			Description: "The credit notes issued with no invoice behind them, read from the note's frozen snapshot " +
				"(`snapshot.kind = \"grant\"`): the snapshot is what the document was issued with, so it cannot drift from the " +
				"note it describes, and a correction to a specific invoice has a different kind and belongs on that invoice. " +
				"Filter with `customer_id` for one account's history. Keyset-paginated on `(created_at, id)`.",
			Responses: []Response{
				{Status: 200, Description: "A page of grants", Schema: PageSchema(grant)},
				{Status: 400, Description: "The cursor is invalid", Schema: ProblemRef()},
				{Status: 404, Description: "`customer_id` is not a customer id", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/credit/grants/{id}": {
			Summary: "One grant, with its audit rows and the balance it left",
			Description: "The screen that answers \"what did that grant do\", in one request: the credit note as issued, every audit " +
				"row written against it (append-only, written inside the transaction that made the change, so it is evidence " +
				"rather than a description), and the customer's balance per currency **now**. The balance is the current one, not " +
				"a replay of the balance at issue time: a per-instant balance is derivable from the ledger but not stored, and a " +
				"screen that reconstructed it would be inventing a number. A currency with nothing left reports a zero balance " +
				"rather than omitting the field, because an absent field reads as a failed read. The route accepts any credit " +
				"note id and reports its `kind`, so a note that turns out to be a refund correction is shown rather than 404ed.",
			Responses: []Response{
				{Status: 200, Description: "The note, its audit rows, its customer and the current balance",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"grant", "audit", "balance", "balances", "customer"},
						"properties": map[string]any{
							"grant":    grant,
							"audit":    map[string]any{"type": "array", "items": auditRow},
							"balance":  AmountRef(),
							"balances": map[string]any{"type": "array", "items": AmountRef()},
							"customer": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":    map[string]any{"type": "string", "format": "uuid"},
									"email": map[string]any{"type": "string"},
								},
							},
						},
					}},
				{Status: 404, Description: "No such credit note", Schema: ProblemRef()},
			},
		},
	}
}
