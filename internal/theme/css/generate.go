// Package css turns a theme document into the stylesheet the browser loads.
//
// # The three vocabularies
//
// The generated CSS defines the same values under three sets of names, so nothing has to be
// rewritten at once and a preset can be authored in either product's vocabulary
// (docs/theming-system.md §7):
//
//  1. **Canonical** — `--color-bg`, `--color-surface-1`, `--space-4`, `--radius-md`. New code
//     uses these.
//  2. **Legacy billing** — `--bg`, `--surface`, `--line`, `--accent`, `--s-1…8`, `--r-xs…full`.
//     This is what web/src/styles already reads, and it is emitted as `var(--canonical)` so the
//     two can never disagree.
//  3. **Game panel** — the HSL-triplet names the game panel's own presets use (`--background`,
//     `--primary`, `--border`, `--radius`, …), emitted where they do not collide with a billing
//     name. Colliding names are listed in gamePanelAliases and deliberately skipped: `--accent`,
//     `--success`, `--warning`, `--danger`, `--info`, `--surface-2` and `--surface-3` are already
//     hex colours that billing CSS reads, and emitting an HSL triplet into them would break the
//     panel in a way that looks like a theming bug rather than a name collision.
//
// # Runtime precedence
//
//	built-in default ← theme revision  ←  lib/brand.ts accent overlay
//
// The theme is a stylesheet linked after the bundled one, so its `:root` and
// `[data-theme="light"]` declarations win on source order at equal specificity. The brand overlay
// is applied later still, by CSSOM `style.setProperty` on <html>, and it may set only the accent
// family (`--accent`, `--accent-ink`, `--accent-text`, `--accent-soft`, `--accent-soft-strong`).
// That narrow mechanism is existing, tested behaviour (web/src/lib/brand.ts, web/test/brand.test.mjs)
// and it is not a theme mechanism: it cannot change a surface, a radius or a layout.
//
// # Derived values
//
// A theme states seeds and this package derives the rest: hover/press/soft from the accent, the
// ink that reads on the accent, the skeleton gradient from two surfaces, the focus ring from the
// background and the accent. The built-in preset writes the derived values out anyway, which is
// what makes the default stylesheet identical to the hand-tuned tokens.css it replaces — and the
// golden test in generate_test.go is what keeps that true.
package css

import (
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/theme"
)

// modeTokens is one colour mode's emitted declarations, in order.
type modeTokens struct {
	name string // "dark" or "light"
	// scheme is the CSS color-scheme keyword.
	scheme string
	// dark selects the derivation direction for hover/press and the soft alpha.
	dark bool
	// palette is the document's palette for this mode.
	palette theme.ModePalette
}

