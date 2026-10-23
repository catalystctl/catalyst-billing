package notify

// The branded HTML alternative for a transactional mail.
//
// # Why an HTML part exists at all
//
// The template body is plain text and stays plain text: a template renders attacker-influenced
// data (an invoice line description, a ticket subject, a customer's own display name), and a
// render step that interprets markup turns a billing panel into a phishing kit. That rule is not
// weakened here. What this file adds is a *shell* around the rendered text — a header with the
// tenant's name and logo, an accent rule, a footer — and every value that enters it is escaped on
// the way out. The body text keeps its own rule: it is escaped, never interpreted, so the only
// markup in the message is the markup this file writes.
//
// # Why the plain-text part is still always there
//
// SMTPSender emits multipart/alternative: the text part is byte-identical to what this product
// sent before branding existed, and the HTML part is a second representation of the same words.
// A client that shows text gets the message it always got, and `mail.brand_html = false` removes
// the decoration without removing the message. That is the difference between a branded mail and
// a mail some customers cannot read.
//
// # Why only the shop's own origin becomes a link
//
// A mail is the one place a customer is trained to click. Auto-linking every URL-shaped string
// would let whoever can type into an invoice line put a link inside a message signed by the
// shop's own DKIM key — the exact phishing vector the plain-text rule exists to prevent. So a
// URL is linked only when it starts with the configured public origin: our links stay clickable,
// and a URL that came from data stays visible text.

import (
	"context"
	"html"
	"strings"

	"github.com/catalyst/billing/internal/settings"
)

// defaultMailAccent is the colour a fresh installation's mail is accented with.
//
// It is the panel's built-in light accent. A mail is read on white, so the light-mode choice is
// the right default rather than the dark-mode one.
const defaultMailAccent = "#3b5bdb"

// MailBrand is the tenant's mark on a transactional email.
//
// Every field has a defined meaning when empty, and the zero value is a valid brand: no logo, no
// accent, the company name as the header and nothing but the company name in the footer. That is
// what makes an installation which never opened the mail-branding screen render a correct mail
// rather than a message with a hole where the branding should be.
type MailBrand struct {
	// HTML reports whether the multipart/alternative HTML part is generated.
	HTML bool
	// Header is the line above the body. Empty means the company name.
	Header string
	// Footer is the small print below the body.
	Footer string
	// Accent is a validated "#rrggbb" used for the rule and for links. Empty means unset.
	Accent string
	// LogoURL is an absolute http(s) URL the recipient's mail client can fetch.
	LogoURL string
	// Company is the fallback text for the header and the footer.
	Company string
}

// MailBrandFor reads the brand overlay for mail inside the caller's transaction.
//
// It reads raw settings rows rather than taking a settings.Store because every caller here has a
// transaction and no pool: an event renders its mail inside the transaction that made the change,
// so "mail exists if and only if the change committed" stays true. A settings read that fails
// yields the defaults, for the same reason companyName does: a mail server configuration mistake
// must not be the reason a customer is not told their password was reset.
func MailBrandFor(ctx context.Context, q Querier) MailBrand {
	values := map[string]string{}
	if q != nil {
		rows, err := q.Query(ctx, `
			SELECT group_key || '.' || key, value FROM settings
			WHERE (group_key = 'mail' AND key IN
			       ('brand_html', 'brand_header', 'brand_footer', 'brand_accent', 'brand_logo_url'))
			   OR (group_key = 'branding' AND key IN ('name', 'accent_light'))
			   OR (group_key = 'general' AND key = 'company_name')`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var name, value string
				if err := rows.Scan(&name, &value); err != nil {
					break
				}
				values[name] = value
			}
		}
	}

	// The settings registry owns the defaults, so this file cannot disagree with the screen an
	// operator edits.
	get := func(group, key string) string {
		if v, ok := values[group+"."+key]; ok {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
		if d, ok := settings.Lookup(group, key); ok {
			return d.Default
		}
		return ""
	}

	company := get("general", "company_name")
	if company == "" {
		company = "Catalyst Billing"
	}

	brand := MailBrand{
		HTML:    parseSettingBool(get("mail", "brand_html"), true),
		Header:  get("mail", "brand_header"),
		Footer:  get("mail", "brand_footer"),
		LogoURL: get("mail", "brand_logo_url"),
		Company: company,
	}
	// The panel's brand name is the display name; the general company name is the fallback. Two
	// keys because a shop may want its legal name on an invoice and a shorter mark in a header.
	if name := get("branding", "name"); name != "" {
		brand.Company = name
	}
	if brand.Header == "" {
		brand.Header = brand.Company
	}
	// One brand means one colour: the mail accent falls back to the panel's light-mode accent,
	// and only to the built-in one when neither is configured.
	brand.Accent = get("mail", "brand_accent")
	if brand.Accent == "" {
		brand.Accent = get("branding", "accent_light")
	}
	if brand.Accent == "" {
		brand.Accent = defaultMailAccent
	}
	brand.Accent = normaliseHex(brand.Accent)
	return brand
}

