package openapi

// Account closure, and the staff-side credential operations the customer overview needs.
//
// The prose here is deliberately explicit about two things a client author cannot guess from the
// route table:
//
//   - **A delete is a close.** `DELETE /api/v1/staff/customers/{id}` does not remove a row. It sets
//     `deleted_at`, disables the portal logins behind the customer and keeps every invoice,
//     service and audit row. A client that renders "deleted" and hides the record from search is
//     rendering something the server did not do.
//   - **The refusals are the contract.** Both closure routes answer 409 `account_close_blocked`
//     with a `detail` that names the live service or the unpaid invoice. A screen that only says
//     "that did not work" throws away the one piece of information the customer needs.
//
// The MFA reset is documented next to the password route it sits beside, because the two are the
// same screen: `staff:manage`, an operator acting on somebody else's credential, and an audit row
// naming both parties.

func init() { RegisterDescriptions(accountClosureOperations) }

func accountClosureOperations() map[string]Operation {
	blocker := map[string]any{
		"type":     "object",
		"required": []string{"kind", "id", "label", "status", "detail"},
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"live_service", "unpaid_invoice"},
				"description": "Why the account cannot be closed. A service that is not `terminated`, or an " +
					"issued invoice (`open` or `disputed`) with more total than has been paid."},
			"id":     map[string]any{"type": "string", "format": "uuid"},
			"label":  map[string]any{"type": "string", "description": "The product name, or the invoice number."},
			"status": map[string]any{"type": "string"},
			"detail": map[string]any{"type": "string",
				"description": "One sentence naming the thing, used verbatim in the refusal."},
		},
	}

	effects := map[string]any{
		"type": "object",
		"description": "What closing the account actually does. The confirmation dialog must render these rather " +
			"than its own prose: they come from the same facts the handler enforces.",
		"properties": map[string]any{
			"portal_access": map[string]any{"type": "object", "properties": map[string]any{
				"revoked": map[string]any{"type": "boolean"},
				"detail":  map[string]any{"type": "string"},
			}},
			"services": map[string]any{"type": "object", "properties": map[string]any{
				"terminated": map[string]any{"type": "boolean",
					"description": "Always false. Closing an account never terminates a service, which is why it is refused while one is live."},
				"detail": map[string]any{"type": "string"},
			}},
			"invoices": map[string]any{"type": "object", "properties": map[string]any{
				"written_off": map[string]any{"type": "boolean",
					"description": "Always false. Closing an account never writes off a debt."},
				"detail": map[string]any{"type": "string"},
			}},
			"record": map[string]any{"type": "object", "properties": map[string]any{
				"retained": map[string]any{"type": "boolean",
					"description": "Always true: the customer row, its invoices and the audit trail are kept."},
				"detail": map[string]any{"type": "string"},
			}},
		},
	}

	login := map[string]any{
		"type":     "object",
		"required": []string{"user_id", "email", "status", "has_mfa"},
		"properties": map[string]any{
			"user_id": map[string]any{"type": "string", "format": "uuid"},
			"email":   map[string]any{"type": "string", "format": "email"},
			"status":  map[string]any{"type": "string", "enum": []string{"active", "disabled", "pending"}},
			"has_mfa": map[string]any{"type": "boolean",
				"description": "A *confirmed* TOTP credential. An unconfirmed enrolment is not a second factor."},
			"enrolled_at":              map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"recovery_codes_remaining": map[string]any{"type": "integer", "minimum": 0},
			"enrolment_required": map[string]any{"type": "boolean",
				"description": "An operator reset the factor and the account owes the panel a new one."},
			"reset_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"reset_by": map[string]any{"type": []string{"string", "null"}, "format": "uuid",
				"description": "The operator who last reset the factor, or null."},
		},
	}

	state := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"customer_id": map[string]any{"type": "string", "format": "uuid"},
			"closed":      map[string]any{"type": "boolean"},
			"closed_at":   map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"closed_kind": map[string]any{"type": []string{"string", "null"}, "enum": []any{"self", "staff", nil}},
			"closed_by":   map[string]any{"type": []string{"string", "null"}, "format": "email"},
			"can_close":   map[string]any{"type": "boolean", "description": "True when nothing blocks the close."},
			"blockers":    map[string]any{"type": "array", "items": blocker},
			"effects":     effects,
			"statement":   map[string]any{"type": "string"},
			"confirm_phrase": map[string]any{"type": "string",
				"description": "The phrase the customer types to confirm. Served so the dialog and the server's check cannot drift apart."},
		},
	}

	return map[string]Operation{
		// -------------------------------------------------------- closure --
		"GET /api/v1/portal/account-closure": {
			Summary: "What closing your own account would do, and what currently stops it",
			Description: "The preflight the confirmation dialog is built from. `effects` states what happens to the " +
				"customer's portal access, services, invoices and records, and `blockers` names every live service and " +
				"unpaid invoice standing in the way. `can_close` is the server's own answer, not a client-side guess.\n\n" +
				"Closing is a soft delete: the record, its invoices and its audit trail are kept, and only the login " +
				"stops working. A hard delete is not available even in principle — invoices, orders, services and audit " +
				"rows reference the customer with NO ACTION.",
			Responses: []Response{
				{Status: 200, Description: "The closure state", Schema: state},
				{Status: 404, Description: "The caller has no customer record", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/account-closure": {
			Summary: "Close your own account",
			Description: "Refused with 409 `account_close_blocked` while the account has a live service (any service " +
				"state but `terminated`) or an unpaid invoice (`open` or `disputed` with more total than paid). The " +
				"refusal's `detail` names the service or the invoice, because a generic refusal is a support ticket.\n\n" +
				"`confirm` must be the phrase `GET /api/v1/portal/account-closure` serves; a client that forgets the " +
				"dialog cannot close an account by accident. On success the customer's sessions are revoked and the " +
				"cookies are cleared, so the response is the last thing this session can do.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"confirm"},
				"properties": map[string]any{
					"confirm": map[string]any{"type": "string",
						"description": "The phrase from the preflight, verbatim."},
					"reason": map[string]any{"type": "string", "maxLength": 500,
						"description": "Optional. Recorded on the closure and in the audit trail."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The account is closed"},
				{Status: 409, Description: "A live service or an unpaid invoice blocks it (`account_close_blocked`)", Schema: ProblemRef()},
				{Status: 422, Description: "The confirmation phrase did not match (`confirmation_mismatch`)", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------ mfa --
		"GET /api/v1/portal/mfa": {
			Summary: "Your own second-factor state",
			Description: "Whether a factor is confirmed, how many recovery codes are left, and whether an operator's " +
				"reset has left an enrolment demand outstanding. The identity endpoint does not report this, so without " +
				"it the security screen can only guess — and a \"two-factor is on\" badge that is a guess is worse than " +
				"no badge.",
			Responses: []Response{
				{Status: 200, Description: "The second-factor state"},
			},
		},
		"GET /api/v1/staff/users/{id}/security": {
			Summary: "An account's second-factor state",
			Description: "The same three facts for any account, so the customer overview can offer \"Reset two-factor\" " +
				"only when there is a factor to reset. A screen that guessed would produce a 409 an operator reads as a bug.",
			Responses: []Response{
				{Status: 200, Description: "The second-factor state"},
				{Status: 404, Description: "No such account", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/users/{id}/mfa/reset": {
			Summary: "Remove another account's second factor",
			Description: "The staff-side answer to \"I lost my phone and my recovery codes\". It is deliberately not " +
				"`POST /api/v1/auth/mfa/disable`: that route is self-service and demands the account's own password and a " +
				"current code, which support cannot supply.\n\n" +
				"The reset *removes* an enrolment — credential, recovery codes and any live challenge — and never reveals " +
				"it, so an operator cannot learn the customer's secret or generate a code. It leaves " +
				"`enrolment_required` behind, so the account's next session is told to enrol a new factor rather than " +
				"quietly dropping to password-only, and it is audited as `user.mfa_reset` naming the operator and the " +
				"target. Sessions are not revoked: a lost phone is not a compromised session.\n\n" +
				"409 `mfa_not_enrolled` when there is no confirmed factor to remove.",
			Responses: []Response{
				{Status: 200, Description: "The factor was removed and an enrolment demand is outstanding"},
				{Status: 404, Description: "No such account", Schema: ProblemRef()},
				{Status: 409, Description: "No confirmed second factor to reset (`mfa_not_enrolled`)", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- staff --
		"GET /api/v1/staff/customers/{id}/account-state": {
			Summary: "A customer's closure state and the security of each login",
			Description: "One request for the customer overview's security panel: whether the record is closed, what " +
				"would stop it being closed, and the second-factor state of every login attached to it. `can_close` " +
				"mirrors what `DELETE /api/v1/staff/customers/{id}` would answer, so the button can be offered honestly.",
			Responses: []Response{
				{Status: 200, Description: "Closure state, blockers and login security",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"customer_id":      map[string]any{"type": "string", "format": "uuid"},
							"closed":           map[string]any{"type": "boolean"},
							"closed_at":        map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
							"closed_kind":      map[string]any{"type": []string{"string", "null"}, "enum": []any{"self", "staff", nil}},
							"closed_by":        map[string]any{"type": []string{"string", "null"}, "format": "email"},
							"closed_reason":    map[string]any{"type": "string"},
							"can_close":        map[string]any{"type": "boolean"},
							"blockers":         map[string]any{"type": "array", "items": blocker},
							"logins":           map[string]any{"type": "array", "items": login},
							"confirm_phrase":   map[string]any{"type": "string"},
							"delete_statement": map[string]any{"type": "string"},
						},
					}},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/customers/{id}": {
			Summary: "Close a customer record",
			Description: "A soft close, and the response says so. `deleted_at` is set, the portal logins behind the " +
				"customer are disabled and their sessions revoked, and every invoice, service and audit row is kept: " +
				"nothing cascades. A hard delete is not available even in principle, because invoices, orders, services " +
				"and audit rows reference the customer with NO ACTION — a delete would either fail on a foreign key or " +
				"succeed only by destroying the evidence an auditor needs.\n\n" +
				"Refused with 409 `account_close_blocked` while a service is live (any state but `terminated`) or an " +
				"invoice is unpaid (`open` or `disputed` with more total than paid); the `detail` names them. A second " +
				"delete is 409 `already_closed` rather than a silent repeat. A reason is required: an audit row that " +
				"does not say why is an audit row nobody can act on.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"reason"},
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 500,
						"description": "Why the record is being closed. Recorded in the audit trail."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The record is closed; nothing was deleted"},
				{Status: 400, Description: "The reason is missing or too long (`reason_required`)", Schema: ProblemRef()},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
				{Status: 409, Description: "A live service or an unpaid invoice blocks it, or it is already closed", Schema: ProblemRef()},
			},
		},
	}
}
