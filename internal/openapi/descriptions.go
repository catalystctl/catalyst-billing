package openapi

// The curated descriptions.
//
// The route table supplies the method, path, permission and middleware flags; it cannot
// supply prose. This file is that prose, and it is deliberately separate from the
// generator so that adding a route without a description is a visible omission (the
// parity test reports it) rather than a silently empty summary.
//
// Every entry here is checked against the route table: a description for a route that
// does not exist is an error, so deleting a route forces its description to be deleted
// too.

// DefaultInfo is the document header.
func DefaultInfo() Info {
	return Info{
		Title:   "Catalyst Billing API",
		Version: "1.0.0",
		Description: "A self-hosted billing panel for hosting providers.\n\n" +
			"Conventions:\n" +
			"* Money is always integer minor units plus an explicit currency; never a float, " +
			"never a decimal string in a request body.\n" +
			"* Errors are application/problem+json with a stable `code`.\n" +
			"* Lists are keyset-paginated: `limit` (default 25, max 100) and an opaque `cursor`.\n" +
			"* Mutations accept `Idempotency-Key`; money actions require it. A replay with the " +
			"same body returns the stored response; a replay with a different body is 422.\n" +
			"* A long action answers 202 with `{operation_id, status_url}` rather than holding " +
			"the connection open.",
		Servers: []string{"http://127.0.0.1:8080"},
	}
}