// parseSettingBool reads a settings boolean with an explicit fallback.
//
// A stored value that does not parse is the fallback rather than an error, matching
// mailDisabledBySetting: configuration that cannot be read must not silently change what is sent,
// and the default is what the installation was sending before anyone edited the key.
func parseSettingBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "t", "1", "yes", "on":
		return true
	case "false", "f", "0", "no", "off":
		return false
	}
	return fallback
}

// normaliseHex returns a "#rrggbb" colour, or the default when the value is not one.
//
// The settings definition already refuses a malformed accent, so this is a second gate rather
// than the first: a value written straight into the table (a restore, a migration, an operator
// with psql) must not be able to inject style attributes into every mail the shop sends.
func normaliseHex(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) != 7 || raw[0] != '#' {
		return defaultMailAccent
	}
	for _, r := range raw[1:] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return defaultMailAccent
		}
	}
	return strings.ToLower(raw)
}

// HTMLPart renders the branded HTML representation of one rendered mail.
//
// The subject becomes the document title, which is what a client shows in its reading pane and
// what a text-to-speech reader announces; the body is the rendered text, escaped and split into
// paragraphs on blank lines so the message keeps the shape the operator wrote.
func (b MailBrand) HTMLPart(r Rendered, publicOrigin string) string {
	if !b.HTML {
		return ""
	}
	accent := normaliseHex(b.Accent)
	header := strings.TrimSpace(b.Header)
	if header == "" {
		header = b.Company
	}
	footer := strings.TrimSpace(b.Footer)

	var out strings.Builder
	out.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n")
	out.WriteString("<meta charset=\"utf-8\">\n")
	out.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	out.WriteString("<title>" + html.EscapeString(r.Subject) + "</title>\n")
	// A media query is the one thing inline styles cannot express. Everything that must survive
	// a client which strips <style> is also written inline below.
	out.WriteString("<style>@media (max-width:600px){.cb-card{width:100%!important}}</style>\n")
	out.WriteString("</head>\n")
	out.WriteString("<body style=\"margin:0;padding:0;background:#f4f5f7;\">\n")
	out.WriteString("<table role=\"presentation\" width=\"100%\" cellpadding=\"0\" cellspacing=\"0\" " +
		"border=\"0\" style=\"background:#f4f5f7;\"><tr><td align=\"center\" " +
		"style=\"padding:24px 12px;\">\n")
	out.WriteString("<table role=\"presentation\" class=\"cb-card\" width=\"600\" cellpadding=\"0\" " +
		"cellspacing=\"0\" border=\"0\" style=\"width:600px;max-width:100%;background:#ffffff;" +
		"border-radius:8px;border:1px solid #e6e8ec;font-family:-apple-system,BlinkMacSystemFont," +
		"'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1a1d24;\">\n")

	// The accent rule is the one place the brand colour is unmistakable, and it is a table row
	// rather than a border because Outlook drops a border on a wrapping table.
	out.WriteString("<tr><td style=\"height:3px;line-height:3px;font-size:0;background:" + accent +
		";\">&nbsp;</td></tr>\n")

	out.WriteString("<tr><td style=\"padding:24px 28px 0 28px;\">\n")
	if b.LogoURL != "" {
		out.WriteString("<img src=\"" + html.EscapeString(b.LogoURL) + "\" alt=\"" +
			html.EscapeString(header) + "\" height=\"32\" style=\"display:block;border:0;" +
			"max-height:32px;margin:0 0 8px 0;\">\n")
	}
	out.WriteString("<div style=\"font-size:16px;font-weight:600;line-height:1.3;color:" + accent +
		";\">" + html.EscapeString(header) + "</div>\n")
	out.WriteString("</td></tr>\n")

	out.WriteString("<tr><td style=\"padding:16px 28px 24px 28px;font-size:15px;line-height:1.55;\">\n")
	out.WriteString(htmlParagraphs(r.Body, publicOrigin, accent))
	out.WriteString("</td></tr>\n")

	out.WriteString("<tr><td style=\"padding:16px 28px 24px 28px;border-top:1px solid #e6e8ec;" +
		"font-size:12px;line-height:1.5;color:#5b6472;\">\n")
	if footer != "" {
		out.WriteString("<div>" + html.EscapeString(footer) + "</div>\n")
	}
	out.WriteString("<div>" + html.EscapeString(b.Company) + "</div>\n")
	out.WriteString("</td></tr>\n")

	out.WriteString("</table>\n</td></tr></table>\n</body>\n</html>\n")
	return out.String()
}

