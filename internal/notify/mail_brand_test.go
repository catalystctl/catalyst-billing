package notify_test

// Mail branding and the two system messages.
//
// The properties these tests pin down are the ones a screenshot cannot see, and the ones a
// regression would ship silently:
//
//   - the plain-text part survives branding byte for byte, because a message whose only part is
//     HTML is a message a text-only client cannot read;
//   - a value from the database is escaped in the HTML part, because the HTML part *is* parsed as
//     markup — the one place in this package where that is true;
//   - only the shop's own origin becomes a link, so customer-supplied text cannot become a
//     clickable link inside a mail the shop signed;
//   - a sender that does not understand HTML receives the plain body, never markup;
//   - the Discord mirror posts an event instance once, even though the outbox has a row per
//     recipient;
//   - a test mail and a cron-failure alert are outbox rows, so the email log can answer for them.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// queuedMail reads the newest queued delivery for a recipient.
type queuedMail struct {
	Subject string
	Body    string
	HTML    string
}

func newestQueued(t *testing.T, pool *store.Pool, to string) queuedMail {
	t.Helper()
	var m queuedMail
	err := pool.QueryRow(context.Background(), `
		SELECT subject, body, body_html FROM notification_deliveries
		WHERE to_address = $1 AND status = 'queued'
		ORDER BY created_at DESC LIMIT 1`, to).Scan(&m.Subject, &m.Body, &m.HTML)
	if err != nil {
		t.Fatalf("read queued mail: %v", err)
	}
	return m
}

// issueInvoice sends the new_invoice_created event to one address and returns what was queued.
func issueInvoice(t *testing.T, pool *store.Pool, to string) queuedMail {
	t.Helper()
	seedTemplates(t, pool)
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoiceIssued(ctx, tx, notify.NewQueue(pool), notify.InvoiceIssuedInput{
			Email:        to,
			InvoiceID:    "11111111-1111-1111-1111-111111111111",
			Number:       "INV-000042",
			CustomerName: "Ada Lovelace",
			Total:        "USD 49.00",
		})
		return err
	})
	return newestQueued(t, pool, to)
}

// A branded mail carries the tenant's name, accent and footer, and keeps the plain text intact.
func TestBrandedMailAddsAHeaderAndFooterWithoutTouchingTheText(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "general", "company_name", "Northwind Hosting")
	setSetting(t, pool, "mail", "brand_header", "Northwind")
	setSetting(t, pool, "mail", "brand_footer", "Northwind Hosting · 12 Harbour Road")
	setSetting(t, pool, "mail", "brand_accent", "#0f766e")
	setSetting(t, pool, "mail", "brand_logo_url", "https://cdn.example.test/logo.png")

	got := issueInvoice(t, pool, "brand@example.test")

	// The plain part is the message the product sent before branding existed: no tags at all.
	if strings.Contains(got.Body, "<") {
		t.Fatalf("the plain-text body contains markup:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "INV-000042") || !strings.Contains(got.Body, "Northwind Hosting") {
		t.Fatalf("the plain-text body lost its content:\n%s", got.Body)
	}
	if got.HTML == "" {
		t.Fatal("no HTML alternative was rendered")
	}
	for _, want := range []string{
		"Northwind", "Northwind Hosting · 12 Harbour Road", "#0f766e",
		"https://cdn.example.test/logo.png", "INV-000042",
	} {
		if !strings.Contains(got.HTML, want) {
			t.Fatalf("the HTML alternative is missing %q:\n%s", want, got.HTML)
		}
	}
}

// Turning the HTML part off leaves a plain-text message, not an empty one.
func TestBrandHTMLOffSendsTextOnly(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "mail", "brand_html", "false")

	got := issueInvoice(t, pool, "plain@example.test")
	if got.HTML != "" {
		t.Fatalf("brand_html=false still rendered an HTML part: %q", got.HTML)
	}
	if !strings.Contains(got.Body, "INV-000042") {
		t.Fatalf("the plain body was emptied when branding was turned off:\n%s", got.Body)
	}
}

