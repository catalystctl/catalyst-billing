package notify_test

// Tests for the activation message (SF-11) and for the templated credential mails.
//
// Both properties under test are database properties, so they run against the real database:
// the message exists because the row it describes committed, the dedupe key is a unique index,
// and the `mail.disable` decision is recorded as a row rather than remembered in a variable.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// seedActivation creates a customer with a login, a product with the given activation snippet,
// and an active service for them. It returns the service id.
//
// It inserts the rows directly rather than going through checkout: this test is about what the
// activation message says, and a checkout bug should not be able to make it pass.
func seedActivation(t *testing.T, pool *store.Pool, snippet string) (serviceID, userID, productName string) {
	t.Helper()
	ctx := context.Background()

	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('buyer@example.test', 'Buyer') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}

	productName = "Game Server"
	var productID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name, email_template) VALUES ('game-server', $1, $2)
		RETURNING id`, productName, snippet).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}

	userID = seedUser(t, pool, "buyer@example.test")
	if _, err := pool.Exec(ctx,
		`INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		userID, customerID); err != nil {
		t.Fatalf("link account: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id, generation)
		VALUES ($1, $2, 'active', 'srv-42', 1) RETURNING id`,
		customerID, productID).Scan(&serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return serviceID, userID, productName
}

// queuedBody returns the body of the most recent message of a kind.
func queuedBody(t *testing.T, pool *store.Pool, kind, to string) string {
	t.Helper()
	var body string
	err := pool.QueryRow(context.Background(), `
		SELECT body FROM notification_deliveries
		WHERE kind = $1 AND to_address = $2
		ORDER BY created_at DESC LIMIT 1`, kind, to).Scan(&body)
	if err != nil {
		t.Fatalf("read %s message: %v", kind, err)
	}
	return body
}

// setSetting writes one settings row, which is how an operator's screen does it.
func setSetting(t *testing.T, pool *store.Pool, group, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
		group, key, value); err != nil {
		t.Fatalf("set %s.%s: %v", group, key, err)
	}
}

// A product's snippet reaches the activation message, and the message goes to the customer's
// inbox as well as their mailbox.
//
// This is the property SF-11 exists for: before it, `products.email_template` was a column
// nothing read, so a customer who bought a game server was told the same generic sentence as a
// customer who bought a domain.
func TestActivationMessageCarriesTheProductSnippet(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)

	const snippet = "Join at game.example.test:27015.\nThe console password is in your account."
	serviceID, userID, productName := seedActivation(t, pool, snippet)

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.ServiceActivated(ctx, tx, queue, serviceID)
		return err
	})

	if !res.Mailed {
		t.Fatalf("no message was queued: %+v", res)
	}
	if !res.Inboxed {
		t.Fatalf("no inbox row was written: %+v", res)
	}

	body := queuedBody(t, pool, notify.EventNewServerCreated, "buyer@example.test")
	if !strings.Contains(body, snippet) {
		t.Fatalf("the activation message does not contain the product snippet:\n%s", body)
	}
	if !strings.Contains(body, productName) {
		t.Fatalf("the activation message does not name the product:\n%s", body)
	}
	// The link in a mail must be absolute or the customer clicks nothing.
	if !strings.Contains(body, "/portal/services/"+serviceID) {
		t.Fatalf("the activation message has no service link:\n%s", body)
	}
	if !strings.Contains(body, "http") {
		t.Fatalf("the mail link was not made absolute:\n%s", body)
	}

	// The inbox row is the in-panel copy, and it must point at the customer-visible route.
	var url string
	if err := pool.QueryRow(ctx, `
		SELECT url FROM notifications WHERE user_id = $1 AND event_key = $2`,
		userID, notify.EventNewServerCreated).Scan(&url); err != nil {
		t.Fatalf("inbox row: %v", err)
	}
	if url != "/portal/services/"+serviceID {
		t.Fatalf("inbox url = %q, want the panel-relative service path", url)
	}
}

// A product with no snippet produces a sensible default, not a hole in the sentence.
func TestActivationMessageFallsBackToADefaultSentence(t *testing.T) {
	pool := storetest.DB(t)
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)

	serviceID, _, _ := seedActivation(t, pool, "")

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.ServiceActivated(ctx, tx, queue, serviceID)
		return err
	})
	if !res.Mailed {
		t.Fatalf("no message was queued: %+v", res)
	}

	body := queuedBody(t, pool, notify.EventNewServerCreated, "buyer@example.test")
	if !strings.Contains(body, notify.DefaultActivationInstructions) {
		t.Fatalf("the default instructions are missing:\n%s", body)
	}
	// An unrendered placeholder is the failure this test is really guarding: the template is
	// seeded with {{product_instructions}}, so a caller that does not supply it mails the
	// customer the braces.
	if strings.Contains(body, "{{") {
		t.Fatalf("the message contains an unrendered placeholder:\n%s", body)
	}
}

// Activation is idempotent on the service: a retried provisioning operation is one message.
func TestActivationIsIdempotentOnTheService(t *testing.T) {
	pool := storetest.DB(t)
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)
	serviceID, _, _ := seedActivation(t, pool, "Ask support for the panel login.")

	call := func() notify.Outcome {
		var res notify.Outcome
		inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = notify.ServiceActivated(ctx, tx, queue, serviceID)
			return err
		})
		return res
	}
	if first := call(); !first.Mailed {
		t.Fatalf("the first activation did not mail: %+v", first)
	}
	if second := call(); second.Mailed {
		t.Fatalf("a replayed activation mailed again: %+v", second)
	}

	var messages int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = $1 AND to_address = 'buyer@example.test'`,
		notify.EventNewServerCreated).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("activation messages = %d, want 1", messages)
	}
}

