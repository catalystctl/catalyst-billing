package theme_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/theme"
)

// The built-in default is what every installation that never opens the Theme Studio serves, so it
// has to satisfy the stricter of the two checks. A default that only passed the layer check would
// be accepted at boot and then refused at render time, which is the worst possible place to learn
// that the fallback document is incomplete.
func TestDefaultPassesBothValidationLayers(t *testing.T) {
	doc := theme.Default()
	if err := theme.Validate(doc); err != nil {
		t.Fatalf("the built-in default is not a valid layer: %v", err)
	}
	if err := theme.ValidateResolved(doc); err != nil {
		t.Fatalf("the built-in default is not a valid resolved document: %v", err)
	}
}

// A preset is a layer: it states the tokens it changes and inherits the rest. If Validate demanded
// the whole palette, every preset would have to restate sixty colours, which is precisely what the
// merge mechanism exists to avoid — so the two entry points must disagree on an omitted token, and
// the stricter one must name the token it wanted.
func TestALayerMayOmitTokensThatAResolvedDocumentMayNot(t *testing.T) {
	cases := []struct {
		name      string
		clear     func(*theme.Document)
		wantField string
	}{
		{"dark background", func(d *theme.Document) { d.Color.Dark.Bg = "" }, "color.dark.bg"},
		{"light text", func(d *theme.Document) { d.Color.Light.Text = "" }, "color.light.text"},
		{"dark surface1", func(d *theme.Document) { d.Color.Dark.Surface1 = "" }, "color.dark.surface1"},
		{"dark border", func(d *theme.Document) { d.Color.Dark.Border = "" }, "color.dark.border"},
		{"light overlay", func(d *theme.Document) { d.Color.Light.Overlay = "" }, "color.light.overlay"},
		{"the default colour mode", func(d *theme.Document) { d.Color.Mode = "" }, "color.mode"},
	}
	for _, c := range cases {
		t.Run(c.name+" may be inherited but not left unresolved", func(t *testing.T) {
			doc := theme.Default()
			c.clear(&doc)
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("a layer that omits %s was refused: %v", c.wantField, err)
			}
			err := theme.ValidateResolved(doc)
			if err == nil {
				t.Fatalf("a resolved document with no %s was accepted; the generator would emit an empty declaration", c.wantField)
			}
			if !strings.Contains(err.Error(), c.wantField) {
				t.Fatalf("the error does not name %s: %v", c.wantField, err)
			}
		})
	}
}

// A misspelt key is an edit that changes nothing, and an operator watching their save succeed while
// nothing moves has no way to diagnose it. Naming the key in the error is the whole value of the
// closed key set, so the tests check the message and not merely that an error occurred.
func TestDecodeRejectsUnknownKeysAndNamesThem(t *testing.T) {
	cases := []struct{ name, raw, wantKey string }{
		{
			"the British spelling of color",
			`{"schema":2,"id":"x","name":"X","colour":{}}`,
			"colour",
		},
		{
			"an invented top-level section",
			`{"schema":2,"id":"x","name":"X","typography2":{}}`,
			"typography2",
		},
		{
			"an unknown key inside color.dark",
			`{"schema":2,"id":"x","name":"X","color":{"dark":{"bgx":"#000000"}}}`,
			"bgx",
		},
		{
			"an unknown key inside typography.scale",
			`{"schema":2,"id":"x","name":"X","typography":{"scale":{"base2":"1rem"}}}`,
			"base2",
		},
		{
			"an unknown key inside layout.shell",
			`{"schema":2,"id":"x","name":"X","layout":{"shell":{"navs":"sidebar"}}}`,
			"navs",
		},
		{
			"an unknown key inside a component override",
			`{"schema":2,"id":"x","name":"X","components":{"button":{"hieght":"32px"}}}`,
			"hieght",
		},
	}
	for _, c := range cases {
		t.Run(c.name+" is refused by name", func(t *testing.T) {
			_, err := theme.Decode([]byte(c.raw))
			if err == nil {
				t.Fatalf("Decode accepted a document with an unknown key %q", c.wantKey)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Fatalf("the error does not name %q: %v", c.wantKey, err)
			}
		})
	}
}