// A value from the database is escaped in the HTML part.
//
// This is the inverse of TestRenderDoesNotInterpretMarkup: the plain text keeps markup as text, and
// the HTML part must too — by escaping it, because in that part the client does parse the markup.
func TestHTMLPartEscapesDataFromTheDatabase(t *testing.T) {
	pool := storetest.DB(t)
	hostile := `<script>alert(1)</script>`
	seedTemplates(t, pool)
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoiceIssued(ctx, tx, notify.NewQueue(pool), notify.InvoiceIssuedInput{
			Email:        "escape@example.test",
			InvoiceID:    "22222222-2222-2222-2222-222222222222",
			Number:       hostile,
			CustomerName: hostile,
			Total:        "USD 1.00",
		})
		return err
	})

	got := newestQueued(t, pool, "escape@example.test")
	if strings.Contains(got.HTML, "<script>") {
		t.Fatalf("customer data became an element in the HTML part:\n%s", got.HTML)
	}
	if !strings.Contains(got.HTML, "&lt;script&gt;") {
		t.Fatalf("customer data was not escaped in the HTML part:\n%s", got.HTML)
	}
	// The plain part is unchanged: a "<" is a "<".
	if !strings.Contains(got.Body, hostile) {
		t.Fatalf("the plain part escaped data, which implies it is parsed as markup:\n%s", got.Body)
	}
}

// Only the shop's own origin is linkified; anything else stays text.
func TestHTMLPartLinksOnlyTheShopsOwnURLs(t *testing.T) {
	brand := notify.MailBrand{HTML: true, Accent: "#3b5bdb", Company: "Example"}
	rendered := notify.Rendered{
		Subject: "Invoice",
		Body:    "Pay here: http://127.0.0.1:8080/portal/invoices/abc\nAlso see https://evil.example/pay",
	}
	htmlPart := brand.HTMLPart(rendered, "http://127.0.0.1:8080")

	if !strings.Contains(htmlPart, `<a href="http://127.0.0.1:8080/portal/invoices/abc"`) {
		t.Fatalf("the shop's own URL was not linked:\n%s", htmlPart)
	}
	if strings.Contains(htmlPart, `href="https://evil.example`) {
		t.Fatalf("a URL from data became a link:\n%s", htmlPart)
	}
	// It is still visible as text, which is what makes it safe to leave unlinked.
	if !strings.Contains(htmlPart, "https://evil.example/pay") {
		t.Fatalf("the foreign URL was dropped instead of shown as text:\n%s", htmlPart)
	}
}

// A header with a newline cannot break out, and a non-ASCII subject is encoded.
func TestMailHeadersAreSafeForANonASCIIBrand(t *testing.T) {
	// The renderer strips control characters from substituted subjects, and the SMTP builder
	// strips them again from the header itself: this asserts the value a brand name can carry.
	rendered := notify.Render(notify.Template{
		EventKey: "x",
		Subject:  "{{company_name}} — invoice",
	}, map[string]string{"company_name": "Café Ünïcode\r\nBcc: evil@example.test"})
	if strings.ContainsAny(rendered.Subject, "\r\n") {
		t.Fatalf("subject carries a header break: %q", rendered.Subject)
	}
	if !strings.Contains(rendered.Subject, "Café Ünïcode") {
		t.Fatalf("the non-ASCII brand name was changed: %q", rendered.Subject)
	}
}

// A sender that does not understand HTML receives the plain body.
//
// Sender's contract is one plain-text body; HTMLSender is an opt-in. The queue must not hand markup
// to an implementation that would have to interpret it — the failure mode is a mail whose body is
// raw HTML, which every client shows as a wall of tags.
func TestQueueFallsBackToPlainTextForASenderWithoutHTML(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "mail", "brand_header", "Northwind")
	issueInvoice(t, pool, "fallback@example.test")

	sender := &fakeSender{}
	queue := notify.NewQueue(pool)
	if _, err := queue.RunOnce(context.Background(), sender, 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) == 0 {
		t.Fatal("nothing was sent")
	}
	for _, line := range sender.sent {
		if strings.Contains(line, "<html") || strings.Contains(line, "<table") {
			t.Fatalf("markup was handed to a text-only sender: %q", line)
		}
	}
}

// htmlSender records the two parts it was given.
type htmlSender struct {
	text []string
	html []string
}

func (h *htmlSender) Send(_ context.Context, to, subject, body string) error {
	h.text = append(h.text, to+"|"+subject+"|"+body)
	return nil
}

