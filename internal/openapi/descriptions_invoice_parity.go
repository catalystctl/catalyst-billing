package openapi

// The invoice parity surface (INV-03, INV-05, INV-10, INV-14, INV-15, INV-16, INV-17,
// INV-18, ADM-24, ADM-25, ADM-31).
//
// Three points in the prose below are load-bearing, because the route names invite the wrong
// reading:
//
//   - **There is no edit route for an issued invoice**, and no amount of wanting one adds it
//     here. The draft lines route refuses anything that is not a draft, and the correction for
//     an issued document is a credit note or a debit note: a second numbered document that
//     names the first. A payment is likewise immutable — it is a fact the gateway or the bank
//     confirmed — and its correction is a refund, which is its own record.
//   - **Voiding a note is a reversal, not a deletion.** The note keeps its number, its amount
//     and its reason, gains a void reason, and its ledger posting is contra-posted in the same
//     transaction. Credit that has already been spent cannot be voided, because un-spending it
//     would silently un-pay the invoice it paid.
//   - **The due-date route can only move a date later.** Moving one earlier would make an
//     invoice retroactively overdue and restart reminders against a document the customer
//     already holds. The installation can also refuse the move entirely with
//     `invoices.immutable`, which is the optional immutability INV-05 asks for.

func init() { RegisterDescriptions(func() map[string]Operation { return invoiceParityOperations }) }

// invoiceTransactionSchema is one movement on an invoice: a payment, applied credit or an
// adjustment note.
func invoiceTransactionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "kind", "amount", "status"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "format": "uuid"},
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"payment", "credit_application", "adjustment_note"},
			},
			"amount":      AmountRef(),
			"occurred_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
			"status":      map[string]any{"type": "string"},
			"provider":    map[string]any{"type": "string", "description": "Payments only."},
			"reference": map[string]any{
				"type":        "string",
				"description": "The gateway's payment identity. Two payments with one reference are one payment.",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "Set on a payment recorded outside a gateway: bank_transfer, cash, cheque, other.",
			},
			"note": map[string]any{"type": "string"},
			"number": map[string]any{
				"type":        "string",
				"description": "Adjustment notes only: the note's own number, which is what a customer quotes.",
			},
			"adjustment": map[string]any{"type": "string", "enum": []string{"credit", "debit"}},
			"refundable": map[string]any{
				"type":        "boolean",
				"description": "Payments only: whether anything is left to refund. A payment is corrected by a refund, never by an edit or a delete.",
			},
			"refunded": map[string]any{
				"type":        "object",
				"description": "Payments only: how much has already gone back, as an Amount.",
			},
			"refund_url": map[string]any{
				"type":        "string",
				"description": "Payments only: the endpoint that starts a refund for this payment.",
			},
		},
	}
}

// adjustmentNoteSchema is one credit or debit note as the invoice's screen renders it.
func adjustmentNoteSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "number", "kind", "status", "amount", "tax", "voidable"},
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "format": "uuid"},
			"number": map[string]any{"type": "string"},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"credit", "debit"},
				"description": "Which direction the adjustment goes. Frozen once the note has a number.",
			},
			"status": map[string]any{"type": "string", "enum": []string{"issued", "applied", "void"}},
			"amount": AmountRef(),
			"tax":    AmountRef(),
			"reason": map[string]any{"type": "string"},
			"void_reason": map[string]any{
				"type":        "string",
				"description": "Why the note was cancelled. Empty unless it was.",
			},
			"voided_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
			"issued_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
			"voidable": map[string]any{
				"type":        "boolean",
				"description": "Whether the cancel route would currently accept this note. The server re-checks under a lock: a note whose credit has been spent answers 409.",
			},
			"pdf_url": map[string]any{"type": "string"},
		},
	}
}

