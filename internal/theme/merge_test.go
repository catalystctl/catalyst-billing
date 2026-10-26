package theme_test

import (
	"testing"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/presets"
)

// boolPtr is how a test spells the tri-state `inherit` field.
func boolPtr(v bool) *bool { return &v }

// Inheritance is the mechanism that lets a preset be forty lines instead of four hundred: a layer
// that says nothing about a token must end up with its parent's value, not with a zero value. If a
// field were ever marshalled as "" (a missing `omitempty`) the merged document would silently
// contain an empty declaration, so this is the property every field tag in document.go exists for.
func TestMergeInheritsFieldsTheLayerOmits(t *testing.T) {
	base := theme.Default()
	over := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "child",
		Name:   "Child",
		Color:  theme.Color{Brand: theme.BrandPalette{Primary: "#00ff00"}},
	}

	merged, err := theme.Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if merged.Color.Brand.Primary != "#00ff00" {
		t.Errorf("color.brand.primary = %q, want the layer's #00ff00", merged.Color.Brand.Primary)
	}
	// Everything the layer did not mention came from the parent.
	for _, c := range []struct{ name, got, want string }{
		{"color.dark.bg", merged.Color.Dark.Bg, base.Color.Dark.Bg},
		{"color.light.text", merged.Color.Light.Text, base.Color.Light.Text},
		{"typography.scale.base", merged.Typography.Scale.Base, base.Typography.Scale.Base},
		{"typography.leading.normal", merged.Typography.Leading.Normal, base.Typography.Leading.Normal},
		{"shape.radius.full", merged.Shape.Radius.Full, base.Shape.Radius.Full},
		{"motion.normal", merged.Motion.Normal, base.Motion.Normal},
		{"layout.shell.nav", merged.Layout.Shell.Nav, base.Layout.Shell.Nav},
		{"background.style", merged.Background.Style, base.Background.Style},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want the inherited %q", c.name, c.got, c.want)
		}
	}
	if merged.Typography.Body.Family != base.Typography.Body.Family {
		t.Errorf("typography.body.family = %q, want the inherited stack", merged.Typography.Body.Family)
	}
	if len(merged.Space.Scale) != len(base.Space.Scale) {
		t.Errorf("space.scale has %d steps, want the inherited %d", len(merged.Space.Scale), len(base.Space.Scale))
	}
}

// The same merge must be able to *override*, otherwise a child scope could never restyle anything.
// Both halves matter together: a merge that inherited everything and a merge that overrode
// everything are each useless, and only the pair proves the fold is per key.
func TestMergeOverridesOnlyTheKeysTheLayerNames(t *testing.T) {
	base := theme.Default()
	over := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "child",
		Name:   "Child",
		Color: theme.Color{
			Dark: theme.ModePalette{Bg: "#000000", Text: "#ffffff"},
		},
		Layout: theme.Layout{Shell: theme.Shell{Nav: "rail"}},
	}

	merged, err := theme.Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Color.Dark.Bg != "#000000" || merged.Color.Dark.Text != "#ffffff" {
		t.Errorf("the layer's palette did not win: bg=%q text=%q", merged.Color.Dark.Bg, merged.Color.Dark.Text)
	}
	if merged.Color.Light.Bg != base.Color.Light.Bg {
		t.Errorf("color.light.bg = %q, want the inherited %q", merged.Color.Light.Bg, base.Color.Light.Bg)
	}
	if merged.Layout.Shell.Nav != "rail" {
		t.Errorf("layout.shell.nav = %q, want the layer's rail", merged.Layout.Shell.Nav)
	}
	if merged.Layout.Shell.SidebarWidth != base.Layout.Shell.SidebarWidth {
		t.Errorf("layout.shell.sidebarWidth = %q, want the inherited %q",
			merged.Layout.Shell.SidebarWidth, base.Layout.Shell.SidebarWidth)
	}
	if merged.Color.Mode != base.Color.Mode {
		t.Errorf("color.mode = %q, want the inherited %q", merged.Color.Mode, base.Color.Mode)
	}
}

