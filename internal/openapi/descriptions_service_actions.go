package openapi

// The operator actions on a service (SVC-11 … SVC-22, CT-12).
//
// Two conventions run through this file and are worth stating once:
//
//   - A remote action answers 202 with `{operation_id, status_url}`. It does not hold the
//     connection open while a provider is called, and it does not claim the provider has done
//     anything: the queue row is the record, and the operation resource is where its outcome
//     appears.
//   - A refusal names what is missing rather than only what is wrong. "no remote resource",
//     "no external subscription" and "service has history" are three different operator
//     problems, and each has its own code so a screen can offer the right next step.

func init() { RegisterDescriptions(serviceActionOperations) }

func serviceActionOperations() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/staff/services/{id}/recalculate-price": {
			Summary: "Recompute a service's price from the catalogue",
			Description: "Reads the product's newest price for the service's interval and currency, re-prices " +
				"the stored config-option selection against it, and reports the difference in exact minor " +
				"units. With `apply: false` — the default — nothing is written. With `apply: true` the " +
				"subscription moves to that price version, the stored option amounts are rewritten to it, and " +
				"a resize is queued when the provisioner plan itself changed. It is not an upgrade: no " +
				"prorated invoice is raised, because this repairs a stale price rather than selling a change.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"apply":  map[string]any{"type": "boolean", "description": "Write the result. Default false."},
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The comparison, and whether it was applied"},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{
					Status: 409, Description: "The service has no billing schedule or no comparable " +
						"catalogue price", Schema: ProblemRef(),
				},
			},
		},
		"POST /api/v1/staff/services/{id}/cancel-subscription": {
			Summary: "Cancel a provider-side subscription",
			Description: "Records the provider's subscription id on the service and queues the gateway call that " +
				"ends it. The call is a `cancel_subscription` queue row, never a direct gateway call, so a lost " +
				"reply is retried against the same idempotency key. The id is stored before the call and cleared " +
				"only after the provider confirms, so a crash in between is recoverable.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subscription_id": map[string]any{
						"type": "string", "maxLength": 200,
						"description": "The provider's id. Omit to use the one already stored on the service.",
					},
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 202, Description: "Queued"},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "No external subscription id, or the service is terminated", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/orders/{id}/cancel-subscription": {
			Summary: "Cancel every provider-side subscription of an order",
			Description: "One order can hold several services and each may carry its own provider subscription, so " +
				"this queues one cancellation per service rather than stopping at the first. Services that fail " +
				"are reported in `failures`; a partial success is never presented as a complete one.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 202, Description: "Queued, with any per-service failures alongside"},
				{Status: 409, Description: "No service of the order carries an external subscription", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/delete-remote": {
			Summary: "Delete the provider-side resource, keeping the service",
			Description: "Destroys the resource at the provider and forgets its id, and deliberately leaves the " +
				"service row, its status and its billing relationship untouched: the customer keeps what they " +
				"bought. Terminating would end the service, which is a different decision. It is a queue row, " +
				"so the call is idempotent and its outcome is visible on the operation resource.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 202, Description: "Queued"},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "The service has no remote resource", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/notify": {
			Summary: "Send the customer a message about their service",
			Description: "Writes the mail to the durable outbox and the portal inbox in one transaction. The message " +
				"is a deliberate act by an operator, so it carries no dedupe key: pressing the button twice " +
				"sends twice, and the button never lies about having sent.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"subject", "body"},
				"properties": map[string]any{
					"subject": map[string]any{"type": "string", "maxLength": 200},
					"body":    map[string]any{"type": "string", "maxLength": 8000},
				},
			},
			Responses: []Response{
				{Status: 202, Description: "Queued"},
				{Status: 400, Description: "A missing or oversized subject or body", Schema: ProblemRef()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "The customer has no email address", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/services/{id}": {
			Summary: "Delete a service with no history",
			Description: "Refused while the service has invoices, provisioning operations, upgrades, cancellations, " +
				"holds, tickets or suspension decisions behind it, because deleting it would orphan or cascade " +
				"away part of the record of what happened. Such a service is terminated instead, and the " +
				"refusal names the records that blocked it.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "Deleted"},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "The service has history and must be terminated", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/upgrades/apply": {
			Summary: "Apply every paid upgrade now",
			Description: "The manual half of the pending-upgrade queue: it applies every upgrade whose invoice has " +
				"been paid, moving the subscription and queueing the resize. It is idempotent — an applied row " +
				"is not a candidate — and it is the same function the scheduled job calls, so the two paths " +
				"cannot disagree.",
			Responses: []Response{
				{Status: 200, Description: "The ids that were applied"},
			},
		},
		"GET /api/v1/staff/services/{id}/provisioning-fields": {
			Summary: "The provisioning inputs an adapter declares, and this service's values",
			Description: "The keys come from the configured provisioner's `provisioner.fields` call, not from the " +
				"core: a cPanel account needs a domain and a Pterodactyl server needs a panel user id, and the " +
				"core does not know either. A provisioner that cannot be reached leaves the stored values " +
				"readable rather than blanking the screen.",
			Responses: []Response{
				{Status: 200, Description: "Declared fields plus stored values"},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/services/{id}/provisioning-fields": {
			Summary: "Store the provisioning inputs on a service",
			Description: "Validated against the adapter's own declaration: an unknown key is refused (it would be " +
				"stored and never sent) and a missing required field is refused (the purchase would fail after " +
				"the money moved). The values are sent back to the adapter in the create and upgrade metadata " +
				"under the same keys.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"values": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "Stored"},
				{Status: 400, Description: "An unknown or missing required field", Schema: ProblemRef()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 422, Description: "No provisioner is configured to declare fields", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/servers/fields": {
			Summary: "The provisioner's purchase-time inputs",
			Description: "Separate from the servers screen's credentials: those are installation configuration, these " +
				"are per-purchase data. An installation with no provider configured answers 200 with " +
				"`configured: false` rather than an error, because there is nothing broken. A failure to launch " +
				"the adapter is reported inline with a stable code, like the connection test.",
			Responses: []Response{
				{Status: 200, Description: "The declaration, or the reason it could not be read"},
			},
		},
	}
}
