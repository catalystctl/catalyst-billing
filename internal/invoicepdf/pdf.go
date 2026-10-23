// Package invoicepdf renders an issued invoice or credit note as a PDF.
//
// # Why there is no dependency
//
// A PDF is a header, a tree of numbered objects, a cross-reference table and a
// trailer. This document needs two of the fourteen standard fonts, one content
// stream per page, and three operators: show text, draw a line, fill a
// rectangle. Pulling in a toolkit to emit that would add a dependency — and a
// supply-chain surface — to a billing panel whose whole point is that money is
// computed exactly and reproducibly. So the writer is here, it is small, and it
// is tested.
//
// # What a hand-written writer must get right
//
//   - The cross-reference table records the *byte offset* of every object. One
//     miscounted byte and a reader either rejects the file or, worse, renders a
//     different object. Offsets are therefore computed from the serialised bytes
//     themselves, never estimated.
//   - A literal string is delimited by parentheses, so a description containing
//     '(' or ')' — or a backslash — must be escaped. An invoice line
//     description is customer-influenced data: it reaches this package from a
//     cart, an operator's free text or an import. Unescaped, it corrupts the
//     document. Escaping happens in exactly one function, escapeLiteral, and a
//     test drives it with those characters.
//   - Text is bytes, not runes. The standard fonts are addressed through
//     WinAnsiEncoding, so a rune outside that repertoire is transliterated or
//     replaced rather than emitted as UTF-8 (which would render as mojibake).
//
// # Money
//
// Nothing in this package converts money to a float. Amounts arrive as integer
// minor units and are rendered by inserting a decimal mark into the digit
// string (see FormatAmount). The only floats here are page coordinates.
package invoicepdf

import (
	"bytes"
	"fmt"
)

// PDF page geometry, in PostScript points (1/72 inch).
const (
	pageWidth  = 595.0 // A4
	pageHeight = 842.0
)

// pdfWriter accumulates numbered objects and serialises them with a correct
// cross-reference table.
//
// Objects are reserved before they are written because a page must reference its
// content stream and the page tree must reference every page; reserving lets the
// caller learn an object number before the object exists.
type pdfWriter struct {
	objects [][]byte
}

// reserve allocates the next object number (1-based; object 0 is the free-list
// head and is never a real object).
func (w *pdfWriter) reserve() int {
	w.objects = append(w.objects, nil)
	return len(w.objects)
}

// set writes an object's body.
func (w *pdfWriter) set(num int, body []byte) {
	w.objects[num-1] = body
}

// finish serialises every object, the cross-reference table and the trailer.
//
// The returned bytes begin with "%PDF-" as the format requires and end with
// "%%EOF". Nothing after the last object may be inserted before the xref: a
// reader locates it through startxref, and a stray byte shifts every offset that
// follows it.
func (w *pdfWriter) finish(root int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	// A binary comment marks the file as binary for tools that sniff it. It is
	// four bytes above 127, which is exactly what the convention intends.
	buf.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	offsets := make([]int, len(w.objects))
	for i, body := range w.objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", i+1)
		buf.Write(body)
		buf.WriteString("\nendobj\n")
	}

	xrefAt := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(w.objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R >>\n", len(w.objects)+1, root)
	fmt.Fprintf(&buf, "startxref\n%d\n%%%%EOF\n", xrefAt)
	return buf.Bytes()
}

// canvas records the drawing operators of one page.
//
// It is a thin wrapper on purpose: the layout layer decides *what* to draw, and
// this layer only knows how to say it in PostScript. Keeping them apart is what
// makes the layout testable without parsing a content stream.
type canvas struct {
	ops bytes.Buffer
}

// text draws s with its left edge at x and its baseline at y.
func (c *canvas) text(x, y float64, s string, f Font, size float64) {
	if s == "" {
		return
	}
	fmt.Fprintf(&c.ops, "BT /%s %.2f Tf 1 0 0 1 %.2f %.2f Tm (%s) Tj ET\n",
		f.resourceName(), size, x, y, escapeLiteral(s))
}

// textRight draws s with its right edge at xRight.
func (c *canvas) textRight(xRight, y float64, s string, f Font, size float64) {
	c.text(xRight-TextWidth(f, size, s), y, s, f, size)
}

// textCentred draws s centred on x.
func (c *canvas) textCentred(x, y float64, s string, f Font, size float64) {
	c.text(x-TextWidth(f, size, s)/2, y, s, f, size)
}

