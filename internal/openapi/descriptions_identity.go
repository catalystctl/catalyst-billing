package openapi

// Identity depth: sessions, impersonation, roles and API-key allowlists (IDN-12, IDN-13,
// IDN-14, IDN-15, IDN-17, ADM-13, ADM-30).
//
// The prose here is deliberately specific about what a caller may not do, because that is
// what a client author needs to know before wiring a button: a session list never contains a
// token, a role matrix refuses the four system roles, and an impersonated session is a real
// session with a named operator behind it.

func init() { RegisterDescriptions(identityOperations) }

func identityOperations() map[string]Operation {
	session := map[string]any{
		"type":     "object",
		"required": []string{"id", "created_at", "last_seen_at", "ip", "user_agent", "current"},
		"properties": map[string]any{
			"id":              map[string]any{"type": "string", "format": "uuid"},
			"user_id":         map[string]any{"type": "string", "format": "uuid"},
			"created_at":      map[string]any{"type": "string", "format": "date-time"},
			"last_seen_at":    map[string]any{"type": "string", "format": "date-time"},
			"expires_at":      map[string]any{"type": "string", "format": "date-time"},
			"ip":              map[string]any{"type": "string", "description": "The address the session was created from, or empty when unknown."},
			"user_agent":      map[string]any{"type": "string"},
			"current":         map[string]any{"type": "boolean", "description": "True for the session that made this request."},
			"impersonated_by": map[string]any{"type": []string{"string", "null"}, "description": "The operator behind an impersonated session."},
		},
	}

	authEvent := map[string]any{
		"type":     "object",
		"required": []string{"id", "event", "created_at"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "integer", "format": "int64"},
			"event":      map[string]any{"type": "string", "description": "e.g. login_succeeded, login_failed, password_changed, impersonation_started."},
			"detail":     map[string]any{"type": "string"},
			"ip":         map[string]any{"type": "string"},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}

	role := map[string]any{
		"type":     "object",
		"required": []string{"id", "name", "is_system", "permissions"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"is_system":   map[string]any{"type": "boolean", "description": "A system role cannot be edited or deleted."},
			"created_at":  map[string]any{"type": "string", "format": "date-time"},
			"permissions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}

	return map[string]Operation{
		// ---------------------------------------------------------- sessions --
		"GET /api/v1/portal/sessions": {
			Summary: "The caller's live sessions",
			Description: "Where this account is signed in, newest activity first. The token and the CSRF secret are never " +
				"returned: the list describes sessions, it does not hand out credentials. `current` marks the session that " +
				"made the request, so a client can render \"this device\" without comparing anything. Expired and revoked " +
				"sessions are excluded — they cannot authenticate, and they are already in the login history.",
			Responses: []Response{{Status: 200, Description: "The sessions", Schema: PageSchema(session)}},
		},
		"DELETE /api/v1/portal/sessions/{id}": {
			Summary: "Revoke one of the caller's sessions",
			Description: "Ownership is part of the query, so another user's session id returns 404 — identical to an id that " +
				"never existed. Revoking the session that made the request is allowed and is a logout: the response says " +
				"`current: true` and both cookies are cleared.",
			Responses: []Response{
				{Status: 200, Description: "Revoked, or signed out when it was the current session"},
				{Status: 404, Description: "No such session for this user", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/sessions/revoke-others": {
			Summary: "Sign out everywhere else",
			Description: "Revokes every live session except the one making the request, and reports how many were ended. The " +
				"current session is kept so the caller is not signed out by their own security action.",
			Responses: []Response{
				{Status: 200, Description: "The others were revoked"},
				{Status: 400, Description: "The request is not authenticated by a session (an API key has none to keep)",
					Schema: ProblemRef()},
			},
		},
		"GET /api/v1/portal/login-history": {
			Summary: "The caller's authentication history",
			Description: "Every auth event recorded for this account — successful and failed logins, password changes, " +
				"impersonation start and stop — newest first, keyset-paginated. Scoped by user id in SQL: there is no " +
				"parameter that names another account.",
			Responses: []Response{{Status: 200, Description: "A page of events", Schema: PageSchema(authEvent)}},
		},
		"GET /api/v1/staff/sessions": {
			Summary: "Sessions, for the caller or for a named user",
			Description: "Without `user_id` this is the caller's own list, identical to the portal route. Naming another user " +
				"requires `staff:manage`; a staff member without it gets 403 rather than an empty page, so the difference " +
				"between \"nobody is signed in\" and \"you may not look\" stays visible.",
			Responses: []Response{
				{Status: 200, Description: "The sessions", Schema: PageSchema(session)},
				{Status: 403, Description: "Naming another user requires staff:manage", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/sessions/{id}": {
			Summary: "Revoke a session as an administrator",
			Description: "Requires `staff:manage`. The subject is the caller unless `user_id` names someone else, and the " +
				"session must belong to that subject. Revoking your own current session signs you out, exactly as the " +
				"portal route does: the consequence of revoking a session must not depend on the URL used.",
			Responses: []Response{
				{Status: 200, Description: "Revoked, or signed out when it was the caller's own current session"},
				{Status: 404, Description: "No such session for that user", Schema: ProblemRef()},
			},
		},

		// ----------------------------------------------------- impersonation --
		"POST /api/v1/staff/users/{id}/impersonate": {
			Summary: "Start impersonating a customer",
			Description: "Requires `staff:manage`. Issues a real session for the target user with `impersonated_by` set to the " +
				"operator, and sets it as the session cookie. Because it is an ordinary session, every existing query scopes " +
				"to the target automatically: the operator can do exactly what the customer can, and nothing more. A staff " +
				"account cannot be impersonated, and neither can a disabled one. Both principals are audited, and the " +
				"response carries `impersonating: true` so the banner can be raised before the first request.",
			Responses: []Response{
				{Status: 201, Description: "The impersonated session"},
				{Status: 400, Description: "Self-impersonation, or the request used an API key", Schema: ProblemRef()},
				{Status: 403, Description: "The target is a staff account", Schema: ProblemRef()},
				{Status: 404, Description: "No such user", Schema: ProblemRef()},
				{Status: 409, Description: "The target account is disabled", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/portal/impersonation/stop": {
			Summary: "Stop impersonating and return to your own session",
			Description: "Ends the borrowed session and rotates the operator's own session token back into the cookie. " +
				"Rotation is deliberate: the impersonated token was in a browser that may have been shared, and a token " +
				"that outlives the impersonation is the one thing that must not happen. If the operator's own session " +
				"ended meanwhile, the cookie is cleared and the answer is 401 rather than a session that looks alive.",
			Responses: []Response{
				{Status: 200, Description: "Back to the operator's own identity"},
				{Status: 400, Description: "This session is not an impersonation", Schema: ProblemRef()},
				{Status: 401, Description: "The operator's own session has ended; sign in again", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- roles --
		"GET /api/v1/staff/roles": {
			Summary: "Roles and their permission sets",
			Description: "System roles first, then custom roles by name. A system role's permissions are reported from the code " +
				"map, not from the table, so a hand-edited row cannot widen what the code grants.",
			Responses: []Response{{Status: 200, Description: "The roles", Schema: PageSchema(role)}},
		},
		"GET /api/v1/staff/permissions": {
			Summary: "The declared permission set and the current matrix",
			Description: "Every permission this build declares, the current set per role, and which role names are system " +
				"roles. Served so the matrix screen cannot offer a permission the server would refuse.",
			Responses: []Response{{Status: 200, Description: "The matrix"}},
		},
		"POST /api/v1/staff/roles": {
			Summary: "Create a custom role",
			Description: "Requires `staff:manage`. The name is lower-case, 2–40 characters, and may not be one of the four " +
				"reserved role names. Every permission must be one this build declares: a role holding a permission no " +
				"handler checks would look wider than it is.",
			Responses: []Response{
				{Status: 201, Description: "The role"},
				{Status: 400, Description: "Invalid name, or an undeclared permission", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/roles/{id}/permissions": {
			Summary: "Replace a custom role's permissions",
			Description: "The submitted set replaces the stored one, because the screen is a matrix and a server-computed diff " +
				"between two racing submits is wrong in a way nobody can see. A system role is refused with 409.",
			Responses: []Response{
				{Status: 200, Description: "The updated role"},
				{Status: 400, Description: "An undeclared permission", Schema: ProblemRef()},
				{Status: 404, Description: "No such role", Schema: ProblemRef()},
				{Status: 409, Description: "A system role cannot be changed", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/roles/{id}": {
			Summary: "Delete a custom role",
			Description: "Refused for a system role (409 `system_role`) and for a role somebody still holds (409 `role_in_use`). " +
				"A cascade would silently strip that user's access, which is an incident, not a convenience.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 404, Description: "No such role", Schema: ProblemRef()},
				{Status: 409, Description: "A system role, or a role that is still assigned", Schema: ProblemRef()},
			},
		},

		// --------------------------------------------------- api key allowlist --
		"GET /api/v1/staff/api-keys/{id}/ip-allowlist": {
			Summary: "Read a key's address allowlist",
			Description: "Returns the CIDRs the key may be used from. An empty list means unrestricted, which is what every " +
				"key minted before this feature has.",
			Responses: []Response{
				{Status: 200, Description: "The allowlist"},
				{Status: 404, Description: "No such key for this user", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/api-keys/{id}/ip-allowlist": {
			Summary: "Restrict a key to a set of networks",
			Description: "CIDRs only; host bits are masked off and duplicates dropped. An empty list removes the restriction. " +
				"The caller's address is taken from the same trusted-proxy logic the rest of the server uses, so the check " +
				"is enforceable behind a load balancer and cannot be spoofed by a header.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"ip_allowlist"},
				"properties": map[string]any{
					"ip_allowlist": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string", "description": "A CIDR, e.g. 203.0.113.0/24 or 2001:db8::/32."},
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The saved allowlist"},
				{Status: 400, Description: "An entry is not a CIDR, or the list is too long", Schema: ProblemRef()},
				{Status: 404, Description: "No such key for this user", Schema: ProblemRef()},
			},
		},
	}
}
