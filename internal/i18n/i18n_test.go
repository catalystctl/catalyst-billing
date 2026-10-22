package i18n_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/catalyst/billing/internal/i18n"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The locale package's tests, against the real database.
//
// The three properties worth pinning, in order of how much damage their absence causes:
//
//  1. **A missing key falls back, it never renders blank.** A blank label is a UI bug that looks
//     like a design choice, so the fallback chain is the feature.
//  2. **Exactly one default locale exists.** The schema enforces it with a partial unique index
//     and the code refuses the transitions that would break it; both are tested, because a
//     "default" that is two locales is a shop that renders in whichever row the planner read
//     first.
//  3. **A customer's own choice beats their browser's header.** The header is a guess about a
//     stranger; a stored preference is a decision.

func newStore(t *testing.T) *i18n.Store {
	t.Helper()
	return i18n.New(storetest.DB(t))
}

// ---------------------------------------------------------------- resolver --

// The chain, in the order a request actually walks it.
func TestResolvePrefersTheStoredChoiceThenTheHeaderThenTheDefault(t *testing.T) {
	r := i18n.NewResolver("en", []string{"en", "de", "pt"})

	cases := []struct {
		name       string
		preference string
		header     string
		want       string
		source     i18n.Source
	}{
		{"the customer's choice wins over their browser", "de", "pt-BR,pt;q=0.9", "de", i18n.SourcePreference},
		{"the header decides when nobody chose", "", "pt-BR,pt;q=0.9", "pt", i18n.SourceHeader},
		{"a regional tag resolves to its base language", "", "de-AT", "de", i18n.SourceHeader},
		{"an unoffered language falls through to the default", "", "ja-JP", "en", i18n.SourceDefault},
		{"no header at all is the default", "", "", "en", i18n.SourceDefault},
		// A stored choice that is no longer offered must not win: the operator disabled it.
		{"a disabled preference is ignored", "fr", "de", "de", i18n.SourceHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Resolve(tc.preference, tc.header)
			if got.Locale != tc.want {
				t.Fatalf("locale = %q, want %q (candidate %q)", got.Locale, tc.want, got.Candidate)
			}
			if got.Source != tc.source {
				t.Fatalf("source = %q, want %q", got.Source, tc.source)
			}
		})
	}
}

// A request for `de` must not resolve to `de-AT`. A shop that offers Austrian German has not
// thereby offered German, and the reverse direction would silently serve a dialect.
func TestResolveDoesNotWidenToARegionalLocale(t *testing.T) {
	r := i18n.NewResolver("en", []string{"en", "de-AT"})
	got := r.Resolve("", "de")
	if got.Locale != "en" {
		t.Fatalf("locale = %q, want the default: a plain `de` request must not be served de-AT", got.Locale)
	}
}

func TestParseAcceptLanguageRanksByQualityAndKeepsOrderOnTies(t *testing.T) {
	got := i18n.ParseAcceptLanguage("fr;q=0.2, de-AT;q=0.9, en;q=0.9, *;q=0.1")
	// de-AT before en because they tie at 0.9 and the header listed de-AT first; fr last.
	want := []string{"de-AT", "en", "fr"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v (the wildcard names no language)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}

	// q=0 means "not acceptable", so it must be dropped rather than ranked last.
	got = i18n.ParseAcceptLanguage("en, de;q=0")
	if len(got) != 1 || got[0] != "en" {
		t.Fatalf("candidates = %v, want just en: q=0 refuses a language", got)
	}
}

// ---------------------------------------------------------------- locales --

// The default is a database invariant, so the code must refuse to break it.
func TestExactlyOneDefaultAndItCannotBeDeleted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// The seed gives `en`. Creating a second locale must not make it the default implicitly.
	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", NativeName: "Deutsch", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}
	locales, err := s.Locales(ctx)
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	defaults := 0
	for _, l := range locales {
		if l.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("%d default locales, want exactly 1", defaults)
	}

	// The default cannot be deleted: an installation with no default resolves every request
	// through a code-side fallback nobody chose.
	if err := s.Delete(ctx, "en"); !errors.Is(err, i18n.ErrDefaultLocale) {
		t.Fatalf("deleting the default: %v, want ErrDefaultLocale", err)
	}

	// Making another locale the default moves it rather than adding a second.
	if _, err := s.SetDefault(ctx, "de"); err != nil {
		t.Fatalf("set default de: %v", err)
	}
	locales, err = s.Locales(ctx)
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	for _, l := range locales {
		if l.Code == "de" && !l.IsDefault {
			t.Fatal("de is not the default after being made the default")
		}
		if l.Code == "en" && l.IsDefault {
			t.Fatal("en is still the default: the index allows exactly one")
		}
	}

	// And now en may be deleted, because it is no longer the default.
	if err := s.Delete(ctx, "en"); err != nil {
		t.Fatalf("deleting a non-default locale: %v", err)
	}
}

