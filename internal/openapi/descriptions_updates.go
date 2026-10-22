package openapi

// The updates, telemetry and marketplace descriptions (OPS-12, OPS-13, OPS-14, ADM-12,
// ADM-23).
//
// Four things belong in the document rather than only in the handler, because they are
// contract, not implementation:
//
//   - **The panel does not apply updates.** `apply.mode` is always `operator` and `apply.steps`
//     is a procedure. A client that renders an "install now" button would be rendering
//     something that does not exist, so the document says so in the response schema and in the
//     prose rather than leaving it to a reader of the handler.
//
//   - **The check is cached.** `check.cached` and `check.expires_at` are part of the contract,
//     because a client that assumes every read fetched the update server would show a stale
//     answer as fresh. `POST /updates/check` is the only route that ignores the cache.
//
//   - **The telemetry payload is a closed document.** Its keys are listed here so a client can
//     render "what is sent" from the contract rather than from a screenshot, and the payload is
//     `null` when telemetry is off.
//
//   - **An install is verified, not running.** `status` is `verified` and `activation` is an
//     instruction. The digest is checked before anything is recorded, and `trust` states what
//     that check did and did not prove.
//
// Response schemas are inline for the same reason the other domain files' are: the component
// list belongs to openapi.go, and a reference to an undefined component resolves to nothing.

func init() { RegisterDescriptions(updatesOperations) }

// updateCheckSchema is the cached update check.
func updateCheckSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"configured", "source_url", "channel", "running_version", "outcome", "checked_at", "cached",
		},
		"properties": map[string]any{
			"configured": map[string]any{
				"type":        "boolean",
				"description": "False when `updates.check_url` is empty. Not an error: an installation with no update server configured is supported.",
			},
			"source_url":      map[string]any{"type": "string", "description": "The check URL the answer came from."},
			"channel":         map[string]any{"type": "string", "enum": []string{"stable", "beta"}},
			"running_version": map[string]any{"type": "string", "description": "The version this build was stamped with (`-ldflags \"-X main.version=…\"`). The default stamp is `dev`."},
			"latest_version":  map[string]any{"type": "string"},
			"download_url": map[string]any{
				"type":        "string",
				"description": "A link for a human to follow. The panel never downloads a release artifact.",
			},
			"notes":        map[string]any{"type": "string"},
			"published_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"outcome": map[string]any{
				"type": "string",
				"enum": []string{"not_configured", "up_to_date", "available", "ahead", "unknown", "unreachable"},
				"description": "`unknown` is the honest answer for a build with no comparable version stamp. " +
					"`unreachable` means the last check failed; `failure_code` says why.",
			},
			"checked_at": map[string]any{"type": "string", "format": "date-time"},
			"cached": map[string]any{
				"type":        "boolean",
				"description": "True when this answer came from the cache rather than a fetch in this request.",
			},
			"expires_at":   map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"failure_code": map[string]any{"type": "string", "description": "Empty for a successful check."},
			"detail":       map[string]any{"type": "string"},
		},
	}
}

// updateApprovalSchema is the recorded decision.
func updateApprovalSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"version", "note", "approved_by", "approved_at"},
		"properties": map[string]any{
			"version":     map[string]any{"type": "string", "description": "The approved target version."},
			"note":        map[string]any{"type": "string"},
			"approved_by": map[string]any{"type": "string", "format": "uuid"},
			"approved_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// updateApplySchema is the honest answer to "can you apply it".
func updateApplySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"mode", "steps", "note"},
		"properties": map[string]any{
			"mode": map[string]any{
				"type": "string",
				"enum": []string{"operator"},
				"description": "Always `operator`. There is no automated mode: the deployment is a plain binary " +
					"with no signed release to verify and no supervisor for the panel to ask for a restart.",
			},
			"steps": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The deployment procedure, in order.",
			},
			"note": map[string]any{"type": "string"},
		},
	}
}