// htmlParagraphs turns rendered plain text into escaped HTML paragraphs.
//
// Blank lines separate paragraphs and a single newline is a line break, which is the only
// structure a plain-text template has. Every character is escaped before it is written, so a
// description containing "<script>" is displayed as that text and never becomes an element.
func htmlParagraphs(text, publicOrigin, accent string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out strings.Builder
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.Trim(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		out.WriteString("<p style=\"margin:0 0 14px 0;\">")
		for i, line := range strings.Split(block, "\n") {
			if i > 0 {
				out.WriteString("<br>")
			}
			out.WriteString(linkifyTrusted(html.EscapeString(line), publicOrigin, accent))
		}
		out.WriteString("</p>\n")
	}
	return out.String()
}

// linkifyTrusted links the shop's own URLs and leaves every other URL as text.
//
// It works on already-escaped text, and the origin is escaped the same way, so the search cannot
// be fooled by an entity. Only a run that begins with the configured origin is linked; anything
// else is returned unchanged, which is what keeps a customer-supplied URL from becoming a
// clickable link inside a mail the shop signed.
func linkifyTrusted(escaped, publicOrigin, accent string) string {
	origin := strings.TrimRight(strings.TrimSpace(publicOrigin), "/")
	if origin == "" {
		return escaped
	}
	prefix := html.EscapeString(origin)
	if !strings.Contains(escaped, prefix) {
		return escaped
	}
	var out strings.Builder
	rest := escaped
	for {
		at := strings.Index(rest, prefix)
		if at < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:at])
		rest = rest[at:]

		end := len(rest)
		for i, r := range rest {
			if r == ' ' || r == '\t' || r == '<' || r == '"' {
				end = i
				break
			}
		}
		url := rest[:end]
		// Trailing punctuation is sentence punctuation, not part of the URL.
		for len(url) > len(prefix) && strings.ContainsRune(".,;:!?)]}'", rune(url[len(url)-1])) {
			url = url[:len(url)-1]
		}
		out.WriteString("<a href=\"" + url + "\" style=\"color:" + accent + ";text-decoration:underline;\">" +
			url + "</a>")
		// Whatever was trimmed (punctuation) is written back after the anchor.
		rest = rest[len(url):]
	}
}
