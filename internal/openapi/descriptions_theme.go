package openapi

// The theme registry (docs/theming-system.md).
//
// # The distinction this file exists to make
//
// `/api/v1/branding` and `/api/v1/staff/branding` are the tenant's *identity*: a name, a logo and one
// accent per colour mode. They stay exactly as they were, and the note on `GET /api/v1/staff/branding`
// says so.
//
// Everything here is *presentation*: a versioned document of design tokens, per scope, with drafts,
// immutable revisions, a publish step and a history. The two are layered at runtime — the theme
// generates the stylesheet, the brand overlay may then set the accent family — and neither is a copy
// of the other. A reader looking for "how do I change my logo" wants branding; a reader looking for
// "how do I change the radius on every card" wants this.
//
// # Why the public surface is public
//
// A resolved palette is not a secret: it is the colour of the page the caller is already looking at.
// The storefront manifest has to answer with no session at all, because the first paint of a public
// shop needs it. The panel and portal manifests do require a session, which is a small but real
// distinction: they describe a signed-in surface, and there is no reason to tell an anonymous caller
// what the staff panel looks like.
//
// # Why the theme resources are separate files
//
// The panel's CSP is `script-src 'self'; style-src 'self'` with no `'unsafe-inline'`. An inline
// `<style>` or `<script>` would need a nonce or a hash — a per-response token carried through the
// whole shell — so the theme is served as two ordinary files with ordinary content types. They are
// revision-addressed, so they are cached `immutable` for a year and a publish is visible on the next
// navigation with no cache purge.

func init() { RegisterDescriptions(func() map[string]Operation { return themeOperations }) }

// themeDocumentSchema describes the document. It is deliberately shallow: the full token contract is
// in docs/theming-system.md §7, and an OpenAPI schema that restated every token would be a third copy
// of it (the document, the validator, and this) that could disagree with both.
func themeDocumentSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"schema", "id", "name"},
		"description": "A theme document. Every field beyond `schema`, `id` and `name` is optional and inherits " +
			"when omitted, which is what lets a preset be forty lines instead of four hundred and what lets a " +
			"scope override one token without restating the rest. The token contract is documented in " +
			"docs/theming-system.md §7.",
		"properties": map[string]any{
			"schema": map[string]any{
				"type":        "integer",
				"description": "The document schema. A document declaring a schema this build does not read is refused rather than half-applied.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "The theme slug: lower-case letters, digits and hyphens. It is part of the import/export contract.",
			},
			"name":        map[string]any{"type": "string", "description": "The display name, at most 80 characters."},
			"description": map[string]any{"type": "string", "description": "One line of prose for the gallery card."},
			"extends": map[string]any{
				"type":        "string",
				"description": "A preset id or parent theme id this document starts from. Resolved at draft time, never at render time.",
			},
			"inherit": map[string]any{
				"type": "boolean",
				"description": "`false` means \"start from the built-in default, not from my parent scope\". Absent means inherit, " +
					"which is the default because it is what a portal or storefront theme usually wants.",
			},
			"color": map[string]any{
				"type":        "object",
				"description": "The palette: one brand family, five signal colours and one surface set per colour mode. Per mode because a panel and a piece of paper are different colour spaces.",
			},
			"typography": map[string]any{
				"type":        "object",
				"description": "Three families, the eight-step type scale, leading and tracking.",
			},
			"space": map[string]any{
				"type":        "object",
				"description": "The base grid, the density and the eight-step spacing ladder.",
			},
			"shape": map[string]any{
				"type":        "object",
				"description": "The corner scale and the border width. Bounded to 64px-equivalent, because a 640px radius is a circle rather than a corner.",
			},
			"elevation": map[string]any{
				"type":        "object",
				"description": "The shadow scale per colour mode. Per mode because a shadow that reads on #171413 is a bruise on #f5f2ee.",
			},
			"motion": map[string]any{
				"type":        "object",
				"description": "Three durations and one easing.",
			},
			"background": map[string]any{
				"type": "object",
				"description": "The page treatment: flat, gradient, mesh, paper or an uploaded image. `image` is an asset " +
					"reference (`theme-asset:<uuid>`), never a remote URL — a theme that could name an arbitrary origin " +
					"would be a theme that leaks a request to it.",
			},
			"layout": map[string]any{
				"type":        "object",
				"description": "The shell shape: nav variant, sidebar and topbar dimensions, content width, card and table styles. `layout.pages` is reserved for the storefront composer and is refused by this build with a specific error rather than silently ignored.",
			},
			"components": map[string]any{
				"type":        "object",
				"description": "Per-component overrides, from a closed set of names (`button`, `input`, `card`, `table`, `badge`, `tabs`, `modal`, `nav`). A typo such as `buton` is refused rather than ignored.",
			},
			"customCss": map[string]any{
				"type": "string",
				"description": "A validated stylesheet. It is parsed and allowlisted, not filtered: CSS escapes are refused, " +
					"comments are stripped before inspection, only nine at-rules are permitted (`@import` is not one of them), " +
					"and every `url()` must point at a same-origin asset route or a base64 raster data URI. 256 KiB maximum.",
			},
		},
	}
}