// This is the property the whole preset mechanism depends on: `color.dark` is an object, and a
// merge that replaced objects wholesale would mean a preset that sets one dark token erases the
// other twenty-three and the resolved document stops being renderable.
func TestMergeFoldsNestedObjectsPerKey(t *testing.T) {
	base := theme.Default()
	over := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "child",
		Name:   "Child",
		Color: theme.Color{
			Dark: theme.ModePalette{Text: "#ffffff"},
		},
	}

	merged, err := theme.Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Color.Dark.Text != "#ffffff" {
		t.Errorf("color.dark.text = %q, want the child's #ffffff", merged.Color.Dark.Text)
	}
	for _, c := range []struct{ name, got, want string }{
		{"color.dark.bg", merged.Color.Dark.Bg, base.Color.Dark.Bg},
		{"color.dark.card", merged.Color.Dark.Card, base.Color.Dark.Card},
		{"color.dark.border", merged.Color.Dark.Border, base.Color.Dark.Border},
		{"color.dark.accent", merged.Color.Dark.Accent, base.Color.Dark.Accent},
		{"color.dark.surface3", merged.Color.Dark.Surface3, base.Color.Dark.Surface3},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q; setting a sibling token wiped the parent's %q", c.name, c.got, c.want)
		}
	}
	// The rest of the colour object is untouched too: `dark` is not the only nested object.
	if merged.Color.Brand.Primary != base.Color.Brand.Primary {
		t.Errorf("color.brand.primary = %q, want the inherited %q", merged.Color.Brand.Primary, base.Color.Brand.Primary)
	}
	if merged.Color.Semantic.Danger != base.Color.Semantic.Danger {
		t.Errorf("color.semantic.danger = %q, want the inherited %q",
			merged.Color.Semantic.Danger, base.Color.Semantic.Danger)
	}
}

// A slice is a whole value, not a bag of extra entries: appending one preset's eight-step space
// scale to another's would produce a fourteen-step scale nobody asked for, and every `--space-*`
// token after the eighth would point at the wrong step.
func TestMergeReplacesSlicesInsteadOfAppending(t *testing.T) {
	base := theme.Default()
	roomy := []float64{0.5, 1, 1.5, 2, 3, 4, 6, 8}
	over := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "child",
		Name:   "Child",
		Space:  theme.Space{Scale: roomy},
	}

	merged, err := theme.Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Space.Scale) != len(roomy) {
		t.Fatalf("space.scale has %d steps after the merge, want the layer's %d: %v",
			len(merged.Space.Scale), len(roomy), merged.Space.Scale)
	}
	for i := range roomy {
		if merged.Space.Scale[i] != roomy[i] {
			t.Fatalf("space.scale[%d] = %v, want the layer's %v: %v", i, merged.Space.Scale[i], roomy[i], merged.Space.Scale)
		}
	}

	t.Run("font weights are replaced too", func(t *testing.T) {
		over := theme.Document{
			Schema: theme.SchemaVersion,
			ID:     "child",
			Name:   "Child",
			Typography: theme.Typography{
				Body: theme.FontSpec{Weights: []int{300, 700}},
			},
		}
		merged, err := theme.Merge(base, over)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if got := merged.Typography.Body.Weights; len(got) != 2 || got[0] != 300 || got[1] != 700 {
			t.Fatalf("typography.body.weights = %v, want [300 700]", got)
		}
	})
}

// Resolve is what a publish stores, so its output has to be renderable: a preset folded onto the
// default that then failed ValidateResolved would be a theme the operator could publish and no
// visitor could load. Checking every preset keeps the catalogue honest as well as the resolver.
func TestResolveProducesARenderableDocument(t *testing.T) {
	if err := presets.Err(); err != nil {
		t.Fatalf("the preset catalogue did not load: %v", err)
	}
	for _, preset := range presets.All() {
		t.Run(preset.ID+" resolves to a valid document", func(t *testing.T) {
			doc, err := theme.Resolve(theme.Default(), preset.Document)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if err := theme.ValidateResolved(doc); err != nil {
				t.Fatalf("the resolved preset is not renderable: %v", err)
			}
			// The preset's own id and name win over the default's, or the gallery would show every
			// preset as "Catalyst Billing".
			if doc.ID != preset.ID {
				t.Errorf("id = %q, want the preset's %q", doc.ID, preset.ID)
			}
			if doc.Name != preset.Name {
				t.Errorf("name = %q, want the preset's %q", doc.Name, preset.Name)
			}
		})
	}
}