func (h *htmlSender) SendHTML(_ context.Context, to, subject, text, htmlPart string) error {
	h.text = append(h.text, to+"|"+subject+"|"+text)
	h.html = append(h.html, htmlPart)
	return nil
}

// A sender that can carry HTML gets both parts.
func TestQueueUsesMultipartForASenderThatSupportsIt(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "mail", "brand_header", "Northwind")
	issueInvoice(t, pool, "multipart@example.test")

	sender := &htmlSender{}
	queue := notify.NewQueue(pool)
	if _, err := queue.RunOnce(context.Background(), sender, 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(sender.html) != 1 {
		t.Fatalf("HTML parts sent = %d, want 1", len(sender.html))
	}
	if !strings.Contains(sender.html[0], "Northwind") {
		t.Fatalf("the HTML part has no brand:\n%s", sender.html[0])
	}
	if strings.Contains(sender.html[0], "<script") {
		t.Fatalf("the HTML part contains a script element")
	}
}

// mirrorDouble records what would have been posted.
type mirrorDouble struct {
	events []string
	title  []string
}

func (m *mirrorDouble) MirrorEvent(_ context.Context, eventKey string, p notify.MirrorPayload) error {
	m.events = append(m.events, eventKey)
	m.title = append(m.title, p.Title)
	return nil
}

// The Discord mirror posts once per event instance, not once per recipient.
func TestDiscordMirrorPostsOncePerEventInstance(t *testing.T) {
	pool := storetest.DB(t)
	seedTemplates(t, pool)
	setSetting(t, pool, "mail", "brand_html", "false")
	// Two recipients for one event: the primary and a CC, exactly what enqueueMail writes.
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		queue := notify.NewQueue(pool)
		for _, to := range []string{"one@example.test", "two@example.test"} {
			if _, _, err := queue.Enqueue(ctx, tx, notify.Message{
				Kind:      notify.EventInvoicePaid,
				To:        to,
				Subject:   "Invoice INV-1 paid",
				Body:      "Thank you.",
				DedupeKey: "invoice_paid:inv-1",
			}); err != nil {
				return err
			}
		}
		return nil
	})

	queue := notify.NewQueue(pool)
	mirror := &mirrorDouble{}
	queue.Mirror = mirror
	if _, err := queue.RunOnce(context.Background(), &fakeSender{}, 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(mirror.events) != 1 {
		t.Fatalf("mirrored %d times for one event, want 1 (%v)", len(mirror.events), mirror.events)
	}
	if mirror.events[0] != notify.EventInvoicePaid {
		t.Fatalf("mirrored event = %q, want invoice_paid", mirror.events[0])
	}

	// A second pass must not post again: the marker is the record that it already happened.
	before := len(mirror.events)
	sender := &fakeSender{}
	if _, err := queue.RunOnce(context.Background(), sender, 10); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(mirror.events) != before {
		t.Fatalf("the mirror posted again on a second drain: %v", mirror.events)
	}
}

// An event with no Discord counterpart is not mirrored at all.
func TestDiscordMirrorIgnoresEventsWithoutACounterpart(t *testing.T) {
	pool := storetest.DB(t)
	seedTemplates(t, pool)
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := notify.NewQueue(pool).Enqueue(ctx, tx, notify.Message{
			Kind:      notify.EventPasswordReset,
			To:        "reset@example.test",
			Subject:   "Reset",
			Body:      "Link",
			DedupeKey: "password_reset:tok",
		})
		return err
	})
	queue := notify.NewQueue(pool)
	mirror := &mirrorDouble{}
	queue.Mirror = mirror
	if _, err := queue.RunOnce(context.Background(), &fakeSender{}, 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(mirror.events) != 0 {
		t.Fatalf("a password reset was mirrored to Discord: %v", mirror.events)
	}
}

