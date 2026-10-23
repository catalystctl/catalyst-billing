package invoicepdf

// The tenant's brand, as a document renderer needs it.
//
// # Why this is an overlay and not a second layout
//
// An invoice is a legal document. Its columns, its totals and its tax evidence are the
// product of a snapshot that was frozen when the document was issued; a brand must not be
// able to move any of that. So the brand adds colour and a mark to the existing layout and
// changes nothing about where the facts are printed:
//
//   - a colour band above the letterhead, which costs no vertical space because it sits in
//     the top margin;
//   - the letterhead rule and the table's header band take the accent instead of grey;
//   - the document title takes a darkened accent, so a pale brand colour cannot print an
//     illegible heading;
//   - the logo is drawn above the merchant name, and the letterhead moves down only by the
//     height of the logo — which is zero when there is no logo.
//
// # Why an unset brand must be byte-identical
//
// Every drawing call below is conditional on a configured value. A zero Brand therefore
// emits exactly the operators the renderer emitted before this file existed — no empty
// `q`/`Q` pair, no unused resource, not one byte of difference. That property is pinned by a
// test that hashes the output of a document with no brand and compares it against the hash
// recorded from the renderer before branding existed; without it, "unset falls back" would
// be a claim rather than a fact.

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	// The two formats a logo is allowed to be. They are registered here rather than in the
	// caller so that a stored image cannot reach the renderer in a format it will not print.
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/catalyst/billing/internal/brand"
)

// Brand is the overlay. The zero value means "no brand configured".
type Brand struct {
	// Name, when set, replaces the merchant name in the letterhead and the footer. It is the
	// tenant's trading name, which is what a customer expects to see above their address.
	Name string
	// Accent is the document accent. Its zero value means "use the built-in grey".
	Accent brand.Colour
	// Logo, when non-nil, is drawn above the merchant name.
	Logo *Logo
}

// Logo is a stored image, undecoded. Decoding happens here so that a corrupt image is a
// missing logo rather than a failed document.
type Logo struct {
	ContentType string
	Bytes       []byte
}

// IsZero reports whether nothing is configured, which is what makes the renderer's
// byte-identical fallback expressible.
func (b Brand) IsZero() bool {
	return b.Name == "" && b.Accent.IsZero() && b.Logo == nil
}

// Geometry of the brand in the header, in points.
const (
	// logoMaxWidth and logoMaxHeight bound the drawn logo. They are deliberately small: a
	// logo is an identifier, not an illustration, and a letterhead that is mostly logo
	// pushes the parties down the page.
	logoMaxWidth  = 150.0
	logoMaxHeight = 38.0
	// accentBandHeight is the colour band at the top of the first page. It sits inside the
	// top margin, so it costs no space in the content area.
	accentBandHeight = 4.0
	// accentBandTop is where the band's top edge sits, measured from the page top.
	accentBandTop = 26.0
	// accentRuleWidth is the letterhead rule's stroke when an accent is configured.
	accentRuleWidth = 1.6
)

// paperWhite is the colour the document is printed on. It is what a transparent logo is
// composited against and what a text colour is measured for contrast against.
var paperWhite = brand.Colour{R: 0xff, G: 0xff, B: 0xff}

// logoImage is a decoded logo: raw RGB bytes, row-major, top row first.
type logoImage struct {
	width, height int
	rgb           []byte
}

// decodeLogo turns stored bytes into raw RGB, compositing any alpha over white.
//
// A failure is not an error the caller has to handle: a logo that cannot be decoded must not
// stop an invoice from being printed, so the renderer treats it as "no logo" and the
// document is the unbranded one. The upload path validates the image too, so this only
// catches a row that was written by hand or an image format that lost support.
func decodeLogo(l *Logo) (logoImage, bool) {
	if l == nil || len(l.Bytes) == 0 {
		return logoImage{}, false
	}
	src, _, err := image.Decode(bytes.NewReader(l.Bytes))
	if err != nil {
		return logoImage{}, false
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return logoImage{}, false
	}
	// A logo larger than this is not a logo, and expanding it would cost memory on a request
	// path that renders a customer's document.
	const maxPixels = 4_000_000
	if w*h > maxPixels {
		return logoImage{}, false
	}

	out := make([]byte, 0, w*h*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r16, g16, b16, a16 := src.At(x, y).RGBA()
			// Composite over white in 16-bit space, then take the high byte. Doing it in
			// float would round differently; doing it in 8-bit would quantise a
			// semi-transparent edge into a visible fringe.
			if a16 == 0 {
				out = append(out, 0xff, 0xff, 0xff)
				continue
			}
			blend := func(v uint32) byte {
				// v is premultiplied by alpha (image.Image's contract), so the white
				// backing contributes (0xffff - a16) before dividing back out.
				unpremultiplied := (v*0xffff + 0x7fff) / a16
				if unpremultiplied > 0xffff {
					unpremultiplied = 0xffff
				}
				mixed := (unpremultiplied*a16 + 0xffff*(0xffff-a16)) / 0xffff
				return byte(mixed >> 8)
			}
			out = append(out, blend(r16), blend(g16), blend(b16))
		}
	}
	return logoImage{width: w, height: h, rgb: out}, true
}

