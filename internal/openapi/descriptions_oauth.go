package openapi

// The OAuth2 server and social login descriptions (IDN-07…IDN-11, ADM-15).
//
// Four things are stated in the document rather than left to the reader of the handler,
// because each is a property a client or an auditor has to be able to rely on:
//
//   - **A token cannot exceed its owner's permissions.** The granted scopes are the
//     intersection of the client's allowed scopes, the request and the user's live
//     permissions, resolved again when the token is minted *and* on every request. Removing a
//     permission from a role narrows a token that was issued before the change.
//
//   - **The secret is shown once.** The create and rotate responses carry `secret` and
//     `shown_once`; no other response has a secret field, and there is no endpoint that
//     reveals one again, because only a SHA-256 digest is stored.
//
//   - **The refresh token rotates.** Exchanging it replaces the digest, so a replayed value is
//     `invalid_grant` rather than a second token. Revoking the grant kills the family.
//
//   - **Social login never merges accounts.** A callback whose provider email matches an
//     existing account is refused with `account_exists`; the person signs in with their
//     password and links the provider, which is the only path that creates a link.
//
// The token endpoint is the one place that answers both `application/problem+json` (the
// panel's envelope) and RFC 6749's `error` member, with the same stable string in each, so a
// standard OAuth2 client and the panel's own client can both read it.

func init() { RegisterDescriptions(oauthOperations) }

// oauthClientSchema is a registered client. It has no secret field, by construction.
func oauthClientSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "client_id", "name", "confidential", "has_secret", "redirect_uris", "allowed_scopes", "enabled"},
		"properties": map[string]any{
			"id":        map[string]any{"type": "string", "format": "uuid"},
			"client_id": map[string]any{"type": "string", "description": "Public identifier used in the authorization request."},
			"name":      map[string]any{"type": "string"},
			"confidential": map[string]any{"type": "boolean",
				"description": "True when the client holds a secret. A public client authenticates with PKCE alone."},
			"has_secret": map[string]any{"type": "boolean",
				"description": "Whether a secret is configured. The value itself is never returned."},
			"redirect_uris": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "format": "uri"},
				"description": "Exact URIs. There is no prefix match and no wildcard.",
			},
			"allowed_scopes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The ceiling of permissions this client may ever request.",
			},
			"enabled":           map[string]any{"type": "boolean"},
			"created_at":        map[string]any{"type": "string", "format": "date-time"},
			"secret_rotated_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
		},
	}
}

// createdClientSchema is the one response that carries the secret.
func createdClientSchema() map[string]any {
	schema := oauthClientSchema()
	properties := schema["properties"].(map[string]any)
	properties["secret"] = map[string]any{
		"type": "string",
		"description": "The credential, returned exactly once. Only a SHA-256 digest is stored, so " +
			"there is no endpoint that reveals it again.",
	}
	properties["shown_once"] = map[string]any{
		"type":        "boolean",
		"description": "Always true when `secret` is present. A UI must not omit the warning.",
	}
	return schema
}

// socialProviderSchema is a configured provider, with the secret redacted to a flag.
func socialProviderSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "slug", "name", "issuer_url", "client_id", "has_secret", "scopes", "enabled"},
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "format": "uuid"},
			"slug": map[string]any{"type": "string", "description": "Used in `/oauth/social/{slug}/start`."},
			"name": map[string]any{"type": "string"},
			"issuer_url": map[string]any{"type": "string", "format": "uri",
				"description": "The OIDC issuer. When the endpoints are omitted they are discovered from `<issuer>/.well-known/openid-configuration`."},
			"authorization_endpoint": map[string]any{"type": "string", "format": "uri"},
			"token_endpoint":         map[string]any{"type": "string", "format": "uri"},
			"userinfo_endpoint":      map[string]any{"type": "string", "format": "uri"},
			"client_id":              map[string]any{"type": "string"},
			"has_secret": map[string]any{"type": "boolean",
				"description": "Whether a client secret is configured. It is never returned."},
			"scopes": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"subject_claim": map[string]any{"type": "string",
				"description": "Which userinfo claim is the subject. A dotted path is allowed, e.g. `user.id`."},
			"email_claim":          map[string]any{"type": "string"},
			"email_verified_claim": map[string]any{"type": "string", "nullable": true},
			"enabled":              map[string]any{"type": "boolean"},
			"created_at":           map[string]any{"type": "string", "format": "date-time"},
			"updated_at":           map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