// A test mail is an outbox row with a branded HTML part and a plain-text fallback.
func TestTestEmailQueuesABrandedOutboxRow(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "general", "company_name", "Northwind Hosting")

	var id string
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = notify.SendTestEmail(ctx, tx, notify.NewQueue(pool), notify.TestEmailInput{
			To: "ops@example.test", RequestedBy: "ops@example.test",
		})
		return err
	})
	if id == "" {
		t.Fatal("the test email returned no delivery id")
	}

	var kind, body, htmlPart string
	if err := pool.QueryRow(context.Background(), `
		SELECT kind, body, body_html FROM notification_deliveries WHERE id = $1`, id).
		Scan(&kind, &body, &htmlPart); err != nil {
		t.Fatalf("read test email: %v", err)
	}
	if kind != notify.KindTestEmail {
		t.Fatalf("kind = %q, want %q", kind, notify.KindTestEmail)
	}
	if !strings.Contains(body, "Northwind Hosting") || !strings.Contains(body, "test message") {
		t.Fatalf("the test mail body is not the test sentence:\n%s", body)
	}
	if strings.Contains(body, "<") {
		t.Fatalf("the plain part of the test mail contains markup:\n%s", body)
	}
	if !strings.Contains(htmlPart, "Northwind Hosting") {
		t.Fatalf("the test mail has no branded HTML part:\n%s", htmlPart)
	}
}

// The same failed run alerts once, however many times it is reported.
func TestCronFailureAlertIsDeduplicatedByRun(t *testing.T) {
	pool := storetest.DB(t)
	setSetting(t, pool, "general", "system_email_address", "ops@example.test")

	send := func() notify.Outcome {
		var out notify.Outcome
		inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = notify.SendCronFailure(ctx, tx, notify.NewQueue(pool), notify.CronFailureInput{
				Job: "renewals", RunID: "33333333-3333-3333-3333-333333333333",
				Detail: "the invoice run panicked",
			})
			return err
		})
		return out
	}
	first := send()
	if !first.Mailed {
		t.Fatalf("the first alert was not queued: %+v", first)
	}
	second := send()
	if second.Mailed {
		t.Fatalf("the same run was alerted twice: %+v", second)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries WHERE kind = $1`,
		notify.KindSystemAlert).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 1 {
		t.Fatalf("alert rows = %d, want 1", count)
	}
}

// A cron alert with no configured address is recorded as skipped, not sent to nobody.
func TestCronFailureWithoutAnAddressIsSkipped(t *testing.T) {
	pool := storetest.DB(t)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM settings WHERE group_key = 'general' AND key = 'system_email_address'`); err != nil {
		t.Fatalf("clear address: %v", err)
	}
	var out notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = notify.SendCronFailure(ctx, tx, notify.NewQueue(pool), notify.CronFailureInput{
			Job: "renewals", RunID: "44444444-4444-4444-4444-444444444444",
		})
		return err
	})
	if out.Mailed {
		t.Fatal("an alert with no recipient reported a send")
	}
	if out.Reason != "no_recipient" {
		t.Fatalf("reason = %q, want no_recipient", out.Reason)
	}
}

// An unknown placeholder is refused at save time, and the refusal names the field.
//
// This is the behaviour that must survive every change in this package: a template error caught
// when the operator saves is a 422 with a sentence; caught at send time it is a customer reading
// "{{invoice_number}}", and caught as a 500 it is a payment that never told anyone anything.
func TestUnknownPlaceholderIsRefusedWithAFieldList(t *testing.T) {
	pool := storetest.DB(t)
	seedTemplates(t, pool)
	bad := notify.Template{
		EventKey: notify.EventInvoicePaid, Name: "Bad",
		Subject: "Invoice {{invoice_number}}", Body: "Total {{total_amount}}",
		Enabled: true, MailEnabled: true, InAppEnabled: true,
	}
	_, err := notify.SaveTemplate(context.Background(), pool, bad)
	if !errors.Is(err, notify.ErrUnknownPlaceholder) {
		t.Fatalf("SaveTemplate error = %v, want ErrUnknownPlaceholder", err)
	}
	fields := notify.UnknownPlaceholderFields(err)
	if len(fields) != 1 {
		t.Fatalf("fields = %+v, want one finding", fields)
	}
	if fields[0].Field != "body" || fields[0].Name != "total_amount" {
		t.Fatalf("finding = %+v, want body/total_amount", fields[0])
	}

	// And nothing was stored: the outbox can never render the refused template.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_templates WHERE event_key = $1`,
		notify.EventInvoicePaid).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("template rows = %d, want the seeded one", count)
	}
}
