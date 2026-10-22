package notify_test

// Tests for the template store, the render step, the preference precedence, the inbox and
// the event functions.
//
// They run against the real database (storetest.DB) because the properties that matter are
// database properties: the partial unique index that makes a replay a no-op, the CHECK that
// constrains a preference value, and the transaction boundary that ties an inbox row to the
// change it describes. A stub would let all three pass while being false.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// seedUser inserts a user and returns its id. Users are the recipient of every channel, so
// almost every test needs one.
func seedUser(t *testing.T, pool *store.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, 'x', 'Recipient', now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedTemplates inserts the shipped defaults, which is what a running installation does on
// boot.
func seedTemplates(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := notify.SeedTemplates(context.Background(), pool); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
}

func inTx(t *testing.T, pool *store.Pool, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := pool.InTx(context.Background(), fn); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

// ------------------------------------------------------------------ render --

// Render substitutes the variables it is given and leaves an unknown placeholder visible.
//
// Leaving it visible is the deliberate choice: replacing an unknown variable with "" produces
// a sentence with a hole in it that the customer sees and the operator never learns about.
func TestRenderSubstitutesVariables(t *testing.T) {
	tmpl := notify.Template{
		EventKey:   "invoice_paid",
		Subject:    "Payment received for invoice {{invoice_number}}",
		Body:       "Invoice {{invoice_number}} was paid: {{total}}.\n{{unknown_key}}",
		InAppTitle: "Paid {{invoice_number}}",
		InAppBody:  "{{total}}",
		InAppURL:   "{{invoice_url}}",
	}
	r := notify.Render(tmpl, map[string]string{
		"invoice_number": "INV-000042",
		"total":          "USD 12.00",
		"invoice_url":    "/invoices/abc",
	})

	if want := "Payment received for invoice INV-000042"; r.Subject != want {
		t.Fatalf("subject = %q, want %q", r.Subject, want)
	}
	if !strings.Contains(r.Body, "Invoice INV-000042 was paid: USD 12.00.") {
		t.Fatalf("body did not substitute: %q", r.Body)
	}
	if !strings.Contains(r.Body, "{{unknown_key}}") {
		t.Fatalf("an unknown placeholder was replaced instead of left visible: %q", r.Body)
	}
	if r.InAppTitle != "Paid INV-000042" || r.InAppBody != "USD 12.00" ||
		r.InAppURL != "/invoices/abc" {
		t.Fatalf("in-app fields did not substitute: %+v", r)
	}
}

// The render step is not a template engine: markup in the data stays markup, as literal
// text. This is the property that stops an invoice description turning a billing mail into a
// phishing page.
func TestRenderDoesNotInterpretMarkup(t *testing.T) {
	hostile := `<a href="https://evil.example/reset">Click to restore your account</a>`
	tmpl := notify.Template{
		EventKey: "new_invoice_created",
		Subject:  "Invoice {{invoice_number}}",
		Body:     "Line: {{description}}",
	}
	r := notify.Render(tmpl, map[string]string{
		"invoice_number": "INV-1",
		"description":    hostile,
	})

	// The exact bytes survive. Not escaped, not stripped, not re-encoded: the output is
	// text/plain and a "<" is a "<".
	if !strings.Contains(r.Body, hostile) {
		t.Fatalf("markup in a value was altered:\n got %q\nwant to contain %q", r.Body, hostile)
	}
	// And it is not double-escaped either, which is what an HTML escaper would do.
	if strings.Contains(r.Body, "&lt;") {
		t.Fatalf("the value was HTML-escaped, which implies the output is parsed as markup: %q", r.Body)
	}
}

// A value that itself looks like a placeholder is not expanded. Substitution is one pass, so
// data cannot choose the template.
func TestRenderIsSinglePass(t *testing.T) {
	tmpl := notify.Template{
		EventKey: "x",
		Subject:  "Hi {{name}}",
		Body:     "{{name}}",
	}
	r := notify.Render(tmpl, map[string]string{
		"name":  "{{total}}",
		"total": "USD 999.00",
	})
	if r.Body != "{{total}}" {
		t.Fatalf("a substituted value was re-expanded: %q", r.Body)
	}
}

// A newline in a value cannot reach the Subject header.
func TestRenderStripsHeaderInjection(t *testing.T) {
	tmpl := notify.Template{
		EventKey: "x",
		Subject:  "Invoice {{number}}",
	}
	r := notify.Render(tmpl, map[string]string{
		"number": "INV-1\r\nBcc: attacker@example.test",
	})
	if strings.ContainsAny(r.Subject, "\r\n") {
		t.Fatalf("subject carries a line break: %q", r.Subject)
	}
}

// ------------------------------------------------------------------ seeding --

// Seeding twice inserts nothing the second time and never overwrites an edit.
func TestSeedTemplatesIsIdempotent(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	first, err := notify.SeedTemplates(ctx, pool)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if first != len(notify.DefaultTemplates()) {
		t.Fatalf("first seed inserted %d rows, want %d", first, len(notify.DefaultTemplates()))
	}
	second, err := notify.SeedTemplates(ctx, pool)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second != 0 {
		t.Fatalf("second seed inserted %d rows, want 0", second)
	}

	// An operator's edit must survive a redeploy.
	edited := notify.Template{
		EventKey: notify.EventInvoicePaid, Name: "Edited", Subject: "Edited subject",
		Body: "Edited body", InAppTitle: "T", Enabled: true, MailEnabled: true, InAppEnabled: true,
	}
	if _, err := notify.SaveTemplate(ctx, pool, edited); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := notify.SeedTemplates(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got, err := notify.TemplateFor(ctx, pool, notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Subject != "Edited subject" {
		t.Fatalf("seeding overwrote an operator's edit: subject = %q", got.Subject)
	}
}

// Every event in the catalogue has a shipped default, so a fresh installation can send every
// event without an operator touching anything.
func TestEveryEventHasADefaultTemplate(t *testing.T) {
	have := map[string]bool{}
	for _, t := range notify.DefaultTemplates() {
		have[t.EventKey] = true
	}
	for _, e := range notify.Catalogue() {
		if !have[e.Key] {
			t.Fatalf("event %q has no default template", e.Key)
		}
	}
	if len(have) != len(notify.Catalogue()) {
		t.Fatalf("there are %d defaults for %d events", len(have), len(notify.Catalogue()))
	}
}

// A missing template is an error, not an empty render.
func TestTemplateForMissingEventFails(t *testing.T) {
	pool := storetest.DB(t)
	if _, err := notify.TemplateFor(context.Background(), pool, "no_such_event"); err == nil {
		t.Fatal("a missing template returned no error")
	}
}

// ------------------------------------------------------------- preferences --

// A force event ignores a stored choice_off. This is the property that stops a customer
// locking themselves out of their own password reset.
func TestForceEventIgnoresChoiceOff(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "force@example.test")

	// Write the opt-out directly: SetPreference refuses it, which is the write-path half of
	// the same rule and is tested separately. Here we prove the *send* path cannot be fooled
	// by a row that exists anyway (an import, a hand-written row, an older bug).
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference)
		VALUES ($1, $2, 'choice_off')`, userID, notify.EventPasswordReset); err != nil {
		t.Fatalf("insert preference: %v", err)
	}

	d, err := notify.Decide(ctx, pool, userID, notify.EventPasswordReset)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !d.Mail || !d.InApp {
		t.Fatalf("a force event was turned off: %+v", d)
	}
	if d.Preference != notify.PreferenceForce {
		t.Fatalf("effective preference = %q, want force", d.Preference)
	}
}

// A choice event honours choice_off.
func TestChoiceEventHonoursChoiceOff(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "choice@example.test")

	// On by default.
	d, err := notify.Decide(ctx, pool, userID, notify.EventNewInvoiceCreated)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !d.Mail || !d.InApp {
		t.Fatalf("an unset choice event should default to on: %+v", d)
	}

	if err := notify.SetPreference(ctx, pool, userID, notify.EventNewInvoiceCreated,
		notify.PreferenceChoiceOff); err != nil {
		t.Fatalf("set preference: %v", err)
	}
	d, err = notify.Decide(ctx, pool, userID, notify.EventNewInvoiceCreated)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Mail || d.InApp {
		t.Fatalf("choice_off was ignored: %+v", d)
	}
	if d.MailReason != "preference_off" {
		t.Fatalf("reason = %q, want preference_off", d.MailReason)
	}
}

// `never` outranks `force`: it is an operator decision, not a user one.
func TestNeverOutranksForce(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "never@example.test")

	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference)
		VALUES ($1, $2, 'never')`, userID, notify.EventPasswordReset); err != nil {
		t.Fatalf("insert preference: %v", err)
	}
	d, err := notify.Decide(ctx, pool, userID, notify.EventPasswordReset)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Mail || d.InApp {
		t.Fatalf("`never` was overridden by force: %+v", d)
	}
}

// The write path refuses the two combinations that would make an account unreachable.
func TestSetPreferenceRefusesForceOptOut(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "refuse@example.test")

	for _, p := range []string{notify.PreferenceChoiceOff, notify.PreferenceNever} {
		err := notify.SetPreference(ctx, pool, userID, notify.EventEmailVerification, p)
		if err == nil {
			t.Fatalf("storing %q against a force event was allowed", p)
		}
	}
	// And a user cannot force an event that is not force: that would give them a
	// notification they cannot then turn off.
	if err := notify.SetPreference(ctx, pool, userID, notify.EventInvoicePaid,
		notify.PreferenceForce); err == nil {
		t.Fatal("a user was allowed to force a choice event")
	}
	// An unknown event is refused rather than stored and silently ignored later.
	if err := notify.SetPreference(ctx, pool, userID, "no_such_event",
		notify.PreferenceChoiceOn); err == nil {
		t.Fatal("an unknown event key was accepted")
	}
}

