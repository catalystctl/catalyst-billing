package openapi

// Descriptions for the routes that arrived with the payment-method, cron-diagnostics, locale,
// marketplace, plan-change and due-charging work.
//
// They live in their own file for the reason `RegisterDescriptions` exists: the alternative is an
// ever-growing single map, and a merge conflict in a map of prose is resolved by deleting somebody
// else's sentences. What follows is prose that states the things a generated document cannot:
// which money routes are idempotent and on what key, which reads are scoped to the caller, and
// which operations are refused rather than silently adjusted.

func init() { RegisterDescriptions(func() map[string]Operation { return mergedRouteOperations }) }

// paymentMethodSchema is one saved instrument. It is deliberately the *public* shape: the provider
// token that can move money is write-only and never appears in a response.
func paymentMethodSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "brand", "last4", "is_default"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"brand":      map[string]any{"type": "string", "description": "Card brand as the provider reported it."},
			"last4":      map[string]any{"type": "string", "description": "The only part of the number that is stored."},
			"exp_month":  map[string]any{"type": "integer", "minimum": 1, "maximum": 12},
			"exp_year":   map[string]any{"type": "integer"},
			"is_default": map[string]any{"type": "boolean"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

var mergedRouteOperations = map[string]Operation{
	// -- saved payment methods, customer side ------------------------------------------------

	"GET /api/v1/portal/payment-methods/saved": {
		Summary: "The customer's saved payment methods",
		Description: "Only the caller's own instruments. The list is scoped in SQL rather than filtered after " +
			"the read: an id belonging to another customer is a 404, not an empty list, so the endpoint " +
			"cannot be used to probe for the existence of somebody else's card.",
		Responses: []Response{{Status: 200, Description: "The caller's instruments, default first."}},
	},
	"POST /api/v1/portal/payment-methods/saved": {
		Summary: "Save a payment method",
		Description: "The request carries a provider token, never a card number: the panel is not in the " +
			"card-data path, so there is nothing here to be compliant with. The token is stored write-only — " +
			"no read path returns it, redacted or otherwise. Requires an `Idempotency-Key`; a replay returns " +
			"the original instrument instead of saving a second copy.",
		Responses: []Response{
			{Status: 201, Description: "Saved. The response returns the public shape, not the token."},
			{Status: 409, Description: "The provider refused the token, or it is already attached to another customer."},
		},
	},
	"GET /api/v1/portal/payment-methods/saved/{id}": {
		Summary:     "One saved payment method",
		Description: "Scoped to the caller in SQL: another customer's id is a 404 with the same body as a nonexistent one.",
		Responses:   []Response{{Status: 200, Description: "The instrument."}, {Status: 404, Description: "No such instrument for this caller."}},
	},
	"POST /api/v1/portal/payment-methods/saved/{id}/default": {
		Summary: "Make a saved method the default",
		Description: "Exactly one method per customer is the default, enforced by the write rather than by the " +
			"reader: a second default would make the off-session charger's choice depend on row order.",
		Responses: []Response{{Status: 200, Description: "The instrument, now the default."}, {Status: 404, Description: "No such instrument for this caller."}},
	},
	"DELETE /api/v1/portal/payment-methods/saved/{id}": {
		Summary: "Forget a saved payment method",
		Description: "Removes the stored token locally. The provider-side mandate is detached separately by " +
			"deleting the agreement, because a token the panel has forgotten can still be chargeable at the " +
			"provider — and a customer who deleted their card expects both to be gone.",
		Responses: []Response{{Status: 204, Description: "Gone."}, {Status: 404, Description: "No such instrument for this caller."}},
	},

	// -- billing agreements, customer side ---------------------------------------------------

	"GET /api/v1/portal/payment-methods/agreements/{id}": {
		Summary:     "One billing agreement",
		Description: "The mandate record: what the customer authorised, for which provider, and whether it is still active. Scoped to the caller.",
		Responses:   []Response{{Status: 200, Description: "The agreement."}, {Status: 404, Description: "No such agreement for this caller."}},
	},
	"POST /api/v1/portal/payment-methods/agreements": {
		Summary: "Accept a billing agreement",
		Description: "Records the mandate that lets the installation charge this customer off-session. " +
			"Off-session charging is the one path that moves money with nobody present, so it is opt-in per " +
			"customer, revocable, and the record of the acceptance is kept rather than overwritten. " +
			"Requires an `Idempotency-Key`.",
		Responses: []Response{
			{Status: 201, Description: "The agreement is active."},
			{Status: 409, Description: "An active agreement for this provider already exists."},
		},
	},
	"DELETE /api/v1/portal/payment-methods/agreements/{id}": {
		Summary: "Revoke a billing agreement",
		Description: "Stops future off-session charges and detaches the provider-side mandate. Invoices already " +
			"issued are untouched: revocation is forward-looking, and retroactively unwinding a charge the " +
			"customer agreed to at the time is a refund, which is a separate decision with its own record.",
		Responses: []Response{{Status: 204, Description: "Revoked."}, {Status: 404, Description: "No such agreement for this caller."}},
	},

	// -- plan changes, customer side ---------------------------------------------------------

	"GET /api/v1/portal/services/{id}/plan-options": {
		Summary: "The plans this service could move to",
		Description: "Every plan that is a legal target from the current one, with the prorated amount the move " +
			"would charge or credit today. Prices are quoted as minor units plus an exact decimal string; no " +
			"float ever reaches the client. A plan the service cannot move to is absent rather than present " +
			"with a reason code, so the client cannot render an option that will be refused.",
		Responses: []Response{{Status: 200, Description: "The available targets and their proration."}, {Status: 404, Description: "No such service for this caller."}},
	},
	"POST /api/v1/portal/services/{id}/change-plan": {
		Summary: "Move a service to another plan",
		Description: "Prorates against the current period and queues the change rather than applying it in place: " +
			"the provisioner is a queue, and a plan change that mutated the service directly would produce a " +
			"billing record for a server that was never rebuilt. Requires an `Idempotency-Key`; a replay is " +
			"refused rather than double-queued. The response states the queue entry, not the new plan — the " +
			"change is not done until the worker has done it.",
		Responses: []Response{
			{Status: 202, Description: "Queued."},
			{Status: 409, Description: "A plan change for this service is already queued."},
			{Status: 422, Description: "The target plan is not a legal move from the current one."},
		},
	},

	// -- staff money routes ------------------------------------------------------------------

	"GET /api/v1/staff/customers/{id}/payment-methods": {
		Summary: "A customer's payment methods, staff view",
		Description: "The same public shape the customer sees, plus whether an active billing agreement covers " +
			"it. There is no route that reveals a token, for staff or anyone else: the token is what charges " +
			"the card, and a support screen does not need it to answer a support question.",
		Responses: []Response{{Status: 200, Description: "Instruments and agreement state."}, {Status: 404, Description: "No such customer."}},
	},
	"POST /api/v1/staff/invoices/{id}/charge-saved-method": {
		Summary: "Charge a saved method for one invoice",
		Description: "An operator-initiated charge of an invoice against the customer's stored instrument. " +
			"Idempotent on the invoice: a second call for an invoice that is already paid or already has a " +
			"payment in flight returns that payment rather than taking the money twice. Requires an " +
			"`Idempotency-Key`, because this is a money mutation.",
		Responses: []Response{
			{Status: 200, Description: "The payment, created or already existing."},
			{Status: 402, Description: "The provider declined; the invoice is unchanged."},
			{Status: 409, Description: "The invoice is paid, void, or has no chargeable instrument."},
		},
	},
	"GET /api/v1/staff/payments/charge-due": {
		Summary: "What the unattended charger would take, without charging anything",
		Description: "The dry run for `charge-agreements`: the invoices the next tick would charge, the amount per " +
			"currency, and the invoices it would skip with the reason (`retry_window`, `checkout_open`, " +
			"`no_mandate`). It resolves no gateway at all — it takes the provider *name* — so asking it cannot " +
			"move money, which is what makes it safe to open before turning the job back on.\n\n" +
			"The three refusal states are reported separately rather than collapsed into an empty list: " +
			"`enabled` is the setting, `kill_switch_set` is the process-level switch, and `reason` says why " +
			"the sweep would do nothing. An empty candidate list for four different causes is the answer that " +
			"sends an operator to the wrong screen.",
		Responses: []Response{{Status: 200, Description: "The plan: candidates, skips with reasons, per-currency totals, and refusal state."}},
	},
	"POST /api/v1/staff/payments/charge-due": {
		Summary: "Charge every overdue invoice with a saved method",
		Description: "The batch form of the single charge, and the entry point a scheduled job uses. It reports " +
			"a per-invoice outcome instead of failing as a whole, because one declined card must not prevent " +
			"the other invoices from being collected. Every invoice it charges is charged through the same " +
			"idempotent path as the single call.",
		Responses: []Response{{Status: 200, Description: "Per-invoice results: charged, declined, skipped and why."}},
	},
	"POST /api/v1/staff/upgrades/{id}/credit": {
		Summary: "Turn an unapplied upgrade into account credit",
		Description: "When a downgrade or a cancelled upgrade leaves value on the table, this posts it as credit " +
			"rather than adjusting the already-issued invoice. An issued invoice is immutable — the correction " +
			"is a new ledger entry with a customer-visible reason, which is why `reason` is required and is " +
			"shown to the customer. Requires an `Idempotency-Key`; a replay is refused rather than crediting twice.",
		Responses: []Response{
			{Status: 201, Description: "Credit posted, with the ledger entry id."},
			{Status: 409, Description: "Already credited, or nothing left to credit."},
		},
	},

	// -- cron diagnostics --------------------------------------------------------------------

	"GET /api/v1/staff/cron/failures": {
		Summary: "Recent failed job runs",
		Description: "The failures behind the heartbeat and run history, newest first. A job that has never run " +
			"does not appear here at all — that is a different fault with a different fix, and it is what " +
			"`/staff/cron/statistics` reports. Keyset-paged on the run id so the view does not load a year of " +
			"failures to show twenty.",
		Responses: []Response{{Status: 200, Description: "Failed runs with their error and attempt count."}},
	},
	"GET /api/v1/staff/cron/statistics": {
		Summary: "Per-job run statistics",
		Description: "Per job: last run, last success, last failure, consecutive failures and run duration. The " +
			"fields a scheduler's health is actually judged by, including the *never ran* case that a " +
			"failure list cannot express.",
		Responses: []Response{{Status: 200, Description: "One row per registered job."}},
	},
	"POST /api/v1/staff/cron/{job}/retry": {
		Summary: "Run a scheduled job now",
		Description: "Queues an immediate run of a named job, through the same path the scheduler uses rather " +
			"than a direct call — a retry that bypassed the queue would produce a run the run history does " +
			"not mention. Refused while a run of the same job is already in flight. Requires an `Idempotency-Key`.",
		Responses: []Response{
			{Status: 202, Description: "Queued."},
			{Status: 409, Description: "That job is already running."},
			{Status: 404, Description: "No such job."},
		},
	},

	// -- locales -----------------------------------------------------------------------------

	"GET /api/v1/staff/locales/available": {
		Summary: "Locales that can be installed",
		Description: "The catalogue the installation could add, each marked installed or not. Read from the " +
			"bundled catalogue rather than the network: a settings screen that cannot render because an " +
			"upstream is down is a support call about the wrong problem.",
		Responses: []Response{{Status: 200, Description: "Available locales with installed state."}},
	},
	"POST /api/v1/staff/locales/{code}/install": {
		Summary: "Install a locale",
		Description: "Copies a catalogue locale into the installation so it can be selected and edited. " +
			"Idempotent: installing an installed locale re-syncs it from the catalogue and reports how many " +
			"messages changed, so an operator who has edited a translation is told what would be overwritten " +
			"before it happens. Requires an `Idempotency-Key`.",
		Responses: []Response{
			{Status: 200, Description: "Installed or re-synced, with a count of changed messages."},
			{Status: 404, Description: "No such locale in the catalogue."},
		},
	},

	// -- marketplace -------------------------------------------------------------------------

	"GET /api/v1/staff/marketplace/scaffold": {
		Summary: "Scaffold for a marketplace entry",
		Description: "The manifest skeleton an extension author starts from, with every required field present " +
			"and commented. Returned by the server so the shape a package must have and the shape the " +
			"installer enforces cannot drift apart.",
		Responses: []Response{{Status: 200, Description: "A manifest template."}},
	},
	"POST /api/v1/staff/marketplace/upload": {
		Summary: "Upload a package",
		Description: "Stages a package for installation and checks its manifest. The uploaded artefact is inert " +
			"until it is explicitly enabled — an upload that ran code on arrival would let a mistaken file " +
			"execute before anybody read it. The digest is recorded so the installed bytes can be proved to " +
			"be the uploaded bytes. Requires an `Idempotency-Key`.",
		Responses: []Response{
			{Status: 201, Description: "Staged, with the manifest it declares and the recorded digest."},
			{Status: 422, Description: "The manifest is invalid; the reasons name each missing or malformed field."},
		},
	},
	"PATCH /api/v1/staff/marketplace/installed/{name}/{version}": {
		Summary: "Change an installed package's state",
		Description: "Enable, disable or pin an installed package version. Disabling stops the extension from " +
			"being launched but keeps its data: an operator turning a feature off must not lose what it " +
			"stored. Pinning prevents an update from replacing a version that is being kept deliberately.",
		Responses: []Response{
			{Status: 200, Description: "The package's new state."},
			{Status: 404, Description: "No such package version installed."},
		},
	},

	// -- notifications -----------------------------------------------------------------------

	"POST /api/v1/staff/notifications/test-email": {
		Summary: "Send a test email",
		Description: "Sends one message through the configured transport to the address given, so an operator " +
			"can prove mail works before customers depend on it. It deliberately does NOT report success when " +
			"the transport accepted the message and the recipient never sees it: the response states the " +
			"transport's answer, and the prose says so, because \"the panel said it sent\" is the answer that " +
			"wastes an afternoon.",
		Responses: []Response{
			{Status: 200, Description: "The transport accepted the message."},
			{Status: 502, Description: "The transport refused it; the error is the transport's own."},
		},
	},
}