// themeRevisionSchema is one immutable revision.
func themeRevisionSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "scope", "number", "status", "digest", "document", "source"},
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "format": "uuid"},
			"scope":  map[string]any{"type": "string", "enum": []string{"panel", "portal", "storefront", "email", "invoice"}},
			"number": map[string]any{"type": "integer", "description": "The revision number within its theme, starting at 1."},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"draft", "published", "archived"},
				"description": "At most one `draft` and at most one `published` revision exist per theme, enforced by partial " +
					"unique indexes. A published revision is immutable: an edit is a new revision, and a rollback is " +
					"republishing an old one.",
			},
			"digest": map[string]any{
				"type":        "string",
				"description": "`sha256:<hex>` of the canonical JSON of the resolved document. It is the cache key, the URL segment of the stylesheet and the idempotency key of an import.",
			},
			"note":     map[string]any{"type": "string", "description": "The operator's reason, shown in the history list."},
			"document": themeDocumentSchema(),
			"source": map[string]any{
				"type": "object",
				"description": "What the operator saved, before resolution. Stored beside the resolved document so that editing a " +
					"child theme starts from the child's own values rather than showing the parent's tokens as if the operator " +
					"had typed them.",
			},
			"created_at":   map[string]any{"type": "string", "format": "date-time"},
			"published_at": map[string]any{"type": []any{"string", "null"}, "format": "date-time"},
			"created_by":   map[string]any{"type": []any{"string", "null"}, "format": "uuid"},
			"published_by": map[string]any{"type": []any{"string", "null"}, "format": "uuid"},
		},
	}
}

// themeManifestSchema is the resolved presentation a client needs.
func themeManifestSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"scope", "revision", "digest", "published", "css", "document", "meta"},
		"properties": map[string]any{
			"scope":    map[string]any{"type": "string"},
			"revision": map[string]any{"type": "string", "description": "The revision segment used in the resource URLs, or `default`."},
			"digest":   map[string]any{"type": "string"},
			"published": map[string]any{
				"type":        "boolean",
				"description": "`false` means no theme is published and the scope is serving the built-in default document, which reproduces the stylesheet the product shipped before the registry existed.",
			},
			"css": map[string]any{
				"type":        "string",
				"description": "A revision-addressed stylesheet URL, cached `immutable` for a year. Link it after the bundled stylesheet: its declarations win on source order at equal specificity.",
			},
			"boot": map[string]any{
				"type": "string",
				"description": "A revision-addressed pre-paint script. A classic (not module) script in `<head>`: it reads the stored " +
					"light/dark choice and sets `data-theme`, which is what stops a returning user seeing the default mode flash " +
					"for one frame.",
			},
			"document": themeDocumentSchema(),
			"meta": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":        map[string]any{"type": "string"},
					"theme_color":  map[string]any{"type": "string", "description": "The value for `<meta name=\"theme-color\">`."},
					"color_scheme": map[string]any{"type": "string", "enum": []string{"dark", "light"}},
					"nav":          map[string]any{"type": "string", "enum": []string{"sidebar", "rail", "topbar"}},
					"density":      map[string]any{"type": "string", "enum": []string{"compact", "comfortable", "roomy"}},
					"card_style":   map[string]any{"type": "string", "enum": []string{"flat", "outlined", "elevated"}},
					"table_style":  map[string]any{"type": "string", "enum": []string{"compact", "comfortable"}},
				},
			},
		},
	}
}