// updateStateSchema is the whole updates screen payload.
func updateStateSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"running_version", "channel", "check", "approval_matches_check", "telemetry", "apply", "server_time",
		},
		"properties": map[string]any{
			"running_version": map[string]any{"type": "string"},
			"channel":         map[string]any{"type": "string", "enum": []string{"stable", "beta"}},
			"check":           updateCheckSchema(),
			"approval": map[string]any{
				"description": "The approved version, or null. Approval records a decision; it does not apply anything.",
				"oneOf":       []any{updateApprovalSchema(), map[string]any{"type": "null"}},
			},
			"approval_matches_check": map[string]any{
				"type": "boolean",
				"description": "True when an approval exists, a newer version is available, and they are the same " +
					"version. This is what \"an update is available and approved\" means precisely.",
			},
			"telemetry": map[string]any{
				"type":     "object",
				"required": []string{"enabled", "endpoint_configured", "instance_id"},
				"properties": map[string]any{
					"enabled": map[string]any{
						"type":        "boolean",
						"description": "`updates.telemetry`. Off unless an operator turns it on.",
					},
					"endpoint_configured": map[string]any{"type": "boolean"},
					"endpoint_host": map[string]any{
						"type":        "string",
						"description": "The endpoint's host only. The URL is a credential and is never returned.",
					},
					"instance_id": map[string]any{
						"type":        "string",
						"format":      "uuid",
						"description": "A random identifier for this installation, generated by the database. Not derived from any customer.",
					},
					"last_send": map[string]any{
						"oneOf": []any{
							map[string]any{
								"type":     "object",
								"required": []string{"id", "sent_at", "status", "endpoint_host"},
								"properties": map[string]any{
									"id":            map[string]any{"type": "string", "format": "uuid"},
									"sent_at":       map[string]any{"type": "string", "format": "date-time"},
									"status":        map[string]any{"type": "string", "enum": []string{"sent", "failed"}},
									"http_status":   map[string]any{"type": "integer"},
									"failure_code":  map[string]any{"type": "string"},
									"endpoint_host": map[string]any{"type": "string"},
								},
							},
							map[string]any{"type": "null"},
						},
					},
				},
			},
			"apply":       updateApplySchema(),
			"server_time": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// telemetryPayloadSchema is the closed telemetry document.
//
// Every count is an integer and every string is a version, an identifier or a platform name.
// The description is the promise, and internal/updates' test asserts it.
func telemetryPayloadSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"schema_version", "instance_id", "panel_version", "channel", "go_version",
			"os", "arch", "database_schema_version", "generated_at", "counts",
		},
		"additionalProperties": false,
		"description": "A fixed set of counts and versions. No customer identifier, name or address; " +
			"no invoice or order contents; no hostname, domain or public URL; no operator identity.",
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "integer", "enum": []int{1}},
			"instance_id":    map[string]any{"type": "string", "format": "uuid"},
			"panel_version":  map[string]any{"type": "string"},
			"channel":        map[string]any{"type": "string", "enum": []string{"stable", "beta"}},
			"go_version":     map[string]any{"type": "string"},
			"os":             map[string]any{"type": "string"},
			"arch":           map[string]any{"type": "string"},
			"database_schema_version": map[string]any{
				"type":        "integer",
				"description": "The highest applied migration version. A number, not a file name: a path is the kind of detail that should not leave the installation.",
			},
			"generated_at": map[string]any{"type": "string", "format": "date-time"},
			"counts": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required": []string{
					"customers", "services_active", "services_suspended", "invoices_open",
					"invoices_paid_last_30d", "tickets_open", "extensions_installed",
				},
				"properties": map[string]any{
					"customers":              map[string]any{"type": "integer"},
					"services_active":        map[string]any{"type": "integer"},
					"services_suspended":     map[string]any{"type": "integer"},
					"invoices_open":          map[string]any{"type": "integer"},
					"invoices_paid_last_30d": map[string]any{"type": "integer"},
					"tickets_open":           map[string]any{"type": "integer"},
					"extensions_installed":   map[string]any{"type": "integer"},
				},
			},
		},
	}
}

// installedExtensionSchema is one verified extension.
func installedExtensionSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"name", "version", "sha256", "pinned", "size_bytes", "installed_at", "status",
		},
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9._-]{0,63}$"},
			"version":     map[string]any{"type": "string"},
			"publisher":   map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"kind":        map[string]any{"type": "string", "enum": []string{"gateway", "provisioner", "other"}},
			"sha256": map[string]any{
				"type":        "string",
				"pattern":     "^[0-9a-f]{64}$",
				"description": "The digest that was verified against the downloaded bytes.",
			},
			"pinned": map[string]any{
				"type":        "boolean",
				"description": "The operator supplied the digest out of band, so the index could not have changed it.",
			},
			"source_url":   map[string]any{"type": "string"},
			"size_bytes":   map[string]any{"type": "integer", "format": "int64"},
			"staged_path":  map[string]any{"type": "string", "description": "Where a copy was written, when a staging directory is configured. Empty means the verified bytes are stored in the panel only."},
			"installed_at": map[string]any{"type": "string", "format": "date-time"},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"verified"},
				"description": "Always `verified`. Never `running`: the core loads extensions as separate processes " +
					"from paths in its own environment and has no runtime registration.",
			},
		},
	}
}

