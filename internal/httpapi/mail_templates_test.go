package httpapi_test

// The credential mails and the product activation snippet, over HTTP.
//
// The properties these tests pin down are the ones a regression would break silently:
//
//   - a product's activation snippet is part of the product API, and it is guarded by
//     catalog:write like every other product field (a billing user can price an invoice, not
//     change what is for sale);
//   - the wording a customer receives during a security flow is the operator's, edited through
//     the staff template API — and the token in it is still single use;
//   - `mail.disable` stops the mail, records why, keeps the in-app copy, and does not change the
//     response: an answer that depends on the setting would be an account oracle.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// tokenFromDelivery reads the token out of the queued message, which is what the customer
// receives. The plaintext is never recoverable from the token table — that is the security
// property — so the outbox is the only honest way to drive the flow.
func tokenFromDelivery(t *testing.T, env *authEnv, kind, to string) string {
	t.Helper()
	var body string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT body FROM notification_deliveries
		WHERE kind = $1 AND to_address = $2 AND status = 'queued'
		ORDER BY created_at DESC LIMIT 1`, kind, to).Scan(&body); err != nil {
		t.Fatalf("read %s message: %v", kind, err)
	}
	idx := strings.Index(body, "token=")
	if idx < 0 {
		t.Fatalf("no token in the message:\n%s", body)
	}
	token := body[idx+len("token="):]
	if nl := strings.IndexAny(token, "\r\n \t"); nl >= 0 {
		token = token[:nl]
	}
	return token
}

// deliveryFor returns the subject and body of the newest queued message of a kind.
func deliveryFor(t *testing.T, env *authEnv, kind, to string) (subject, body string) {
	t.Helper()
	err := env.pool.QueryRow(context.Background(), `
		SELECT subject, body FROM notification_deliveries
		WHERE kind = $1 AND to_address = $2 AND status = 'queued'
		ORDER BY created_at DESC LIMIT 1`, kind, to).Scan(&subject, &body)
	if err != nil {
		t.Fatalf("read %s message: %v", kind, err)
	}
	return subject, body
}

// setMailDisable flips the installation-wide mail switch the way the settings screen does.
func setMailDisable(t *testing.T, env *authEnv, value string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('mail', 'disable', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatalf("set mail.disable: %v", err)
	}
}

// --------------------------------------------------------- the product field --

// A product's activation snippet is settable through the existing product API, and it is guarded
// by catalog:write rather than by a new permission.
func TestProductEmailTemplateNeedsCatalogWrite(t *testing.T) {
	env := newStaffEnv(t)

	const snippet = "Connect over SSH to the address in your welcome mail.\nThe root password is in the panel."
	rec := env.postJSON(t, "/api/v1/staff/catalog/products", env.admin, env.adminCSRF, map[string]any{
		"slug": "managed-vps", "name": "Managed VPS", "email_template": snippet,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create product = %d: %s", rec.Code, rec.Body)
	}
	created := decodeBody[map[string]any](t, rec)
	productID, _ := created["id"].(string)
	if created["email_template"] != snippet {
		t.Fatalf("the snippet did not round-trip: %v", created["email_template"])
	}

	// Billing may read the catalog to price an invoice and must not be able to change what a
	// customer is told when they buy. Same guard as the rest of the product body.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID, env.billing, env.billingCSRF,
		map[string]any{"slug": "managed-vps", "name": "Managed VPS",
			"email_template": "Ignore the panel; email us instead."})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing edit = %d, want 403: %s", rec.Code, rec.Body)
	}

	// The read path exposes it to staff, which is what the product screen needs to render the
	// field. It is deliberately not in the public storefront shape.
	rec = env.get(t, "/api/v1/staff/catalog/products/"+productID, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("read product = %d: %s", rec.Code, rec.Body)
	}
	if got := decodeBody[map[string]any](t, rec)["email_template"]; got != snippet {
		t.Fatalf("stored snippet = %v, want the one that was set", got)
	}
	rec = env.publicGet(t, "/api/v1/catalog")
	if strings.Contains(rec.Body.String(), "email_template") {
		t.Fatalf("the storefront read exposes the activation snippet: %s", rec.Body)
	}

	// An over-long snippet is refused as invalid input rather than silently truncated: a
	// half-sentence instruction is worse than a rejected one.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID, env.admin, env.adminCSRF,
		map[string]any{"slug": "managed-vps", "name": "Managed VPS",
			"email_template": strings.Repeat("x", 4001)})
	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("over-long snippet = %d, want 4xx: %s", rec.Code, rec.Body)
	}
}

// A product update that does not mention the snippet leaves it alone.
//
// The panel's product form predates this field, and its "publish / hide" toggle PUTs the whole
// product without it. If an absent field meant "empty", toggling visibility would silently wipe
// the instructions a customer is told when they buy — the kind of data loss nobody notices until
// a customer asks how to log in.
func TestProductUpdateWithoutTheSnippetLeavesItAlone(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.postJSON(t, "/api/v1/staff/catalog/products", env.admin, env.adminCSRF, map[string]any{
		"slug": "kept", "name": "Kept", "email_template": "The panel login is the one you registered with.",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	productID, _ := decodeBody[map[string]any](t, rec)["id"].(string)

	// Exactly the body the visibility toggle sends: no email_template key at all.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID, env.admin, env.adminCSRF,
		map[string]any{"slug": "kept", "name": "Kept", "hidden": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}
	if got := decodeBody[map[string]any](t, rec)["email_template"]; got != "The panel login is the one you registered with." {
		t.Fatalf("an update that omitted the snippet erased it: %v", got)
	}

	// An explicit empty string does clear it, so "remove my instructions" is expressible.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID, env.admin, env.adminCSRF,
		map[string]any{"slug": "kept", "name": "Kept", "email_template": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body)
	}
	if got := decodeBody[map[string]any](t, rec)["email_template"]; got != "" {
		t.Fatalf("an explicit empty snippet did not clear the field: %v", got)
	}
}

// ------------------------------------------------------------------ wording --

// An operator rewords the reset mail and the customer receives that wording — and the token in it
// is still single use, because the wording is a template and not a second implementation of the
// flow.
func TestOperatorRewordedResetMailReachesTheCustomer(t *testing.T) {
	t.Setenv("BILLING_PUBLIC_URL", "https://panel.example.test")
	env := newStaffEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "reword@example.test", "a good password 1", "")

	const subject = "Your {{company_name}} password reset link"
	const body = "Hello. Choose a new password here:\n{{link}}\n\nThis link works once and expires in {{expires_in}}.\n\n— the team"
	rec := env.putJSON(t, "/api/v1/staff/notification-templates/password_reset",
		env.admin, env.adminCSRF, map[string]any{
			"subject": subject, "body": body,
			"in_app_title": "Reset requested", "in_app_body": "A reset was requested.",
			"in_app_url": "/reset",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("save template = %d: %s", rec.Code, rec.Body)
	}

	if rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "reword@example.test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("reset request = %d: %s", rec.Code, rec.Body)
	}

	gotSubject, gotBody := deliveryFor(t, env.authEnv, "password_reset", "reword@example.test")
	if !strings.Contains(gotSubject, "password reset link") {
		t.Fatalf("the operator's subject was not used: %q", gotSubject)
	}
	if !strings.Contains(gotBody, "— the team") || !strings.Contains(gotBody, "works once") {
		t.Fatalf("the operator's body was not used:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "https://panel.example.test/reset?token=") {
		t.Fatalf("the reset link is missing or not absolute:\n%s", gotBody)
	}
	if strings.Contains(gotBody, "{{") {
		t.Fatalf("the message contains an unrendered placeholder:\n%s", gotBody)
	}

	// The security property is unchanged by the wording: the token works once.
	token := tokenFromDelivery(t, env.authEnv, "password_reset", "reword@example.test")
	if rec := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "a brand new password 2"}); rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body)
	}
	if rec := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "another password 3"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("the reworded mail's token was reusable: %d %s", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------- mail disabled --

// With mail switched off, the credential endpoints answer exactly as they did and nothing is
// sent. The answer must not depend on the switch: only an address that exists reaches the code
// that reads it, so a different response would be an account oracle.
func TestMailDisableStopsCredentialMailAndKeepsTheAnswer(t *testing.T) {
	env := newStaffEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "known@example.test", "a good password 1", "")
	setMailDisable(t, env.authEnv, "true")

	known := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "known@example.test"})
	unknown := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "nobody@example.test"})
	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 for both", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("mail.disable made the answers distinguishable:\n known:   %s\n unknown: %s",
			known.Body, unknown.Body)
	}

	// Nothing is deliverable, for either address.
	var deliverable int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'password_reset' AND status IN ('queued', 'failed')`).Scan(&deliverable); err != nil {
		t.Fatal(err)
	}
	if deliverable != 0 {
		t.Fatalf("%d reset message(s) are deliverable while mail is disabled", deliverable)
	}

	// The decision is recorded, so support can answer "why did no mail arrive".
	var suppressed int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'password_reset' AND status = 'suppressed'`).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if suppressed == 0 {
		t.Fatal("no suppressed row was recorded for the reset request")
	}

	// The in-app copy still lands: the panel is how a customer is told while the relay is off.
	var inboxed int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications n
		JOIN users u ON u.id = n.user_id
		WHERE u.email = 'known@example.test' AND n.event_key = 'password_reset'`).Scan(&inboxed); err != nil {
		t.Fatal(err)
	}
	if inboxed != 1 {
		t.Fatalf("inbox rows = %d, want 1", inboxed)
	}

	// The verification mail is suppressed the same way, and its request still answers 202.
	var userID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'known@example.test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := env.issueSessionFor(t, userID)
	if rec := env.postJSON(t, "/api/v1/auth/verify/request", cookie, csrf,
		map[string]any{}); rec.Code != http.StatusAccepted {
		t.Fatalf("verification request = %d: %s", rec.Code, rec.Body)
	}
	var verifiable int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'email_verification' AND status IN ('queued', 'failed')`).Scan(&verifiable); err != nil {
		t.Fatal(err)
	}
	if verifiable != 0 {
		t.Fatalf("%d verification message(s) are deliverable while mail is disabled", verifiable)
	}
}
