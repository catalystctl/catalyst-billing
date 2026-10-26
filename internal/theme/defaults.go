package theme

// The built-in default document.
//
// This is the "no theme configured" answer, and it is deliberately a byte-for-byte
// reproduction of the values in web/src/styles/tokens.css at the time the theme registry
// landed. That is the whole upgrade story: an installation that never opens the Theme Studio
// keeps serving exactly the stylesheet it served before, because the generated default CSS is
// the same declarations with the same values in the same order of precedence.
//
// # Why the default is the warm billing palette and not the game panel's console palette
//
// docs/theming-system.md §8 recommends `Catalyst Console` (the near-black/rose game-panel
// match) as the default for *new* installations, and preserving installed tokens on upgrade.
// This build does the second half — the default document below is the current product — and
// ships Console as a preset one click away (internal/theme/presets/catalyst-console.json).
//
// The reason is that "preserve installed tokens" and "a fresh install looks like the game
// panel" are the same code path here: there is no row to migrate, the default document *is*
// the fallback. Making it Console would silently restyle every existing installation on
// upgrade, which the same section forbids. When the product decision is confirmed the other
// way, it is a change to this file and to the documentation — not to the schema.
//
// # Why so many fields are filled in
//
// Everything that the generator can derive is left out (see css/generate.go); everything the
// current stylesheet hand-tuned is written down. A field that is present is a decision; a
// field that is absent is a derivation. Keeping the derivations out of this file is what makes
// it obvious which values are load-bearing.

// DefaultFontStackSans is the built-in body/display stack. It is a full stack rather than a
// single family because billing ships no self-hosted font: the system UI font is the design.
const DefaultFontStackSans = `ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, Roboto, "Helvetica Neue", Arial, sans-serif`

// DefaultFontStackMono is the built-in monospace stack.
const DefaultFontStackMono = `ui-monospace, "SF Mono", "JetBrains Mono", "Cascadia Code", Menlo, Consolas, "Liberation Mono", monospace`

