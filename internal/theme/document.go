// Package theme is the versioned presentation document: the thing an operator edits in the
// Theme Studio and the thing the server turns into a stylesheet.
//
// # Why a document and not a settings group
//
// `internal/brand` is a handful of values in `settings`, because a name, a logo and two
// accents are a handful of values. A theme is not: it is a colour space (two modes), a type
// scale, a space scale, a shape scale, elevation, motion, a background treatment, shell
// dimensions and a block of validated custom CSS — and every one of those is a different kind
// of value with a different validation rule. Storing that in `(group_key, key, value)` would
// mean a row per token, a code change per token, and no way to say "this theme, as a whole, is
// the thing that was published".
//
// So a theme is one JSON document, versioned, immutable once published, and resolved once
// (see merge.go) so that a published theme cannot change under a page that is reading it.
//
// # What this package deliberately does not model
//
// Tenant identity. The display name, the logo and the per-mode accent stay in `branding.*`
// (`internal/brand`, migration 0060), mail branding stays in `mail.brand_*` (0068) and the
// invoice template stays in `invoices.pdf_template`. Those are existing, tested contracts with
// renderers that read them directly. A theme carries *presentation tokens*; a second copy of
// the tenant's name inside a theme would be a second source of truth that drifts the first
// time an operator renames themselves from `/branding`.
//
// The one place they meet is the runtime: `lib/brand.ts` writes the accent family on top of
// whatever the theme generated. The precedence is documented in css/generate.go.
//
// # Every field is optional, and that is the inheritance mechanism
//
// A document that omits `typography` inherits the parent's (see merge.go). This is what lets a
// preset be forty lines instead of four hundred, and it is what lets a scope override one
// token without restating the rest.
//
// Every JSON field therefore carries `omitempty`. That is not cosmetic: the merge works by
// marshalling each layer to a map and folding them together, so a field that marshalled as
// `""` would *override* its parent with an empty value instead of inheriting it. The one
// consequence to know about is that a value cannot be cleared to empty on purpose — clearing
// means inheriting, which is the only thing it could sensibly mean.
package theme

// SchemaVersion is the document schema this build reads and writes.
//
// It is checked, not assumed: a document declaring a different schema is refused rather than
// half-applied, because a field this build does not know about is a field it would silently
// drop — and the operator would publish a theme that lost their work.
const SchemaVersion = 2

