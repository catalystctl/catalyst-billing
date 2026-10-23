package openapi

// The tenant's brand overlay (E6.10).
//
// The prose here exists to stop two misreadings that the route names invite:
//
//   - `/api/v1/branding` is *public*. It is the tenant's name, logo and accent — the same
//     facts a visitor already sees on the storefront page they are standing on — and it is
//     public because the panel needs it before the first paint and the storefront needs it
//     with no session at all. It is not a settings read and it exposes no setting it was not
//     asked for.
//   - `/api/v1/staff/branding` is a *settings group*, not a theme registry. There is no route
//     that lists themes, selects one per user, or stores a token set, because that is a much
//     larger feature and not what a tenant needs in order to look like themselves. A reader
//     looking for the registry should stop here and read the note on the group read.

func init() { RegisterDescriptions(func() map[string]Operation { return brandingOperations }) }

// brandResolvedSchema is the resolved brand: stored values plus the derived colour facts.
func brandResolvedSchema() map[string]any {
	modeDerived := func(name string) map[string]any {
		return map[string]any{
			"type":     "object",
			"required": []string{"ink", "text", "contrast", "legible", "set"},
			"properties": map[string]any{
				"ink": map[string]any{
					"type": "string",
					"description": "The colour text takes on top of the accent, as `#rrggbb`. When no accent " +
						"is configured it is the built-in " + name + " theme's ink, so a client can use it " +
						"unconditionally.",
				},
				"text": map[string]any{
					"type": "string",
					"description": "The accent adjusted until it reaches WCAG AA against the " + name + " theme's " +
						"background, as `#rrggbb`. Empty when no accent is configured. This is what a link or a " +
						"small label should use when the raw accent would be too faint — a pale brand colour is a " +
						"legitimate choice and an illegible label.",
				},
				"contrast": map[string]any{
					"type":        "number",
					"description": "The raw accent's WCAG contrast ratio against the " + name + " background, 1 to 21. `0` when unset.",
				},
				"legible": map[string]any{
					"type":        "boolean",
					"description": "Whether that ratio reaches 4.5:1. `true` when unset, because the built-in accent does.",
				},
				"set": map[string]any{
					"type": "boolean",
					"description": "Whether an accent is configured for this mode. A client uses this to decide whether " +
						"to emit an override at all: emitting the built-in colour would make \"unset\" indistinguishable " +
						"from \"set to the default\", and the byte-identical fallback could not be proven.",
				},
			},
		}
	}
	return map[string]any{
		"type":     "object",
		"required": []string{"name", "accent_light", "accent_dark", "logo_id", "logo_url", "resolved"},
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The tenant's display name, or the empty string when not configured.",
			},
			"accent_light": map[string]any{
				"type":        "string",
				"description": "The light-mode accent as `#rrggbb`, or the empty string.",
			},
			"accent_dark": map[string]any{
				"type":        "string",
				"description": "The dark-mode accent as `#rrggbb`, or the empty string.",
			},
			"logo_id": map[string]any{
				"type":        "string",
				"description": "The stored image id, or the empty string.",
			},
			"logo_url": map[string]any{
				"type":        "string",
				"description": "`/api/v1/media/{id}`, or the empty string. Derived from `logo_id`.",
			},
			"resolved": map[string]any{
				"type":     "object",
				"required": []string{"light", "dark"},
				"properties": map[string]any{
					"light": modeDerived("light"),
					"dark":  modeDerived("dark"),
				},
			},
			"updated_at": map[string]any{
				"type":        []any{"string", "null"},
				"format":      "date-time",
				"description": "When the group was last written, or null on a fresh installation.",
			},
		},
	}
}