// marketplaceIndexSchema is the fetched listing.
func marketplaceIndexSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"schema_version", "extensions"},
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "integer", "enum": []int{1}},
			"extensions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"required": []string{
						"name", "version", "publisher", "description", "kind", "download_url", "sha256",
					},
					"properties": map[string]any{
						"name":         map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9._-]{0,63}$"},
						"version":      map[string]any{"type": "string"},
						"publisher":    map[string]any{"type": "string"},
						"description":  map[string]any{"type": "string"},
						"kind":         map[string]any{"type": "string", "enum": []string{"gateway", "provisioner", "other"}},
						"download_url": map[string]any{"type": "string"},
						"sha256":       map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
						"homepage":     map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

// marketplaceStateSchema is the marketplace screen payload.
func marketplaceStateSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"configured", "index_url", "install_dir", "index", "installed", "trust"},
		"properties": map[string]any{
			"configured": map[string]any{
				"type":        "boolean",
				"description": "False when no index URL is set. Rendered as a state, not as an empty marketplace.",
			},
			"index_url":   map[string]any{"type": "string"},
			"install_dir": map[string]any{"type": "string", "description": "Where a verified artifact is staged. Empty means the panel stores it only."},
			"fetched_at":  map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"cached":      map[string]any{"type": "boolean"},
			"index":       marketplaceIndexSchema(),
			"failure_code": map[string]any{
				"type":        "string",
				"description": "Set when the last index fetch failed, so a broken index is visible rather than looking like an empty marketplace.",
			},
			"installed": map[string]any{"type": "array", "items": installedExtensionSchema()},
			"trust": map[string]any{
				"type": "string",
				"description": "What a digest check proves: a digest from the index catches a corrupted transfer, " +
					"not a compromised index, because whoever controls the index controls the digest. Pin the " +
					"digest out of band to close that gap.",
			},
		},
	}
}

// installResultSchema is what a verified install reports.
func installResultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"name", "version", "sha256", "pinned", "verified_at", "size_bytes", "status", "activation", "trust",
		},
		"properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"version":     map[string]any{"type": "string"},
			"publisher":   map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"kind":        map[string]any{"type": "string", "enum": []string{"gateway", "provisioner", "other"}},
			"sha256":      map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
			"pinned":      map[string]any{"type": "boolean"},
			"verified_at": map[string]any{"type": "string", "format": "date-time"},
			"size_bytes":  map[string]any{"type": "integer", "format": "int64"},
			"staged_path": map[string]any{"type": "string"},
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{"verified"},
				"description": "`verified`: the artifact is checked and stored. The panel never reports `running`.",
			},
			"activation": map[string]any{
				"type": "string",
				"description": "What an operator must do to make the core use it: place the artifact and set " +
					"`BILLING_GATEWAY_EXTENSION` or `BILLING_PROVISIONER_EXTENSION` to its path.",
			},
			"trust": map[string]any{"type": "string"},
		},
	}
}

