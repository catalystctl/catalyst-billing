package presets_test

import (
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/css"
	"github.com/catalyst/billing/internal/theme/presets"
)

// The `catalyst-billing` preset must render the built-in default's stylesheet exactly, so that
// applying it cannot change a single declaration an installation already serves.
//
// The comparison strips the generated header comment, which carries the document digest: the preset
// states two values the default derives (`color.brand.secondary`, which resolves to the same colour
// the default computes from its border, and a description), so the digests differ while every
// declaration is identical. Asserting on declarations rather than on bytes is what makes the test
// about the thing that matters — the page — instead of about a comment.
func TestBillingPresetEqualsTheDefault(t *testing.T) {
	doc, err := presets.Document("catalyst-billing")
	if err != nil {
		t.Fatal(err)
	}
	want, err := css.Generate(theme.Default())
	if err != nil {
		t.Fatal(err)
	}
	got, err := css.Generate(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, want = stripHeader(got), stripHeader(want)
	if string(got) != string(want) {
		// Show the first differing line, which is the one that matters.
		gl, wl := splitLines(string(got)), splitLines(string(want))
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("line %d differs:\n preset: %s\n default: %s", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("the preset stylesheet is %d lines, the default is %d", len(gl), len(wl))
	}
}

// stripHeader drops the generated header comment, up to and including its closing line.
func stripHeader(sheet []byte) []byte {
	const marker = " */\n"
	if i := strings.Index(string(sheet), marker); i >= 0 {
		return sheet[i+len(marker):]
	}
	return sheet
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
