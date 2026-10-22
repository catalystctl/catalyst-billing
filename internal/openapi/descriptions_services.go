package openapi

// Service lifecycle descriptions (P6: CAN-*, UPG-*, SVC-07…SVC-10, SF-07…SF-09).
//
// The prose lives in its own file for the same reason the route table does: two people
// adding two domains must not edit one block. The parity test compares this map against the
// router in both directions, so a route added without an entry here fails, and an entry whose
// route is gone fails too.
//
// Three conventions are worth stating once, because they apply to every route in this file:
//
//   * Money is integer minor units plus an explicit currency, never a float.
//   * A customer-scoped route resolves the customer from the session and scopes in SQL, so a
//     substituted identifier returns 404 rather than 403.
//   * An immediate cancellation is a *request*: it is executed by staff or the due-work cron,
//     never by the customer who asked for it.

func init() { RegisterDescriptions(serviceOperations) }

// cancellationSchema is the wire shape of a cancellation request.
func cancellationSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "service_id", "type", "status", "reason"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string", "format": "uuid"},
			"service_id": map[string]any{"type": "string", "format": "uuid"},
			"type": map[string]any{
				"type": "string", "enum": []string{"immediate", "end_of_period"},
				"description": "immediate destroys the service and is executed by staff; end_of_period stops the schedule at the paid-through date.",
			},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"pending", "scheduled", "executed", "cancelled", "rejected"},
			},
			"reason":       map[string]any{"type": "string", "maxLength": 255},
			"requested_by": map[string]any{"type": "string", "format": "uuid"},
			"effective_at": map[string]any{"type": "string", "format": "date-time"},
			"executed_at":  map[string]any{"type": "string", "format": "date-time"},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"product_name": map[string]any{"type": "string"},
			"customer": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "format": "uuid"},
					"name":  map[string]any{"type": "string"},
					"email": map[string]any{"type": "string", "format": "email"},
				},
			},
			"message": map[string]any{"type": "string"},
		},
	}
}

// prorationSchema is the exact fraction of the period that remained at the change.
func prorationSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"period_start", "period_end", "as_of", "ratio"},
		"properties": map[string]any{
			"period_start": map[string]any{"type": "string", "format": "date-time"},
			"period_end":   map[string]any{"type": "string", "format": "date-time"},
			"as_of":        map[string]any{"type": "string", "format": "date-time"},
			"remaining_secs": map[string]any{
				"type": "integer", "format": "int64",
				"description": "Elapsed time left in the period. Durations, not day counts, so a DST transition and a 28-day February are both exact.",
			},
			"total_secs": map[string]any{"type": "integer", "format": "int64"},
			"ratio": map[string]any{
				"type": "string", "example": "1/2",
				"description": "The exact rational remaining/total. The prorated amount is (to - from) * ratio, rounded once with an explicit mode.",
			},
		},
	}
}

// upgradeResultSchema is what an upgrade, downgrade or option change returns.
func upgradeResultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"upgrade_id", "service_id", "direction", "currency", "prorated_minor", "status",
		},
		"properties": map[string]any{
			"upgrade_id":            map[string]any{"type": "string", "format": "uuid"},
			"service_id":            map[string]any{"type": "string", "format": "uuid"},
			"from_price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"to_price_version_id":   map[string]any{"type": "string", "format": "uuid"},
			"direction": map[string]any{
				"type": "string", "enum": []string{"upgrade", "downgrade", "none"},
			},
			"currency":       map[string]any{"type": "string"},
			"prorated_minor": map[string]any{"type": "integer", "format": "int64", "description": "Signed: positive is charged, negative is credited."},
			"status": map[string]any{
				"type": "string", "enum": []string{"pending", "applied", "cancelled", "failed"},
			},
			"invoice_id":     map[string]any{"type": "string", "format": "uuid"},
			"invoice_number": map[string]any{"type": "string"},
			"credit_minor":   map[string]any{"type": "integer", "format": "int64"},
			"credit_pending": map[string]any{
				"type":        "boolean",
				"description": "True when the customer is owed a credit that the credits work package has not posted yet. The obligation is recorded on the service_upgrades row.",
			},
			"message":   map[string]any{"type": "string"},
			"proration": prorationSchema(),
		},
	}
}