// Document is one theme revision's presentation.
//
// The JSON keys are the operator-facing schema and are part of the import/export contract.
// Renaming one is a schema bump.
type Document struct {
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Extends names a preset (or a parent theme id) this document starts from. The resolver
	// applies `extends`, then the document itself on top.
	Extends string `json:"extends,omitempty"`
	// Inherit is the escape hatch from scope inheritance: `false` means "start from the
	// built-in default, not from my parent scope". Absent (nil) means inherit, which is the
	// common case and the reason a portal theme does not have to restate the panel's palette.
	Inherit *bool `json:"inherit,omitempty"`

	Color      Color                `json:"color"`
	Typography Typography           `json:"typography"`
	Space      Space                `json:"space"`
	Shape      Shape                `json:"shape"`
	Elevation  Elevation            `json:"elevation"`
	Motion     Motion               `json:"motion"`
	Background Background           `json:"background"`
	Layout     Layout               `json:"layout"`
	Components map[string]Component `json:"components,omitempty"`
	CustomCSS  string               `json:"customCss,omitempty"`
	// Annotations is free-form provenance an import can carry (the preset it came from, the
	// operator's note). It is not rendered and is not part of the token contract.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Color is the palette: one brand family, five semantic colours, and one surface set per
// colour mode.
//
// The two mode sets are separate values rather than one value with an opacity, because a panel
// and a piece of paper are different colour spaces: the accent that reads on #171413 is not
// the accent that reads on #f5f2ee, and the surfaces are not a tint of each other.
type Color struct {
	// Mode is the default mode a first visit uses: dark, light or system.
	Mode     string       `json:"mode,omitempty"`
	Brand    BrandPalette `json:"brand"`
	Semantic Semantic     `json:"semantic"`
	Dark     ModePalette  `json:"dark"`
	Light    ModePalette  `json:"light"`
}

// BrandPalette is the three brand colours.
//
// Primary and accent are both emitted because the game panel's schema has both and an operator
// copying a theme between the two products should not have to learn that one of them is
// ignored. Secondary is the quieter companion the game panel also carries.
type BrandPalette struct {
	Primary   string `json:"primary,omitempty"`
	Secondary string `json:"secondary,omitempty"`
	Accent    string `json:"accent,omitempty"`
	// Hover and Press were considered here and rejected: they are not mode-independent. The
	// dark accent's hover is a lighter tint and the light accent's is a darker one, so a single
	// pair of values would be wrong in one of the two modes. The per-mode overrides live on
	// ModePalette instead (Accent, AccentHover, AccentPress, AccentInk).
}

// Semantic is the four signal colours plus the focus ring.
//
// They only ever mean success, warning, danger or information: in a billing panel colour is a
// signal, and a decorative green would make "money arrived" ambiguous.
type Semantic struct {
	Success string `json:"success,omitempty"`
	Warning string `json:"warning,omitempty"`
	Danger  string `json:"danger,omitempty"`
	Info    string `json:"info,omitempty"`
	Focus   string `json:"focus,omitempty"`
}

// ModePalette is one colour mode's surfaces.
//
// The first thirteen fields are the canonical contract (docs/theming-system.md §7.1). The
// fields after them are the *legacy* vocabulary tokens.css already defines, kept as optional
// overrides so that the built-in preset can reproduce today's stylesheet byte for byte while a
// new preset only has to state the canonical set — the rest is derived.
type ModePalette struct {
	// Ground.
	Bg      string `json:"bg,omitempty"`
	Popover string `json:"popover,omitempty"`
	Overlay string `json:"overlay,omitempty"`

	// Surfaces.
	Card     string `json:"card,omitempty"`
	Surface1 string `json:"surface1,omitempty"`
	Surface2 string `json:"surface2,omitempty"`
	Surface3 string `json:"surface3,omitempty"`

	// Lines and text.
	Border       string `json:"border,omitempty"`
	BorderStrong string `json:"borderStrong,omitempty"`
	Text         string `json:"text,omitempty"`
	TextMuted    string `json:"textMuted,omitempty"`
	TextFaint    string `json:"textFaint,omitempty"`
	Input        string `json:"input,omitempty"`

	// The accent family for this mode.
	//
	// These exist because the accent is the one colour that genuinely has two correct values:
	// #ff951e reads on #171413 and disappears on #f5f2ee, so the light mode carries #d36c09
	// instead — a different colour with the same intent. A preset that states only
	// color.brand.primary gets these derived (lighter hover in dark mode, darker in light); the
	// built-in preset writes them out so it reproduces today's stylesheet exactly.
	Accent      string `json:"accent,omitempty"`
	AccentHover string `json:"accentHover,omitempty"`
	AccentPress string `json:"accentPress,omitempty"`
	AccentInk   string `json:"accentInk,omitempty"`

	// The four signal colours, per mode.
	//
	// They are per-mode for the same reason the accent is: #11bd85 reads on #171413 and is a
	// washed-out mint on #f5f2ee, so the light mode carries the darker #11815e. A preset that
	// states only color.semantic gets one value in both modes, which is the right default for a
	// colour chosen to work on a mid-tone ground.
	Success string `json:"success,omitempty"`
	Warning string `json:"warning,omitempty"`
	Danger  string `json:"danger,omitempty"`
	Info    string `json:"info,omitempty"`

	// Legacy vocabulary from tokens.css. Optional: an absent value is derived from the canonical
	// set, so a preset authored today does not have to know these exist.
	Side          string `json:"side,omitempty"`
	Top           string `json:"top,omitempty"`
	SurfaceHover  string `json:"surfaceHover,omitempty"`
	SurfaceInset  string `json:"surfaceInset,omitempty"`
	SurfaceActive string `json:"surfaceActive,omitempty"`
	LineSoft      string `json:"lineSoft,omitempty"`
	TextSubtle    string `json:"textSubtle,omitempty"`
	TextInverse   string `json:"textInverse,omitempty"`
	Scrollbar     string `json:"scrollbar,omitempty"`
	Pink          string `json:"pink,omitempty"`
}

// Typography is the type system: three families, a scale, leading and tracking.
type Typography struct {
	Display  FontSpec  `json:"display"`
	Body     FontSpec  `json:"body"`
	Mono     FontSpec  `json:"mono"`
	Scale    TextScale `json:"scale"`
	Leading  Leading   `json:"leading"`
	Tracking Tracking  `json:"tracking"`
}

// FontSpec is one family.
//
// Family is either a family from the installed set (see InstalledFamilies) or a complete CSS
// font stack such as the built-in `ui-sans-serif, -apple-system, …`. Both are validated as a
// font stack, so a value that would terminate the declaration early — or smuggle a `url()` in
// — is refused rather than emitted into the stylesheet.
type FontSpec struct {
	Family   string `json:"family,omitempty"`
	Fallback string `json:"fallback,omitempty"`
	// Weights is the set of weights the theme uses. It is what the generator reads when it
	// decides which @font-face declarations a self-hosted family needs.
	Weights []int `json:"weights,omitempty"`
}

// Leading is the three line-heights.
type Leading struct {
	Tight  string `json:"tight,omitempty"`
	Snug   string `json:"snug,omitempty"`
	Normal string `json:"normal,omitempty"`
}

// Tracking is the two letter-spacing steps.
type Tracking struct {
	Tight string `json:"tight,omitempty"`
	Wide  string `json:"wide,omitempty"`
}

// TextScale is the eight-step type scale.
type TextScale struct {
	XS   string `json:"xs,omitempty"`
	SM   string `json:"sm,omitempty"`
	Base string `json:"base,omitempty"`
	MD   string `json:"md,omitempty"`
	LG   string `json:"lg,omitempty"`
	XL   string `json:"xl,omitempty"`
	XXL  string `json:"2xl,omitempty"`
	XXXL string `json:"3xl,omitempty"`
}

// Space is the spacing system. Density changes the multiplier the shell uses; Scale is the
// eight-step ladder itself.
type Space struct {
	// Unit is the base grid in pixels. 4 is the billing default.
	Unit    int       `json:"unit,omitempty"`
	Density string    `json:"density,omitempty"`
	Scale   []float64 `json:"scale,omitempty"`
}

// Shape is radius and border width.
type Shape struct {
	Radius      Radius `json:"radius"`
	BorderWidth string `json:"borderWidth,omitempty"`
}

// Radius is the corner scale. Full is the pill radius and is not subject to the 64px bound.
type Radius struct {
	XS   string `json:"xs,omitempty"`
	SM   string `json:"sm,omitempty"`
	MD   string `json:"md,omitempty"`
	LG   string `json:"lg,omitempty"`
	XL   string `json:"xl,omitempty"`
	Full string `json:"full,omitempty"`
}

// Elevation is the shadow scale, per colour mode.
//
// It is per-mode because a shadow is not a tint: a 28%-black drop shadow that reads on
// #171413 is a bruise on #f5f2ee, and the light mode's shadows are warmer and lighter. One
// shared value would be wrong in one of the two modes, which is the same reasoning that gives
// the brand one accent per mode.
type Elevation struct {
	Dark  Shadows `json:"dark"`
	Light Shadows `json:"light"`
}

// Shadows is one mode's elevation levels plus the focus ring.
type Shadows struct {
	One   string `json:"1,omitempty"`
	Two   string `json:"2,omitempty"`
	Three string `json:"3,omitempty"`
	// Focus is the ring a focused control draws. It is a shadow rather than an outline so that
	// it follows the control's radius; when absent it is derived from the mode's background and
	// the accent (see css/generate.go).
	Focus string `json:"focus,omitempty"`
}

// Motion is duration and easing.
type Motion struct {
	Fast   string `json:"fast,omitempty"`
	Normal string `json:"normal,omitempty"`
	Slow   string `json:"slow,omitempty"`
	Ease   string `json:"ease,omitempty"`
}

// Background is the page treatment.
//
// Style is the enum that decides what the generator emits; Image and Gradient are the optional
// paint. Image is an asset reference (`theme-asset:<uuid>`), never a remote URL: a theme that
// could name an arbitrary origin would be a theme that leaks a request to it.
type Background struct {
	Style    string  `json:"style,omitempty"`
	Image    string  `json:"image,omitempty"`
	Gradient string  `json:"gradient,omitempty"`
	Noise    float64 `json:"noise,omitempty"`
}

// Layout is the shell.
type Layout struct {
	Shell Shell `json:"shell"`
	// Pages is reserved for the storefront composer (docs/theming-system.md §9). This build
	// refuses a non-empty value with a specific error rather than accepting it and rendering the
	// classic storefront anyway, which would look like the operator's edit was lost.
	Pages map[string]any `json:"pages,omitempty"`
}

// Shell is the panel/portal shell shape. These are CSS variables plus one data attribute, not a
// second stylesheet, which is what lets a preset change the *shape* of the panel.
type Shell struct {
	Nav            string `json:"nav,omitempty"` // sidebar | rail | topbar
	SidebarWidth   string `json:"sidebarWidth,omitempty"`
	TopbarHeight   string `json:"topbarHeight,omitempty"`
	ContentMax     string `json:"contentMax,omitempty"`
	CardStyle      string `json:"cardStyle,omitempty"`  // flat | outlined | elevated
	TableStyle     string `json:"tableStyle,omitempty"` // compact | comfortable
	CommandPalette bool   `json:"commandPalette,omitempty"`
	Breadcrumbs    bool   `json:"breadcrumbs,omitempty"`
}

// Component is a per-component override. The set of names is closed by validate.go: a typo such
// as "buton" must fail rather than be silently ignored.
type Component struct {
	Height    string `json:"height,omitempty"`
	Radius    string `json:"radius,omitempty"`
	Padding   string `json:"padding,omitempty"`
	Uppercase bool   `json:"uppercase,omitempty"`
}

// Clone returns a deep copy. The document is passed between the validator, the resolver and the
// CSS generator, and a shallow copy would let one of them mutate what another is reading.
func (d Document) Clone() Document {
	out := d
	out.Typography = d.Typography.Clone()
	out.Space = d.Space.Clone()
	if d.Components != nil {
		out.Components = make(map[string]Component, len(d.Components))
		for k, v := range d.Components {
			out.Components[k] = v
		}
	}
	if d.Annotations != nil {
		out.Annotations = make(map[string]string, len(d.Annotations))
		for k, v := range d.Annotations {
			out.Annotations[k] = v
		}
	}
	if d.Layout.Pages != nil {
		out.Layout.Pages = make(map[string]any, len(d.Layout.Pages))
		for k, v := range d.Layout.Pages {
			out.Layout.Pages[k] = v
		}
	}
	return out
}

// Clone deep-copies the slices a merge could otherwise share.
func (t Typography) Clone() Typography {
	out := t
	out.Display = t.Display.Clone()
	out.Body = t.Body.Clone()
	out.Mono = t.Mono.Clone()
	return out
}

// Clone deep-copies the weight list.
func (f FontSpec) Clone() FontSpec {
	out := f
	if f.Weights != nil {
		out.Weights = append([]int(nil), f.Weights...)
	}
	return out
}

// Clone deep-copies the scale.
func (s Space) Clone() Space {
	out := s
	if s.Scale != nil {
		out.Scale = append([]float64(nil), s.Scale...)
	}
	return out
}

// Palette returns the palette for a colour mode name ("dark" or "light").
//
// It is a method rather than a map because the two modes are the only ones, and a caller that
// mistypes one should get the dark palette rather than a zero value it then renders.
func (d Document) Palette(mode string) ModePalette {
	if mode == "light" {
		return d.Color.Light
	}
	return d.Color.Dark
}

// AccentFor returns the accent colour a mode actually renders with.
//
// The per-mode value wins when it is set; otherwise the brand accent, and otherwise the brand
// primary. It is the same precedence css.Generate uses, exposed here so a contrast check, a
// thumbnail and the generated stylesheet cannot disagree about which colour a mode is.
func (d Document) AccentFor(mode string) string {
	p := d.Palette(mode)
	if p.Accent != "" {
		return p.Accent
	}
	if d.Color.Brand.Accent != "" {
		return d.Color.Brand.Accent
	}
	return d.Color.Brand.Primary
}
