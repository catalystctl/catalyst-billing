// Package brand is a tenant's brand overlay: a name, a logo and two accent colours.
//
// # What this is not
//
// It is not a theme registry. There is one brand per installation, it is a handful of
// values, and it sits *on top of* the two colour themes the panel already has (light and
// dark). A theme registry — many named themes, each a complete set of tokens, selectable per
// user — is a much larger thing: it needs a token schema, a store, a preview surface and a
// migration story for every token added later. That is not what a tenant needs to look like
// themselves. What a tenant needs is their logo, their name and their colour, in both modes,
// and that is exactly what this package models.
//
// # Why the accent is two colours
//
// The panel has two themes and they are not the same colour space. The default accent is
// #3b5bdb in light mode and #7b95ff in dark mode — the same intent at two luminances,
// because a mid-tone blue that reads well on white is too dark on #0a0d13 and a pale blue
// that reads well on near-black is invisible on white. A single stored accent would
// therefore be legible in one theme and washed out in the other. So a brand carries one
// accent per mode. Either may be empty, and an empty accent means "leave the built-in
// colour alone" — which is what makes an unset brand a byte-identical fallback rather than
// a brand that happens to match.
//
// # Why the colour maths is here and not in the renderers
//
// Two renderers need it: the invoice PDF (which prints an accent rule and a title on white
// paper) and the panel (which derives hover, press and soft variants in CSS). The web copy
// is a deliberate mirror in web/src/lib/brand.ts — it cannot import Go — and both are pinned
// by tests so the two do not drift.
package brand

import (
	"fmt"
	"math"
	"strings"
)

// Colour is an sRGB triple. It is stored as three bytes rather than as a string so that a
// malformed value cannot travel any further than the parser.
type Colour struct {
	R, G, B uint8
}

// ParseHex reads "#rgb" or "#rrggbb". The leading "#" is optional, because a form field that
// requires it is a form field people get wrong for no benefit.
//
// Anything else is an error: a colour that is not a colour must be refused at the edge, not
// rendered as black somewhere downstream.
func ParseHex(s string) (Colour, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "#")
	switch len(raw) {
	case 3:
		// "#abc" is "#aabbcc": each nibble is duplicated, not zero-extended.
		var out Colour
		for i, c := range []byte{raw[0], raw[1], raw[2]} {
			v, ok := hexNibble(c)
			if !ok {
				return Colour{}, fmt.Errorf("brand: %q is not a hex colour", s)
			}
			full := v<<4 | v
			switch i {
			case 0:
				out.R = full
			case 1:
				out.G = full
			case 2:
				out.B = full
			}
		}
		return out, nil
	case 6:
		var out Colour
		for i := 0; i < 3; i++ {
			hi, ok1 := hexNibble(raw[i*2])
			lo, ok2 := hexNibble(raw[i*2+1])
			if !ok1 || !ok2 {
				return Colour{}, fmt.Errorf("brand: %q is not a hex colour", s)
			}
			full := hi<<4 | lo
			switch i {
			case 0:
				out.R = full
			case 1:
				out.G = full
			case 2:
				out.B = full
			}
		}
		return out, nil
	default:
		return Colour{}, fmt.Errorf("brand: %q is not a hex colour", s)
	}
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// Hex renders the colour as "#rrggbb", which is the form the settings table stores and the
// form the panel's stylesheet accepts.
func (c Colour) Hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// IsZero reports whether the colour is the zero value, i.e. "not set". It exists so a
// caller can say "no accent" without a pointer, and so #000000 remains expressible.
func (c Colour) IsZero() bool { return c == Colour{} }

