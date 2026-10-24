package openapi

// The live server card and its power buttons: what a customer and an operator both see, and the
// four actions either can ask for.
//
// The prose here states the things a generated document cannot, and each one is a way a client
// author gets this surface wrong:
//
//   - A failed read is a 200 with a sentence, not a 5xx. A provider that is down must not take the
//     invoice list down with it, and the card has to be able to say "we could not ask" rather than
//     render an empty box that looks like a bug in the panel.
//   - `provisioned: false` is normal. A service ordered minutes ago has no server yet.
//   - The power route distinguishes "this cannot be done" from "we could not ask", because the
//     customer's next action differs: stop retrying, or retry.
//   - The power action is idempotent on the idempotency key, which is folded into the extension's
//     operation key so a retried press and a replayed response describe one act.

func init() { RegisterDescriptions(func() map[string]Operation { return serviceServerOperations }) }

// serverViewSchema is the live card. Every field beyond the identifiers is optional, because the
// view degrades to a sentence rather than to an error.
func serverViewSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"service_id", "service_status", "provisioned", "available"},
		"properties": map[string]any{
			"service_id":     map[string]any{"type": "string", "format": "uuid"},
			"service_status": map[string]any{"type": "string", "description": "The billing state of the service, not the machine's state."},
			"provisioned": map[string]any{
				"type":        "boolean",
				"description": "False for a service with no resource yet. Not an error: the card renders a sentence.",
			},
			"available": map[string]any{
				"type":        "boolean",
				"description": "True only when the provider actually reported. False with `detail` set says why not.",
			},
			"detail": map[string]any{
				"type":        "string",
				"description": "A sentence for a human, set when there is nothing live to show. Present is not an error condition.",
			},
			"provider":  map[string]any{"type": "string", "description": "The provisioner that owns the resource."},
			"extension": map[string]any{"type": "string", "description": "The extension that answered, when one did."},
			"name":      map[string]any{"type": "string"},
			"state":     map[string]any{"type": "string", "description": "The machine's own state word, as the provider spells it."},
			"status":    map[string]any{"type": "string", "description": "The normalised state: running, stopped, starting and so on."},
			"address":   map[string]any{"type": "string", "description": "What a player connects to, when the provider gives one."},
			"host":      map[string]any{"type": "string"},
			"port":      map[string]any{"type": "integer"},
			"panel_url": map[string]any{"type": "string", "description": "The provider's own control panel for this server, if any."},
			"can_power": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The actions this server currently offers. A button the provider would refuse is not rendered.",
			},
			"plan_id":     map[string]any{"type": "string"},
			"region":      map[string]any{"type": "string"},
			"observed_at": map[string]any{"type": "string", "format": "date-time"},
			"stats": map[string]any{
				"type": "object",
				"description": "Live metrics. Every field is optional and a missing one renders as a dash: " +
					"\"0 MB\" and \"no figure\" are different statements and only one of them is true.",
				"properties": map[string]any{
					"cpu_percent":     map[string]any{"type": "number"},
					"memory_used_mb":  map[string]any{"type": "number"},
					"memory_limit_mb": map[string]any{"type": "number"},
					"disk_used_mb":    map[string]any{"type": "number"},
					"disk_limit_mb":   map[string]any{"type": "number"},
					"network_rx_mb":   map[string]any{"type": "number"},
					"network_tx_mb":   map[string]any{"type": "number"},
					"uptime_seconds":  map[string]any{"type": "number"},
				},
			},
		},
	}
}

// powerResultSchema is what one accepted action reports.
func powerResultSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"service_id", "action", "changed"},
		"properties": map[string]any{
			"service_id": map[string]any{"type": "string", "format": "uuid"},
			"action":     map[string]any{"type": "string", "enum": []string{"start", "stop", "restart", "kill"}},
			"state":      map[string]any{"type": "string"},
			"status":     map[string]any{"type": "string"},
			"changed": map[string]any{
				"type":        "boolean",
				"description": "False when the server was already in the requested state, which is a success rather than a refusal.",
			},
			"provider": map[string]any{"type": "string"},
		},
	}
}

// serverViewDescription is the shared half of the two read routes.
const serverViewDescription = "The live state of the machine behind a service, read from the provider at request time. " +
	"`Cache-Control: no-store`, because it is a question about now.\n\n" +
	"A failed provider read is a **200 with a sentence in `detail`**, not a 5xx: the billing page must " +
	"stay usable while a game panel is down, and the card has to be able to say \"we could not ask\" " +
	"instead of rendering an empty box that reads as a bug in this software. `available: false` plus " +
	"`detail` is that state; `available: true` means every field was reported. " +
	"`provisioned: false` is a third, entirely normal state: a service ordered a minute ago has no " +
	"server yet, and the card says so rather than failing."