// A merge that mutated its inputs would corrupt the parent scope's document on every child publish —
// the parent is the same value every child is resolved from, so the corruption would be cumulative
// and invisible until the palette was mostly the last child's. This is the bug a shallow copy of the
// map (or of the scale slice) would produce.
func TestMergeDoesNotMutateItsInputs(t *testing.T) {
	base := theme.Default()
	baseScale := append([]float64(nil), base.Space.Scale...)
	baseBg := base.Color.Dark.Bg
	baseWeights := append([]int(nil), base.Typography.Body.Weights...)

	over := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "child",
		Name:   "Child",
		Color:  theme.Color{Dark: theme.ModePalette{Bg: "#000000"}},
		Space:  theme.Space{Scale: []float64{1, 2, 3, 4, 5, 6, 7, 8}},
		Typography: theme.Typography{
			Body: theme.FontSpec{Weights: []int{300}},
		},
	}
	overScale := append([]float64(nil), over.Space.Scale...)

	merged, err := theme.Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if base.Color.Dark.Bg != baseBg {
		t.Errorf("Merge changed the base document: color.dark.bg is now %q, was %q", base.Color.Dark.Bg, baseBg)
	}
	if len(base.Space.Scale) != len(baseScale) {
		t.Fatalf("Merge changed the base space scale length: %v", base.Space.Scale)
	}
	for i := range baseScale {
		if base.Space.Scale[i] != baseScale[i] {
			t.Fatalf("Merge changed the base space scale: %v, was %v", base.Space.Scale, baseScale)
		}
	}
	if len(base.Typography.Body.Weights) != len(baseWeights) || base.Typography.Body.Weights[0] != baseWeights[0] {
		t.Errorf("Merge changed the base font weights: %v, was %v", base.Typography.Body.Weights, baseWeights)
	}
	if len(over.Space.Scale) != len(overScale) || over.Space.Scale[0] != overScale[0] {
		t.Errorf("Merge changed the overriding layer: %v, was %v", over.Space.Scale, overScale)
	}

	// The result must also be independent of both inputs, or a later edit of the resolved document
	// would reach back into the layer it came from.
	merged.Space.Scale[0] = 99
	merged.Typography.Body.Weights[0] = 999
	if base.Space.Scale[0] == 99 || over.Space.Scale[0] == 99 {
		t.Error("the merged document shares its space scale with an input")
	}
	if base.Typography.Body.Weights[0] == 999 || over.Typography.Body.Weights[0] == 999 {
		t.Error("the merged document shares its font weights with an input")
	}

	t.Run("Resolve leaves its layers alone", func(t *testing.T) {
		layer := theme.Document{Schema: theme.SchemaVersion, ID: "child", Name: "Child", Color: theme.Color{Dark: theme.ModePalette{Bg: "#000000"}}}
		if _, err := theme.Resolve(theme.Default(), layer); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if layer.Color.Dark.Bg != "#000000" || layer.ID != "child" {
			t.Errorf("Resolve mutated its layer: %+v", layer)
		}
	})
}

// Absent `inherit` must mean "yes": a portal theme written before the field existed, or by someone
// who never thought about it, has to keep following its parent scope. Only an explicit false — the
// escape hatch — starts from the built-in default instead.
func TestInheritsDefaultsToTrue(t *testing.T) {
	cases := []struct {
		name  string
		value *bool
		want  bool
	}{
		{"an absent inherit means inherit", nil, true},
		{"an explicit true means inherit", boolPtr(true), true},
		{"an explicit false opts out", boolPtr(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := theme.Default()
			doc.Inherit = c.value
			if got := theme.Inherits(doc); got != c.want {
				t.Fatalf("Inherits = %v, want %v", got, c.want)
			}
		})
	}
}

// A stored resolved document must not imply a future parent lookup, so clearing `inherit` is part of
// publishing; the copy must be independent or the caller's source document would lose its setting.
func TestWithoutInheritanceClearsTheFlagOnACopy(t *testing.T) {
	doc := theme.Default()
	doc.Inherit = boolPtr(false)
	out := theme.WithoutInheritance(doc)
	if out.Inherit != nil {
		t.Fatalf("the copy still carries inherit=%v", *out.Inherit)
	}
	if doc.Inherit == nil || *doc.Inherit != false {
		t.Error("the source document lost its inherit flag")
	}
}

// Resolve with nothing to resolve is a programming error, not an empty theme: returning a zero
// document would be a document with no schema that fails much later, at render time.
func TestResolveWithNoLayersIsAnError(t *testing.T) {
	if _, err := theme.Resolve(); err == nil {
		t.Fatal("Resolve accepted zero layers")
	}
}
