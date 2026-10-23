package i18n_test

// The shipped-language menu and the machine-readable refusals.
//
// Two behaviours are pinned here:
//
//   - installing a shipped language produces a locale an operator can use — enabled, named in both
//     languages, and with a stable sort position — because the alternative the parity audit found
//     was "type the language's English name, native name and tag by hand";
//   - a refusal names the field it is about, so a client can render it in the reader's locale
//     instead of showing the server's English sentence.

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/i18n"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The menu is the breadth this feature is measured by, and every entry must be installable.
func TestShippedLocalesAreWellFormedAndUnique(t *testing.T) {
	locales := i18n.ShippedLocales()
	if len(locales) != 24 {
		t.Fatalf("shipped locales = %d, want the twenty-four this build advertises", len(locales))
	}
	seen := map[string]bool{}
	for _, l := range locales {
		if seen[l.Code] {
			t.Fatalf("duplicate shipped code %q", l.Code)
		}
		seen[l.Code] = true
		normalized, err := i18n.NormalizeCode(l.Code)
		if err != nil {
			t.Fatalf("shipped code %q is not a valid tag: %v", l.Code, err)
		}
		if normalized != l.Code {
			t.Fatalf("shipped code %q is not canonical (want %q)", l.Code, normalized)
		}
		if l.Name == "" || l.NativeName == "" {
			t.Fatalf("shipped locale %q has no names: %+v", l.Code, l)
		}
	}
	if _, ok := i18n.ShippedLocaleFor("de"); !ok {
		t.Fatal("German is not in the shipped menu")
	}
	// The list is returned as a copy: a caller sorting it must not reorder the package's own order.
	out := i18n.ShippedLocales()
	out[0].Code = "zz"
	if again := i18n.ShippedLocales(); again[0].Code != "en" {
		t.Fatal("ShippedLocales handed out its own slice")
	}
}

// Installing a shipped language creates an enabled, named locale.
func TestInstallShippedCreatesAnEnabledLocale(t *testing.T) {
	pool := storetest.DB(t)
	store := i18n.New(pool)
	ctx := context.Background()

	installed, err := store.InstallShippedTx(ctx, pool, "pt-br")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Code != "pt-BR" {
		t.Fatalf("code = %q, want the canonical pt-BR", installed.Code)
	}
	if !installed.Enabled {
		t.Fatal("an installed language is not offered")
	}
	if installed.Name != "Portuguese (Brazil)" || installed.NativeName != "Português (Brasil)" {
		t.Fatalf("names = %q / %q, want the shipped ones", installed.Name, installed.NativeName)
	}
	if installed.SortOrder <= 0 {
		t.Fatalf("sort order = %d, want a positive position", installed.SortOrder)
	}

	// And it is no longer offered for installation, so the screen cannot show a row that would
	// fail on submit.
	all, err := store.Locales(ctx)
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	for _, l := range i18n.AvailableShipped(all) {
		if l.Code == "pt-BR" {
			t.Fatal("an installed language is still offered for installation")
		}
	}
}

// A language that is already configured is the existing duplicate conflict, not a new one.
func TestInstallShippedRefusesADuplicate(t *testing.T) {
	pool := storetest.DB(t)
	store := i18n.New(pool)
	ctx := context.Background()
	if _, err := store.InstallShippedTx(ctx, pool, "fr"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := store.InstallShippedTx(ctx, pool, "fr"); !errors.Is(err, i18n.ErrCodeTaken) {
		t.Fatalf("second install error = %v, want ErrCodeTaken", err)
	}
}

// A language this build does not ship is a field refusal naming `code`.
func TestInstallShippedRefusesAnUnknownLanguage(t *testing.T) {
	pool := storetest.DB(t)
	store := i18n.New(pool)
	_, err := store.InstallShippedTx(context.Background(), pool, "zz-ZZ")
	if !errors.Is(err, i18n.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	problems := i18n.FieldProblems(err)
	if len(problems) != 1 || problems[0].Field != "code" || problems[0].Code != i18n.FieldUnknown {
		t.Fatalf("field problems = %+v, want code/unknown", problems)
	}
}

// A validation failure carries the field and a stable code, and still matches ErrInvalidInput.
func TestInvalidFieldNamesTheFieldAndKeepsTheSentinel(t *testing.T) {
	err := i18n.ValidateKey("Not A Key")
	if !errors.Is(err, i18n.ErrInvalidInput) {
		t.Fatalf("ValidateKey error = %v, want ErrInvalidInput", err)
	}
	problems := i18n.FieldProblems(err)
	if len(problems) != 1 {
		t.Fatalf("field problems = %+v, want one", problems)
	}
	if problems[0].Field != "key" || problems[0].Code != i18n.FieldInvalid {
		t.Fatalf("problem = %+v, want key/invalid", problems[0])
	}
	if problems[0].Detail == "" {
		t.Fatal("the English explanation was dropped")
	}
}

// A refusal that cannot name a field has none, rather than a guessed one.
func TestRefusalWithoutAFieldHasNoFakeField(t *testing.T) {
	if got := i18n.FieldProblems(errors.New("something else")); got != nil {
		t.Fatalf("a plain error produced field problems: %+v", got)
	}
}

// The catalogue key for a code is derivable from the code the response carries.
func TestProblemKeyIsTheNamespacePlusTheCode(t *testing.T) {
	if got := i18n.ProblemKey("unknown_placeholder"); got != "error.unknown_placeholder" {
		t.Fatalf("ProblemKey = %q", got)
	}
}