// Preferences resolves the whole catalogue, including events the user never touched.
func TestPreferencesListsWholeCatalogue(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "list@example.test")

	if err := notify.SetPreference(ctx, pool, userID, notify.EventInvoicePaid,
		notify.PreferenceChoiceOff); err != nil {
		t.Fatalf("set: %v", err)
	}
	rows, err := notify.Preferences(ctx, pool, userID)
	if err != nil {
		t.Fatalf("preferences: %v", err)
	}
	if len(rows) != len(notify.Catalogue()) {
		t.Fatalf("got %d rows, want %d", len(rows), len(notify.Catalogue()))
	}
	for _, r := range rows {
		switch r.EventKey {
		case notify.EventInvoicePaid:
			if r.Preference != notify.PreferenceChoiceOff || r.Mail {
				t.Fatalf("invoice_paid row = %+v, want choice_off and no mail", r)
			}
		case notify.EventPasswordReset:
			if !r.Force || r.Preference != notify.PreferenceForce {
				t.Fatalf("password_reset row = %+v, want force", r)
			}
		}
	}
}

// ------------------------------------------------------------------- inbox --

// The inbox marks read, the unread count follows, and mark-all clears it.
func TestInboxMarksReadAndCountsUnread(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "inbox@example.test")

	var ids []string
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		for i, title := range []string{"first", "second", "third"} {
			created, err := notify.Create(ctx, tx, notify.Notification{
				UserID:    userID,
				EventKey:  notify.EventInvoicePaid,
				Title:     title,
				DedupeKey: "test:" + title,
			})
			if err != nil {
				return err
			}
			if !created {
				t.Fatalf("notification %d was not created", i)
			}
		}
		rows, _, err := notify.List(ctx, tx, userID, notify.InboxFilter{}, 10, "")
		if err != nil {
			return err
		}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return nil
	})

	if n, err := notify.UnreadCount(ctx, pool, userID); err != nil || n != 3 {
		t.Fatalf("unread = %d (err %v), want 3", n, err)
	}

	if err := notify.MarkRead(ctx, pool, userID, ids[0]); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if n, _ := notify.UnreadCount(ctx, pool, userID); n != 2 {
		t.Fatalf("unread = %d, want 2", n)
	}

	// Marking the same row again succeeds and does not decrement twice.
	if err := notify.MarkRead(ctx, pool, userID, ids[0]); err != nil {
		t.Fatalf("re-mark read: %v", err)
	}
	if n, _ := notify.UnreadCount(ctx, pool, userID); n != 2 {
		t.Fatalf("unread = %d after a re-mark, want 2", n)
	}

	marked, err := notify.MarkAllRead(ctx, pool, userID)
	if err != nil {
		t.Fatalf("mark all: %v", err)
	}
	if marked != 2 {
		t.Fatalf("mark-all changed %d rows, want 2", marked)
	}
	if n, _ := notify.UnreadCount(ctx, pool, userID); n != 0 {
		t.Fatalf("unread = %d after mark-all, want 0", n)
	}
}

