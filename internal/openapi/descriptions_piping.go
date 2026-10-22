package openapi

// Email piping descriptions (E3.8 / TKT-11).
//
// The prose lives in its own file for the same reason the route table does: two people
// adding two domains must not edit one block. The parity test compares this map against the
// router in both directions, so a route added without an entry here fails, and an entry
// whose route is gone fails too.

func init() { RegisterDescriptions(pipingOperations) }

// pipingIngestSchema is the body a mail provider posts for one parsed message.
func pipingIngestSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"from", "body"},
		"properties": map[string]any{
			"message_id": map[string]any{
				"type": "string", "maxLength": 998,
				"description": "The email Message-ID. It is the idempotency key: the same id delivered twice " +
					"creates one reply, not two. When it is absent the panel derives a stable one from the " +
					"message's content, so a retry still dedupes.",
			},
			"from": map[string]any{
				"type": "string", "format": "email",
				"description": "The sender. This is the address attribution is checked against, and it is " +
					"untrusted input: a message may only be appended to a ticket whose customer owns this " +
					"address (or a verified login address linked to that customer).",
			},
			"from_name": map[string]any{"type": "string", "maxLength": 200},
			"to": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "format": "email"},
				"description": "Recipients. A ticket id embedded in a local part (support+ticket-<uuid>@…) is " +
					"the second attribution method, after the threading headers.",
			},
			"cc":      map[string]any{"type": "array", "items": map[string]any{"type": "string", "format": "email"}},
			"subject": map[string]any{"type": "string", "maxLength": 500},
			"in_reply_to": map[string]any{
				"type": "string", "maxLength": 998,
				"description": "The threading header. It is matched against stored message ids first, because " +
					"it is the only attribution method a sender cannot guess.",
			},
			"references": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "The References chain. A chain that names more than one ticket is refused as " +
					"ambiguous rather than guessed at.",
			},
			"body": map[string]any{
				"type": "string", "maxLength": 65536,
				"description": "The plain-text body. Over 64 KiB the message is refused and recorded, never " +
					"truncated.",
			},
			"auto_submitted": map[string]any{
				"type": "string",
				"description": "The RFC 3834 header. Any value other than `no` marks an automated message, " +
					"which is dropped before attribution so two machines cannot reply to each other.",
			},
			"headers": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Additional headers. The loop check reads `Auto-Submitted`, `Precedence`, " +
					"`List-Id` and the panel's own outgoing marker from here.",
			},
			"attachments": map[string]any{
				"type":  "array",
				"items": map[string]any{},
				"description": "Attachments are refused, not stored: an inbound file is untrusted input from an " +
					"unauthenticated sender and would bypass the portal's size and content-type checks. The count " +
					"is recorded so an agent can ask for the file through the portal.",
			},
			"received_at": map[string]any{
				"type": "string", "format": "date-time",
				"description": "When the provider received the message. Omitted, the panel's clock is used.",
			},
		},
	}
}

