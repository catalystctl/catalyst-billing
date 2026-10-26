package notify

// Mail in the recipient's language.
//
// A queued delivery stores the rendered subject and body, not the variables that produced them:
// that is deliberate, because "what did we actually send" has to be answerable from the row. It
// means the sender is the last place that can still change the wording, so localization is a
// Sender decorator rather than a change to the outbox.
//
// The hook is one line in the worker:
//
//	sender = notify.NewLocalizedSender(sender, resolver)
//
// where `resolver` is an *i18n.Store (it satisfies LocaleResolver). Everything else — the queue,
// the retry schedule, the dedupe key — is untouched.
//
// # How a message is recognised
//
// A Sender sees (to, subject, body) and no kind, so this file carries a small spec per message
// kind: the ordered lines of the body, each either a catalog key or a piece of data (a link, a
// number, the company name). A message is localized only when its body matches one spec exactly —
// the right number of lines and every literal line equal to the default locale's text for that
// line's key. A body that does not match is sent exactly as queued.
//
// That refusal is the point. A future edit to one of the message builders would otherwise be
// silently reworded by a stale spec, and the operator who changed the sentence would find their
// change replaced by a translation. Matching, or passing through, is the only honest pair of
// outcomes.
//
// # What is translated and what is not
//
// Translated: the subject and the prose lines of the password-reset and email-verification mail.
// Those are the messages every installation sends before an operator has edited anything, and
// their words ship in the catalog's `mail.` namespace. The other kinds (invoice issued, dunning
// reminder) are rendered from operator-editable rows in `notification_templates`, so their wording
// is already data an operator can translate; they are left alone here rather than duplicated.
//
// Data lines — the reset link, the recipient's address, the company name — are copied verbatim.
// They are values, not template text: a link that happens to contain "{{company_name}}" is a link,
// and substituting it would let data choose the template.

import (
	"context"
	"html"
	"strings"

	"github.com/catalyst/billing/internal/i18n"
)

// LocaleResolver answers the two questions this file needs: which language does an address read,
// and what does a key say in a language.
//
// *i18n.Store satisfies it. It is an interface so a test can supply a table of two rows and so this
// package does not depend on the whole i18n surface.
type LocaleResolver interface {
	// LocaleForEmail returns the stored preference for an address, or "" when the account has
	// none (or no account exists), which means "send the default".
	LocaleForEmail(ctx context.Context, email string) (string, error)
	// CatalogSet returns the default locale's messages and one locale's own messages.
	//
	// Both are needed: the default's text is what recognises a queued message (the body was
	// rendered in it), and the locale's text is what replaces it. A locale with no rows returns an
	// empty map rather than the default's, which is what makes "untranslated" a pass-through
	// instead of a copy.
	CatalogSet(ctx context.Context, locale string) (def, localized map[string]string, err error)
}

// localizedSender renders each message in its recipient's language before sending it.
type localizedSender struct {
	inner    Sender
	resolver LocaleResolver
}

// NewLocalizedSender wraps a Sender so mail is rendered in the recipient's locale.
//
// A nil resolver, or a resolver that fails, leaves the message as queued: a mail in the wrong
// language is a nuisance, and a mail that is not sent because a translation lookup timed out is a
// lost invoice.
func NewLocalizedSender(inner Sender, resolver LocaleResolver) Sender {
	if inner == nil || resolver == nil {
		return inner
	}
	return &localizedSender{inner: inner, resolver: resolver}
}

// Send implements Sender.
func (s *localizedSender) Send(ctx context.Context, to, subject, body string) error {
	localizedSubject, localizedBody, _ := s.localize(ctx, to, subject, body)
	return s.inner.Send(ctx, to, localizedSubject, localizedBody)
}

// SendHTML implements HTMLSender.
//
// The branded HTML alternative is localized alongside the text rather than dropped: wrapping a
// Sender must not silently turn a multipart message into a plain-text one. The HTML is rewritten
// by replacing each escaped original line with its escaped translation, which preserves every
// element and attribute the brand shell wrote and keeps the escaping rule intact — translated prose
// is never interpreted as markup.
func (s *localizedSender) SendHTML(ctx context.Context, to, subject, text, htmlPart string) error {
	localizedSubject, localizedText, changes := s.localize(ctx, to, subject, text)
	if hs, ok := s.inner.(HTMLSender); ok {
		return hs.SendHTML(ctx, to, localizedSubject, localizedText,
			localizeHTML(htmlPart, subject, localizedSubject, changes))
	}
	return s.inner.Send(ctx, to, localizedSubject, localizedText)
}

