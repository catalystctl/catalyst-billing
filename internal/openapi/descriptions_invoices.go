package openapi

// Invoice documents: the PDF downloads, the client-visible note, void with a
// reason, and the proforma lifecycle.
//
// The descriptions are separate from the handlers because the generator cannot
// infer prose, and because a route without an entry is a visible omission (the
// parity test reports it) rather than a silently empty summary.

func init() { RegisterDescriptions(invoiceDocumentOperations) }

// pdfResponse describes a binary download.
//
// A PDF has no JSON schema, so the honest description is the media type and the
// disposition. Claiming a schema here would make a generated client try to parse
// the bytes.
func pdfResponse(what string) []Response {
	return []Response{
		{
			Status: 200,
			Description: what + ", served as application/pdf with " +
				"`Content-Disposition: attachment` and a filename derived from the document number.",
		},
		{Status: 404, Description: "No such document, or it is not the caller's", Schema: ProblemRef()},
	}
}

func invoiceDocumentOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------- PDF downloads --
		"GET /api/v1/portal/invoices/{id}/pdf": {
			Summary: "Download an invoice as a PDF",
			Description: "The same document the staff endpoint serves, scoped to the caller: the ownership predicate is " +
				"`customer_accounts.user_id` in the SQL, so another customer's identifier selects no row and answers 404, " +
				"byte-identical to an id that never existed. A draft is not a document and is also 404. " +
				"The page carries the merchant block, the bill-to block (including `billing_profiles.bill_to_text`), the " +
				"number, the issue and due dates, one row per line with quantity/unit/net/tax/total, the subtotal, the tax " +
				"with its jurisdiction and rate, the total, the client-visible note when enabled, and the terms. " +
				"A proforma prints as a proforma and says its number is not yet assigned.",
			Responses: pdfResponse("The invoice"),
		},
		"GET /api/v1/staff/invoices/{id}/pdf": {
			Summary: "Download any invoice as a PDF",
			Description: "The operator's copy, from the same renderer and the same frozen snapshot as the customer's. " +
				"A draft answers 409 `not_issued` rather than 404: for an operator the difference between 'a typo' and " +
				"'not issued yet' is actionable. A void invoice prints the recorded reason on the page.",
			Responses: []Response{
				{Status: 200, Description: "The invoice as application/pdf, as an attachment"},
				{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
				{Status: 409, Description: "The invoice has not been issued", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/credit-notes/{id}/pdf": {
			Summary: "Download a credit note as a PDF",
			Description: "A credit note is the counter-document that corrects an invoice, and it is immutable once issued. " +
				"It renders with negative amounts, its own CN-series number, and the number of the invoice it corrects. " +
				"Scoped in SQL exactly like the invoice download.",
			Responses: pdfResponse("The credit note"),
		},
		"GET /api/v1/staff/credit-notes/{id}/pdf": {
			Summary: "Download any credit note as a PDF",
			Description: "The operator's copy of a credit note. A goodwill credit that corrects no invoice is still " +
				"printable: its letterhead comes from the installation settings rather than from an invoice snapshot.",
			Responses: pdfResponse("The credit note"),
		},

		// ------------------------------------------------------- client note --
		"GET /api/v1/portal/invoices/{id}/document": {
			Summary: "The customer's view of a document's lifecycle facts",
			Description: "What a customer needs to read and download the documents attached to their invoice: the " +
				"client-visible note (present only while `invoices.notes_client_visible` is on), whether the document is a " +
				"proforma rather than a numbered invoice, the recorded void reason, and the credit notes issued against " +
				"it — each with the id its PDF is served under. Without this read a customer is sent a credit note they " +
				"cannot open: the download exists, but nothing tells them it does. The ownership predicate is " +
				"`customer_accounts.user_id` in the SQL, so another customer's invoice selects no row and answers 404, " +
				"byte-identical to an id that never existed. A draft is not a document and is also 404.",
			Responses: []Response{
				{Status: 200, Description: "The document state and its credit notes"},
				{Status: 404, Description: "No such document, or it is not the caller's", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/invoices/{id}/client-note": {
			Summary: "Set or clear the client-visible note on an invoice",
			Description: "The note is printed on the customer's PDF when `invoices.notes_client_visible` is on. It is " +
				"stored even when that setting is off: the setting decides what a customer sees, not what an operator may " +
				"record, and a note written today appears when the setting is turned on. An empty `note` clears it. " +
				"A void document refuses a note, so the void statement stays the last word on the page.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"note"},
				"properties": map[string]any{
					"note": map[string]any{
						"type":        "string",
						"maxLength":   2000,
						"description": "Empty clears the note.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The stored note"},
				{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
				{Status: 409, Description: "The invoice is void", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------- void reason --
		"POST /api/v1/staff/invoices/{id}/void-reason": {
			Summary: "Void an invoice and record the reason on it",
			Description: "The same transition as `POST /api/v1/staff/invoices/{id}/void`, with the reason additionally " +
				"written to `invoices.void_reason` so it travels with the document: a customer who downloads a voided " +
				"invoice reads why it was cancelled. Status and reason are written by one statement, so no reader sees a " +
				"void invoice with an empty reason. A paid or partially paid invoice still cannot be voided — the money " +
				"moved, and the correction is a credit note.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"reason"},
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The void invoice and its reason"},
				{Status: 400, Description: "The reason is missing or too long", Schema: ProblemRef()},
				{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
				{Status: 409, Description: "Paid, or already void", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- proforma --
		"POST /api/v1/staff/invoices/{id}/proforma": {
			Summary: "Issue a draft as a provisional, unnumbered invoice",
			Description: "A proforma is issued to the customer and payable, but carries no number: `invoices.proforma` " +
				"is set and `number` stays NULL, which the `invoices_proforma_unnumbered` CHECK enforces. The receivable " +
				"is posted now, because the customer owes it now; conversion later posts nothing, because conversion is a " +
				"numbering event and not a money event. The number is assigned in the single UPDATE that clears the flag " +
				"when the invoice is paid. The response reports `number: null` explicitly — the absence is the point. " +
				"`due_in_days` defaults to the configured `invoices.due_days` (7).",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"due_in_days": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     3650,
						"description": "Defaults to the configured payment terms.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The provisional document, with no number"},
				{Status: 404, Description: "No such invoice", Schema: ProblemRef()},
				{Status: 409, Description: "Not a draft, or already numbered", Schema: ProblemRef()},
				{Status: 503, Description: "Issuance is not configured", Schema: ProblemRef()},
			},
		},
	}
}
