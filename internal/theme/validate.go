package theme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Validation.
//
// # Why it is closed
//
// Every field is checked against a grammar the generator can emit, not merely "looks like a
// colour". A theme is authored by an operator and served to every visitor, so a value the
// generator cannot express is not a cosmetic problem: it is a declaration that breaks the
// stylesheet for the whole page, or — worse — one that carries a `url()` out of the document
// and into a request the operator did not make. The whitelist is the security boundary, so a
// new field is a new branch here rather than a pass-through.
//
// # Why unknown keys are rejected
//
// `{"colour": {...}}` (the British spelling) is a typo, and a validator that ignored it would
// accept a document that changes nothing while the operator watches their edit do nothing.
// The error names the key. The same reasoning applies to nested objects and to component
// names, which is why components are a closed set too.

// Limits. They are exported so the Studio can state them instead of letting an operator
// discover them through a refused save.
const (
	// MaxDocumentBytes bounds the whole JSON document, excluding asset bytes.
	MaxDocumentBytes = 1 << 20 // 1 MiB
	// MaxCustomCSSBytes bounds the custom-CSS block. The game panel caps at 100 KiB; the
	// theme registry raises it because a 100 KiB stylesheet is a legitimate small design
	// system and the parsed-and-validated path is what makes the size safe.
	MaxCustomCSSBytes = 256 << 10
	// MaxNameLength bounds a theme's display name.
	MaxNameLength = 80
	// MaxIDLength bounds a theme's slug.
	MaxIDLength = 64
)

// Allowed enums. They are slices so the API can publish them and the Studio can render a
// select rather than a free-text field that fails on save.
var (
	AllowedModes       = []string{"dark", "light", "system"}
	AllowedDensities   = []string{"compact", "comfortable", "roomy"}
	AllowedNavs        = []string{"sidebar", "rail", "topbar"}
	AllowedCardStyles  = []string{"flat", "outlined", "elevated"}
	AllowedTableStyles = []string{"compact", "comfortable"}
	AllowedBackgrounds = []string{"flat", "gradient", "image", "mesh", "paper"}
	// AllowedComponents is the closed set of per-component overrides.
	AllowedComponents = []string{"button", "input", "card", "table", "badge", "tabs", "modal", "nav"}
)

// InstalledFamilies is the closed set of families a theme may name.
//
// They are the open-licence families the product ships or expects an operator to self-host
// (docs/theming-system.md §8). A family outside this set is refused because the browser would
// silently fall back to its default serif — the theme would "work" and look like a 1996 web
// page, which is the least diagnosable failure mode a design system can have.
//
// A complete CSS stack is also accepted (see checkFontStack), which is how the built-in
// default names the system UI font.
var InstalledFamilies = []string{
	"Inter",
	"DM Sans",
	"JetBrains Mono",
	"Source Serif 4",
	"Space Grotesk",
	"IBM Plex Mono",
	"Fraunces",
	"system-ui",
	"ui-sans-serif",
	"ui-serif",
	"ui-monospace",
	"Georgia",
	"Helvetica Neue",
	"Arial",
}

// topLevelKeys is the closed set of document keys.
var topLevelKeys = []string{
	"schema", "id", "name", "description", "extends", "inherit",
	"color", "typography", "space", "shape", "elevation", "motion",
	"background", "layout", "components", "customCss", "annotations",
}