var brandingOperations = map[string]Operation{
	"GET /api/v1/branding": {
		Summary: "The tenant's brand, resolved",
		Description: "Public, because the panel needs the brand before it paints and the storefront needs it with " +
			"no session at all. It exposes a display name, two accents and a media URL — the facts a visitor already " +
			"sees on the page they are standing on — and never a setting it was not asked for: the shape is built " +
			"from four named keys, not from a settings group dump.\n\n" +
			"The accents are *not* resolved to one colour. The panel decides which theme it is in before it paints, " +
			"so both are served and the client picks; resolving server-side would flash the built-in colour on the " +
			"first frame. `resolved` carries the derived ink and contrast for each mode, computed with the same " +
			"arithmetic the invoice renderer uses, so the panel and the document cannot disagree about whether a " +
			"colour is legible.\n\n" +
			"Every field is present whether or not it is configured, and an unset field is the empty string — not " +
			"the built-in value. That is what makes the fallback expressible: a client compares against `\"\"` to " +
			"decide whether to emit an override, so an installation that never opened the branding screen renders " +
			"exactly as it did before the screen existed.",
		Responses: []Response{
			{Status: 200, Description: "The resolved brand", Schema: brandResolvedSchema()},
			{Status: 500, Description: "The brand could not be read", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/branding": {
		Summary: "The branding settings group, with the resolved brand beside it",
		Description: "The same resolved shape as the public read, plus the settings group as the settings API renders " +
			"it and the limits the server enforces. One request, so the screen cannot render a form whose values " +
			"disagree with the resolved brand beside it.\n\n" +
			"**This is not a theme registry.** There is one brand per installation — a name, a logo and one accent per " +
			"colour mode — layered on top of the two colour themes the panel already has. There is no route that " +
			"lists themes, selects one per user, or stores a token set; that is a much larger feature with its own " +
			"schema (every design token becomes a row or a column), its own migration story (a new token is a " +
			"migration per theme) and its own failure mode (a theme missing a token renders an unstyled control).",
		Responses: []Response{
			{Status: 200, Description: "The brand and its settings group", Schema: map[string]any{
				"type":     "object",
				"required": []string{"brand", "group", "limits"},
				"properties": map[string]any{
					"brand": brandResolvedSchema(),
					"group": map[string]any{
						"type":        "object",
						"description": "The `branding` settings group, in the same shape `GET /api/v1/staff/settings?group=branding` returns.",
					},
					"limits": map[string]any{
						"type":     "object",
						"required": []string{"name_max", "logo_max_bytes", "logo_types", "min_text_contrast"},
						"properties": map[string]any{
							"name_max":          map[string]any{"type": "integer", "description": "Characters."},
							"logo_max_bytes":    map[string]any{"type": "integer"},
							"logo_types":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"min_text_contrast": map[string]any{"type": "number", "description": "WCAG AA for body text, 4.5."},
						},
					},
				},
			}},
			{Status: 403, Description: "The caller does not hold settings:write", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},
	"PUT /api/v1/staff/branding": {
		Summary: "Write the branding group",
		Description: "One group write, validated as a whole before anything is stored: a form that sends a good name " +
			"and an invalid colour stores neither, and a form that fails cannot leave a half-applied brand visible " +
			"to the next reader. Every write is audited inside the same transaction, one row per changed key.\n\n" +
			"A colour is normalised before it is validated — the leading `#` is optional and the three-digit form is " +
			"expanded — so there is exactly one spelling of a colour in the store and the panel's comparison " +
			"against its own token cannot be fooled by case. The response is the committed state, not an echo of " +
			"the request.\n\n" +
			"An empty string means \"not configured\" and restores the built-in appearance. `logo_id` is **not** " +
			"writable here: the logo is set by uploading it and cleared by deleting it, so a form cannot point the " +
			"brand at an arbitrary image by typing an id.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"values"},
			"properties": map[string]any{
				"values": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"maxLength":   80,
							"description": "The display name. Trimmed; control characters are refused.",
						},
						"accent_light": map[string]any{
							"type":        "string",
							"description": "A hex colour (`#3b5bdb`, `3b5bdb` or `#abc`), or empty to clear.",
						},
						"accent_dark": map[string]any{
							"type":        "string",
							"description": "A hex colour, or empty to clear.",
						},
					},
					"additionalProperties": false,
				},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The committed brand", Schema: map[string]any{
				"type":     "object",
				"required": []string{"brand", "group"},
				"properties": map[string]any{
					"brand": brandResolvedSchema(),
					"group": map[string]any{"type": "object"},
				},
			}},
			{Status: 400, Description: "`values` is absent, or `logo_id` was sent", Schema: ProblemRef()},
			{Status: 403, Description: "The caller does not hold settings:write", Schema: ProblemRef()},
			{Status: 422, Description: "A value is not a hex colour, or the name is too long or contains a control character", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/branding/logo": {
		Summary: "Upload the brand logo",
		Description: "A raw body with the content type in the header — one fewer parser in the request path, and the " +
			"panel has a `File` in hand already. The bytes are stored through the catalog's image store (deduped by " +
			"digest, capped at 256 KiB) and the brand is pointed at the id **in the same transaction**: a failure " +
			"between the two writes would leave a screen showing a logo id that names nothing.\n\n" +
			"PNG and JPEG only. The catalog also accepts WebP and SVG for product logos, and both are wrong here: " +
			"the invoice renderer can draw neither, so a WebP logo would render in the panel and vanish from every " +
			"document, and an SVG is a document that can carry a script. A brand that is half-applied is worse than " +
			"one that is refused with a reason, so the refusal names the formats an invoice can print.\n\n" +
			"The body is decoded before it is stored: a file with a PNG content type and no PNG inside it would " +
			"otherwise be accepted and then be invisible on every document.",
		Responses: []Response{
			{Status: 201, Description: "The stored image and the brand it was attached to", Schema: map[string]any{
				"type":     "object",
				"required": []string{"id", "content_type", "byte_size", "url", "brand"},
				"properties": map[string]any{
					"id":           map[string]any{"type": "string"},
					"content_type": map[string]any{"type": "string"},
					"byte_size":    map[string]any{"type": "integer"},
					"url":          map[string]any{"type": "string"},
					"brand":        brandResolvedSchema(),
				},
			}},
			{Status: 400, Description: "The request carries no Content-Type, or the body could not be read", Schema: ProblemRef()},
			{Status: 403, Description: "The caller does not hold settings:write", Schema: ProblemRef()},
			{Status: 413, Description: "The image exceeds 256 KiB", Schema: ProblemRef()},
			{Status: 415, Description: "The content type is not PNG or JPEG", Schema: ProblemRef()},
			{Status: 422, Description: "The body is not an image this server can read", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},
	"DELETE /api/v1/staff/branding/logo": {
		Summary: "Remove the brand logo",
		Description: "Clears the reference. The stored image row is deliberately kept: it is deduped by digest and " +
			"shared with the catalog, so deleting it could break a product logo that happens to be the same file, and " +
			"\"remove the logo from the brand\" is not \"destroy the image\".\n\n" +
			"This is a route of its own rather than \"save the form with an empty field\", because the two are " +
			"different intentions: clearing a text field is often a slip, and a logo that vanishes on a slipped save " +
			"is worse than one that takes a click to remove.",
		Responses: []Response{
			{Status: 200, Description: "The committed brand", Schema: map[string]any{
				"type":     "object",
				"required": []string{"brand", "group"},
				"properties": map[string]any{
					"brand": brandResolvedSchema(),
					"group": map[string]any{"type": "object"},
				},
			}},
			{Status: 403, Description: "The caller does not hold settings:write", Schema: ProblemRef()},
			{Status: 503, Description: "No database is configured", Schema: ProblemRef()},
		},
	},
}
