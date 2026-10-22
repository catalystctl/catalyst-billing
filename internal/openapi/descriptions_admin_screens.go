package openapi

// The three operator screens that had an API but no route to a human: the notification
// template editor, the automation list, and the service-property catalogue.
//
// Two of these routes exist only because a screen needed them and a client could not honestly
// derive the answer:
//
//   - The template vocabulary is served, not embedded, so the editor cannot offer a
//     placeholder the server would refuse (or hide one it accepts).
//   - The automation list is derived from the code that registers the jobs. A hand-written
//     list in a client would be a list of jobs that used to exist.
//
// The prose on the preview route is the important part: it is the one route here that refuses
// a request, and the refusal is the feature.

func init() { RegisterDescriptions(adminScreenOperations) }

// templateVariableSchema is one placeholder an event supplies.
func templateVariableSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"name", "description", "sample"},
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "The word between the braces, e.g. `invoice_number`."},
			"description": map[string]any{"type": "string"},
			"sample": map[string]any{
				"type": "string",
				"description": "A realistic value, shaped like the real one (same date layout, same money " +
					"formatting), so a preview shows what a customer will read.",
			},
		},
	}
}

// renderedChannelSchema is one channel of a rendered preview.
func renderedChannelSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subject":       map[string]any{"type": "string"},
			"body":          map[string]any{"type": "string"},
			"title":         map[string]any{"type": "string"},
			"url":           map[string]any{"type": "string"},
			"cc_addresses":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"bcc_addresses": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}

// automationJobSchema is one periodic job.
func automationJobSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"name", "interval_seconds", "summary", "disabled", "source"},
		"properties": map[string]any{
			"name":             map[string]any{"type": "string", "description": "The name the job records its runs under."},
			"interval_seconds": map[string]any{"type": "integer", "format": "int64"},
			"interval":         map[string]any{"type": "string", "description": "The same period, as a sentence: `every 5 seconds`."},
			"summary":          map[string]any{"type": "string", "description": "One sentence: what the job does."},
			"disabled": map[string]any{
				"type": "boolean",
				"description": "True when the job is not registered with the worker because its kill switch is set. " +
					"A disabled job does nothing at all: it does not fall back to a default threshold.",
			},
			"kill_switch": map[string]any{
				"type": []string{"string", "null"},
				"description": "The environment variable that stops this job, or null when there is none. " +
					"Read-only: it is read when the worker starts, so changing it is a restart, not a request.",
			},
			"kill_switch_set": map[string]any{"type": "boolean"},
			"switch_effect": map[string]any{
				"type":        "string",
				"description": "Present when the switch stops less than the whole job, e.g. dunning's switch stops suspension only.",
			},
			"settings": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The setting keys the job obeys, e.g. `cronjob.order_cancel_days`.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "The file that registers the job, so an operator can read the code that decides.",
			},
		},
	}
}

func adminScreenOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------- notification template editor --
		"GET /api/v1/staff/notification-template-variables": {
			Summary: "The placeholders each notification event supplies",
			Description: "Every event in the catalogue with its template vocabulary, in display order. " +
				"The editor uses it to offer the placeholders an event can actually fill, and to mark the " +
				"events a customer cannot opt out of (`force`). " +
				"It is served rather than embedded in the client: a copy in the browser would offer a " +
				"placeholder the server refuses and hide one it accepts. `company_name` is listed for every " +
				"event because the dispatcher supplies it to every template.",
			Responses: []Response{
				{Status: 200, Description: "The vocabulary, per event", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data"},
					"properties": map[string]any{
						"data": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":     "object",
								"required": []string{"event_key", "name", "force", "variables"},
								"properties": map[string]any{
									"event_key": map[string]any{"type": "string"},
									"name":      map[string]any{"type": "string"},
									"force": map[string]any{
										"type": "boolean",
										"description": "The customer cannot opt out of this event, so a wording " +
											"change reaches everybody who triggers it.",
									},
									"variables": map[string]any{"type": "array", "items": templateVariableSchema()},
								},
							},
						},
					},
				}},
			},
		},
		"POST /api/v1/staff/notification-templates/{event_key}/preview": {
			Summary: "Render a draft template with sample data",
			Description: "Renders the submitted wording for one event as both channels would send it: the mail " +
				"body with absolute links and a header-safe subject, and the inbox row with panel-relative " +
				"links. Nothing is stored — this is what the editor calls before saving, so an operator sees " +
				"the sentence a customer would read instead of discovering it afterwards. " +
				"A placeholder the event does not supply is refused with 422 `unknown_placeholder` and the " +
				"offending names in `detail`. The renderer leaves an unknown placeholder as literal text, " +
				"which is the right behaviour at send time and the wrong thing to store: the customer would " +
				"read the plumbing. The same check runs inside the save path, so a client that skips the " +
				"preview cannot store an unrenderable template either.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"subject", "body"},
				"properties": map[string]any{
					"name":           map[string]any{"type": "string"},
					"subject":        map[string]any{"type": "string"},
					"body":           map[string]any{"type": "string"},
					"in_app_title":   map[string]any{"type": "string"},
					"in_app_body":    map[string]any{"type": "string"},
					"in_app_url":     map[string]any{"type": "string"},
					"cc_addresses":   map[string]any{"type": "string"},
					"bcc_addresses":  map[string]any{"type": "string"},
					"enabled":        map[string]any{"type": "boolean"},
					"mail_enabled":   map[string]any{"type": "boolean"},
					"in_app_enabled": map[string]any{"type": "boolean"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The draft, rendered", Schema: map[string]any{
					"type":     "object",
					"required": []string{"event_key", "force", "mail", "in_app"},
					"properties": map[string]any{
						"event_key": map[string]any{"type": "string"},
						"force":     map[string]any{"type": "boolean"},
						"mail":      renderedChannelSchema(),
						"in_app":    renderedChannelSchema(),
						"variables": map[string]any{"type": "array", "items": templateVariableSchema()},
					},
				}},
				{Status: 404, Description: "No such event", Schema: ProblemRef()},
				{Status: 422, Description: "The draft uses a placeholder its event does not supply", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------ automation --
		"GET /api/v1/staff/automation": {
			Summary: "Every periodic job the worker runs, with its kill switch",
			Description: "The job list, in registration order, with each job's interval, one-sentence purpose " +
				"and the environment variable that stops it. " +
				"The two lifecycle rules come from `lifecycle.Jobs()` — the function the worker itself calls — " +
				"so their names and intervals are the ones actually scheduled; the rest are read from the " +
				"registration in `cmd/billing/jobs.go`, which is package main and cannot be imported. A job whose " +
				"switch is set is reported with `disabled: true` and the reason, rather than omitted: an operator " +
				"looking for the switch that stops renewals needs to learn that there is not one. " +
				"The switches are read-only here — they are environment variables, so a control on this screen " +
				"would change nothing — and `kill_switches` says so in the response itself.",
			Responses: []Response{
				{Status: 200, Description: "The job list and the switch mechanism", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data", "kill_switches"},
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": automationJobSchema()},
						"kill_switches": map[string]any{
							"type":     "object",
							"required": []string{"mechanism", "read_only", "note"},
							"properties": map[string]any{
								"mechanism": map[string]any{"type": "string", "enum": []string{"environment"}},
								"read_only": map[string]any{"type": "boolean"},
								"note":      map[string]any{"type": "string"},
							},
						},
						"worker": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"embedded_switch": map[string]any{"type": "string"},
								"embedded":        map[string]any{"type": "boolean"},
							},
						},
					},
				}},
				{Status: 503, Description: "No database is configured, so the registration cannot be read", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------ service property catalogue --
		"GET /api/v1/staff/service-properties/usage": {
			Summary: "How many services carry each property definition",
			Description: "One row per definition with the number of services holding that key. It counts " +
				"presence, not a non-empty value, because that is exactly what the database's delete guard " +
				"tests: a service carrying `{\"rack\": \"\"}` still blocks the delete. So the number tells an " +
				"operator whether `DELETE` will be refused before they click it, instead of after.",
			Responses: []Response{
				{Status: 200, Description: "Usage per definition", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data"},
					"properties": map[string]any{
						"data": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":     "object",
								"required": []string{"key", "services"},
								"properties": map[string]any{
									"key":      map[string]any{"type": "string"},
									"services": map[string]any{"type": "integer"},
								},
							},
						},
					},
				}},
			},
		},
		"POST /api/v1/staff/service-properties/reorder": {
			Summary: "Reorder the property definitions",
			Description: "Sets the display order of the definitions in one transaction, renumbering in tens so a " +
				"later single-row edit has room between two entries. The order is the definition's `sort_order`, " +
				"which is also the order a service page and an invoice print them in. " +
				"Keys not named in the request are not deleted: they keep their relative order after the named " +
				"ones, which is what makes the request safe when another operator creates a definition between " +
				"the render and the save. A key named twice, or an empty list, is refused with 422 `invalid_order`; " +
				"a key with no definition is 400 `unknown_property`.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"keys"},
				"properties": map[string]any{
					"keys": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "The property keys, most significant first.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The order after the change, including keys that were not named", Schema: map[string]any{
					"type":     "object",
					"required": []string{"keys"},
					"properties": map[string]any{
						"keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				}},
				{Status: 400, Description: "A key has no definition", Schema: ProblemRef()},
				{Status: 422, Description: "The order is empty or names a key twice", Schema: ProblemRef()},
			},
		},
	}
}
