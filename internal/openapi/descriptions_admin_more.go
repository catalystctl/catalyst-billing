package openapi

// Descriptions for the four operator screens that had an API and no route to a human: the hold
// register, the product upgrade graph and the security surface (sessions and sign-ins).
//
// The prose lives in its own file for the same reason every other domain does: two people
// adding two screens must not edit one block, and the parity test compares this map against the
// router in both directions, so a route added without an entry fails and an entry whose route is
// gone fails too.
//
// Three conventions apply to every route here:
//
//   - A secret is never in a response body. The provider registry is not in this file (it lives
//     in descriptions_oauth.go) but the same rule governs it: `has_secret` is a boolean, and
//     there is no field that could carry the value.
//   - A mutation writes its audit row inside the transaction that makes the change, so an audit
//     row for a rolled-back change cannot exist.
//   - An operator cannot end the session that is making the request. That refusal is a stable
//     code (`own_session`), because it is a rule rather than an accident.

func init() { RegisterDescriptions(adminMoreOperations) }

// holdRegisterEntrySchema is one hold with the things an operator needs beside it.
func holdRegisterEntrySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"id", "service_id", "service_status", "product_name", "customer", "reason",
			"created_at", "expired",
		},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"service_id": map[string]any{"type": "string", "format": "uuid"},
			"service_status": map[string]any{
				"type": "string",
				"description": "The service's own status. A hold does not change it: the service keeps " +
					"running while billing and automation are told to leave it alone.",
			},
			"generation":   map[string]any{"type": "integer", "format": "int64"},
			"product_name": map[string]any{"type": "string"},
			"product_slug": map[string]any{"type": "string"},
			"customer": map[string]any{
				"type":     "object",
				"required": []string{"id", "email"},
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "format": "uuid"},
					"name":  map[string]any{"type": "string"},
					"email": map[string]any{"type": "string", "format": "email"},
				},
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why the hold was placed. Required on write, because a hold nobody can explain is a hold nobody dares lift.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"expires_at": map[string]any{
				"type": []string{"string", "null"}, "format": "date-time",
				"description": "When the hold lapses. Null means it lasts until a human clears it, " +
					"which is the safe default: a hold that expires on its own is a hold nobody " +
					"notices has lapsed.",
			},
			"expired": map[string]any{
				"type": "boolean",
				"description": "Computed server-side against the same clock the rows were read with, " +
					"so a client's clock cannot disagree with it. A lapsed hold no longer protects the " +
					"service, which is the opposite of what the operator who placed it probably believes.",
			},
			"placed_by": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"user_id":      map[string]any{"type": "string"},
					"email":        map[string]any{"type": "string"},
					"display_name": map[string]any{"type": "string"},
				},
				"description": "The operator who placed the hold. Null only when the row was written " +
					"without an actor at all.",
			},
			"subscription": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"status": map[string]any{"type": "string"},
					"next_billing_at": map[string]any{
						"type": []string{"string", "null"}, "format": "date-time",
						"description": "The honest answer to “when is this customer invoiced next”. " +
							"A hold stops automation, not the calendar.",
					},
				},
			},
			"open_invoices":   map[string]any{"type": "integer"},
			"last_invoice_no": map[string]any{"type": "string"},
		},
	}
}

// upgradeGraphPriceSchema is one current price of a product.
func upgradeGraphPriceSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"price_version_id", "currency", "amount_minor", "interval"},
		"properties": map[string]any{
			"price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"version":          map[string]any{"type": "integer"},
			"name":             map[string]any{"type": "string"},
			"currency":         map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"amount_minor":     map[string]any{"type": "integer", "format": "int64"},
			"setup_minor":      map[string]any{"type": "integer", "format": "int64"},
			"interval": map[string]any{
				"type": "string", "enum": []string{"monthly", "quarterly", "semiannual", "annual"},
			},
			"amount": AmountRef(),
		},
	}
}