func updatesOperations() map[string]Operation {
	return map[string]Operation{
		// --------------------------------------------------------------- updates --
		"GET /api/v1/staff/updates": {
			Summary: "The running version, the update check, the approval and the telemetry state",
			Description: "One read, so the screen cannot render a mix of two moments. The check is served from " +
				"the cache when it is fresh: a check on every page load would be a denial of service against " +
				"the update server and against us. `apply` is a procedure, never an automated mode — the panel " +
				"cannot replace its own binary, because this deployment has no signed release to verify and no " +
				"supervisor for the process to ask for a restart.",
			Responses: []Response{
				{Status: 200, Description: "The updates state", Schema: updateStateSchema()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
				{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/updates/check": {
			Summary: "Check for a newer version, ignoring the cache",
			Description: "Fetches `updates.check_url` with a bounded timeout and a bounded body, compares the " +
				"published version against the running one, and stores the answer. This is the only route that " +
				"ignores the cache; the screen read uses it. A failed fetch is cached with a shorter TTL and " +
				"answered as 502 with a stable code, because an operator who asked for an answer and did not get " +
				"one should be told.",
			Responses: []Response{
				{Status: 200, Description: "The check result", Schema: updateCheckSchema()},
				{Status: 422, Description: "No check URL is configured", Schema: ProblemRef()},
				{Status: 502, Description: "The update server could not be reached, answered non-2xx, or returned a manifest that is not valid", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/updates/approval": {
			Summary: "Approve a target version",
			Description: "Records which version an operator approved, and returns the state so the screen shows " +
				"the decision immediately. It applies nothing: the deployment is a plain binary that this process " +
				"cannot replace, so approving records the decision and `apply.steps` is the procedure. The version " +
				"must parse as a semantic version and must be newer than the running build; a build stamped `dev` " +
				"has nothing to compare with and is exempt from the second rule.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"version"},
				"properties": map[string]any{
					"version": map[string]any{"type": "string", "description": "The target release, e.g. `1.5.0`."},
					"note":    map[string]any{"type": "string", "description": "Why, in the operator's words. It is what an incident review reads later."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The updates state with the approval", Schema: updateStateSchema()},
				{Status: 422, Description: "The version does not parse, or is not newer than the running version", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/updates/approval": {
			Summary: "Withdraw the approved version",
			Description: "Deletes the approval row and audits the withdrawal. There is no history table here: the " +
				"audit log is where the history of approvals lives, and a second copy would be a second thing to " +
				"keep true.",
			Responses: []Response{
				{Status: 200, Description: "The updates state without an approval", Schema: updateStateSchema()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/updates/settings": {
			Summary: "Configure the update check and telemetry",
			Description: "Writes `updates.check_url`, `updates.channel` and `updates.telemetry` through the settings " +
				"store, so validation, defaults and the audit row are the same ones every other settings write gets. " +
				"The telemetry endpoint is written to `update_config` rather than to `settings`, because it commonly " +
				"carries an ingest token and the settings table's read paths are shared with a generic settings page. " +
				"Every field is optional: a request that omits one leaves it unchanged.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"check_url": map[string]any{
						"type":        "string",
						"description": "An http(s) URL serving `{version, download_url, notes, published_at}`. Empty disables the check.",
					},
					"channel":            map[string]any{"type": "string", "enum": []string{"stable", "beta"}},
					"telemetry":          map[string]any{"type": "boolean", "description": "Off by default. When on, the payload is a fixed set of counts and versions."},
					"telemetry_endpoint": map[string]any{"type": "string", "description": "Where a payload is POSTed. A credential: it is never returned by a read path."},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The updates state after the change", Schema: updateStateSchema()},
				{Status: 400, Description: "The request carries no setting to write", Schema: ProblemRef()},
				{Status: 422, Description: "A URL is not http(s), or the channel is not a known value", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/updates/telemetry": {
			Summary: "Preview the telemetry payload",
			Description: "Builds the payload without sending it. When telemetry is off, `payload` is null and " +
				"`payload_json` is empty: \"off\" means no payload exists, not that one is waiting. The document is " +
				"a closed set of counts and versions — no customer identifier, name or address, no invoice or order " +
				"contents, no hostname or public URL, no operator identity.",
			Responses: []Response{
				{Status: 200, Description: "The preview", Schema: map[string]any{
					"type":     "object",
					"required": []string{"enabled", "payload", "payload_json", "delivery_possible", "fields"},
					"properties": map[string]any{
						"enabled": map[string]any{"type": "boolean"},
						"payload": map[string]any{
							"oneOf": []any{telemetryPayloadSchema(), map[string]any{"type": "null"}},
						},
						"payload_json": map[string]any{
							"type":        "string",
							"description": "The exact bytes that would be sent, so an operator reads them without re-serialising.",
						},
						"endpoint_host":     map[string]any{"type": "string"},
						"delivery_possible": map[string]any{"type": "boolean", "description": "False when no endpoint is configured."},
						"fields":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"last_send":         map[string]any{"type": []string{"object", "null"}},
					},
				}},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/updates/telemetry/send": {
			Summary: "Send the telemetry payload now",
			Description: "Delivers the payload and records the attempt, whether or not it succeeded, with the digest " +
				"of the exact bytes sent. A delivery the receiver rejects is a 502: an operator who clicked send and " +
				"got a 200 would reasonably conclude it arrived.",
			Responses: []Response{
				{Status: 200, Description: "The delivery result", Schema: map[string]any{
					"type":     "object",
					"required": []string{"id", "sent_at", "status", "payload_sha256", "endpoint_host"},
					"properties": map[string]any{
						"id":             map[string]any{"type": "string", "format": "uuid"},
						"sent_at":        map[string]any{"type": "string", "format": "date-time"},
						"status":         map[string]any{"type": "string", "enum": []string{"sent", "failed"}},
						"http_status":    map[string]any{"type": "integer"},
						"failure_code":   map[string]any{"type": "string"},
						"payload_sha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
						"endpoint_host":  map[string]any{"type": "string"},
					},
				}},
				{Status: 422, Description: "Telemetry is off, or no endpoint is configured", Schema: ProblemRef()},
				{Status: 502, Description: "The endpoint rejected the payload", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold update:manage", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------------- marketplace --
		"GET /api/v1/staff/marketplace": {
			Summary: "The marketplace index and what is installed",
			Description: "The configured index URL, the cached listing, and every verified extension. The listing " +
				"is cached, so a page load does not become a fetch against the index host; `POST /refresh` is the " +
				"on-demand path. `trust` states what a digest check proves, so the screen can say it once rather " +
				"than leaving an operator to assume a green tick means the artifact is signed.",
			Responses: []Response{
				{Status: 200, Description: "The marketplace state", Schema: marketplaceStateSchema()},
				{Status: 403, Description: "The caller does not hold extension:manage", Schema: ProblemRef()},
				{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/marketplace/settings": {
			Summary: "Configure the marketplace index and staging directory",
			Description: "Where the listing is fetched from and where a verified artifact is staged. `install_dir` " +
				"empty means the verified bytes live in the panel only, which is still a real install: the artifact " +
				"can be downloaded back byte for byte. A URL that is not http(s) is refused when it is saved rather " +
				"than discovered when the fetch runs.",
			RequestSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index_url":   map[string]any{"type": "string"},
					"install_dir": map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The marketplace state after the change", Schema: marketplaceStateSchema()},
				{Status: 400, Description: "The request carries no setting to write", Schema: ProblemRef()},
				{Status: 422, Description: "The index URL is not http(s)", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold extension:manage", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/marketplace/refresh": {
			Summary: "Fetch the marketplace index now",
			Description: "The only route that ignores the index cache. The fetch is bounded in time and size, and the " +
				"document is validated completely before anything is shown: an index with one bad entry is refused " +
				"as a whole rather than rendered with the bad entry silently dropped.",
			Responses: []Response{
				{Status: 200, Description: "The marketplace state after the refresh", Schema: marketplaceStateSchema()},
				{Status: 422, Description: "No index URL is configured", Schema: ProblemRef()},
				{Status: 502, Description: "The index could not be fetched or is not a valid index", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold extension:manage", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/marketplace/install": {
			Summary: "Download, verify and record an extension",
			Description: "Downloads the artifact with a bounded size, compares its SHA-256 with the index before " +
				"anything is recorded, and stores the verified bytes with their digest. A mismatch refuses with " +
				"`marketplace_digest_mismatch` and leaves nothing behind. `pinned_sha256` lets an operator supply the " +
				"digest out of band; the install then requires the index and the artifact to match the pin, which is " +
				"the only check a compromised index cannot forge. Nothing is executed and nothing is registered: the " +
				"core loads extensions from paths in its own environment, so the result is `verified` with an " +
				"`activation` instruction.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"name"},
				"properties": map[string]any{
					"name":    map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9._-]{0,63}$"},
					"version": map[string]any{"type": "string", "description": "Empty installs the highest version listed."},
					"pinned_sha256": map[string]any{
						"type":        "string",
						"pattern":     "^[0-9a-fA-F]{64}$",
						"description": "The digest from the release announcement. Refused unless it matches both the index and the downloaded bytes.",
					},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The verified extension", Schema: installResultSchema()},
				{Status: 404, Description: "No extension with that name and version is in the index", Schema: ProblemRef()},
				{Status: 413, Description: "The artifact is larger than the limit", Schema: ProblemRef()},
				{Status: 422, Description: "The digest did not match, the pin disagrees, the version is already installed with different bytes, or the artifact is empty", Schema: ProblemRef()},
				{Status: 502, Description: "The artifact or the index could not be fetched", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold extension:manage", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/marketplace/installed/{name}/{version}/artifact": {
			Summary: "Download the verified artifact",
			Description: "Returns exactly the bytes whose digest was checked, with the digest in `X-Content-SHA256`, " +
				"so the operator deploys the artifact that was verified rather than a fresh download from an index " +
				"that may have changed since.",
			Responses: []Response{
				{Status: 200, Description: "The artifact"},
				{Status: 404, Description: "No such installed extension", Schema: ProblemRef()},
				{Status: 403, Description: "The caller does not hold extension:manage", Schema: ProblemRef()},
			},
		},
	}
}
