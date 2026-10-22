package openapi

// Custom ticket field descriptions.
//
// The prose lives here rather than in descriptions_tickets.go for the same reason every other
// domain has its own file: two people adding two things must not edit one block. The definitions,
// the answers and the two requirements are one subject, so they are one map.

func init() { RegisterDescriptions(ticketFieldOperations) }

// ticketFieldSchema is one configured field.
func ticketFieldSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "key", "label", "type", "required_on_create", "required_to_close"},
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "format": "uuid"},
			"key":   map[string]any{"type": "string", "description": "The stable slug a client sends the answer under. Lower-case letters, digits and underscores."},
			"label": map[string]any{"type": "string", "maxLength": 120, "description": "What the field is called on the form, and what a refused close names."},
			"type": map[string]any{
				"type": "string",
				"enum": []string{"text", "select", "checkbox"},
			},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The option set of a select field; empty for the other types.",
			},
			"required_on_create": map[string]any{"type": "boolean", "description": "A new ticket is refused while this is unanswered."},
			"required_to_close":  map[string]any{"type": "boolean", "description": "Closing is refused while this is unanswered, and the refusal names the field."},
			"sort_order":         map[string]any{"type": "integer"},
			"archived":           map[string]any{"type": "boolean", "description": "A retired field: no longer asked, no longer required, existing answers kept."},
			"answer_count":       map[string]any{"type": "integer", "description": "How many tickets carry an answer."},
			"created_at":         map[string]any{"type": "string", "format": "date-time"},
			"updated_at":         map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// ticketFieldAnswerSchema is a field plus one ticket's answer to it.
func ticketFieldAnswerSchema() map[string]any {
	schema := ticketFieldSchema()
	props, _ := schema["properties"].(map[string]any)
	props["answered"] = map[string]any{"type": "boolean"}
	props["value"] = map[string]any{
		"description": "A string for text and select, a boolean for checkbox, null when unanswered. Null is not the same as \"\" or false.",
		"nullable":    true,
	}
	props["answered_at"] = map[string]any{"type": "string", "format": "date-time"}
	schema["required"] = append(schema["required"].([]string), "answered")
	return schema
}

// ticketFieldInputSchema is the body of a create or a change.
func ticketFieldInputSchema(creating bool) map[string]any {
	required := []string{}
	if creating {
		required = []string{"key", "label", "type"}
	}
	return map[string]any{
		"type":     "object",
		"required": required,
		"properties": map[string]any{
			"key":                map[string]any{"type": "string", "description": "Immutable in practice: clients send answers under it."},
			"label":              map[string]any{"type": "string", "maxLength": 120},
			"type":               map[string]any{"type": "string", "enum": []string{"text", "select", "checkbox"}},
			"options":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Required for a select, refused for the other types."},
			"required_on_create": map[string]any{"type": "boolean"},
			"required_to_close":  map[string]any{"type": "boolean"},
			"sort_order":         map[string]any{"type": "integer"},
			"archived":           map[string]any{"type": "boolean", "description": "true retires the field, false restores it."},
		},
	}
}

// ticketFieldValuesSchema is the body of an answer write.
func ticketFieldValuesSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"values"},
		"properties": map[string]any{
			"values": map[string]any{
				"type": "object",
				"description": "Answers keyed by field key. A string for text and select, a boolean for checkbox. " +
					"An omitted key is left alone; null or an empty string clears the answer. An unknown key, a value " +
					"outside a select's options, or a value of the wrong type is refused with invalid_input naming the field.",
				"additionalProperties": map[string]any{"nullable": true},
			},
		},
	}
}

// ticketFieldOperations is every ticket-field route's prose.
func ticketFieldOperations() map[string]Operation {
	return map[string]Operation{
		"GET /api/v1/staff/ticket-fields": {
			Summary: "The configured ticket fields",
			Description: "The definitions, in display order, with the closed set of types. `include_archived=true` adds the " +
				"retired fields, which the settings screen shows so an operator can see what a closed ticket was asked.",
			Responses: []Response{
				{Status: 200, Description: "The field definitions", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fields": map[string]any{"type": "array", "items": ticketFieldSchema()},
						"types":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				}},
			},
		},
		"POST /api/v1/staff/ticket-fields": {
			Summary: "Create a ticket field",
			Description: "A field is text, select or checkbox. `required_on_create` and `required_to_close` are independent: " +
				"the first refuses a new ticket that does not answer it, the second refuses a close. A select needs a " +
				"non-empty option list; the other types refuse one. The key is a slug the API and the stored answer both " +
				"use, so it cannot collide with a ticket column.",
			RequestSchema: ticketFieldInputSchema(true),
			Responses: []Response{
				{Status: 201, Description: "The new field", Schema: ticketFieldSchema()},
				{Status: 400, Description: "An invalid key, label, type or option list", Schema: ProblemRef()},
			},
		},
		"PATCH /api/v1/staff/ticket-fields/{id}": {
			Summary: "Change a ticket field",
			Description: "Every field is optional and an omitted one is left alone. A type change or an option removal is " +
				"refused while tickets already answer the field (409-style invalid_input): an operator adds an option, or " +
				"archives the field and creates a new one. `archived: false` restores a retired field.",
			RequestSchema: ticketFieldInputSchema(false),
			Responses: []Response{
				{Status: 200, Description: "The updated field", Schema: ticketFieldSchema()},
				{Status: 400, Description: "An invalid value, or a change that would strand an answer", Schema: ProblemRef()},
				{Status: 404, Description: "No such field", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/ticket-fields/{id}": {
			Summary: "Retire a ticket field",
			Description: "Archives the definition rather than deleting it: the field stops being asked and stops being required, " +
				"and the answers already given stay readable on the tickets that carry them. A definition that tickets point " +
				"at is never deletable, because those answers are part of a closed ticket's record.",
			Responses: []Response{
				{Status: 200, Description: "The archived field", Schema: ticketFieldSchema()},
				{Status: 404, Description: "No such field", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/portal/tickets/{id}/fields": {
			Summary: "Answer the custom fields on the customer's own ticket",
			Description: "Answers keyed by field key; an omitted key is left alone and null or an empty string clears one. " +
				"The write is scoped to the caller's customer in SQL, so another customer's ticket is 404 not_found. This is " +
				"how a close-time requirement is satisfied: closing is refused until the field is answered, so there has to " +
				"be a way to answer it afterwards.",
			RequestSchema: ticketFieldValuesSchema(),
			Responses: []Response{
				{Status: 200, Description: "Every field, with this ticket's answers", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fields": map[string]any{"type": "array", "items": ticketFieldAnswerSchema()},
					},
				}},
				{Status: 400, Description: "An unknown key, a value outside the options, or a value of the wrong type", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket, or it belongs to someone else", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/tickets/{id}/fields": {
			Summary:       "Answer the custom fields on any ticket",
			Description:   "Same body and rules as the customer route; requires ticket:write:all.",
			RequestSchema: ticketFieldValuesSchema(),
			Responses: []Response{
				{Status: 200, Description: "Every field, with this ticket's answers", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fields": map[string]any{"type": "array", "items": ticketFieldAnswerSchema()},
					},
				}},
				{Status: 400, Description: "An unknown key or an invalid value", Schema: ProblemRef()},
				{Status: 404, Description: "No such ticket", Schema: ProblemRef()},
			},
		},
	}
}