// upgradeEdgeSchema is one permitted move.
func upgradeEdgeSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"product_id", "upgrade_to_id", "compatible"},
		"properties": map[string]any{
			"product_id":        map[string]any{"type": "string", "format": "uuid"},
			"product_name":      map[string]any{"type": "string"},
			"product_slug":      map[string]any{"type": "string"},
			"product_hidden":    map[string]any{"type": "boolean"},
			"upgrade_to_id":     map[string]any{"type": "string", "format": "uuid"},
			"upgrade_to_name":   map[string]any{"type": "string"},
			"upgrade_to_slug":   map[string]any{"type": "string"},
			"upgrade_to_hidden": map[string]any{"type": "boolean"},
			"created_at":        map[string]any{"type": "string", "format": "date-time"},
			"compatible": map[string]any{
				"type": "boolean",
				"description": "Whether the two products share a currency and billing interval. " +
					"Upgrade options are only offered in the subscription's own currency and interval " +
					"— this system has no exchange rate — so an incompatible edge is invisible to every " +
					"customer. It is accepted and flagged rather than refused, because staging the " +
					"graph before the prices exist is a real workflow and believing a dead edge works " +
					"is not.",
			},
			"compatible_prices": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "The shared pairs, as `CURRENCY/interval`.",
			},
			"from_prices": map[string]any{"type": "array", "items": upgradeGraphPriceSchema()},
			"to_prices":   map[string]any{"type": "array", "items": upgradeGraphPriceSchema()},
			"warning": map[string]any{
				"type":        "string",
				"description": "Present when the edge cannot be offered, with the reason in words.",
			},
		},
	}
}

// securitySessionSchema is one live session with its owner.
func securitySessionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "user", "created_at", "last_seen_at", "expires_at", "current"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "format": "uuid"},
			"user": map[string]any{
				"type":     "object",
				"required": []string{"id", "email"},
				"properties": map[string]any{
					"id":           map[string]any{"type": "string", "format": "uuid"},
					"email":        map[string]any{"type": "string", "format": "email"},
					"display_name": map[string]any{"type": "string"},
				},
			},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"last_seen_at": map[string]any{"type": "string", "format": "date-time"},
			"expires_at":   map[string]any{"type": "string", "format": "date-time"},
			"ip":           map[string]any{"type": "string"},
			"user_agent":   map[string]any{"type": "string"},
			"current": map[string]any{
				"type":        "boolean",
				"description": "True for the session making the request. It cannot be revoked through this route.",
			},
			"impersonated_by": map[string]any{
				"type": []string{"string", "null"}, "format": "uuid",
				"description": "The operator behind an impersonated session. Present on every row " +
					"(null when the session is the account's own) so a client does not have to " +
					"special-case a missing field.",
			},
			"impersonator": map[string]any{
				"type": []string{"string", "null"},
				"description": "The impersonating operator's email, for the screen that has to say " +
					"who is behind the session.",
			},
		},
	}
}

// signInEntrySchema is one row of the authentication trail.
func signInEntrySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "event", "outcome", "created_at"},
		"properties": map[string]any{
			"id":    map[string]any{"type": "integer", "format": "int64"},
			"event": map[string]any{"type": "string"},
			"outcome": map[string]any{
				"type": "string", "enum": []string{"success", "failure"},
				"description": "Computed from the event code. The vocabulary in `auth_events` is " +
					"stable (`login_succeeded`, `login_failed`, `login_rejected`, `login_throttled`); " +
					"a client that branched on those strings would silently show nothing if one were added.",
			},
			"detail":     map[string]any{"type": "string"},
			"ip":         map[string]any{"type": "string"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"user": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "format": "uuid"},
					"email": map[string]any{"type": "string", "format": "email"},
				},
				"description": "Null for an attempt against an address that has no account — the row " +
					"a spray leaves behind.",
			},
		},
	}
}

