package openapi

// The importer's descriptions: uploading a dump, and reading what a previous run did.
//
// Three things are worth stating in the document rather than leaving to the reader of the
// handler:
//
//   - **A dry run is the default.** `commit` must be sent as `true` to write anything, and a
//     dry run writes nothing at all — not a customer, not a run row. A client that renders a
//     confirmation screen for an import should call this endpoint twice: once without
//     `commit`, show the report, and once with it.
//
//   - **Every rejection carries its row.** `row` is the n-th data row (the header is not a
//     row), `line` is the physical line in the file, and `code` is stable. A client groups by
//     `code` and shows `detail`; it must never parse `detail`.
//
//   - **Re-running a dump is safe.** The importer is idempotent on the legacy id from the old
//     panel, so a second run of the same files creates nothing and reports the rows as
//     `unchanged`. That is also what makes a dump larger than the request body limit
//     importable: send it in several runs, clients first, and each run resumes where the last
//     one stopped.
//
// Response schemas are inline for the same reason the other description files' are: the
// component list belongs to openapi.go.

func init() { RegisterDescriptions(importerOperations) }

// importTotalsSchema is the headline count of a report.
func importTotalsSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"rows_read", "created", "updated", "unchanged", "rejected"},
		"properties": map[string]any{
			"rows_read": map[string]any{
				"type":        "integer",
				"description": "Data rows read across all five files. The header is not a row.",
			},
			"created":   map[string]any{"type": "integer"},
			"updated":   map[string]any{"type": "integer"},
			"unchanged": map[string]any{"type": "integer", "description": "Already imported and identical: the expected result of replaying a dump."},
			"rejected":  map[string]any{"type": "integer"},
		},
	}
}

// importFileSchema is what happened to one document.
func importFileSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"file", "rows_read", "rejected"},
		"properties": map[string]any{
			"file": map[string]any{
				"type": "string",
				"enum": []string{"clients", "invoices", "invoice_items", "services", "transactions"},
			},
			"rows_read": map[string]any{"type": "integer"},
			"rejected":  map[string]any{"type": "integer"},
			"error": map[string]any{
				"type": []string{"string", "null"},
				"description": "Set when the document could not be read at all — a missing required " +
					"column, or a quoting error. The other documents are still processed.",
			},
		},
	}
}

// importTableSchema is what happened to one entity.
func importTableSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"entity", "created", "updated", "unchanged", "rejected"},
		"properties": map[string]any{
			"entity": map[string]any{
				"type": "string",
				"enum": []string{"customer", "invoice", "invoice_line", "service", "payment"},
			},
			"created":   map[string]any{"type": "integer"},
			"updated":   map[string]any{"type": "integer"},
			"unchanged": map[string]any{"type": "integer"},
			"rejected":  map[string]any{"type": "integer"},
		},
	}
}

// importRejectionSchema is one refused row.
func importRejectionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"file", "row", "line", "entity", "code", "detail"},
		"properties": map[string]any{
			"file": map[string]any{"type": "string"},
			"row": map[string]any{
				"type":        "integer",
				"description": "The n-th data row in the file, counting from 1 below the header.",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "The physical line the record starts on, for an editor.",
			},
			"entity":    map[string]any{"type": "string"},
			"legacy_id": map[string]any{"type": []string{"string", "null"}},
			"code": map[string]any{
				"type": "string",
				"enum": []string{
					"invalid_id", "duplicate_id_in_dump", "missing_column", "invalid_csv",
					"field_count", "invalid_email", "invalid_amount", "negative_amount",
					"invalid_date", "missing_date", "missing_currency", "invalid_currency",
					"unknown_status", "unknown_client", "unknown_invoice", "parent_rejected",
					"missing_invoice_number", "duplicate_invoice_number", "number_taken",
					"invoice_immutable", "draft_invoice", "total_mismatch", "email_taken",
					"unknown_product", "non_positive_payment", "payment_immutable",
					"duplicate_payment", "overpayment", "invalid_value", "conflict",
				},
				"description": "Stable machine code. A client branches on this, never on `detail`.",
			},
			"detail": map[string]any{
				"type":        "string",
				"description": "A sentence naming the value and the rule. Never parsed by a client.",
			},
		},
	}
}

// importWarningSchema is something imported that the operator should look at.
func importWarningSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "detail"},
		"properties": map[string]any{
			"file":      map[string]any{"type": []string{"string", "null"}},
			"row":       map[string]any{"type": []string{"integer", "null"}},
			"legacy_id": map[string]any{"type": []string{"string", "null"}},
			"code": map[string]any{
				"type": "string",
				"enum": []string{
					"paid_invoice_without_payments", "invoice_paid_disagrees_with_payments",
					"invoice_subtotal_disagrees_with_lines", "payment_without_invoice",
					"services_imported_without_subscription", "line_tax_not_reconstructed",
					"customer_matched_by_email", "service_cancelled_imported_as_terminated",
				},
			},
			"detail": map[string]any{"type": "string"},
		},
	}
}

