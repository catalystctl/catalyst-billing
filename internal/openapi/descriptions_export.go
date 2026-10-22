package openapi

// The CSV export descriptions (OPS-12).
//
// Four things are stated in the document rather than left to the reader of the handler:
//
//   - **The body is a file, not JSON.** The response is `text/csv` with a
//     `Content-Disposition` filename, so a client's correct behaviour is to hand the URL to
//     the browser (or to save the bytes) rather than to decode it. Every failure is still the
//     usual problem+json envelope, because a failure happens before the first byte is written.
//
//   - **Money travels twice per amount.** `total` is an exact decimal string in the currency's
//     major unit — produced from integer minor units, never a float, so a spreadsheet reads it
//     as a number and no cent can move — and `total_minor` is the raw integer count a
//     reconciliation should be checked against. The same pair exists for every amount column.
//
//   - **The export is the list, unpaginated.** Each export takes the same filter parameters as
//     its list endpoint, with the same meanings, and refuses any parameter it does not honour
//     rather than silently returning the whole book. There is no `limit` and no `cursor`.
//
//   - **A `customer_id` filter needs the permission to read that customer.** An operator who
//     may export invoices in general cannot extract a specific customer's documents, services
//     or balance unless they hold customer:read:all.
//
// Response schemas are described in prose for the same reason the other description files'
// are: the document does not model CSV columns as JSON Schema, and a guessed schema would be
// confidently wrong. The columns are listed here instead.

func init() { RegisterDescriptions(exportOperations) }

// csvResponse is the success response every export shares.
func csvResponse(description string) Response {
	return Response{Status: 200, Description: description}
}

func exportOperations() map[string]Operation {
	return map[string]Operation{
		"GET /api/v1/staff/exports/invoices.csv": {
			Summary: "Export invoices as CSV",
			Description: "Streams every invoice matching the filters as `text/csv`, in the order the invoice " +
				"list shows them. Rows are read a page at a time and flushed as they are written, so the " +
				"download does not grow with the size of the book.\n\n" +
				"Filters are the list's: `status`, `customer_id`, `currency`, `from`, `to` (RFC3339) and " +
				"`overdue`. A parameter this endpoint does not honour is a 400 rather than a filter that is " +
				"silently ignored.\n\n" +
				"Columns: `invoice_id`, `invoice_number`, `status`, `currency`, `issued_at` (RFC3339, UTC), " +
				"`due_at` (RFC3339, UTC), `due_date` (yyyy-mm-dd), `customer_id`, `customer_email`, `total`, " +
				"`total_minor`, `paid`, `paid_minor`, `balance_due`, `balance_due_minor`, `overdue`. Every " +
				"amount appears twice: the bare column is an exact decimal string in the major unit " +
				"(integer minor units divided by the currency's scale — never a float), and the `_minor` " +
				"column is the integer count. Text fields that would start a spreadsheet formula are " +
				"prefixed with an apostrophe, so a customer name cannot execute anything.\n\n" +
				"Scoped twice: the route needs invoice:read:all, and a `customer_id` filter additionally " +
				"needs customer:read:all — an operator cannot export a customer they cannot read.",
			Responses: []Response{
				csvResponse("The matching invoices as CSV"),
				{Status: 400, Description: "A filter is malformed or is one this export does not honour", Schema: ProblemRef()},
				{Status: 403, Description: "The caller may not read invoices, or may not read the filtered customer", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/exports/customers.csv": {
			Summary: "Export customers as CSV",
			Description: "Streams every customer matching the search as `text/csv`, one row per currency the " +
				"customer owes in. A customer who owes nothing is one row with an empty currency, an empty " +
				"amount and `outstanding_minor` 0: a balance is not one number, and adding a USD balance to " +
				"a EUR one would be arithmetic on unrelated quantities.\n\n" +
				"Filter: `q`, the same search the customer list uses (email or name, at least two " +
				"characters). The sort the list offers is not a filter and is refused here: the export is " +
				"ordered by creation, so two exports of the same data are comparable.\n\n" +
				"Columns: `customer_id`, `email`, `name`, `created_at`, `open_invoices`, `services`, " +
				"`has_login`, `outstanding_currency`, `outstanding`, `outstanding_minor`. Requires " +
				"customer:read:all.",
			Responses: []Response{
				csvResponse("The matching customers as CSV"),
				{Status: 400, Description: "The search is too short, or a parameter is one this export does not honour", Schema: ProblemRef()},
				{Status: 403, Description: "The caller may not read customers", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/exports/services.csv": {
			Summary: "Export services as CSV",
			Description: "Streams every service matching the filters as `text/csv`, with the state an operator " +
				"or an auditor asks about: whose it is, which product it is on, whether it is provisioned, " +
				"and whether the queue is still holding something for it.\n\n" +
				"Filters are the list's: `status`, `customer_id`, `product_id` and `has_open_operation`. A " +
				"parameter this endpoint does not honour is a 400.\n\n" +
				"Columns: `service_id`, `customer_id`, `customer_email`, `product_id`, `product_name`, " +
				"`status`, `external_id`, `generation`, `created_at`, `next_bill_at`, `open_operations`, " +
				"`last_error`. There is no money column: what a service costs is on the invoice export and " +
				"on the invoice itself.\n\n" +
				"Scoped twice: the route needs service:read:all, and a `customer_id` filter additionally " +
				"needs customer:read:all.",
			Responses: []Response{
				csvResponse("The matching services as CSV"),
				{Status: 400, Description: "A filter is malformed or is one this export does not honour", Schema: ProblemRef()},
				{Status: 403, Description: "The caller may not read services, or may not read the filtered customer", Schema: ProblemRef()},
			},
		},
	}
}
