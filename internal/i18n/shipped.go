package i18n

// The languages this build can offer, and the one-click install that turns one of them on.
//
// Paymenter ships twenty-four `lang/*` files, so an operator choosing a language picks from a
// menu. This panel shipped two seeded rows — English, and German as the worked example — which
// made "add Portuguese" a task that started with typing the language's English name, its native
// name and a BCP 47 tag into a form. Nothing was technically missing: a locale row was always
// creatable. What was missing was the menu.
//
// So the shipped list lives here, in code, and it is deliberately *not* a migration's seed rows.
// A locale row is configuration (the design note in 0037_i18n.sql says so: like a currency row,
// it decides how a page reads). Seeding twenty-four disabled rows would put every language this
// shop does not speak into its switcher's source of truth, into every coverage query, and into the
// operator's table — most of them at 0% translated, which is a wall of rows that says "unfinished"
// about a feature nobody asked for.
//
// What the list does buy is completeness of the *choice*: `GET /api/v1/staff/locales/available`
// answers with the shipped languages that are not configured yet, and
// `POST /api/v1/staff/locales/{code}/install` adds one with its names already filled in. A shop
// that wants German, Portuguese and Japanese sees three clicks and three rows.
//
// # What this does not do
//
// It does not ship the translations themselves. Enabling a language whose catalogue is empty means
// every key falls back to the default locale's text — readable, correct, and in the wrong language
// until someone translates it on /locales. That is the honest state of "breadth": the menu is
// complete, the vocabulary is not, and the coverage column on the locales screen is where that is
// visible instead of surprising.

import "context"

// ShippedLocale is one language this build knows how to name.
type ShippedLocale struct {
	// Code is a BCP 47 tag in canonical case.
	Code string `json:"code"`
	// Name is the English name, for an operator's list.
	Name string `json:"name"`
	// NativeName is the name in the language itself, for the customer's switcher.
	NativeName string `json:"native_name"`
}

// shippedLocales is the menu, in the order a list should show it: the default first, then the
// languages a shop is most likely to want, then the rest by code.
//
// Twenty-four entries because that is the breadth this parity item is measured against, and
// because a menu that grows to include every possible tag is a menu nobody finishes reading. A
// language not listed here is still creatable by hand — the form accepts any well-formed tag, and
// that path is what makes this list a convenience rather than a limit.
var shippedLocales = []ShippedLocale{
	{Code: "en", Name: "English", NativeName: "English"},
	{Code: "de", Name: "German", NativeName: "Deutsch"},
	{Code: "fr", Name: "French", NativeName: "Français"},
	{Code: "es", Name: "Spanish", NativeName: "Español"},
	{Code: "it", Name: "Italian", NativeName: "Italiano"},
	{Code: "pt", Name: "Portuguese", NativeName: "Português"},
	{Code: "pt-BR", Name: "Portuguese (Brazil)", NativeName: "Português (Brasil)"},
	{Code: "nl", Name: "Dutch", NativeName: "Nederlands"},
	{Code: "pl", Name: "Polish", NativeName: "Polski"},
	{Code: "ro", Name: "Romanian", NativeName: "Română"},
	{Code: "sv", Name: "Swedish", NativeName: "Svenska"},
	{Code: "fi", Name: "Finnish", NativeName: "Suomi"},
	{Code: "el", Name: "Greek", NativeName: "Ελληνικά"},
	{Code: "tr", Name: "Turkish", NativeName: "Türkçe"},
	{Code: "ru", Name: "Russian", NativeName: "Русский"},
	{Code: "uk", Name: "Ukrainian", NativeName: "Українська"},
	{Code: "ar", Name: "Arabic", NativeName: "العربية"},
	{Code: "hi", Name: "Hindi", NativeName: "हिन्दी"},
	{Code: "id", Name: "Indonesian", NativeName: "Bahasa Indonesia"},
	{Code: "vi", Name: "Vietnamese", NativeName: "Tiếng Việt"},
	{Code: "ja", Name: "Japanese", NativeName: "日本語"},
	{Code: "ko", Name: "Korean", NativeName: "한국어"},
	{Code: "zh-CN", Name: "Chinese (Simplified)", NativeName: "简体中文"},
	{Code: "zh-TW", Name: "Chinese (Traditional)", NativeName: "繁體中文"},
}

// ShippedLocales returns the shipped menu.
//
// It returns a copy so a caller cannot reorder the package's own list — the order is the order an
// operator reads, and a handler that sorted it in place would change a later request's answer.
func ShippedLocales() []ShippedLocale {
	out := make([]ShippedLocale, len(shippedLocales))
	copy(out, shippedLocales)
	return out
}

// ShippedLocaleFor finds one shipped language by code, normalising the case first.
func ShippedLocaleFor(code string) (ShippedLocale, bool) {
	normalized, err := NormalizeCode(code)
	if err != nil {
		return ShippedLocale{}, false
	}
	for _, l := range shippedLocales {
		if l.Code == normalized {
			return l, true
		}
	}
	return ShippedLocale{}, false
}

// InstallShippedTx adds one shipped language to the installation, in the caller's transaction.
//
// The locale is created *enabled*: the operator clicked "install", which is the deliberate act
// this design note asks for — a language that has to be enabled in a second step is one somebody
// will think they added and cannot use. Its catalogue is empty, so every label falls back to the
// default locale's text until it is translated; the coverage column makes that visible.
//
// An unknown code is a 422 rather than a 404 because it is the request that is wrong (a language
// this build does not know), not a record that is missing; an already-configured code is the
// existing duplicate-locale conflict.
func (s *Store) InstallShippedTx(ctx context.Context, q querier, code string) (Locale, error) {
	shipped, ok := ShippedLocaleFor(code)
	if !ok {
		return Locale{}, invalidField("code", FieldUnknown,
			"this build does not ship that language; add it by hand with a BCP 47 code")
	}
	sortOrder := 0
	for i, l := range shippedLocales {
		if l.Code == shipped.Code {
			sortOrder = (i + 1) * 10
			break
		}
	}
	enabled := true
	return s.CreateTx(ctx, q, LocaleInput{
		Code:       shipped.Code,
		Name:       shipped.Name,
		NativeName: shipped.NativeName,
		Enabled:    &enabled,
		SortOrder:  &sortOrder,
	})
}

// AvailableShipped reports the shipped languages an installation has not configured yet.
//
// The comparison is on the normalised code the locales table stores, so a hand-added `pt_br` row
// is not silently offered again as `pt-BR`: NormalizeCode is the same function that decides what a
// row's primary key is.
func AvailableShipped(configured []Locale) []ShippedLocale {
	have := make(map[string]struct{}, len(configured))
	for _, l := range configured {
		have[l.Code] = struct{}{}
	}
	out := make([]ShippedLocale, 0, len(shippedLocales))
	for _, l := range shippedLocales {
		if _, ok := have[l.Code]; ok {
			continue
		}
		out = append(out, l)
	}
	return out
}