// lineChange is one body line that localization replaced.
type lineChange struct {
	from string
	to   string
}

// localize returns the subject, body and the line replacements, or the originals.
func (s *localizedSender) localize(ctx context.Context, to, subject, body string) (string, string, []lineChange) {
	locale, err := s.resolver.LocaleForEmail(ctx, to)
	if err != nil || strings.TrimSpace(locale) == "" {
		return subject, body, nil
	}
	def, localized, err := s.resolver.CatalogSet(ctx, locale)
	if err != nil || len(def) == 0 {
		return subject, body, nil
	}
	spec, ok := matchMailSpec(body, def)
	if !ok {
		return subject, body, nil
	}

	newSubject := subject
	if text := localized[spec.subjectKey]; text != "" {
		newSubject = stripControl(text)
	}

	lines := strings.Split(body, "\n")
	newLines := make([]string, 0, len(spec.lines))
	changes := make([]lineChange, 0, len(spec.lines))
	for i, line := range spec.lines {
		original := lines[i]
		switch {
		case line.data:
			// A link is data, not language: it is copied verbatim.
			newLines = append(newLines, original)
		case line.passthrough:
			// An address or a company name is data too, and it is not translated.
			newLines = append(newLines, original)
		case line.key == "":
			// A blank separator line.
			newLines = append(newLines, original)
		default:
			text := localized[line.key]
			if text == "" {
				// Untranslated: keep the line the queue stored, which is the default locale's
				// already-instantiated text.
				newLines = append(newLines, original)
				continue
			}
			if strings.Contains(text, "{{value}}") {
				value, ok := valueOf(def, line, original)
				if !ok {
					newLines = append(newLines, original)
					continue
				}
				text = i18n.Substitute(text, map[string]string{"value": value})
			}
			newLines = append(newLines, text)
			if text != original {
				changes = append(changes, lineChange{from: original, to: text})
			}
		}
	}
	return newSubject, strings.Join(newLines, "\n"), changes
}

// localizeHTML rewrites the translated lines inside an already-rendered HTML part.
//
// It operates on the escaped forms of both sides, so a "<" in a translated sentence stays text.
// The original structure is otherwise untouched: this function knows nothing about the brand
// shell, which is what makes it safe to add branding without teaching it about locales.
func localizeHTML(part, oldSubject, newSubject string, changes []lineChange) string {
	if part == "" {
		return part
	}
	if oldSubject != newSubject {
		part = strings.ReplaceAll(part, html.EscapeString(oldSubject), html.EscapeString(newSubject))
	}
	for _, change := range changes {
		if change.from == "" || change.from == change.to {
			continue
		}
		part = strings.ReplaceAll(part, html.EscapeString(change.from), html.EscapeString(change.to))
	}
	return part
}

// mailLine is one line of a body spec: a catalog key, a blank separator, data, or a line copied
// through untranslated.
type mailLine struct {
	// key is the catalog key for a prose line. Empty for a blank separator.
	key string
	// data marks a line that is copied verbatim and must be non-empty (a link).
	data bool
	// passthrough marks a line that is copied verbatim and may be anything (an address, a
	// company name).
	passthrough bool
}

// mailSpec is one message kind's shape.
type mailSpec struct {
	subjectKey string
	lines      []mailLine
}