// Default returns the built-in default document.
//
// It returns a fresh copy each call so a caller cannot mutate the process-wide default by
// accident — which is exactly the bug a package-level var would produce the first time a
// handler merged into it.
func Default() Document {
	return Document{
		Schema: SchemaVersion,
		ID:     "catalyst-billing",
		Name:   "Catalyst Billing",
		Color: Color{
			// `system`, not `dark`, and the distinction is not cosmetic.
			//
			// tokens.css puts the dark palette under `:root`, so with no `data-theme` attribute the
			// product paints dark — that is the CSS fallback, and it is why the dark block below is
			// the one the generator emits first. But what a *first visit* does is a different
			// question, and the answer the product has always given is "follow the OS": lib/theme.ts
			// reads prefers-color-scheme when nothing is stored. A theme default of `dark` silently
			// overrode that, so a light-preferring visitor got a dark panel.
			//
			// A browser found it: scripts/e2e-ui.mjs asserts the branding preview paints the
			// *light* accent, and it came back with the dark one. `system` keeps the old behaviour
			// and leaves `dark` available for a preset that genuinely wants a dark-first product.
			Mode: "system",
			Brand: BrandPalette{
				Primary: "#ff951e",
				Accent:  "#ff951e",
			},
			Semantic: Semantic{
				Success: "#11bd85",
				Warning: "#ff951e",
				Danger:  "#f45b64",
				Info:    "#318fe5",
			},
			Dark: ModePalette{
				Bg:      "#171413",
				Popover: "#1e1a18",
				Overlay: "rgba(8, 7, 6, 0.73)",

				Card:     "#1e1a18",
				Surface1: "#1e1a18",
				Surface2: "#201c1a",
				Surface3: "#29231f",

				Border:       "#37322e",
				BorderStrong: "#4a4139",
				Text:         "#e7e2de",
				TextMuted:    "#aaa29b",
				TextFaint:    "#807973",
				Input:        "#29231f",

				Side:          "#201c1a",
				Top:           "#181514",
				SurfaceHover:  "#29231f",
				SurfaceInset:  "#171413",
				SurfaceActive: "#3a302a",
				LineSoft:      "#302b28",
				TextSubtle:    "#67615c",
				TextInverse:   "#241407",
				Scrollbar:     "#5b534c",
				Pink:          "#ff4086",

				Accent:      "#ff951e",
				AccentHover: "#ffab4a",
				AccentPress: "#e98212",
				AccentInk:   "#241407",

				Success: "#11bd85",
				Warning: "#ff951e",
				Danger:  "#f45b64",
				Info:    "#318fe5",
			},
			Light: ModePalette{
				Bg:      "#f5f2ee",
				Popover: "#fdfaf6",
				Overlay: "rgba(43, 37, 31, 0.5)",

				Card:     "#fdfaf6",
				Surface1: "#fdfaf6",
				Surface2: "#f9f7f3",
				Surface3: "#f1eae3",

				Border:       "#d9d0c7",
				BorderStrong: "#c4b8ac",
				Text:         "#2b251f",
				TextMuted:    "#615950",
				TextFaint:    "#82796e",
				Input:        "#f1eae3",

				Side:          "#ebe6e0",
				Top:           "#f9f7f3",
				SurfaceHover:  "#f1eae3",
				SurfaceInset:  "#f5f2ee",
				SurfaceActive: "#f0ddce",
				LineSoft:      "#e5dcd3",
				TextSubtle:    "#9b9187",
				TextInverse:   "#ffffff",
				Scrollbar:     "#b8ada2",
				Pink:          "#e4346d",

				Accent:      "#d36c09",
				AccentHover: "#bc5f07",
				AccentPress: "#a95506",
				AccentInk:   "#ffffff",

				Success: "#11815e",
				Warning: "#d36c09",
				Danger:  "#bc424a",
				Info:    "#286dad",
			},
		},
		Typography: Typography{
			Display: FontSpec{Family: DefaultFontStackSans, Weights: []int{600, 700}},
			Body:    FontSpec{Family: DefaultFontStackSans, Weights: []int{400, 500}},
			Mono:    FontSpec{Family: DefaultFontStackMono, Weights: []int{400, 500}},
			Scale: TextScale{
				XS:   "0.6875rem",
				SM:   "0.75rem",
				Base: "0.8125rem",
				MD:   "0.875rem",
				LG:   "1rem",
				XL:   "1.125rem",
				XXL:  "1.5rem",
				XXXL: "2.25rem",
			},
			Leading:  Leading{Tight: "1.15", Snug: "1.35", Normal: "1.45"},
			Tracking: Tracking{Tight: "-0.015em", Wide: "0.06em"},
		},
		Space: Space{
			Unit:    4,
			Density: "compact",
			Scale:   []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4},
		},
		Shape: Shape{
			Radius: Radius{XS: "4px", SM: "4px", MD: "4px", LG: "4px", XL: "8px", Full: "999px"},
			// The current stylesheet relies on the browser default border width, so the
			// default document states it rather than leaving it to the generator's fallback:
			// a 2px default would thicken every existing hairline.
			BorderWidth: "1px",
		},
		Elevation: Elevation{
			Dark: Shadows{
				One:   "none",
				Two:   "0 12px 32px rgba(0, 0, 0, 0.28)",
				Three: "0 20px 70px rgba(0, 0, 0, 0.38)",
				Focus: "0 0 0 2px var(--bg), 0 0 0 4px var(--line-accent)",
			},
			Light: Shadows{
				One:   "none",
				Two:   "0 12px 32px rgba(54, 33, 18, 0.1)",
				Three: "0 20px 70px rgba(54, 33, 18, 0.14)",
				Focus: "0 0 0 2px var(--surface), 0 0 0 4px var(--line-accent)",
			},
		},
		Motion: Motion{
			Fast:   "90ms",
			Normal: "160ms",
			Slow:   "260ms",
			Ease:   "cubic-bezier(0.2, 0.8, 0.2, 1)",
		},
		Background: Background{Style: "flat"},
		Layout: Layout{Shell: Shell{
			Nav:          "sidebar",
			SidebarWidth: "242px",
			TopbarHeight: "55px",
			ContentMax:   "1900px",
			CardStyle:    "outlined",
			TableStyle:   "compact",
		}},
		Components: map[string]Component{},
	}
}