// A notification belonging to another user cannot be marked read, and the error does not
// distinguish "not yours" from "does not exist".
func TestInboxIsScopedToTheOwner(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "owner@example.test")
	other := seedUser(t, pool, "other@example.test")

	var id string
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO notifications (user_id, event_key, title) VALUES ($1, 'invoice_paid', 't')
			RETURNING id`, owner).Scan(&id)
	})

	if err := notify.MarkRead(ctx, pool, other, id); err == nil {
		t.Fatal("another user marked the notification read")
	}
	if err := notify.Delete(ctx, pool, other, id); err == nil {
		t.Fatal("another user deleted the notification")
	}
	// The owner's list is the only one that contains it.
	rows, _, err := notify.List(ctx, pool, other, notify.InboxFilter{}, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("another user's inbox returned %d rows", len(rows))
	}
	if n, _ := notify.UnreadCount(ctx, pool, owner); n != 1 {
		t.Fatalf("the owner's unread count changed: %d", n)
	}
}

// A replayed create with the same dedupe key writes one row, which is what makes a retried
// webhook safe. The second call reports created=false rather than an error.
func TestInboxDedupeIsExact(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "dedupe@example.test")

	row := notify.Notification{
		UserID: userID, EventKey: notify.EventInvoicePaid,
		Title: "paid", DedupeKey: "invoice_paid:inv-1",
	}
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		created, err := notify.Create(ctx, tx, row)
		if err != nil {
			return err
		}
		if !created {
			t.Fatal("the first create reported no row")
		}
		again, err := notify.Create(ctx, tx, row)
		if err != nil {
			return err
		}
		if again {
			t.Fatal("the replay created a second row")
		}
		return nil
	})

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d rows for one dedupe key, want 1", n)
	}
}

// A notification with no dedupe key inserts every time: a human-triggered notice must never
// be swallowed by another one.
func TestInboxWithoutDedupeKeyAlwaysInserts(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "nokey@example.test")

	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < 2; i++ {
			created, err := notify.Create(ctx, tx, notify.Notification{
				UserID: userID, EventKey: "announcement", Title: "notice",
			})
			if err != nil {
				return err
			}
			if !created {
				t.Fatalf("insert %d reported no row", i)
			}
		}
		return nil
	})
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

// ---------------------------------------------------------------- events --

// A replayed enqueue with the same dedupe key creates one delivery row and one inbox row.
func TestEventReplayWritesOneRowPerChannel(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "replay@example.test")
	queue := notify.NewQueue(pool)

	in := notify.InvoicePaidInput{
		UserID: userID, Email: "replay@example.test",
		InvoiceID: "11111111-1111-1111-1111-111111111111",
		Number:    "INV-000001", Total: "USD 12.00",
	}

	var first, second notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = notify.InvoicePaid(ctx, tx, queue, in)
		return err
	})
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = notify.InvoicePaid(ctx, tx, queue, in)
		return err
	})

	if !first.Mailed || !first.Inboxed {
		t.Fatalf("first call did not write both channels: %+v", first)
	}
	if second.Mailed || second.Inboxed {
		t.Fatalf("the replay wrote a row: %+v", second)
	}

	var deliveries, inbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries
		WHERE kind = $1`, notify.EventInvoicePaid).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications
		WHERE user_id = $1 AND event_key = $2`, userID, notify.EventInvoicePaid).Scan(&inbox); err != nil {
		t.Fatalf("count inbox: %v", err)
	}
	if deliveries != 1 || inbox != 1 {
		t.Fatalf("deliveries = %d, inbox = %d; want 1 and 1", deliveries, inbox)
	}
}

// A choice event the user turned off writes no mail and no inbox row, and says why.
func TestEventHonoursChoiceOffEndToEnd(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "off@example.test")
	queue := notify.NewQueue(pool)

	if err := notify.SetPreference(ctx, pool, userID, notify.EventInvoicePaid,
		notify.PreferenceChoiceOff); err != nil {
		t.Fatalf("set preference: %v", err)
	}

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.InvoicePaid(ctx, tx, queue, notify.InvoicePaidInput{
			UserID: userID, Email: "off@example.test",
			InvoiceID: "22222222-2222-2222-2222-222222222222",
			Number:    "INV-2", Total: "USD 1.00",
		})
		return err
	})
	if res.Mailed || res.Inboxed {
		t.Fatalf("a choice_off event was sent: %+v", res)
	}
	if res.Reason != "preference_off" {
		t.Fatalf("reason = %q, want preference_off", res.Reason)
	}
}

// A force event is sent even when a choice_off row exists for it, and the row lands in the
// outbox with the event key as its kind.
//
// The opt-out is written directly because SetPreference refuses it — that refusal is the
// write-path half of the same rule, tested in TestSetPreferenceRefusesForceOptOut. Here we
// prove the *send* path cannot be fooled by a row that exists anyway: an import, a
// hand-written row, or an older build.
func TestForceEventIsSentRegardless(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "forced@example.test")
	queue := notify.NewQueue(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference)
		VALUES ($1, $2, 'choice_off')`, userID, notify.EventPasswordReset); err != nil {
		t.Fatalf("insert preference: %v", err)
	}

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.PasswordReset(ctx, tx, queue, notify.PasswordResetInput{
			UserID: userID, Email: "forced@example.test",
			Link: "http://127.0.0.1:8080/reset?token=abc", ExpiresIn: 24 * time.Hour,
			Token: "abc123def456",
		})
		return err
	})
	if !res.Mailed || !res.Inboxed {
		t.Fatalf("a force event was suppressed: %+v", res)
	}

	var kind, body string
	if err := pool.QueryRow(ctx, `
		SELECT kind, body FROM notification_deliveries WHERE kind = $1`,
		notify.EventPasswordReset).Scan(&kind, &body); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if kind != notify.KindPasswordReset {
		t.Fatalf("outbox kind = %q, want %q", kind, notify.KindPasswordReset)
	}
	if !strings.Contains(body, "http://127.0.0.1:8080/reset?token=abc") {
		t.Fatalf("the reset link is not in the body: %q", body)
	}
	if !strings.Contains(body, "24 hours") {
		t.Fatalf("the expiry was not rendered: %q", body)
	}
}

