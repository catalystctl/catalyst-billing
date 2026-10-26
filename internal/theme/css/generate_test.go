package css_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/css"
)

// The two generated colour blocks. The dark block also matches :root, so a token declared
// there is the default for a cold visit.
const (
	darkBlock  = ":root,\n[data-theme=\"dark\"]"
	lightBlock = "[data-theme=\"light\"]"
)

// declaration pulls one custom property out of a rendered block.
//
// The generator is tested by reading its output as a browser would — a declaration that is
// present, with the right value, in the right block — rather than by asserting on Go values it
// derived internally. A test on the internal value would pass even if the value never reached the
// stylesheet.
func declaration(t *testing.T, sheet, block, name string) string {
	t.Helper()
	body, ok := blockOf(sheet, block)
	if !ok {
		t.Fatalf("block %q not found in:\n%s", block, sheet)
	}
	re := regexp.MustCompile(`(?:^|[;\n])\s*` + regexp.QuoteMeta(name) + `:\s*([^;]+);`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s is not declared in %q:\n%s", name, block, body)
	}
	return strings.TrimSpace(m[1])
}

// blockOf returns the declarations of the first rule whose selector matches.
func blockOf(sheet, selector string) (string, bool) {
	idx := strings.Index(sheet, selector+" {")
	if idx < 0 {
		return "", false
	}
	rest := sheet[idx+len(selector)+2:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// The default document must reproduce the values the panel ships today.
//
// This is the upgrade guarantee: an installation that never opens the Theme Studio keeps the
// stylesheet it had, because the generated default is the same declarations. Each pair below is
// copied from web/src/styles/tokens.css; if a token there changes, this test is what notices that
// the built-in theme no longer matches it.
func TestDefaultReproducesTheShippedTokens(t *testing.T) {
	doc := theme.Default()
	if err := theme.ValidateResolved(doc); err != nil {
		t.Fatalf("the built-in default is not a valid resolved document: %v", err)
	}
	sheet, err := css.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)

	dark := []struct{ name, want string }{
		{"--bg", "var(--color-bg)"},
		{"--color-bg", "#171413"},
		{"--side", "#201c1a"},
		{"--top", "#181514"},
		{"--color-surface-1", "#1e1a18"},
		{"--color-surface-2", "#201c1a"},
		{"--color-surface-3", "#29231f"},
		{"--surface-active", "#3a302a"},
		{"--line", "var(--color-border)"},
		{"--color-border", "#37322e"},
		{"--line-soft", "#302b28"},
		{"--color-border-strong", "#4a4139"},
		{"--color-text", "#e7e2de"},
		{"--color-text-muted", "#aaa29b"},
		{"--color-text-faint", "#807973"},
		{"--text-subtle", "#67615c"},
		{"--accent", "#ff951e"},
		{"--accent-hover", "#ffab4a"},
		{"--accent-press", "#e98212"},
		{"--accent-ink", "#241407"},
		{"--accent-text", "var(--accent)"},
		{"--pink", "#ff4086"},
		{"--color-success", "#11bd85"},
		{"--color-warning", "#ff951e"},
		{"--color-danger", "#f45b64"},
		{"--color-info", "#318fe5"},
		{"--color-overlay", "rgba(8, 7, 6, 0.73)"},
		{"--scrollbar", "#5b534c"},
		{"--shadow-1", "none"},
		{"--shadow-2", "0 12px 32px rgba(0, 0, 0, 0.28)"},
		{"--shadow-3", "0 20px 70px rgba(0, 0, 0, 0.38)"},
	}
	for _, c := range dark {
		if got := declaration(t, out, darkBlock, c.name); got != c.want {
			t.Errorf("dark %s = %q, want %q", c.name, got, c.want)
		}
	}

	light := []struct{ name, want string }{
		{"--color-bg", "#f5f2ee"},
		{"--side", "#ebe6e0"},
		{"--top", "#f9f7f3"},
		{"--color-surface-1", "#fdfaf6"},
		{"--surface-active", "#f0ddce"},
		{"--color-border", "#d9d0c7"},
		{"--color-border-strong", "#c4b8ac"},
		{"--color-text", "#2b251f"},
		{"--accent", "#d36c09"},
		{"--accent-hover", "#bc5f07"},
		{"--accent-press", "#a95506"},
		{"--accent-ink", "#ffffff"},
		{"--color-success", "#11815e"},
		{"--color-overlay", "rgba(43, 37, 31, 0.5)"},
		{"--shadow-2", "0 12px 32px rgba(54, 33, 18, 0.1)"},
	}
	for _, c := range light {
		if got := declaration(t, out, lightBlock, c.name); got != c.want {
			t.Errorf("light %s = %q, want %q", c.name, got, c.want)
		}
	}

	shared := []struct{ name, want string }{
		{"--text-base", "0.8125rem"},
		{"--text-3xl", "2.25rem"},
		{"--space-1", "0.25rem"},
		{"--space-8", "4rem"},
		{"--radius-sm", "4px"},
		{"--radius-full", "999px"},
		{"--sidebar-w", "242px"},
		{"--topbar-h", "55px"},
		{"--content-max", "1900px"},
		{"--ease", "cubic-bezier(0.2, 0.8, 0.2, 1)"},
		{"--duration-normal", "160ms"},
	}
	for _, c := range shared {
		if got := declaration(t, out, ":root", c.name); got != c.want {
			t.Errorf("shared %s = %q, want %q", c.name, got, c.want)
		}
	}
}

// The legacy aliases must reference the canonical tokens, not repeat their values.
//
// If they repeated the value, changing a canonical token would leave the legacy name stale, and
// the two vocabularies would drift silently — which is exactly what the alias layer exists to
// prevent.
func TestLegacyAliasesReferenceCanonicalTokens(t *testing.T) {
	sheet, err := css.Generate(theme.Default())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)
	for legacy, canonical := range map[string]string{
		"--bg":          "--color-bg",
		"--surface":     "--color-surface-1",
		"--line":        "--color-border",
		"--text":        "--color-text",
		"--text-dim":    "--color-text-muted",
		"--overlay":     "--color-overlay",
		"--success":     "--color-success",
		"--warning":     "--color-warning",
		"--danger":      "--color-danger",
		"--info":        "--color-info",
		"--shadow-2":    "",
		"--line-strong": "--color-border-strong",
	} {
		if canonical == "" {
			continue
		}
		if got := declaration(t, out, darkBlock, legacy); got != "var("+canonical+")" {
			t.Errorf("%s = %q, want var(%s)", legacy, got, canonical)
		}
	}
}

