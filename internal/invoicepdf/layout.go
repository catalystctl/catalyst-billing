package invoicepdf

// Font is one of the two standard PDF fonts this document uses.
//
// They are among the fourteen fonts every conforming reader must provide, so the
// file needs no embedded font program. That keeps a generated invoice a few
// kilobytes rather than a few hundred, which matters when a customer downloads
// it on a phone and when a year of documents is archived.
type Font int

const (
	// Helvetica is the body face.
	Helvetica Font = iota
	// HelveticaBold is used for headings, column headers and the total.
	HelveticaBold
)

// resourceName is the key the font is registered under in the page resources.
func (f Font) resourceName() string {
	if f == HelveticaBold {
		return "F2"
	}
	return "F1"
}

// baseFont is the PDF base-font name.
func (f Font) baseFont() string {
	if f == HelveticaBold {
		return "Helvetica-Bold"
	}
	return "Helvetica"
}

// widths holds the advance width of every printable ASCII character, in
// thousandths of an em, for each face.
//
// These are the Adobe AFM metrics for Helvetica and Helvetica-Bold. They are
// needed because layout has to know how wide a string is before it decides where
// the next column starts: a right-aligned amount drawn at a guessed x overlaps
// the column beside it. A reader does not require them, but a *document* does.
var widths = map[Font][95]int{
	Helvetica: {
		278, 278, 355, 556, 556, 889, 667, 222, 333, 333, 389, 584, 278, 333, 278, 278,
		556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
		1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
		667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
		222, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
		556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
	},
	HelveticaBold: {
		278, 333, 474, 556, 556, 889, 722, 278, 333, 333, 389, 584, 278, 333, 278, 278,
		556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 333, 333, 584, 584, 584, 611,
		975, 722, 722, 722, 722, 667, 611, 778, 722, 278, 556, 722, 611, 833, 722, 778,
		667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 333, 278, 333, 584, 556,
		278, 556, 611, 556, 611, 556, 333, 611, 611, 278, 278, 556, 278, 889, 611, 611,
		611, 611, 389, 556, 333, 611, 556, 778, 556, 556, 500, 389, 280, 389, 584,
	},
}

// TextWidth returns the width of s in points when set in f at size.
//
// The string is measured after WinAnsi encoding, because that is what is drawn:
// measuring the runes would over-count a character that was transliterated.
func TextWidth(f Font, size float64, s string) float64 {
	table := widths[f]
	total := 0
	for _, b := range encodeWinAnsi(s) {
		if b >= 32 && b <= 126 {
			total += table[b-32]
			continue
		}
		// Outside the table every WinAnsi glyph is roughly the width of 'n',
		// except the spaces, which are already handled above. Guessing is
		// acceptable here because it only affects where a column breaks, never
		// an amount.
		total += 556
	}
	return float64(total) * size / 1000
}

// wrap splits s into lines no wider than maxWidth.
//
// It breaks on spaces and, for a single word longer than the line, on bytes: an
// unbroken 300-character token must not push a description off the page, because
// a document that silently loses a line item is worse than one with an awkward
// break.
func wrap(f Font, size float64, s string, maxWidth float64) []string {
	if s == "" {
		return nil
	}
	if maxWidth <= 0 || TextWidth(f, size, s) <= maxWidth {
		return []string{s}
	}

	var (
		lines []string
		cur   string
	)
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, word := range splitWords(s) {
		candidate := word
		if cur != "" {
			candidate = cur + " " + word
		}
		if TextWidth(f, size, candidate) <= maxWidth {
			cur = candidate
			continue
		}
		flush()
		// The word alone may still be too wide.
		for TextWidth(f, size, word) > maxWidth {
			cut := longestPrefix(f, size, word, maxWidth)
			if cut == 0 {
				// A single glyph wider than the column: emit it anyway so the
				// text is never dropped, then continue with the rest.
				cut = 1
			}
			lines = append(lines, word[:cut])
			word = word[cut:]
		}
		cur = word
	}
	flush()
	if len(lines) == 0 {
		return []string{s}
	}
	return lines
}

// longestPrefix returns the number of bytes of s that fit in maxWidth.
func longestPrefix(f Font, size float64, s string, maxWidth float64) int {
	best := 0
	for i := 1; i <= len(s); i++ {
		if TextWidth(f, size, s[:i]) > maxWidth {
			break
		}
		best = i
	}
	return best
}

// splitWords splits on runs of spaces without dropping a word.
func splitWords(s string) []string {
	var (
		out  []string
		word []byte
	)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if len(word) > 0 {
				out = append(out, string(word))
				word = word[:0]
			}
			continue
		}
		word = append(word, s[i])
	}
	if len(word) > 0 {
		out = append(out, string(word))
	}
	return out
}

// truncate shortens s with an ellipsis so it fits in maxWidth.
//
// It is used where wrapping would change the meaning of a single value — a
// filename, an email address — rather than a prose field.
func truncate(f Font, size float64, s string, maxWidth float64) string {
	if TextWidth(f, size, s) <= maxWidth {
		return s
	}
	const ellipsis = "..."
	if TextWidth(f, size, ellipsis) > maxWidth {
		return ""
	}
	for i := len(s); i > 0; i-- {
		if TextWidth(f, size, s[:i]+ellipsis) <= maxWidth {
			return s[:i] + ellipsis
		}
	}
	return ellipsis
}