// A template's own channel toggle is applied after the preference: disabling the in-app
// channel suppresses the inbox row but not the mail.
func TestTemplateChannelToggleAppliesAfterPreference(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "toggle@example.test")
	queue := notify.NewQueue(pool)

	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		t, err := notify.TemplateFor(ctx, tx, notify.EventNewLoginDetected)
		if err != nil {
			return err
		}
		t.InAppEnabled = false
		if _, err := notify.SaveTemplate(ctx, tx, t); err != nil {
			return err
		}
		return nil
	})

	if saved, err := notify.TemplateFor(ctx, pool, notify.EventNewLoginDetected); err != nil {
		t.Fatalf("read template: %v", err)
	} else if saved.InAppEnabled {
		t.Fatal("the in-app toggle was not saved")
	}

	var login notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		login, err = notify.NewLogin(ctx, tx, queue, notify.NewLoginInput{
			UserID: userID, Email: "toggle@example.test",
			AuthEventID: "55555555-5555-5555-5555-555555555555",
			IP:          "203.0.113.9", UserAgent: "test",
		})
		return err
	})
	if !login.Mailed {
		t.Fatalf("the mail channel was suppressed by an in-app toggle: %+v", login)
	}
	if login.Inboxed {
		t.Fatalf("an in-app-disabled event wrote an inbox row: %+v", login)
	}
	if login.Reason != "in_app_disabled" {
		t.Fatalf("reason = %q, want in_app_disabled", login.Reason)
	}
}

