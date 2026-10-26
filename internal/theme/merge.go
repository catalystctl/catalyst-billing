package theme

import (
	"encoding/json"
	"fmt"
)

// Resolution.
//
// # The three layers
//
//	A scope's document is resolved from: the built-in default ← the parent scope ← the scope's
//	own document. A preset is resolved the same way, with the preset in the last position and
//	its `extends` (if any) in the middle.
//
// # Why the merge happens once, at publish time
//
// A scope that inherited its palette from its parent at *render* time would change the moment
// the parent was republished — silently, for a storefront nobody had touched. So the resolver
// runs when a revision is published and the result is stored beside the source (see store.go:
// `document` is the resolved document, `source` is what the operator saved). Editing a child
// later starts from its own `source`, not from the resolved snapshot, which is why a parent
// republish offers to re-resolve children instead of doing it invisibly.
//
// # Why it is a map merge and not a field-by-field function
//
// A field-by-field merge is four hundred lines that must be edited every time a token is added,
// and the failure mode of forgetting is a token that silently does not inherit. The map merge
// is derived from the struct tags, so a new field is merged correctly the day it is added. The
// cost is that every field must carry `omitempty` (see the package comment), which is a rule
// the tests pin.

// Merge returns `over` folded onto `base`.
//
// Both documents must already be schema-valid layers; Merge does not validate, because the
// intermediate steps of a three-layer resolution are legitimately incomplete.
func Merge(base, over Document) (Document, error) {
	baseMap, err := toMap(base)
	if err != nil {
		return Document{}, err
	}
	overMap, err := toMap(over)
	if err != nil {
		return Document{}, err
	}
	return fromMap(mergeMap(baseMap, overMap))
}

// Resolve folds the layers left to right: Resolve(default, parent, own).
//
// # The one rule that is not a plain merge: the accent family
//
// A palette's accent family (`accent`, `accentHover`, `accentPress`, `accentInk`) is a *derivation*
// of the brand accent for that mode: the dark accent is the brand colour and the light accent is the
// same intent darkened until it reads on paper. The built-in default states them anyway, because its
// values are hand-tuned and the default stylesheet must stay byte-identical.
//
// That creates a trap: a preset that states `color.brand.accent: #ff3d7f` and nothing else would
// inherit the *default's* `#ff951e` in both modes, so the preset would be rose everywhere except the
// one colour an operator looks at first. Making every preset author restate four values per mode is
// a discipline, and a discipline is a thing that gets forgotten in the eleventh preset.
//
// So a layer that names a brand accent or primary drops the accent family it would have inherited,
// for each mode it does not itself override. The accent then follows the brand, which is what the
// operator means by "this preset is pink". A layer that does state a mode accent still wins: that is
// how the default keeps its hand-tuned light accent and how the studio stores an edited one.
//
// It returns an error when a layer cannot be marshalled, which can only happen for a document
// holding a value the JSON encoder rejects (a NaN float, a channel) — impossible for a document that
// came through Decode, and worth reporting rather than panicking if it ever is.
func Resolve(layers ...Document) (Document, error) {
	if len(layers) == 0 {
		return Document{}, fmt.Errorf("theme: nothing to resolve")
	}
	out := layers[0].Clone()
	for _, layer := range layers[1:] {
		base := out.Clone()
		if layerSetsBrandAccent(layer) {
			base = withoutInheritedBrandAccent(base, layer)
			base = withoutInheritedAccent(base, layer)
		}
		merged, err := Merge(base, layer)
		if err != nil {
			return Document{}, err
		}
		out = merged
	}
	return out, nil
}

// layerSetsBrandAccent reports whether a layer names a brand colour of its own.
func layerSetsBrandAccent(doc Document) bool {
	return doc.Color.Brand.Accent != "" || doc.Color.Brand.Primary != ""
}

// withoutInheritedBrandAccent clears the parent's brand accent when the child states only a primary.
//
// The schema's rule is that `accent` falls back to `primary`, so a theme that says "my brand colour is
// #7c3aed" and nothing else means the accent is #7c3aed. Inheriting the parent's accent instead made a
// portal theme that set only a primary keep the panel's accent — the child asked for violet and got the
// parent's red, which is the same class of bug as the per-mode one below and is worth the same care.
func withoutInheritedBrandAccent(base Document, layer Document) Document {
	if layer.Color.Brand.Accent != "" || layer.Color.Brand.Primary == "" {
		return base
	}
	base.Color.Brand.Accent = ""
	return base
}

// withoutInheritedAccent clears the accent family the base would contribute to a mode that the layer
// does not override itself.
func withoutInheritedAccent(base Document, layer Document) Document {
	for _, mode := range []struct {
		palette *ModePalette
		own     ModePalette
	}{
		{&base.Color.Dark, layer.Color.Dark},
		{&base.Color.Light, layer.Color.Light},
	} {
		if mode.own.Accent != "" {
			continue
		}
		mode.palette.Accent = ""
		mode.palette.AccentHover = ""
		mode.palette.AccentPress = ""
		mode.palette.AccentInk = ""
	}
	return base
}

// mergeMap folds src onto dst, recursing into objects.
//
// A slice is replaced, not concatenated: `space.scale` and `typography.body.weights` are whole
// values, and appending one preset's scale to another's would produce a fourteen-step scale
// nobody asked for.
func mergeMap(dst, src map[string]any) map[string]any {
	for key, value := range src {
		if srcObj, ok := value.(map[string]any); ok {
			if dstObj, ok := dst[key].(map[string]any); ok {
				dst[key] = mergeMap(dstObj, srcObj)
				continue
			}
			dst[key] = mergeMap(map[string]any{}, srcObj)
			continue
		}
		dst[key] = value
	}
	return dst
}

// toMap renders a document as a generic JSON object.
func toMap(doc Document) (map[string]any, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("theme: encode a layer: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("theme: decode a layer: %w", err)
	}
	return out, nil
}

// fromMap renders a generic JSON object back into a document.
func fromMap(m map[string]any) (Document, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return Document{}, fmt.Errorf("theme: encode a merged document: %w", err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, fmt.Errorf("theme: decode a merged document: %w", err)
	}
	return doc, nil
}

// Inherits reports whether a document wants its parent scope's presentation.
//
// Absent `inherit` means yes: inheriting is the default because it is what a portal or
// storefront theme usually wants, and because a document written before the field existed must
// keep inheriting rather than suddenly starting from the built-in default.
func Inherits(doc Document) bool {
	return doc.Inherit == nil || *doc.Inherit
}

// WithoutInheritance returns a copy with `inherit` cleared, so the copy can be stored as a
// resolved document without implying a future parent lookup.
func WithoutInheritance(doc Document) Document {
	out := doc.Clone()
	out.Inherit = nil
	return out
}