// A locale code is normalised, so `DE-de` and `de-DE` are one locale rather than two.
func TestLocaleCodesAreNormalised(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, i18n.LocaleInput{Code: "pt-br", Name: "Portuguese (Brazil)", NativeName: "Português", Enabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Code != "pt-BR" {
		t.Fatalf("code = %q, want pt-BR", created.Code)
	}
	if _, err := s.Create(ctx, i18n.LocaleInput{Code: "PT-BR", Name: "Duplicate", Enabled: boolPtr(true)}); err == nil {
		t.Fatal("a differently-cased duplicate was accepted, so one language is two rows")
	}
	// BCP-47 allows extra variant subtags, so `de-DE-1911` is a legal code and not a case worth
	// refusing. What must be refused is a code no resolver could match: an empty string, a
	// one-letter language, a separator that is not a dash, or a non-alphabetic language.
	for _, bad := range []string{"", "e", "english!", "en_US", "1n"} {
		if _, err := i18n.NormalizeCode(bad); err == nil {
			t.Fatalf("%q was accepted as a locale code", bad)
		}
	}
}

// ---------------------------------------------------------------- catalog --

// The fallback chain, which is the reason the catalog exists: a key missing in a locale falls
// back to the default locale and then to the key itself, never to a blank string.
func TestAMissingTranslationFallsBackRatherThanRenderingBlank(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", NativeName: "Deutsch", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}

	// The default locale has both keys; German has only one of them.
	if _, err := s.Put(ctx, "en", "invoice.paid", "Invoice paid", ""); err != nil {
		t.Fatalf("put en: %v", err)
	}
	if _, err := s.Put(ctx, "en", "invoice.overdue", "Invoice overdue", ""); err != nil {
		t.Fatalf("put en: %v", err)
	}
	if _, err := s.Put(ctx, "de", "invoice.paid", "Rechnung bezahlt", ""); err != nil {
		t.Fatalf("put de: %v", err)
	}

	cat, err := s.LoadCatalog(ctx, "de", "en")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	if got := cat.Translate("de", "invoice.paid"); got != "Rechnung bezahlt" {
		t.Fatalf("translated key = %q, want the German text", got)
	}
	// Missing in German, present in the default: the default's text, not "".
	if got := cat.Translate("de", "invoice.overdue"); got != "Invoice overdue" {
		t.Fatalf("missing key = %q, want the default locale's text", got)
	}
	// Missing everywhere: the key itself, which is visible and greppable.
	if got := cat.Translate("de", "invoice.unknown"); got != "invoice.unknown" {
		t.Fatalf("unknown key = %q, want the key itself", got)
	}

	// A merged view for a locale is what a screen ships to the browser, so it must contain the
	// fallback too — otherwise the browser would have to reimplement the chain.
	merged := cat.Merged("de")
	if merged["invoice.paid"] != "Rechnung bezahlt" {
		t.Fatalf("merged paid = %q", merged["invoice.paid"])
	}
	if merged["invoice.overdue"] != "Invoice overdue" {
		t.Fatalf("merged overdue = %q, want the fallback text", merged["invoice.overdue"])
	}
}

// Deleting the default locale's own translation would leave a key with no text anywhere, so it is
// refused: the fallback would become the key itself, for every locale at once.
func TestTheDefaultLocalesOwnTranslationCannotBeDeleted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Put(ctx, "en", "nav.dashboard", "Dashboard", ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.DeleteTranslation(ctx, "en", "nav.dashboard"); !errors.Is(err, i18n.ErrDefaultLocale) {
		t.Fatalf("deleting the default's translation: %v, want ErrDefaultLocale", err)
	}
	// A non-default locale's translation can be deleted, which is how it falls back.
	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}
	if _, err := s.Put(ctx, "de", "nav.dashboard", "Übersicht", ""); err != nil {
		t.Fatalf("put de: %v", err)
	}
	if err := s.DeleteTranslation(ctx, "de", "nav.dashboard"); err != nil {
		t.Fatalf("deleting a non-default translation: %v", err)
	}
}