func oauthOperations() map[string]Operation {
	client := oauthClientSchema()
	provider := socialProviderSchema()
	return map[string]Operation{
		// ------------------------------------------------------- authorization --
		"GET /oauth/authorize": {
			Summary: "Start the authorization-code flow",
			Description: "The browser destination a third party redirects a person to. With no session the person is " +
				"sent to `/login?next=…` and brought back to exactly this URL. The client and `redirect_uri` are " +
				"validated **before** anything else, because every later error is reported by redirecting: an " +
				"unvalidated redirect is how a code is delivered to an attacker. `code_challenge_method` must be " +
				"`S256`, and a public client must present a challenge. When the person has already consented to " +
				"these scopes for this client the code is issued immediately; otherwise a small script-free consent " +
				"page is rendered.",
			Responses: []Response{
				{Status: 302, Description: "Redirect to the client with `code` and `state`, or to `/login`"},
				{Status: 200, Description: "The consent screen (HTML)"},
				{Status: 400, Description: "The client or redirect URI could not be validated", Schema: ProblemRef()},
			},
		},
		"POST /oauth/authorize": {
			Summary: "Answer the consent screen",
			Description: "Form-encoded. Carries the original request's parameters back plus `decision` (`approve` or " +
				"`deny`) and the session's CSRF token as a hidden field, because this is a plain HTML form rather " +
				"than a scripted request. A refusal redirects with `error=access_denied`.",
			Responses: []Response{
				{Status: 302, Description: "Redirect to the client with `code`, or with `error=access_denied`"},
				{Status: 403, Description: "The CSRF token is missing or invalid", Schema: ProblemRef()},
				{Status: 401, Description: "No session", Schema: ProblemRef()},
			},
		},

		// -------------------------------------------------------- token endpoint --
		"POST /oauth/token": {
			Summary: "Exchange a code or a refresh token for a token pair",
			Description: "`application/x-www-form-urlencoded`, client-authenticated with HTTP Basic or form credentials. " +
				"Grant types: `authorization_code` (with `code_verifier` for a public client) and `refresh_token`. " +
				"The response carries `access_token`, `token_type`, `expires_in`, `refresh_token` and `scope`, and is " +
				"`Cache-Control: no-store`. A failure is `application/problem+json` whose `error` member is the same " +
				"stable string as `code`, so both a standard OAuth2 client and this panel's client can read it. The " +
				"refresh token rotates on every use, so a replayed one is `invalid_grant`.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"grant_type"},
				"properties": map[string]any{
					"grant_type":    map[string]any{"type": "string", "enum": []string{"authorization_code", "refresh_token"}},
					"code":          map[string]any{"type": "string"},
					"redirect_uri":  map[string]any{"type": "string"},
					"code_verifier": map[string]any{"type": "string", "description": "43–128 characters, RFC 7636."},
					"refresh_token": map[string]any{"type": "string"},
					"scope":         map[string]any{"type": "string", "description": "Optional on a refresh; may only narrow."},
					"client_id":     map[string]any{"type": "string"},
					"client_secret": map[string]any{"type": "string"},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "A token pair"},
				{Status: 400, Description: "`invalid_request`, `invalid_grant`, `invalid_scope` or `unsupported_grant_type`", Schema: ProblemRef()},
				{Status: 401, Description: "`invalid_client`", Schema: ProblemRef()},
				{Status: 429, Description: "Too many token requests from this address", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- the API --
		"GET /api/v1/me": {
			Summary: "The account behind the credential, and what it may do",
			Description: "The first call a third party makes. It reports the account and the credential kind: a session, " +
				"an API key, or an OAuth token with the client it belongs to and the scopes it was narrowed to. " +
				"`permissions` is resolved from the account's roles *now*, so a client can see that its token has " +
				"been narrowed rather than discovering it through a 403.",
			Responses: []Response{
				{Status: 200, Description: "The identity and its effective permissions"},
				{Status: 401, Description: "No credential", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/me/invoices": {
			Summary: "The caller's invoices",
			Description: "The documented read a third party actually makes: the same portal query a session uses, behind " +
				"the same `invoice:read:self` permission, reached with an OAuth token. The customer is resolved from " +
				"the credential, never from the request, so another customer's invoices are not addressable at all.",
			Responses: []Response{
				{Status: 200, Description: "The caller's invoices"},
				{Status: 403, Description: "The token does not carry `invoice:read:self`, or the owner no longer holds it", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------- client registry --
		"GET /api/v1/staff/oauth-clients": {
			Summary:     "List registered OAuth2 clients",
			Description: "Never contains a secret. `has_secret` says whether one is configured, which is the only thing a screen needs to show.",
			Responses:   []Response{{Status: 200, Description: "The clients"}},
		},
		"GET /api/v1/staff/oauth-clients/{id}": {
			Summary:     "Read one OAuth2 client",
			Description: "The registered redirect URIs, the allowed scopes and whether a secret is configured.",
			Responses: []Response{
				{Status: 200, Description: "The client", Schema: client},
				{Status: 404, Description: "No such client", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/oauth-clients": {
			Summary: "Register an OAuth2 client",
			Description: "Redirect URIs must be absolute, http(s), fragment-free and exact; a wildcard is refused. The " +
				"allowed scopes must be permission names this build declares. A confidential client's secret is " +
				"returned in this response and nowhere else, with `shown_once: true`; a public client has no secret " +
				"at all and must use PKCE.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"name", "redirect_uris", "allowed_scopes"},
				"properties": map[string]any{
					"name":           map[string]any{"type": "string"},
					"redirect_uris":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "format": "uri"}},
					"allowed_scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"confidential": map[string]any{"type": "boolean",
						"description": "Defaults to false. A client that can keep a secret has to say so."},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The client, plus its secret exactly once", Schema: createdClientSchema()},
				{Status: 400, Description: "`invalid_client_request`", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/oauth-clients/{id}/rotate-secret": {
			Summary: "Rotate a client secret",
			Description: "Replaces the secret and returns the new one once. Outstanding tokens are not revoked: the secret " +
				"authenticates the client at the token endpoint, while an issued token is the user's grant. Revoke the " +
				"grant or delete the client for that.",
			Responses: []Response{
				{Status: 200, Description: "The client, plus the new secret exactly once", Schema: createdClientSchema()},
				{Status: 404, Description: "No such client", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/oauth-clients/{id}/secret": {
			Summary: "Disable a client secret",
			Description: "Destroys the secret and turns the client into a public one, which then has to use PKCE. The " +
				"credential is removed rather than kept-but-ignored: a secret that exists but is not checked is the " +
				"worst of both worlds.",
			Responses: []Response{
				{Status: 200, Description: "The client, now public", Schema: client},
				{Status: 404, Description: "No such client", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/oauth-clients/{id}": {
			Summary:     "Delete a client",
			Description: "Every code, grant, token and consent it owns is deleted with it. The audit row is the only trace that survives.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 404, Description: "No such client", Schema: ProblemRef()},
			},
		},

		// ------------------------------------------------------------- social login --
		"GET /api/v1/auth/providers": {
			Summary: "The providers the sign-in screen may offer",
			Description: "Public, because the sign-in screen is public. Only enabled providers are listed, so a half-configured " +
				"provider is not a broken button; with none configured the list is empty and the screen renders no " +
				"buttons at all.",
			Responses: []Response{{Status: 200, Description: "`{data: [{slug, name, start_url, link_url}]}`"}},
		},
		"GET /oauth/social/{slug}/start": {
			Summary: "Redirect to a social provider",
			Description: "Issues a signed, single-use `state` (HMAC-SHA256, key in the database, nonce consumed by a row) and " +
				"redirects to the provider's authorization endpoint. `?link=1` switches to the link flow, which needs a " +
				"session and is recorded inside the signed state so a callback cannot claim to be a link when it was " +
				"started as a sign-in.",
			Responses: []Response{
				{Status: 302, Description: "Redirect to the provider"},
				{Status: 404, Description: "No such provider, or it is disabled", Schema: ProblemRef()},
			},
		},
		"GET /oauth/social/{slug}/callback": {
			Summary: "Complete a social login or link",
			Description: "Verifies and burns the state, exchanges the code at the provider, then resolves the identity. An " +
				"existing link signs that account in — unless it has MFA enrolled, which a social sign-in would skip. " +
				"An email that matches an existing account is **refused** with `account_exists`: the person is sent to " +
				"the sign-in screen to sign in and link. Nothing is ever created or merged by this endpoint.",
			Responses: []Response{
				{Status: 302, Description: "A session and a redirect to `/`, or a redirect to `/login?social_error=…`"},
				{Status: 400, Description: "The provider returned no code", Schema: ProblemRef()},
				{Status: 404, Description: "No such provider", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/oauth-providers": {
			Summary:     "List social providers",
			Description: "The client secret is never returned; `has_secret` says whether one is configured.",
			Responses:   []Response{{Status: 200, Description: "The providers"}},
		},
		"GET /api/v1/staff/oauth-providers/{id}": {
			Summary:     "Read one social provider",
			Description: "The endpoints, scopes and claim mapping, with the secret redacted to `has_secret`.",
			Responses: []Response{
				{Status: 200, Description: "The provider", Schema: provider},
				{Status: 404, Description: "No such provider", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/oauth-providers": {
			Summary: "Register a social provider",
			Description: "Google, GitHub and Discord are rows here, not implementations: the endpoints, the scopes and which " +
				"claim carries the subject and the email are configuration. When the three endpoints are omitted they " +
				"are discovered from `<issuer_url>/.well-known/openid-configuration`, so for a provider that publishes " +
				"discovery the issuer URL is the whole configuration. A provider is created disabled.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"slug", "issuer_url", "client_id", "client_secret"},
				"properties": map[string]any{
					"slug":                   map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9_-]{1,31}$"},
					"name":                   map[string]any{"type": "string"},
					"issuer_url":             map[string]any{"type": "string", "format": "uri"},
					"authorization_endpoint": map[string]any{"type": "string", "format": "uri"},
					"token_endpoint":         map[string]any{"type": "string", "format": "uri"},
					"userinfo_endpoint":      map[string]any{"type": "string", "format": "uri"},
					"client_id":              map[string]any{"type": "string"},
					"client_secret":          map[string]any{"type": "string", "format": "password"},
					"scopes":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"subject_claim":          map[string]any{"type": "string", "description": "Defaults to `sub`. A dotted path is allowed."},
					"email_claim":            map[string]any{"type": "string", "description": "Defaults to `email`."},
					"email_verified_claim": map[string]any{"type": "string",
						"description": "When set, linking requires the claim to be true."},
					"enabled": map[string]any{"type": "boolean"},
				},
			},
			Responses: []Response{
				{Status: 201, Description: "The provider", Schema: provider},
				{Status: 400, Description: "`invalid_provider`", Schema: ProblemRef()},
				{Status: 502, Description: "`provider_unreachable`: OIDC discovery failed", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/oauth-providers/{id}": {
			Summary:     "Update a social provider",
			Description: "A full replacement, except that an empty `client_secret` keeps the stored one — submitting the edit form unchanged must not wipe a credential.",
			Responses: []Response{
				{Status: 200, Description: "The provider", Schema: provider},
				{Status: 400, Description: "`invalid_provider`", Schema: ProblemRef()},
				{Status: 404, Description: "No such provider", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/oauth-providers/{id}": {
			Summary:     "Delete a social provider",
			Description: "Every identity linked through it is deleted with it, so a sign-in method that is removed really stops existing.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 404, Description: "No such provider", Schema: ProblemRef()},
			},
		},

		// -------------------------------------------------------- customer links --
		"GET /api/v1/portal/social-links": {
			Summary:     "The provider identities linked to the caller's account",
			Description: "Scoped to the session in SQL: another customer's links are not addressable. The subject is the provider's opaque id, included so a support report can be matched to a row.",
			Responses:   []Response{{Status: 200, Description: "The caller's links"}},
		},
		"DELETE /api/v1/portal/social-links/{id}": {
			Summary:     "Unlink a provider identity",
			Description: "Ownership is part of the `WHERE` clause, so somebody else's link is indistinguishable from one that does not exist and answers 404.",
			Responses: []Response{
				{Status: 204, Description: "Unlinked"},
				{Status: 404, Description: "No such link", Schema: ProblemRef()},
			},
		},
	}
}