// line draws a hairline between two points.
func (c *canvas) line(x1, y1, x2, y2 float64, width float64) {
	fmt.Fprintf(&c.ops, "%.2f w 0.72 G %.2f %.2f m %.2f %.2f l S\n", width, x1, y1, x2, y2)
}

// fillRect fills a rectangle with a light grey, used for the table header.
//
// The `q`/`Q` pair is not decoration. The fill colour is part of the graphics state, so a bare
// `g ... re f` leaves the grey set and every glyph drawn afterwards — the column labels, every
// row, the totals, the terms — is painted in it. That was the behaviour before this pair was
// added: an invoice's totals printed at 92% grey, which is legible on screen and nearly
// invisible on paper. The state is now saved and restored, so a fill cannot leak into text.
func (c *canvas) fillRect(x, y, w, h float64, grey float64) {
	fmt.Fprintf(&c.ops, "q %.2f g %.2f %.2f %.2f %.2f re f Q\n", grey, x, y, w, h)
}

// rect strokes a rectangle outline.
func (c *canvas) rect(x, y, w, h, width float64) {
	fmt.Fprintf(&c.ops, "%.2f w 0.5 G %.2f %.2f %.2f %.2f re S\n", width, x, y, w, h)
}

// contentStream wraps the operators in the stream object body.
func (c *canvas) contentStream() []byte {
	body := c.ops.Bytes()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "<< /Length %d >>\nstream\n", len(body))
	buf.Write(body)
	buf.WriteString("endstream")
	return buf.Bytes()
}

// escapeLiteral renders s as the body of a PDF literal string, without the
// surrounding parentheses.
//
// Three characters must be escaped: the backslash itself, and the two
// parentheses that delimit the literal. Nothing else needs to be, but control
// characters are replaced with a space because a raw newline inside a literal is
// legal yet makes the file impossible to inspect, and a NUL truncates it in some
// readers.
func escapeLiteral(s string) string {
	encoded := encodeWinAnsi(s)
	var out bytes.Buffer
	out.Grow(len(encoded))
	for _, b := range encoded {
		switch b {
		case '\\', '(', ')':
			out.WriteByte('\\')
			out.WriteByte(b)
		case '\r', '\n', '\t':
			out.WriteByte(' ')
		default:
			if b < 0x20 || b == 0x7F {
				out.WriteByte(' ')
				continue
			}
			out.WriteByte(b)
		}
	}
	return out.String()
}

// winAnsiExtras maps the Unicode punctuation that appears in real invoices onto
// its WinAnsiEncoding byte. Without this a typographic apostrophe in a product
// name would render as '?', which is the kind of detail a customer notices on a
// document they are asked to pay.
var winAnsiExtras = map[rune]byte{
	'\u20ac': 0x80, // €
	'\u201a': 0x82,
	'\u0192': 0x83,
	'\u201e': 0x84,
	'\u2026': 0x85, // …
	'\u2020': 0x86,
	'\u2021': 0x87,
	'\u02c6': 0x88,
	'\u2030': 0x89,
	'\u0160': 0x8A,
	'\u2039': 0x8B,
	'\u0152': 0x8C,
	'\u017d': 0x8E,
	'\u2018': 0x91, // ‘
	'\u2019': 0x92, // ’
	'\u201c': 0x93, // “
	'\u201d': 0x94, // ”
	'\u2022': 0x95, // •
	'\u2013': 0x96, // –
	'\u2014': 0x97, // —
	'\u02dc': 0x98,
	'\u2122': 0x99, // ™
	'\u0161': 0x9A,
	'\u203a': 0x9B,
	'\u0153': 0x9C,
	'\u017e': 0x9E,
	'\u0178': 0x9F,
	'\u00a0': ' ', // no-break space
	'\u00ad': '-', // soft hyphen
}

// encodeWinAnsi converts a Go string to the single-byte encoding the standard
// fonts are addressed through.
//
// Run es outside Latin-1 that have no transliteration become '?' rather than
// being emitted as UTF-8: two UTF-8 bytes inside a single-byte font render as
// two wrong glyphs, which looks like a broken document rather than an
// unsupported character.
func encodeWinAnsi(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x20:
			out = append(out, ' ')
		case r < 0x7F:
			out = append(out, byte(r))
		case r >= 0xA0 && r <= 0xFF:
			out = append(out, byte(r))
		default:
			if b, ok := winAnsiExtras[r]; ok {
				out = append(out, b)
				continue
			}
			out = append(out, '?')
		}
	}
	return out
}
