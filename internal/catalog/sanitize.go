// A sanitiser for the one field in the catalog that is markup: a product description.
//
// # Why this exists rather than a library
//
// A product description is written by staff, shown to visitors, and stored as HTML. "Staff"
// is not a trust boundary in a panel where a compromised operator account is the threat
// model: an account with `catalog:write` could otherwise store a script that runs in every
// visitor's browser on the storefront, and — because the panel and the storefront share an
// origin — with whatever session that visitor holds. The panel's CSP (`script-src 'self'`,
// no `unsafe-inline`) is the second line of defence; this is the first.
//
// The allowlist is a closed set written out below. The parser is deliberately small and
// strict: it does not try to be an HTML5 parser, it refuses anything it does not recognise,
// and it re-emits what it keeps from scratch rather than passing the input through. That
// combination is what makes `<img src=x onerror=alert(1)>`, `<script>`, and
// `href="javascript:..."` neutralised rather than merely discouraged: the tag, the attribute
// or the whole link does not survive into the stored value.
//
// Two rules matter more than the rest and are easy to get wrong:
//
//   - **A URL is validated after its entities are decoded.** A browser decodes
//     `&#106;avascript:` in an attribute before it resolves the scheme, so validating the raw
//     attribute text is validating a different string than the one that will run.
//   - **Text is never escaped on the way out, because it is never re-parsed on the way in.**
//     The scanner splits the input on `<`, so a text run cannot contain a tag start; keeping
//     `&amp;` as written means an operator's entity survives, and it cannot become markup.
package catalog

import (
	"fmt"
	"strings"
)

// maxDescriptionBytes bounds a rich description. A product page is a page, not a document
// store; the cap keeps one field from being able to bloat every storefront response.
const maxDescriptionBytes = 64 << 10

// allowedElements is the closed set of tags that survive. Everything else — script, style,
// iframe, object, form, input, svg, math, and every custom element — is dropped along with
// its content when it is a raw-text element, and dropped on its own otherwise.
//
// The set is what a product page actually needs: paragraphs, headings, emphasis, lists,
// quotes, code, a link, an image from our own media endpoint, and enough structure for a
// rich-text editor to round-trip.
var allowedElements = map[string]bool{
	"p": true, "br": true, "hr": true, "div": true, "span": true,
	"strong": true, "b": true, "em": true, "i": true, "u": true, "s": true,
	"ul": true, "ol": true, "li": true,
	"h2": true, "h3": true, "h4": true,
	"blockquote": true, "code": true, "pre": true,
	"a": true, "img": true,
}

// voidElements have no closing tag and no children.
var voidElements = map[string]bool{"br": true, "hr": true, "img": true}

// droppedWithContent are elements whose *content* must go too. A `<script>` or `<style>`
// body is not text a visitor should read, and a `<title>`/`<textarea>` body would otherwise
// be emitted as prose.
var droppedWithContent = map[string]bool{
	"script": true, "style": true, "title": true, "textarea": true,
	"iframe": true, "object": true, "noscript": true, "template": true,
	"svg": true, "math": true,
}

// SanitizeHTML returns the subset of `in` that is in the allowlist above.
//
// It is idempotent: sanitising its own output returns the same string. That matters because
// the stored value is also the value a client renders, so a second pass (the browser's own
// sanitiser in web/src/lib/richtext.ts) must not change it.
func SanitizeHTML(in string) (string, error) {
	if len(in) > maxDescriptionBytes {
		return "", fmt.Errorf("%w: a description may not exceed %d bytes", ErrInvalidInput, maxDescriptionBytes)
	}
	var out strings.Builder
	// open tracks the elements we have emitted and not yet closed, so a stray `</div>` is
	// dropped instead of escaping the container the renderer puts the description in.
	var open []string
	s := in
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:i])
		s = s[i:]

		// A comment, CDATA section or doctype is not something a description needs, and
		// `<!--[if IE]><script>` is the classic way to smuggle markup past a naive filter.
		if strings.HasPrefix(s, "<!--") {
			end := strings.Index(s, "-->")
			if end < 0 {
				s = ""
				break
			}
			s = s[end+3:]
			continue
		}
		if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") {
			end := strings.IndexByte(s, '>')
			if end < 0 {
				s = ""
				break
			}
			s = s[end+1:]
			continue
		}

		end, ok := tagEnd(s)
		if !ok {
			// An unterminated `<` is not markup we can reason about. Drop the remainder
			// rather than emit it and hope the browser agrees with our guess.
			s = ""
			break
		}
		raw := s[:end+1]
		s = s[end+1:]

		name, closing, attrs, selfClosing := parseTag(raw)
		if name == "" || !allowedElements[name] {
			if !closing && droppedWithContent[name] {
				s = skipElementContent(s, name)
			}
			continue
		}
		if closing {
			if n := len(open); n > 0 && open[n-1] == name {
				open = open[:n-1]
				out.WriteString("</" + name + ">")
			}
			// A closing tag that does not match the innermost open element is dropped: it
			// would either close something outside the description or nothing at all.
			continue
		}
		out.WriteString(openTag(name, attrs, selfClosing))
		if !voidElements[name] && !selfClosing {
			open = append(open, name)
		}
	}
	// Close whatever is still open, in reverse, so the stored value is well-formed and
	// cannot break out of the element the renderer places it in.
	for i := len(open) - 1; i >= 0; i-- {
		out.WriteString("</" + open[i] + ">")
	}
	return strings.TrimSpace(out.String()), nil
}