// mailSpecs is the closed set of messages this file localizes.
//
// The first spec for each kind is the body the *production* dispatch renders from the shipped
// editable template; the second is the deprecated Go builder's body, kept because that constructor
// is still exported. The order matters only in that the production shape is tried first.
//
// The line order must match the builders in events.go and notify.go exactly. `matchMailSpec`
// verifies that rather than trusting it, so a mismatch degrades to "sent in the default language"
// instead of scrambling the body.
var mailSpecs = []mailSpec{
	{
		// password_reset, rendered from the shipped DefaultTemplates entry.
		subjectKey: "mail.password_reset.subject",
		lines: []mailLine{
			{passthrough: true}, // "Someone asked to reset the password for the account <email>."
			{},
			{key: "mail.password_reset.line.2"},
			{data: true}, // the reset link
			{},
			{key: "mail.password_reset.line.3"}, // {{value}}: the expiry
			{key: "mail.password_reset.line.4"},
			{},
			{passthrough: true}, // the company name
		},
	},
	{
		// email_verification, rendered from the shipped DefaultTemplates entry.
		subjectKey: "mail.email_verification.subject",
		lines: []mailLine{
			{passthrough: true}, // "Confirm this address to finish setting up the account <email>."
			{},
			{data: true}, // the verification link
			{},
			{key: "mail.email_verification.line.2"}, // {{value}}: the expiry
			{},
			{passthrough: true}, // the company name
		},
	},
	{
		// PasswordResetMessage, the deprecated Go builder.
		subjectKey: "mail.password_reset.subject",
		lines: []mailLine{
			{key: "mail.password_reset.line.1"},
			{},
			{key: "mail.password_reset.line.2"},
			{data: true}, // the reset link
			{},
			{key: "mail.password_reset.line.3"}, // contains {{value}}: the expiry
			{key: "mail.password_reset.line.4"},
		},
	},
	{
		// EmailVerificationMessage, the deprecated Go builder.
		subjectKey: "mail.email_verification.subject",
		lines: []mailLine{
			{key: "mail.email_verification.line.1"},
			{},
			{data: true}, // the verification link
			{},
			{key: "mail.email_verification.line.2"}, // contains {{value}}: the expiry
		},
	},
}

// matchMailSpec finds the spec whose body shape this message has.
//
// The test is exact: the same number of lines, every literal line equal to the default locale's
// text for that key (allowing the {{value}} slot to hold anything), and every data line non-empty.
// A near-match is not enough — see the file comment for why a mismatch must pass through.
func matchMailSpec(body string, def map[string]string) (mailSpec, bool) {
	lines := strings.Split(body, "\n")
	for _, spec := range mailSpecs {
		if len(spec.lines) != len(lines) {
			continue
		}
		ok := true
		for i, line := range spec.lines {
			switch {
			case line.data:
				if strings.TrimSpace(lines[i]) == "" {
					ok = false
				}
			case line.passthrough:
				// Data the spec deliberately does not translate. It is accepted as-is so an
				// address or a brand name cannot make an otherwise recognisable message fall
				// through to English.
			case line.key == "":
				if strings.TrimSpace(lines[i]) != "" {
					ok = false
				}
			default:
				defaultText := def[line.key]
				if defaultText == "" {
					// Without the default text there is nothing to compare against, so the
					// message cannot be recognised. That happens when the catalog was truncated
					// or a kind was added to the builders and not to this table.
					ok = false
					continue
				}
				// A line with a value slot matches its instantiation; a literal line must be
				// equal, because "close enough" is how a body gets reworded by accident.
				var matched bool
				if strings.Contains(defaultText, "{{value}}") {
					_, matched = stripTemplate(defaultText, lines[i])
				} else {
					matched = defaultText == lines[i]
				}
				if !matched {
					ok = false
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return spec, true
		}
	}
	return mailSpec{}, false
}

// valueOf recovers the data substituted into a prose line.
//
// The default locale's text is the template ("The link expires in {{value}} and can be used
// once.") and the queued line is its instantiation ("The link expires in 1h0m0s and can be used
// once."), so the value is whatever sits where the placeholder was. Recovering it rather than
// re-deriving it keeps this file independent of how the builder formats a duration.
func valueOf(def map[string]string, line mailLine, original string) (string, bool) {
	defaultText := def[line.key]
	if defaultText == "" {
		return "", false
	}
	return stripTemplate(defaultText, original)
}

// stripTemplate splits `template` around its single `{{value}}` placeholder and returns the text
// the placeholder stood for in `rendered`.
//
// A template with no placeholder, or one with several, does not match: this is a value slot, and
// guessing which occurrence is the value is how a body gets scrambled.
func stripTemplate(template, rendered string) (string, bool) {
	const marker = "{{value}}"
	before, after, found := strings.Cut(template, marker)
	if !found || strings.Contains(after, marker) {
		return "", false
	}
	if !strings.HasPrefix(rendered, before) || !strings.HasSuffix(rendered, after) {
		return "", false
	}
	value := rendered[len(before) : len(rendered)-len(after)]
	if value == "" {
		return "", false
	}
	return value, true
}

// stripControl removes control characters from a subject.
//
// A translated subject is operator-supplied text that lands in an RFC 5322 header, where a newline
// is how one message becomes two. SMTPSender refuses a newline in an address for the same reason;
// this is the same defence one field over.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, s)
}