// A game-panel name that billing already uses for a hex colour must not be overwritten with an
// HSL triplet. Emitting `--accent: 340 100% 62%` would turn every accented control into an
// invalid colour, and the failure would look like a theming bug rather than a name collision.
func TestGamePanelAliasesSkipCollidingNames(t *testing.T) {
	sheet, err := css.Generate(theme.Default())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)
	// A non-colliding alias is emitted as the game panel's HSL triplet.
	if got := declaration(t, out, darkBlock, "--background"); !strings.Contains(got, "%") {
		t.Errorf("--background = %q, want an HSL triplet", got)
	}
	// The colliding ones keep their billing meaning.
	for name, want := range map[string]string{
		"--accent":    "#ff951e",
		"--success":   "var(--color-success)",
		"--surface-2": "var(--color-surface-2)",
	} {
		if got := declaration(t, out, darkBlock, name); got != want {
			t.Errorf("%s = %q, want %q (a colliding game-panel name must not win)", name, got, want)
		}
	}
	// The whole collision list is documented in the stylesheet, so an operator reading the file
	// can see why the names are missing.
	for _, name := range []string{"--accent", "--surface-3"} {
		if !strings.Contains(out, name) {
			t.Errorf("the collision note does not mention %s", name)
		}
	}
}

// An unresolved document must be refused, not rendered with empty declarations.
func TestGenerateRefusesAnIncompleteDocument(t *testing.T) {
	doc := theme.Default()
	doc.Color.Light.Bg = ""
	if _, err := css.Generate(doc); err == nil {
		t.Fatal("Generate accepted a document with no light background")
	}
}

// The boot script must set every data attribute the generated structure rules key off, and it
// must be a classic script (no module syntax) so it can run in <head> before first paint.
func TestBootScriptSetsTheShellAttributes(t *testing.T) {
	doc := theme.Default()
	// The built-in default is `system`, which is what makes a first visit follow the OS. Setting it
	// explicitly here as well keeps the test independent of that choice.
	doc.Color.Mode = "system"
	doc.Layout.Shell.Nav = "rail"
	doc.Space.Density = "roomy"
	script := string(css.BootScript(doc))

	for _, want := range []string{
		`"system"`,
		`data-theme`,
		`data-shell`,
		`"rail"`,
		`data-density`,
		`"roomy"`,
		`localStorage.getItem("catalyst.theme")`,
		`matchMedia("(prefers-color-scheme: light)")`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("boot script is missing %q:\n%s", want, script)
		}
	}
	for _, forbidden := range []string{"import ", "export ", "await "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("boot script contains %q, which a classic script cannot use", forbidden)
		}
	}
	// The script must be small: it is on the critical path before first paint.
	if len(script) > 2048 {
		t.Errorf("boot script is %d bytes; it should stay under 2 KiB", len(script))
	}
}

// Custom CSS is appended verbatim (it was validated on write) and after the generated tokens, so
// an operator's override wins on source order.
func TestCustomCSSIsAppendedAfterTheTokens(t *testing.T) {
	doc := theme.Default()
	doc.CustomCSS = ".card { border-radius: 0; }"
	sheet, err := css.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)
	if !strings.Contains(out, ".card { border-radius: 0; }") {
		t.Fatal("custom CSS is missing from the generated stylesheet")
	}
	if strings.Index(out, "--color-bg: #171413") > strings.Index(out, ".card { border-radius: 0; }") {
		t.Fatal("custom CSS was emitted before the tokens; it would be overridden by them")
	}
}

// A shell shape from a preset must reach the stylesheet as a rule, not just a variable: a preset
// that changes the nav has to change the layout.
func TestShellShapeRulesFollowTheDocument(t *testing.T) {
	doc := theme.Default()
	doc.Layout.Shell.Nav = "rail"
	doc.Layout.Shell.CardStyle = "elevated"
	doc.Layout.Shell.TableStyle = "comfortable"
	doc.Space.Density = "roomy"
	sheet, err := css.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)
	for _, want := range []string{
		`[data-shell="rail"] .sidebar`,
		`[data-card-variant="elevated"] .card`,
		`[data-table-variant="comfortable"] table td`,
		`--density: roomy`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated stylesheet is missing %q", want)
		}
	}
}

// A background image must be referenced through the asset route, never by an arbitrary URL.
func TestBackgroundImageUsesTheAssetRoute(t *testing.T) {
	doc := theme.Default()
	doc.Background = theme.Background{
		Style: "image",
		Image: theme.AssetRef("0f0e0d0c-0b0a-4908-8706-050403020100"),
	}
	sheet, err := css.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(sheet)
	if !strings.Contains(out, `/theme/assets/0f0e0d0c-0b0a-4908-8706-050403020100`) {
		t.Fatalf("the background image is not served from the asset route:\n%s", out)
	}
	if strings.Contains(out, "https://") || strings.Contains(out, "http://") {
		t.Fatal("the generated stylesheet references an external origin")
	}
}
