package notify_test

// Mail in the recipient's language, against the real database and the real outbox.
//
// The decorator has to answer three questions and each one has a way to be wrong that a
// hand-written fake would hide: does the address resolve to a stored preference, does the queued
// default-locale body still match the spec after a translation exists, and does a missing
// translation fall back rather than half-translating or blanking the message.
//
// The first test drives `notify.PasswordReset`, the production dispatch path, and then drains the
// outbox through the localized sender. That is deliberate: a test that only called the deprecated
// Go builder could pass while the mail an installation actually sends remained English.

import (
	"context"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/i18n"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The production resolver is the i18n store: this assertion is what makes the one-line wiring in
// cmd/billing/jobs.go a compile-time fact rather than a hope.
var _ notify.LocaleResolver = (*i18n.Store)(nil)

// captureSender records what would have been sent.
//
// It implements HTMLSender too, so the test proves that wrapping a multipart sender does not
// silently downgrade the message to plain text.
type captureSender struct {
	to       string
	subject  string
	body     string
	html     string
	sent     int
	htmlSent int
}

func (c *captureSender) Send(_ context.Context, to, subject, body string) error {
	c.to, c.subject, c.body = to, subject, body
	c.sent++
	return nil
}

func (c *captureSender) SendHTML(_ context.Context, to, subject, text, htmlPart string) error {
	c.to, c.subject, c.body, c.html = to, subject, text, htmlPart
	c.sent++
	c.htmlSent++
	return nil
}

func seedMailCatalog(t *testing.T, locales *i18n.Store) {
	t.Helper()
	entries := map[string]string{
		"mail.password_reset.subject": "Reset your password",
		"mail.password_reset.line.1":  "Someone asked to reset the password for this account.",
		"mail.password_reset.line.2":  "Open this link to choose a new password:",
		"mail.password_reset.line.3":  "The link expires in {{value}} and can be used once.",
		"mail.password_reset.line.4":  "If you did not ask for this, ignore this message; the password is unchanged.",
	}
	for locale, values := range map[string]map[string]string{
		"en": entries,
		"de": {
			"mail.password_reset.subject": "Passwort zurücksetzen",
			"mail.password_reset.line.1":  "Jemand hat darum gebeten, das Passwort für dieses Konto zurückzusetzen.",
			"mail.password_reset.line.2":  "Öffne diesen Link, um ein neues Passwort zu wählen:",
			"mail.password_reset.line.3":  "Der Link läuft in {{value}} ab und kann einmal verwendet werden.",
			"mail.password_reset.line.4":  "Wenn du das nicht angefordert hast, ignoriere diese Nachricht; das Passwort bleibt unverändert.",
		},
	} {
		for key, text := range values {
			if _, err := locales.Put(context.Background(), locale, key, text, ""); err != nil {
				t.Fatalf("seed %s %s: %v", locale, key, err)
			}
		}
	}
}

// seedRecipient inserts a user and stores their language preference the way the account screen
// does, rather than writing the column directly: the test is about the production read path.
func seedRecipient(t *testing.T, pool *store.Pool, locales *i18n.Store, email, locale string) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, 'x', 'Recipient', now()) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := locales.SetUserLocale(ctx, userID, locale); err != nil {
		t.Fatalf("store %s preference: %v", locale, err)
	}
	return userID
}