// A disabled template sends nothing on any channel, which is how an operator silences an
// event without deleting its wording.
func TestDisabledTemplateSendsNothing(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "disabled@example.test")
	queue := notify.NewQueue(pool)
	_ = ctx

	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		t, err := notify.TemplateFor(ctx, tx, notify.EventInvoicePaid)
		if err != nil {
			return err
		}
		t.Enabled = false
		_, err = notify.SaveTemplate(ctx, tx, t)
		return err
	})

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.InvoicePaid(ctx, tx, queue, notify.InvoicePaidInput{
			UserID: userID, Email: "disabled@example.test",
			InvoiceID: "66666666-6666-6666-6666-666666666666",
			Number:    "INV-6", Total: "USD 3.00",
		})
		return err
	})
	if res.Mailed || res.Inboxed {
		t.Fatalf("a disabled template sent something: %+v", res)
	}
	if res.Reason != "template_disabled" {
		t.Fatalf("reason = %q, want template_disabled", res.Reason)
	}
}

// The mail carries the rendered template, and a relative panel path becomes absolute so the
// customer can actually click it.
func TestEventMailIsRenderedAndLinkIsAbsolute(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "render@example.test")
	queue := notify.NewQueue(pool)

	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoicePaid(ctx, tx, queue, notify.InvoicePaidInput{
			UserID: userID, Email: "render@example.test",
			InvoiceID: "77777777-7777-7777-7777-777777777777",
			Number:    "INV-000777", Total: "USD 42.00",
			InvoiceURL: "/portal/invoices/77777777-7777-7777-7777-777777777777",
		})
		return err
	})

	var subject, body string
	if err := pool.QueryRow(ctx, `
		SELECT subject, body FROM notification_deliveries WHERE kind = $1`,
		notify.EventInvoicePaid).Scan(&subject, &body); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if !strings.Contains(subject, "INV-000777") {
		t.Fatalf("subject was not rendered: %q", subject)
	}
	if !strings.Contains(body, "USD 42.00") {
		t.Fatalf("body was not rendered: %q", body)
	}
	if strings.Contains(body, "{{") {
		t.Fatalf("body still contains a placeholder: %q", body)
	}
	// An absolute link, over either scheme. Asserting on `http://` specifically was wrong: the
	// deployment's public base URL decides the scheme, and a mail that links to an https origin is
	// more correct than one that does not. What matters is that the link is absolute at all — a
	// relative path in an email is unclickable, which is the defect this test exists for.
	if !strings.Contains(body, "://") || !strings.Contains(body, "/portal/invoices/7777") {
		t.Fatalf("the invoice link is not absolute: %q", body)
	}

	// The inbox keeps the relative path: it is rendered inside the panel.
	var url string
	if err := pool.QueryRow(ctx,
		`SELECT url FROM notifications WHERE user_id = $1`, userID).Scan(&url); err != nil {
		t.Fatalf("read inbox url: %v", err)
	}
	if !strings.HasPrefix(url, "/portal/invoices/") {
		t.Fatalf("inbox url = %q, want a panel-relative path", url)
	}
}