// Operations returns the description of each handler.
func Operations() map[string]Operation {
	amount := AmountRef()
	ops := map[string]Operation{
		// ---------------------------------------------------------------- core --
		"GET /healthz": {
			Summary:     "Liveness",
			Description: "Reports that the process is running. It does not touch the database, so a database blip does not restart every instance.",
			Responses:   []Response{{Status: 200, Description: "The process is alive"}},
		},
		"GET /readyz": {
			Summary:     "Readiness",
			Description: "Reports that this instance can serve traffic: the database is reachable and the schema is current.",
			Responses: []Response{
				{Status: 200, Description: "Ready to serve"},
				{Status: 503, Description: "Not ready", Schema: ProblemRef()},
			},
		},
		"POST /webhooks/{provider}": {
			Summary: "Ingest one provider webhook delivery",
			Description: "The body is bounded, the signature is verified over the exact bytes received, and the event is " +
				"stored before the delivery is acknowledged. A failure before persistence returns a non-2xx so the provider retries.",
			Responses: []Response{
				{Status: 202, Description: "The event is stored and will be processed"},
				{Status: 200, Description: "The delivery was already ingested"},
				{Status: 400, Description: "The signature did not verify", Schema: ProblemRef()},
				{Status: 413, Description: "The body exceeds the configured limit", Schema: ProblemRef()},
			},
		},
		"GET /metrics": {
			Summary:     "Prometheus text exposition",
			Description: "Process-local counters describing what this instance has seen. Staff-only: it exposes money and customer counts.",
			Responses:   []Response{{Status: 200, Description: "Metrics"}},
		},
		"GET /api/v1/ops/overview": {
			Summary:     "Operator exception overview",
			Description: "One request, independent checks. A slow database fails this endpoint rather than degrading it silently.",
			Responses:   []Response{{Status: 200, Description: "The overview"}},
		},
		"POST /api/v1/auth/login": {
			Summary: "Exchange credentials for a session",
			Description: "Sets an HttpOnly session cookie and returns the CSRF token. When a confirmed MFA credential exists the " +
				"response is 200 with `mfa_required` and a challenge token instead, and no session cookie is set.",
			Responses: []Response{
				{Status: 200, Description: "Signed in, or an MFA challenge"},
				{Status: 401, Description: "Wrong credentials", Schema: ProblemRef()},
				{Status: 429, Description: "Throttled", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/auth/logout": {
			Summary:     "Revoke the current session",
			Description: "Revokes the session server-side and clears both cookies. The cookie is cleared even when the revoke call fails.",
			Responses:   []Response{{Status: 200, Description: "Signed out"}},
		},
		"GET /api/v1/auth/me": {
			Summary:     "The current identity and its permissions",
			Description: "The UI hides actions with this list, and the server refuses them independently.",
			Responses:   []Response{{Status: 200, Description: "The identity"}},
		},
		"GET /api/v1/events": {
			Summary: "Server-sent stream of committed changes",
			Description: "Filtered per caller: a customer sees only their own events, staff only events whose permission they hold. " +
				"`Last-Event-ID` resumes after that id; nothing older is replayed. Heartbeat every 20s.",
			Responses: []Response{{Status: 200, Description: "text/event-stream"}},
		},
		"GET /": {
			Summary:     "The embedded application shell",
			Description: "Serves the SPA. An unknown non-API path falls back to the shell; an unknown API path stays a JSON 404.",
			Responses:   []Response{{Status: 200, Description: "The shell"}},
		},

		// -------------------------------------------------------------- portal --
		"GET /api/v1/portal/overview": {
			Summary:     "The customer landing page",
			Description: "Balance per currency, upcoming renewals, open and recent invoices. The customer is resolved from the session, never from the URL.",
			Responses:   []Response{{Status: 200, Description: "The overview"}},
		},
		"GET /api/v1/portal/invoices": {
			Summary:     "The customer's invoices",
			Description: "Scoped in SQL: substituting another customer's identifier returns not_found, identical to a nonexistent id.",
			Responses:   []Response{{Status: 200, Description: "The invoices"}},
		},
		"GET /api/v1/portal/invoices/{id}": {
			Summary:   "One invoice",
			Responses: []Response{{Status: 200, Description: "The invoice"}},
		},
		"GET /api/v1/portal/services": {
			Summary:   "The customer's services",
			Responses: []Response{{Status: 200, Description: "The services"}},
		},
		"GET /api/v1/portal/services/{id}": {
			Summary:   "One service",
			Responses: []Response{{Status: 200, Description: "The service"}},
		},
		"POST /api/v1/portal/services/{id}/cancel": {
			Summary: "Schedule cancellation at the end of the paid period",
			Description: "A scheduled intent, not an immediate state change: the service keeps running and still renews its final period. " +
				"The response states the date the service ends.",
			Responses: []Response{{Status: 200, Description: "Cancellation scheduled"}},
		},
		"GET /api/v1/portal/billing-profile": {
			Summary:   "The customer's own billing details",
			Responses: []Response{{Status: 200, Description: "The profile"}},
		},
		"PUT /api/v1/portal/billing-profile": {
			Summary:     "Update the customer's own billing details",
			Description: "Input to the next invoice, never a rewrite of a past one: the issuer snapshots these details at issuance.",
			Responses: []Response{
				{Status: 200, Description: "The saved profile"},
				{Status: 400, Description: "The profile is invalid", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/invoices/{id}/checkout": {
			Summary: "Start or reuse a payment session",
			Description: "The amount comes from the invoice under a row lock; the body is empty and no amount is ever accepted from the client. " +
				"A second call reuses the existing session instead of opening a second one at the gateway.",
			Responses: []Response{
				{Status: 200, Description: "A redirect URL and the exact amount",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"attempt_id":   map[string]any{"type": "string", "format": "uuid"},
							"invoice_id":   map[string]any{"type": "string", "format": "uuid"},
							"redirect_url": map[string]any{"type": "string"},
							"amount":       amount,
							"reused":       map[string]any{"type": "boolean"},
						},
					}},
				{Status: 402, Description: "The invoice is not payable", Schema: ProblemRef()},
				{Status: 409, Description: "The invoice is already settled", Schema: ProblemRef()},
				{Status: 503, Description: "No gateway is configured", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- account --
		"POST /api/v1/auth/password-reset/request": {
			Summary: "Request a password reset",
			Description: "Always 202 with an identical body, whether or not the address exists, and the dummy-hash path is taken so the " +
				"timing matches too. The response never contains the token.",
			Responses: []Response{{Status: 202, Description: "Accepted"}},
		},
		"POST /api/v1/auth/password-reset/complete": {
			Summary:     "Complete a password reset",
			Description: "Single use, 30-minute token, stored as a hash. Success bumps `credential_version`, which revokes every session.",
			Responses: []Response{
				{Status: 200, Description: "The password was changed"},
				{Status: 400, Description: "The token is unknown, used or expired", Schema: ProblemRef()},
				{Status: 422, Description: "The new password is too weak", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/auth/verify/request": {
			Summary:   "Request an email verification message",
			Responses: []Response{{Status: 202, Description: "Accepted"}},
		},
		"GET /api/v1/auth/verify": {
			Summary:   "Verify an address with a token",
			Responses: []Response{{Status: 200, Description: "Verified"}, {Status: 400, Description: "Invalid token", Schema: ProblemRef()}},
		},
		"POST /api/v1/auth/mfa/enrol": {
			Summary:     "Start TOTP enrolment",
			Description: "Returns the secret and otpauth URL once. The credential stays unconfirmed until a code is presented.",
			Responses:   []Response{{Status: 200, Description: "Secret and otpauth URL"}},
		},
		"POST /api/v1/auth/mfa/confirm": {
			Summary:     "Confirm TOTP enrolment",
			Description: "Returns ten recovery codes, shown once, each single use and stored hashed.",
			Responses:   []Response{{Status: 200, Description: "Recovery codes"}, {Status: 401, Description: "Wrong code", Schema: ProblemRef()}},
		},
		"POST /api/v1/auth/mfa/disable": {
			Summary:     "Disable TOTP for the current user",
			Description: "Requires both the password and a valid code: a stolen session alone must not be able to remove the second factor.",
			Responses:   []Response{{Status: 204, Description: "Disabled"}},
		},
		"POST /api/v1/auth/mfa/verify": {
			Summary:     "Complete the second login step",
			Description: "Exchanges a challenge token plus a code for a session. A wrong code does not consume the challenge.",
			Responses: []Response{
				{Status: 200, Description: "Signed in"},
				{Status: 401, Description: "Wrong code or expired challenge", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/api-keys": {
			Summary:     "List API keys",
			Description: "Never returns a key value: only the prefix, the scopes and the metadata.",
			Responses:   []Response{{Status: 200, Description: "The keys"}},
		},
		"POST /api/v1/staff/api-keys": {
			Summary:     "Create an API key",
			Description: "The key is generated server-side, returned once, and stored only as a hash plus an eight-character prefix.",
			Responses:   []Response{{Status: 201, Description: "The key, shown once"}},
		},
		"DELETE /api/v1/staff/api-keys/{id}": {
			Summary:   "Revoke an API key",
			Responses: []Response{{Status: 204, Description: "Revoked"}},
		},

		// ---------------------------------------------------------- staff reads --
		"GET /api/v1/staff/customers": {
			Summary:     "Customer list with search",
			Description: "Searches email and name; reports open invoices, outstanding per currency, service count and whether the customer has a login.",
			Responses:   []Response{{Status: 200, Description: "A page of customers"}},
		},
		"GET /api/v1/staff/customers/{id}": {
			Summary:     "Customer detail",
			Description: "Joins billing, services, subscriptions, holds, the last invoices and recent authentication events in one response.",
			Responses:   []Response{{Status: 200, Description: "The customer"}},
		},
		"GET /api/v1/staff/customers/{id}/billing-profile": {
			Summary:   "A customer's billing details",
			Responses: []Response{{Status: 200, Description: "The profile"}},
		},
		"GET /api/v1/staff/customers/{id}/portal": {
			Summary:     "A customer's portal overview, as they see it",
			Description: "The same payload the customer's own /api/v1/portal/overview returns, resolved for the customer in the path. Read-only: it cannot pay, cancel or change anything. Every view is audited.",
			Responses: []Response{
				{Status: 200, Description: "The portal overview"},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/customers/{id}/portal/invoices": {
			Summary:     "A customer's invoices, as they see them",
			Description: "The customer-scoped invoice list. Read-only and audited.",
			Responses: []Response{
				{Status: 200, Description: "The invoice list"},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/customers/{id}/portal/services": {
			Summary:     "A customer's services, as they see them",
			Description: "The customer-scoped service list. Read-only and audited.",
			Responses: []Response{
				{Status: 200, Description: "The service list"},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/customers/{id}/portal/billing-profile": {
			Summary:     "A customer's billing profile, as they see it",
			Description: "The billing details the customer entered, resolved for the customer in the path. Read-only and audited.",
			Responses: []Response{
				{Status: 200, Description: "The profile"},
				{Status: 404, Description: "No such customer", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/customers/{id}/billing-profile": {
			Summary:     "Update a customer's billing details",
			Description: "Applied to the next invoice only; an issued invoice's snapshot is frozen by a database trigger.",
			Responses:   []Response{{Status: 200, Description: "The saved profile"}, {Status: 400, Description: "Invalid profile", Schema: ProblemRef()}},
		},
		"GET /api/v1/staff/invoices": {
			Summary:     "Invoice list across all customers",
			Description: "Filter by status, customer, currency, issue date and overdue. `overdue` is derived, never stored.",
			Responses:   []Response{{Status: 200, Description: "A page of invoices"}},
		},
		"GET /api/v1/staff/invoices/{id}": {
			Summary:     "Invoice detail",
			Description: "Includes the snapshot verbatim, the lines, the payments and their allocations, refunds, credit notes and the journal postings.",
			Responses:   []Response{{Status: 200, Description: "The invoice"}},
		},
		"GET /api/v1/staff/services": {
			Summary:     "Service list",
			Description: "Includes the open-operation count and the newest failure reason, so a stuck service is explainable without a second request.",
			Responses:   []Response{{Status: 200, Description: "A page of services"}},
		},
		"GET /api/v1/staff/services/{id}": {
			Summary:   "Service detail",
			Responses: []Response{{Status: 200, Description: "The service"}},
		},
		"GET /api/v1/staff/services/{id}/suspension-eligibility": {
			Summary:     "Evaluate suspension eligibility",
			Description: "Separate from the detail read because it records a `suspension_decisions` row: a read endpoint that writes is surprising.",
			Responses:   []Response{{Status: 200, Description: "The decision and its reason"}},
		},
		"GET /api/v1/staff/services/{id}/holds": {
			Summary:   "Holds on a service",
			Responses: []Response{{Status: 200, Description: "The holds"}},
		},
		"POST /api/v1/staff/services/{id}/holds": {
			Summary:     "Place a manual hold",
			Description: "`created_by` is the session's user id, never a body field. A hold with no expiry blocks automation until a human clears it.",
			Responses:   []Response{{Status: 201, Description: "The hold"}},
		},
		"DELETE /api/v1/staff/holds/{hold_id}": {
			Summary:   "Clear a manual hold",
			Responses: []Response{{Status: 204, Description: "Cleared"}},
		},
		"GET /api/v1/staff/orders": {
			Summary:   "Order list",
			Responses: []Response{{Status: 200, Description: "A page of orders"}},
		},
		"GET /api/v1/staff/orders/{id}": {
			Summary:     "Order detail",
			Description: "Shows the price snapshot each line was sold at and the invoice the order produced.",
			Responses:   []Response{{Status: 200, Description: "The order"}},
		},
		"POST /api/v1/staff/orders": {
			Summary: "Buy a product for a billing account",
			Description: "Prices one catalog price version and writes the order, its items, a pending subscription and an issued " +
				"invoice in one transaction. `customer_id` may be omitted, in which case the acting staff member's own billing " +
				"account is used and created from their login if it does not exist; no `customer_accounts` link is required " +
				"because that table attaches a customer login to an account. The price is read from the catalog, never from the " +
				"body, so a caller cannot choose what to charge.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"price_version_id"},
				"properties": map[string]any{
					"customer_id":      map[string]any{"type": "string", "format": "uuid", "description": "Billing account; omit to bill yourself."},
					"price_version_id": map[string]any{"type": "string", "format": "uuid"},
					"quantity":         map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 1},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The order and its invoice"},
				{Status: 400, Description: "The quantity or the price is not sellable", Schema: ProblemRef()},
				{Status: 404, Description: "No such price or customer", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/payments": {
			Summary:   "Payment history",
			Responses: []Response{{Status: 200, Description: "A page of payments"}},
		},
		"GET /api/v1/staff/payments/{id}": {
			Summary:     "Payment detail",
			Description: "A payment is immutable once recorded; a wrong amount is corrected by a refund or a credit note.",
			Responses:   []Response{{Status: 200, Description: "The payment"}},
		},
		"GET /api/v1/staff/operations": {
			Summary:     "Operation queue",
			Description: "`lease_expires_at` tells the operator whether to wait for a reaper or intervene.",
			Responses:   []Response{{Status: 200, Description: "A page of operations"}},
		},
		"GET /api/v1/staff/operations/{id}": {
			Summary:     "One operation",
			Description: "The status URL a long action returns.",
			Responses:   []Response{{Status: 200, Description: "The operation", Schema: OperationSchema()}},
		},
		"GET /api/v1/staff/webhooks": {
			Summary:   "Webhook inbox",
			Responses: []Response{{Status: 200, Description: "A page of events"}},
		},
		"GET /api/v1/staff/catalog/products": {
			Summary:     "Catalog with price versions",
			Description: "Every product including hidden ones, each with its price versions newest first. Requires invoice:read:all: pricing an invoice must not require the right to change a price.",
			Responses:   []Response{{Status: 200, Description: "The products"}},
		},
		"GET /api/v1/staff/catalog/products/{id}": {
			Summary:     "One product",
			Description: "`active_subscriptions` explains why a price version cannot be deleted.",
			Responses:   []Response{{Status: 200, Description: "The product"}},
		},
		"GET /api/v1/staff/catalog/categories": {
			Summary:     "Categories",
			Description: "Categories with a product count, ordered by sort_order then name.",
			Responses:   []Response{{Status: 200, Description: "The categories"}},
		},

		// ------------------------------------------------------- catalog writes --
		"POST /api/v1/staff/catalog/categories": {
			Summary:     "Create a category",
			Description: "Requires catalog:write. The slug must be lowercase letters, digits and single hyphens.",
			Responses: []Response{
				{Status: 201, Description: "The category"},
				{Status: 409, Description: "The slug is already in use", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/catalog/categories/{id}": {
			Summary:     "Edit a category",
			Description: "A category is not referenced by an invoice, so its slug may change.",
			Responses: []Response{
				{Status: 200, Description: "The category"},
				{Status: 404, Description: "No such category", Schema: ProblemRef()},
				{Status: 409, Description: "The slug is already in use", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/catalog/categories/{id}": {
			Summary:     "Delete a category",
			Description: "Refused while any product still uses it: deleting would silently ungroup those products.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 409, Description: "Products still use it", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/catalog/products": {
			Summary:     "Create a product",
			Description: "A product starts with no price. Metadata (name, description, logo, category, visibility) is editable; a price is not, so it is added with a separate call.",
			Responses: []Response{
				{Status: 201, Description: "The product"},
				{Status: 409, Description: "The slug is already in use", Schema: ProblemRef()},
				{Status: 422, Description: "Invalid input", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/catalog/products/{id}": {
			Summary:     "Edit a product",
			Description: "Metadata only. There is no price field here on purpose: an issued invoice records the price version it priced from, so changing a price in place would rewrite a document the customer already has.",
			Responses: []Response{
				{Status: 200, Description: "The product"},
				{Status: 404, Description: "No such product", Schema: ProblemRef()},
				{Status: 409, Description: "The slug is already in use", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/catalog/products/{id}": {
			Summary:     "Delete a product",
			Description: "Refused while any service or order item references it. Hiding a product is the supported way to stop selling it.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 409, Description: "Services or orders reference it", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/catalog/products/{id}/prices": {
			Summary:     "Append a price version",
			Description: "The version number is allocated inside the insert, so two concurrent appends cannot both claim version N. An existing version is never modified.",
			Responses: []Response{
				{Status: 201, Description: "The new price version"},
				{Status: 422, Description: "Invalid price", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/catalog/images": {
			Summary:     "Upload a product logo",
			Description: "A raw body whose Content-Type is the image type (image/png, image/jpeg, image/webp or image/svg+xml), up to 256 KiB. Identical bytes collapse to one row by digest, which is what lets the media endpoint serve an immutable cache header.",
			Responses: []Response{
				{Status: 201, Description: "The stored image"},
				{Status: 413, Description: "Larger than 256 KiB", Schema: ProblemRef()},
				{Status: 415, Description: "Unsupported image type", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------ storefront --
		"GET /api/v1/catalog": {
			Summary:     "The public catalog",
			Description: "Unauthenticated and cacheable for 60 seconds. Returns only published products in visible categories that have at least one price; a hidden product is indistinguishable from a missing one. Omits provision targets, active-subscription counts and every internal id.",
			Responses: []Response{
				{Status: 200, Description: "Categories and their published products"},
			},
		},
		"GET /api/v1/catalog/{slug}": {
			Summary:     "One public product",
			Description: "Same visibility rules as the index: hidden, in a hidden category, or unpriced all answer 404.",
			Responses: []Response{
				{Status: 200, Description: "The product"},
				{Status: 404, Description: "Not published", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/media/{id}": {
			Summary:     "A stored logo",
			Description: "Public, because an <img> on the storefront has no session. Served with nosniff and an immutable cache. SVG is the one format that can carry a script, so it is served with a sandboxing Content-Security-Policy rather than as a document.",
			Responses: []Response{
				{Status: 200, Description: "The image bytes"},
				{Status: 404, Description: "No such image", Schema: ProblemRef()},
			},
		},

		// -------------------------------------------------------- staff writes --
		"POST /api/v1/staff/services/{id}/suspend": {
			Summary:     "Suspend a service",
			Description: "Goes through the dunning engine, so the eligibility decision and the generation are recorded. A manual hold is refused unless a second staff actor approves the override.",
			Responses: []Response{
				{Status: 202, Description: "The operation was enqueued", Schema: AcceptedSchema()},
				{Status: 409, Description: "Not eligible, held, or approval required", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/resume": {
			Summary:     "Resume a suspended service",
			Description: "Requires that the customer owes nothing at all, checked against the subledger rather than the invoice columns.",
			Responses: []Response{
				{Status: 202, Description: "The operation was enqueued", Schema: AcceptedSchema()},
				{Status: 409, Description: "The customer still owes money", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/terminate": {
			Summary:     "Terminate a service",
			Description: "Destructive and not automatically reconcilable. The operator must echo the service's `external_id` or id; a mismatch is 400 and enqueues nothing.",
			Responses: []Response{
				{Status: 202, Description: "The operation was enqueued", Schema: AcceptedSchema()},
				{Status: 400, Description: "The confirmation did not match", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/invoices": {
			Summary:     "Create an invoice",
			Description: "The server computes every total with the configured pricer; a client-supplied total is never accepted. `issue_now` drafts and issues in one transaction.",
			Responses:   []Response{{Status: 201, Description: "The invoice"}, {Status: 400, Description: "Invalid line", Schema: ProblemRef()}},
		},
		"POST /api/v1/staff/invoices/{id}/issue": {
			Summary:     "Issue a draft invoice",
			Description: "Allocates the next number and posts the receivable in one transaction. Issuing twice is 409.",
			Responses:   []Response{{Status: 200, Description: "The issued document"}, {Status: 409, Description: "Not a draft", Schema: ProblemRef()}},
		},
		"POST /api/v1/staff/invoices/{id}/void": {
			Summary:     "Void an invoice",
			Description: "Does not delete the invoice and does not reverse its posting. A paid invoice cannot become void; the correction is a credit note.",
			Responses:   []Response{{Status: 200, Description: "Voided"}, {Status: 409, Description: "Not voidable", Schema: ProblemRef()}},
		},

		// ------------------------------------------------------- money actions --
		"POST /api/v1/staff/payments/{id}/refunds": {
			Summary: "Refund a payment",
			Description: "The refundable amount is computed under a row lock on the payment. The refund row is created before the provider call with a stable " +
				"operation key; a timeout leaves `unknown` and no posting, and a reconciler queries the provider rather than retrying. " +
				"Above the configured threshold a second staff signature is required.",
			Responses: []Response{
				{Status: 201, Description: "The refund"},
				{Status: 202, Description: "The provider did not answer; the outcome is unknown and will be reconciled"},
				{Status: 409, Description: "Exceeds refundable, already refunded, or approval required", Schema: ProblemRef()},
				{Status: 502, Description: "The provider refused; nothing was recorded", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/refunds/{id}": {
			Summary:   "One refund",
			Responses: []Response{{Status: 200, Description: "The refund"}},
		},
		"POST /api/v1/staff/invoices/{id}/credit-notes": {
			Summary:     "Issue a credit note",
			Description: "An immutable counter-document with its own gap-free number series. Total credits against an invoice may not exceed its total.",
			Responses: []Response{
				{Status: 201, Description: "The credit note"},
				{Status: 409, Description: "Exceeds the invoice total", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/customers/{id}/credit/apply": {
			Summary:     "Apply customer credit to an invoice",
			Description: "Posts an adjustment (debit customer_credit, credit receivable) and may settle the invoice. Idempotent per application: a replay is a no-op.",
			Responses: []Response{
				{Status: 200, Description: "The application"},
				{Status: 409, Description: "Insufficient credit", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/approvals": {
			Summary:   "Pending and decided approvals",
			Responses: []Response{{Status: 200, Description: "A page of approvals"}},
		},
		"POST /api/v1/staff/approvals/{id}/approve": {
			Summary:     "Approve a high-value action",
			Description: "The approver must be a different person from the requester; the database refuses a self-approval even if this handler is bypassed.",
			Responses:   []Response{{Status: 200, Description: "Approved"}, {Status: 409, Description: "Already decided or self-approval", Schema: ProblemRef()}},
		},
		"POST /api/v1/staff/approvals/{id}/reject": {
			Summary:   "Reject a high-value action",
			Responses: []Response{{Status: 200, Description: "Rejected"}, {Status: 409, Description: "Already decided", Schema: ProblemRef()}},
		},

		// ------------------------------------------------------------- trust --
		"GET /api/v1/staff/audit": {
			Summary:     "Application audit log",
			Description: "Append-only. Written by the domain services inside the transaction that made the change, with secrets redacted before insert.",
			Responses:   []Response{{Status: 200, Description: "A page of audit events"}},
		},
	}
	return ops
}