// importReportSchema is the answer to "what would happen" or "what happened".
func importReportSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"source", "dry_run", "started_at", "duration_ms", "totals", "files", "tables",
			"rejections", "warnings",
		},
		"properties": map[string]any{
			"source": map[string]any{"type": "string", "enum": []string{"whmcs", "paymenter"}},
			"dry_run": map[string]any{
				"type":        "boolean",
				"description": "True when nothing was written. The single most important field in the document.",
			},
			"run_id": map[string]any{
				"type":        []string{"string", "null"},
				"format":      "uuid",
				"description": "The recorded run. Null on a dry run, which is not recorded at all.",
			},
			"started_at":  map[string]any{"type": "string", "format": "date-time"},
			"duration_ms": map[string]any{"type": "integer"},
			"totals":      importTotalsSchema(),
			"files":       map[string]any{"type": "array", "items": importFileSchema()},
			"tables":      map[string]any{"type": "array", "items": importTableSchema()},
			"rejections":  map[string]any{"type": "array", "items": importRejectionSchema()},
			"warnings":    map[string]any{"type": "array", "items": importWarningSchema()},
		},
	}
}

// importRunSchema is one recorded run.
func importRunSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "source", "created", "updated", "unchanged", "rejected", "created_at", "report"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"source":     map[string]any{"type": "string"},
			"actor_id":   map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
			"created":    map[string]any{"type": "integer"},
			"updated":    map[string]any{"type": "integer"},
			"unchanged":  map[string]any{"type": "integer"},
			"rejected":   map[string]any{"type": "integer"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"report":     importReportSchema(),
		},
	}
}

func importerOperations() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/staff/imports": {
			Summary: "Run an import from another billing panel (dry run by default)",
			Description: "Takes the five CSV documents a WHMCS-style dump carries — clients, invoices, " +
				"invoice_items, services and transactions — and reports, row by row, what would be " +
				"created, updated, left alone and refused, with the reason and the row number for each " +
				"rejection.\n\n" +
				"**Nothing is written unless `commit` is true.** The dry run is the same code inside a " +
				"transaction that is always rolled back, so its counts are the counts a commit " +
				"produces; there is no second implementation to drift.\n\n" +
				"A commit is one transaction: either every valid row lands or none does, and a " +
				"rejected row is reported rather than aborting the run, so a dump with three " +
				"unimportable rows still migrates the rest. Imported rows are keyed on the legacy id " +
				"from the old panel, so re-running the same dump creates nothing and reports " +
				"`unchanged` — which is also how a dump larger than the request body limit is " +
				"imported: send it in several runs, clients first.\n\n" +
				"An imported invoice always carries its historical number. A row with no number is " +
				"refused (`missing_invoice_number`) and a number already printed on another invoice " +
				"is a conflict (`number_taken`); the importer never renumbers. If the number belongs " +
				"to this panel's own `INV-` series, the series is advanced past it so a later invoice " +
				"cannot reuse it.\n\n" +
				"Nothing is posted to the ledger: an imported invoice records what the old system did " +
				"and is not a revenue event of this one.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"source", "files"},
				"properties": map[string]any{
					"source": map[string]any{
						"type":        "string",
						"enum":        []string{"whmcs", "paymenter"},
						"description": "The dump dialect. It selects the column names each document may use.",
					},
					"commit": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "Write the import. Omitted or false is a dry run, which writes nothing.",
					},
					"default_currency": map[string]any{
						"type":        "string",
						"description": "Currency for rows that carry none of their own. Without it such a row is refused with `missing_currency`.",
					},
					"files": map[string]any{
						"type":     "object",
						"required": []string{"clients", "invoices", "invoice_items", "services", "transactions"},
						"description": "Each document's CSV text, header row included. A header-only file " +
							"is valid and means \"the old installation had none of these\".",
						"properties": map[string]any{
							"clients":       map[string]any{"type": "string"},
							"invoices":      map[string]any{"type": "string"},
							"invoice_items": map[string]any{"type": "string"},
							"services":      map[string]any{"type": "string"},
							"transactions":  map[string]any{"type": "string"},
						},
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The report — a dry run, or a committed run", Schema: importReportSchema()},
				{Status: 409, Description: "A concurrent import wrote the same record; nothing was changed", Schema: ProblemRef()},
				{Status: 413, Description: "The dump exceeds the request body limit", Schema: ProblemRef()},
				{Status: 422, Description: "The source is unknown, a document is missing, or a document name is not one this importer reads", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/imports": {
			Summary: "List recorded import runs",
			Description: "Committed runs, newest first, each with its full report. Dry runs are absent by " +
				"construction: they write nothing, so there is nothing to list — the report of a dry run " +
				"is in the response of the request that ran it.\n\n" +
				"The permission is import:write rather than a reporting one: an import log says how many " +
				"customers, invoices and payments came from another panel, and whoever may run an import " +
				"is the person who should see what the last one did.",
			Responses: []Response{
				{Status: 200, Description: "The runs", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":        map[string]any{"type": "array", "items": importRunSchema()},
						"next_cursor": map[string]any{"type": []string{"string", "null"}},
					},
				}},
			},
		},
		"GET /api/v1/staff/imports/{id}": {
			Summary: "Read one import run",
			Description: "The run with the report it recorded: every rejection with its row number, every " +
				"warning, and the per-file and per-entity counts. The report is stored verbatim, because " +
				"\"why is this customer here\" must not depend on the dump still existing on someone's " +
				"laptop.",
			Responses: []Response{
				{Status: 200, Description: "The run", Schema: importRunSchema()},
				{Status: 400, Description: "The id is not a uuid", Schema: ProblemRef()},
				{Status: 404, Description: "No such run", Schema: ProblemRef()},
			},
		},
	}
}
