package openapi

// Web push: the caller's own browser subscriptions, and a test notification.
//
// These routes are `AccessUser`, not permissioned, and the document says so: the security
// requirement is a session, and there is no `x-permission` because "subscribe my own browser to
// my own notifications" is not a privilege anybody can hold or be granted. Every statement is
// scoped to the caller's user id in SQL, so another account's subscription id is a 404 — which
// is documented on the delete operation, because a generated client that treats 404 as "already
// deleted" and one that treats it as "wrong account" behave differently and both are reasonable.

func init() { RegisterDescriptions(pushOperations) }

// pushConfigSchema is GET /api/v1/push/config.
func pushConfigSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"enabled", "vapid_public_key", "max_devices"},
		"properties": map[string]any{
			"enabled": map[string]any{
				"type": "boolean",
				"description": "True only when push is switched on and the VAPID key pair is present and " +
					"usable. The portal hides its \"enable notifications\" control when this is false: " +
					"offering a button that cannot work is worse than offering none.",
			},
			"vapid_public_key": map[string]any{
				"type": "string",
				"description": "Base64url VAPID public key, an uncompressed P-256 point. Pass it to " +
					"`pushManager.subscribe({applicationServerKey})` as bytes. Empty when `enabled` is false.",
			},
			"reason": map[string]any{
				"type": "string",
				"enum": []string{"disabled", "keys_missing", "keys_invalid"},
				"description": "Why push is unavailable. Empty when it is available. `keys_invalid` means the " +
					"stored key pair is unusable — a half-copied value, a key on the wrong curve, or two halves " +
					"from different pairs.",
			},
			"max_devices": map[string]any{
				"type":        "integer",
				"description": "How many devices one account may register.",
			},
		},
	}
}

// pushSubscriptionSchema is one registered device.
//
// It deliberately has no field for the auth secret or the p256dh key. The device already holds
// both, and a response that echoed them back would be a secret in a log line the first time
// somebody debugged this endpoint.
func pushSubscriptionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "endpoint", "created_at"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "format": "uuid"},
			"endpoint": map[string]any{
				"type":        "string",
				"format":      "uri",
				"description": "The push service URL this device subscribed with. Always https.",
			},
			"user_agent": map[string]any{
				"type":        "string",
				"description": "What the browser reported when it subscribed, truncated. A label, never parsed.",
			},
			"created_at": map[string]any{
				"type":        "string",
				"format":      "date-time",
				"description": "When this endpoint was first registered. Re-subscribing keeps the original value: it is the device's first appearance, not the last.",
			},
			"last_used_at": map[string]any{
				"type":        []string{"string", "null"},
				"format":      "date-time",
				"description": "When a push was last handed to the push service. Null means the device has never received one.",
			},
		},
	}
}

// pushTestResultSchema is the summary a test push returns.
func pushTestResultSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"sent", "removed", "failed"},
		"properties": map[string]any{
			"sent": map[string]any{
				"type":        "integer",
				"description": "Devices the push service accepted the notification for.",
			},
			"removed": map[string]any{
				"type": "integer",
				"description": "Devices deleted because the push service answered 404 or 410. That is a dead " +
					"device — the browser was uninstalled or its site data was cleared — not an error, and the " +
					"subscription is gone from the next list.",
			},
			"failed": map[string]any{
				"type": "integer",
				"description": "Devices that could not be reached for a transient reason (a 5xx, a rate limit, " +
					"a network error). The subscription is kept: these are retried by the next notification.",
			},
			"failures": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Up to three human-readable reasons, for the screen and the log. Never a database message.",
			},
		},
	}
}

func pushOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ config --
		"GET /api/v1/push/config": {
			Summary: "Whether web push is available, and the key to subscribe with",
			Description: "The portal reads this before rendering its notification control. `enabled` is false " +
				"when the operator has switched push off or when the VAPID key pair is missing or unusable, and " +
				"the screen then hides the control rather than offering one that cannot work. The public key is " +
				"served here rather than stored in the frontend so a key rotation reaches every browser without " +
				"a rebuild.",
			Responses: []Response{
				{Status: 200, Description: "The push configuration", Schema: pushConfigSchema()},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------- subscriptions --
		"GET /api/v1/push/subscriptions": {
			Summary: "List the caller's push devices",
			Description: "Newest first. The user id comes from the session and is in the WHERE clause, so this " +
				"cannot be asked for another account's devices. The auth secret and the device's public key are " +
				"not returned: the device already has them, and echoing a secret is a leak waiting for a log line.",
			Responses: []Response{
				{Status: 200, Description: "The caller's devices", Schema: map[string]any{
					"type":     "object",
					"required": []string{"data", "max_devices"},
					"properties": map[string]any{
						"data": map[string]any{
							"type":  "array",
							"items": pushSubscriptionSchema(),
						},
						"max_devices": map[string]any{"type": "integer"},
					},
				}},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/push/subscriptions": {
			Summary: "Register a browser for push notifications",
			Description: "The body is the browser's own `PushSubscription.toJSON()`, passed through unchanged: " +
				"`endpoint`, and a `keys` object with `p256dh` and `auth`. The endpoint is the key. Re-subscribing " +
				"the same endpoint replaces the row rather than adding a second one — including its owner, " +
				"because a browser subscription belongs to the browser profile and not to an account, so a shared " +
				"laptop that signs into another account must stop delivering the previous one's notifications. " +
				"The endpoint must be https: the server POSTs to it, so a plaintext or non-URL value is refused " +
				"with 422 `invalid_subscription` rather than being stored and retried forever. An account may " +
				"register at most `max_devices` devices; a new one beyond that is 422 `push_device_limit`.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"endpoint", "keys"},
				"properties": map[string]any{
					"endpoint": map[string]any{"type": "string", "format": "uri"},
					"keys": map[string]any{
						"type":     "object",
						"required": []string{"p256dh", "auth"},
						"properties": map[string]any{
							"p256dh": map[string]any{
								"type":        "string",
								"description": "The device's P-256 public key, base64url, uncompressed point.",
							},
							"auth": map[string]any{
								"type":        "string",
								"description": "The 16-byte shared auth secret, base64url. RFC 8291.",
							},
						},
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The registered device", Schema: pushSubscriptionSchema()},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
				{Status: 422, Description: "`invalid_subscription` or `push_device_limit`", Schema: ProblemRef()},
				{Status: 503, Description: "`push_not_configured`: no VAPID key pair is set", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/push/subscriptions/{id}": {
			Summary: "Unregister one of the caller's push devices",
			Description: "Scoped to the caller in SQL: another account's subscription id matches no row and " +
				"answers 404 `not_found`, the same answer as an id that never existed, so the endpoint is not an " +
				"oracle for which ids exist. Deleting an already-deleted subscription is also 404; a client that " +
				"treats that as success is right to.",
			Responses: []Response{
				{Status: 200, Description: "The device was removed", Schema: map[string]any{
					"type":     "object",
					"required": []string{"status", "id"},
					"properties": map[string]any{
						"status": map[string]any{"type": "string", "enum": []string{"deleted"}},
						"id":     map[string]any{"type": "string", "format": "uuid"},
					},
				}},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
				{Status: 404, Description: "`not_found`: no such device for this account", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- test --
		"POST /api/v1/push/test": {
			Summary: "Send a test notification to every device on the account",
			Description: "A real delivery: the same RFC 8291 encryption, the same VAPID signature, the same " +
				"dead-device cleanup as any notification the panel sends. The body is empty. The event key is " +
				"empty on purpose — a test push is not an event, and a customer who pressed the button has " +
				"already consented to receiving it, so the preference lookup is skipped rather than consulted. " +
				"409 `no_push_device` means there was nothing to send to, either because no device is " +
				"registered or because every registered device turned out to be gone and was removed. 502 " +
				"`push_delivery_failed` means the push service refused: usually a VAPID key pair that does not " +
				"match, a subject the service rejects, or a clock far enough out that the token is expired.",
			Responses: []Response{
				{Status: 200, Description: "Delivered to at least one device", Schema: pushTestResultSchema()},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
				{Status: 409, Description: "`no_push_device`", Schema: ProblemRef()},
				{Status: 502, Description: "`push_delivery_failed`", Schema: ProblemRef()},
				{Status: 503, Description: "`push_not_configured`", Schema: ProblemRef()},
			},
		},
	}
}
