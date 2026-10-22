package openapi

// The customer purchase path: registration, the signed-in password change and the
// customer checkout.
//
// The prose here is the contract the storefront and the cart are written against, so it
// says what a client must do as well as what the server returns. Three things it has to
// make unmissable, because getting them wrong is a bug in the client rather than in the
// server:
//
//   - registration never returns a session, and always returns the same 202 body, so
//     the client must send the visitor to sign in (or to check their mail);
//   - checkout takes no customer id and no amount: the session decides whose order it
//     is and the catalog decides what it costs;
//   - a free order has no invoice, which is why `invoice_id` is empty exactly when
//     `activated` is true.

func init() { RegisterDescriptions(purchaseOperations) }

// checkoutLinesSchema is the cart array, shared by the `items` and `lines` spellings.
func checkoutLinesSchema() map[string]any {
	return map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": 50,
		"items": map[string]any{
			"type":     "object",
			"required": []string{"price_version_id"},
			"properties": map[string]any{
				"price_version_id": map[string]any{"type": "string", "format": "uuid"},
				"quantity": map[string]any{
					"type": "integer", "minimum": 1, "maximum": 1000, "default": 1,
					"description": "Refused with quantity_not_allowed when the product sells one at a time.",
				},
				"config": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description": "Chosen config options as {config_option_id: value}. " +
						"Validated against the product's options and priced from the catalog.",
				},
			},
		},
	}
}

