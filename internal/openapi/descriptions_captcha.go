package openapi

// The captcha description (IDN-18).
//
// One public route, and the interesting part is what it does *not* say: the response carries
// the site key and the provider, never the secret key. A site key is public by design — it is
// in the page the visitor loads — while the secret key is the credential that turns a token
// into a verdict, and a settings read that echoed it back would put it in every proxy log
// between the panel and the operator.
//
// The document also cannot express the property that matters most about this feature: when
// no provider is configured, registration and password reset make no outbound call and behave
// exactly as they did before. That is proven by tests, not by a schema.

func init() { RegisterDescriptions(captchaOperations) }

// captchaConfigSchema is the browser-facing half of the challenge configuration.
func captchaConfigSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"provider", "site_key", "enabled"},
		"properties": map[string]any{
			"provider": map[string]any{
				"type": "string",
				"enum": []string{"none", "turnstile", "recaptcha", "hcaptcha"},
				"description": "The widget to render. `none` means no challenge is configured and the " +
					"form must not render one.",
			},
			"site_key": map[string]any{
				"type": "string",
				"description": "The provider's public key, used to initialise the widget. Public by " +
					"design; the secret key is never in a response.",
			},
			"enabled": map[string]any{
				"type": "boolean",
				"description": "Whether a widget can be rendered at all (a provider *and* a site key). " +
					"`false` means the form submits without a token.",
			},
		},
	}
}

// captchaOperations describes the routes this domain serves.
func captchaOperations() map[string]Operation {
	return map[string]Operation{
		"GET /api/v1/auth/captcha": {
			Summary: "The human-challenge configuration for this installation",
			Description: "Public, because the registration, sign-in and password-reset screens render before " +
				"anyone has a session. It answers `{\"provider\": \"none\", \"site_key\": \"\", " +
				"\"enabled\": false}` when no challenge is configured, so a page renders no widget " +
				"rather than guessing.\n\n" +
				"A configured provider does not replace the per-source rate limit on the endpoints " +
				"that consume a token: the challenge is a second control, and a solved challenge is " +
				"still bounded by the credential budget.\n\n" +
				"The `secret_key` setting is not part of this shape and is redacted on every read " +
				"path. 503 means the configuration could not be read; a client renders no widget and " +
				"the server still refuses an unverified request.",
			Responses: []Response{
				{Status: 200, Description: "The provider and its public site key", Schema: captchaConfigSchema()},
				{Status: 503, Description: "The challenge configuration could not be read", Schema: ProblemRef()},
			},
		},
	}
}