// `mail.disable` stops the mail and records why, and the inbox row still happens: the panel is
// where a customer can still be told something while the mail relay is off.
func TestMailDisableSuppressesTheMailAndKeepsTheInbox(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)
	serviceID, _, _ := seedActivation(t, pool, "Everything you need is in the panel.")
	setSetting(t, pool, "mail", "disable", "true")

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.ServiceActivated(ctx, tx, queue, serviceID)
		return err
	})

	if res.Mailed {
		t.Fatalf("mail.disable was set and a message was still queued: %+v", res)
	}
	if res.Reason != "mail_disabled_by_setting" {
		t.Fatalf("reason = %q, want mail_disabled_by_setting", res.Reason)
	}
	if !res.Inboxed {
		t.Fatalf("the inbox row was skipped as well: %+v", res)
	}

	// Nothing is claimable by the drain loop...
	var queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = $1 AND status IN ('queued', 'failed')`,
		notify.EventNewServerCreated).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("%d message(s) are still deliverable", queued)
	}
	// ...and the decision is a row, so support can answer "why was this not sent".
	var reason string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(last_error, '') FROM notification_deliveries
		WHERE kind = $1 AND status = 'suppressed'`, notify.EventNewServerCreated).Scan(&reason); err != nil {
		t.Fatalf("no suppressed row: %v", err)
	}
	if !strings.Contains(reason, "mail.disable") {
		t.Fatalf("suppression reason = %q", reason)
	}
}