// themeLimitsSchema publishes the bounds the server enforces, so the studio states them instead of
// letting an operator discover them through a refused save.
func themeLimitsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"max_document_bytes":   map[string]any{"type": "integer"},
			"max_custom_css_bytes": map[string]any{"type": "integer"},
			"max_name_length":      map[string]any{"type": "integer"},
			"modes":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"densities":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"navs":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"card_styles":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"table_styles":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"backgrounds":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"components":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"families": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The installed font families. A theme naming a family outside this set is refused, because the browser would silently fall back to its default serif and the theme would look broken in the least diagnosable way.",
			},
			"scopes":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"asset_max_bytes":     map[string]any{"type": "integer"},
			"asset_kinds":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"asset_types":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"preview_ttl_seconds": map[string]any{"type": "integer"},
		},
	}
}

// themePermissionsSchema reports what the caller may do, so the studio hides an action rather than
// letting it fail.
func themePermissionsSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"can_read", "can_write", "can_publish"},
		"properties": map[string]any{
			"can_read":  map[string]any{"type": "boolean", "description": "`theme:read` — the studio, the gallery and the history."},
			"can_write": map[string]any{"type": "boolean", "description": "`theme:write` — drafts, assets and import. A draft is invisible to visitors."},
			"can_publish": map[string]any{
				"type": "boolean",
				"description": "`theme:publish` — make a draft live, roll back, and edit custom CSS. Separate from `theme:write` because " +
					"panel custom CSS can hide staff-critical UI: the operator who may recolour the storefront is not " +
					"necessarily the one who may hide the refund button from every colleague.",
			},
		},
	}
}

// presetMetaSchema is one gallery card.
func presetMetaSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "name", "character", "mode", "radius", "density", "nav", "swatch"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string"},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"character": map[string]any{
				"type":        "string",
				"description": "The short label under the card: what makes this preset structurally different, not what colour it is.",
			},
			"mode":    map[string]any{"type": "string", "enum": []string{"dark", "light", "system"}},
			"radius":  map[string]any{"type": "string", "description": "The `shape.radius.md` value, so the card shows that a preset changes the shape and not only the hue."},
			"density": map[string]any{"type": "string"},
			"nav":     map[string]any{"type": "string"},
			"swatch":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Ground, card, text and accent, in that order."},
		},
	}
}

// themeDraftBodySchema is the create/update payload.
func themeDraftBodySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"preset": map[string]any{
				"type":        "string",
				"description": "Apply an embedded preset as the source document. Resolved at draft time, so a later change to the preset cannot mutate a theme already published.",
			},
			"document": map[string]any{
				"type": "object",
				"description": "An explicit source document. It is a layer: a field it omits is inherited, so a partial edit " +
					"is a legitimate payload.",
			},
			"note": map[string]any{"type": "string", "description": "Why, shown in the history list."},
		},
	}
}