func purchaseOperations() map[string]Operation {
	amount := AmountRef()

	return map[string]Operation{
		// ------------------------------------------------------- registration --
		"POST /api/v1/auth/register": {
			Summary: "Create a customer account",
			Description: "Self-registration: one transaction creates the login, the billing account and the link " +
				"between them, then queues the email-verification message.\n\n" +
				"**The response never says whether the address was already registered.** It is always `202` with " +
				"`{status: \"accepted\", verification_required}`. A duplicate still queues a verification message to " +
				"the existing account (which is useful and indistinguishable from the fresh case), and the password " +
				"is hashed before the insert is attempted so the timing matches too. A client must therefore never " +
				"branch on this response to tell the two apart — and must not treat it as a sign-in: no session " +
				"cookie is set. Send the visitor to sign in with the credentials they chose, or to their mail when " +
				"`verification_required` is true.\n\n" +
				"Refusals are only for things the caller can fix: a malformed address (`400 invalid_email`), a " +
				"missing name (`400 invalid_display_name`), a weak password (`422 weak_password`), terms not accepted " +
				"when the installation has terms (`422 tos_required`), and a closed registration " +
				"(`403 registration_disabled`).\n\n" +
				"Rate limited per source address and per submitted address with the credential budget, and " +
				"same-origin enforced. CSRF does not apply: there is no session to bind a token to.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"email", "password", "display_name"},
				"properties": map[string]any{
					"email":        map[string]any{"type": "string", "format": "email", "maxLength": 254},
					"password":     map[string]any{"type": "string", "minLength": 12, "maxLength": 1024},
					"display_name": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
					"accept_tos": map[string]any{
						"type": "boolean", "default": false,
						"description": "Required only when the installation has terms configured.",
					},
				},
			},
			Responses: []Response{
				{Status: 202, Description: "Accepted (whether the account was created or the address already existed)",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"status", "verification_required"},
						"properties": map[string]any{
							"status": map[string]any{"type": "string", "enum": []string{"accepted"}},
							"verification_required": map[string]any{
								"type": "boolean",
								"description": "The installation requires a confirmed address before signing in. " +
									"This is policy, not a fact about the submitted address.",
							},
						},
					}},
				{Status: 400, Description: "The address or the display name is unusable", Schema: ProblemRef()},
				{Status: 403, Description: "Registration is closed, or the request was cross-origin", Schema: ProblemRef()},
				{Status: 422, Description: "The password is too weak, or terms were not accepted", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/auth/password/change": {
			Summary: "Change your own password",
			Description: "For a signed-in user rotating a credential they still know. The current password is verified " +
				"first, then the new one is hashed with the same Argon2id parameters the login path verifies with.\n\n" +
				"Success revokes **every other session** and keeps the caller's own: a password change is what someone " +
				"does when they suspect the old one leaked, so every other device must be signed out, while the device " +
				"that just proved the password must not be. The response reports how many sessions were revoked.\n\n" +
				"A wrong current password is `401 invalid_credentials` (safe to say: the caller already holds a " +
				"session). A new password outside the length bounds is `422 weak_password`. An `auth_events` row " +
				"records the change, attributed to the account itself.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"current_password", "new_password"},
				"properties": map[string]any{
					"current_password": map[string]any{"type": "string"},
					"new_password":     map[string]any{"type": "string", "minLength": 12, "maxLength": 1024},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The password was changed and other sessions were revoked",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"status", "sessions_revoked"},
						"properties": map[string]any{
							"status":           map[string]any{"type": "string", "enum": []string{"password_changed"}},
							"sessions_revoked": map[string]any{"type": "integer", "minimum": 0},
						},
					}},
				{Status: 400, Description: "A password was not supplied", Schema: ProblemRef()},
				{Status: 401, Description: "The current password is wrong", Schema: ProblemRef()},
				{Status: 422, Description: "The new password is too weak", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------------- checkout --
		"POST /api/v1/portal/orders": {
			Summary: "Buy products as the signed-in customer",
			Description: "The customer checkout. One transaction re-prices every line from the catalog, locks the " +
				"products, enforces stock and per-user limits, applies an optional coupon and writes the order, one " +
				"subscription per line, and one issued invoice.\n\n" +
				"**There is no customer id and no amount in the request.** The billing account comes from the " +
				"session, and each line names a `price_version_id` whose price is read from the catalog under a lock — " +
				"so a client cannot choose whose order it is or what it costs. Sending `customer_id` is refused as an " +
				"unknown field, not ignored.\n\n" +
				"**A coupon is a line on the invoice, not a lower unit price.** The charge lines keep the catalog " +
				"price so the document explains itself, and the discount appears as its own negative line naming the " +
				"code. The invoice snapshot records the discount amount and the coupon id as evidence.\n\n" +
				"**A zero total skips the invoice (CT-08).** When the order costs nothing, the subscriptions are " +
				"activated and the services are created and queued for provisioning immediately, exactly as a settled " +
				"payment would do. That is why `invoice_id` and `invoice_number` are empty exactly when `activated` " +
				"is true: a free order has nothing to pay, so there is no document to pay.\n\n" +
				"A non-zero order is `201` with an issued invoice; pay it with " +
				"`POST /api/v1/portal/invoices/{id}/checkout`. Stock that ran out is `409 out_of_stock` (the request " +
				"was valid when it was made; the shop changed), and a per-user limit is `409 user_limit_reached`. " +
				"Every coupon refusal has its own code so the cart can say something true. Requires CSRF and an " +
				"`Idempotency-Key` is accepted.",
			RequestSchema: map[string]any{
				"type": "object",
				"description": "Send the cart as `items` (canonical) or `lines` (alias), not both. " +
					"Terms acceptance is `tos_accepted` (canonical) or `accept_tos` (alias).",
				"properties": map[string]any{
					"items": checkoutLinesSchema(),
					"lines": checkoutLinesSchema(),
					"coupon_code": map[string]any{
						"type": "string", "description": "Matched case-insensitively.",
					},
					"tos_accepted": map[string]any{
						"type": "boolean", "default": false,
						"description": "Required only when the installation has terms configured. `accept_tos` is an alias.",
					},
					"accept_tos": map[string]any{
						"type": "boolean", "default": false,
						"description": "Alias for `tos_accepted`.",
					},
				},
				"oneOf": []map[string]any{
					{"required": []string{"items"}},
					{"required": []string{"lines"}},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The order, its invoice (unless the total was zero) and its subscriptions",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"id", "status", "currency", "subtotal", "discount", "tax", "total", "lines"},
						"properties": map[string]any{
							"id":       map[string]any{"type": "string", "format": "uuid"},
							"status":   map[string]any{"type": "string", "enum": []string{"pending_payment", "paid"}},
							"currency": map[string]any{"type": "string"},
							"subtotal": amount,
							"discount": amount,
							"tax":      amount,
							"total":    amount,
							"invoice_id": map[string]any{
								"type": "string", "format": "uuid",
								"description": "Empty when the order was free; there is no document to pay.",
							},
							"invoice_number":   map[string]any{"type": "string"},
							"subscription_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "format": "uuid"}},
							"service_ids": map[string]any{
								"type": "array", "items": map[string]any{"type": "string", "format": "uuid"},
								"description": "Populated only when the order was free and activated immediately.",
							},
							"activated": map[string]any{
								"type":        "boolean",
								"description": "True when the total was zero and the service was delivered without an invoice.",
							},
							"coupon_code": map[string]any{"type": "string"},
							"lines": map[string]any{
								"type": "array",
								"description": "The issued document's lines, the discount line included, so " +
									"the cart total reconciles with what is owed.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"description": map[string]any{"type": "string"},
										"quantity":    map[string]any{"type": "integer"},
										"unit":        amount,
										"net":         amount,
										"tax":         amount,
										"total":       amount,
									},
								},
							},
						},
					}},
				{Status: 400, Description: "No lines, too many lines, both `items` and `lines`, a bad quantity or a mixed-currency cart",
					Schema: ProblemRef()},
				{Status: 403, Description: "No billing account, or an unverified address where verification is required",
					Schema: ProblemRef()},
				{Status: 404, Description: "No such price", Schema: ProblemRef()},
				{Status: 409, Description: "Out of stock, or the per-user limit is reached", Schema: ProblemRef()},
				{Status: 422, Description: "A coupon, a config selection or the terms were refused", Schema: ProblemRef()},
			},
		},
	}
}
