package openapi

// The extension-hook descriptions: extension-contributed UI and the one call it makes.
//
// The prose here carries the safety argument, because the safety argument is the feature.
// An extension does not ship code to the browser: it declares a navigation item and a
// service action as data, and the panel calls back exactly one method the extension
// advertised in its handshake. A client author reading this should come away knowing
// three things:
//
//   - The permission on a hook is the *extension's declared* permission, which the core
//     refused at registration unless it is one the core itself declares. A hook cannot
//     invent a capability.
//   - The list endpoint returns only what the caller may use, so a menu built from it
//     never shows an item that will fail on click.
//   - A hook call is a mutation: it requires an Idempotency-Key, which travels to the
//     extension as the operation key, so a retried click returns the first result instead
//     of acting twice.

func init() { RegisterDescriptions(extensionOperations) }

// hookNavItemSchema is one navigation item an extension contributes.
func hookNavItemSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"extension", "id", "label", "path", "icon", "permission"},
		"properties": map[string]any{
			"extension": map[string]any{
				"type":        "string",
				"description": "The extension that declared the item. It is what the client sends back when calling an action of the same extension.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Stable within the extension. The client keys on `extension` + `id`.",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Plain text. The client renders it as text, never as markup.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "An absolute in-panel path, e.g. `/staff/extensions/example-hooks`. It is a path the panel already routes; it is never an external URL and never a scheme such as `javascript:`.",
			},
			"icon": map[string]any{
				"type":        "string",
				"pattern":     "^[a-z0-9][a-z0-9-]*$",
				"description": "The name of an icon in the panel's own set. Not a URL and not SVG.",
			},
			"permission": map[string]any{
				"type":        "string",
				"description": "The permission required to see and use the item. It is one of the core's declared permissions; a declaration naming anything else was refused at registration.",
			},
		},
	}
}

// hookServiceActionSchema is one action an extension contributes to a service.
func hookServiceActionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"extension", "id", "label", "method", "permission"},
		"properties": map[string]any{
			"extension": map[string]any{"type": "string"},
			"id": map[string]any{
				"type":        "string",
				"description": "Stable within the extension; it is the `{action}` path segment of the call.",
			},
			"label": map[string]any{"type": "string", "description": "Plain text on the button."},
			"method": map[string]any{
				"type":        "string",
				"description": "The extension method the panel calls. It must be a capability the extension advertised in its handshake, and it is never one of the core's own protocol methods.",
			},
			"confirm": map[string]any{
				"type":        "string",
				"description": "The question the client asks before calling, e.g. `Reboot this service?`. Absent means the action is not destructive.",
			},
			"permission": map[string]any{
				"type":        "string",
				"description": "The permission required to call the action.",
			},
		},
	}
}

// hookCallResultSchema is what a hook call returns.
func hookCallResultSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"extension", "action", "method", "service_id"},
		"properties": map[string]any{
			"extension":  map[string]any{"type": "string"},
			"action":     map[string]any{"type": "string"},
			"method":     map[string]any{"type": "string"},
			"service_id": map[string]any{"type": "string", "format": "uuid"},
			"message": map[string]any{
				"type":        "string",
				"description": "A short human-readable result from the extension, shown to the operator.",
			},
			"data": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"description":          "Extension-specific detail, passed through untouched. The panel does not interpret it.",
			},
		},
	}
}

// extensionOperations is the prose for the extension-hook routes.
func extensionOperations() map[string]Operation {
	callDescription := "Calls one service action the extension declared, with the service id. " +
		"The guard is the *declared* permission, not the route's: the extension named a " +
		"permission at registration and the caller must hold exactly it, so a hook declared " +
		"as `service:terminate` cannot be called by someone holding only `service:write:self`. " +
		"The method called is the one the extension advertised in its handshake, which is what " +
		"keeps the surface at \"call this hook\" rather than \"run this code\". " +
		"The `Idempotency-Key` is passed to the extension as the operation key, so a retried " +
		"click returns the first result instead of acting twice. Every call is audited; a call " +
		"whose audit row cannot be written is not reported as a success."
	return map[string]Operation{
		"GET /api/v1/extensions/hooks": {
			Summary: "Extension-contributed navigation and service actions",
			Description: "What the installed extensions would like the panel to show, filtered to " +
				"the permissions the caller holds. An item whose permission the caller lacks is " +
				"omitted rather than shown and refused on click. The declarations are data only: " +
				"an extension never supplies markup, script or a URL to load, so contributing UI " +
				"does not mean running its code in an operator's session. " +
				"`Cache-Control: no-store`, because the answer depends on who is asking.",
			Responses: []Response{
				{
					Status:      200,
					Description: "The hooks this caller may use",
					Schema: map[string]any{
						"type":     "object",
						"required": []string{"nav_items", "service_actions"},
						"properties": map[string]any{
							"nav_items": map[string]any{
								"type":  "array",
								"items": hookNavItemSchema(),
							},
							"service_actions": map[string]any{
								"type":  "array",
								"items": hookServiceActionSchema(),
							},
						},
					},
				},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/services/{id}/hooks/{extension}/{action}": {
			Summary: "Call an extension's service action, as staff",
			Description: callDescription + " " +
				"The service may belong to any customer; the permission the action declares is " +
				"what decides whether this operator may act on it.",
			Responses: []Response{
				{Status: 200, Description: "The action ran", Schema: hookCallResultSchema()},
				{Status: 403, Description: "The caller does not hold the declared permission", Schema: ProblemRef()},
				{Status: 404, Description: "No installed extension offers that action, or no such service", Schema: ProblemRef()},
				{Status: 422, Description: "The extension refused the action", Schema: ProblemRef()},
				{Status: 502, Description: "The extension is not running or failed", Schema: ProblemRef()},
				{Status: 504, Description: "The extension did not answer in time", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/services/{id}/hooks/{extension}/{action}": {
			Summary: "Call an extension's service action on your own service",
			Description: callDescription + " " +
				"The service id is resolved in SQL against the caller's own account, so another " +
				"customer's service is a 404 rather than a 403 that confirms it exists.",
			Responses: []Response{
				{Status: 200, Description: "The action ran", Schema: hookCallResultSchema()},
				{Status: 403, Description: "The caller does not hold the declared permission", Schema: ProblemRef()},
				{Status: 404, Description: "No such service, or no installed extension offers that action", Schema: ProblemRef()},
				{Status: 422, Description: "The extension refused the action", Schema: ProblemRef()},
				{Status: 502, Description: "The extension is not running or failed", Schema: ProblemRef()},
				{Status: 504, Description: "The extension did not answer in time", Schema: ProblemRef()},
			},
		},
	}
}
