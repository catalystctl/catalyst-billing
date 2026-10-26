package theme_test

import (
	"regexp"
	"testing"

	"github.com/catalyst/billing/internal/theme"
)

// digestPattern is the contract the digest is consumed under: it goes in a cache key and a URL, so
// a different shape (a bare hex string, a base64 body) would break the route that parses it.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// The digest answers "is this the same theme as that one" for the cache key, the duplicate-import
// check and the history screen. Two documents with identical content must produce identical
// digests regardless of how many times they are built, or an unchanged theme would look edited on
// every save and every import would create a second revision.
func TestDigestIsStable(t *testing.T) {
	first := theme.Default()
	second := theme.Default()

	a, err := theme.Digest(first)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	b, err := theme.Digest(first)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if a != b {
		t.Fatalf("two calls on the same document disagree: %s vs %s", a, b)
	}

	c, err := theme.Digest(second)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if a != c {
		t.Fatalf("two independently built identical documents disagree: %s vs %s", a, c)
	}

	if !digestPattern.MatchString(a) {
		t.Fatalf("digest %q does not match %s", a, digestPattern)
	}
}

// The canonical form is `encoding/json`, which sorts object keys — so two spellings of the same
// document (the order an editor or a hand-written preset happens to use) must hash identically.
// Otherwise re-importing an exported theme would look like a change and create a spurious revision.
func TestDigestIgnoresKeyOrder(t *testing.T) {
	// The same document, spelled with the keys in two different orders.
	const spellingA = `{
		"schema": 2,
		"id": "order-test",
		"name": "Order Test",
		"color": {"mode": "dark", "brand": {"primary": "#ff951e"}, "dark": {"bg": "#171413"}},
		"space": {"unit": 4, "density": "compact", "scale": [0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4]}
	}`
	const spellingB = `{
		"space": {"scale": [0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4], "density": "compact", "unit": 4},
		"name": "Order Test",
		"id": "order-test",
		"color": {"dark": {"bg": "#171413"}, "brand": {"primary": "#ff951e"}, "mode": "dark"},
		"schema": 2
	}`

	a, err := theme.Decode([]byte(spellingA))
	if err != nil {
		t.Fatalf("Decode spelling A: %v", err)
	}
	b, err := theme.Decode([]byte(spellingB))
	if err != nil {
		t.Fatalf("Decode spelling B: %v", err)
	}

	digestA, err := theme.Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	digestB, err := theme.Digest(b)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if digestA != digestB {
		t.Fatalf("key order changed the digest: %s vs %s", digestA, digestB)
	}

	canonicalA, err := theme.Canonical(a)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	canonicalB, err := theme.Canonical(b)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if string(canonicalA) != string(canonicalB) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", canonicalA, canonicalB)
	}
}

// A digest that did not change with a token would make the cache key, the duplicate-import check and
// the "no change" detection all lie at once, so the single-token change is the case that has to
// produce a different hash.
func TestDigestChangesWhenATokenChanges(t *testing.T) {
	base := theme.Default()
	baseDigest, err := theme.Digest(base)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	changes := []struct {
		name string
		mut  func(*theme.Document)
	}{
		{"one palette colour", func(d *theme.Document) { d.Color.Dark.Bg = "#000001" }},
		{"one type-scale step", func(d *theme.Document) { d.Typography.Scale.Base = "0.875rem" }},
		{"one space step", func(d *theme.Document) { d.Space.Scale[3] = 1.25 }},
		{"one shell dimension", func(d *theme.Document) { d.Layout.Shell.SidebarWidth = "260px" }},
		{"the name", func(d *theme.Document) { d.Name = "Renamed" }},
		{"the custom CSS", func(d *theme.Document) { d.CustomCSS = ".card { border-radius: 0; }" }},
	}
	seen := map[string]string{baseDigest: "the unmodified default"}
	for _, c := range changes {
		t.Run("changing "+c.name+" changes the digest", func(t *testing.T) {
			doc := base.Clone()
			c.mut(&doc)
			got, err := theme.Digest(doc)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if got == baseDigest {
				t.Fatalf("the digest did not change when %s changed", c.name)
			}
			if other, clash := seen[got]; clash {
				t.Fatalf("%s and %s produced the same digest", c.name, other)
			}
			seen[got] = c.name
		})
	}
}

// An import is idempotent by digest, and the stored bytes are the canonical bytes, so decoding them
// has to yield the same document. If the round trip lost a field, re-importing an exported theme
// would produce a different digest and a duplicate revision.
func TestDecodeOfCanonicalFormRoundTrips(t *testing.T) {
	original := theme.Default()
	original.CustomCSS = `.card { border-radius: 0; }`
	original.Components = map[string]theme.Component{"button": {Height: "32px", Uppercase: true}}

	canonical, err := theme.Canonical(original)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	decoded, err := theme.Decode(canonical)
	if err != nil {
		t.Fatalf("Decode(Canonical(doc)): %v", err)
	}

	want, err := theme.Digest(original)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	got, err := theme.Digest(decoded)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != want {
		t.Fatalf("the round trip changed the digest: %s -> %s", want, got)
	}
	if err := theme.ValidateResolved(decoded); err != nil {
		t.Fatalf("the round-tripped document is not renderable: %v", err)
	}
	if decoded.CustomCSS != original.CustomCSS {
		t.Errorf("customCss = %q, want %q", decoded.CustomCSS, original.CustomCSS)
	}
	if decoded.Components["button"].Height != "32px" {
		t.Errorf("components.button.height = %q, want 32px", decoded.Components["button"].Height)
	}
	if decoded.Inherit != nil || original.Inherit != nil {
		t.Errorf("inherit did not round-trip as absent: %v", decoded.Inherit)
	}

	// The canonical bytes must themselves be stable, or two encodes of the same document would
	// produce different digests and the cache key would move on every request.
	again, err := theme.Canonical(decoded)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if string(again) != string(canonical) {
		t.Fatalf("Canonical is not idempotent:\n%s\n%s", canonical, again)
	}
}
