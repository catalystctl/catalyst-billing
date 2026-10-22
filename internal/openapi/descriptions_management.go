package openapi

// The management surface: creating a customer, editing one, and creating a service for one.
//
// These are the routes that turn the panel from a read-heavy console into something an operator
// can onboard with. Until they existed, a phone order, a manually onboarded account, a comped
// customer and a service added to an existing customer all needed SQL.
//
// The prose states the properties a client cannot infer from the schema, because each one is a
// decision rather than a detail:
//
//   - Creating a customer returns no credential. The account starts with the hash of 32 random
//     bytes nobody kept, so the only way in is the invitation mail, which carries a reset link.
//     `invitation_sent` says whether that message was queued, because "we did not send it" and
//     "we failed to send it" are different answers to a support ticket.
//   - A duplicate address is `409 email_taken`. Self-registration cannot say that (it would be an
//     account-discovery oracle); an authenticated operator about to create a record must be told.
//   - Creating a service never raises an invoice. The response says `invoiced: false` and reports
//     the subscription the renewal engine will act on, or `null` when the service is comped.
//   - Editing a customer never touches an issued invoice. The issuer froze a snapshot of the
//     details it was issued with; a correction changes the next document.

func init() { RegisterDescriptions(managementOperations) }

// billingAddressSchema is the bill-to block, shared by the create and edit bodies.
func billingAddressSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"legal_name": map[string]any{
				"type": "string", "maxLength": 200,
				"description": "The name printed on documents. An empty value falls back to the customer's " +
					"display name, so a document always prints a name.",
			},
			"address_line1": map[string]any{"type": "string", "maxLength": 200},
			"address_line2": map[string]any{"type": "string", "maxLength": 200},
			"city":          map[string]any{"type": "string", "maxLength": 200},
			"postal_code":   map[string]any{"type": "string", "maxLength": 200},
			"region":        map[string]any{"type": "string", "maxLength": 200},
			"country_code": map[string]any{
				"type": "string", "minLength": 2, "maxLength": 2, "pattern": "^[A-Z]{2}$",
				"description": "ISO 3166-1 alpha-2, upper case. It is the jurisdiction the tax engine " +
					"resolves the rate for.",
			},
			"tax_id": map[string]any{"type": "string", "maxLength": 200},
		},
	}
}

// customerWriteSchema is the shape both customer writes return.
func customerWriteSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "email", "name", "has_login"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"email":      map[string]any{"type": "string", "format": "email"},
			"name":       map[string]any{"type": "string"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"has_login": map[string]any{
				"type":        "boolean",
				"description": "Whether a login is attached. A staff-created customer always has one, and it has no password the caller knows.",
			},
			"tax_exempt": map[string]any{
				"type":        "boolean",
				"description": "Read from the billing profile, where the tax engine reads it.",
			},
			"tax_exemption_reason": map[string]any{
				"type":        "string",
				"description": "Why the account is exempt. Empty when it is not. The database keeps the flag and the reason together, so a taxable account cannot carry a stale reason.",
			},
		},
	}
}

// createCustomerRequestSchema is the body of a staff-created customer.
func createCustomerRequestSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"email", "name"},
		"additionalProperties": false,
		"properties": map[string]any{
			"email": map[string]any{
				"type": "string", "format": "email", "maxLength": 254,
				"description": "Stored lower-cased. It is unique across customers and logins, so an address " +
					"that already has a record is refused rather than adopted.",
			},
			"name": map[string]any{
				"type": "string", "minLength": 1, "maxLength": 200,
				"description": "The display name, and the fallback for the legal name on a document.",
			},
			"billing_address": map[string]any{
				"allOf": []any{billingAddressSchema()},
				"description": "Optional. Omitted means no address is stored — an empty profile row would " +
					"claim someone cleared fields they never filled in.",
			},
			"tax_exempt": map[string]any{
				"type":        "boolean",
				"description": "Marks the account exempt from tax. Requires tax_exemption_reason.",
			},
			"tax_exemption_reason": map[string]any{
				"type": "string", "maxLength": 200,
				"description": "Why the account is exempt. A 400 when the flag is set and this is empty: an " +
					"exemption without a reason is a number on a tax return nobody can defend.",
			},
		},
	}
}

