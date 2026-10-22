package openapi

// The operator surface: system settings, announcements, the log viewers, cron history and
// the dashboard (P8 / E5.1, E5.4, E5.6, GRW-01).
//
// The prose in this file is where the two rules a generated client cannot see are written
// down: a secret is never returned (a read says `is_set` instead), and money is never summed
// across currencies (revenue is a list, and there is no total). Both are documented on the
// operations that enforce them, because a client author who does not know them will build a
// screen that looks correct and is wrong.

func init() { RegisterDescriptions(operatorOperations) }

// settingValueSchema is one setting as a read path sees it.
func settingValueSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"group", "key", "kind", "value", "is_secret", "is_set"},
		"properties": map[string]any{
			"group": map[string]any{"type": "string"},
			"key":   map[string]any{"type": "string"},
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"string", "int", "bool", "json"},
				"description": "How the value is parsed. A JSON value is stored as text and must be " +
					"valid JSON; an int is an integer, never a float.",
			},
			"value": map[string]any{
				"type": "string",
				"description": "The stored text, or `[redacted]` when `is_secret` is true. A secret's " +
					"value is never returned, whether or not a row exists.",
			},
			"is_secret": map[string]any{
				"type":        "boolean",
				"description": "This value is a credential and was redacted. Render it as an empty password field.",
			},
			"is_set": map[string]any{
				"type": "boolean",
				"description": "A row exists. It is how a client tells an unchanged default from an " +
					"explicitly saved value, and the only thing a secret's read reveals.",
			},
			"updated_at": map[string]any{
				"type":        []string{"string", "null"},
				"format":      "date-time",
				"description": "When the row was last written; null when the value is the code-side default.",
			},
		},
	}
}

// settingsGroupSchema is one group of settings.
func settingsGroupSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"group", "keys"},
		"properties": map[string]any{
			"group": map[string]any{"type": "string", "enum": []string{
				"general", "invoices", "credits", "tax", "tickets", "cronjob", "mail", "announcements",
			}},
			"keys": map[string]any{"type": "array", "items": settingValueSchema()},
		},
	}
}

// settingsWriteSchema is the bulk upsert body.
func settingsWriteSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"group", "values"},
		"properties": map[string]any{
			"group": map[string]any{
				"type":        "string",
				"description": "The group being written. One request writes one group, so a form that fails cannot half-apply across groups.",
			},
			"values": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "key -> text value. A secret sent as `[redacted]` is skipped rather than stored, " +
					"which is what makes \"load the page, change the company name, save\" safe.",
			},
		},
	}
}

// announcementSchema is one announcement.
func announcementSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "title", "placement", "published", "created_at", "updated_at"},
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "format": "uuid"},
			"title": map[string]any{"type": "string", "maxLength": 200},
			"body":  map[string]any{"type": "string"},
			"placement": map[string]any{
				"type": "string",
				"enum": []string{"storefront", "dashboard", "both"},
			},
			"published":    map[string]any{"type": "boolean"},
			"published_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"expires_at":   map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"created_by": map[string]any{
				"type":        "string",
				"format":      "uuid",
				"description": "Empty in the public feed: who wrote an announcement is operator information.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
			"live": map[string]any{
				"type":        "boolean",
				"description": "Published and not expired, derived at read time so it cannot drift from the clock.",
			},
		},
	}
}

// announcementWriteSchema is the create/update body.
func announcementWriteSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"title"},
		"properties": map[string]any{
			"title":     map[string]any{"type": "string", "maxLength": 200},
			"body":      map[string]any{"type": "string"},
			"placement": map[string]any{"type": "string", "enum": []string{"storefront", "dashboard", "both"}, "default": "both"},
			"published": map[string]any{
				"type":        "boolean",
				"description": "Omitted keeps the current state on update. Publishing stamps published_at once; unpublishing keeps it.",
			},
			"expires_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
		},
	}
}

// emailLogSchema is one row of the email log.
func emailLogSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "kind", "to", "subject", "status", "created_at"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "format": "uuid"},
			"kind": map[string]any{
				"type": "string",
				"description": "The message kind: password_reset, email_verification, invoice_issued, " +
					"dunning_reminder or one of the notification events.",
			},
			"to":         map[string]any{"type": "string", "format": "email"},
			"subject":    map[string]any{"type": "string"},
			"status":     map[string]any{"type": "string", "enum": []string{"queued", "sending", "sent", "failed", "suppressed"}},
			"attempts":   map[string]any{"type": "integer"},
			"last_error": map[string]any{"type": "string"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"sent_at":    map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
		},
	}
}

