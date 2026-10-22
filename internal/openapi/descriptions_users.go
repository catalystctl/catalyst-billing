package openapi

// Staff user administration (ADM-12, IDN-01).
//
// This is the document for the screen that closes the panel's headline gap. Before it, the
// only way to create a login was `billing admin create-user`, which needs shell access to
// the host: an administrator could not add a support agent, and nobody could add a second
// administrator, without ssh.
//
// The prose is specific about the two refusals that matter, because a client author wiring a
// "Demote" button needs to know that the server can say no and why:
//
//   - `last_administrator` — the change would leave no active administrator. It is a
//     database invariant (migration 0045), so it holds under two concurrent demotions, not
//     just two sequential ones.
//   - `self_lockout` — an operator disabling or deleting the account they are signed in as.
//
// And about what a password response never contains: a password set here is never returned,
// never stored in the audit row, and never written to a response body.

func init() { RegisterDescriptions(userAdminOperations) }

func userAdminOperations() map[string]Operation {
	// The account shape. `role` is the name the roles screen shows and is empty for a
	// customer-only account, so a client renders "Customer" from one field rather than
	// from a boolean that could disagree with it.
	user := map[string]any{
		"type": "object",
		"required": []string{"id", "email", "display_name", "status", "email_verified",
			"role", "is_staff", "created_at"},
		"properties": map[string]any{
			"id":             map[string]any{"type": "string", "format": "uuid"},
			"email":          map[string]any{"type": "string", "format": "email"},
			"display_name":   map[string]any{"type": "string"},
			"status":         map[string]any{"type": "string", "enum": []string{"active", "disabled", "pending"}},
			"email_verified": map[string]any{"type": "boolean", "description": "Whether the address has been confirmed."},
			"role": map[string]any{"type": "string",
				"description": "The staff role name, empty for a customer-only account. One of administrator, billing, " +
					"support, infrastructure, or a custom role."},
			"is_staff":    map[string]any{"type": "boolean", "description": "True when `role` is set."},
			"customer_id": map[string]any{"type": []string{"string", "null"}, "format": "uuid", "description": "The billing record this login owns. Every account created here has one — a staff member buys and holds services like any customer — so null means the account predates migration 0056 and was not backfilled, which happens only when the address is already another login's billing record."},
			"created_at":  map[string]any{"type": "string", "format": "date-time"},
			"last_sign_in_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time",
				"description": "The last successful sign-in, from the authentication trail rather than from the live " +
					"session list: a revoked session is still evidence that somebody signed in."},
		},
	}

	createRequest := map[string]any{
		"type":     "object",
		"required": []string{"email", "display_name", "password"},
		"properties": map[string]any{
			"email":        map[string]any{"type": "string", "format": "email"},
			"display_name": map[string]any{"type": "string", "maxLength": 200},
			"password": map[string]any{"type": "string", "format": "password",
				"description": "At least 12 characters. The same validator registration uses: a weak one is refused " +
					"with the same 422 `weak_password`."},
			"role": map[string]any{"type": "string",
				"description": "administrator, billing, support, infrastructure, or a custom role name. Omit or send " +
					"an empty string for a customer-only account."},
			"email_verified": map[string]any{"type": "boolean", "default": true,
				"description": "Whether the operator has confirmed the address. Defaults to true: the address was " +
					"given to them by the person on the other end."},
		},
	}

	updateRequest := map[string]any{
		"type":        "object",
		"description": "A partial update. An absent field is left alone; `\"role\": \"\"` removes the staff role and makes the account a customer.",
		"properties": map[string]any{
			"display_name": map[string]any{"type": "string", "maxLength": 200},
			"email": map[string]any{"type": "string", "format": "email",
				"description": "Changing the address clears its verification: a confirmation given for one mailbox does not transfer to another."},
			"role": map[string]any{"type": "string",
				"description": "A role name, or an empty string to demote the account to a customer."},
			"status": map[string]any{"type": "string", "enum": []string{"active", "disabled", "pending"}},
		},
	}

	return map[string]Operation{
		// ------------------------------------------------------------ list --
		"GET /api/v1/staff/users": {
			Summary: "List accounts, staff and customers",
			Description: "One list for both audiences, because the screen is one screen: an operator looking for " +
				"\"the person who rang up\" and an operator looking for \"the support agent we hired\" are asking the " +
				"same question of the same table. Keyset-paginated newest first.\n\n" +
				"`q` searches the address and the display name and needs at least two characters (a one-character " +
				"search matches most of the table and is refused with 400 `invalid_user`). `staff=true` is the agent " +
				"list, `staff=false` the customers; absent is both. `role` matches a role name exactly, `status` one " +
				"of active, disabled or pending.",
			Responses: []Response{
				{Status: 200, Description: "The accounts", Schema: PageSchema(user)},
				{Status: 400, Description: "A short search, an unknown status, or a bad cursor", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- create --
		"POST /api/v1/staff/users": {
			Summary: "Create a login and assign its role",
			Description: "Requires `staff:manage`. The account and its role are written in one transaction: a staff " +
				"account with no role can sign in and do nothing, which is a worse outcome than a failed create.\n\n" +
				"An empty `role` creates a customer-only account, which is the same screen and the same call: a " +
				"support agent onboarding somebody who rang up is not a different job from onboarding an agent.\n\n" +
				"The account is also given its **billing identity** in the same transaction, staff included, so it can " +
				"buy and hold a service without an operator linking two records by hand; `customer_id` in the response " +
				"is that record. An address that already has a billing record with no login attached is adopted rather " +
				"than duplicated — one address is one billing account. An address whose billing record already belongs " +
				"to another login is refused with the same 409 `email_taken`: two logins on one billing account would " +
				"expose that customer's invoices through the customer-scoped endpoints, so it is never shared. The link " +
				"grants nothing else — a staff member's reach is still decided by their role.\n\n" +
				"The password is hashed with the same validator registration uses, so a short one is 422 " +
				"`weak_password` with the same message. No response contains a password.",
			RequestSchema: createRequest,
			Responses: []Response{
				{Status: 201, Description: "The account", Schema: user},
				{Status: 400, Description: "An invalid address, display name, or role name", Schema: ProblemRef()},
				{Status: 409, Description: "That address already has an account, or is already another login's billing record (`email_taken`)", Schema: ProblemRef()},
				{Status: 422, Description: "The password is outside the accepted length (`weak_password`)", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- update --
		"PATCH /api/v1/staff/users/{id}": {
			Summary: "Edit an account, or enable/disable it",
			Description: "A partial update: the screen sends the field the operator touched, and a full replacement " +
				"would let a dialog that only renamed somebody silently clear their role.\n\n" +
				"Two refusals carry their own codes. `self_lockout` (409): an operator cannot disable the account " +
				"they are signed in as. `last_administrator` (409): the change would demote or disable the only active " +
				"administrator. The second is a database invariant — the trigger counts under an advisory transaction " +
				"lock — so two operators demoting two administrators at the same moment cannot both succeed.\n\n" +
				"Disabling revokes the account's sessions as well as its status, so the session list of a disabled " +
				"account does not look like an account somebody is still using.",
			RequestSchema: updateRequest,
			Responses: []Response{
				{Status: 200, Description: "The updated account", Schema: user},
				{Status: 400, Description: "An invalid address, display name, status or role name", Schema: ProblemRef()},
				{Status: 404, Description: "No such account", Schema: ProblemRef()},
				{Status: 409, Description: "`email_taken`, `last_administrator` or `self_lockout`", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- delete --
		"DELETE /api/v1/staff/users/{id}": {
			Summary: "Delete an account that nothing depends on",
			Description: "Deliberately hard, and the empty case is the honest one: an account that has done anything has " +
				"audit rows, and a customer's login is referenced by the customer record and by the invoices behind it. " +
				"Those references are NO ACTION in the schema, so an account with a history cannot be deleted at all. The " +
				"account's own authentication history is the exception: it is a login trail with a retention policy rather " +
				"than an append-only record, and it is removed with the account.\n\n" +
				"The billing identity created with the account is removed with it too, because it holds nothing but an " +
				"address. A billing record an operator or an import created is never deleted here — it carries a name, an " +
				"address and possibly attribution that outlive the login. A record that already holds a service, an " +
				"order or an invoice refuses the delete outright.\n\n" +
				"The refusal is named rather than a foreign-key error: 409 `user_has_history` says what holds the " +
				"account, and 409 `last_administrator` and `self_lockout` cover the two cases where deleting would " +
				"end the panel's own access. Disabling is the operation an operator almost always wants, and it " +
				"preserves the history that makes an incident review possible.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 404, Description: "No such account", Schema: ProblemRef()},
				{Status: 409, Description: "The account is referenced, or is the last administrator, or is your own", Schema: ProblemRef()},
			},
		},

		// -------------------------------------------------------- password --
		"POST /api/v1/staff/users/{id}/password": {
			Summary: "Set another account's password",
			Description: "Sets the password directly rather than mailing a reset link, because this is the onboarding " +
				"path for an installation that may have no mail transport configured — the same situation that made " +
				"the CLI the only way to create the first account. The cost is that the operator knows the password, " +
				"so the screen tells them to have the user change it and the audit row records who set it.\n\n" +
				"Every session of the target is revoked, by the same credential-version bump the reset flow uses: " +
				"whoever held the old password may still be holding a live session. The response reports the number " +
				"of sessions ended and nothing about the credential — no endpoint in this API returns a password. A " +
				"weak one is 422 `weak_password`, the same code and message registration answers with.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"password"},
				"properties": map[string]any{
					"password": map[string]any{"type": "string", "format": "password",
						"description": "At least 12 characters."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The password was set and the target's sessions ended"},
				{Status: 404, Description: "No such account", Schema: ProblemRef()},
				{Status: 422, Description: "The password is outside the accepted length (`weak_password`)", Schema: ProblemRef()},
			},
		},
	}
}