// CC and BCC produce their own outbox rows, so one row is still one recipient and a retry
// still cannot double-send.
func TestEventCCAndBCCFanOut(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "cc@example.test")
	queue := notify.NewQueue(pool)

	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		t, err := notify.TemplateFor(ctx, tx, notify.EventInvoicePaid)
		if err != nil {
			return err
		}
		t.CC = "billing@example.test"
		t.BCC = "archive@example.test, second@example.test"
		if _, err := notify.SaveTemplate(ctx, tx, t); err != nil {
			return err
		}
		return nil
	})

	in := notify.InvoicePaidInput{
		UserID: userID, Email: "cc@example.test",
		InvoiceID: "88888888-8888-8888-8888-888888888888",
		Number:    "INV-8", Total: "USD 4.00",
	}
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoicePaid(ctx, tx, queue, in)
		return err
	})
	// A replay must not add rows for the copies either.
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoicePaid(ctx, tx, queue, in)
		return err
	})

	rows, err := pool.Query(ctx, `
		SELECT to_address FROM notification_deliveries WHERE kind = $1 ORDER BY to_address`,
		notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, to)
	}
	want := []string{"archive@example.test", "billing@example.test", "cc@example.test", "second@example.test"}
	if len(got) != len(want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recipients = %v, want %v", got, want)
		}
	}
}

// An event with no recipient address writes the inbox row and skips the mail, and says so.
func TestEventWithoutEmailStillWritesInbox(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	userID := seedUser(t, pool, "nomail@example.test")
	queue := notify.NewQueue(pool)

	// The preference is untouched, so the event is on; only the address is missing.
	if d, err := notify.Decide(ctx, pool, userID, notify.EventInvoicePaid); err != nil {
		t.Fatalf("decide: %v", err)
	} else if !d.Mail {
		t.Fatalf("the event should be enabled by default: %+v", d)
	}

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.InvoicePaid(ctx, tx, queue, notify.InvoicePaidInput{
			UserID: userID,
			// No Email.
			InvoiceID: "99999999-9999-9999-9999-999999999999",
			Number:    "INV-9", Total: "USD 5.00",
		})
		return err
	})
	if res.Mailed {
		t.Fatalf("a message was mailed with no recipient: %+v", res)
	}
	if !res.Inboxed {
		t.Fatalf("the inbox row was skipped: %+v", res)
	}
	if res.Reason != "no_recipient" {
		t.Fatalf("reason = %q, want no_recipient", res.Reason)
	}
}

