package theme_test

import (
	"testing"

	"github.com/catalyst/billing/internal/theme"
)

// Default is constructed per call, not shared as a package-level value: a handler that merged into
// the fallback would otherwise change it for every other request in the process, and the damage
// would look like a random palette rather than like a mutation bug.
func TestDefaultReturnsAnIndependentDocument(t *testing.T) {
	first := theme.Default()
	first.Space.Scale[0] = 99
	first.Typography.Body.Weights[0] = 999
	first.Components["button"] = theme.Component{Height: "1px"}
	first.Color.Dark.Bg = "#000000"

	second := theme.Default()
	if second.Space.Scale[0] != 0.25 {
		t.Errorf("space.scale[0] = %v, want the fresh 0.25", second.Space.Scale[0])
	}
	if second.Typography.Body.Weights[0] != 400 {
		t.Errorf("typography.body.weights[0] = %d, want the fresh 400", second.Typography.Body.Weights[0])
	}
	if _, ok := second.Components["button"]; ok {
		t.Error("the second default carries a component added to the first")
	}
	if second.Color.Dark.Bg != "#171413" {
		t.Errorf("color.dark.bg = %q, want the fresh #171413", second.Color.Dark.Bg)
	}
}

// Clone is what makes "merging does not mutate its inputs" true: the resolver clones the base before
// folding layers onto it, so the copy must not share the slices and maps that a later edit would
// write through. A shallow copy here would corrupt the parent scope's document on every publish.
func TestCloneCopiesTheSlicesAndMaps(t *testing.T) {
	original := theme.Default()
	original.Components = map[string]theme.Component{"button": {Height: "32px"}}
	original.Annotations = map[string]string{"importedFrom": "preset"}

	clone := original.Clone()
	clone.Space.Scale[0] = 99
	clone.Typography.Display.Weights[0] = 999
	clone.Typography.Body.Weights[0] = 999
	clone.Typography.Mono.Weights[0] = 999
	clone.Components["button"] = theme.Component{Height: "1px"}
	clone.Components["input"] = theme.Component{}
	clone.Annotations["importedFrom"] = "edited"
	clone.Color.Dark.Bg = "#000000"

	if original.Space.Scale[0] != 0.25 {
		t.Errorf("the clone shares the space scale with the original: %v", original.Space.Scale[0])
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"typography.display.weights", original.Typography.Display.Weights[0], 600},
		{"typography.body.weights", original.Typography.Body.Weights[0], 400},
		{"typography.mono.weights", original.Typography.Mono.Weights[0], 400},
	} {
		if c.got != c.want {
			t.Errorf("the clone shares %s with the original: %d, want %d", c.name, c.got, c.want)
		}
	}
	if original.Components["button"].Height != "32px" {
		t.Errorf("the clone shares the components map: button.height = %q", original.Components["button"].Height)
	}
	if _, ok := original.Components["input"]; ok {
		t.Error("the clone shares the components map: a key added to the copy appeared in the original")
	}
	if original.Annotations["importedFrom"] != "preset" {
		t.Errorf("the clone shares the annotations map: %q", original.Annotations["importedFrom"])
	}
	if original.Color.Dark.Bg != "#171413" {
		t.Errorf("the clone shares a colour with the original: %q", original.Color.Dark.Bg)
	}
}

// A cloned document must still be the same document: the copy exists so a caller can mutate it, not
// so it can drift. If Clone lost a field, the resolver would publish a theme missing a token.
func TestClonePreservesTheDocument(t *testing.T) {
	original := theme.Default()
	original.Components = map[string]theme.Component{"button": {Height: "32px"}}
	clone := original.Clone()

	want, err := theme.Digest(original)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	got, err := theme.Digest(clone)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != want {
		t.Fatalf("Clone changed the document: %s -> %s", want, got)
	}
}