var invoiceParityOperations = map[string]Operation{
	"GET /api/v1/staff/invoices/{id}/transactions": {
		Summary: "Every money movement on one invoice",
		Description: "Payments (with what has already been refunded), applied credit and adjustment notes in one list, " +
			"newest first. They are one list because \"why is the balance what it is\" is one question: a screen " +
			"that showed payments alone would leave an applied credit or a debit note unexplained.\n\n" +
			"**Nothing in this list is editable.** A payment is a fact the gateway or the bank confirmed; the " +
			"correction is a refund, which `refund_url` points at and which is its own record with its own status. " +
			"An adjustment note is cancelled through its void route, which reverses its posting rather than " +
			"deleting the document. There is deliberately no edit and no delete route here — the subledger is " +
			"append-only and the documents are evidence.",
		Responses: []Response{
			{Status: 200, Description: "The invoice's movements", Schema: map[string]any{
				"type":     "object",
				"required": []string{"invoice_id", "transactions"},
				"properties": map[string]any{
					"invoice_id":   map[string]any{"type": "string", "format": "uuid"},
					"transactions": map[string]any{"type": "array", "items": invoiceTransactionSchema()},
				},
			}},
			{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},

	"GET /api/v1/staff/invoices/{id}/adjustment-notes": {
		Summary: "The credit and debit notes against one invoice",
		Description: "A note is a document, not a payment: it has its own number, its own series and its own PDF, so " +
			"it is listed separately from the transactions even though it moves money. Cancelled notes stay in " +
			"the list with their reason — cancelling a document is not a reason to forget it.",
		Responses: []Response{
			{Status: 200, Description: "The invoice's adjustment notes", Schema: map[string]any{
				"type":     "object",
				"required": []string{"invoice_id", "notes"},
				"properties": map[string]any{
					"invoice_id": map[string]any{"type": "string", "format": "uuid"},
					"notes":      map[string]any{"type": "array", "items": adjustmentNoteSchema()},
				},
			}},
			{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},

	"POST /api/v1/staff/invoices/{id}/debit-notes": {
		Summary: "Charge more against an invoice with a debit note",
		Description: "A debit note is the mirror of a credit note: it posts a receivable up and revenue up, is numbered " +
			"in its own series (`invoices.debit_note_prefix` / `debit_note_padding`, with the same date tokens as the " +
			"invoice format) and leaves the invoice itself untouched. Under-charging is corrected by adding a " +
			"document, never by editing the one the customer holds.\n\n" +
			"The tax share is derived from the corrected invoice's own snapshot — the rate that document recorded — " +
			"not from today's tax table. A request may state `tax_minor` explicitly to correct a specific taxed line; " +
			"it may never exceed the amount. Unlike a credit, a debit note is not capped by what is left on the " +
			"invoice: it charges more, which is why it exists.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"amount_minor", "reason"},
			"properties": map[string]any{
				"amount_minor": map[string]any{
					"type":        "integer",
					"format":      "int64",
					"minimum":     1,
					"description": "The total charge, in the invoice's currency, in minor units.",
				},
				"tax_minor": map[string]any{
					"type":        "integer",
					"format":      "int64",
					"minimum":     0,
					"description": "The tax share. Omitted or zero means \"derive it from the invoice's snapshot rate\".",
				},
				"reason": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   255,
					"description": "Printed on the note and recorded in the audit log.",
				},
			},
			"additionalProperties": false,
		},
		Responses: []Response{
			{Status: 201, Description: "The issued debit note", Schema: adjustmentNoteSchema()},
			{Status: 400, Description: "Malformed amount, tax share or reason", Schema: ProblemRef()},
			{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
			{Status: 409, Description: "The invoice is a draft or void, so there is nothing to charge", Schema: ProblemRef()},
		},
	},

	"POST /api/v1/staff/credit-notes/{id}/void": {
		Summary: "Cancel an adjustment note and reverse its posting",
		Description: "The note is kept: it gains a status of `void`, a reason and a timestamp, and its ledger posting is " +
			"contra-posted in the same transaction. That is what an append-only subledger means — \"cancelled\" and " +
			"\"never issued\" stay two distinguishable facts, which is what an auditor needs and what a customer " +
			"reprint must show.\n\n" +
			"Only an `issued` note can be voided. Credit that has already been applied cannot be un-issued: the " +
			"invoice it paid would become unpaid without anybody deciding that, so the server answers 409 and the " +
			"correction is the other kind of note. A second void is also a conflict, and the 0065 trigger refuses any " +
			"change to a voided note afterwards.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"reason"},
			"properties": map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   255,
					"description": "Why the note is being cancelled. Printed on a reprint and recorded in the audit log.",
				},
			},
			"additionalProperties": false,
		},
		Responses: []Response{
			{Status: 200, Description: "The cancelled note", Schema: map[string]any{
				"type":     "object",
				"required": []string{"id", "number", "kind", "status", "void_reason", "voided_at"},
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "format": "uuid"},
					"number":      map[string]any{"type": "string"},
					"kind":        map[string]any{"type": "string", "enum": []string{"credit", "debit"}},
					"status":      map[string]any{"type": "string", "enum": []string{"void"}},
					"void_reason": map[string]any{"type": "string"},
					"voided_at":   map[string]any{"type": "string", "format": "date-time"},
				},
			}},
			{Status: 400, Description: "A reason is required", Schema: ProblemRef()},
			{Status: 404, Description: "No such note", Schema: ProblemRef()},
			{Status: 409, Description: "Already void, or its credit has been spent", Schema: ProblemRef()},
		},
	},

	"PUT /api/v1/staff/invoices/{id}/lines": {
		Summary: "Replace a draft invoice's lines",
		Description: "A draft is a worksheet: a price typed wrong or an address that changed can be fixed until the " +
			"document has a number. This route replaces the complete line set (not a patch — a draft line has no " +
			"identity a client can safely address) and re-prices it through the same issuer path creation uses, so " +
			"a preview and the stored total cannot disagree.\n\n" +
			"The billed party is re-read from the invoice's own customer, not taken from the body: a request cannot " +
			"move a document to another customer. The reply is a 409 `not_draft` for anything already issued; the " +
			"database trigger from `0021_proforma.sql` refuses the write anyway, so this is the code answering " +
			"before the constraint has to.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"lines"},
			"properties": map[string]any{
				"due_in_days": map[string]any{
					"type":        "integer",
					"description": "Optional. Omitted keeps the draft's current due date; a value replaces it, defaulting to `invoices.due_days`.",
				},
				"lines": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type":     "object",
						"required": []string{"description", "quantity", "unit_amount_minor", "currency"},
						"properties": map[string]any{
							"description":       map[string]any{"type": "string", "minLength": 1},
							"quantity":          map[string]any{"type": "integer", "minimum": 1},
							"unit_amount_minor": map[string]any{"type": "integer", "format": "int64"},
							"currency":          map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
							"product_id":        map[string]any{"type": "string", "format": "uuid"},
							"price_version_id":  map[string]any{"type": "string", "format": "uuid"},
						},
					},
				},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The re-priced draft", Schema: map[string]any{
				"type":     "object",
				"required": []string{"id", "status", "totals", "currency"},
				"properties": map[string]any{
					"id":     map[string]any{"type": "string", "format": "uuid"},
					"status": map[string]any{"type": "string", "enum": []string{"draft"}},
					"totals": map[string]any{
						"type":     "object",
						"required": []string{"subtotal", "tax", "total"},
						"properties": map[string]any{
							"subtotal": AmountRef(),
							"tax":      AmountRef(),
							"total":    AmountRef(),
						},
					},
					"currency": map[string]any{"type": "string"},
				},
			}},
			{Status: 400, Description: "No lines, a mixed currency, or an invalid line", Schema: ProblemRef()},
			{Status: 404, Description: "No such invoice, product or price version", Schema: ProblemRef()},
			{Status: 409, Description: "The invoice has been issued; correct it with a note", Schema: ProblemRef()},
			{Status: 503, Description: "Invoice issuance is not configured", Schema: ProblemRef()},
		},
	},

	"PUT /api/v1/staff/invoices/{id}/due-date": {
		Summary: "Move an invoice's due date later",
		Description: "The database leaves `due_at` writable after issue because dunning depends on it and an operator " +
			"negotiating terms needs to move it. Two limits keep that from becoming a way to make a document " +
			"disagree with itself: the date may only move **later** (moving it earlier would make an invoice " +
			"retroactively overdue and restart reminders against a document the customer already holds), and when " +
			"`invoices.immutable` is on — the optional immutability of INV-05 — an issued document may not move at " +
			"all.\n\n" +
			"A paid, refunded or void invoice never moves, regardless of the setting: its due date is history. Send " +
			"either `due_at` or `due_in_days`.",
		RequestSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"due_at":      map[string]any{"type": "string", "format": "date-time"},
				"due_in_days": map[string]any{"type": "integer", "minimum": 1, "maximum": 3650},
			},
			"additionalProperties": false,
		},
		Responses: []Response{
			{Status: 200, Description: "The new due date", Schema: map[string]any{
				"type":     "object",
				"required": []string{"id", "due_at"},
				"properties": map[string]any{
					"id":     map[string]any{"type": "string", "format": "uuid"},
					"due_at": map[string]any{"type": "string", "format": "date-time"},
				},
			}},
			{Status: 400, Description: "Neither `due_at` nor `due_in_days` was sent", Schema: ProblemRef()},
			{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
			{Status: 409, Description: "The document is immutable, or the date would move earlier", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},
}
