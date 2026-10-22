package openapi

// Notifications: the customer inbox, per-user preferences, and the staff template editor.
//
// The prose matters more here than usual, because two of these routes have a rule that a
// generated client cannot see: a notification belonging to somebody else is 404 and not 403,
// and a force event cannot be turned off. Both are documented on the operations that
// enforce them.

func init() { RegisterDescriptions(notificationOperations) }

// notificationSchema is one inbox row.
func notificationSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "event_key", "title", "unread", "created_at"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"event_key":  map[string]any{"type": "string", "description": "The event that produced this row."},
			"title":      map[string]any{"type": "string"},
			"body":       map[string]any{"type": "string"},
			"url":        map[string]any{"type": "string", "description": "Panel path to open; relative to the panel origin."},
			"unread":     map[string]any{"type": "boolean"},
			"read_at":    map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// preferenceSchema is one line of the preferences screen.
func preferenceSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"event_key", "name", "force", "preference", "mail", "in_app"},
		"properties": map[string]any{
			"event_key": map[string]any{"type": "string"},
			"name":      map[string]any{"type": "string"},
			"force": map[string]any{
				"type": "boolean",
				"description": "The user cannot turn this event off. True for password reset and email " +
					"verification, which are the only ways to complete an action the user started.",
			},
			"preference": map[string]any{
				"type": "string",
				"enum": []string{"force", "choice_on", "choice_off", "never"},
				"description": "The effective preference after precedence is applied. `never` is an " +
					"operator decision and outranks `force`; a force event ignores `choice_off`.",
			},
			"stored": map[string]any{
				"type":        "string",
				"description": "The user's own stored choice, empty when they have never set one. `preference` is what will actually happen.",
			},
			"mail":   map[string]any{"type": "boolean"},
			"in_app": map[string]any{"type": "boolean"},
		},
	}
}

// templateSchema is one editable template.
func templateSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"event_key", "name", "subject", "body", "enabled"},
		"properties": map[string]any{
			"event_key": map[string]any{"type": "string"},
			"name":      map[string]any{"type": "string"},
			"subject": map[string]any{
				"type": "string",
				"description": "Plain text with {{variables}}. Substituted values are stripped of control " +
					"characters, so a value cannot inject a header.",
			},
			"body": map[string]any{
				"type": "string",
				"description": "Plain text with {{variables}}. It is never interpreted as markup and is sent " +
					"as text/plain: an invoice description is customer-influenced, and rendering it as HTML " +
					"would turn the panel into a phishing vector.",
			},
			"in_app_title": map[string]any{"type": "string"},
			"in_app_body":  map[string]any{"type": "string"},
			"in_app_url":   map[string]any{"type": "string"},
			"enabled": map[string]any{
				"type": "boolean", "description": "Master switch. Disabled sends nothing on any channel.",
			},
			"mail_enabled":   map[string]any{"type": "boolean"},
			"in_app_enabled": map[string]any{"type": "boolean"},
			"cc_addresses":   map[string]any{"type": "string", "description": "Comma-separated. Each address receives its own outbox row."},
			"bcc_addresses":  map[string]any{"type": "string", "description": "Comma-separated. Each address receives its own outbox row."},
			"force":          map[string]any{"type": "boolean", "description": "Derived from the event, not editable."},
			"seeded":         map[string]any{"type": "boolean", "description": "False when the event has no template row yet."},
			"updated_at":     map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// templateBodySchema is the writable part of a template.
func templateBodySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"subject", "body"},
		"properties": map[string]any{
			"name":           map[string]any{"type": "string"},
			"subject":        map[string]any{"type": "string"},
			"body":           map[string]any{"type": "string"},
			"in_app_title":   map[string]any{"type": "string"},
			"in_app_body":    map[string]any{"type": "string"},
			"in_app_url":     map[string]any{"type": "string"},
			"enabled":        map[string]any{"type": "boolean"},
			"mail_enabled":   map[string]any{"type": "boolean"},
			"in_app_enabled": map[string]any{"type": "boolean"},
			"cc_addresses":   map[string]any{"type": "string"},
			"bcc_addresses":  map[string]any{"type": "string"},
		},
	}
}

func notificationOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------ customer inbox --
		"GET /api/v1/portal/notifications": {
			Summary: "List the caller's in-app notifications",
			Description: "Newest first, keyset-paginated. The user id comes from the session and is in the WHERE " +
				"clause, so this endpoint cannot be asked for anyone else's inbox. " +
				"`unread=true` returns only unread rows. Each row carries the `url` the inbox should open, " +
				"relative to the panel origin.",
			Responses: []Response{
				{Status: 200, Description: "One page of notifications", Schema: PageSchema(notificationSchema())},
			},
		},
		"GET /api/v1/portal/notifications/unread-count": {
			Summary: "Unread notification count",
			Description: "The number the inbox badge shows. Separate from the list because the shell polls it " +
				"far more often than the list is opened, and fetching twenty notifications to render one integer " +
				"is a slow sidebar for no reason.",
			Responses: []Response{
				{Status: 200, Description: "The count", Schema: map[string]any{
					"type": "object", "required": []string{"unread"},
					"properties": map[string]any{"unread": map[string]any{"type": "integer"}},
				}},
			},
		},
		"POST /api/v1/portal/notifications/{id}/read": {
			Summary: "Mark one notification read",
			Description: "Marking an already-read row succeeds: the caller's intent is satisfied, and an error " +
				"there would make a double click look like a failure. A notification belonging to another " +
				"account updates no row and answers 404, byte-identical to an id that never existed, so the " +
				"endpoint cannot be used to discover which ids are real.",
			Responses: []Response{
				{Status: 200, Description: "Marked read, with the remaining unread count", Schema: map[string]any{
					"type": "object", "required": []string{"status", "unread"},
					"properties": map[string]any{
						"status": map[string]any{"type": "string"},
						"unread": map[string]any{"type": "integer"},
					},
				}},
			},
		},
		"POST /api/v1/portal/notifications/read-all": {
			Summary: "Mark every notification read",
			Description: "One statement, so a partial failure cannot leave a badge showing a number the user " +
				"already dismissed. Reports how many rows changed.",
			Responses: []Response{
				{Status: 200, Description: "All read", Schema: map[string]any{
					"type": "object", "required": []string{"status", "marked", "unread"},
					"properties": map[string]any{
						"status": map[string]any{"type": "string"},
						"marked": map[string]any{"type": "integer"},
						"unread": map[string]any{"type": "integer"},
					},
				}},
			},
		},

		// -------------------------------------------------------- preferences --
		"GET /api/v1/portal/notifications/preferences": {
			Summary: "Read the caller's notification preferences",
			Description: "Every event in the catalogue, resolved for this user, in display order. Events the " +
				"user has never set appear with their default rather than being absent, so the screen shows " +
				"what will actually happen. `force` marks the two events that cannot be turned off.",
			Responses: []Response{
				{Status: 200, Description: "The resolved preferences", Schema: map[string]any{
					"type": "object", "required": []string{"data"},
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": preferenceSchema()},
					},
				}},
			},
		},
		"PUT /api/v1/portal/notifications/preferences": {
			Summary: "Replace the caller's notification preferences",
			Description: "A PUT because the screen submits the whole set. Every entry is validated before any " +
				"is written, so a request that turns three events off and fails on the fourth does not leave " +
				"the account half-configured. " +
				"`choice_off` and `never` against a force event (password reset, email verification) are " +
				"refused with 422 `force_event`: opting out of the reset mail does not protect anyone, it " +
				"makes the account unrecoverable.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"preferences"},
				"properties": map[string]any{
					"preferences": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"event_key", "preference"},
							"properties": map[string]any{
								"event_key":  map[string]any{"type": "string"},
								"preference": map[string]any{"type": "string", "enum": []string{"choice_on", "choice_off", "never"}},
							},
						},
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The resolved preferences after the change", Schema: map[string]any{
					"type": "object", "required": []string{"data"},
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": preferenceSchema()},
					},
				}},
				{Status: 422, Description: "An unknown event, an invalid preference, or an opt-out of a force event", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------- staff templates --
		"GET /api/v1/staff/notification-templates": {
			Summary: "List notification templates",
			Description: "Every event in the catalogue, in display order, whether or not it has a template row. " +
				"An event with no row is reported with `seeded: false` so the omission is visible rather than " +
				"the event silently missing from the screen. Requires notification:write: support answers " +
				"\"why did I get this\", it does not decide what the mail says.",
			Responses: []Response{
				{Status: 200, Description: "The templates", Schema: map[string]any{
					"type": "object", "required": []string{"data"},
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": templateSchema()},
					},
				}},
			},
		},
		"GET /api/v1/staff/notification-templates/{event_key}": {
			Summary: "Read one notification template",
			Description: "The template for one event, plus `force` (derived from the event, not editable) so the " +
				"editor can explain why a channel cannot be turned off.",
			Responses: []Response{
				{Status: 200, Description: "The template", Schema: templateSchema()},
				{Status: 404, Description: "No such event", Schema: ProblemRef()},
				{Status: 503, Description: "The event exists but has no template row yet", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/notification-templates/{event_key}": {
			Summary: "Edit one notification template",
			Description: "The event key is in the path and cannot be changed by the body, so a rename cannot leave " +
				"the old event without wording. Fields not present in the body keep their current value, so an " +
				"editor that only touches the subject does not blank the in-app title. " +
				"The change is audited: it changes what the business tells its customers. " +
				"Bodies are plain text and are never interpreted as markup.",
			RequestSchema: templateBodySchema(),
			Responses: []Response{
				{Status: 200, Description: "The saved template", Schema: templateSchema()},
				{Status: 404, Description: "No such event", Schema: ProblemRef()},
				{Status: 422, Description: "The body is not valid", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/notification-templates/{event_key}/reset": {
			Summary: "Restore a template's shipped wording",
			Description: "Replaces the row with the default this build ships for that event. It is a mutation " +
				"rather than a delete because every event must always have wording: a deleted template would " +
				"mean an event that cannot be sent at all, and the send would fail inside the transaction that " +
				"recorded the customer's change.",
			Responses: []Response{
				{Status: 200, Description: "The restored template", Schema: templateSchema()},
				{Status: 404, Description: "No such event", Schema: ProblemRef()},
			},
		},
	}
}
