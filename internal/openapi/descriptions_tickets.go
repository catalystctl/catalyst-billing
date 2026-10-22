package openapi

// Support ticket descriptions (P4 / TKT-01…TKT-10).
//
// The prose lives in its own file for the same reason the route table does: two
// people adding two domains must not edit one block. The parity test compares this
// map against the router in both directions, so a route added without an entry here
// fails, and an entry whose route is gone fails too.

func init() { RegisterDescriptions(ticketOperations) }

// ticketSchema is the wire shape of a ticket row.
func ticketSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "number", "subject", "status", "priority", "department"},
		"properties": map[string]any{
			"id":            map[string]any{"type": "string", "format": "uuid"},
			"number":        map[string]any{"type": "string", "example": "TKT-000042"},
			"subject":       map[string]any{"type": "string", "maxLength": 200},
			"status":        map[string]any{"type": "string", "enum": []string{"open", "replied", "closed"}},
			"priority":      map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}},
			"department":    map[string]any{"type": "string"},
			"service_id":    map[string]any{"type": "string", "format": "uuid"},
			"assigned_to":   map[string]any{"type": "string", "format": "uuid"},
			"message_count": map[string]any{"type": "integer"},
			"last_reply_at": map[string]any{"type": "string", "format": "date-time"},
			"closed_at":     map[string]any{"type": "string", "format": "date-time"},
			"created_at":    map[string]any{"type": "string", "format": "date-time"},
			"updated_at":    map[string]any{"type": "string", "format": "date-time"},
			"last_message": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from_staff": map[string]any{"type": "boolean"},
					"body":       map[string]any{"type": "string"},
				},
			},
		},
	}
}

// messageSchema is one message in a thread.
func messageSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "ticket_id", "body", "from_staff"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"ticket_id":  map[string]any{"type": "string", "format": "uuid"},
			"from_staff": map[string]any{"type": "boolean"},
			"body":       map[string]any{"type": "string"},
			"message_id": map[string]any{"type": "string", "description": "The email Message-ID a piped reply carried, when any."},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"author": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string", "format": "uuid"},
					"name": map[string]any{"type": "string"},
				},
			},
			"attachments": map[string]any{
				"type":  "array",
				"items": attachmentSchema(),
			},
		},
	}
}

// ticketThreadSchema is a ticket and its messages.
func ticketThreadSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"ticket", "messages"},
		"properties": map[string]any{
			"ticket": ticketSchema(),
			"messages": map[string]any{
				"type":  "array",
				"items": messageSchema(),
			},
			"fields": map[string]any{
				"type":  "array",
				"items": ticketFieldAnswerSchema(),
				"description": "The operator's custom fields, each with this ticket's answer or the fact that it has none. " +
					"An unanswered field that is required to close is what the close refusal names.",
			},
		},
	}
}

// attachmentSchema is one stored file.
func attachmentSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "filename", "content_type", "byte_size", "url"},
		"properties": map[string]any{
			"id":           map[string]any{"type": "string", "format": "uuid"},
			"filename":     map[string]any{"type": "string"},
			"content_type": map[string]any{"type": "string"},
			"byte_size":    map[string]any{"type": "integer", "maximum": 10485760},
			"digest":       map[string]any{"type": "string"},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"url":          map[string]any{"type": "string"},
		},
	}
}

// ticketCreateSchema is the body of a new ticket.
func ticketCreateSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"subject", "message"},
		"properties": map[string]any{
			"subject":    map[string]any{"type": "string", "maxLength": 200},
			"department": map[string]any{"type": "string", "description": "One of the configured departments from GET /api/v1/portal/tickets/meta."},
			"priority":   map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}, "default": "normal"},
			"service_id": map[string]any{"type": "string", "format": "uuid", "description": "The caller's own service the ticket is about, when it is about one."},
			"fields": map[string]any{
				"type":        "object",
				"description": "Answers to the operator's custom fields, keyed by field key. A field marked required_on_create must be answered or the ticket is refused with required_field_missing, naming it.",
			},
			"message": map[string]any{
				"type":     "object",
				"required": []string{"body"},
				"properties": map[string]any{
					"body":       map[string]any{"type": "string"},
					"message_id": map[string]any{"type": "string", "description": "An email Message-ID, for a piped ticket. A duplicate is ignored."},
				},
			},
		},
	}
}