// updateCustomerRequestSchema is the body of a partial edit.
func updateCustomerRequestSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":  map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
			"email": map[string]any{"type": "string", "format": "email", "maxLength": 254},
			"billing_address": map[string]any{
				"allOf":       []any{billingAddressSchema()},
				"description": "Replaces the bill-to block when it is present.",
			},
			"tax_exempt": map[string]any{
				"type":        "boolean",
				"description": "Absent leaves the exemption alone. Setting it false clears the reason with it.",
			},
			"tax_exemption_reason": map[string]any{"type": "string", "maxLength": 200},
		},
	}
}

// createServiceRequestSchema is the body of a hand-made service.
func createServiceRequestSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"customer_id", "price_version_id", "reason"},
		"additionalProperties": false,
		"properties": map[string]any{
			"customer_id":      map[string]any{"type": "string", "format": "uuid"},
			"price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"quantity": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 1000, "default": 1,
				"description": "Refused above one for a product whose quantity policy is disabled.",
			},
			"config": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "The chosen config options as {config_option_id: value}. Validated against the " +
					"product's options and priced from the catalog for this price version; an unknown option, " +
					"an unavailable choice or a missing required option is a 400 rather than being ignored.",
			},
			"reason": map[string]any{
				"type": "string", "minLength": 1, "maxLength": 500,
				"description": "Why the service was added. Required, stored on the service and written into " +
					"the audit row: a service with no order behind it is exactly what an auditor asks about.",
			},
			"comp": map[string]any{
				"type": "boolean",
				"description": "Create the service with no subscription, so nothing ever invoices it. The " +
					"absence of the row is what makes that true in the database rather than in a code path.",
			},
		},
	}
}

// createServiceResponseSchema is what a hand-made service returns.
func createServiceResponseSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"service", "invoiced", "comped"},
		"properties": map[string]any{
			"service": map[string]any{
				"type":     "object",
				"required": []string{"id", "status", "origin"},
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "format": "uuid"},
					"customer_id": map[string]any{"type": "string", "format": "uuid"},
					"product_id":  map[string]any{"type": "string", "format": "uuid"},
					"status": map[string]any{
						"type":        "string",
						"description": "'pending' until the queued provisioning operation succeeds.",
					},
					"origin": map[string]any{
						"type":        "string",
						"enum":        []string{"manual"},
						"description": "Every service this endpoint creates is 'manual'. A purchased one is 'purchase'.",
					},
					"reason":     map[string]any{"type": "string"},
					"quantity":   map[string]any{"type": "integer"},
					"created_at": map[string]any{"type": "string", "format": "date-time"},
				},
			},
			"price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"subscription": map[string]any{
				"description": "The schedule the renewal engine will act on, or null when there is none — " +
					"because the service is comped, or because the price is one-time or free.",
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"id":                   map[string]any{"type": "string", "format": "uuid"},
					"status":               map[string]any{"type": "string"},
					"interval":             map[string]any{"type": "string"},
					"current_period_start": map[string]any{"type": "string", "format": "date-time"},
					"current_period_end":   map[string]any{"type": "string", "format": "date-time"},
					"next_billing_at":      map[string]any{"type": "string", "format": "date-time"},
				},
			},
			"config": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"config_option_id": map[string]any{"type": "string", "format": "uuid"},
						"name":             map[string]any{"type": "string"},
						"env_variable":     map[string]any{"type": "string"},
						"value":            map[string]any{"type": "string"},
						"amount":           AmountRef(),
						"setup":            AmountRef(),
					},
				},
				"description": "The priced selection, with the amount captured at selection time so a later " +
					"option-price change cannot silently re-price the service.",
			},
			"invoiced": map[string]any{
				"type": "boolean",
				"enum": []bool{false},
				"description": "Always false. This endpoint is a delivery, not a demand for money: an invoice " +
					"is raised by POST /api/v1/staff/invoices with a product line, which fulfilment " +
					"provisions when it settles.",
			},
			"comped": map[string]any{
				"type":        "boolean",
				"description": "True when no subscription was created, so nothing will ever invoice the service.",
			},
		},
	}
}