// configOptionSchema is the wire shape of a configurable option.
func configOptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "product_id", "name", "type"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"product_id":  map[string]any{"type": "string", "format": "uuid"},
			"parent_id":   map[string]any{"type": "string", "format": "uuid"},
			"name":        map[string]any{"type": "string", "maxLength": 120},
			"description": map[string]any{"type": "string"},
			"type": map[string]any{
				"type": "string",
				"enum": []string{"dropdown", "radio", "checkbox", "text", "number"},
			},
			"choices":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"env_variable": map[string]any{"type": "string", "description": "The environment variable the provisioner receives when this option is selected."},
			"required":     map[string]any{"type": "boolean"},
			"hidden":       map[string]any{"type": "boolean"},
			"upgradable":   map[string]any{"type": "boolean", "description": "A live service may change this option, which issues a prorated charge."},
			"sort_order":   map[string]any{"type": "integer"},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"updated_at":   map[string]any{"type": "string", "format": "date-time"},
			"prices": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":               map[string]any{"type": "string", "format": "uuid"},
						"price_version_id": map[string]any{"type": "string", "format": "uuid"},
						"choice":           map[string]any{"type": "string"},
						"amount_minor":     map[string]any{"type": "integer", "format": "int64"},
						"setup_minor":      map[string]any{"type": "integer", "format": "int64"},
					},
				},
			},
		},
	}
}

// resolvedSelectionSchema is a priced selection: the breakdown plus the totals.
func resolvedSelectionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"price_version_id", "currency", "options", "total", "setup_total"},
		"properties": map[string]any{
			"price_version_id": map[string]any{"type": "string", "format": "uuid"},
			"currency":         map[string]any{"type": "string"},
			"options": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"config_option_id", "value", "amount", "setup"},
					"properties": map[string]any{
						"config_option_id": map[string]any{"type": "string", "format": "uuid"},
						"name":             map[string]any{"type": "string"},
						"env_variable":     map[string]any{"type": "string"},
						"value":            map[string]any{"type": "string"},
						"amount":           AmountRef(),
						"setup":            AmountRef(),
					},
				},
			},
			"total":       AmountRef(),
			"setup_total": AmountRef(),
			"env": map[string]any{
				"type": "object", "additionalProperties": map[string]any{"type": "string"},
				"description": "The environment the provisioner receives: one entry per selected option that names an env_variable.",
			},
		},
	}
}

