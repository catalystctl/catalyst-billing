package openapi

// The integration configuration descriptions: the payment gateway and the provisioning
// server (E4.2 ADM-10, E4.5 ADM-11).
//
// The prose here is where the two rules a generated client cannot see are written down:
//
//   - **A credential is write-only.** A read reports `is_set` and `source`, never a value.
//     `source` is the precedence decision, and it is the field a client must render: when it
//     is `environment` the deployment supplies the value and the form must be read-only,
//     because the runtime will ignore anything the panel saves.
//   - **The connection test is a read.** It is a `GET`, it takes no idempotency key, and a
//     failed test is a 200 whose answer is "no". A client that treats `ok: false` as a
//     request error will show a scary banner for the normal case of a wrong API key.
//
// The response schemas are inline for the same reason the commerce file's are: the
// component list is built by openapi.go, which this domain does not own, and a `$ref` to a
// component nobody defines is a document that resolves to nothing.

func init() { RegisterDescriptions(integrationOperations) }

// integrationValueSchema is one credential as a read path sees it.
func integrationValueSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key", "label", "env", "is_secret", "is_set", "source", "editable"},
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "description": "The settings key, e.g. `secret_key`."},
			"label": map[string]any{"type": "string", "description": "What the operator sees."},
			"env": map[string]any{
				"type":        "string",
				"description": "The environment variable the extension process reads this value from.",
			},
			"hint": map[string]any{"type": "string", "description": "What the value is and where to get it."},
			"is_secret": map[string]any{
				"type":        "boolean",
				"description": "The value is write-only: it is never returned, and `is_set` is the only fact about it.",
			},
			"is_set": map[string]any{
				"type":        "boolean",
				"description": "A value exists, from either source. For a secret this is all the read reveals.",
			},
			"source": map[string]any{
				"type": "string",
				"enum": []string{"environment", "panel", "default", "unset"},
				"description": "Where the effective value came from. `environment` wins over `panel`, so a field " +
					"whose source is `environment` is read-only: the runtime will ignore a saved value.",
			},
			"editable": map[string]any{
				"type":        "boolean",
				"description": "False when the environment supplies the value. Render the field read-only rather than accepting a save that does nothing.",
			},
		},
	}
}

// integrationFieldSchema is one credential a provider accepts, as declared by the server.
func integrationFieldSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key", "label", "env", "is_secret"},
		"properties": map[string]any{
			"key":       map[string]any{"type": "string"},
			"label":     map[string]any{"type": "string"},
			"env":       map[string]any{"type": "string"},
			"is_secret": map[string]any{"type": "boolean"},
			"hint":      map[string]any{"type": "string"},
		},
	}
}

// integrationProviderSchema is one launchable provider.
func integrationProviderSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "name", "extension", "fields"},
		"properties": map[string]any{
			"id":        map[string]any{"type": "string", "description": "The value stored as the provider."},
			"name":      map[string]any{"type": "string", "description": "The provider's human name."},
			"extension": map[string]any{"type": "string", "description": "The binary under `bin/` when no explicit path is configured."},
			"note":      map[string]any{"type": "string"},
			"fields":    map[string]any{"type": "array", "items": integrationFieldSchema()},
			"env_only": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"env"},
					"properties": map[string]any{
						"env":  map[string]any{"type": "string"},
						"hint": map[string]any{"type": "string"},
					},
				},
				"description": "Settings the adapter accepts that the panel has no field for. They come from the " +
					"environment only, and are listed so an operator can find them.",
			},
		},
	}
}

// integrationConfigSchema is the configuration as a read path sees it.
func integrationConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"kind", "provider", "provider_source", "provider_editable", "enabled",
			"enabled_source", "enabled_editable", "extension_path", "extension_source", "values", "providers",
		},
		"properties": map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{"gateway", "server"}},
			"provider": map[string]any{"type": "string", "description": "The stored provider id, empty when nothing is configured."},
			"provider_name": map[string]any{
				"type": "string", "description": "The provider's human name; the raw id when this build cannot launch it.",
			},
			"provider_source": map[string]any{
				"type": "string", "enum": []string{"environment", "panel", "default", "unset"},
				"description": "`environment` means `BILLING_GATEWAY_PROVIDER` named it, which also enables the gateway.",
			},
			"provider_editable": map[string]any{"type": "boolean"},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "Whether the runtime should use this integration. A provisioner is enabled whenever a provider is stored; the gateway has a switch because taking money on first boot is a surprise.",
			},
			"enabled_source":   map[string]any{"type": "string", "enum": []string{"environment", "panel", "default", "unset"}},
			"enabled_editable": map[string]any{"type": "boolean"},
			"off_session_enabled": map[string]any{
				"type": "boolean",
				"description": "May a renewal charge a saved payment method with no customer present. Off by default: " +
					"it is the one setting that moves money with no human in the loop.",
			},
			"extension_path": map[string]any{
				"type": "string", "description": "The binary the worker and the server launch, and the binary the test launches.",
			},
			"extension_source": map[string]any{"type": "string", "enum": []string{"environment", "panel", "default", "unset"}},
			"values":           map[string]any{"type": "array", "items": integrationValueSchema()},
			"webhook_url": map[string]any{
				"type": "string",
				"description": "Where the provider must deliver events, derived from `BILLING_PUBLIC_URL` " +
					"(`<public-url>/webhooks/<provider>`). Empty when the public URL or the provider is unknown.",
			},
			"providers": map[string]any{"type": "array", "items": integrationProviderSchema()},
			"notes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Non-fatal remarks: a missing secret, a binary the runtime could not find. They are what makes the screen explain itself.",
			},
		},
	}
}

