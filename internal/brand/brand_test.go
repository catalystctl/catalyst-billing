package brand_test

import (
	"testing"

	"github.com/catalyst/billing/internal/brand"
)

func TestParseHexAcceptsTheFormsAnOperatorTypes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want brand.Colour
	}{
		{"#3b5bdb", brand.Colour{R: 0x3b, G: 0x5b, B: 0xdb}},
		{"3b5bdb", brand.Colour{R: 0x3b, G: 0x5b, B: 0xdb}},
		{"  #3B5BDB ", brand.Colour{R: 0x3b, G: 0x5b, B: 0xdb}},
		// Three digits expand each nibble rather than zero-extending it: #abc is #aabbcc.
		{"#abc", brand.Colour{R: 0xaa, G: 0xbb, B: 0xcc}},
		{"#000", brand.Colour{}},
	} {
		got, err := brand.ParseHex(tc.in)
		if err != nil {
			t.Fatalf("ParseHex(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseHex(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseHexRejectsWhatIsNotAColour(t *testing.T) {
	// Each of these is something a form field actually receives. A colour that is not a
	// colour must be refused at the edge: rendered downstream it becomes black or a panic.
	for _, in := range []string{
		"", "   ", "#", "3b5bd", "#3b5bdba", "#3b5bdg", "rgb(59,91,219)", "rebeccapurple",
		"#3b5bdb;", "url(x)", "0x3b5bdb", "#+3b5bd",
	} {
		if _, err := brand.ParseHex(in); err == nil {
			t.Fatalf("ParseHex(%q) accepted a value that is not a hex colour", in)
		}
	}
}

func TestHexRoundTrips(t *testing.T) {
	for _, in := range []string{"#3b5bdb", "#7b95ff", "#000000", "#ffffff", "#c0392b"} {
		c, err := brand.ParseHex(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := c.Hex(); got != in {
			t.Fatalf("%q round-tripped to %q", in, got)
		}
	}
}

// The two built-in accents are the reference the derivation has to beat: if a brand colour
// could not be read where the built-in one can, the brand would be a regression.
func TestContrastMatchesTheBuiltInPalette(t *testing.T) {
	white := brand.Colour{R: 0xff, G: 0xff, B: 0xff}
	dark := brand.Colour{R: 0x0a, G: 0x0d, B: 0x13}

	lightAccent, _ := brand.ParseHex("#3b5bdb")
	darkAccent, _ := brand.ParseHex("#7b95ff")

	// The light theme's accent is chosen to read on white, and its ink is white.
	if ratio := brand.Contrast(lightAccent, white); ratio < 4.5 {
		t.Fatalf("the light accent fails AA on white: %.2f", ratio)
	}
	if ink := brand.Ink(lightAccent); ink != white {
		t.Fatalf("Ink(#3b5bdb) = %v, want white", ink)
	}
	// The dark theme's accent is chosen to read on near-black, and its ink is near-black.
	if ratio := brand.Contrast(darkAccent, dark); ratio < 4.5 {
		t.Fatalf("the dark accent fails AA on the dark background: %.2f", ratio)
	}
	if ink := brand.Ink(darkAccent); ink != dark {
		t.Fatalf("Ink(#7b95ff) = %v, want the dark surface colour", ink)
	}
}

func TestContrastIsSymmetricAndBounded(t *testing.T) {
	a, _ := brand.ParseHex("#3b5bdb")
	b, _ := brand.ParseHex("#ffffff")
	if brand.Contrast(a, b) != brand.Contrast(b, a) {
		t.Fatal("contrast is not symmetric")
	}
	if got := brand.Contrast(b, b); got != 1 {
		t.Fatalf("a colour against itself has contrast %.3f, want 1", got)
	}
	black := brand.Colour{}
	if got := brand.Contrast(black, b); got < 20 || got > 21 {
		t.Fatalf("black on white has contrast %.3f, want ~21", got)
	}
}

func TestReadableLeavesAnAccessibleColourAlone(t *testing.T) {
	white := brand.Colour{R: 0xff, G: 0xff, B: 0xff}
	ok, _ := brand.ParseHex("#3b5bdb")
	if got := brand.Readable(ok, white); got != ok {
		t.Fatalf("a colour that already passes was changed to %v", got)
	}
}

func TestReadableDarkensAPaleColourUntilItReads(t *testing.T) {
	white := brand.Colour{R: 0xff, G: 0xff, B: 0xff}
	pale, _ := brand.ParseHex("#f1e05a")
	got := brand.Readable(pale, white)
	if ratio := brand.Contrast(got, white); ratio < brand.MinTextContrast {
		t.Fatalf("Readable left the colour at contrast %.2f", ratio)
	}
	// The adjustment must stay on the same hue: a "readable" grey would be a different
	// brand colour, which is not what the tenant chose.
	if got.R < got.G {
		t.Fatalf("the hue was not preserved: %v", got)
	}
	if got == pale {
		t.Fatal("the pale colour was returned unchanged")
	}
}

func TestReadableLightensADarkColourOnADarkBackground(t *testing.T) {
	dark := brand.Colour{R: 0x0a, G: 0x0d, B: 0x13}
	midnight, _ := brand.ParseHex("#0b1d4a")
	got := brand.Readable(midnight, dark)
	if ratio := brand.Contrast(got, dark); ratio < brand.MinTextContrast {
		t.Fatalf("Readable left the colour at contrast %.2f", ratio)
	}
	if got.Luminance() <= midnight.Luminance() {
		t.Fatal("the colour was not lightened on a dark background")
	}
}

func TestMixEndsAreExact(t *testing.T) {
	a, _ := brand.ParseHex("#000000")
	b, _ := brand.ParseHex("#ffffff")
	if got := brand.Mix(a, b, 0); got != a {
		t.Fatalf("Mix at 0 = %v, want the start colour", got)
	}
	if got := brand.Mix(a, b, 1); got != b {
		t.Fatalf("Mix at 1 = %v, want the end colour", got)
	}
	mid := brand.Mix(a, b, 0.5)
	if mid.R < 0x7e || mid.R > 0x80 {
		t.Fatalf("Mix at 0.5 = %v, want mid-grey", mid)
	}
}

// A brand with no accent must report "not configured" for both modes, which is what makes
// the fallback expressible: the caller has nothing to draw.
func TestZeroBrandConfiguresNothing(t *testing.T) {
	var b brand.Brand
	if !b.IsZero() {
		t.Fatal("the zero brand is not zero")
	}
	for _, dark := range []bool{false, true} {
		if _, ok := b.Accent(dark); ok {
			t.Fatalf("the zero brand reported an accent (dark=%v)", dark)
		}
	}
	if b.Name != "" || b.LogoID != "" {
		t.Fatal("the zero brand carries values")
	}
}

func TestBrandAccentIsPerMode(t *testing.T) {
	b := brand.Brand{AccentLight: "#3b5bdb", AccentDark: "#7b95ff"}
	light, ok := b.Accent(false)
	if !ok || light.Hex() != "#3b5bdb" {
		t.Fatalf("light accent = %v, %v", light, ok)
	}
	dark, ok := b.Accent(true)
	if !ok || dark.Hex() != "#7b95ff" {
		t.Fatalf("dark accent = %v, %v", dark, ok)
	}
	// One mode configured, the other not: the unconfigured mode falls back on its own.
	only := brand.Brand{AccentLight: "#3b5bdb"}
	if _, ok := only.Accent(true); ok {
		t.Fatal("a dark accent was reported although only the light one is configured")
	}
	if only.IsZero() {
		t.Fatal("a brand with one accent reports as zero")
	}
}

// A row written by hand can hold anything, and a document must still print.
func TestMalformedStoredAccentReadsAsUnset(t *testing.T) {
	b := brand.Brand{AccentLight: "chartreuse", AccentDark: "#7b95ff"}
	if _, ok := b.Accent(false); ok {
		t.Fatal("a malformed accent was accepted")
	}
	if _, ok := b.Accent(true); !ok {
		t.Fatal("a valid accent in the other mode was dropped")
	}
}
