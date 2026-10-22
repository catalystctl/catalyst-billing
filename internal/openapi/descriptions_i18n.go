package openapi

// The locale and translation descriptions (I18N-01…I18N-08).
//
// Two audiences, and the split between them is the design of this domain rather than a
// presentation choice.
//
// The **public** routes exist because the storefront is public: a visitor who has not signed in
// still sees a language switcher, and the `Accept-Language` header that resolves their request is
// read before any account exists. What they may fetch is bounded accordingly — the locale list
// carries codes and names, and the message catalogue is the merged text for one locale, never the
// whole table.
//
// The **staff** routes are the body of work: adding a locale, moving the default, and editing
// translations. Three properties of them cannot be expressed in a schema and are stated here
// because a client author needs to know them:
//
//  1. Exactly one locale is the default, enforced by a partial unique index. `POST
//     /staff/locales/{code}/default` moves it; nothing else can set it.
//  2. The default locale cannot be deleted, and its own translations cannot be removed. Both are
//     refused with the `default_locale` code rather than a generic validation error, because the
//     request is well-formed and the *installation's state* is what makes it impossible.
//  3. Removing a translation is not removing a key. A locale that loses a row falls back to the
//     default locale for that key; the key stays in the catalogue.

func init() { RegisterDescriptions(i18nOperations) }

func localeSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "name", "enabled", "is_default"},
		"properties": map[string]any{
			"code": map[string]any{
				"type": "string",
				"description": "A BCP-47 tag, normalised to lower-case language and upper-case region: " +
					"`de`, `pt-BR`, `zh-Hans`. It is the primary key, appears in URLs and is referenced " +
					"by every catalogue row and customer preference, so it is read on create and never " +
					"changed by an update.",
			},
			"name":        map[string]any{"type": "string", "description": "The English name, e.g. `German`."},
			"native_name": map[string]any{"type": "string", "description": "The name in the language itself, e.g. `Deutsch`. What the switcher shows."},
			"enabled":     map[string]any{"type": "boolean", "description": "Whether the locale is offered and can resolve. A disabled locale's translations are kept."},
			"is_default": map[string]any{
				"type": "boolean",
				"description": "The locale a request with no usable preference resolves to. Exactly one " +
					"locale has this; it is moved with `POST /staff/locales/{code}/default`, not by " +
					"sending this field.",
			},
			"sort_order": map[string]any{"type": "integer", "description": "Display order in the switcher."},
			"translated": map[string]any{"type": "integer", "description": "How many keys this locale has its own text for. Staff reads only."},
			"total_keys": map[string]any{"type": "integer", "description": "How many keys exist in the default catalogue."},
			"coverage": map[string]any{
				"type": "integer",
				"description": "`translated / total_keys` as a rounded percentage. Computed server-side so " +
					"every surface agrees on the rounding.",
			},
			"created_at": map[string]any{"type": "string", "format": "date-time"},
			"updated_at": map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

func i18nOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ public --

		"GET /api/v1/i18n/locales": {
			Summary: "The languages this shop offers, and what this request resolved to",
			Description: "Public. Answers the enabled locales, the default, and the resolution this " +
				"request produced.\n\n" +
				"The `resolved.source` field is the useful half: `preference` means the signed-in " +
				"customer chose this language, `header` means their browser asked for it, and " +
				"`default` means neither was usable. A storefront that showed a switcher pre-set to " +
				"the header's guess without distinguishing the two would misrepresent a choice as a " +
				"guess.\n\n" +
				"A stored preference is ignored when its locale is no longer enabled, because an " +
				"operator switching a language off is a decision that outranks a customer's earlier " +
				"one.",
		},
		"GET /api/v1/i18n/messages": {
			Summary: "The merged message catalogue for one locale",
			Description: "Public. `locale` defaults to whatever this request resolved to.\n\n" +
				"**Merged** is the whole contract: the response contains the locale's own text *plus* " +
				"the default locale's text for every key it lacks. A client that received only the " +
				"locale's rows would have to reimplement the fallback chain, and would get it subtly " +
				"different — which is how a blank label reaches a screen.\n\n" +
				"Cached for 60 seconds with revalidation: an operator editing a translation expects " +
				"the change to appear without a deploy, and the catalogue is public configuration " +
				"rather than per-customer state.",
		},

		// ------------------------------------------------------------- staff --

		"GET /api/v1/staff/locales": {
			Summary: "Every locale with its translation coverage",
			Description: "Includes disabled locales, because an operator working on one has not enabled it " +
				"yet.\n\n" +
				"Coverage is part of this response rather than a second call: an operator adds a locale " +
				"and immediately asks how much is still untranslated, and two requests is two chances " +
				"to render `0%` for a moment.",
		},
		"POST /api/v1/staff/locales": {
			Summary: "Add a locale",
			Description: "A new locale starts with no translations, so every key falls back to the default " +
				"until it is translated. Enabling it immediately is allowed: the shop reads in the " +
				"default language for untranslated keys, which is a coherent state rather than a " +
				"broken one.\n\n" +
				"The code is normalised, so `DE-de` and `de-DE` are one locale rather than two.",
		},
		"PATCH /api/v1/staff/locales/{code}": {
			Summary: "Edit a locale's name, native name, enabled state or sort order",
			Description: "Partial: only the fields present are changed. The `code` is the primary key and is " +
				"never changed by an update, because it is referenced by every catalogue row and every " +
				"customer preference — renaming it is a data migration, not an edit.\n\n" +
				"Refused with `default_locale` when it would disable the default.",
		},
		"POST /api/v1/staff/locales/{code}/default": {
			Summary: "Make this locale the default",
			Description: "Moves the default rather than adding a second one: the database holds a partial " +
				"unique index, so a `default` that is two locales cannot exist.\n\n" +
				"Its own endpoint rather than a field on the update, because it is the one locale " +
				"change that moves every request in the installation at once, and an operator should " +
				"be able to make it without also sending the rest of the record — a full update " +
				"carrying a stale name would silently revert a rename made in another tab.",
		},
		"DELETE /api/v1/staff/locales/{code}": {
			Summary: "Remove a locale and its translations",
			Description: "Deletes the locale's catalogue rows and stops offering the language. The keys " +
				"themselves are untouched — they stay in the default locale.\n\n" +
				"Refused with `default_locale` when the locale is the default: an installation with no " +
				"default resolves every request through a code-side fallback nobody chose. The " +
				"operator's next action is to make another locale the default first, which is what the " +
				"error says.",
		},
		"GET /api/v1/staff/locales/keys": {
			Summary: "Search the catalogue by key",
			Description: "Answers one row per key with its default text, its translations, and the enabled " +
				"locales that still lack it. That last list is what makes \"still to translate\" a list " +
				"rather than an arithmetic exercise.\n\n" +
				"A key with an empty `default` is a real state, not a bug: a translation row that " +
				"mentions a key the default locale has no row for. It is shown so it can be corrected " +
				"rather than hidden.",
		},
		"PUT /api/v1/staff/locales/{code}/keys/{key}": {
			Summary: "Set one translation",
			Description: "Idempotent on the (locale, key) pair: writing the same text twice is one row, and " +
				"the response carries who last edited it.\n\n" +
				"The text is stored verbatim — it is operator prose, not markup, and it is rendered as " +
				"text. Refused with `default_locale` when it would leave the default locale's own row " +
				"empty, because that row is what every other locale falls back to.",
		},
		"DELETE /api/v1/staff/locales/{code}/keys/{key}": {
			Summary: "Remove one translation so the key falls back",
			Description: "This does not delete the key: the default locale's row is untouched, and this locale " +
				"now falls back to it for that key. That distinction is the reason the endpoint exists " +
				"separately from deleting a locale.\n\n" +
				"Refused with `default_locale` for the default locale's own row, which would leave the " +
				"key with no text anywhere and make the fallback the key itself, in every locale at " +
				"once.",
		},
	}
}

var _ = localeSchema