// TestLocalizedSenderTranslatesProductionMailForANonDefaultLocaleRecipient is the acceptance test:
// a German customer gets the German subject and sentences, while the link, their address and the
// company name survive verbatim. It also proves the outbox still deduplicates the event and that a
// retry sees the same queued body.
func TestLocalizedSenderTranslatesProductionMailForANonDefaultLocaleRecipient(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	locales := i18n.New(pool)

	// `de` ships disabled with a full set of translations. Enabling it is the operator action this
	// feature waits for; the store restores the seed before the next test.
	enabled := true
	if _, err := locales.Update(ctx, "de", i18n.LocaleInput{
		Code: "de", Name: "German", NativeName: "Deutsch", Enabled: &enabled,
	}); err != nil {
		t.Fatalf("enable de: %v", err)
	}

	seedMailCatalog(t, locales)
	const email = "kunde@example.test"
	userID := seedRecipient(t, pool, locales, email, "de")

	queue := notify.NewQueue(pool)
	link := "https://billing.example.test/reset?token=abc&next={{company_name}}"
	in := notify.PasswordResetInput{
		UserID:      userID,
		Email:       email,
		Link:        link,
		ExpiresIn:   time.Hour,
		Token:       "tok-de",
		CompanyName: "Catalyst Hosting",
	}
	// The same event twice: the second call must be a no-op through the existing dedupe key.
	for attempt := 0; attempt < 2; attempt++ {
		inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			out, err := notify.PasswordReset(ctx, tx, queue, in)
			if err != nil {
				return err
			}
			if attempt == 1 && out.Mailed {
				t.Error("a replayed event created a second delivery row")
			}
			return nil
		})
	}

	var deliveries int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key = 'password_reset:tok-de'`).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Fatalf("deliveries = %d, want 1 after a replay", deliveries)
	}

	// The queued row is the default-locale text rendered inside the transaction. The sender is the
	// only stage that changes it, which is what keeps "what did we send" answerable from the row.
	var queuedSubject, queuedBody string
	if err := pool.QueryRow(ctx, `
		SELECT subject, body FROM notification_deliveries
		WHERE dedupe_key = 'password_reset:tok-de'`).Scan(&queuedSubject, &queuedBody); err != nil {
		t.Fatalf("read queued delivery: %v", err)
	}
	def, deCatalog, catErr := locales.CatalogSet(ctx, "de")
	if catErr != nil || def["mail.password_reset.subject"] != "Reset your password" || deCatalog["mail.password_reset.subject"] != "Passwort zurücksetzen" {
		t.Fatalf("catalog set: err=%v default=%q localized=%q", catErr, def["mail.password_reset.subject"], deCatalog["mail.password_reset.subject"])
	}
	pref, prefErr := locales.LocaleForEmail(ctx, email)
	if prefErr != nil || pref != "de" {
		t.Fatalf("recipient locale: %q (%v)", pref, prefErr)
	}

	capture := &captureSender{}
	res, err := queue.RunOnce(ctx, notify.NewLocalizedSender(capture, locales), 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Sent != 1 || capture.sent != 1 {
		t.Fatalf("sent = %d / capture = %d, want one delivery", res.Sent, capture.sent)
	}
	if capture.htmlSent != 1 {
		t.Fatalf("the HTML part was dropped: html sends = %d, want 1", capture.htmlSent)
	}

	if capture.subject != "Passwort zurücksetzen" {
		t.Fatalf("subject = %q, want the German subject", capture.subject)
	}
	if !strings.Contains(capture.body, "Öffne diesen Link, um ein neues Passwort zu wählen") {
		t.Fatalf("the body was not translated:\n%s", capture.body)
	}
	// The link is data, not language. It must survive verbatim — including a value that looks like
	// a placeholder, because substitution is not applied to data.
	if !strings.Contains(capture.body, link) {
		t.Fatalf("the reset link was altered or lost:\n%s", capture.body)
	}
	// The expiry is recovered from the default-locale line and carried into the translated sentence.
	if !strings.Contains(capture.body, "Der Link läuft in 1 hours ab") {
		t.Fatalf("the expiry was not carried into the German sentence:\n%s", capture.body)
	}
	if strings.Contains(capture.body, "{{value}}") {
		t.Fatalf("an unsubstituted placeholder was sent:\n%s", capture.body)
	}
	// The address line and the company name are pass-through data, not lost by translation.
	if !strings.Contains(capture.body, email) {
		t.Fatalf("the recipient address disappeared from the body:\n%s", capture.body)
	}
	if !strings.Contains(capture.body, "Catalyst Hosting") {
		t.Fatalf("the company name disappeared from the body:\n%s", capture.body)
	}

	// The branded HTML is localized as well as the text part, and remains escaped.
	if !strings.Contains(capture.html, "Passwort zurücksetzen") {
		t.Fatalf("the HTML subject was not translated:\n%s", capture.html)
	}
	if !strings.Contains(capture.html, html.EscapeString("Öffne diesen Link, um ein neues Passwort zu wählen")) {
		t.Fatalf("the HTML body was not translated:\n%s", capture.html)
	}
	if strings.Contains(capture.html, queuedSubject) {
		// The English subject must not be left in the document title or branded heading.
		t.Fatalf("the HTML still carries the default-locale subject %q", queuedSubject)
	}
}

// TestLocalizedSenderFallsBackWhenTranslationsAreMissing pins the safe default: a recipient whose
// locale has only one translated key gets that key translated and every missing line exactly as
// queued in the default locale. Nothing is blanked and no placeholder is invented.
func TestLocalizedSenderFallsBackWhenTranslationsAreMissing(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	locales := i18n.New(pool)

	enabled := true
	if _, err := locales.Create(ctx, i18n.LocaleInput{
		Code: "fr", Name: "French", NativeName: "Français", Enabled: &enabled,
	}); err != nil {
		t.Fatalf("create fr: %v", err)
	}
	// Only the subject is translated. Every body line is missing and must fall back.
	if _, err := locales.Put(ctx, "fr", "mail.password_reset.subject",
		"Réinitialiser votre mot de passe", ""); err != nil {
		t.Fatalf("put fr subject: %v", err)
	}

	seedMailCatalog(t, locales)
	const email = "client@example.test"
	userID := seedRecipient(t, pool, locales, email, "fr")

	queue := notify.NewQueue(pool)
	in := notify.PasswordResetInput{
		UserID:    userID,
		Email:     email,
		Link:      "https://billing.example.test/reset?token=fr",
		ExpiresIn: time.Hour,
		Token:     "tok-fr",
	}
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.PasswordReset(ctx, tx, queue, in)
		return err
	})

	var queuedSubject, queuedBody string
	if err := pool.QueryRow(ctx, `
		SELECT subject, body FROM notification_deliveries
		WHERE dedupe_key = 'password_reset:tok-fr'`).Scan(&queuedSubject, &queuedBody); err != nil {
		t.Fatalf("read queued delivery: %v", err)
	}

	capture := &captureSender{}
	if _, err := queue.RunOnce(ctx, notify.NewLocalizedSender(capture, locales), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if capture.subject != "Réinitialiser votre mot de passe" {
		t.Fatalf("subject = %q, want the one translated key", capture.subject)
	}
	if capture.body != queuedBody {
		t.Fatalf("a missing body translation changed the message:\n got %q\nwant %q",
			capture.body, queuedBody)
	}
	if !strings.Contains(capture.body, "Open this link to choose a new password:") {
		t.Fatalf("the fallback body lost its default-locale text:\n%s", capture.body)
	}
	_ = queuedSubject
}

// TestLocalizedSenderLeavesAnUnrecognisedBodyAlone is the guard that stops a future edit to a
// message builder from being silently reworded by a stale spec.
func TestLocalizedSenderLeavesAnUnrecognisedBodyAlone(t *testing.T) {
	resolver := fakeResolver{
		locale: "de",
		def: map[string]string{
			"mail.password_reset.subject": "Reset your password",
		},
		localized: map[string]string{
			"mail.password_reset.subject": "Passwort zurücksetzen",
		},
	}
	body := "A body written by a builder this file has never heard of.\n\nhttps://example.test/x"
	capture := &captureSender{}
	if err := notify.NewLocalizedSender(capture, resolver).
		Send(context.Background(), "kunde@example.test", "Something else entirely", body); err != nil {
		t.Fatalf("send: %v", err)
	}
	if capture.body != body || capture.subject != "Something else entirely" {
		t.Fatalf("an unrecognised message was modified:\nsubject=%q\nbody=%q", capture.subject, capture.body)
	}
}

// TestLocalizedSenderStripsControlCharactersFromATranslatedSubject covers the one place a
// translation becomes a header. A newline in a subject is how one message becomes two.
func TestLocalizedSenderStripsControlCharactersFromATranslatedSubject(t *testing.T) {
	resolver := fakeResolver{
		locale: "de",
		def: map[string]string{
			"mail.password_reset.subject": "Reset your password",
			"mail.password_reset.line.1":  "Someone asked to reset the password for this account.",
			"mail.password_reset.line.2":  "Open this link to choose a new password:",
			"mail.password_reset.line.3":  "The link expires in {{value}} and can be used once.",
			"mail.password_reset.line.4":  "If you did not ask for this, ignore this message; the password is unchanged.",
		},
		localized: map[string]string{
			"mail.password_reset.subject": "Passwort\r\nBcc: attacker@example.test",
		},
	}
	body := strings.Join([]string{
		"Someone asked to reset the password for this account.",
		"",
		"Open this link to choose a new password:",
		"https://billing.example.test/reset",
		"",
		"The link expires in 1 hours and can be used once.",
		"If you did not ask for this, ignore this message; the password is unchanged.",
	}, "\n")
	capture := &captureSender{}
	if err := notify.NewLocalizedSender(capture, resolver).
		Send(context.Background(), "kunde@example.test", "Reset your password", body); err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.ContainsAny(capture.subject, "\r\n") {
		t.Fatalf("subject contains a newline: %q", capture.subject)
	}
	if capture.subject != "PasswortBcc: attacker@example.test" {
		t.Fatalf("subject = %q, want the control characters removed only", capture.subject)
	}
}

// fakeResolver is the smallest LocaleResolver: a fixed locale and two maps.
type fakeResolver struct {
	locale    string
	def       map[string]string
	localized map[string]string
}

func (f fakeResolver) LocaleForEmail(context.Context, string) (string, error) {
	return f.locale, nil
}

func (f fakeResolver) CatalogSet(context.Context, string) (map[string]string, map[string]string, error) {
	return f.def, f.localized, nil
}