// Every default link points somewhere a *customer* can actually reach.
//
// A notification is read by a customer, and the panel only lets a customer reach /portal,
// /account, /cart, /checkout and /tickets. A default of "/invoices/<id>" would deep-link a
// customer into the staff invoice screen and bounce them out of their own area — a broken
// link in the one place a customer is most likely to click it. This test pins the rule so a
// future default cannot quietly reintroduce it.
func TestDefaultLinksAreCustomerReachable(t *testing.T) {
	customerPrefixes := []string{"/portal", "/account", "/cart", "/checkout", "/tickets"}

	reachable := func(path string) bool {
		for _, p := range customerPrefixes {
			if path == p || strings.HasPrefix(path, p+"/") {
				return true
			}
		}
		return false
	}

	// Every default is expressed as a literal in the event functions, so assert on the
	// rendered result with the optional URLs left empty — that is exactly the fallback path.
	cases := map[string]func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error){
		notify.EventNewInvoiceCreated: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.InvoiceIssued(ctx, tx, q, notify.InvoiceIssuedInput{
				UserID: userID, Email: "links@example.test",
				InvoiceID: "11111111-1111-1111-1111-111111111111",
			})
		},
		notify.EventInvoicePaid: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.InvoicePaid(ctx, tx, q, notify.InvoicePaidInput{
				UserID: userID, Email: "links@example.test",
				InvoiceID: "22222222-2222-2222-2222-222222222222",
			})
		},
		notify.EventInvoicePaymentFailed: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.InvoicePaymentFailed(ctx, tx, q, notify.InvoicePaymentFailedInput{
				UserID: userID, Email: "links@example.test",
				InvoiceID: "33333333-3333-3333-3333-333333333333",
			})
		},
		notify.EventNewOrderCreated: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.OrderCreated(ctx, tx, q, notify.OrderCreatedInput{
				UserID: userID, Email: "links@example.test",
				OrderID: "44444444-4444-4444-4444-444444444444",
			})
		},
		notify.EventNewServerCreated: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.ServerCreated(ctx, tx, q, notify.ServerCreatedInput{
				UserID: userID, Email: "links@example.test",
				ServiceID: "55555555-5555-5555-5555-555555555555",
			})
		},
		notify.EventServerSuspended: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.ServerSuspended(ctx, tx, q, notify.ServerSuspendedInput{
				UserID: userID, Email: "links@example.test",
				ServiceID: "66666666-6666-6666-6666-666666666666",
			})
		},
		notify.EventServerTerminated: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.ServerTerminated(ctx, tx, q, notify.ServerTerminatedInput{
				UserID: userID, Email: "links@example.test",
				ServiceID: "77777777-7777-7777-7777-777777777777",
			})
		},
		notify.EventServiceCancellationReceived: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.CancellationReceived(ctx, tx, q, notify.CancellationReceivedInput{
				UserID: userID, Email: "links@example.test",
				ServiceID: "88888888-8888-8888-8888-888888888888",
			})
		},
		notify.EventNewTicketMessage: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.TicketMessage(ctx, tx, q, notify.TicketMessageInput{
				UserID: userID, Email: "links@example.test",
				TicketID: "99999999-9999-9999-9999-999999999999",
			})
		},
		notify.EventNewLoginDetected: func(ctx context.Context, tx pgx.Tx, q *notify.Queue, userID string) (notify.Outcome, error) {
			return notify.NewLogin(ctx, tx, q, notify.NewLoginInput{
				UserID: userID, Email: "links@example.test",
				AuthEventID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			})
		},
	}

	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)
	userID := seedUser(t, pool, "links@example.test")

	for event, call := range cases {
		var res notify.Outcome
		inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = call(ctx, tx, queue, userID)
			return err
		})
		_ = res

		// The inbox row carries the relative path, which is the one a customer clicks.
		var url string
		if err := pool.QueryRow(ctx, `
			SELECT url FROM notifications
			WHERE user_id = $1 AND event_key = $2
			ORDER BY created_at DESC LIMIT 1`, userID, event).Scan(&url); err != nil {
			t.Fatalf("%s: no inbox row to check: %v", event, err)
		}
		if url == "" {
			t.Fatalf("%s: the inbox row has no url", event)
		}
		if !reachable(url) {
			t.Fatalf("%s: default url %q is not a customer-reachable path (allowed prefixes %v)",
				event, url, customerPrefixes)
		}
		if strings.HasPrefix(url, "http") {
			t.Fatalf("%s: the inbox url must be panel-relative, got %q", event, url)
		}
	}
}