// Generate renders the stylesheet for a resolved theme document.
//
// The document must be resolved (theme.ValidateResolved), because a missing canonical token would
// otherwise emit `--color-bg: ;` — a declaration that silently falls back to the browser default
// and makes the panel unreadable in a way nobody would connect to "the theme".
func Generate(doc theme.Document) ([]byte, error) {
	if err := theme.ValidateResolved(doc); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.Grow(16 << 10)

	fmt.Fprintf(&b, "/* %s — revision digest %s\n", commentSafe(doc.Name), shortDigest(doc))
	fmt.Fprintf(&b, " * Generated from a theme document. Do not edit: the next publish replaces this file.\n")
	fmt.Fprintf(&b, " * Scope-wide tokens are in :root; colours are per colour mode.\n */\n\n")

	writeShared(&b, doc)
	writeMode(&b, doc, modeTokens{name: "dark", scheme: "dark", dark: true, palette: doc.Color.Dark})
	writeMode(&b, doc, modeTokens{name: "light", scheme: "light", dark: false, palette: doc.Color.Light})
	writeStructure(&b, doc)

	if strings.TrimSpace(doc.CustomCSS) != "" {
		// Re-validated at serve time. The write path already checked it, but this file is what
		// the browser executes, and a row written by hand would otherwise reach it unchecked.
		if err := theme.ValidateCustomCSS(doc.CustomCSS); err != nil {
			return nil, err
		}
		b.WriteString("\n/* ---------------------------------------------------------------- operator CSS -- */\n")
		b.WriteString(strings.TrimSpace(doc.CustomCSS))
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// writeShared emits the mode-independent tokens.
func writeShared(b *strings.Builder, doc theme.Document) {
	b.WriteString(":root {\n")

	// Type.
	fmt.Fprintf(b, "  --font-display: %s;\n", fontStack(doc.Typography.Display))
	fmt.Fprintf(b, "  --font-sans: %s;\n", fontStack(doc.Typography.Body))
	fmt.Fprintf(b, "  --font-body: %s;\n", fontStack(doc.Typography.Body))
	fmt.Fprintf(b, "  --font-mono: %s;\n", fontStack(doc.Typography.Mono))

	scale := doc.Typography.Scale
	fmt.Fprintf(b, "  --text-xs: %s; --text-sm: %s; --text-base: %s; --text-md: %s;\n",
		scale.XS, scale.SM, scale.Base, scale.MD)
	fmt.Fprintf(b, "  --text-lg: %s; --text-xl: %s; --text-2xl: %s; --text-3xl: %s;\n",
		scale.LG, scale.XL, scale.XXL, scale.XXXL)

	fmt.Fprintf(b, "  --leading-tight: %s; --leading-snug: %s; --leading-normal: %s;\n",
		doc.Typography.Leading.Tight, doc.Typography.Leading.Snug, doc.Typography.Leading.Normal)
	fmt.Fprintf(b, "  --tracking-tight: %s; --tracking-wide: %s;\n",
		doc.Typography.Tracking.Tight, doc.Typography.Tracking.Wide)

	// Space. The scale is a list of rem values — `--s-1: 0.25rem` is the first step of the
	// built-in 4px grid — so it is emitted as written. `unit` is the grid size the scale is built
	// on, published as its own token for code that needs a multiple of the grid rather than a
	// step of the ladder.
	for i, step := range doc.Space.Scale {
		fmt.Fprintf(b, "  --space-%d: %srem;\n", i+1, trimFloat(step))
		fmt.Fprintf(b, "  --s-%d: var(--space-%d);\n", i+1, i+1)
	}
	fmt.Fprintf(b, "  --grid: %dpx;\n", doc.Space.Unit)
	fmt.Fprintf(b, "  --density: %s;\n", doc.Space.Density)
	switch doc.Space.Density {
	case "roomy":
		fmt.Fprintf(b, "  --gutter: var(--space-5);\n")
	case "comfortable":
		fmt.Fprintf(b, "  --gutter: var(--space-4);\n")
	default:
		fmt.Fprintf(b, "  --gutter: var(--space-3);\n")
	}

	// Shape.
	r := doc.Shape.Radius
	fmt.Fprintf(b, "  --radius-xs: %s; --radius-sm: %s; --radius-md: %s;\n", r.XS, r.SM, r.MD)
	fmt.Fprintf(b, "  --radius-lg: %s; --radius-xl: %s; --radius-full: %s;\n", r.LG, r.XL, r.Full)
	fmt.Fprintf(b, "  --r-xs: var(--radius-xs); --r-sm: var(--radius-sm); --r-md: var(--radius-md);\n")
	fmt.Fprintf(b, "  --r-lg: var(--radius-lg); --r-xl: var(--radius-xl); --r-full: var(--radius-full);\n")
	fmt.Fprintf(b, "  --border-width: %s;\n", doc.Shape.BorderWidth)
	// The game panel's single radius token, kept for preset interchange.
	fmt.Fprintf(b, "  --radius: %s;\n", r.MD)

	// Motion.
	m := doc.Motion
	fmt.Fprintf(b, "  --duration-fast: %s; --duration-normal: %s; --duration-slow: %s;\n", m.Fast, m.Normal, m.Slow)
	fmt.Fprintf(b, "  --dur-1: var(--duration-fast); --dur-2: var(--duration-normal); --dur-3: var(--duration-slow);\n")
	fmt.Fprintf(b, "  --ease: %s;\n", m.Ease)

	// Shell.
	sh := doc.Layout.Shell
	fmt.Fprintf(b, "  --sidebar-w: %s; --topbar-h: %s; --content-max: %s;\n",
		sh.SidebarWidth, sh.TopbarHeight, sh.ContentMax)

	// Components, when a theme overrides them.
	if c, ok := doc.Components["button"]; ok {
		writeLength(b, "--button-h", c.Height)
		writeLength(b, "--button-radius", c.Radius)
	}
	if c, ok := doc.Components["input"]; ok {
		writeLength(b, "--input-h", c.Height)
		writeLength(b, "--input-radius", c.Radius)
	}
	if c, ok := doc.Components["card"]; ok {
		writeLength(b, "--card-radius", c.Radius)
		writeLength(b, "--card-padding", c.Padding)
	}
	if c, ok := doc.Components["table"]; ok {
		writeLength(b, "--table-row-h", c.Height)
	}

	// The page treatment. `flat` is the default and emits nothing, so a theme that does not care
	// about backgrounds does not override the bundled stylesheet's own `--bg-grain`.
	switch doc.Background.Style {
	case "gradient", "mesh":
		if doc.Background.Gradient != "" {
			fmt.Fprintf(b, "  --bg-grain: %s;\n", doc.Background.Gradient)
		}
	case "image":
		if id := theme.AssetRefID(doc.Background.Image); id != "" {
			fmt.Fprintf(b, "  --bg-grain: url(\"/theme/assets/%s\");\n", id)
		}
	}

	b.WriteString("}\n\n")
}

// writeMode emits one colour mode.
func writeMode(b *strings.Builder, doc theme.Document, m modeTokens) {
	p := m.palette
	// The selector is the same shape tokens.css uses, so the cascade order between the two files
	// is the same in both modes: dark under `:root, [data-theme="dark"]`, light under
	// `[data-theme="light"]`, light later.
	if m.dark {
		b.WriteString(":root,\n[data-theme=\"dark\"] {\n")
	} else {
		b.WriteString("[data-theme=\"light\"] {\n")
	}
	fmt.Fprintf(b, "  color-scheme: %s;\n", m.scheme)

	accent := doc.AccentFor(m.name)
	primary := resolvePrimary(doc, accent)

	// ---- canonical ----
	fmt.Fprintf(b, "\n  /* canonical */\n")
	fmt.Fprintf(b, "  --color-bg: %s;\n", p.Bg)
	fmt.Fprintf(b, "  --color-bg-elevated: %s;\n", p.Card)
	fmt.Fprintf(b, "  --color-overlay: %s;\n", p.Overlay)
	fmt.Fprintf(b, "  --color-scrim: %s;\n", p.Overlay)
	fmt.Fprintf(b, "  --color-surface-1: %s;\n", p.Surface1)
	fmt.Fprintf(b, "  --color-surface-2: %s;\n", p.Surface2)
	fmt.Fprintf(b, "  --color-surface-3: %s;\n", p.Surface3)
	fmt.Fprintf(b, "  --color-surface-hover: %s;\n", orDefault(p.SurfaceHover, p.Surface2))
	fmt.Fprintf(b, "  --color-surface-inset: %s;\n", orDefault(p.SurfaceInset, p.Bg))
	fmt.Fprintf(b, "  --color-text: %s;\n", p.Text)
	fmt.Fprintf(b, "  --color-text-muted: %s;\n", p.TextMuted)
	fmt.Fprintf(b, "  --color-text-faint: %s;\n", p.TextFaint)
	fmt.Fprintf(b, "  --color-text-inverse: %s;\n", orDefault(p.TextInverse, inkOf(accent, "#ffffff")))
	fmt.Fprintf(b, "  --color-border: %s;\n", p.Border)
	fmt.Fprintf(b, "  --color-border-strong: %s;\n", p.BorderStrong)
	fmt.Fprintf(b, "  --color-focus-ring: %s;\n", orDefault(doc.Color.Semantic.Focus, accent))
	fmt.Fprintf(b, "  --color-primary: %s;\n", primary)
	fmt.Fprintf(b, "  --color-primary-hover: %s;\n", stateOf(primary, m.dark, 0.12))
	fmt.Fprintf(b, "  --color-primary-press: %s;\n", stateOf(primary, m.dark, 0.20))
	fmt.Fprintf(b, "  --color-on-primary: %s;\n", inkOf(primary, "#ffffff"))
	fmt.Fprintf(b, "  --color-primary-soft: %s;\n", tintOf(primary, softAlpha(m.dark), p.Bg))
	fmt.Fprintf(b, "  --color-secondary: %s;\n", orDefault(doc.Color.Brand.Secondary, p.BorderStrong))
	fmt.Fprintf(b, "  --color-accent: %s;\n", accent)
	for _, sem := range []struct {
		name  string
		value string
	}{
		{"success", orDefault(p.Success, doc.Color.Semantic.Success)},
		{"warning", orDefault(p.Warning, doc.Color.Semantic.Warning)},
		{"danger", orDefault(p.Danger, doc.Color.Semantic.Danger)},
		{"info", orDefault(p.Info, doc.Color.Semantic.Info)},
	} {
		fmt.Fprintf(b, "  --color-%s: %s;\n", sem.name, sem.value)
		fmt.Fprintf(b, "  --color-%s-soft: %s;\n", sem.name, tintOf(sem.value, softAlpha(m.dark), p.Bg))
	}

	// ---- legacy billing aliases ----
	//
	// Emitted as var(--canonical) so the two vocabularies cannot disagree. The values that have no
	// canonical twin (the chrome surfaces, the pink decorative accent) are emitted literally.
	fmt.Fprintf(b, "\n  /* legacy billing aliases */\n")
	fmt.Fprintf(b, "  --bg: var(--color-bg);\n")
	fmt.Fprintf(b, "  --side: %s;\n", orDefault(p.Side, p.Card))
	fmt.Fprintf(b, "  --top: %s;\n", orDefault(p.Top, p.Card))
	fmt.Fprintf(b, "  --surface: var(--color-surface-1);\n")
	fmt.Fprintf(b, "  --surface-2: var(--color-surface-2);\n")
	fmt.Fprintf(b, "  --surface-3: var(--color-surface-3);\n")
	fmt.Fprintf(b, "  --surface-active: %s;\n", orDefault(p.SurfaceActive, mixHexOr(p.Surface3, p.Text, 0.12)))
	fmt.Fprintf(b, "  --surface-hover: var(--color-surface-hover);\n")
	fmt.Fprintf(b, "  --surface-inset: var(--color-surface-inset);\n")
	fmt.Fprintf(b, "  --line: var(--color-border);\n")
	fmt.Fprintf(b, "  --line-soft: %s;\n", orDefault(p.LineSoft, mixHexOr(p.Border, p.Bg, 0.5)))
	fmt.Fprintf(b, "  --line-strong: var(--color-border-strong);\n")
	fmt.Fprintf(b, "  --line-accent: %s;\n", tintOf(accent, 0.30, p.Bg))
	fmt.Fprintf(b, "  --text: var(--color-text);\n")
	fmt.Fprintf(b, "  --text-dim: var(--color-text-muted);\n")
	fmt.Fprintf(b, "  --text-faint: var(--color-text-faint);\n")
	fmt.Fprintf(b, "  --text-subtle: %s;\n", orDefault(p.TextSubtle, mixHexOr(p.TextFaint, p.Bg, 0.3)))
	fmt.Fprintf(b, "  --text-inverse: var(--color-text-inverse);\n")
	fmt.Fprintf(b, "  --accent: %s;\n", accent)
	fmt.Fprintf(b, "  --accent-hover: %s;\n", orDefault(p.AccentHover, stateOf(accent, m.dark, 0.12)))
	fmt.Fprintf(b, "  --accent-press: %s;\n", orDefault(p.AccentPress, stateOf(accent, m.dark, 0.20)))
	fmt.Fprintf(b, "  --accent-ink: %s;\n", orDefault(p.AccentInk, inkOf(accent, "#ffffff")))
	// --accent-text is the accent as text. Its default is the accent itself, exactly as
	// tokens.css defines it, so an installation with no brand renders identically; lib/brand.ts
	// may override it with a readable relative of the tenant's colour.
	fmt.Fprintf(b, "  --accent-text: var(--accent);\n")
	fmt.Fprintf(b, "  --accent-soft: %s;\n", tintOf(accent, accentSoftAlpha(m.dark), p.Bg))
	fmt.Fprintf(b, "  --accent-soft-strong: %s;\n", tintOf(accent, 0.18, p.Bg))
	fmt.Fprintf(b, "  --pink: %s;\n", orDefault(p.Pink, orDefault(p.Danger, doc.Color.Semantic.Danger)))
	fmt.Fprintf(b, "  --success: var(--color-success);\n")
	fmt.Fprintf(b, "  --success-soft: var(--color-success-soft);\n")
	fmt.Fprintf(b, "  --warning: var(--color-warning);\n")
	fmt.Fprintf(b, "  --warning-soft: var(--color-warning-soft);\n")
	fmt.Fprintf(b, "  --danger: var(--color-danger);\n")
	fmt.Fprintf(b, "  --danger-soft: var(--color-danger-soft);\n")
	fmt.Fprintf(b, "  --info: var(--color-info);\n")
	fmt.Fprintf(b, "  --info-soft: var(--color-info-soft);\n")
	fmt.Fprintf(b, "  --overlay: var(--color-overlay);\n")
	fmt.Fprintf(b, "  --scrollbar: %s;\n", orDefault(p.Scrollbar, mixHexOr(p.Border, p.TextMuted, 0.5)))

	shadows := doc.Elevation.Dark
	if !m.dark {
		shadows = doc.Elevation.Light
	}
	fmt.Fprintf(b, "  --shadow-1: %s;\n", shadows.One)
	fmt.Fprintf(b, "  --shadow-2: %s;\n", shadows.Two)
	fmt.Fprintf(b, "  --shadow-3: %s;\n", shadows.Three)
	fmt.Fprintf(b, "  --shadow-focus: %s;\n",
		orDefault(shadows.Focus, "0 0 0 2px "+p.Bg+", 0 0 0 4px "+tintOf(accent, 0.30, p.Bg)))
	// A skeleton is two surfaces and a highlight; deriving it keeps a preset from having to
	// restate a gradient for each mode.
	fmt.Fprintf(b, "  --skeleton: linear-gradient(90deg, %s 0%%, %s 40%%, %s 80%%);\n",
		p.Surface2, p.Surface3, p.Surface2)

	// ---- game panel HSL aliases ----
	writeGamePanelAliases(b, p, accent, primary)

	b.WriteString("}\n\n")
}

// gamePanelCollisions are the game-panel names that are NOT emitted, because billing already
// uses them for a hex colour. Emitting an HSL triplet into `--accent` would turn every accented
// control in the panel into an invalid colour, and the failure would look like a theming bug
// rather than a naming collision.
var gamePanelCollisions = []string{
	"--accent", "--success", "--warning", "--danger", "--info", "--surface-2", "--surface-3",
}

// writeGamePanelAliases emits the HSL triplets the game panel's own presets expect.
//
// A colour that is not a hex triple (an `rgb()`, a `var()`) cannot be converted, so its alias is
// skipped rather than emitted wrong: a preset authored in the game panel's format is hex by
// construction, which is the case this exists for.
func writeGamePanelAliases(b *strings.Builder, p theme.ModePalette, accent, primary string) {
	var lines []string
	add := func(name, source, raw string) {
		if c, ok := parseHex(raw); ok {
			lines = append(lines, fmt.Sprintf("  %s: %s;", name, hslTriplet(c)))
			return
		}
		// Not a hex colour: reference the canonical token so the alias is still coherent.
		lines = append(lines, fmt.Sprintf("  %s: var(%s);", name, source))
	}
	add("--background", "--color-bg", p.Bg)
	add("--foreground", "--color-text", p.Text)
	add("--card", "--color-surface-1", p.Surface1)
	add("--card-foreground", "--color-text", p.Text)
	add("--popover", "--color-surface-1", p.Surface1)
	add("--primary", "--color-primary", primary)
	add("--primary-foreground", "--color-on-primary", orDefault(p.AccentInk, inkOf(accent, "#ffffff")))
	add("--secondary", "--color-secondary", orDefault(p.Surface2, p.Surface2))
	add("--muted", "--color-surface-2", p.Surface2)
	add("--muted-foreground", "--color-text-muted", p.TextMuted)
	add("--border", "--color-border", p.Border)
	add("--input", "--color-border", p.Input)
	add("--ring", "--color-focus-ring", accent)
	add("--surface-1", "--color-surface-1", p.Surface1)

	b.WriteString("\n  /* game panel aliases (colliding names are deliberately absent: " +
		strings.Join(gamePanelCollisions, " ") + ") */\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
}

// writeStructure emits the shell rules.
//
// These are the "a preset can change the shape of the panel, not only its colours" half of
// docs/theming-system.md §9.5. They are keyed off data attributes on <html> rather than a second
// stylesheet, so switching a preset is one attribute write and no new request.
func writeStructure(b *strings.Builder, doc theme.Document) {
	b.WriteString("/* ---------------------------------------------------------------- shell shape -- */\n")
	b.WriteString("[data-shell=\"sidebar\"] .sidebar { width: var(--sidebar-w); }\n")

	// The rail: a strip of icons. Widening it to 64px is not enough — the wordmark, the account name,
	// the section labels and every item's text label have to go, or they overflow a 64px column and
	// paint over each other. A browser is the only thing that shows you this: the stylesheet is valid,
	// the width is right, and the panel looks broken.
	//
	// The item label is a bare `<span>` inside `.nav-item` (ui/layout.ts), so it is selected by its
	// position rather than a class. That is deliberate: adding a class to every nav item to support one
	// preset would be a change to the shell for the benefit of a theme, and the shell is not the
	// theme's to change.
	b.WriteString("[data-shell=\"rail\"] .sidebar { width: 64px; }\n")
	b.WriteString("[data-shell=\"rail\"] .brand-name,\n" +
		"[data-shell=\"rail\"] .nav-label,\n" +
		"[data-shell=\"rail\"] .nav-item > span,\n" +
		"[data-shell=\"rail\"] .who-name,\n" +
		"[data-shell=\"rail\"] .collapse-btn { display: none; }\n")
	b.WriteString("[data-shell=\"rail\"] .sidebar-head,\n" +
		"[data-shell=\"rail\"] .nav-item,\n" +
		"[data-shell=\"rail\"] .who { justify-content: center; padding-inline: 0; }\n")
	b.WriteString("[data-shell=\"rail\"] .nav { overflow-x: hidden; }\n")
	// The footer's own text has to go for the same reason: the sign-out label and the version
	// wordmark are wider than 64px, and a sidebar that overflows its own column scrolls sideways
	// under a horizontal scrollbar instead of looking like a rail.
	b.WriteString("[data-shell=\"rail\"] .bottom-action,\n" +
		"[data-shell=\"rail\"] .version { display: none; }\n")
	b.WriteString("[data-shell=\"rail\"] .sidebar-foot { align-items: center; overflow: hidden; }\n")
	// The item is the hover target, and its `title` is what replaces the label — so a rail stays
	// navigable rather than becoming a column of unlabelled glyphs.
	b.WriteString("[data-shell=\"rail\"] .nav-item { min-height: 34px; }\n")

	b.WriteString("[data-shell=\"topbar\"] .sidebar { display: none; }\n")

	b.WriteString("[data-card-variant=\"flat\"] .card { box-shadow: none; border-color: transparent; }\n")
	b.WriteString("[data-card-variant=\"elevated\"] .card { box-shadow: var(--shadow-2); border-color: transparent; }\n")
	b.WriteString("[data-card-variant=\"outlined\"] .card { box-shadow: none; border-color: var(--color-border); }\n")

	b.WriteString("[data-table-variant=\"comfortable\"] table td, [data-table-variant=\"comfortable\"] table th " +
		"{ padding-top: var(--space-3); padding-bottom: var(--space-3); }\n")
	b.WriteString("[data-table-variant=\"compact\"] table td, [data-table-variant=\"compact\"] table th " +
		"{ padding-top: var(--space-2); padding-bottom: var(--space-2); }\n")

	// The background treatment. `paper` and `mesh` are additive rather than exclusive: an
	// operator who picks a gradient still wants the flat colour behind it while the gradient
	// paints.
	if doc.Background.Style == "image" {
		if id := theme.AssetRefID(doc.Background.Image); id != "" {
			fmt.Fprintf(b, "body { background-image: url(\"/theme/assets/%s\"); background-size: cover; "+
				"background-attachment: fixed; }\n", id)
		}
	}
	if doc.Background.Style == "gradient" && doc.Background.Gradient != "" {
		fmt.Fprintf(b, "body { background-image: %s; background-attachment: fixed; }\n", doc.Background.Gradient)
	}
	if doc.Background.Style == "paper" {
		// A paper treatment is a rule, not a texture: the editorial preset wants hairlines and
		// generous whitespace, not a bitmap.
		b.WriteString("body { background-image: none; }\n")
	}
}

// ---------------------------------------------------------------- derivations --

// resolvePrimary returns the primary colour for a mode.
func resolvePrimary(doc theme.Document, accent string) string {
	if doc.Color.Brand.Primary != "" {
		return doc.Color.Brand.Primary
	}
	return accent
}

// softAlpha is the alpha a semantic "soft" tint uses in a mode.
//
// It is heavier in dark mode because a tint over a near-black ground needs more of the colour to
// register; the light mode's 0.09 is the same perceived strength on paper.
func softAlpha(dark bool) float64 {
	if dark {
		return 0.11
	}
	return 0.09
}

// accentSoftAlpha is the accent's soft alpha. It is slightly lighter than the semantic one
// because the accent is used on larger surfaces (a selected row, an active tab).
func accentSoftAlpha(dark bool) float64 {
	if dark {
		return 0.10
	}
	return 0.11
}

// stateOf derives a hover/press state from a colour.
//
// In dark mode a state is lighter (there is headroom above), in light mode darker. If the colour
// is not a hex triple the colour itself is returned: a wrong-but-valid colour is better than an
// invalid declaration.
func stateOf(raw string, dark bool, amount float64) string {
	c, ok := parseHex(raw)
	if !ok {
		return raw
	}
	target := brand.Colour{R: 0, G: 0, B: 0}
	if dark {
		target = brand.Colour{R: 0xff, G: 0xff, B: 0xff}
	}
	return brand.Mix(c, target, amount).Hex()
}

// tintOf renders a colour as an rgba() tint over a background.
//
// The background is unused today — an rgba over a transparent declaration composites with
// whatever is behind it, which is what the existing stylesheet relies on — but it is kept in the
// signature because a future opaque-tint mode would need it, and because passing it documents
// that the caller thought about which surface the tint sits on.
func tintOf(raw string, alpha float64, _ string) string {
	c, ok := parseHex(raw)
	if !ok {
		return raw
	}
	return fmt.Sprintf("rgba(%d, %d, %d, %s)", c.R, c.G, c.B, trimFloat(alpha))
}

// inkOf returns the colour text takes on top of a background.
func inkOf(raw, fallback string) string {
	c, ok := parseHex(raw)
	if !ok {
		return fallback
	}
	return brand.Ink(c).Hex()
}

// mixHexOr blends two colours, or returns the first when either is not a hex triple.
func mixHexOr(a, b string, t float64) string {
	ca, ok1 := parseHex(a)
	cb, ok2 := parseHex(b)
	if !ok1 || !ok2 {
		return a
	}
	return brand.Mix(ca, cb, t).Hex()
}

// hslTriplet renders a colour as the `H S% L%` form the game panel's variables expect.
func hslTriplet(c brand.Colour) string {
	h, s, l := rgbToHSL(c)
	return fmt.Sprintf("%s %s%% %s%%", trimFloat(round1(h)), trimFloat(round1(s*100)), trimFloat(round1(l*100)))
}

// rgbToHSL converts an sRGB triple to degrees and fractions.
func rgbToHSL(c brand.Colour) (h, s, l float64) {
	r := float64(c.R) / 255
	g := float64(c.G) / 255
	b := float64(c.B) / 255
	max := r
	if g > max {
		max = g
	}
	if b > max {
		max = b
	}
	min := r
	if g < min {
		min = g
	}
	if b < min {
		min = b
	}
	l = (max + min) / 2
	if max == min {
		return 0, 0, l
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60, s, l
}

// round1 rounds to one decimal place.
func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// parseHex parses a hex colour, reporting false for anything else.
func parseHex(raw string) (brand.Colour, bool) {
	c, err := brand.ParseHex(strings.TrimSpace(raw))
	if err != nil {
		return brand.Colour{}, false
	}
	return c, true
}

// fontStack renders a family and its fallback as one CSS value.
func fontStack(spec theme.FontSpec) string {
	family := strings.TrimSpace(spec.Family)
	fallback := strings.TrimSpace(spec.Fallback)
	if fallback == "" {
		return family
	}
	return family + ", " + fallback
}

// writeLength emits a custom property when the value is present.
func writeLength(b *strings.Builder, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fmt.Fprintf(b, "  %s: %s;\n", name, value)
}

// orDefault returns value when it is set, otherwise fallback.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// trimFloat renders a float without a trailing zero or a needless ".0".
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// commentSafe strips the characters that would end a CSS comment.
func commentSafe(s string) string {
	s = strings.ReplaceAll(s, "*/", "")
	s = strings.ReplaceAll(s, "/*", "")
	return s
}

// shortDigest is the first 12 hex characters of the document digest, for the file header.
func shortDigest(doc theme.Document) string {
	d, err := theme.Digest(doc)
	if err != nil {
		return "unavailable"
	}
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// BootScript returns the pre-paint script a revision's boot.js serves.
//
// # Why this is a file and not an inline script
//
// The panel's CSP is `script-src 'self'` with no 'unsafe-inline'. An inline boot script would need
// a nonce or a hash, and a nonce is a per-response token the shell would have to carry — a much
// larger change than the problem warrants. A same-origin script file needs nothing: the policy
// already permits it.
//
// # Why it must run before the stylesheet paints
//
// The shell's <html data-theme> is the theme's default mode. A returning user who chose the other
// mode would see the default mode flash for one frame if the choice were applied by the module
// bundle, which loads after the stylesheet. This file is a classic script in <head>, so it runs
// before the first paint.
//
// It is deliberately tiny and total: every path through it ends with data-theme set, including
// the ones where storage throws (a blocked cookie jar, a private window).
func BootScript(doc theme.Document) []byte {
	defaultMode := doc.Color.Mode
	if defaultMode == "" {
		defaultMode = "dark"
	}
	// The mode is baked in as a literal, so the script needs no fetch and cannot race the shell.
	script := fmt.Sprintf(`/* theme boot — sets the colour mode before first paint. */
(function () {
  var root = document.documentElement;
  var fallback = %q;
  var stored = null;
  try { stored = window.localStorage.getItem("catalyst.theme"); } catch (e) { stored = null; }
  var mode;
  if (stored === "dark" || stored === "light") {
    mode = stored;
  } else if (fallback === "system") {
    var prefersLight = false;
    try {
      prefersLight = window.matchMedia("(prefers-color-scheme: light)").matches;
    } catch (e) { prefersLight = false; }
    mode = prefersLight ? "light" : "dark";
  } else {
    mode = fallback;
  }
  root.setAttribute("data-theme", mode);
  root.setAttribute("data-shell", %q);
  root.setAttribute("data-density", %q);
  root.setAttribute("data-card-variant", %q);
  root.setAttribute("data-table-variant", %q);
})();
`, defaultMode,
		orDefault(doc.Layout.Shell.Nav, "sidebar"),
		orDefault(doc.Space.Density, "compact"),
		orDefault(doc.Layout.Shell.CardStyle, "outlined"),
		orDefault(doc.Layout.Shell.TableStyle, "compact"))
	return []byte(script)
}

// ThemeColor returns the value for <meta name="theme-color">.
//
// For a `system` theme it is the dark ground, because that is what the generated `:root` block paints
// and what a cold visit shows until the boot script has decided. Returning the light colour there
// would give a dark page a light browser chrome for one frame.
func ThemeColor(doc theme.Document) string {
	if doc.Color.Mode == "light" {
		return doc.Color.Light.Bg
	}
	return doc.Color.Dark.Bg
}

// ColorScheme returns the CSS color-scheme keyword a cold visit gets.
//
// `system` resolves to `dark` for the same reason as ThemeColor: it is the `:root` block. The
// per-mode blocks still declare their own `color-scheme`, so once the boot script has set
// `data-theme` the browser is told the right one.
func ColorScheme(doc theme.Document) string {
	if doc.Color.Mode == "light" {
		return "light"
	}
	return "dark"
}