// openTag re-emits one allowed element from the allowlisted attributes only.
func openTag(name string, attrs map[string]string, selfClosing bool) string {
	var b strings.Builder
	b.WriteString("<" + name)
	switch name {
	case "a":
		// A link keeps its destination only if the destination is a URL a browser will treat
		// as navigation. `javascript:` and `data:` are the two that execute; a protocol-
		// relative `//host` is off-site and is refused too, because the panel's CSP allows
		// `connect-src 'self'` and a link out is not something a product description needs.
		if href, ok := attrs["href"]; ok {
			if safe := safeURL(href, false); safe != "" {
				b.WriteString(` href="` + escapeAttr(safe) + `"`)
				// An external link opened from a product page must not be able to reach back
				// into the opener. `noopener` is the reason this attribute is forced rather
				// than optional.
				b.WriteString(` rel="noopener noreferrer nofollow"`)
			}
		}
		if title, ok := attrs["title"]; ok && title != "" {
			b.WriteString(` title="` + escapeAttr(title) + `"`)
		}
	case "img":
		// An image may only point at this installation. `img-src 'self' data:` is the CSP, so
		// an off-site src would be blocked anyway; refusing it here means the stored value
		// never contains a beacon that only fails later.
		if src, ok := attrs["src"]; ok {
			if safe := safeURL(src, true); safe != "" {
				b.WriteString(` src="` + escapeAttr(safe) + `"`)
				alt := attrs["alt"]
				b.WriteString(` alt="` + escapeAttr(alt) + `"`)
				b.WriteString(` loading="lazy"`)
			} else {
				// No safe src means no image. Emitting a bare <img> would render as a broken
				// icon with an empty alt, which is noise on a product page.
				return ""
			}
		} else {
			return ""
		}
	}
	if selfClosing && !voidElements[name] {
		b.WriteString(" /")
	}
	b.WriteString(">")
	return b.String()
}

// safeURL returns the URL if a browser would treat it as a plain navigation or a same-origin
// image, and "" otherwise.
//
// `image` selects the stricter rule: an image must be a path on this installation
// (`/api/v1/media/...`), because a product description has no business loading bytes from
// anywhere else and `img-src` would refuse it in any case.
func safeURL(raw string, image bool) string {
	// Decode entities first: `&#106;avascript:alert(1)` is the same string to a browser as
	// `javascript:alert(1)`, so it has to be the same string to this function.
	value := strings.TrimSpace(decodeEntities(raw))
	// Control characters and interior whitespace are ignored by URL parsers in the scheme
	// position (`java\tscript:`), so they are removed before the scheme is inspected.
	var cleaned strings.Builder
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			continue
		}
		cleaned.WriteRune(r)
	}
	value = cleaned.String()
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)

	if image {
		// Same-origin absolute path only. `//host/x` is protocol-relative and off-site.
		if strings.HasPrefix(value, "//") || strings.HasPrefix(value, `/\`) {
			return ""
		}
		if !strings.HasPrefix(value, "/") {
			return ""
		}
		if strings.ContainsAny(value, `\`) || strings.Contains(value, ":") {
			return ""
		}
		return value
	}

	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return value
	case strings.HasPrefix(lower, "mailto:"):
		return value
	case strings.HasPrefix(value, "//"):
		// Protocol-relative: off-site, and indistinguishable from a bare `https://` link to a
		// reader of the stored value.
		return ""
	case strings.HasPrefix(value, "/"), strings.HasPrefix(value, "#"):
		// Same-origin path or an in-page anchor.
		return value
	default:
		// Everything else, `javascript:` and `data:` included, is not a URL we navigate to.
		return ""
	}
}