// ticketReplySchema is the body of a reply.
func ticketReplySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"body"},
		"properties": map[string]any{
			"body":       map[string]any{"type": "string"},
			"message_id": map[string]any{"type": "string", "description": "An email Message-ID. A duplicate is ignored and the stored message is returned with duplicate=true."},
		},
	}
}

// ticketOperations is every ticket route's prose.
func ticketOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------- customer reads --
		"GET /api/v1/portal/tickets": {
			Summary: "The customer's tickets",
			Description: "Scoped in SQL: the customer is resolved from the session and the query filters on it, so substituting " +
				"another customer's ticket identifier returns not_found, identical to an id that never existed. " +
				"`status` filters to open, replied or closed; `all` (the default) does not filter.",
			Responses: []Response{
				{Status: 200, Description: "A page of tickets", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tickets":     map[string]any{"type": "array", "items": ticketSchema()},
						"next_cursor": map[string]any{"type": "string"},
					},
				}},
			},
		},
		"GET /api/v1/portal/tickets/meta": {
			Summary: "The ticket form's configuration",
			Description: "The departments the operator configured (from the settings table, with code-side defaults so a missing " +
				"row does not break the screen), the fixed priority and status lists, whether tickets are disabled, whether " +
				"customers may close their own, and the attachment cap in bytes.",
			Responses: []Response{
				{Status: 200, Description: "The ticket configuration", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled":                 map[string]any{"type": "boolean"},
						"client_closing_disabled": map[string]any{"type": "boolean"},
						"departments":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"priorities":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"statuses":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"max_attachment_bytes":    map[string]any{"type": "integer"},
						"max_subject_length":      map[string]any{"type": "integer"},
					},
				}},
			},
		},
		"GET /api/v1/portal/tickets/{id}": {
			Summary: "One ticket thread",
			Description: "The ticket and every message with its attachments' metadata. Another customer's ticket is not_found, " +
				"never forbidden: a 403 would confirm that the identifier exists.",
			Responses: []Response{
				{Status: 200, Description: "The thread", Schema: ticketThreadSchema()},
				{Status: 404, Description: "No such ticket, or it belongs to someone else", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------ customer writes --
		"POST /api/v1/portal/tickets": {
			Summary: "Open a ticket",
			Description: "The customer is the session's, never the body's. Refused with 403 tickets_disabled when the operator has " +
				"switched support off. A service_id must be one of the caller's own services; anything else is not_found. " +
				"A custom field the operator made required at creation must be answered, or the refusal is 400 " +
				"required_field_missing with the field named. " +
				"The ticket number is allocated from the same sequence invoices use, under the `ticket` scope.",
			RequestSchema: ticketCreateSchema(),
			Responses: []Response{
				{Status: 201, Description: "The new thread", Schema: ticketThreadSchema()},
				{Status: 200, Description: "A duplicate email delivery; the existing thread with duplicate=true", Schema: ticketThreadSchema()},
				{Status: 400, Description: "Invalid subject, department, priority, body or custom field", Schema: ProblemRef()},
				{Status: 403, Description: "Tickets are disabled", Schema: ProblemRef()},
				{Status: 409, Description: "A concurrent delivery of the same message id won the race; retry to read it", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/tickets/{id}/replies": {
			Summary: "Reply to a ticket",
			Description: "Appends a customer message and reopens the ticket. A duplicate `message_id` (a redelivered email) is not an " +
				"error: the stored message is returned with duplicate=true and the thread is unchanged.",
			RequestSchema: ticketReplySchema(),
			Responses: []Response{
				{Status: 201, Description: "The new message", Schema: messageSchema()},
				{Status: 200, Description: "A duplicate delivery; the stored message", Schema: messageSchema()},
				{Status: 404, Description: "No such ticket, or it belongs to someone else", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/tickets/{id}/close": {
			Summary: "Close a ticket",
			Description: "Closes the caller's own ticket. Refused with 403 client_closing_disabled when the operator has reserved " +
				"closing to staff, and with 409 required_field_missing when a custom field the operator made required to " +
				"close is still unanswered — the detail names the field.",
			Responses: []Response{
				{Status: 200, Description: "The closed ticket", Schema: ticketSchema()},
				{Status: 403, Description: "Customers may not close tickets", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket, or it belongs to someone else", Schema: ProblemRef()},
				{Status: 409, Description: "A close-required field is unanswered; the detail names it", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------------- attachments --
		"POST /api/v1/portal/tickets/{id}/attachments": {
			Summary: "Attach a file to the customer's own ticket",
			Description: "The body is the file itself, with the name in `?filename=` and the type in Content-Type; `?message_id=` " +
				"selects the message (the newest when omitted). Capped at 10 MB, checked against Content-Length before the body " +
				"is read and again while reading, so an oversized upload is refused with 413 attachment_too_large rather than " +
				"buffered. The type must be on the allowlist (images, PDF, plain text, CSV, JSON, archives and Office documents, " +
				"plus application/octet-stream); anything a browser would render — HTML, SVG, JavaScript, XML — is refused with " +
				"415 attachment_type_not_allowed rather than downgraded. Identical bytes attached to one message collapse to one " +
				"row by digest. This route deliberately does not use the idempotency wrapper, which buffers bodies at 1 MB and " +
				"would refuse a valid 9 MB file with the wrong code.",
			Responses: []Response{
				{Status: 201, Description: "The stored attachment", Schema: attachmentSchema()},
				{Status: 400, Description: "No filename, or an empty body", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket, or it belongs to someone else", Schema: ProblemRef()},
				{Status: 413, Description: "Larger than 10 MB", Schema: ProblemRef()},
				{Status: 415, Description: "The content type is not on the allowlist", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/tickets/attachments/{id}": {
			Summary: "Download an attachment",
			Description: "Served only to the customer who owns the ticket. The stored content type is normalised on upload " +
				"(anything a browser would treat as a document becomes application/octet-stream); the response carries " +
				"X-Content-Type-Options: nosniff, Content-Disposition: attachment, a sandboxing Content-Security-Policy and " +
				"Cache-Control: private, no-store. Another customer's attachment is 404.",
			Responses: []Response{
				{Status: 200, Description: "The file bytes"},
				{Status: 404, Description: "No such attachment, or it belongs to someone else", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/tickets/{id}/attachments": {
			Summary:     "Attach a file to any ticket",
			Description: "Same shape and cap as the customer upload; requires ticket:write:all.",
			Responses: []Response{
				{Status: 201, Description: "The stored attachment", Schema: attachmentSchema()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
				{Status: 413, Description: "Larger than 10 MB", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/tickets/attachments/{id}": {
			Summary:     "Download any attachment",
			Description: "Same headers as the customer download; requires ticket:read:all and a staff role.",
			Responses: []Response{
				{Status: 200, Description: "The file bytes"},
				{Status: 404, Description: "No such attachment", Schema: ProblemRef()},
			},
		},

		// --------------------------------------------------------------- staff --
		"POST /api/v1/staff/tickets": {
			Summary: "Open a ticket on a customer's behalf",
			Description: "An agent answering the phone cannot become the customer, so the customer is named in the body. The " +
				"ticket records both: `opened_by` is the staff account and `opened_by_staff` marks it as staff-opened, while " +
				"`customer_id` is the account it was opened for. The first message is recorded as a staff message, which is what " +
				"the customer's thread shows. A customer_id that does not exist is 404 not_found; a service_id must belong to " +
				"that customer. A custom field the operator made required at creation is enforced here exactly as on the portal.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"customer_id", "subject", "message"},
				"properties": map[string]any{
					"customer_id": map[string]any{"type": "string", "format": "uuid", "description": "The billing account this ticket is opened for."},
					"subject":     map[string]any{"type": "string", "maxLength": 200},
					"department":  map[string]any{"type": "string", "description": "One of the configured departments from GET /api/v1/staff/tickets/meta."},
					"priority":    map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}, "default": "normal"},
					"service_id":  map[string]any{"type": "string", "format": "uuid", "description": "One of the customer's own services, when the ticket is about one."},
					"message": map[string]any{
						"type":     "object",
						"required": []string{"body"},
						"properties": map[string]any{
							"body":       map[string]any{"type": "string"},
							"message_id": map[string]any{"type": "string", "description": "An email Message-ID, for a ticket raised from an email. A duplicate is ignored."},
						},
					},
					"fields": map[string]any{
						"type":        "object",
						"description": "Answers to the operator's custom fields, keyed by field key.",
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The new thread, as staff", Schema: ticketThreadSchema()},
				{Status: 200, Description: "A duplicate email delivery; the existing thread with duplicate=true", Schema: ticketThreadSchema()},
				{Status: 400, Description: "A missing customer_id, or an invalid subject, department, priority or custom field", Schema: ProblemRef()},
				{Status: 403, Description: "Tickets are disabled", Schema: ProblemRef()},
				{Status: 404, Description: "No such customer, or the service is not theirs", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/tickets": {
			Summary: "The support queue",
			Description: "Every ticket, newest first, unscoped because a queue that shows one customer is not a queue. Filters: " +
				"`status`, `priority`, `department`, `assignee` (a staff user id, or `unassigned`), and `q` (at least two " +
				"characters, searched against the subject and the customer's email and name). Each row carries the customer " +
				"and the last message, so the queue renders without a second request.",
			Responses: []Response{
				{Status: 200, Description: "A page of tickets", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tickets":     map[string]any{"type": "array", "items": ticketSchema()},
						"next_cursor": map[string]any{"type": "string"},
					},
				}},
				{Status: 400, Description: "An unknown filter value", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/tickets/meta": {
			Summary: "The queue's configuration",
			Description: "The departments, priorities, statuses and the auto-close threshold, with code-side defaults so a " +
				"missing settings row does not break the filter bar.",
			Responses: []Response{
				{Status: 200, Description: "The ticket configuration", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled":                 map[string]any{"type": "boolean"},
						"client_closing_disabled": map[string]any{"type": "boolean"},
						"departments":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"priorities":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"statuses":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"autoclose_days":          map[string]any{"type": "integer"},
						"max_attachment_bytes":    map[string]any{"type": "integer"},
						"max_subject_length":      map[string]any{"type": "integer"},
					},
				}},
			},
		},
		"GET /api/v1/staff/tickets/{id}": {
			Summary:     "One ticket thread, as staff",
			Description: "The same thread the customer sees, plus the customer record and the assignee's name.",
			Responses: []Response{
				{Status: 200, Description: "The thread", Schema: ticketThreadSchema()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/tickets/{id}/replies": {
			Summary: "Reply as staff",
			Description: "Appends a staff message and sets the ticket to `replied`. A duplicate `message_id` is ignored, exactly " +
				"as on the customer path.",
			RequestSchema: ticketReplySchema(),
			Responses: []Response{
				{Status: 201, Description: "The new message", Schema: messageSchema()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/tickets/{id}/close": {
			Summary: "Close a ticket as staff",
			Description: "Closes any ticket. Not affected by the client_closing_disabled setting, which is about customers. A custom " +
				"field the operator made required to close is enforced for staff too — a rule the person closing the ticket can " +
				"ignore is not a rule — and the 409 names the field.",
			Responses: []Response{
				{Status: 200, Description: "The closed ticket", Schema: ticketSchema()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
				{Status: 409, Description: "A close-required field is unanswered; the detail names it", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/tickets/{id}/assign": {
			Summary: "Assign or unassign a ticket",
			Description: "`{\"assignee\": \"<user id>\"}` assigns; `{\"assignee\": \"\"}` unassigns. The assignee must hold a staff " +
				"role — a ticket is assigned to an operator, not to a customer login.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"assignee"},
				"properties": map[string]any{
					"assignee": map[string]any{"type": "string", "description": "A staff user id, or empty to unassign."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The updated ticket", Schema: ticketSchema()},
				{Status: 400, Description: "The assignee is not staff", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
			},
		},
		"PATCH /api/v1/staff/tickets/{id}": {
			Summary: "Change a ticket's status, priority or department",
			Description: "Every field is optional and an omitted field is left alone; sending none of them is a 400. Setting the " +
				"status to `closed` stamps closed_at; any other status clears it. A change to `closed` is a close, so the " +
				"close-time custom field requirement applies and the 409 names the field.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":     map[string]any{"type": "string", "enum": []string{"open", "replied", "closed"}},
					"priority":   map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}},
					"department": map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The updated ticket", Schema: ticketSchema()},
				{Status: 400, Description: "No fields, or an unknown value", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
				{Status: 409, Description: "A close-required field is unanswered; the detail names it", Schema: ProblemRef()},
			},
		},
	}
}