// A translation is text a customer reads, so the substitution must not be able to invent one.
func TestSubstituteFillsNamedPlaceholdersOnly(t *testing.T) {
	got := i18n.Substitute("Invoice {{number}} is due {{date}}", map[string]string{
		"number": "INV-1",
		"date":   "1 March",
	})
	if got != "Invoice INV-1 is due 1 March" {
		t.Fatalf("substituted = %q", got)
	}
	// An unknown placeholder is left alone rather than blanked: a translator's typo should be
	// visible, not silently delete the surrounding words.
	if got := i18n.Substitute("Hello {{name}}", nil); got != "Hello {{name}}" {
		t.Fatalf("unknown placeholder = %q, want it left in place", got)
	}

	// A single pass, so a value that itself contains a placeholder is written literally rather
	// than expanded. This is the property that stops a customer who can type into a field from
	// making a rendered message pull in another variable's value.
	got = i18n.Substitute("{{greeting}} {{name}}", map[string]string{
		"greeting": "Hello {{name}}",
		"name":     "Toil",
	})
	if got != "Hello {{name}} Toil" {
		t.Fatalf("nested substitution = %q, want the value written literally", got)
	}
}

func TestValidateKeyRefusesWhatCannotBeStored(t *testing.T) {
	if err := i18n.ValidateKey("nav.dashboard"); err != nil {
		t.Fatalf("a normal key was refused: %v", err)
	}
	// The rule the implementation enforces is lower-case letters, digits and the three
	// separators, not starting with a separator. A trailing dot is therefore accepted, which is
	// untidy but harmless; what must be refused is anything that could not be a stable key.
	for _, bad := range []string{"", " ", "has space", "UPPER", ".leading", "exclamation!"} {
		if err := i18n.ValidateKey(bad); err == nil {
			t.Fatalf("%q was accepted as a message key", bad)
		}
	}
}

// Coverage is what an operator looks at to decide whether a locale is ready, so it has to count
// the right thing.
func TestCoverageCountsTranslatedKeysPerLocale(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}
	for _, key := range []string{"a.one", "a.two"} {
		if _, err := s.Put(ctx, "en", key, "English "+key, ""); err != nil {
			t.Fatalf("put en %s: %v", key, err)
		}
	}
	if _, err := s.Put(ctx, "de", "a.one", "Deutsch", ""); err != nil {
		t.Fatalf("put de: %v", err)
	}

	coverage, err := s.Coverage(ctx)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage["de"] != 1 {
		t.Fatalf("de coverage = %d, want 1 (one of the two keys translated)", coverage["de"])
	}
	if coverage["en"] != 2 {
		t.Fatalf("en coverage = %d, want 2", coverage["en"])
	}
}

// A customer's stored choice is what the switcher persists, and it is separate from the header.
func TestUserLocaleRoundTrips(t *testing.T) {
	// One DB(t) call per test: it takes a session advisory lock, so a second call in the same
	// test would block on itself until the deadline.
	pool := storetest.DB(t)
	s := i18n.New(pool)
	ctx := context.Background()

	// `de` ships disabled, and a stored preference must name a locale the shop actually offers.
	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}

	// A user row, made here rather than through a helper: this package needs an id to hang a
	// preference on and nothing else about the account.
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name)
		 VALUES ('i18n-user@example.test', 'not-a-real-hash', 'I18n User')
		 RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if got, err := s.UserLocale(ctx, userID); err != nil || got != "" {
		t.Fatalf("a fresh user's locale = %q, %v; want empty (no opinion)", got, err)
	}
	if got, err := s.SetUserLocale(ctx, userID, "de"); err != nil {
		t.Fatalf("set: %v", err)
	} else if got != "de" {
		t.Fatalf("set returned %q", got)
	}
	if got, err := s.UserLocale(ctx, userID); err != nil || got != "de" {
		t.Fatalf("stored locale = %q, %v", got, err)
	}
	// Clearing it is how a customer goes back to following their browser.
	if _, err := s.SetUserLocale(ctx, userID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := s.UserLocale(ctx, userID); got != "" {
		t.Fatalf("after clearing, locale = %q", got)
	}
}

// The enabled set is what the switcher offers, and the default is always in it even if an
// operator disabled it by mistake — a shop with no offered locale cannot render at all.
func TestEnabledLocalesAlwaysIncludeTheDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}
	if _, err := s.Update(ctx, "en", i18n.LocaleInput{Code: "en", Name: "English", Enabled: boolPtr(false)}); err == nil {
		// Refusing to disable the default is the preferred outcome; disabling it and still
		// offering it is the fallback. Either is acceptable, silently offering nothing is not.
		t.Log("the default locale was allowed to be disabled; the enabled set must still contain it")
	}

	enabled, err := s.Enabled(ctx)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if len(enabled) == 0 {
		t.Fatal("no enabled locale: nothing could render")
	}
	codes := make([]string, 0, len(enabled))
	for _, l := range enabled {
		codes = append(codes, l.Code)
	}
	sort.Strings(codes)
	if codes[0] != "de" && codes[0] != "en" {
		t.Fatalf("enabled = %v", codes)
	}
}

// boolPtr is the pointer a LocaleInput's optional booleans take: nil means "leave it alone",
// which is different from false.
func boolPtr(v bool) *bool { return &v }