// tagEnd finds the `>` that closes the tag starting at s[0], skipping quoted attribute
// values so `<a title="a > b">` is one tag rather than two.
func tagEnd(s string) (int, bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '>':
			return i, true
		}
	}
	return 0, false
}

// parseTag splits `<name attr=value ...>` into its parts. It returns an empty name for
// anything it does not understand, which the caller treats as "drop".
func parseTag(raw string) (name string, closing bool, attrs map[string]string, selfClosing bool) {
	if len(raw) < 3 || raw[0] != '<' {
		return "", false, nil, false
	}
	body := raw[1 : len(raw)-1]
	body = strings.TrimSuffix(strings.TrimSpace(body), "/")
	selfClosing = strings.HasSuffix(strings.TrimSpace(raw[1:len(raw)-1]), "/")

	if strings.HasPrefix(body, "/") {
		closing = true
		body = strings.TrimSpace(body[1:])
	}
	i := 0
	for i < len(body) && isNameByte(body[i]) {
		i++
	}
	name = strings.ToLower(body[:i])
	if name == "" {
		return "", false, nil, false
	}
	attrs = parseAttrs(body[i:])
	return name, closing, attrs, selfClosing
}

// parseAttrs reads the attribute list of a tag. A malformed attribute ends the list rather
// than being guessed at: everything after it is dropped, which is the safe direction.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && isNameByte(s[i]) {
			i++
		}
		if i == start {
			// Not an attribute name (a stray `=` or quote). Stop.
			break
		}
		key := strings.ToLower(s[start:i])
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
		value := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && isSpaceByte(s[i]) {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				quote := s[i]
				i++
				vstart := i
				for i < len(s) && s[i] != quote {
					i++
				}
				value = s[vstart:i]
				if i < len(s) {
					i++
				}
			} else {
				vstart := i
				for i < len(s) && !isSpaceByte(s[i]) && s[i] != '/' {
					i++
				}
				value = s[vstart:i]
			}
		}
		// First occurrence wins: a duplicate attribute is how a parser-differential attack
		// starts (`<a href="/x" href="javascript:...">`), and keeping the first is the
		// choice that matches what this sanitiser validated.
		if _, seen := attrs[key]; !seen {
			attrs[key] = value
		}
	}
	return attrs
}

// skipElementContent advances past `</name>` for an element whose content must be dropped.
func skipElementContent(s, name string) string {
	lower := strings.ToLower(s)
	closing := "</" + name
	if i := strings.Index(lower, closing); i >= 0 {
		rest := s[i:]
		if end, ok := tagEnd(rest); ok {
			return rest[end+1:]
		}
	}
	return ""
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == ':' || c == '_'
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// escapeAttr re-encodes an attribute value so it cannot terminate the attribute it sits in.
func escapeAttr(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodeEntities resolves the entity forms that can change how a URL is read. An unknown
// named entity is left as written; escaping it afterwards makes it render literally, which
// is the harmless outcome.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// Numeric: &#106; or &#x6a;
		if i+1 < len(s) && s[i+1] == '#' {
			j := i + 2
			base := 10
			if j < len(s) && (s[j] == 'x' || s[j] == 'X') {
				base = 16
				j++
			}
			start := j
			for j < len(s) && isHexOrDigit(s[j], base) {
				j++
			}
			if j > start && j < len(s) && s[j] == ';' {
				var n int64
				for _, c := range s[start:j] {
					var d int64
					switch {
					case c >= '0' && c <= '9':
						d = int64(c - '0')
					case c >= 'a' && c <= 'f':
						d = int64(c-'a') + 10
					case c >= 'A' && c <= 'F':
						d = int64(c-'A') + 10
					}
					n = n*int64(base) + d
					if n > 0x10FFFF {
						n = 0xFFFD
						break
					}
				}
				b.WriteRune(rune(n))
				i = j + 1
				continue
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		// Named: a small closed set. These are the ones that can appear inside a URL.
		matched := false
		for _, ent := range []struct{ name, value string }{
			{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
			{"&quot;", "\""}, {"&apos;", "'"}, {"&nbsp;", " "},
			{"&colon;", ":"}, {"&sol;", "/"}, {"&Tab;", "\t"}, {"&NewLine;", "\n"},
		} {
			if strings.HasPrefix(s[i:], ent.name) {
				b.WriteString(ent.value)
				i += len(ent.name)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isHexOrDigit(c byte, base int) bool {
	if c >= '0' && c <= '9' {
		return true
	}
	if base == 16 {
		return c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	return false
}