// A schema mismatch must not be half-applied: this build would silently drop the fields it does not
// know, and the operator would publish a theme that lost their work. Naming both versions is what
// tells them the document came from a newer (or older) build rather than from a typo.
func TestWrongSchemaNamesBothVersions(t *testing.T) {
	for _, schema := range []int{0, 1, theme.SchemaVersion + 1, 99} {
		t.Run("schema "+strconv.Itoa(schema)+" is refused with both versions named", func(t *testing.T) {
			doc := theme.Default()
			doc.Schema = schema
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("schema %d was accepted", schema)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(schema)) {
				t.Fatalf("the error does not name the document's schema %d: %v", schema, err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(theme.SchemaVersion)) {
				t.Fatalf("the error does not name this build's schema %d: %v", theme.SchemaVersion, err)
			}
		})
	}
}

// Colours reach a stylesheet as raw text, so the grammar is the boundary between "a value the
// generator can emit" and "a value that breaks the page or issues a request". `url(` and
// `expression(` are the two that execute or exfiltrate rather than merely misrender.
func TestColourGrammar(t *testing.T) {
	accepted := []string{
		"#abc",              // three-digit hex is what a hand-written preset usually uses
		"#aabbcc",           // six-digit hex, the canonical spelling
		"#aabbccdd",         // eight-digit hex carries the alpha the overlay tokens need
		"rgb(1,2,3)",        // legacy function syntax without spaces
		"rgba(1,2,3,.5)",    // the alpha form the overlay values are written in
		"hsl(10, 20%, 30%)", // hue/percent form, as the game-panel import writes it
		"var(--color-bg)",   // a reference to another token, which the generator must pass through
	}
	for _, raw := range accepted {
		t.Run("accepts "+raw, func(t *testing.T) {
			doc := theme.Default()
			doc.Color.Dark.Bg = raw
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("%q was refused as a colour: %v", raw, err)
			}
		})
	}

	rejected := []struct {
		raw string
		why string
	}{
		{"url(x)", "a url() in a colour slot is an outbound request, not a colour"},
		{"expression(alert(1))", "expression() executes script in old IE engines"},
		{"red", "a named colour is not in the grammar the generator emits"},
		{"#gggggg", "not hex, and would be an invalid declaration"},
		{"rgb(url(1))", "the function form must not be able to carry a url()"},
	}
	for _, c := range rejected {
		t.Run("refuses "+c.raw, func(t *testing.T) {
			doc := theme.Default()
			doc.Color.Dark.Bg = c.raw
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("%q was accepted as a colour (%s)", c.raw, c.why)
			}
			if !strings.Contains(err.Error(), "color.dark.bg") {
				t.Fatalf("the error does not name the field: %v", err)
			}
		})
	}
}

// lengthField names one length-valued token, so a table can drive the grammar through the real
// validation path instead of a helper the production code does not call.
type lengthField struct {
	name string
	set  func(*theme.Document, string)
}

var lengthFields = map[string]lengthField{
	"layout.shell.contentMax": {
		name: "layout.shell.contentMax",
		set:  func(d *theme.Document, v string) { d.Layout.Shell.ContentMax = v },
	},
	"typography.tracking.tight": {
		name: "typography.tracking.tight",
		set:  func(d *theme.Document, v string) { d.Typography.Tracking.Tight = v },
	},
}

// A unitless or unknown-unit length is the difference between a stylesheet and a broken stylesheet:
// `4` is not a length, `4pt` is a print unit the panel never renders in, and a trailing `;` would
// end the declaration early and turn the rest of the value into a new (unvalidated) declaration.
func TestLengthGrammar(t *testing.T) {
	accepted := []struct{ field, raw string }{
		{"layout.shell.contentMax", "4px"},
		{"layout.shell.contentMax", "1.5rem"},
		{"layout.shell.contentMax", "100%"},
		{"layout.shell.contentMax", "0"},
		{"typography.tracking.tight", "-0.015em"}, // tracking is legitimately negative
	}
	for _, c := range accepted {
		t.Run("accepts "+c.raw+" as "+c.field, func(t *testing.T) {
			doc := theme.Default()
			lengthFields[c.field].set(&doc, c.raw)
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("%q was refused as %s: %v", c.raw, c.field, err)
			}
		})
	}

	rejected := []struct{ field, raw, why string }{
		{"layout.shell.contentMax", "4", "a bare number is not a length"},
		{"layout.shell.contentMax", "4pt", "pt is a print unit the panel never emits"},
		{"layout.shell.contentMax", "calc(1px)", "calc() is not a length the generator can bound"},
		{"layout.shell.contentMax", "4px;", "a trailing semicolon would end the declaration early"},
	}
	for _, c := range rejected {
		t.Run("refuses "+c.raw+" as "+c.field, func(t *testing.T) {
			doc := theme.Default()
			lengthFields[c.field].set(&doc, c.raw)
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("%q was accepted as %s (%s)", c.raw, c.field, c.why)
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Fatalf("the error does not name %s: %v", c.field, err)
			}
		})
	}
}