// Decode reads a theme document, rejecting unknown keys and then validating every value.
//
// It is the only way a document should enter the process from outside (an import, a request
// body, a preset read from disk). A document produced by this package — Default, a merged
// resolution — has already been through it.
func Decode(raw []byte) (Document, error) {
	if len(raw) > MaxDocumentBytes {
		return Document{}, fmt.Errorf("theme: the document exceeds %d KiB", MaxDocumentBytes>>10)
	}
	// The key check comes first so the error names the offending key. DisallowUnknownFields
	// would report it too, but as a bare "unknown field" against a Go type name, which tells an
	// operator nothing about the JSON they wrote.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Document{}, fmt.Errorf("theme: the document is not a JSON object: %w", err)
	}
	allowed := make(map[string]struct{}, len(topLevelKeys))
	for _, k := range topLevelKeys {
		allowed[k] = struct{}{}
	}
	var unknown []string
	for key := range probe {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Document{}, fmt.Errorf("theme: unknown field(s) %s; allowed: %s",
			strings.Join(unknown, ", "), strings.Join(topLevelKeys, ", "))
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("theme: %w", err)
	}
	if err := Validate(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Validate checks a document that may be a *layer* — a preset, a scope override, a draft that
// inherits the rest from its parent — and returns the first problem it finds, named by field.
//
// It is called on write (import, draft save) and again when a document is read from the database
// for serving. Validating twice is deliberate: the write path is the one that can be bypassed by
// a psql insert, and a document that reached the generator without passing here would be emitted
// into a stylesheet unread.
//
// It does not require the canonical palette fields, because a layer legitimately omits them.
// ValidateResolved is the check for a document that is about to be rendered.
func Validate(doc Document) error { return validateDoc(doc, false) }

// ValidateResolved checks a fully resolved document: everything Validate checks, plus the
// canonical token set, because a resolved document is what the CSS generator reads and a missing
// `color.dark.bg` would emit a declaration with no value.
func ValidateResolved(doc Document) error { return validateDoc(doc, true) }

// validateDoc is the shared body. complete selects whether the canonical tokens are required.
func validateDoc(doc Document, complete bool) error {
	if doc.Schema != SchemaVersion {
		return fmt.Errorf("theme: schema %d is not supported (this build reads schema %d)",
			doc.Schema, SchemaVersion)
	}
	if err := checkSlug(doc.ID, "id", MaxIDLength); err != nil {
		return err
	}
	if strings.TrimSpace(doc.Name) == "" {
		return fmt.Errorf("theme: name is required")
	}
	if len([]rune(doc.Name)) > MaxNameLength {
		return fmt.Errorf("theme: name is longer than %d characters", MaxNameLength)
	}
	if doc.Extends != "" {
		if err := checkSlug(doc.Extends, "extends", MaxIDLength); err != nil {
			return err
		}
	}
	if err := validateColor(doc.Color, complete); err != nil {
		return err
	}
	if err := validateTypography(doc.Typography); err != nil {
		return err
	}
	if err := validateSpace(doc.Space); err != nil {
		return err
	}
	if err := validateShape(doc.Shape); err != nil {
		return err
	}
	if err := validateElevation(doc.Elevation); err != nil {
		return err
	}
	if err := validateMotion(doc.Motion); err != nil {
		return err
	}
	if err := validateBackground(doc.Background); err != nil {
		return err
	}
	if err := validateLayout(doc.Layout); err != nil {
		return err
	}
	if err := validateComponents(doc.Components); err != nil {
		return err
	}
	if len(doc.CustomCSS) > MaxCustomCSSBytes {
		return fmt.Errorf("theme: customCss is %d bytes; the limit is %d",
			len(doc.CustomCSS), MaxCustomCSSBytes)
	}
	if strings.TrimSpace(doc.CustomCSS) != "" {
		if err := ValidateCustomCSS(doc.CustomCSS); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- the fields --

func validateColor(c Color, complete bool) error {
	if c.Mode != "" {
		if err := checkEnum(c.Mode, AllowedModes, "color.mode"); err != nil {
			return err
		}
	} else if complete {
		return fmt.Errorf("theme: color.mode is required")
	}
	values := map[string]string{
		"color.brand.primary":    c.Brand.Primary,
		"color.brand.secondary":  c.Brand.Secondary,
		"color.brand.accent":     c.Brand.Accent,
		"color.semantic.success": c.Semantic.Success,
		"color.semantic.warning": c.Semantic.Warning,
		"color.semantic.danger":  c.Semantic.Danger,
		"color.semantic.info":    c.Semantic.Info,
		"color.semantic.focus":   c.Semantic.Focus,
	}
	for _, name := range sortedKeys(values) {
		if err := checkColour(values[name], name, true); err != nil {
			return err
		}
	}
	if err := validatePalette(c.Dark, "color.dark", complete); err != nil {
		return err
	}
	return validatePalette(c.Light, "color.light", complete)
}

func validatePalette(p ModePalette, prefix string, complete bool) error {
	required := map[string]string{
		".bg": p.Bg, ".popover": p.Popover, ".overlay": p.Overlay,
		".card": p.Card, ".surface1": p.Surface1, ".surface2": p.Surface2, ".surface3": p.Surface3,
		".border": p.Border, ".borderStrong": p.BorderStrong,
		".text": p.Text, ".textMuted": p.TextMuted, ".textFaint": p.TextFaint,
		".input": p.Input,
	}
	optional := map[string]string{
		".side": p.Side, ".top": p.Top,
		".surfaceHover": p.SurfaceHover, ".surfaceInset": p.SurfaceInset,
		".surfaceActive": p.SurfaceActive, ".lineSoft": p.LineSoft,
		".textSubtle": p.TextSubtle, ".textInverse": p.TextInverse,
		".scrollbar": p.Scrollbar, ".pink": p.Pink,
	}
	// Sorted so the reported problem is stable across runs; a map iteration order would make
	// "the first error" a different error each time, which is unhelpful in a test.
	for _, name := range sortedKeys(required) {
		// A layer may omit a canonical token (it inherits); a resolved document may not, because
		// the generator would emit a declaration with no value.
		if err := checkColour(required[name], prefix+name, !complete); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(optional) {
		if err := checkColour(optional[name], prefix+name, true); err != nil {
			return err
		}
	}
	return nil
}

func validateTypography(t Typography) error {
	for _, name := range sortedFontKeys(t) {
		if err := checkFontStack(t.font(name), name); err != nil {
			return err
		}
	}
	scale := map[string]string{
		"typography.scale.xs": t.Scale.XS, "typography.scale.sm": t.Scale.SM,
		"typography.scale.base": t.Scale.Base, "typography.scale.md": t.Scale.MD,
		"typography.scale.lg": t.Scale.LG, "typography.scale.xl": t.Scale.XL,
		"typography.scale.2xl": t.Scale.XXL, "typography.scale.3xl": t.Scale.XXXL,
	}
	for _, name := range sortedKeys(scale) {
		if err := checkFontSize(scale[name], name); err != nil {
			return err
		}
	}
	leading := map[string]string{
		"typography.leading.tight":  t.Leading.Tight,
		"typography.leading.snug":   t.Leading.Snug,
		"typography.leading.normal": t.Leading.Normal,
	}
	for _, name := range sortedKeys(leading) {
		if leading[name] == "" {
			continue
		}
		if err := checkNumber(leading[name], 0.8, 3, name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(map[string]string{
		"typography.tracking.tight": t.Tracking.Tight,
		"typography.tracking.wide":  t.Tracking.Wide,
	}) {
		raw := map[string]string{
			"typography.tracking.tight": t.Tracking.Tight,
			"typography.tracking.wide":  t.Tracking.Wide,
		}[name]
		if raw == "" {
			continue
		}
		if err := checkLength(raw, name, -1, 1); err != nil {
			return err
		}
	}
	return nil
}

// sortedFontKeys returns the font-role field names in order.
func sortedFontKeys(t Typography) []string {
	return []string{"typography.display", "typography.body", "typography.mono"}
}

// font returns one font role by field name.
func (t Typography) font(field string) FontSpec {
	switch field {
	case "typography.display":
		return t.Display
	case "typography.body":
		return t.Body
	default:
		return t.Mono
	}
}

func validateSpace(s Space) error {
	if s.Unit == 0 && s.Scale == nil {
		return nil // absent: inherit
	}
	if s.Unit < 2 || s.Unit > 12 {
		return fmt.Errorf("theme: space.unit must be between 2 and 12, got %d", s.Unit)
	}
	if s.Density != "" {
		if err := checkEnum(s.Density, AllowedDensities, "space.density"); err != nil {
			return err
		}
	}
	if s.Scale == nil {
		return nil
	}
	if len(s.Scale) != 8 {
		return fmt.Errorf("theme: space.scale must have 8 steps, got %d", len(s.Scale))
	}
	prev := 0.0
	for i, step := range s.Scale {
		if step <= 0 || step > 12 {
			return fmt.Errorf("theme: space.scale[%d] must be between 0 and 12, got %v", i, step)
		}
		// A scale that is not monotonic is a scale with two names for one size, which is how a
		// spacing system stops meaning anything.
		if step < prev {
			return fmt.Errorf("theme: space.scale must increase; step %d (%v) is smaller than the previous (%v)",
				i, step, prev)
		}
		prev = step
	}
	return nil
}

func validateShape(s Shape) error {
	radii := map[string]string{
		"shape.radius.xs": s.Radius.XS, "shape.radius.sm": s.Radius.SM,
		"shape.radius.md": s.Radius.MD, "shape.radius.lg": s.Radius.LG,
		"shape.radius.xl": s.Radius.XL,
	}
	for _, name := range sortedKeys(radii) {
		if err := checkRadius(radii[name], name); err != nil {
			return err
		}
	}
	if s.Radius.Full != "" {
		if err := checkLength(s.Radius.Full, "shape.radius.full", 0, 1000); err != nil {
			return err
		}
	}
	if s.BorderWidth != "" {
		if err := checkLength(s.BorderWidth, "shape.borderWidth", 0, 8); err != nil {
			return err
		}
	}
	return nil
}

func validateElevation(e Elevation) error {
	for _, mode := range []struct {
		name string
		s    Shadows
	}{{"dark", e.Dark}, {"light", e.Light}} {
		values := map[string]string{
			"1": mode.s.One, "2": mode.s.Two, "3": mode.s.Three, "focus": mode.s.Focus,
		}
		for _, name := range sortedKeys(values) {
			if err := checkShadow(values[name], "elevation."+mode.name+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMotion(m Motion) error {
	values := map[string]string{
		"motion.fast": m.Fast, "motion.normal": m.Normal, "motion.slow": m.Slow,
	}
	for _, name := range sortedKeys(values) {
		if values[name] == "" {
			continue
		}
		if err := checkDuration(values[name], name); err != nil {
			return err
		}
	}
	if m.Ease != "" {
		if err := checkEase(m.Ease, "motion.ease"); err != nil {
			return err
		}
	}
	return nil
}

func validateBackground(b Background) error {
	if b.Style == "" {
		return nil
	}
	if err := checkEnum(b.Style, AllowedBackgrounds, "background.style"); err != nil {
		return err
	}
	if b.Image != "" {
		if err := checkAssetRef(b.Image, "background.image"); err != nil {
			return err
		}
	}
	if b.Gradient != "" {
		if err := checkGradient(b.Gradient, "background.gradient"); err != nil {
			return err
		}
	}
	if b.Noise < 0 || b.Noise > 1 {
		return fmt.Errorf("theme: background.noise must be between 0 and 1, got %v", b.Noise)
	}
	return nil
}

func validateLayout(l Layout) error {
	if len(l.Pages) > 0 {
		// The storefront composer is not part of this build (docs/theming-system.md §9). Accepting
		// the value and rendering the classic storefront would look like the operator's edit was
		// saved and then lost, which is worse than a refusal that says what is missing.
		return fmt.Errorf("theme: layout.pages is not supported by this build; " +
			"storefront page composition is not implemented yet")
	}
	sh := l.Shell
	if sh.Nav != "" {
		if err := checkEnum(sh.Nav, AllowedNavs, "layout.shell.nav"); err != nil {
			return err
		}
	}
	if sh.CardStyle != "" {
		if err := checkEnum(sh.CardStyle, AllowedCardStyles, "layout.shell.cardStyle"); err != nil {
			return err
		}
	}
	if sh.TableStyle != "" {
		if err := checkEnum(sh.TableStyle, AllowedTableStyles, "layout.shell.tableStyle"); err != nil {
			return err
		}
	}
	for name, raw := range map[string]string{
		"layout.shell.sidebarWidth": sh.SidebarWidth,
		"layout.shell.topbarHeight": sh.TopbarHeight,
		"layout.shell.contentMax":   sh.ContentMax,
	} {
		if raw == "" {
			continue
		}
		if err := checkLength(raw, name, 0, 4000); err != nil {
			return err
		}
	}
	return nil
}

func validateComponents(c map[string]Component) error {
	for name, comp := range c {
		if err := checkEnum(name, AllowedComponents, "components"); err != nil {
			return err
		}
		for field, raw := range map[string]string{
			"height": comp.Height, "radius": comp.Radius, "padding": comp.Padding,
		} {
			if raw == "" {
				continue
			}
			if err := checkLength(raw, "components."+name+"."+field, 0, 1000); err != nil {
				return err
			}
		}
	}
	return nil
}

// ------------------------------------------------------------------ grammars --

var (
	hexColour  = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	colourFunc = regexp.MustCompile(`^(rgb|rgba|hsl|hsla)\(([-+0-9.%\s,/]*)\)$`)
	tokenRef   = regexp.MustCompile(`^var\(--[a-z0-9-]+\)$`)
	lengthRe   = regexp.MustCompile(`^([-+]?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+))(px|rem|em|%|vh|vw|ch)$`)
	numberRe   = regexp.MustCompile(`^[-+]?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	assetRefRe = regexp.MustCompile(`^theme-asset:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// fontStackRe is deliberately narrow: letters, digits, spaces, commas, quotes, hyphens and
	// dots. A CSS font stack needs nothing else, and everything a stack must not contain —
	// `;`, `}`, `(`, `\`, a `url(` — is outside it.
	fontStackRe = regexp.MustCompile(`^[A-Za-z0-9 ,"'._-]+$`)
	// shadowRe allows a shadow list: colours, lengths, `inset`, commas and spaces.
	shadowRe = regexp.MustCompile(`^(none|[A-Za-z0-9 ,().%#-]+)$`)
	easeRe   = regexp.MustCompile(`^(linear|ease|ease-in|ease-out|ease-in-out|step-start|step-end|cubic-bezier\([-+0-9.,\s]+\))$`)
	gradRe   = regexp.MustCompile(`^(?:linear|radial|conic)-gradient\([-A-Za-z0-9 ,().%#]+\)$`)
	durRe    = regexp.MustCompile(`^([-+]?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+))(ms|s)$`)
)

// checkColour validates a colour value.
//
// allowEmpty is true for every field whose absence means "derive it" or "inherit it".
func checkColour(raw, field string, allowEmpty bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("theme: %s is required", field)
	}
	if hexColour.MatchString(raw) || tokenRef.MatchString(raw) {
		return nil
	}
	if m := colourFunc.FindStringSubmatch(raw); m != nil {
		// The argument list is validated by the regexp; the extra check is that a `url(` cannot
		// be smuggled through it, which the character class already forbids. Saying so here keeps
		// the guarantee visible to the next reader.
		if strings.Contains(m[2], "url") {
			return fmt.Errorf("theme: %s must not contain url()", field)
		}
		return nil
	}
	return fmt.Errorf("theme: %s is not a colour this server can emit (%q); "+
		"use #rgb, #rrggbb, #rrggbbaa, rgb(), hsl() or var(--token)", field, raw)
}

// checkLength validates a CSS length and bounds its magnitude.
//
// The bound is expressed in the unit's own numbers rather than in pixels because `%` and `vh`
// have no fixed pixel meaning. max is a magnitude, so `-12px` is checked as 12.
func checkLength(raw, field string, min, max float64) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if raw == "0" {
		return nil
	}
	m := lengthRe.FindStringSubmatch(raw)
	if m == nil {
		return fmt.Errorf("theme: %s must be a number with a unit (px, rem, em, %%, vh, vw, ch), got %q", field, raw)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return fmt.Errorf("theme: %s is not a number: %q", field, raw)
	}
	if value < min || value > max {
		return fmt.Errorf("theme: %s must be between %v and %v, got %v", field, min, max, value)
	}
	return nil
}

// checkRadius bounds a corner radius to 64px-equivalent.
func checkRadius(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if err := checkLength(raw, field, 0, 64); err != nil {
		return err
	}
	// `4rem` is 64px and legal; `40rem` is 640px and is a circle, not a corner.
	if strings.HasSuffix(raw, "rem") || strings.HasSuffix(raw, "em") {
		value, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(raw, "rem"), "em"), 64)
		if err == nil && value > 4 {
			return fmt.Errorf("theme: %s must be at most 4rem (64px), got %q", field, raw)
		}
	}
	return nil
}

// checkFontSize bounds a type-scale step to 6px..48px.
func checkFontSize(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	m := lengthRe.FindStringSubmatch(raw)
	if m == nil {
		return fmt.Errorf("theme: %s must be a length, got %q", field, raw)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return fmt.Errorf("theme: %s is not a number: %q", field, raw)
	}
	px := value
	switch m[2] {
	case "rem", "em":
		px = value * 16
	case "px":
	default:
		return fmt.Errorf("theme: %s must use px or rem, got %q", field, raw)
	}
	if px < 6 || px > 48 {
		return fmt.Errorf("theme: %s must be between 6px and 48px, got %q", field, raw)
	}
	return nil
}

// checkNumber validates a unitless number in a range.
func checkNumber(raw string, min, max float64, field string) error {
	if !numberRe.MatchString(strings.TrimSpace(raw)) {
		return fmt.Errorf("theme: %s must be a number, got %q", field, raw)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fmt.Errorf("theme: %s is not a number: %q", field, raw)
	}
	if math.IsNaN(value) || value < min || value > max {
		return fmt.Errorf("theme: %s must be between %v and %v, got %v", field, min, max, value)
	}
	return nil
}

// checkFontStack validates a family and its fallback.
func checkFontStack(spec FontSpec, field string) error {
	family := strings.TrimSpace(spec.Family)
	if family == "" {
		if strings.TrimSpace(spec.Fallback) == "" {
			return nil // absent: inherit
		}
		return fmt.Errorf("theme: %s.family is required when a fallback is set", field)
	}
	combined := family
	if spec.Fallback != "" {
		combined += ", " + spec.Fallback
	}
	if !fontStackRe.MatchString(combined) {
		return fmt.Errorf("theme: %s must be a font stack of names and commas, got %q", field, combined)
	}
	if strings.Contains(strings.ToLower(combined), "url") {
		return fmt.Errorf("theme: %s must not contain url()", field)
	}
	// A single unquoted name must be a family this build knows how to load. A stack is accepted
	// as written, because a stack is the operator's own fallback chain and every name in it is
	// resolved by the browser.
	if !strings.Contains(combined, ",") {
		if !isInstalledFamily(family) {
			return fmt.Errorf("theme: %s names %q, which is not an installed family; "+
				"choose one of: %s", field, family, strings.Join(InstalledFamilies, ", "))
		}
	}
	for _, w := range spec.Weights {
		if w < 100 || w > 900 || w%100 != 0 {
			return fmt.Errorf("theme: %s.weights contains %d; weights are 100..900 in steps of 100", field, w)
		}
	}
	return nil
}

// isInstalledFamily reports whether name is in the installed set, case-insensitively.
func isInstalledFamily(name string) bool {
	for _, f := range InstalledFamilies {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

// checkShadow validates a box-shadow value.
func checkShadow(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !shadowRe.MatchString(raw) {
		return fmt.Errorf("theme: %s is not a shadow this server can emit (%q)", field, raw)
	}
	if strings.Contains(strings.ToLower(raw), "url") ||
		strings.Contains(strings.ToLower(raw), "expression") ||
		strings.Contains(raw, "@") {
		return fmt.Errorf("theme: %s must not contain url(), expression() or @", field)
	}
	return nil
}

// checkDuration validates a motion duration.
func checkDuration(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "0" || raw == "0ms" || raw == "0s" {
		return nil
	}
	m := durRe.FindStringSubmatch(raw)
	if m == nil {
		return fmt.Errorf("theme: %s must be a duration in ms or s such as 160ms, got %q", field, raw)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil || value < 0 || value > 5000 {
		return fmt.Errorf("theme: %s must be between 0 and 5000, got %q", field, raw)
	}
	return nil
}

// checkEase validates an easing function.
func checkEase(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if !easeRe.MatchString(raw) {
		return fmt.Errorf("theme: %s must be a named easing or cubic-bezier(...), got %q", field, raw)
	}
	return nil
}

// checkGradient validates a gradient.
func checkGradient(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if !gradRe.MatchString(raw) {
		return fmt.Errorf("theme: %s must be a linear-, radial- or conic-gradient(...), got %q", field, raw)
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "url") || strings.Contains(lower, "var(") {
		return fmt.Errorf("theme: %s must not contain url() or var()", field)
	}
	return nil
}

// checkAssetRef validates a theme-asset reference.
func checkAssetRef(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !assetRefRe.MatchString(raw) {
		return fmt.Errorf("theme: %s must be a theme asset reference (theme-asset:<uuid>), got %q", field, raw)
	}
	return nil
}

// checkSlug validates an identifier.
func checkSlug(raw, field string, max int) error {
	if raw == "" {
		return fmt.Errorf("theme: %s is required", field)
	}
	if len(raw) > max {
		return fmt.Errorf("theme: %s is longer than %d characters", field, max)
	}
	if !slugRe.MatchString(raw) {
		return fmt.Errorf("theme: %s must be lower-case letters, digits and hyphens, got %q", field, raw)
	}
	return nil
}

// checkEnum validates a value against a closed set.
func checkEnum(raw string, allowed []string, field string) error {
	for _, a := range allowed {
		if raw == a {
			return nil
		}
	}
	return fmt.Errorf("theme: %s must be one of %s, got %q", field, strings.Join(allowed, ", "), raw)
}

// sortedKeys returns a map's keys in order, so validation reports problems deterministically.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AssetRefID extracts the uuid from a theme-asset reference, or "".
func AssetRefID(raw string) string {
	if !assetRefRe.MatchString(strings.TrimSpace(raw)) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(raw), "theme-asset:")
}

// AssetRef renders a uuid as a theme-asset reference.
func AssetRef(id string) string { return "theme-asset:" + id }