// managementOperations is the prose for the management routes.
func managementOperations() map[string]Operation {
	return map[string]Operation{

		// ------------------------------------------------------------ customers --

		"POST /api/v1/staff/customers": {
			Summary: "Create a customer and invite them to set a password",
			Description: "Creates the billing account, a login for it and the link between them, in one " +
				"transaction. The login has no password the caller knows: it stores the hash of 32 bytes of " +
				"server randomness that are generated and discarded, so the only way in is the invitation " +
				"mail, which carries a single-use reset link. No response to this route contains a " +
				"credential.\n\n" +
				"An address that already belongs to a customer or a login is refused with `409 " +
				"email_taken`. That is deliberately different from self-registration, which must not reveal " +
				"whether an address is known: this caller is an authenticated operator about to create a " +
				"record, and the useful answer is \"open that record instead\".\n\n" +
				"`invitation_sent` reports whether the message reached the outbox, and " +
				"`invitation_detail` explains it when it did not. The account exists either way; the " +
				"operator can resend from the customer's page. The address is stamped verified because the " +
				"operator typed it — otherwise a customer could set a password through the reset flow and " +
				"still not sign in on an installation that requires verification.",
			RequestSchema: createCustomerRequestSchema(),
			Responses: []Response{
				{Status: 201, Description: "The customer was created",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"customer", "invitation_sent"},
						"properties": map[string]any{
							"customer":        customerWriteSchema(),
							"billing_profile": map[string]any{"type": "object"},
							"invitation_sent": map[string]any{"type": "boolean"},
							"invitation_detail": map[string]any{
								"type":        "string",
								"description": "Why the invitation was not queued. Absent when it was.",
							},
						},
					}},
				{Status: 400, Description: "An invalid address or name, or an exemption with no reason",
					Schema: ProblemRef()},
				{Status: 409, Description: "The address already has a customer record or a login",
					Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/customers/{id}": {
			Summary: "Edit a customer's identity, address and tax exemption",
			Description: "A partial edit: an absent field is left alone, so a rename cannot blank an address " +
				"the operator did not touch. Changing the address moves the login with it — an account whose " +
				"address and login disagree is two identities that only look like one, and the customer " +
				"would never be able to reset a password at the address just set.\n\n" +
				"`tax_exempt` and `tax_exemption_reason` are written to the billing profile, where the tax " +
				"engine reads them. Setting the flag false clears the reason with it: a leftover sentence on " +
				"a taxable account is a contradiction an auditor reads. Setting it true with no reason is a " +
				"400.\n\n" +
				"This never touches an invoice. An issued document carries a frozen snapshot of the details " +
				"it was issued with, enforced by a trigger, so a correction changes the next document and " +
				"nothing already sent.",
			RequestSchema: updateCustomerRequestSchema(),
			Responses: []Response{
				{Status: 200, Description: "The customer was updated",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"customer"},
						"properties": map[string]any{
							"customer":        customerWriteSchema(),
							"billing_profile": map[string]any{"type": "object"},
						},
					}},
				{Status: 400, Description: "An invalid field, an invalid profile, or an exemption with no reason",
					Schema: ProblemRef()},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
				{Status: 409, Description: "The new address already belongs to another record",
					Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- services --

		"POST /api/v1/staff/services": {
			Summary: "Add a service to an existing customer",
			Description: "Creates the service, its config selection and a queued provisioning operation, " +
				"and — unless it is comped — a subscription whose period is derived from the price's own " +
				"anchor and timezone. That subscription is what the renewal engine scans, so a service " +
				"created here is billed from the next period onwards exactly like a purchased one.\n\n" +
				"**It never raises an invoice, and the response says so.** An invoice is a demand for money " +
				"backed by an immutable document and a ledger posting; a manual creation is the delivery of " +
				"something already agreed — a transfer that cleared, an account onboarded from another " +
				"panel, a comped service. Billing for a service is the separate action `POST " +
				"/api/v1/staff/invoices` with a `product_id` line, which fulfilment provisions when it " +
				"settles. There is one money path, not two.\n\n" +
				"`comp: true` creates the service with no subscription at all, which is how \"this customer " +
				"does not pay for this\" is expressed: nothing will ever invoice it, because there is no row " +
				"to find.\n\n" +
				"Stock, the per-customer limit and the product's quantity policy are enforced by the same " +
				"helpers the customer checkout calls, inside the same transaction, so a manual creation " +
				"cannot bypass a sold-out product. The audit row names the operator and the reason, and the " +
				"service records its origin in `origin`, `origin_reason` and `origin_actor`.",
			RequestSchema: createServiceRequestSchema(),
			Responses: []Response{
				{Status: 201, Description: "The service was created", Schema: createServiceResponseSchema()},
				{Status: 400, Description: "A missing reason, an invalid quantity, or an unavailable option selection",
					Schema: ProblemRef()},
				{Status: 404, Description: "No such customer or price version", Schema: ProblemRef()},
				{Status: 409, Description: "The product is sold out, or the customer holds the maximum of it",
					Schema: ProblemRef()},
			},
		},
	}
}