// A corner radius has a physical meaning: past 64px it is no longer a corner but a circle, and a
// card with a 640px radius has lost its shape rather than been restyled. The pill radius (`full`)
// is the one value that is deliberately exempt, because "as round as possible" is a real design.
func TestRadiusBound(t *testing.T) {
	accepted := []struct {
		name string
		raw  string
		set  func(*theme.Document, string)
	}{
		{"shape.radius.md", "4rem", func(d *theme.Document, v string) { d.Shape.Radius.MD = v }}, // exactly 64px: the bound is inclusive
		{"shape.radius.md", "64px", func(d *theme.Document, v string) { d.Shape.Radius.MD = v }},
		{"shape.radius.xs", "0", func(d *theme.Document, v string) { d.Shape.Radius.XS = v }},
		{"shape.radius.full", "999px", func(d *theme.Document, v string) { d.Shape.Radius.Full = v }}, // the pill radius is exempt
	}
	for _, c := range accepted {
		t.Run("accepts "+c.raw+" as "+c.name, func(t *testing.T) {
			doc := theme.Default()
			c.set(&doc, c.raw)
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("%q was refused as %s: %v", c.raw, c.name, err)
			}
		})
	}

	doc := theme.Default()
	doc.Shape.Radius.MD = "40rem"
	err := theme.Validate(doc)
	if err == nil {
		t.Fatal("a 640px corner radius was accepted; that is a circle, not a corner")
	}
	if !strings.Contains(err.Error(), "shape.radius.md") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}

// The type scale is bounded at both ends because a 4px step is unreadable and a 64px step is a
// display size that breaks every table in the panel; both are the sort of value a hand-edited
// import introduces, and neither is caught by a grammar check alone.
func TestFontScaleBound(t *testing.T) {
	accepted := []string{"0.8125rem", "13px", "6px", "3rem"}
	for _, raw := range accepted {
		t.Run("accepts "+raw+" as a type-scale step", func(t *testing.T) {
			doc := theme.Default()
			doc.Typography.Scale.Base = raw
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("%q was refused: %v", raw, err)
			}
		})
	}

	rejected := []struct{ raw, why string }{
		{"0.25rem", "4px is below the 6px floor"},
		{"4rem", "64px is above the 48px ceiling"},
		{"49px", "just past the ceiling"},
	}
	for _, c := range rejected {
		t.Run("refuses "+c.raw+" as a type-scale step", func(t *testing.T) {
			doc := theme.Default()
			doc.Typography.Scale.Base = c.raw
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("%q was accepted (%s)", c.raw, c.why)
			}
			if !strings.Contains(err.Error(), "typography.scale.base") {
				t.Fatalf("the error does not name the field: %v", err)
			}
		})
	}
}

// A family outside the installed set fails silently in the browser: it falls back to the default
// serif and the theme "works" while looking like a 1996 web page. The error has to list what is
// installed, otherwise the operator's only recourse is guessing.
func TestFontStacks(t *testing.T) {
	t.Run("a single unknown family is refused with the installed list", func(t *testing.T) {
		doc := theme.Default()
		doc.Typography.Body = theme.FontSpec{Family: "Comic Sans"}
		err := theme.Validate(doc)
		if err == nil {
			t.Fatal("an uninstalled family was accepted; the browser would fall back silently")
		}
		if !strings.Contains(err.Error(), "typography.body") {
			t.Fatalf("the error does not name the field: %v", err)
		}
		for _, family := range []string{"Inter", "JetBrains Mono", "system-ui"} {
			if !strings.Contains(err.Error(), family) {
				t.Fatalf("the error does not list the installed families (missing %q): %v", family, err)
			}
		}
	})

	t.Run("a single installed family is accepted", func(t *testing.T) {
		doc := theme.Default()
		doc.Typography.Display = theme.FontSpec{Family: "Space Grotesk", Weights: []int{600}}
		if err := theme.Validate(doc); err != nil {
			t.Fatalf("an installed family was refused: %v", err)
		}
	})

	t.Run("a full stack is accepted as written", func(t *testing.T) {
		doc := theme.Default()
		doc.Typography.Body = theme.FontSpec{
			Family:   `ui-sans-serif, -apple-system, "Segoe UI", Inter, sans-serif`,
			Fallback: "Arial, sans-serif",
			Weights:  []int{400, 500},
		}
		if err := theme.Validate(doc); err != nil {
			t.Fatalf("a complete stack was refused: %v", err)
		}
	})

	t.Run("a stack carrying a url is refused", func(t *testing.T) {
		doc := theme.Default()
		doc.Typography.Body = theme.FontSpec{Family: "Inter, url(https://evil.example/f.woff2)"}
		if err := theme.Validate(doc); err == nil {
			t.Fatal("a font stack containing url() was accepted; it would become a request per visitor")
		}
	})

	t.Run("a weight outside the 100..900 grid is refused", func(t *testing.T) {
		doc := theme.Default()
		doc.Typography.Body = theme.FontSpec{Family: "Inter", Weights: []int{450}}
		if err := theme.Validate(doc); err == nil {
			t.Fatal("weight 450 was accepted; no installed family ships it")
		}
	})
}