// The credential mails are templates an operator can reword, not string literals.
//
// The wording is the operator's; what must not change is that the message is queued at all, that
// it carries the link, and that it lands in the inbox where the customer can see a reset was
// requested even if the mailbox is gone.
func TestCredentialMailsUseTheEditableTemplate(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)
	userID := seedUser(t, pool, "reset-me@example.test")

	// An operator rewrites the reset mail through the same store the staff screen writes.
	if _, err := notify.SaveTemplate(ctx, pool, notify.Template{
		EventKey:     notify.EventPasswordReset,
		Name:         "Password reset",
		Subject:      "Choose a new password for {{company_name}}",
		Body:         "Use this link within {{expires_in}}: {{link}}\n\nNeed help? Reply to this message.",
		InAppTitle:   "Reset requested",
		InAppBody:    "A reset was requested for this account.",
		InAppURL:     "/reset",
		Enabled:      true,
		MailEnabled:  true,
		InAppEnabled: true,
	}); err != nil {
		t.Fatalf("save template: %v", err)
	}

	in := notify.PasswordResetInput{
		UserID:    userID,
		Email:     "reset-me@example.test",
		Link:      "https://panel.example.test/reset?token=abc123",
		ExpiresIn: time.Hour,
		Token:     "abc123abc123",
	}
	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.PasswordReset(ctx, tx, queue, in)
		return err
	})
	if !res.Mailed || !res.Inboxed {
		t.Fatalf("outcome = %+v", res)
	}

	var subject, body string
	if err := pool.QueryRow(ctx, `
		SELECT subject, body FROM notification_deliveries
		WHERE kind = $1 AND to_address = 'reset-me@example.test'`,
		notify.KindPasswordReset).Scan(&subject, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subject, "Choose a new password") {
		t.Fatalf("the operator's subject was not used: %q", subject)
	}
	if !strings.Contains(body, "Need help? Reply to this message.") {
		t.Fatalf("the operator's body was not used:\n%s", body)
	}
	if !strings.Contains(body, "https://panel.example.test/reset?token=abc123") {
		t.Fatalf("the link is missing from the mail:\n%s", body)
	}
	// The mail keeps the single-use promise the token table enforces, in words the customer can
	// read. The template says {{expires_in}}, so this is also the proof that it was substituted.
	if !strings.Contains(body, "1 hours") && !strings.Contains(body, "an hour") {
		t.Fatalf("the expiry was not rendered into the mail:\n%s", body)
	}

	var title string
	if err := pool.QueryRow(ctx, `
		SELECT title FROM notifications WHERE user_id = $1 AND event_key = $2`,
		userID, notify.EventPasswordReset).Scan(&title); err != nil {
		t.Fatalf("inbox row: %v", err)
	}
	if title != "Reset requested" {
		t.Fatalf("inbox title = %q", title)
	}
}

// With mail switched off, a reset request sends nothing and still tells the customer in the
// panel. The endpoint's answer does not change: a 202 that depends on the mail setting would be
// an account oracle, because only an existing address reaches this code at all.
func TestCredentialMailIsSuppressedWhenMailIsDisabled(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)
	queue := notify.NewQueue(pool)
	userID := seedUser(t, pool, "quiet@example.test")
	setSetting(t, pool, "mail", "disable", "true")

	var res notify.Outcome
	inTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = notify.EmailVerification(ctx, tx, queue, notify.EmailVerificationInput{
			UserID: userID, Email: "quiet@example.test",
			Link: "https://panel.example.test/api/v1/auth/verify?token=x", ExpiresIn: time.Hour,
			Token: "abcdefabcdef",
		})
		return err
	})

	if res.Mailed {
		t.Fatalf("a verification mail was queued while mail.disable was set: %+v", res)
	}
	if res.Reason != "mail_disabled_by_setting" {
		t.Fatalf("reason = %q", res.Reason)
	}
	var deliveries int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = $1 AND status IN ('queued', 'failed')`,
		notify.KindEmailVerification).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("%d verification message(s) are deliverable", deliveries)
	}
}

// A template row that does not exist yet is the shipped wording, not an error.
//
// This is what keeps the credential flows working on an installation that has been migrated but
// not seeded — and it is why a test database needs no seeding step before it can exercise a
// password reset. An event key this build knows nothing about is still an error.
func TestTemplateForFallsBackToTheShippedDefault(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	// No seedTemplates: the table is empty, which is the state after Truncate.

	tmpl, err := notify.TemplateFor(ctx, pool, notify.EventPasswordReset)
	if err != nil {
		t.Fatalf("TemplateFor on an unseeded event: %v", err)
	}
	if !strings.Contains(tmpl.Body, "{{link}}") {
		t.Fatalf("the fallback is not the shipped reset wording: %+v", tmpl)
	}

	if _, err := notify.TemplateFor(ctx, pool, "not_an_event"); !errors.Is(err, notify.ErrTemplateNotFound) {
		t.Fatalf("an unknown event = %v, want ErrTemplateNotFound", err)
	}
}
