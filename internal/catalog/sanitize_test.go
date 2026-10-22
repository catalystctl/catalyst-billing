package catalog_test

import (
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/catalog"
)

// The description sanitiser is the one place in the catalog where a staff member's text is
// stored as markup and later parsed by every visitor's browser. These tests are written from
// the attacker's side: each case is a payload that would do something if it survived, and the
// assertion is that the dangerous part is gone — not merely that a friendly paragraph still
// round-trips.
//
// A round-trip test alone would pass against a sanitiser that strips nothing, because a
// paragraph has nothing to strip. That is why every case below names what must NOT be in the
// output.

// hostilePayloads are the classic shapes, each paired with the substrings that must not appear
// in the sanitised result.
var hostilePayloads = []struct {
	name    string
	payload string
	// forbidden is checked case-insensitively against the whole output.
	forbidden []string
}{
	{
		name:      "event handler on an image",
		payload:   `<p>Nice</p><img src=x onerror=alert(1)>`,
		forbidden: []string{"onerror", "alert(1)"},
	},
	{
		name:      "script element",
		payload:   `<p>Hello</p><script>alert(document.cookie)</script><p>Bye</p>`,
		forbidden: []string{"<script", "alert(document.cookie)"},
	},
	{
		name:      "javascript url",
		payload:   `<a href="javascript:alert(1)">click</a>`,
		forbidden: []string{"javascript:", "alert(1)"},
	},
	{
		name:      "javascript url with a tab in the scheme",
		payload:   "<a href=\"java\tscript:alert(1)\">click</a>",
		forbidden: []string{"javascript:", "script:"},
	},
	{
		name:      "javascript url with an entity in the scheme",
		payload:   `<a href="&#106;avascript:alert(1)">click</a>`,
		forbidden: []string{"avascript", "alert(1)"},
	},
	{
		name:      "data url",
		payload:   `<a href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">x</a>`,
		forbidden: []string{"data:", "base64"},
	},
	{
		name:      "uppercase script with attributes",
		payload:   `<SCRIPT SRC="https://evil.example/x.js"></SCRIPT>`,
		forbidden: []string{"<script", "evil.example"},
	},
	{
		name:      "style element",
		payload:   `<style>body{display:none}</style><p>hi</p>`,
		forbidden: []string{"<style", "display:none"},
	},
	{
		name:      "iframe",
		payload:   `<iframe src="https://evil.example"></iframe>`,
		forbidden: []string{"<iframe", "evil.example"},
	},
	{
		name:      "svg with a script",
		payload:   `<svg><script>alert(1)</script></svg>`,
		forbidden: []string{"<svg", "<script", "alert(1)"},
	},
	{
		name:      "onclick attribute",
		payload:   `<p onclick="steal()">text</p>`,
		forbidden: []string{"onclick", "steal("},
	},
	{
		name:      "style attribute",
		payload:   `<p style="position:fixed;inset:0">overlay</p>`,
		forbidden: []string{"style=", "position:fixed"},
	},
	{
		name:      "comment-wrapped script",
		payload:   `<!--[if IE]><script>alert(1)</script><![endif]--><p>ok</p>`,
		forbidden: []string{"<script", "alert(1)"},
	},
	{
		name: "broken tag hiding a script",
		// A browser tokenises `<p <script>` as a `<p>` whose attribute list contains a bogus
		// `<script` name, so no script element exists and `alert(1)` is inert text. The tag
		// must not survive; the leftover words are just words, which is why only the tag is
		// asserted here.
		payload:   `<p <script>alert(1)</script>`,
		forbidden: []string{"<script"},
	},
	{
		name:      "protocol-relative link",
		payload:   `<a href="//evil.example/x">x</a>`,
		forbidden: []string{"evil.example"},
	},
	{
		name:      "off-site image",
		payload:   `<img src="https://evil.example/track.gif">`,
		forbidden: []string{"evil.example"},
	},
	{
		name:      "nested and unbalanced tags",
		payload:   `</div></div><p>text<div><span>`,
		forbidden: []string{"</div></div>"},
	},
}