// adminMoreOperations is the prose for the hold, upgrade-graph and security routes.
func adminMoreOperations() map[string]Operation {
	return map[string]Operation{

		// ------------------------------------------------------------ holds --

		"GET /api/v1/staff/holds": {
			Summary: "Every hold, with the service, the customer, when it was placed and who placed it",
			Description: "A manual hold is the operator's veto over automation: it outranks dunning, " +
				"suspension and termination. The rows existed and could be placed and cleared from a " +
				"service screen; this is the cross-service register, which is the only way to answer " +
				"“why is this customer not being invoiced” or “who told us to leave this box alone”. " +
				"Each row carries the subscription's next billing date, because a hold stops automation " +
				"and not the calendar: the held service keeps running and keeps renewing. Placing or " +
				"lifting a hold moves no money — no invoice is issued and no credit is posted, and " +
				"lifting one never bills the period it covered. `status=expired` lists the holds whose " +
				"expiry has passed, which is a real state: automation is free to act again.",
			Responses: []Response{
				{Status: 200, Description: "The register, with counts and the server's clock",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"data", "counts", "as_of"},
						"properties": map[string]any{
							"data":        map[string]any{"type": "array", "items": holdRegisterEntrySchema()},
							"next_cursor": map[string]any{"type": []string{"string", "null"}},
							"counts": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"active":        map[string]any{"type": "integer"},
									"expiring_7d":   map[string]any{"type": "integer"},
									"lapsed":        map[string]any{"type": "integer"},
									"never_expires": map[string]any{"type": "integer"},
								},
							},
							"as_of": map[string]any{"type": "string", "format": "date-time"},
						},
					}},
				{Status: 400, Description: "An invalid status or cursor", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------- upgrade graph --

		"GET /api/v1/staff/upgrade-graph": {
			Summary: "The product upgrade graph, with both endpoints and their current prices",
			Description: "`product_upgrades` is one directed table and a downgrade is the same edge read " +
				"backwards, so this is exactly the promise the storefront makes: from this product a " +
				"customer may move to that one. The storefront's upgrade options are computed from it and " +
				"`internal/service` enforces it, so the graph is the whole of the rule. Each edge carries " +
				"the current price of both products — the newest version per currency and interval, which " +
				"is the price a customer would be offered today — because the amount a move actually costs " +
				"is the difference between them scaled by the fraction of the period that is left. " +
				"`compatible` is false when the two products share no currency and interval: upgrade " +
				"options are only offered in the subscription's own currency and interval, so such an edge " +
				"is invisible to every customer until they do. The response also lists every product, so a " +
				"client can offer both ends of a new edge without a second request.",
			Responses: []Response{
				{Status: 200, Description: "The graph", Schema: map[string]any{
					"type":     "object",
					"required": []string{"edges", "products"},
					"properties": map[string]any{
						"edges": map[string]any{"type": "array", "items": upgradeEdgeSchema()},
						"products": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":     "object",
								"required": []string{"id", "name", "slug", "hidden"},
								"properties": map[string]any{
									"id":     map[string]any{"type": "string", "format": "uuid"},
									"name":   map[string]any{"type": "string"},
									"slug":   map[string]any{"type": "string"},
									"hidden": map[string]any{"type": "boolean"},
									"prices": map[string]any{"type": "array", "items": upgradeGraphPriceSchema()},
								},
							},
						},
					},
				}},
			},
		},
		"POST /api/v1/staff/upgrade-graph": {
			Summary: "Add an edge: from this product, a customer may move to that one",
			Description: "An edge is permission to move, not a move: nothing is repriced and nobody is " +
				"charged when it is created. A self-edge is refused by the database " +
				"(`product_upgrades_not_self`) and by this route before the insert, so the answer is the " +
				"stable code `self_edge` rather than a constraint violation surfacing as a 500. An edge " +
				"that already exists is `edge_exists` (409), not a second row. An edge between products " +
				"with no shared currency and interval is accepted and returned with `compatible: false` " +
				"and a `warning`: refusing it would block staging the graph before the prices exist, and " +
				"saying nothing would let an operator believe it works.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"product_id", "upgrade_to_id"},
				"properties": map[string]any{
					"product_id":    map[string]any{"type": "string", "format": "uuid"},
					"upgrade_to_id": map[string]any{"type": "string", "format": "uuid"},
				},
				"additionalProperties": false,
			},
			Responses: []Response{
				{Status: 201, Description: "The edge, with both endpoints' prices", Schema: upgradeEdgeSchema()},
				{Status: 400, Description: "An invalid product id, or a self-edge (`self_edge`)", Schema: ProblemRef()},
				{Status: 404, Description: "No such product", Schema: ProblemRef()},
				{Status: 409, Description: "The edge already exists (`edge_exists`)", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/upgrade-graph/{product_id}/{upgrade_to_id}": {
			Summary: "Remove an edge from the upgrade graph",
			Description: "New upgrade options stop being offered immediately. Services that already moved " +
				"keep the price they moved to, and a pending upgrade whose invoice is unpaid is untouched: " +
				"this removes a permission, it does not reprice anybody. The removal is audited in the " +
				"transaction that makes it.",
			Responses: []Response{
				{Status: 204, Description: "The edge is gone"},
				{Status: 404, Description: "No such edge", Schema: ProblemRef()},
			},
		},

		// -------------------------------------------------------- security --

		"GET /api/v1/staff/security/sessions": {
			Summary: "Every live session, across every account",
			Description: "The portal route answers “where am I signed in”; this one answers the incident " +
				"question: which accounts have sessions open right now, from where, since when, and when " +
				"they were last seen. Expired and revoked sessions are excluded — a row that can no longer " +
				"authenticate is history, and it is already in the sign-in trail. The order is by creation " +
				"time rather than last-seen, because `last_seen_at` changes while a page is being read and " +
				"a keyset cursor over a moving column can skip or repeat a row. `current` marks the session " +
				"making the request, which is the one this surface will not revoke.",
			Responses: []Response{
				{Status: 200, Description: "The sessions, with a summary", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data", "summary"},
					"properties": map[string]any{
						"data":        map[string]any{"type": "array", "items": securitySessionSchema()},
						"next_cursor": map[string]any{"type": []string{"string", "null"}},
						"summary": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"sessions":       map[string]any{"type": "integer"},
								"users":          map[string]any{"type": "integer"},
								"impersonations": map[string]any{"type": "integer"},
							},
						},
					},
				}},
				{Status: 400, Description: "An invalid user_id, cursor or limit", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/security/sessions/{id}": {
			Summary: "Revoke any account's session",
			Description: "The row is marked revoked, so the next request carrying that cookie is anonymous; " +
				"the account's other sessions are untouched. Two things happen in the same transaction as " +
				"the update: an audit row naming the operator and the account, and an `auth_events` entry " +
				"on the affected account — a customer whose session disappears is entitled to see that an " +
				"operator ended it rather than inferring a compromise. The caller's own current session is " +
				"refused with `own_session` (409): that is a sign-out, it belongs on the logout route, and " +
				"doing it by mis-click in a table of identical rows throws the operator out mid-incident.",
			Responses: []Response{
				{Status: 200, Description: "The session was revoked", Schema: map[string]any{
					"type":     "object",
					"required": []string{"status", "user_id"},
					"properties": map[string]any{
						"status":  map[string]any{"type": "string", "enum": []string{"revoked"}},
						"user_id": map[string]any{"type": "string", "format": "uuid"},
						"email":   map[string]any{"type": "string", "format": "email"},
						"current": map[string]any{"type": "boolean"},
					},
				}},
				{Status: 404, Description: "No such session", Schema: ProblemRef()},
				{Status: 409, Description: "That is the caller's own current session (`own_session`)",
					Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/security/sign-ins": {
			Summary: "Successful and failed sign-in attempts, across every account",
			Description: "Read from `auth_events`, which already records `login_succeeded`, " +
				"`login_failed`, `login_rejected` and `login_throttled` with the account and the address — " +
				"so “is this account being attacked” is a question about rows that exist rather than a new " +
				"log to start writing today. `outcome` is computed server-side so a client does not branch " +
				"on the event vocabulary. A row whose account is unknown has `user: null`, which is exactly " +
				"what a spray against made-up addresses leaves behind. The summary is the last 24 hours: " +
				"how many attempts failed, and the accounts and addresses they were aimed at, because a " +
				"single failure is a forgotten password and a hundred against one account is not.",
			Responses: []Response{
				{Status: 200, Description: "The trail, with the last day's summary", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data", "summary"},
					"properties": map[string]any{
						"data":        map[string]any{"type": "array", "items": signInEntrySchema()},
						"next_cursor": map[string]any{"type": []string{"string", "null"}},
						"summary": map[string]any{
							"type":     "object",
							"required": []string{"window_hours", "successes", "failures"},
							"properties": map[string]any{
								"window_hours": map[string]any{"type": "integer"},
								"successes":    map[string]any{"type": "integer"},
								"failures":     map[string]any{"type": "integer"},
								"top_failing_accounts": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type":     "object",
										"required": []string{"label", "count"},
										"properties": map[string]any{
											"label": map[string]any{"type": "string"},
											"count": map[string]any{"type": "integer"},
										},
									},
								},
								"top_failing_ips": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type":     "object",
										"required": []string{"label", "count"},
										"properties": map[string]any{
											"label": map[string]any{"type": "string"},
											"count": map[string]any{"type": "integer"},
										},
									},
								},
							},
						},
					},
				}},
				{Status: 400, Description: "An invalid outcome, cursor or limit", Schema: ProblemRef()},
			},
		},
	}
}