// The enums are what the Studio renders as a select, so an exact-match comparison is the contract:
// a trailing space or a capitalised value is a value the frontend switch will not handle, and
// accepting it would produce a theme whose data attribute matches no generated rule.
func TestEnumsAreClosed(t *testing.T) {
	groups := []struct {
		field    string
		allowed  []string
		nearMiss string
		set      func(*theme.Document, string)
	}{
		{"color.mode", theme.AllowedModes, "Dark", func(d *theme.Document, v string) { d.Color.Mode = v }},
		{"space.density", theme.AllowedDensities, "comfortable ", func(d *theme.Document, v string) { d.Space.Density = v }},
		{"layout.shell.nav", theme.AllowedNavs, "Sidebar", func(d *theme.Document, v string) { d.Layout.Shell.Nav = v }},
		{"layout.shell.cardStyle", theme.AllowedCardStyles, "Flat", func(d *theme.Document, v string) { d.Layout.Shell.CardStyle = v }},
		{"layout.shell.tableStyle", theme.AllowedTableStyles, "comfortable ", func(d *theme.Document, v string) { d.Layout.Shell.TableStyle = v }},
		{"background.style", theme.AllowedBackgrounds, "Gradient", func(d *theme.Document, v string) { d.Background.Style = v }},
	}
	for _, g := range groups {
		for _, value := range g.allowed {
			t.Run(g.field+" accepts "+value, func(t *testing.T) {
				doc := theme.Default()
				g.set(&doc, value)
				if err := theme.Validate(doc); err != nil {
					t.Fatalf("%s = %q was refused: %v", g.field, value, err)
				}
			})
		}
		t.Run(g.field+" refuses "+strconv.Quote(g.nearMiss), func(t *testing.T) {
			doc := theme.Default()
			g.set(&doc, g.nearMiss)
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("%s = %q was accepted; a near-miss must not pass", g.field, g.nearMiss)
			}
			if !strings.Contains(err.Error(), g.field) {
				t.Fatalf("the error does not name %s: %v", g.field, err)
			}
		})
	}
}

// The storefront composer does not exist in this build. Accepting `layout.pages` and rendering the
// classic storefront would look like a saved edit that silently vanished, so the refusal has to say
// what is missing rather than just "invalid".
func TestLayoutPagesIsRefusedWithAReason(t *testing.T) {
	doc := theme.Default()
	doc.Layout.Pages = map[string]any{"home": map[string]any{"blocks": []any{}}}
	err := theme.Validate(doc)
	if err == nil {
		t.Fatal("layout.pages was accepted, but this build cannot compose storefront pages")
	}
	if !strings.Contains(err.Error(), "storefront page composition is not implemented") {
		t.Fatalf("the error does not say why: %v", err)
	}
}

// Component names are a closed set because the generator emits one rule per known component: an
// override called `buton` would be validated, stored, and then never applied — a silent no-op the
// operator cannot see.
func TestComponentNamesAreAClosedSet(t *testing.T) {
	for _, name := range theme.AllowedComponents {
		t.Run("accepts the "+name+" component", func(t *testing.T) {
			doc := theme.Default()
			doc.Components = map[string]theme.Component{name: {Height: "32px", Radius: "4px", Padding: "8px"}}
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("component %q was refused: %v", name, err)
			}
		})
	}

	t.Run("refuses a misspelt component name", func(t *testing.T) {
		doc := theme.Default()
		doc.Components = map[string]theme.Component{"buton": {Height: "32px"}}
		err := theme.Validate(doc)
		if err == nil {
			t.Fatal("a misspelt component name was accepted and would never be applied")
		}
		if !strings.Contains(err.Error(), "buton") {
			t.Fatalf("the error does not name the component: %v", err)
		}
	})

	t.Run("refuses an out-of-range component length", func(t *testing.T) {
		doc := theme.Default()
		// 2000px is past the 1000 bound that component lengths share with the shell dimensions.
		doc.Components = map[string]theme.Component{"button": {Height: "2000px"}}
		if err := theme.Validate(doc); err == nil {
			t.Fatal("a 2000px button height was accepted")
		}
	})
}