func TestHostileDescriptionIsNeutralised(t *testing.T) {
	for _, tc := range hostilePayloads {
		t.Run(tc.name, func(t *testing.T) {
			out, err := catalog.SanitizeHTML(tc.payload)
			if err != nil {
				t.Fatalf("sanitise: %v", err)
			}
			lower := strings.ToLower(out)
			for _, bad := range tc.forbidden {
				if strings.Contains(lower, strings.ToLower(bad)) {
					t.Fatalf("payload %q survived as %q: found %q", tc.payload, out, bad)
				}
			}
		})
	}
}

// A sanitiser that returns "" for everything would pass the test above and be useless. This is
// the other half: the markup a product description legitimately uses has to survive intact.
func TestGoodDescriptionSurvives(t *testing.T) {
	in := `<h2>What you get</h2><p>Fast <strong>NVMe</strong> storage and a <em>dedicated</em> IPv4.</p>` +
		`<ul><li>8 GB RAM</li><li>4 vCPU</li></ul>` +
		`<p>Read the <a href="https://example.com/terms">terms</a>.</p>` +
		`<img src="/api/v1/media/6f6c1f2c-0000-0000-0000-000000000000" alt="rack">`
	out, err := catalog.SanitizeHTML(in)
	if err != nil {
		t.Fatalf("sanitise: %v", err)
	}
	for _, want := range []string{
		"<h2>What you get</h2>", "<strong>NVMe</strong>", "<em>dedicated</em>",
		"<li>8 GB RAM</li>", "<ul>", "</ul>",
		`href="https://example.com/terms"`, "rel=\"noopener noreferrer nofollow\"",
		`src="/api/v1/media/6f6c1f2c-0000-0000-0000-000000000000"`, `alt="rack"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sanitised output lost %q:\n%s", want, out)
		}
	}
}

// Idempotence matters because the stored value is also the value the browser's own sanitiser
// sees. If a second pass changed the string, the client and the server would disagree about
// what was stored.
func TestSanitizeIsIdempotent(t *testing.T) {
	for _, in := range []string{
		`<p>a <b>b</b> <a href="/x">c</a></p>`,
		`<img src=x onerror=alert(1)>`,
		`<div><p>unclosed`,
		`plain text with an & and a <`,
	} {
		once, err := catalog.SanitizeHTML(in)
		if err != nil {
			t.Fatalf("sanitise %q: %v", in, err)
		}
		twice, err := catalog.SanitizeHTML(once)
		if err != nil {
			t.Fatalf("sanitise twice %q: %v", once, err)
		}
		if once != twice {
			t.Fatalf("not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

// A text run cannot become markup: an entity that decodes to a tag is text, and the sanitiser
// must not turn it into one.
func TestEncodedTagStaysText(t *testing.T) {
	out, err := catalog.SanitizeHTML(`<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>`)
	if err != nil {
		t.Fatalf("sanitise: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "<script") {
		t.Fatalf("an encoded tag became a real one: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("the text was lost: %q", out)
	}
}

func TestDescriptionFormatIsValidated(t *testing.T) {
	if _, _, err := catalog.Description("<p>x</p>", "markdown"); err == nil {
		t.Fatal("an unknown description_format must be refused")
	}
	text, format, err := catalog.Description("<p>x</p>", "")
	if err != nil {
		t.Fatalf("default format: %v", err)
	}
	if format != "plain" || text != "<p>x</p>" {
		t.Fatalf("default = (%q, %q), want the text unchanged and format plain", text, format)
	}
	text, format, err = catalog.Description("<p onclick=\"x()\">hi</p>", "html")
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if format != "html" || strings.Contains(text, "onclick") {
		t.Fatalf("html = (%q, %q), want sanitised html", text, format)
	}
}

func TestDescriptionSizeIsBounded(t *testing.T) {
	if _, err := catalog.SanitizeHTML(strings.Repeat("<p>x</p>", 20000)); err == nil {
		t.Fatal("an oversized description must be refused rather than stored")
	}
}