// integrationWriteSchema is the write body, shared by both screens.
func integrationWriteSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "The provider to launch. Absent leaves the stored provider unchanged; an id this build cannot launch is `unknown_provider`.",
			},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "The gateway switch. Enabling with no provider is refused; sending it to the provisioner is `unknown_setting`.",
			},
			"off_session_enabled": map[string]any{"type": "boolean"},
			"values": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Credential key -> text, matching the provider's declared `fields`. A secret sent as " +
					"`[redacted]` is skipped, so saving a loaded form cannot overwrite a credential with the marker. " +
					"An empty string clears it.",
			},
		},
	}
}

// gatewayDescriptionSchema is what a successful gateway test reports.
func gatewayDescriptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"provider", "extension", "protocol_version", "currencies", "methods", "tested_at"},
		"properties": map[string]any{
			"provider":      map[string]any{"type": "string"},
			"provider_name": map[string]any{"type": "string"},
			"extension": map[string]any{
				"type":        "string",
				"description": "The name the extension gave in the protocol handshake — not what the panel calls it.",
			},
			"publisher":        map[string]any{"type": "string"},
			"protocol_version": map[string]any{"type": "integer"},
			"capabilities":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"currencies": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The ISO 4217 codes the gateway will settle in. Check these against the catalog's currencies.",
			},
			"methods": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"id", "name"},
					"properties": map[string]any{
						"id":   map[string]any{"type": "string"},
						"name": map[string]any{"type": "string"},
					},
				},
			},
			"supports_refunds":  map[string]any{"type": "boolean"},
			"supports_webhooks": map[string]any{"type": "boolean"},
			"min_amount":        amountDescriptionSchema(),
			"max_amount":        amountDescriptionSchema(),
			"webhook_url":       map[string]any{"type": "string"},
			"tested_at":         map[string]any{"type": "string", "format": "date-time"},
			"duration_ms":       map[string]any{"type": "integer", "format": "int64"},
		},
	}
}

// serverDescriptionSchema is what a successful provisioner test reports.
func serverDescriptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"provider", "extension", "protocol_version", "plans", "regions", "tested_at"},
		"properties": map[string]any{
			"provider":         map[string]any{"type": "string"},
			"provider_name":    map[string]any{"type": "string"},
			"extension":        map[string]any{"type": "string"},
			"publisher":        map[string]any{"type": "string"},
			"protocol_version": map[string]any{"type": "integer"},
			"capabilities":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"plans": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"id", "name"},
					"properties": map[string]any{
						"id":     map[string]any{"type": "string"},
						"name":   map[string]any{"type": "string"},
						"amount": amountDescriptionSchema(),
						"period": map[string]any{"type": "string"},
					},
				},
			},
			"regions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"tested_at": map[string]any{"type": "string", "format": "date-time"},
			"duration_ms": map[string]any{
				"type": "integer", "format": "int64",
				"description": "Launch plus handshake plus describe. A slow adapter will time out under load.",
			},
		},
	}
}

// amountDescriptionSchema is money in a description: minor units, the code, and the display
// string the panel can show without arithmetic of its own.
func amountDescriptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"minor", "currency"},
		"properties": map[string]any{
			"minor":    map[string]any{"type": "integer", "format": "int64"},
			"currency": map[string]any{"type": "string"},
			"display":  map[string]any{"type": "string", "description": "The value formatted for the currency; empty when the code is not ISO 4217."},
		},
	}
}