// errorLogSchema is one row of the error log.
func errorLogSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "level", "message", "created_at"},
		"properties": map[string]any{
			"id":      map[string]any{"type": "integer", "format": "int64"},
			"level":   map[string]any{"type": "string", "example": "error"},
			"source":  map[string]any{"type": "string", "example": "worker.renewals"},
			"message": map[string]any{"type": "string"},
			"detail": map[string]any{
				"type":        "string",
				"description": "Diagnostic text, with credential-looking key/value pairs replaced by `[redacted]` before it was stored.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// httpLogSchema is one row of the HTTP log.
func httpLogSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "method", "path", "status", "duration_ms", "created_at"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "integer", "format": "int64"},
			"method":      map[string]any{"type": "string"},
			"path":        map[string]any{"type": "string", "description": "The path only. A query string is stripped before it is stored, so a token in one is not copied into a backup."},
			"status":      map[string]any{"type": "integer"},
			"duration_ms": map[string]any{"type": "integer"},
			"actor_id":    map[string]any{"type": "string", "format": "uuid"},
			"ip":          map[string]any{"type": "string"},
			"created_at":  map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// cronRunSchema is one job run.
func cronRunSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "job", "started_at", "outcome", "items"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"job":         map[string]any{"type": "string", "example": "renewals"},
			"started_at":  map[string]any{"type": "string", "format": "date-time"},
			"finished_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"outcome":     map[string]any{"type": "string", "enum": []string{"running", "ok", "failed", "skipped"}},
			"detail":      map[string]any{"type": "string"},
			"items":       map[string]any{"type": "integer"},
			"duration_ms": map[string]any{"type": []string{"integer", "null"}, "description": "Null while the run is still going."},
		},
	}
}

// heartbeatSchema is the worker heartbeat.
func heartbeatSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "beat_at", "age_seconds", "jobs", "version", "stale"},
		"properties": map[string]any{
			"id":                  map[string]any{"type": "string", "example": "worker"},
			"beat_at":             map[string]any{"type": "string", "format": "date-time"},
			"age_seconds":         map[string]any{"type": "integer"},
			"jobs":                map[string]any{"type": "integer"},
			"version":             map[string]any{"type": "string"},
			"stale":               map[string]any{"type": "boolean", "description": "True when the beat is older than the server's stale threshold. A row existing is not the same as a worker being alive."},
			"stale_after_seconds": map[string]any{"type": "integer"},
		},
	}
}

// dashboardSchema is the dashboard response.
func dashboardSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"period", "from", "to", "revenue", "invoices", "available", "generated_at"},
		"properties": map[string]any{
			"period": map[string]any{"type": "string", "enum": []string{"7d", "30d", "90d"}},
			"from":   map[string]any{"type": "string", "format": "date-time"},
			"to":     map[string]any{"type": "string", "format": "date-time"},
			"revenue": map[string]any{
				"type": "array",
				"description": "Revenue per currency, from the ledger's revenue account (credits minus debits, " +
					"so a credit note reduces it and a void invoice never counted). There is deliberately " +
					"no cross-currency total: adding currencies would be arithmetic on unrelated quantities.",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"currency", "scale", "minor", "display"},
					"properties": map[string]any{
						"currency": map[string]any{"type": "string"},
						"scale":    map[string]any{"type": "integer"},
						"minor":    map[string]any{"type": "integer", "format": "int64"},
						"display":  map[string]any{"type": "string"},
					},
				},
			},
			"invoices": map[string]any{
				"type":        "array",
				"description": "Invoice counts by status, for invoices issued in the period.",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"status", "count"},
					"properties": map[string]any{
						"status": map[string]any{"type": "string"},
						"count":  map[string]any{"type": "integer"},
					},
				},
			},
			"active_customers": map[string]any{
				"type":        "integer",
				"description": "Customers with a live service or an invoice in the period.",
			},
			"open_tickets": map[string]any{"type": "integer", "description": "The support queue depth right now."},
			"recurring": map[string]any{
				"type":        "array",
				"description": "Normalised monthly recurring revenue per currency. Each active subscription is divided by its interval in months, so a quarterly price contributes a third.",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"currency", "scale", "minor", "display", "subscriptions"},
					"properties": map[string]any{
						"currency":      map[string]any{"type": "string"},
						"scale":         map[string]any{"type": "integer"},
						"minor":         map[string]any{"type": "integer", "format": "int64"},
						"display":       map[string]any{"type": "string"},
						"subscriptions": map[string]any{"type": "integer"},
					},
				},
			},
			"exceptions":         map[string]any{"type": "integer", "description": "The exception count from the operator overview, so the dashboard does not have to fetch it twice."},
			"exception_critical": map[string]any{"type": "integer"},
			"available":          map[string]any{"type": "boolean", "description": "False when a figure could not be read. Reporting zero instead would make a broken database look like an empty business."},
			"generated_at":       map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// operatorOperations is the prose for the operator surface.
func operatorOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ settings --
		"GET /api/v1/staff/settings": {
			Summary: "Read system settings, secrets redacted",
			Description: "Returns every group, or one group when `?group=` names it. A secret is never returned: " +
				"its `value` is `[redacted]` and `is_set` says whether a credential is configured. A key with " +
				"no row reads its code-side default with `is_set: false`, so a fresh installation renders " +
				"before an operator saves anything. Reading requires `settings:write` because the page is one " +
				"screen: separating read from write would let a support agent read the SMTP and gateway " +
				"configuration, which is reconnaissance rather than support.",
			Responses: []Response{
				{Status: 200, Description: "The settings groups", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":   map[string]any{"type": "array", "items": settingsGroupSchema()},
						"groups": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				}},
				{Status: 404, Description: "No such settings group", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/settings": {
			Summary: "Write one settings group",
			Description: "A bulk upsert of one group, validated as a whole before anything is stored: a form that " +
				"fails on its last key writes nothing. Every write records `updated_by` and `updated_at` and " +
				"writes an audit row in the same transaction, so the change carries the operator, the " +
				"correlation id and the source address. A secret submitted as `[redacted]` is skipped rather " +
				"than stored, so saving a loaded form cannot overwrite a credential with the marker.",
			RequestSchema: settingsWriteSchema(),
			Responses: []Response{
				{Status: 200, Description: "The group as it now reads", Schema: settingsGroupSchema()},
				{Status: 400, Description: "The group is missing, or a key is not declared", Schema: ProblemRef()},
				{Status: 422, Description: "A value does not parse as its declared kind", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------- announcements --
		"GET /api/v1/staff/announcements": {
			Summary:     "List announcements, drafts included",
			Description: "Every announcement, newest first, with `live` derived from `published` and `expires_at`.",
			Responses:   []Response{{Status: 200, Description: "The announcements"}},
		},
		"GET /api/v1/staff/announcements/{id}": {
			Summary:   "One announcement",
			Responses: []Response{{Status: 200, Description: "The announcement"}, {Status: 404, Description: "No such announcement", Schema: ProblemRef()}},
		},
		"POST /api/v1/staff/announcements": {
			Summary:       "Create an announcement",
			Description:   "A draft unless `published` is true. Publishing stamps `published_at` once, so a draft published later keeps the instant it went live.",
			RequestSchema: announcementWriteSchema(),
			Responses: []Response{
				{Status: 201, Description: "The announcement", Schema: announcementSchema()},
				{Status: 422, Description: "The title is missing or too long, or the placement is not accepted", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/announcements/{id}": {
			Summary:       "Update an announcement",
			Description:   "Omitted `published` keeps the current state, so a client editing only the body does not silently unpublish it.",
			RequestSchema: announcementWriteSchema(),
			Responses: []Response{
				{Status: 200, Description: "The announcement", Schema: announcementSchema()},
				{Status: 404, Description: "No such announcement", Schema: ProblemRef()},
				{Status: 422, Description: "The title is missing or too long, or the placement is not accepted", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/announcements/{id}": {
			Summary: "Delete an announcement",
			Description: "Delete is allowed here, unlike an invoice: an announcement has no legal weight and no record " +
				"references it, so a typo published for a minute is removable. The audit row keeps the fact " +
				"that it existed.",
			Responses: []Response{
				{Status: 200, Description: "Deleted"},
				{Status: 404, Description: "No such announcement", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/announcements": {
			Summary: "Published announcements for the storefront and dashboard",
			Description: "Public, because the storefront is public: a visitor who cannot see \"we are closed for " +
				"maintenance\" has no way to find out before registering. Returns only published, unexpired " +
				"rows, and only the fields a customer may see — the creator and the draft state are operator " +
				"information. `?placement=storefront|dashboard` narrows it; `both` matches either. A " +
				"database failure yields an empty feed rather than a 500, because the storefront must render.",
			Responses: []Response{{Status: 200, Description: "The live announcements"}},
		},

		// ---------------------------------------------------------------- logs --
		"GET /api/v1/staff/logs/email": {
			Summary: "Email delivery log",
			Description: "A viewer over `notification_deliveries`, the durable outbox: what was sent, to whom, and " +
				"whether it worked. Keyset-paginated. Filter by `status`, `kind`, `to` (an address fragment) " +
				"and a `from`/`to_time` window.",
			Responses: []Response{
				{Status: 200, Description: "A page of deliveries", Schema: PageSchema(emailLogSchema())},
				{Status: 400, Description: "The cursor, limit or timestamp is invalid", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/logs/errors": {
			Summary: "Error log",
			Description: "What failed in the process. Keyset-paginated. Filter by `level`, `source` and `q` (a " +
				"fragment of the message). A detail is stored with credential-looking key/value pairs " +
				"already redacted.",
			Responses: []Response{
				{Status: 200, Description: "A page of error rows", Schema: PageSchema(errorLogSchema())},
				{Status: 400, Description: "The cursor or limit is invalid", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/logs/http": {
			Summary: "HTTP request log",
			Description: "What the process served, and how long it took. Keyset-paginated. Filter by `method`, " +
				"`status`, `min_status` and `path` (a prefix). Headers are deliberately not stored: a header " +
				"map is where an Authorization token or a session cookie would be copied into a backup, and " +
				"the query string is stripped from the path for the same reason.",
			Responses: []Response{
				{Status: 200, Description: "A page of requests", Schema: PageSchema(httpLogSchema())},
				{Status: 400, Description: "The cursor, limit or status is invalid", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------------- cron --
		"GET /api/v1/staff/cron": {
			Summary: "Recent cron runs",
			Description: "One row per job run, newest first, so an operator can tell \"nothing was due\" from \"the " +
				"worker is dead\". Keyset-paginated. Filter by `job` and `outcome`; a run still in progress has " +
				"`outcome: running`, a null `finished_at` and a null `duration_ms`.",
			Responses: []Response{
				{Status: 200, Description: "A page of runs", Schema: PageSchema(cronRunSchema())},
				{Status: 400, Description: "The cursor or limit is invalid", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/cron/heartbeat": {
			Summary: "Worker heartbeat",
			Description: "The heartbeat row plus whether it is stale. `stale` is derived from the age, because a " +
				"heartbeat row that exists is not the same as a worker that is alive: a process killed nine " +
				"hours ago leaves a row behind forever. A worker that has never run answers 200 with " +
				"`present: false` rather than a 404, because that is the sentence the operator needs.",
			Responses: []Response{
				{Status: 200, Description: "The heartbeat, or its absence", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"heartbeat": map[string]any{
							"oneOf": []map[string]any{
								heartbeatSchema(),
								map[string]any{"type": "null"},
							},
							"description": "Null when no worker has ever reported. `present` says the same thing, for a client that prefers a boolean.",
						},
						"present":             map[string]any{"type": "boolean"},
						"stale":               map[string]any{"type": "boolean"},
						"stale_after_seconds": map[string]any{"type": "integer"},
					},
				}},
			},
		},

		// ----------------------------------------------------------- dashboard --
		"GET /api/v1/staff/dashboard": {
			Summary: "Dashboard figures for a period",
			Description: "Revenue from the ledger (the revenue account, credits minus debits) so a credit note reduces " +
				"it and a void invoice never counted; invoice counts by status; active customers; the open " +
				"ticket count; normalised monthly recurring revenue from subscriptions; and the exception " +
				"count from the operator overview. Every money figure is a list per currency — there is no " +
				"cross-currency total, because adding currencies would be arithmetic on unrelated quantities. " +
				"`available: false` means a figure could not be read; it is not the same as zero.",
			Responses: []Response{
				{Status: 200, Description: "The dashboard", Schema: dashboardSchema()},
				{Status: 400, Description: "The period is not 7d, 30d or 90d", Schema: ProblemRef()},
			},
		},
	}
}