// The space scale is the spacing system: eight named steps that must strictly increase. Seven steps
// means one of the eight names the stylesheet references has no value, and a non-monotonic scale has
// two names for one size, which is how a spacing system stops meaning anything.
func TestSpaceScaleMustBeEightMonotonicSteps(t *testing.T) {
	accepted := [][]float64{
		{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4}, // the built-in ladder
		{0.5, 1, 1.5, 2, 3, 4, 6, 8},       // a roomier ladder a preset may state
	}
	for i, scale := range accepted {
		t.Run("accepts scale "+strconv.Itoa(i), func(t *testing.T) {
			doc := theme.Default()
			doc.Space.Scale = scale
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("scale %v was refused: %v", scale, err)
			}
		})
	}

	rejected := []struct {
		name  string
		scale []float64
		why   string
	}{
		{"seven steps", []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3}, "one of the eight space names would have no value"},
		{"a non-monotonic ladder", []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 1.5, 4}, "two steps would be the same size"},
		{"a step above the ceiling", []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 13}, "13 is past the 12 bound"},
		{"a zero step", []float64{0.25, 0.5, 0.75, 0, 1.5, 2, 3, 4}, "a zero step collapses every gap that uses it"},
	}
	for _, c := range rejected {
		t.Run("refuses "+c.name, func(t *testing.T) {
			doc := theme.Default()
			doc.Space.Scale = c.scale
			if err := theme.Validate(doc); err == nil {
				t.Fatalf("scale %v was accepted (%s)", c.scale, c.why)
			}
		})
	}
}

// The document limit is enforced before parsing, not after: a 40 MiB import must be refused without
// being decoded, otherwise a single upload is a memory amplifier.
func TestDecodeRefusesAnOversizedDocument(t *testing.T) {
	doc := theme.Default()
	doc.Description = strings.Repeat("a", theme.MaxDocumentBytes+1)
	raw, err := theme.Canonical(doc)
	if err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	if len(raw) <= theme.MaxDocumentBytes {
		t.Fatalf("the test document is only %d bytes; it must exceed %d", len(raw), theme.MaxDocumentBytes)
	}
	_, err = theme.Decode(raw)
	if err == nil {
		t.Fatal("a document over MaxDocumentBytes was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("the error does not say the document is too large: %v", err)
	}
}

// Ids become URL path segments and cache keys, so the slug grammar is what keeps a theme from being
// reachable at a path that needs escaping — and `-lead` or `Bad_ID` would break the digest-addressed
// stylesheet route rather than merely looking untidy.
func TestSlugGrammar(t *testing.T) {
	accepted := []string{"catalyst-console", "a", "theme-2", strings.Repeat("a", theme.MaxIDLength)}
	for _, id := range accepted {
		t.Run("accepts the id "+id, func(t *testing.T) {
			doc := theme.Default()
			doc.ID = id
			if err := theme.Validate(doc); err != nil {
				t.Fatalf("id %q was refused: %v", id, err)
			}
		})
	}

	rejected := []string{"Bad_ID", "-lead", "", "with space", strings.Repeat("a", theme.MaxIDLength+1)}
	for _, id := range rejected {
		t.Run("refuses the id "+strconv.Quote(id), func(t *testing.T) {
			doc := theme.Default()
			doc.ID = id
			err := theme.Validate(doc)
			if err == nil {
				t.Fatalf("id %q was accepted", id)
			}
			if !strings.Contains(err.Error(), "id") {
				t.Fatalf("the error does not name the field: %v", err)
			}
		})
	}
}

// A theme with no name is unusable in the gallery list, and an 81-character name is a paste accident
// rather than a design decision, so both are refused at the edge instead of at render time.
func TestNameIsRequiredAndBounded(t *testing.T) {
	t.Run("refuses a blank name", func(t *testing.T) {
		doc := theme.Default()
		doc.Name = "   "
		if err := theme.Validate(doc); err == nil {
			t.Fatal("a whitespace-only name was accepted")
		}
	})

	t.Run("refuses a name longer than the limit", func(t *testing.T) {
		doc := theme.Default()
		doc.Name = strings.Repeat("n", theme.MaxNameLength+1)
		if err := theme.Validate(doc); err == nil {
			t.Fatalf("a %d-character name was accepted", theme.MaxNameLength+1)
		}
	})
}