// serviceOperations is the prose for the service-lifecycle routes.
func serviceOperations() map[string]Operation {
	return map[string]Operation{

		// ------------------------------------------------------ cancellations --

		"POST /api/v1/portal/services/{id}/cancellation": {
			Summary: "Request cancellation of a service",
			Description: "Records a request rather than acting on the service. `end_of_period` sets " +
				"`cancel_at_period_end` and returns a `scheduled` request with the paid-through date; the " +
				"service keeps running and the renewal engine stops billing at that date. `immediate` " +
				"records a `pending` request that staff or the due-work cron executes — an immediate " +
				"cancellation destroys time the customer has already paid for, so it is a decision, not a " +
				"button. The service is resolved through the caller's customer account in SQL, so another " +
				"customer's identifier returns 404.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"type"},
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
					"type": map[string]any{
						"type": "string", "enum": []string{"immediate", "end_of_period"},
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded request", Schema: cancellationSchema()},
				{Status: 409, Description: "The service cannot be cancelled, or a request is already open", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/services/{id}/cancellation": {
			Summary: "The open cancellation request for a service",
			Description: "Answers `{\"cancellation\": null}` when there is none: the absence of a request is " +
				"the normal state, not an error. Used by the portal to show \"cancellation requested\" " +
				"instead of offering the button again.",
			Responses: []Response{{Status: 200, Description: "The request, or null"}},
		},
		"POST /api/v1/staff/services/{id}/cancellation": {
			Summary: "Record a cancellation on a customer's behalf",
			Description: "The same body and the same state machine as the portal route, without the customer " +
				"scoping. Gated on service:upgrade because moving a service's lifecycle for a customer is " +
				"the same kind of decision as repricing it.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"type"},
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "maxLength": 255},
					"type": map[string]any{
						"type": "string", "enum": []string{"immediate", "end_of_period"},
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded request", Schema: cancellationSchema()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "The service cannot be cancelled", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/cancellations": {
			Summary: "The cancellation queue",
			Description: "Newest first. `status` filters to one of pending, scheduled, executed, cancelled or " +
				"rejected; an absent status means the open queue (pending and scheduled). `service_id` " +
				"narrows to one service. Each row carries the customer and the product name, so a queue is " +
				"readable without three more requests.",
			Responses: []Response{{Status: 200, Description: "The queue"}},
		},
		"POST /api/v1/staff/cancellations/{id}/execute": {
			Summary: "Execute a pending immediate cancellation",
			Description: "Enqueues the existing provisioning `terminate` operation for the service and marks " +
				"the request executed in the same transaction, so the queue and the operation cannot " +
				"disagree. Answers 202 with the operation id because the destructive part is queued; the " +
				"service disappears when the worker has spoken to the provider. A `scheduled` request is " +
				"refused: an end-of-period cancellation runs at its effective date, and destroying the " +
				"service early is a refund event, not an execution.",
			Responses: []Response{
				{Status: 202, Description: "Executed and the termination is queued", Schema: AcceptedSchema()},
				{Status: 200, Description: "The service was already terminated; the request is marked executed",
					Schema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cancellation_id":    map[string]any{"type": "string", "format": "uuid"},
							"service_id":         map[string]any{"type": "string", "format": "uuid"},
							"status":             map[string]any{"type": "string"},
							"already_terminated": map[string]any{"type": "boolean"},
						},
					}},
				{Status: 409, Description: "The request is not pending or not immediate", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/cancellations/{id}/reject": {
			Summary: "Reject a cancellation request",
			Description: "A reason is required: rejecting a customer's request without one leaves the next agent " +
				"and the customer with no explanation. An end-of-period request has already stopped the " +
				"schedule, so rejecting it clears `cancel_at_period_end` in the same transaction — otherwise " +
				"the queue would say rejected while the renewal engine quietly ended the subscription.",
			RequestSchema: map[string]any{
				"type":       "object",
				"required":   []string{"reason"},
				"properties": map[string]any{"reason": map[string]any{"type": "string", "maxLength": 255}},
			},
			Responses: []Response{
				{Status: 200, Description: "The rejected request", Schema: cancellationSchema()},
				{Status: 400, Description: "No reason given", Schema: ProblemRef()},
				{Status: 409, Description: "The request is not open", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- upgrades --

		"GET /api/v1/portal/services/{id}/upgrade-options": {
			Summary: "Products this service can move to",
			Description: "The products reachable from the service's product through `product_upgrades`, each " +
				"with its price versions in the service's own currency and billing interval. Only prices in " +
				"that currency are offered: this system deliberately has no exchange rate. Each price " +
				"carries `prorated`, the exact charge or credit for the remainder of the current period.",
			Responses: []Response{{Status: 200, Description: "The reachable products and their prices"}},
		},
		"POST /api/v1/portal/services/{id}/upgrade": {
			Summary: "Upgrade or downgrade a service",
			Description: "Prices the change against the remainder of the current period using exact rational " +
				"arithmetic — never a float, never a \"30 days per month\" assumption. A higher price issues " +
				"an invoice for the prorated amount; the service keeps its current price until that invoice " +
				"is paid, which is what makes a half-applied upgrade visible. A lower price records the " +
				"credit owed and reports `credit_pending`. The target must be reachable from the service's " +
				"product, in the same currency and on the same billing interval.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"price_version_id"},
				"properties": map[string]any{
					"price_version_id": map[string]any{"type": "string", "format": "uuid"},
					"reason":           map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded upgrade", Schema: upgradeResultSchema()},
				{Status: 409, Description: "The target is not reachable, or the currencies or intervals differ", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/upgrade": {
			Summary: "Move any service to a different price",
			Description: "The same pricing as the portal route, without the customer scoping: staff may move a " +
				"service within the upgrade graph, including to a more expensive price the customer did not " +
				"ask for. Requires service:upgrade, which is separate from service:provision because it " +
				"changes what the customer is billed, not just what is running.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"price_version_id"},
				"properties": map[string]any{
					"price_version_id": map[string]any{"type": "string", "format": "uuid"},
					"reason":           map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded upgrade", Schema: upgradeResultSchema()},
				{Status: 404, Description: "No such service", Schema: ProblemRef()},
				{Status: 409, Description: "The target is not reachable, or the service cannot be upgraded", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/upgrades": {
			Summary: "The pending-upgrade queue",
			Description: "`paid=true` lists only the rows whose invoice has been paid — the ones the worker will " +
				"apply. `paid=false` lists every pending row, including a downgrade whose credit has not " +
				"been posted yet, which is what an operator chasing a stuck change wants to see.",
			Responses: []Response{{Status: 200, Description: "The queue"}},
		},

		// ---------------------------------------------------- config options --

		"GET /api/v1/staff/config-options": {
			Summary: "A product's configurable options",
			Description: "Every option with its per-price-version amounts and setup fees, ordered by sort_order " +
				"then name. `product_id` is required. Requires invoice:read:all: reading the pricing of an " +
				"option must not require the right to change it.",
			Responses: []Response{{Status: 200, Description: "The options and the valid types"}},
		},
		"POST /api/v1/staff/config-options": {
			Summary: "Create a configurable option",
			Description: "A dropdown or radio option needs at least one choice; a checkbox, text or number " +
				"option does not. Changing an option changes what every subscription on that product renews " +
				"at, which is why this needs config:write rather than catalog:write.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"product_id", "name", "type"},
				"properties": map[string]any{
					"product_id":   map[string]any{"type": "string", "format": "uuid"},
					"parent_id":    map[string]any{"type": "string", "format": "uuid"},
					"name":         map[string]any{"type": "string", "maxLength": 120},
					"description":  map[string]any{"type": "string"},
					"type":         map[string]any{"type": "string", "enum": []string{"dropdown", "radio", "checkbox", "text", "number"}},
					"choices":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"env_variable": map[string]any{"type": "string"},
					"required":     map[string]any{"type": "boolean"},
					"hidden":       map[string]any{"type": "boolean"},
					"upgradable":   map[string]any{"type": "boolean"},
					"sort_order":   map[string]any{"type": "integer"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The option", Schema: configOptionSchema()},
				{Status: 400, Description: "Invalid definition", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/config-options/{id}": {
			Summary: "Edit a configurable option",
			Description: "`upgradable` is preserved when the body omits it, so an edit from a screen that does " +
				"not show the flag cannot turn a live-changeable option into a fixed one.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"name", "type"},
				"properties": map[string]any{
					"parent_id":    map[string]any{"type": "string", "format": "uuid"},
					"name":         map[string]any{"type": "string", "maxLength": 120},
					"description":  map[string]any{"type": "string"},
					"type":         map[string]any{"type": "string", "enum": []string{"dropdown", "radio", "checkbox", "text", "number"}},
					"choices":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"env_variable": map[string]any{"type": "string"},
					"required":     map[string]any{"type": "boolean"},
					"hidden":       map[string]any{"type": "boolean"},
					"upgradable":   map[string]any{"type": "boolean"},
					"sort_order":   map[string]any{"type": "integer"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The option", Schema: configOptionSchema()},
				{Status: 404, Description: "No such option", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/config-options/{id}": {
			Summary: "Delete a configurable option",
			Description: "Refused while any live service has selected it: a selection is a priced fact, and " +
				"deleting the option underneath it would leave a renewal that cannot re-price.",
			Responses: []Response{
				{Status: 200, Description: "Deleted"},
				{Status: 409, Description: "A live service has selected it", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/config-options/{id}/prices": {
			Summary: "Set one choice's amount and setup fee",
			Description: "An upsert on (option, price version, choice): the same choice priced twice is one " +
				"price, not two rows whose order decides the answer. The option and the price version must " +
				"belong to the same product, so a typo cannot create a price no selection can ever match.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"price_version_id", "amount_minor"},
				"properties": map[string]any{
					"price_version_id": map[string]any{"type": "string", "format": "uuid"},
					"choice":           map[string]any{"type": "string"},
					"amount_minor":     map[string]any{"type": "integer", "format": "int64", "minimum": 0},
					"setup_minor":      map[string]any{"type": "integer", "format": "int64", "minimum": 0},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The stored price"},
				{Status: 400, Description: "The option and price version belong to different products", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/config-options/{id}/prices": {
			Summary: "Remove one choice's price",
			Description: "A choice with no price row is free, which is the documented default rather than an " +
				"error state.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"price_version_id"},
				"properties": map[string]any{
					"price_version_id": map[string]any{"type": "string", "format": "uuid"},
					"choice":           map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "Deleted"},
				{Status: 404, Description: "No such price", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/config-options/resolve": {
			Summary: "Price a selection",
			Description: "The same resolver the checkout path and the renewal engine call, so a quote and a " +
				"renewal cannot disagree: they are the same function reading the same rows. Query " +
				"`product_id`, `price_version_id`, and one `option.<config_option_id>=<value>` pair per " +
				"selection. The response is a breakdown — each option's contribution, the recurring total " +
				"and the one-off setup total — plus the environment the provisioner would receive.",
			Responses: []Response{
				{Status: 200, Description: "The priced selection", Schema: resolvedSelectionSchema()},
				{Status: 400, Description: "An option or value the product does not offer", Schema: ProblemRef()},
			},
		},

		// --------------------------------------------- service config (portal) --

		"GET /api/v1/portal/services/{id}/config": {
			Summary: "The selection stored on a service",
			Description: "Each row carries the amount it contributed at selection time, so a later option-price " +
				"change does not silently re-price an existing service.",
			Responses: []Response{{Status: 200, Description: "The selection"}},
		},
		"PUT /api/v1/portal/services/{id}/config": {
			Summary: "Change an upgradable option on a service",
			Description: "An upgrade in everything but name: the option changes what the subscription renews at, " +
				"so the difference for the remainder of the period is charged or credited exactly as a " +
				"product upgrade would be. An option whose `upgradable` flag is false is refused.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"config_option_id", "value"},
				"properties": map[string]any{
					"config_option_id": map[string]any{"type": "string", "format": "uuid"},
					"value":            map[string]any{"type": "string"},
					"reason":           map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The recorded change", Schema: upgradeResultSchema()},
				{Status: 400, Description: "An option or value the product does not offer", Schema: ProblemRef()},
				{Status: 409, Description: "The option is not upgradable", Schema: ProblemRef()},
			},
		},
	}
}