// channel converts an sRGB byte to the linear-light fraction WCAG defines.
func channel(v uint8) float64 {
	c := float64(v) / 255
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// Luminance is the WCAG 2.1 relative luminance of the colour.
func (c Colour) Luminance() float64 {
	return 0.2126*channel(c.R) + 0.7152*channel(c.G) + 0.0722*channel(c.B)
}

// Contrast is the WCAG contrast ratio between two colours, from 1 to 21.
func Contrast(a, b Colour) float64 {
	la, lb := a.Luminance(), b.Luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Ink is the colour text should take when it sits *on* c: white or near-black, whichever
// contrasts more.
//
// The default panel tokens already encode this (--accent-ink is #0a0d13 in dark mode and
// #ffffff in light mode), so a brand that sets no ink gets the same rule.
func Ink(c Colour) Colour {
	white := Colour{0xff, 0xff, 0xff}
	black := Colour{0x0a, 0x0d, 0x13}
	if Contrast(white, c) >= Contrast(black, c) {
		return white
	}
	return black
}

// MinTextContrast is the ratio a colour must reach to be used for text. 4.5:1 is WCAG AA
// for body text; anything less and a tenant's brand colour turns the invoice's own heading
// into a grey smudge.
const MinTextContrast = 4.5

// Readable returns c adjusted towards white or black until it reaches MinTextContrast
// against bg, or the closest it can get.
//
// This is what keeps a tenant from printing an unreadable invoice. A pale yellow accent is a
// legitimate brand colour and an illegible heading, so the *document* uses a darkened
// version of it: the brand is honoured, the paper stays readable. The adjustment is towards
// whichever of black or white the background is not, so the hue is preserved rather than
// replaced.
func Readable(c, bg Colour) Colour {
	if Contrast(c, bg) >= MinTextContrast {
		return c
	}
	target := Colour{0, 0, 0}
	if bg.Luminance() < 0.5 {
		target = Colour{0xff, 0xff, 0xff}
	}
	best := c
	bestRatio := Contrast(c, bg)
	// Twenty steps of 5% is enough to cross 4.5:1 from any starting point, and it stops
	// immediately once the target is met, so a colour that already passes is untouched.
	for step := 1; step <= 20; step++ {
		mixed := Mix(c, target, float64(step)*0.05)
		if ratio := Contrast(mixed, bg); ratio > bestRatio {
			best, bestRatio = mixed, ratio
			if ratio >= MinTextContrast {
				break
			}
		}
	}
	return best
}

// Mix blends a towards b by t (0..1), channel by channel.
//
// It is exported because the document renderer needs the same blend: a table header takes a
// tint of the brand accent rather than the accent itself, and two implementations of "86%
// white" would be two slightly different greys on the same page.
func Mix(a, b Colour, t float64) Colour {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	lerp := func(x, y uint8) uint8 {
		return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t))
	}
	return Colour{R: lerp(a.R, b.R), G: lerp(a.G, b.G), B: lerp(a.B, b.B)}
}

// Brand is the overlay itself.
//
// Every field's zero value means "not configured", and a zero Brand must change nothing:
// that is the property that makes an installation which never opened the branding screen
// look exactly as it did before the screen existed.
type Brand struct {
	// Name replaces the merchant name in the panel chrome and on the invoice letterhead.
	Name string
	// AccentLight is the accent used in light mode. Empty means the built-in one.
	AccentLight string
	// AccentDark is the accent used in dark mode. Empty means the built-in one.
	AccentDark string
	// LogoID names a stored image (product_images.id) served by /api/v1/media/{id}.
	LogoID string
}

// IsZero reports whether nothing is configured. A zero brand must be a no-op everywhere.
func (b Brand) IsZero() bool {
	return b.Name == "" && b.AccentLight == "" && b.AccentDark == "" && b.LogoID == ""
}

// Accent returns the parsed accent for one mode, and whether one is configured.
//
// A malformed stored value reads as "not configured" rather than as an error: the value is
// validated on write, so a malformed one means a row written by hand, and a document must
// still print.
func (b Brand) Accent(dark bool) (Colour, bool) {
	raw := b.AccentLight
	if dark {
		raw = b.AccentDark
	}
	if strings.TrimSpace(raw) == "" {
		return Colour{}, false
	}
	c, err := ParseHex(raw)
	if err != nil {
		return Colour{}, false
	}
	return c, true
}
