package openapi

// The tax-rate import descriptions: the source, the preview, the import and its history.
//
// These exist because the routes exist — `TestEveryRouteIsDescribed` fails otherwise — but the
// prose is the useful part. A client author integrating this surface needs to know three things a
// generated document cannot say: that the preview is not a promise (the import re-fetches), that
// `overwrite_manual` is the only way a rate a human typed is ever replaced, and that a refused row
// is reported rather than fatal.

func init() { RegisterDescriptions(func() map[string]Operation { return taxImportOperations }) }

// taxImportSourceSchema is the configured document.
func taxImportSourceSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"url", "format", "enabled", "refresh_interval_hours"},
		"properties": map[string]any{
			"url":    map[string]any{"type": "string", "description": "http or https only; no credentials in the URL."},
			"format": map[string]any{"type": "string", "enum": []string{"json", "csv"}},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "A disabled source is never fetched, by a person or by the worker.",
			},
			"refresh_interval_hours": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 8760,
				"description": "How often the scheduled refresh re-fetches. The worker's own tick is fixed.",
			},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// taxImportChangeSchema is one row's fate in a report.
func taxImportChangeSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"line", "jurisdiction", "outcome", "proposed"},
		"properties": map[string]any{
			"line":         map[string]any{"type": "integer", "description": "The row's line in the document."},
			"jurisdiction": map[string]any{"type": "string", "description": "`DE/standard`, `US-CA/standard` or `all/standard`."},
			"outcome": map[string]any{
				"type": "string", "enum": []string{"create", "update", "unchanged", "protected", "rejected"},
				"description": "`protected` is the manual-rate guard working: the source disagrees, and the " +
					"rate a human typed was left alone.",
			},
			"reason": map[string]any{"type": "string", "description": "Why a row was protected or rejected."},
			"code":   map[string]any{"type": "string", "description": "Stable rejection code; empty for an applied row."},
			"current": map[string]any{
				"type":        "object",
				"description": "The open rate this change would replace, with its own provenance.",
				"properties": map[string]any{
					"id":             map[string]any{"type": "string", "format": "uuid"},
					"rate":           map[string]any{"type": "string"},
					"inclusive":      map[string]any{"type": "boolean"},
					"effective_from": map[string]any{"type": "string", "format": "date-time"},
					"origin":         map[string]any{"type": "string", "enum": []string{"manual", "import"}},
					"source":         map[string]any{"type": "string"},
					"fetched_at":     map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
				},
			},
			"proposed": map[string]any{
				"type":     "object",
				"required": []string{"country_code", "tax_class", "rate"},
				"properties": map[string]any{
					"line":           map[string]any{"type": "integer"},
					"name":           map[string]any{"type": "string"},
					"country_code":   map[string]any{"type": "string"},
					"region_code":    map[string]any{"type": "string"},
					"tax_class":      map[string]any{"type": "string"},
					"rate":           map[string]any{"type": "string", "description": "As the document spelled it; canonicalised on write."},
					"inclusive":      map[string]any{"type": "boolean"},
					"effective_from": map[string]any{"type": "string", "format": "date-time"},
				},
			},
		},
	}
}

// taxImportReportSchema is the preview and the run's stored report.
func taxImportReportSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"source_url", "format", "fetched_at", "rows", "changes", "rejections"},
		"properties": map[string]any{
			"source_url": map[string]any{"type": "string"},
			"format":     map[string]any{"type": "string", "enum": []string{"json", "csv"}},
			"fetched_at": map[string]any{"type": "string", "format": "date-time"},
			"rows":       map[string]any{"type": "integer"},
			"creates":    map[string]any{"type": "integer"},
			"updates":    map[string]any{"type": "integer"},
			"unchanged":  map[string]any{"type": "integer"},
			"protected":  map[string]any{"type": "integer"},
			"rejected":   map[string]any{"type": "integer"},
			"changes":    map[string]any{"type": "array", "items": taxImportChangeSchema()},
			"rejections": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"line", "code", "detail"},
					"properties": map[string]any{
						"line":   map[string]any{"type": "integer"},
						"key":    map[string]any{"type": "string"},
						"code":   map[string]any{"type": "string"},
						"detail": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

// taxImportRunSchema is one recorded import.
func taxImportRunSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "source_url", "format", "trigger", "outcome", "counts"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"source_url":  map[string]any{"type": "string"},
			"format":      map[string]any{"type": "string", "enum": []string{"json", "csv"}},
			"trigger":     map[string]any{"type": "string", "enum": []string{"manual", "scheduled"}},
			"actor_id":    map[string]any{"type": "string", "format": "uuid"},
			"fetched_at":  map[string]any{"type": "string", "format": "date-time"},
			"started_at":  map[string]any{"type": "string", "format": "date-time"},
			"finished_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"outcome":     map[string]any{"type": "string", "enum": []string{"running", "ok", "failed"}},
			"detail":      map[string]any{"type": "string"},
			"counts": map[string]any{
				"type":     "object",
				"required": []string{"rows", "created", "updated", "unchanged", "protected", "rejected"},
				"properties": map[string]any{
					"rows":      map[string]any{"type": "integer"},
					"created":   map[string]any{"type": "integer"},
					"updated":   map[string]any{"type": "integer"},
					"unchanged": map[string]any{"type": "integer"},
					"protected": map[string]any{"type": "integer"},
					"rejected":  map[string]any{"type": "integer"},
				},
			},
			"report": taxImportReportSchema(),
		},
	}
}