// pipingResultSchema is what the ingest endpoint answers.
func pipingResultSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"outcome", "log_id"},
		"properties": map[string]any{
			"outcome": map[string]any{
				"type": "string",
				"enum": []string{"attached", "created", "duplicate", "ignored", "unattributed"},
				"description": "`attached` is a reply appended to an existing ticket, `created` a new ticket " +
					"opened for the sender, `duplicate` a Message-ID already imported (nothing written), and " +
					"`ignored`/`unattributed` a message that was recorded but attached to nothing.",
			},
			"ticket_id":   map[string]any{"type": "string", "format": "uuid"},
			"customer_id": map[string]any{"type": "string", "format": "uuid"},
			"message_id": map[string]any{
				"type": "string", "format": "uuid",
				"description": "The stored message's id, not the email Message-ID.",
			},
			"log_id": map[string]any{
				"type": "string", "format": "uuid",
				"description": "The inbound mail log row. Every delivery produces one, including a refusal.",
			},
			"attributed_by": map[string]any{
				"type": "string", "enum": []string{"thread", "reply_address", "sender"},
			},
			"reason": map[string]any{
				"type": "string",
				"description": "A stable refusal code: own_address, auto_reply, bulk, sender_mismatch, " +
					"ambiguous_sender, no_customer, ambiguous_thread, unknown_ticket, too_large, empty_body, " +
					"duplicate_message, tickets_disabled.",
			},
			"subject":  map[string]any{"type": "string"},
			"source":   map[string]any{"type": "string", "enum": []string{"webhook", "imap"}},
			"piped_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// pipingMailSchema is one inbound message in the staff view.
func pipingMailSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "status", "from", "subject", "created_at"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"message_id":  map[string]any{"type": "string", "description": "The email Message-ID."},
			"from":        map[string]any{"type": "string", "format": "email"},
			"to":          map[string]any{"type": "string"},
			"subject":     map[string]any{"type": "string"},
			"body":        map[string]any{"type": "string"},
			"status":      map[string]any{"type": "string", "enum": []string{"received", "ignored"}},
			"ticket_id":   map[string]any{"type": "string", "format": "uuid"},
			"customer_id": map[string]any{"type": "string", "format": "uuid"},
			"attributed_by": map[string]any{
				"type": "string", "enum": []string{"thread", "reply_address", "sender"},
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why the message was not attached. Empty when it was.",
			},
			"attachments_dropped": map[string]any{"type": "integer"},
			"source":              map[string]any{"type": "string", "enum": []string{"webhook", "imap"}},
			"created_at":          map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// pipingOperations is the prose for the piping routes.
func pipingOperations() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/webhooks/mail/piping": {
			Summary: "Ingest one inbound email (mail provider webhook)",
			Description: "The provider-facing ingest path. Authenticated by `mail.piping_secret`, presented as " +
				"`X-Piping-Secret` or `Authorization: Bearer …` and compared in constant time; an installation " +
				"with no secret configured refuses every request with 503 piping_not_configured, because an " +
				"ingest endpoint that accepts the empty secret is an open door. The message is attributed by " +
				"threading headers first, then a ticket id in the reply address, then the sender's verified " +
				"address against exactly one customer — and whichever method matches, the sender must be that " +
				"ticket's customer or the message is refused as sender_mismatch rather than appended to a " +
				"stranger's thread. The panel's own address, its own outgoing header, an empty From and the " +
				"RFC 3834 auto-reply markers are dropped before attribution, so a mail loop is impossible. " +
				"Idempotent on the email Message-ID: a redelivery answers 200 with outcome=duplicate and writes " +
				"nothing. A refused message is stored with its reason and answers 202 — the provider must not " +
				"retry it.",
			RequestSchema: pipingIngestSchema(),
			Responses: []Response{
				{Status: 201, Description: "The reply was attached, or a ticket was opened", Schema: pipingResultSchema()},
				{Status: 200, Description: "A duplicate delivery; nothing was written", Schema: pipingResultSchema()},
				{Status: 202, Description: "Recorded but not attached (ignored or unattributed), with a reason", Schema: pipingResultSchema()},
				{Status: 400, Description: "The body is not valid JSON", Schema: ProblemRef()},
				{Status: 401, Description: "The piping secret is missing or wrong", Schema: ProblemRef()},
				{Status: 413, Description: "The envelope is larger than 512 KiB", Schema: ProblemRef()},
				{Status: 503, Description: "Piping is not configured on this installation", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/tickets/piping": {
			Summary: "Inbound mail, including the messages that could not be attached",
			Description: "Every message that arrived by email, newest first, with the outcome and — for a " +
				"message that was not attached — the stable reason (`sender_mismatch`, `ambiguous_sender`, " +
				"`no_customer`, `own_address`, `auto_reply`, `bulk`, `too_large`, …). The body is included so an " +
				"agent can answer a refused message by hand; `attachments_dropped` says that the sender attached " +
				"something the panel refused to store. This is the route that makes \"a reply was never dropped " +
				"silently\" checkable rather than aspirational. Filter with `status=received|ignored|all`.",
			Responses: []Response{
				{Status: 200, Description: "A page of inbound messages", Schema: map[string]any{
					"type":     "object",
					"required": []string{"messages"},
					"properties": map[string]any{
						"messages":    map[string]any{"type": "array", "items": pipingMailSchema()},
						"next_cursor": map[string]any{"type": "string"},
					},
				}},
				{Status: 400, Description: "An unknown status filter, or an invalid cursor", Schema: ProblemRef()},
			},
		},
	}
}