// integrationTestSchema is the answer to a connection test.
func integrationTestSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"ok", "tested_at"},
		"properties": map[string]any{
			"ok": map[string]any{
				"type":        "boolean",
				"description": "Whether the extension launched, handshook and described itself. `false` is a normal answer, not a failed request.",
			},
			"code": map[string]any{
				"type": "string",
				"enum": []string{
					"not_configured", "extension_missing", "extension_crashed",
					"protocol_version_mismatch", "protocol_error", "timeout",
					"provider_refused", "test_failed",
				},
				"description": "A stable identifier for the failure, so a client can branch on it and a copy edit cannot break behaviour.",
			},
			"detail":    map[string]any{"type": "string"},
			"gateway":   gatewayDescriptionSchema(),
			"server":    serverDescriptionSchema(),
			"notes":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"tested_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

func integrationOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ gateway --
		"GET /api/v1/staff/gateways": {
			Summary: "Read the payment gateway configuration",
			Description: "Which provider is configured, whether it is switched on, which binary the runtime will " +
				"launch, and the state of each credential. A credential is never returned: `is_set` says whether " +
				"one exists and `source` says where the effective value came from. `source: environment` means the " +
				"deployment's environment variable wins over the saved row — the runtime will ignore a value saved " +
				"from the panel, so a client must render that field read-only. `webhook_url` is derived from " +
				"`BILLING_PUBLIC_URL`, not from the request's Host header, because the address a provider must " +
				"deliver to is the deployment's public origin. Reading requires `gateway:write`, because a " +
				"credential's shape and the public webhook URL are reconnaissance rather than support information.",
			Responses: []Response{
				{Status: 200, Description: "The gateway configuration", Schema: integrationConfigSchema()},
				{Status: 500, Description: "The settings table could not be read", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/gateways": {
			Summary: "Save the payment gateway configuration",
			Description: "Writes the provider, the switch and the credentials in one transaction, with one audit row " +
				"per changed key (action `gateway.config.update`) written inside that transaction. A credential " +
				"submitted as `[redacted]` is skipped, so saving a loaded form cannot overwrite a secret with the " +
				"marker; an empty string clears it. A field whose effective value comes from the environment is " +
				"refused with `env_override` (409) instead of being stored and ignored. An unknown provider is " +
				"`unknown_provider` (422), and enabling with no provider is `provider_required` (422) — both are " +
				"configurations that would otherwise fail at the first payment. The response is the committed " +
				"state, not an echo of the request.",
			RequestSchema: integrationWriteSchema(),
			Responses: []Response{
				{Status: 200, Description: "The configuration as it now reads", Schema: integrationConfigSchema()},
				{Status: 400, Description: "The body is not valid JSON, or a key is not declared for this group", Schema: ProblemRef()},
				{Status: 409, Description: "The environment supplies that value", Schema: ProblemRef()},
				{Status: 422, Description: "The provider is unknown, the switch has no provider, or a value does not parse", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/gateways/test": {
			Summary: "Test the payment gateway connection",
			Description: "Launches the saved configuration's extension — the same binary and the same environment the " +
				"worker uses — performs the protocol handshake and calls `gateway.describe`, then reports the " +
				"provider's name, its currencies and its methods. It is a `GET` because it changes nothing: no " +
				"settings row, no audit row and no idempotency record, so pressing it twice is two tests and not " +
				"two writes. It works while the gateway is switched off, because testing before enabling is the " +
				"point. It tests the *saved* configuration, never credentials in the query string: a secret in a " +
				"URL is a secret in the access log. A failed test is a successful request whose answer is \"no\": " +
				"the status is 200 and `ok` is false with a stable `code`.",
			Responses: []Response{
				{Status: 200, Description: "The test result, success or failure", Schema: integrationTestSchema()},
				{Status: 429, Description: "Too many tests in the window; each press starts a process", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------ servers --
		"GET /api/v1/staff/servers": {
			Summary: "Read the provisioning server configuration",
			Description: "The same shape as the gateway read: provider, whether it is on, the binary the runtime will " +
				"launch, and the state of each credential with its source. A provisioner has no environment " +
				"variable for its provider name — a deployment names its provisioner by the binary " +
				"`BILLING_PROVISIONER_EXTENSION` points at — so `servers.provider` is that field's only source. " +
				"Requires `server:write`.",
			Responses: []Response{
				{Status: 200, Description: "The provisioner configuration", Schema: integrationConfigSchema()},
				{Status: 500, Description: "The settings table could not be read", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/servers": {
			Summary: "Save the provisioning server configuration",
			Description: "Writes the provider and the panel URL and API key in one transaction, audited as " +
				"`server.config.update`. A provisioner has no enabled switch: it is on whenever a provider is " +
				"stored, so sending `enabled` here is `unknown_setting`. The environment wins over a saved value " +
				"per field, and an environment-supplied field is refused with `env_override` (409).",
			RequestSchema: integrationWriteSchema(),
			Responses: []Response{
				{Status: 200, Description: "The configuration as it now reads", Schema: integrationConfigSchema()},
				{Status: 400, Description: "The body is not valid JSON, or a key is not declared for this group", Schema: ProblemRef()},
				{Status: 409, Description: "The environment supplies that value", Schema: ProblemRef()},
				{Status: 422, Description: "The provider is unknown, or a value does not parse", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/servers/test": {
			Summary: "Test the provisioning server connection",
			Description: "Launches the saved configuration's provisioner and calls `provisioner.describe`, reporting its " +
				"plans, regions and capabilities. The same rules as the gateway test: a read, safe to press " +
				"twice, saved configuration only, and a failure is a 200 with `ok: false` and a stable code.",
			Responses: []Response{
				{Status: 200, Description: "The test result, success or failure", Schema: integrationTestSchema()},
				{Status: 429, Description: "Too many tests in the window; each press starts a process", Schema: ProblemRef()},
			},
		},
	}
}