var themeOperations = map[string]Operation{
	// ---------------------------------------------------------------- public --

	"GET /api/v1/theme": {
		Summary: "The theme manifest for the scope the request came from",
		Description: "The scope is derived from the request path, using the same rule the shell uses: `/` and " +
			"`/products*` are the storefront, `/portal*`, `/cart*`, `/checkout*`, `/account*`, `/tickets*` and the " +
			"auth screens are the portal, and everything else is the panel. A client therefore gets the right answer " +
			"without knowing the rule.\n\n" +
			"An anonymous caller is refused for the panel and portal scopes. A resolved palette is not a secret — it is " +
			"the colour of the page the caller is looking at — but those two describe a signed-in surface and there is " +
			"no reason to describe it to a stranger.\n\n" +
			"`published: false` means no theme is published and the scope is serving the built-in default document, which " +
			"reproduces the stylesheet the product shipped before this registry existed. That is a value, not an error: it " +
			"is the state of a fresh installation and of an installation that has never opened the studio.",
		Responses: []Response{
			{Status: 200, Description: "The resolved manifest", Schema: themeManifestSchema()},
			{Status: 304, Description: "The caller's `If-None-Match` matches the current digest"},
			{Status: 401, Description: "The scope describes a signed-in surface", Schema: ProblemRef()},
			{Status: 404, Description: "No such scope", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/theme/{scope}": {
		Summary: "The theme manifest for a named scope",
		Description: "The same manifest as the path-derived read, for a client that already knows which scope it wants — " +
			"the Theme Studio's preview, for instance. The same session rule applies: panel and portal require one.",
		Responses: []Response{
			{Status: 200, Description: "The resolved manifest", Schema: themeManifestSchema()},
			{Status: 304, Description: "The caller's `If-None-Match` matches the current digest"},
			{Status: 401, Description: "The scope describes a signed-in surface", Schema: ProblemRef()},
			{Status: 404, Description: "No such scope", Schema: ProblemRef()},
		},
	},
	"GET /theme/r/{revision}/theme.css": {
		Summary: "A revision's generated stylesheet",
		Description: "The theme compiled to CSS: the canonical token set, the legacy billing aliases (emitted as " +
			"`var(--canonical)` so the two vocabularies cannot disagree), the game panel's HSL aliases where their names " +
			"do not collide with a billing token that already means a hex colour, the shell shape rules, and the " +
			"operator's custom CSS last so it wins on source order.\n\n" +
			"Served as a file rather than an inline `<style>` because the panel's CSP is `style-src 'self'` with no " +
			"`'unsafe-inline'`. Addressed by revision, so it is cached `immutable` for a year: a browser holding a shell " +
			"from before the last publish asks for the revision it expects and gets that revision's bytes rather than a " +
			"404 that would leave the page unstyled.",
		Responses: []Response{
			{Status: 200, Description: "The stylesheet"},
			{Status: 304, Description: "The caller's `If-None-Match` matches"},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
		},
	},
	"GET /theme/r/{revision}/boot.js": {
		Summary: "A revision's pre-paint script",
		Description: "A classic (not module) script, loaded in `<head>`, that sets `data-theme` — and the shell's " +
			"`data-shell`, `data-density`, `data-card-variant` and `data-table-variant` — before the first paint. A module " +
			"would be deferred, and a returning user who chose the other colour mode would see the default mode flash for " +
			"one frame.\n\n" +
			"A file rather than an inline script because `script-src 'self'` forbids inline; widening the policy for a " +
			"colour preference would be the wrong trade.",
		Responses: []Response{
			{Status: 200, Description: "The boot script"},
			{Status: 304, Description: "The caller's `If-None-Match` matches"},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
		},
	},
	"GET /theme/assets/{id}": {
		Summary: "A theme-only asset",
		Description: "Backgrounds, social cards, favicons and self-hosted fonts. Cached `immutable` because the id is a " +
			"uuid for a digest-deduped payload: the bytes behind an id never change.\n\n" +
			"This is not where the tenant logo lives. The logo and the catalog images stay in `product_images` and are " +
			"served by `/api/v1/media/{id}`; copying those bytes here would create two answers to \"what is the logo\". " +
			"This store exists for the asset classes the catalog cannot represent, and it is what a theme document's " +
			"`theme-asset:<uuid>` reference resolves to — a reference a CSS `url()` is only allowed to make, which is " +
			"what stops a custom stylesheet becoming an exfiltration channel.",
		Responses: []Response{
			{Status: 200, Description: "The asset"},
			{Status: 304, Description: "The caller's `If-None-Match` matches"},
			{Status: 404, Description: "No such asset", Schema: ProblemRef()},
		},
	},
	"GET /theme/manifest.webmanifest": {
		Summary: "The themed PWA manifest",
		Description: "Name, icons and colours for an installed instance. The name and the colour come from the tenant's " +
			"brand and the current scope's theme, so an installed panel does not keep the built-in name after an " +
			"operator renames themselves.",
		Responses: []Response{{Status: 200, Description: "The manifest"}},
	},
	"GET /theme/favicon.ico": {
		Summary: "The favicon, redirected to the themed one",
		Description: "A redirect rather than a proxy: the bytes are already served from a stable URL, and re-serving them " +
			"here would be a second path with a second cache policy. This route exists because a browser asks for it by " +
			"name.",
		Responses: []Response{{Status: 302, Description: "The themed favicon URL"}},
	},
	"GET /theme/og.png": {
		Summary: "The social card, redirected to the themed one",
		Description: "The same reasoning as the favicon: one addressable image, one cache policy. The shell's " +
			"`og:image` is rendered per request from the published theme, so an operator can replace their social card " +
			"without a rebuild — the bytes a crawler fetches are the ones the server rendered.",
		Responses: []Response{{Status: 302, Description: "The themed social card URL"}},
	},
	"GET /theme/preview": {
		Summary: "Render an unpublished draft in the studio's preview frame",
		Description: "The one response in the product that may be framed by its own origin, and the only path that renders " +
			"an unpublished document. It is authorised by an HMAC-signed token that names the operator, the revision and " +
			"an expiry, so a leaked link stops working after fifteen minutes and is useless to a different session. The " +
			"response is `no-store` and `noindex`, and its `frame-ancestors` is `'self'` rather than `'none'` — every other " +
			"response in the product keeps `'none'`.",
		Responses: []Response{
			{Status: 200, Description: "The draft shell"},
			{Status: 403, Description: "The token is missing, expired or forged", Schema: ProblemRef()},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
			{Status: 422, Description: "The draft no longer validates", Schema: ProblemRef()},
		},
	},

	// ----------------------------------------------------------------- staff --

	"GET /api/v1/staff/theme": {
		Summary: "What is live in every scope, plus the gallery and the limits",
		Description: "One request for the studio's landing screen: the published revision and the open draft for each of " +
			"the five scopes, the preset catalogue and the bounds the server enforces. A scope with nothing published " +
			"simply has no `published` key — that is the built-in default, not a failure.",
		Responses: []Response{
			{Status: 200, Description: "The registry overview", Schema: map[string]any{
				"type":     "object",
				"required": []string{"scopes", "presets", "limits", "permissions"},
				"properties": map[string]any{
					"scopes": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"scope", "label"},
							"properties": map[string]any{
								"scope":     map[string]any{"type": "string"},
								"label":     map[string]any{"type": "string"},
								"published": themeRevisionSchema(),
								"draft":     themeRevisionSchema(),
							},
						},
					},
					"presets":     map[string]any{"type": "array", "items": presetMetaSchema()},
					"limits":      themeLimitsSchema(),
					"permissions": themePermissionsSchema(),
					"presets_ok":  map[string]any{"type": "boolean", "description": "`false` means the embedded catalogue failed to load, which is a build problem rather than an operator problem."},
				},
			}},
			{Status: 403, Description: "The caller lacks `theme:read`", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/theme/presets": {
		Summary: "The preset gallery",
		Description: "Ten structurally different starting points, not ten recolours: a preset may change the nav variant, " +
			"the density, the radius scale, the shadow language and the type families. Each card carries the character " +
			"line, the mode, the radius and a four-colour swatch.\n\n" +
			"Applying a preset creates a draft. Nothing here publishes, and `extends` is resolved at draft time, so a later " +
			"change to a preset cannot mutate a theme somebody already published.",
		Responses: []Response{
			{Status: 200, Description: "The catalogue", Schema: map[string]any{
				"type":     "object",
				"required": []string{"presets", "limits"},
				"properties": map[string]any{
					"presets": map[string]any{"type": "array", "items": presetMetaSchema()},
					"limits":  themeLimitsSchema(),
				},
			}},
		},
	},
	"GET /api/v1/staff/theme/{scope}": {
		Summary: "One scope: the document to edit, its draft, its published revision and its history",
		Description: "The editor's single read. `source` is what the operator wrote and is what the form binds to; " +
			"`document` is the resolved form and is what a preview shows. They differ whenever a scope inherits, which is " +
			"why both are served: binding a form to the resolved document would show the parent's tokens as if the operator " +
			"had typed them, and the next save would copy them into the child permanently.\n\n" +
			"When nothing exists yet, both are the built-in default — the editor opens on the product's own appearance " +
			"rather than on an empty form.",
		Responses: []Response{
			{Status: 200, Description: "The scope", Schema: map[string]any{
				"type":     "object",
				"required": []string{"scope", "label", "document", "source", "drafts", "history", "presets", "limits", "permissions"},
				"properties": map[string]any{
					"scope":       map[string]any{"type": "string"},
					"label":       map[string]any{"type": "string"},
					"document":    themeDocumentSchema(),
					"source":      themeDocumentSchema(),
					"published":   themeRevisionSchema(),
					"drafts":      map[string]any{"type": "array", "items": themeRevisionSchema()},
					"history":     map[string]any{"type": "array", "items": themePublicationSchema()},
					"presets":     map[string]any{"type": "array", "items": presetMetaSchema()},
					"limits":      themeLimitsSchema(),
					"permissions": themePermissionsSchema(),
				},
			}},
			{Status: 404, Description: "No such scope", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/theme/{scope}/draft": {
		Summary: "Create a draft",
		Description: "Three ways to start, in precedence order: an explicit `document` (validated as a layer, so a partial " +
			"edit is legitimate), a `preset` id, or neither — which duplicates what is live in that scope, the " +
			"\"a seasonal campaign, then revert\" case.\n\n" +
			"Creating a draft archives any previous open draft for the theme, in the same transaction. One open draft per " +
			"theme is the invariant that makes \"Save\" unambiguous: with two, which one it wrote to would depend on load " +
			"order.",
		RequestSchema: themeDraftBodySchema(),
		Responses: []Response{
			{Status: 201, Description: "The draft", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"revision"},
				"properties": map[string]any{"revision": themeRevisionSchema()},
			}},
			{Status: 403, Description: "The caller lacks `theme:write`", Schema: ProblemRef()},
			{Status: 422, Description: "The document does not validate", Schema: ProblemRef()},
		},
	},
	"PUT /api/v1/staff/theme/revisions/{id}": {
		Summary: "Replace a draft's document",
		Description: "Only a draft may be edited. A published revision is a promise about what a live site serves: editing it " +
			"would change the site with no publish and nothing to roll back to, so the attempt is refused with `not_a_draft` " +
			"and the remedy — restore it as a draft — is named in the error.",
		RequestSchema: themeDraftBodySchema(),
		Responses: []Response{
			{Status: 200, Description: "The updated draft", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"revision"},
				"properties": map[string]any{"revision": themeRevisionSchema()},
			}},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
			{Status: 409, Description: "The revision is not a draft", Schema: ProblemRef()},
			{Status: 422, Description: "The document does not validate", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/theme/revisions/{id}/publish": {
		Summary: "Publish a revision",
		Description: "Three writes in one transaction: archive the previously published revision, mark this one published, " +
			"point the scope's assignment at it. A publication row records who, when, what it replaced and why, because the " +
			"audit log has the action and the history screen needs the list.\n\n" +
			"The cache is invalidated after the commit, never before: invalidating first would let a concurrent request " +
			"reload the old revision and cache it for another TTL, which is the \"I published and nothing changed\" bug.\n\n" +
			"Requires `theme:publish`, not `theme:write`. Publishing panel custom CSS can hide staff-critical UI, and that is " +
			"a different decision from recolouring the storefront.",
		RequestSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"note": map[string]any{"type": "string", "description": "Why this went live, shown in the history list."}},
		},
		Responses: []Response{
			{Status: 200, Description: "The published revision and its publication record", Schema: map[string]any{
				"type":     "object",
				"required": []string{"revision", "publication"},
				"properties": map[string]any{
					"revision":    themeRevisionSchema(),
					"publication": themePublicationSchema(),
				},
			}},
			{Status: 403, Description: "The caller lacks `theme:publish`", Schema: ProblemRef()},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
			{Status: 409, Description: "An archived revision cannot be published", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/theme/revisions/{id}/restore": {
		Summary: "Restore an old revision as a new draft",
		Description: "Rollback is two steps — restore, then publish — so the history stays linear and the new revision is " +
			"visible rather than a revision silently reappearing.\n\n" +
			"It copies the old revision's *source*, not its resolved document: restoring revision 3 of a storefront theme " +
			"should start from what the operator wrote then and re-resolve against today's parent, not freeze the parent as " +
			"it was.",
		Responses: []Response{
			{Status: 201, Description: "The new draft", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"revision"},
				"properties": map[string]any{"revision": themeRevisionSchema()},
			}},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/theme/revisions/{id}/diff": {
		Summary: "What a revision changes against what is live",
		Description: "A structured diff, not two documents side by side: `{path, from, to}` per changed token, ordered by " +
			"path. The paths are the JSON keys the export envelope uses, so a line can be pasted into an import or matched " +
			"against a field name in the editor.\n\n" +
			"Both documents are flattened through the same JSON representation the merge uses, so a difference the merge " +
			"would not see is a difference this does not report either — which is what makes the preview trustworthy.",
		Responses: []Response{
			{Status: 200, Description: "The change list", Schema: map[string]any{
				"type":     "object",
				"required": []string{"changes", "summary"},
				"properties": map[string]any{
					"changes": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"path", "from", "to"},
							"properties": map[string]any{
								"path": map[string]any{"type": "string"},
								"from": map[string]any{"type": "string", "description": "The previous value as JSON, or empty when the field did not exist."},
								"to":   map[string]any{"type": "string", "description": "The new value as JSON, or empty when the field was removed."},
							},
						},
					},
					"summary": map[string]any{"type": "string", "description": "One line naming the count and the first few paths, for a confirmation dialog."},
				},
			}},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/theme/preview-token": {
		Summary: "Mint a preview link for a draft",
		Description: "The token is HMAC-signed over the operator, the revision, the scope and an expiry, and it is generated " +
			"per process — so a preview opened through a load balancer may reach an instance that did not mint it and be " +
			"refused. That is a refusal, not a bypass; making it work across instances is a deployment decision (sticky " +
			"sessions or a shared secret) rather than something this build guesses at.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"revision_id"},
			"properties": map[string]any{
				"revision_id": map[string]any{"type": "string", "format": "uuid"},
				"scope":       map[string]any{"type": "string", "description": "Defaults to the revision's own scope."},
			},
		},
		Responses: []Response{
			{Status: 200, Description: "The token and the URL to frame", Schema: map[string]any{
				"type":     "object",
				"required": []string{"token", "url", "expires_at"},
				"properties": map[string]any{
					"token":      map[string]any{"type": "string"},
					"url":        map[string]any{"type": "string"},
					"expires_at": map[string]any{"type": "string", "format": "date-time"},
				},
			}},
			{Status: 404, Description: "No such revision", Schema: ProblemRef()},
			{Status: 503, Description: "Preview signing is unavailable on this instance", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/theme/{scope}/export": {
		Summary: "Export a scope as a portable envelope",
		Description: "The envelope carries the *source* document, not the resolved one: an export is what the operator wrote, " +
			"and importing it into another installation should re-resolve against that installation's parent and default " +
			"rather than freezing this one's.\n\n" +
			"Deterministic, because the format is meant to be committed to a repository and diffed: two exports of the same " +
			"theme produce the same bytes.",
		Responses: []Response{
			{Status: 200, Description: "The envelope", Schema: map[string]any{
				"type":     "object",
				"required": []string{"kind", "schema", "exported_at", "theme"},
				"properties": map[string]any{
					"kind":        map[string]any{"type": "string", "enum": []string{"catalyst.theme"}},
					"schema":      map[string]any{"type": "integer"},
					"exported_at": map[string]any{"type": "string", "format": "date-time"},
					"theme":       themeDocumentSchema(),
					"assets": map[string]any{
						"type":        "array",
						"description": "Embedded assets, base64. Empty in this build: an export names the asset references and the operator moves the bytes through the asset library, because a multi-megabyte envelope is not a diffable file.",
						"items":       map[string]any{"type": "object"},
					},
				},
			}},
			{Status: 404, Description: "No such scope", Schema: ProblemRef()},
		},
	},
	"POST /api/v1/staff/theme/import": {
		Summary: "Import an envelope as a draft",
		Description: "The target scope comes from `?scope=`, not from the document: one document can legitimately serve " +
			"several scopes, and a document cannot know where it is being installed.\n\n" +
			"Idempotent on the resolved digest: importing the same envelope twice against a scope whose draft already has " +
			"that digest returns the existing revision with `unchanged: true` rather than creating a second identical one.",
		RequestSchema: map[string]any{
			"type":     "object",
			"required": []string{"kind", "schema", "theme"},
			"properties": map[string]any{
				"kind":        map[string]any{"type": "string", "enum": []string{"catalyst.theme"}},
				"schema":      map[string]any{"type": "integer"},
				"exported_at": map[string]any{"type": "string"},
				"theme":       themeDocumentSchema(),
				"assets":      map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			},
		},
		Responses: []Response{
			{Status: 201, Description: "The new draft", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"revision"},
				"properties": map[string]any{"revision": themeRevisionSchema()},
			}},
			{Status: 200, Description: "The draft already had this digest", Schema: map[string]any{
				"type":     "object",
				"required": []string{"revision", "unchanged"},
				"properties": map[string]any{
					"revision":  themeRevisionSchema(),
					"unchanged": map[string]any{"type": "boolean"},
				},
			}},
			{Status: 400, Description: "The target scope is missing or unknown", Schema: ProblemRef()},
			{Status: 422, Description: "The envelope or its document does not validate", Schema: ProblemRef()},
		},
	},
	"GET /api/v1/staff/theme/assets": {
		Summary: "The theme asset library",
		Description: "Backgrounds, social cards, favicons and fonts, with a usage count per asset. The count is exact: the " +
			"only way an asset is referenced is as `theme-asset:<uuid>` inside a document, so searching the revisions' JSON " +
			"for the id finds every reference.",
		Responses: []Response{
			{Status: 200, Description: "The library", Schema: map[string]any{
				"type":     "object",
				"required": []string{"assets", "limits"},
				"properties": map[string]any{
					"assets": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":           map[string]any{"type": "string", "format": "uuid"},
							"kind":         map[string]any{"type": "string"},
							"content_type": map[string]any{"type": "string"},
							"byte_size":    map[string]any{"type": "integer"},
							"digest":       map[string]any{"type": "string"},
							"width":        map[string]any{"type": []any{"integer", "null"}},
							"height":       map[string]any{"type": []any{"integer", "null"}},
							"alt":          map[string]any{"type": "string"},
							"usage":        map[string]any{"type": "integer"},
						},
					}},
					"limits": themeLimitsSchema(),
				},
			}},
		},
	},
	"POST /api/v1/staff/theme/assets": {
		Summary: "Upload a theme asset",
		Description: "A raw-body upload with the content type in the header and the role in `?kind=`, matching the catalog's " +
			"image upload: one fewer parser in the request path, and the browser has a File in hand.\n\n" +
			"The bytes are checked, not trusted: a PNG's dimensions are read with the standard decoder and its declared type " +
			"must match its contents, a WOFF2 must start with the WOFF2 signature, and the dimensions are bounded. SVG is " +
			"refused — a sanitiser for a format whose purpose is to be a document is worse than a refusal, and a favicon can " +
			"be a PNG.\n\n" +
			"Deduped on `(digest, kind)`: the same bytes as a favicon and as a banner are two roles, and deleting one must not " +
			"break the other.",
		Responses: []Response{
			{Status: 201, Description: "The stored asset", Schema: map[string]any{
				"type":     "object",
				"required": []string{"id", "kind", "content_type", "url", "ref"},
				"properties": map[string]any{
					"id":           map[string]any{"type": "string", "format": "uuid"},
					"kind":         map[string]any{"type": "string"},
					"content_type": map[string]any{"type": "string"},
					"byte_size":    map[string]any{"type": "integer"},
					"width":        map[string]any{"type": []any{"integer", "null"}},
					"height":       map[string]any{"type": []any{"integer", "null"}},
					"digest":       map[string]any{"type": "string"},
					"url":          map[string]any{"type": "string", "description": "Where the asset is served."},
					"ref":          map[string]any{"type": "string", "description": "The `theme-asset:<uuid>` reference to put in a document."},
				},
			}},
			{Status: 400, Description: "The kind is missing or the body is empty", Schema: ProblemRef()},
			{Status: 413, Description: "The asset exceeds 4 MiB", Schema: ProblemRef()},
			{Status: 415, Description: "The content type is not accepted", Schema: ProblemRef()},
			{Status: 422, Description: "The bytes do not match the declared type", Schema: ProblemRef()},
		},
	},
	"DELETE /api/v1/staff/theme/assets/{id}": {
		Summary: "Delete an unused theme asset",
		Description: "Refused while a revision still references the asset: a published revision's stylesheet would start " +
			"returning 404 for its own background, which is a silent visual regression on a live site rather than an error " +
			"anybody sees.",
		Responses: []Response{
			{Status: 200, Description: "The asset was deleted", Schema: map[string]any{
				"type":       "object",
				"required":   []string{"deleted"},
				"properties": map[string]any{"deleted": map[string]any{"type": "string", "format": "uuid"}},
			}},
			{Status: 404, Description: "No such asset", Schema: ProblemRef()},
			{Status: 409, Description: "A revision still references the asset", Schema: ProblemRef()},
		},
	},
}

// themePublicationSchema is one entry in the history list.
func themePublicationSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "scope", "revision_id", "revision_number", "published_at"},
		"properties": map[string]any{
			"id":                   map[string]any{"type": "string", "format": "uuid"},
			"scope":                map[string]any{"type": "string"},
			"theme_id":             map[string]any{"type": "string", "format": "uuid"},
			"revision_id":          map[string]any{"type": "string", "format": "uuid"},
			"revision_number":      map[string]any{"type": "integer"},
			"previous_revision_id": map[string]any{"type": []any{"string", "null"}, "format": "uuid", "description": "What this publish replaced, so a rollback target is one click away."},
			"published_by":         map[string]any{"type": []any{"string", "null"}, "format": "uuid"},
			"published_at":         map[string]any{"type": "string", "format": "date-time"},
			"note":                 map[string]any{"type": "string"},
		},
	}
}