// drawnLogoSize is the size the logo occupies on the page, preserving its aspect ratio.
func drawnLogoSize(img logoImage) (w, h float64) {
	scale := math.Min(logoMaxWidth/float64(img.width), logoMaxHeight/float64(img.height))
	if scale <= 0 || math.IsInf(scale, 0) {
		return 0, 0
	}
	return float64(img.width) * scale, float64(img.height) * scale
}

// logoObject serialises the image as an XObject stream.
//
// The pixels are re-encoded with FlateDecode rather than passed through: a JPEG could be
// embedded verbatim with DCTDecode, but a PNG could not, and one code path that handles
// both is worth more here than the few kilobytes a passthrough would save. A logo is a
// handful of kilobytes either way.
func logoObject(img logoImage) ([]byte, error) {
	var raw bytes.Buffer
	zw, err := zlib.NewWriterLevel(&raw, zlib.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("invoicepdf: compress logo: %w", err)
	}
	if _, err := zw.Write(img.rgb); err != nil {
		return nil, fmt.Errorf("invoicepdf: compress logo: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("invoicepdf: compress logo: %w", err)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		"<< /Type /XObject /Subtype /Image /Width %d /Height %d "+
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n",
		img.width, img.height, raw.Len())
	buf.Write(raw.Bytes())
	buf.WriteString("\nendstream")
	return buf.Bytes(), nil
}

// logoResourceName is the key the logo is registered under in each page's resources.
const logoResourceName = "Im1"

// drawImage places the logo. The unit square the XObject is defined in is mapped to the
// drawn box with `cm`, so the operator is the same whatever the image's pixel size.
func (c *canvas) drawImage(x, y, w, h float64, name string) {
	fmt.Fprintf(&c.ops, "q %.2f 0 0 %.2f %.2f %.2f cm /%s Do Q\n", w, h, x, y, name)
}

// textIn draws s in a colour, restoring black afterwards.
//
// The colour is set inside `q`/`Q` rather than left set: a fill colour is part of the
// graphics state and would otherwise leak into every operator after it, which would print
// the rest of the document in the brand colour.
func (c *canvas) textIn(x, y float64, s string, f Font, size float64, col brand.Colour) {
	if s == "" {
		return
	}
	fmt.Fprintf(&c.ops, "q %.3f %.3f %.3f rg BT /%s %.2f Tf 1 0 0 1 %.2f %.2f Tm (%s) Tj ET Q\n",
		float64(col.R)/255, float64(col.G)/255, float64(col.B)/255,
		f.resourceName(), size, x, y, escapeLiteral(s))
}

// fillRectIn fills a rectangle with a colour.
func (c *canvas) fillRectIn(x, y, w, h float64, col brand.Colour) {
	fmt.Fprintf(&c.ops, "q %.3f %.3f %.3f rg %.2f %.2f %.2f %.2f re f Q\n",
		float64(col.R)/255, float64(col.G)/255, float64(col.B)/255, x, y, w, h)
}

// lineIn draws a hairline in a colour.
func (c *canvas) lineIn(x1, y1, x2, y2 float64, width float64, col brand.Colour) {
	fmt.Fprintf(&c.ops, "q %.2f w %.3f %.3f %.3f RG %.2f %.2f m %.2f %.2f l S Q\n",
		width, float64(col.R)/255, float64(col.G)/255, float64(col.B)/255, x1, y1, x2, y2)
}