// taxImportRequestSchema is the preview and import body.
func taxImportRequestSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"overwrite_manual": map[string]any{
				"type": "boolean",
				"description": "Replace a rate an operator typed. Omitted means no: the row is reported " +
					"as `protected` and left exactly as it was. The scheduled refresh never sets it.",
			},
		},
	}
}

var taxImportOperations = map[string]Operation{
	"GET /api/v1/staff/tax-import/source": {
		Summary: "The configured tax-rate source",
		Description: "`configured: false` is the normal state of a fresh installation and is not an error. " +
			"`last_run` is the most recent import, so the screen can say when the table was last refreshed " +
			"and whether that refresh failed.",
		Responses: []Response{
			{Status: 200, Description: "The source and the most recent run", Schema: map[string]any{
				"type":     "object",
				"required": []string{"configured"},
				"properties": map[string]any{
					"configured": map[string]any{"type": "boolean"},
					"source":     taxImportSourceSchema(),
					"last_run":   taxImportRunSchema(),
				},
			}},
		},
	},
	"PUT /api/v1/staff/tax-import/source": {
		Summary: "Configure the tax-rate source",
		Description: "One source per installation: this replaces the previous one. Only `http` and `https` " +
			"are accepted and a URL carrying credentials is refused, because the panel fetches it with its " +
			"own network identity and a credential in a URL ends up in a log and a run report. Rates already " +
			"imported keep the URL they were fetched from, so repointing the source does not rewrite their " +
			"provenance.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url":                    map[string]any{"type": "string"},
				"format":                 map[string]any{"type": "string", "enum": []string{"json", "csv"}},
				"enabled":                map[string]any{"type": "boolean"},
				"refresh_interval_hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 8760},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The saved source", Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"configured": map[string]any{"type": "boolean"}, "source": taxImportSourceSchema()},
			}},
			{Status: 422, Description: "The URL, format or interval is not acceptable", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/tax-import/preview": {
		Summary: "Preview what an import would change",
		Description: "Fetches the configured source and diffs it against the open rates, writing nothing. " +
			"Every row is reported: `create`, `update`, `unchanged`, `protected` (a rate a human typed, or one " +
			"an operator disabled) or `rejected` with a stable code. A refused row is not a failed preview. The " +
			"preview is not a promise — the import re-fetches and re-diffs inside its transaction — but it is " +
			"the same code, so it cannot tell a nicer story than the import.",
		RequestSchema: taxImportRequestSchema(),
		Responses: []Response{
			{Status: 200, Description: "The preview", Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"preview": taxImportReportSchema()},
			}},
			{Status: 409, Description: "No source is configured, or it is switched off", Schema: ProblemRef()},
			{Status: 422, Description: "The document cannot be read", Schema: ProblemRef()},
			{Status: 502, Description: "The source could not be fetched", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/tax-import/import": {
		Summary: "Import the source into the rate table",
		Description: "Fetches, diffs and applies in one transaction, then returns the run and the report of " +
			"what was actually applied. Each imported rate records its provenance: `origin: import`, the URL " +
			"it came from and when it was fetched. A rate that changed is appended, not rewritten: the rate " +
			"it replaces is closed at the new row's start, because a rate that has taken effect is the " +
			"evidence an issued invoice points at. A manual rate is replaced only when `overwrite_manual` is " +
			"true; otherwise it is reported as `protected`.",
		RequestSchema: taxImportRequestSchema(),
		Responses: []Response{
			{Status: 200, Description: "The run and its report", Schema: map[string]any{
				"type":     "object",
				"required": []string{"run", "preview"},
				"properties": map[string]any{
					"run":     taxImportRunSchema(),
					"preview": taxImportReportSchema(),
				},
			}},
			{Status: 409, Description: "No source is configured, or it is switched off", Schema: ProblemRef()},
			{Status: 422, Description: "The document cannot be read", Schema: ProblemRef()},
			{Status: 502, Description: "The source could not be fetched", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/tax-import/runs": {
		Summary: "Recent tax-rate imports",
		Description: "Newest first, with the full per-row report. A failed fetch is recorded as a `failed` " +
			"run with its reason, so \"the refresh did nothing\" and \"the refresh could not read the source\" " +
			"are distinguishable.",
		Responses: []Response{
			{Status: 200, Description: "The runs", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"data"},
				"properties": map[string]any{"data": map[string]any{"type": "array", "items": taxImportRunSchema()}},
			}},
		},
	},
	"GET /api/v1/staff/tax-import/provenance": {
		Summary: "Where every tax rate came from",
		Description: "One entry per rate row, keyed by id, carrying `origin` (`manual` or `import`), the " +
			"source URL and the fetch time. A manual rate has an empty source and no fetch time: an operator " +
			"typed it, no document said it.",
		Responses: []Response{
			{Status: 200, Description: "The provenance", Schema: map[string]any{
				"type":     "object",
				"required": []string{"data"},
				"properties": map[string]any{
					"data": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"rate_id", "origin", "source"},
							"properties": map[string]any{
								"rate_id":    map[string]any{"type": "string", "format": "uuid"},
								"origin":     map[string]any{"type": "string", "enum": []string{"manual", "import"}},
								"source":     map[string]any{"type": "string"},
								"fetched_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
							},
						},
					},
				},
			}},
		},
	},
}