// powerDescription is the shared half of the two power routes.
const powerDescription = "Performs one power action against the provider and records it in the append-only audit log. " +
	"The audit row is written after the remote effect has happened, because there is no local " +
	"transaction to join and a power action nobody can attribute is exactly what that log exists to " +
	"prevent.\n\n" +
	"Requires an `Idempotency-Key`, which is folded into the extension's operation key: a retried " +
	"press and a replayed response describe the same act rather than two. `changed: false` means the " +
	"server was already in the requested state, which is a success and not a refusal.\n\n" +
	"The error codes deliberately separate **\"this cannot be done\"** from **\"we could not ask\"**. " +
	"`power_rejected` and `power_unsupported` are the first: a retry will not help, so the customer " +
	"should stop. `power_timeout` and `power_failed` are the second: retrying is exactly right. " +
	"Reporting the second as the first teaches people the button is broken; reporting the first as " +
	"the second invites a retry that can never work."

var serviceServerOperations = map[string]Operation{
	"GET /api/v1/portal/services/{id}/server": {
		Summary:     "Your service's live server state",
		Description: serverViewDescription + "\n\nThe service id is resolved in SQL against the caller's own account, so another customer's service is a 404 with the same body as one that never existed.",
		Responses: []Response{
			{Status: 200, Description: "The live card, or a sentence explaining why there is nothing live to show.", Schema: serverViewSchema()},
			{Status: 401, Description: "No session", Schema: ProblemRef()},
			{Status: 404, Description: "No such service for this caller", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/portal/services/{id}/server/power": {
		Summary:     "Start, stop, restart or kill your own server",
		Description: powerDescription + "\n\n`can_power` on the read route lists what this server currently offers; a client should render only those buttons rather than discovering a refusal after the press.",
		Responses: []Response{
			{Status: 200, Description: "The action was performed and audited.", Schema: powerResultSchema()},
			{Status: 400, Description: "`invalid_action`: not one of start, stop, restart, kill.", Schema: ProblemRef()},
			{Status: 404, Description: "No such service for this caller, or the provider does not recognise it.", Schema: ProblemRef()},
			{Status: 409, Description: "`not_provisioned` (no server yet), `provider_not_configured`, or `power_unsupported`.", Schema: ProblemRef()},
			{Status: 422, Description: "`power_rejected`: the provider refused. Retrying will not help.", Schema: ProblemRef()},
			{Status: 502, Description: "`power_failed`: the provider could not perform it. Retrying may help.", Schema: ProblemRef()},
			{Status: 504, Description: "`power_timeout`: the provider did not answer in time. Retrying may help.", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/services/{id}/server": {
		Summary:     "Any service's live server state, for support",
		Description: serverViewDescription + "\n\nThe operator's view of the same read, so support can answer \"is it running\" without opening the game panel. It is one handler, not two, so a customer and an operator cannot be told different things about the same machine.",
		Responses: []Response{
			{Status: 200, Description: "The live card, or a sentence explaining why there is nothing live to show.", Schema: serverViewSchema()},
			{Status: 404, Description: "No such service", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/services/{id}/server/power": {
		Summary:     "Power any service's server, as staff",
		Description: powerDescription + "\n\nThe audit row names the operator, not the customer: an action taken by support on somebody's behalf has to be attributable to the person who took it.",
		Responses: []Response{
			{Status: 200, Description: "The action was performed and audited.", Schema: powerResultSchema()},
			{Status: 400, Description: "`invalid_action`: not one of start, stop, restart, kill.", Schema: ProblemRef()},
			{Status: 404, Description: "No such service, or the provider does not recognise it.", Schema: ProblemRef()},
			{Status: 409, Description: "`not_provisioned` (no server yet), `provider_not_configured`, or `power_unsupported`.", Schema: ProblemRef()},
			{Status: 422, Description: "`power_rejected`: the provider refused. Retrying will not help.", Schema: ProblemRef()},
			{Status: 502, Description: "`power_failed`: the provider could not perform it. Retrying may help.", Schema: ProblemRef()},
			{Status: 504, Description: "`power_timeout`: the provider did not answer in time. Retrying may help.", Schema: ProblemRef()},
		},
	},
}
