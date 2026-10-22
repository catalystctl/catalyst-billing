package httpapi_test

// The notification API: the customer inbox and preferences, and the staff template editor.
//
// The tests assert the properties that make the surface safe rather than the happy path:
// another user's notification is unreachable and indistinguishable from a missing one, a
// force event cannot be turned off through the API, a staff route refuses a caller without
// notification:write, and every failure is problem+json with a stable code.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

// seedNotification inserts an inbox row owned by userID and returns its id.
func (e *authEnv) seedNotification(t *testing.T, userID, eventKey, title string) string {
	t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(), `
		INSERT INTO notifications (user_id, event_key, title, body, url)
		VALUES ($1, $2, $3, 'body', '/invoices/x') RETURNING id`,
		userID, eventKey, title).Scan(&id)
	if err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

// seedTemplates inserts the shipped defaults, which is what a running installation does.
func (e *authEnv) seedTemplates(t *testing.T) {
	t.Helper()
	if _, err := notify.SeedTemplates(context.Background(), e.pool); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
}

// decodeProblem reads the problem document from a response.
type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json (body %s)", ct, rec.Body)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body is not JSON: %v (%s)", err, rec.Body)
	}
	if p.Code == "" {
		t.Fatalf("problem body has no code: %s", rec.Body)
	}
	if p.Status != rec.Code {
		t.Fatalf("body status %d != HTTP status %d", p.Status, rec.Code)
	}
	return p
}

// ---------------------------------------------------------------- the inbox --

// A user sees their own notifications and not another user's.
func TestPortalNotificationsAreScopedToTheCaller(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.createUser(t, "alice@example.test", "alice password 1", "")
	bob := env.createUser(t, "bob@example.test", "bob password 1", "")

	env.seedNotification(t, alice, notify.EventInvoicePaid, "alice paid")
	bobID := env.seedNotification(t, bob, notify.EventInvoicePaid, "bob paid")

	aliceCookie, aliceCSRF := env.login(t, "alice@example.test", "alice password 1")
	rec := env.get(t, "/api/v1/portal/notifications", aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body)
	}
	if len(page.Data) != 1 || page.Data[0].Title != "alice paid" {
		t.Fatalf("alice's inbox = %+v, want only her own row", page.Data)
	}

	// Bob's notification id is not merely forbidden to Alice: it answers exactly like an id
	// that never existed, so the endpoint cannot be used to discover which ids are real.
	rec = env.do(t, http.MethodPost, "/api/v1/portal/notifications/"+bobID+"/read",
		aliceCookie, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("marking another user's notification read: status %d, want 404", rec.Code)
	}
	if p := decodeProblem(t, rec); p.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", p.Code)
	}

	// And Bob's row is still unread.
	var unread int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, bob).
		Scan(&unread); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unread != 1 {
		t.Fatalf("bob has %d unread, want 1", unread)
	}
}

// Marking read and marking all read move the unread count, and the count endpoint agrees with
// the list.
func TestPortalNotificationMarkRead(t *testing.T) {
	env := newAuthEnv(t)
	userID := env.createUser(t, "reader@example.test", "reader password 1", "")
	first := env.seedNotification(t, userID, notify.EventInvoicePaid, "one")
	env.seedNotification(t, userID, notify.EventInvoicePaid, "two")

	cookie, csrf := env.login(t, "reader@example.test", "reader password 1")

	rec := env.get(t, "/api/v1/portal/notifications/unread-count", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("count: status %d, body %s", rec.Code, rec.Body)
	}
	var count struct {
		Unread int `json:"unread"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("count body: %v", err)
	}
	if count.Unread != 2 {
		t.Fatalf("unread = %d, want 2", count.Unread)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/portal/notifications/"+first+"/read", cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark read: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("mark-read body: %v", err)
	}
	if count.Unread != 1 {
		t.Fatalf("unread after one mark = %d, want 1", count.Unread)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/portal/notifications/read-all", cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark all: status %d, body %s", rec.Code, rec.Body)
	}
	rec = env.get(t, "/api/v1/portal/notifications/unread-count", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("count body: %v", err)
	}
	if count.Unread != 0 {
		t.Fatalf("unread after mark-all = %d, want 0", count.Unread)
	}
}

// A malformed notification id is 404, not a 500: the shape is checked before it reaches SQL.
func TestPortalNotificationRejectsMalformedID(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "shape@example.test", "shape password 1", "")
	cookie, csrf := env.login(t, "shape@example.test", "shape password 1")

	rec := env.do(t, http.MethodPost, "/api/v1/portal/notifications/not-a-uuid/read", cookie, csrf)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body %s", rec.Code, rec.Body)
	}
	decodeProblem(t, rec)
}

// An anonymous caller is refused before the handler runs, with the standard envelope.
func TestPortalNotificationsRequireAuthentication(t *testing.T) {
	env := newAuthEnv(t)
	for _, path := range []string{
		"/api/v1/portal/notifications",
		"/api/v1/portal/notifications/unread-count",
		"/api/v1/portal/notifications/preferences",
	} {
		rec := env.get(t, path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", path, rec.Code)
		}
		decodeProblem(t, rec)
	}
}

// ------------------------------------------------------------ preferences --

// The preferences endpoint lists the whole catalogue and honours a choice_off.
func TestPortalPreferencesRoundTrip(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "prefs@example.test", "prefs password 1", "")
	cookie, csrf := env.login(t, "prefs@example.test", "prefs password 1")

	rec := env.get(t, "/api/v1/portal/notifications/preferences", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			EventKey   string `json:"event_key"`
			Force      bool   `json:"force"`
			Preference string `json:"preference"`
			Mail       bool   `json:"mail"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("get body: %v (%s)", err, rec.Body)
	}
	if len(body.Data) != len(notify.Catalogue()) {
		t.Fatalf("got %d preference rows, want %d", len(body.Data), len(notify.Catalogue()))
	}

	// Turn one choice event off.
	payload := `{"preferences":[{"event_key":"invoice_paid","preference":"choice_off"}]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/portal/notifications/preferences",
		strings.NewReader(payload))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "application/json")
	rec = env.doRequest(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("put body: %v (%s)", err, rec.Body)
	}
	var found bool
	for _, row := range body.Data {
		if row.EventKey != notify.EventInvoicePaid {
			continue
		}
		found = true
		if row.Preference != "choice_off" || row.Mail {
			t.Fatalf("invoice_paid row = %+v, want choice_off with mail off", row)
		}
	}
	if !found {
		t.Fatal("invoice_paid is missing from the response")
	}

	// A force event cannot be turned off through the API either.
	payload = `{"preferences":[{"event_key":"password_reset","preference":"choice_off"}]}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/portal/notifications/preferences",
		strings.NewReader(payload))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "application/json")
	rec = env.doRequest(t, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("opting out of a force event: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "force_event" {
		t.Fatalf("code = %q, want force_event", p.Code)
	}
}

// An unknown event key is refused with a code, not silently stored.
func TestPortalPreferencesRejectUnknownEvent(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "unknown@example.test", "unknown password 1", "")
	cookie, csrf := env.login(t, "unknown@example.test", "unknown password 1")

	payload := `{"preferences":[{"event_key":"no_such_event","preference":"choice_off"}]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/portal/notifications/preferences",
		strings.NewReader(payload))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "application/json")
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "unknown_event" {
		t.Fatalf("code = %q, want unknown_event", p.Code)
	}
}

// ----------------------------------------------------------- staff templates --

// A caller without notification:write is refused, and the refusal says nothing about which
// permission was missing.
func TestStaffTemplateRoutesRequirePermission(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)

	// A customer holds no staff permission at all.
	env.createUser(t, "customer@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "customer@example.test", "customer password 1")
	rec := env.get(t, "/api/v1/staff/notification-templates", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer reading templates: status %d, want 403", rec.Code)
	}
	// A customer is refused by the staff guard, which runs outermost: the code says "this area
	// is for staff" and reveals nothing about the permission behind it.
	if p := decodeProblem(t, rec); p.Code != "staff_required" && p.Code != "forbidden" {
		t.Fatalf("code = %q, want staff_required or forbidden", p.Code)
	}

	// Support holds log:read and ticket permissions but deliberately not notification:write:
	// support answers "why did I get this", it does not decide what the mail says.
	env.createUser(t, "support@example.test", "support password 1", "support")
	cookie, csrf := env.login(t, "support@example.test", "support password 1")
	rec = env.get(t, "/api/v1/staff/notification-templates", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support reading templates: status %d, want 403", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/staff/notification-templates/invoice_paid",
		strings.NewReader(`{"subject":"x","body":"y"}`))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "template-write-0001")
	req.Header.Set("Content-Type", "application/json")
	rec = env.doRequest(t, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support writing a template: status %d, want 403", rec.Code)
	}
	decodeProblem(t, rec)

	// Billing holds notification:write, so it may read.
	env.createUser(t, "billing@example.test", "billing password 1", "billing")
	cookie, _ = env.login(t, "billing@example.test", "billing password 1")
	rec = env.get(t, "/api/v1/staff/notification-templates", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("billing reading templates: status %d, want 200; body %s", rec.Code, rec.Body)
	}
}

// An operator edits a template and the change is what the next message renders.
func TestStaffTemplateEditChangesRenderedMail(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	cookie, csrf := env.login(t, "admin@example.test", "admin password 1")

	payload := `{"name":"Invoice paid","subject":"Thanks for invoice {{invoice_number}}",` +
		`"body":"We received {{total}}.","in_app_title":"Paid","in_app_body":"{{total}}",` +
		`"in_app_url":"/invoices","enabled":true,"mail_enabled":true,"in_app_enabled":true,` +
		`"cc_addresses":"","bcc_addresses":""}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/staff/notification-templates/invoice_paid", strings.NewReader(payload))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "template-edit-0001")
	req.Header.Set("Content-Type", "application/json")
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: status %d, body %s", rec.Code, rec.Body)
	}

	var saved struct {
		Subject string `json:"subject"`
		Force   bool   `json:"force"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("put body: %v", err)
	}
	if saved.Subject != "Thanks for invoice {{invoice_number}}" {
		t.Fatalf("subject = %q", saved.Subject)
	}
	if saved.Force {
		t.Fatal("invoice_paid was reported as a force event")
	}

	// The stored template is what the event function renders.
	got, err := notify.TemplateFor(context.Background(), env.pool, notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Subject != "Thanks for invoice {{invoice_number}}" || got.Body != "We received {{total}}." {
		t.Fatalf("stored template = %+v", got)
	}

	// An edit is audited: changing what the business tells its customers must leave a trace.
	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = 'notification.template.update'
		  AND changes->>'event_key' = 'invoice_paid'`).
		Scan(&audited); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audited != 1 {
		t.Fatalf("audit rows = %d, want 1", audited)
	}
}

// Resetting a template restores the shipped wording.
func TestStaffTemplateResetRestoresDefault(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	cookie, csrf := env.login(t, "admin@example.test", "admin password 1")

	// Change it first.
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE notification_templates SET subject = 'mangled' WHERE event_key = $1`,
		notify.EventInvoicePaid); err != nil {
		t.Fatalf("mangle: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/staff/notification-templates/invoice_paid/reset", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "template-reset-0001")
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d, body %s", rec.Code, rec.Body)
	}
	got, err := notify.TemplateFor(context.Background(), env.pool, notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Subject == "mangled" {
		t.Fatal("reset did not restore the default")
	}
	var want string
	for _, d := range notify.DefaultTemplates() {
		if d.EventKey == notify.EventInvoicePaid {
			want = d.Subject
		}
	}
	if got.Subject != want {
		t.Fatalf("subject = %q, want %q", got.Subject, want)
	}
}

// An unknown event key is 404, not a template invented on the spot.
func TestStaffTemplateUnknownEventIs404(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	cookie, _ := env.login(t, "admin@example.test", "admin password 1")

	rec := env.get(t, "/api/v1/staff/notification-templates/no_such_event", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", p.Code)
	}
}

// A staff mutation without an Idempotency-Key is refused before the handler runs: a
// double-submitted template edit is the same failure a double-submitted refund is.
func TestStaffTemplateMutationRequiresIdempotencyKey(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	cookie, csrf := env.login(t, "admin@example.test", "admin password 1")

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/staff/notification-templates/invoice_paid",
		strings.NewReader(`{"subject":"x","body":"y"}`))
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "application/json")
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "idempotency_key_required" {
		t.Fatalf("code = %q, want idempotency_key_required", p.Code)
	}
}

// A mutation without a CSRF token is refused, so a cross-site form cannot edit a template.
func TestStaffTemplateMutationRequiresCSRF(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)
	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	cookie, _ := env.login(t, "admin@example.test", "admin password 1")

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/staff/notification-templates/invoice_paid",
		strings.NewReader(`{"subject":"x","body":"y"}`))
	req.AddCookie(cookie)
	req.Header.Set("Idempotency-Key", "template-csrf-0001")
	req.Header.Set("Content-Type", "application/json")
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "csrf_failed" {
		t.Fatalf("code = %q, want csrf_failed", p.Code)
	}
}

// The inbox a customer reads is the one the event functions write, so the two halves of the
// feature agree about a real payment.
func TestInboxShowsWhatAnEventWrote(t *testing.T) {
	env := newAuthEnv(t)
	env.seedTemplates(t)
	userID := env.createUser(t, "event@example.test", "event password 1", "")
	queue := notify.NewQueue(env.pool)

	err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.InvoicePaid(ctx, tx, queue, notify.InvoicePaidInput{
			UserID: userID, Email: "event@example.test",
			InvoiceID: "11111111-2222-3333-4444-555555555555",
			Number:    "INV-000123", Total: "USD 12.00",
			InvoiceURL: "/invoices/11111111-2222-3333-4444-555555555555",
		})
		return err
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}

	cookie, _ := env.login(t, "event@example.test", "event password 1")
	rec := env.get(t, "/api/v1/portal/notifications", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			EventKey string `json:"event_key"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			URL      string `json:"url"`
			Unread   bool   `json:"unread"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body)
	}
	if len(page.Data) != 1 {
		t.Fatalf("inbox has %d rows, want 1", len(page.Data))
	}
	row := page.Data[0]
	if row.EventKey != notify.EventInvoicePaid {
		t.Fatalf("event_key = %q", row.EventKey)
	}
	if !row.Unread {
		t.Fatal("a fresh notification is not unread")
	}
	if !strings.Contains(row.Body, "INV-000123") {
		t.Fatalf("body was not rendered: %q", row.Body)
	}
	if row.Title == "" {
		t.Fatal("the in-app title is empty")
	}
	// The inbox URL stays panel-relative: an absolute one would break behind a proxy.
	if !strings.HasPrefix(row.URL, "/invoices/") {
		t.Fatalf("url = %q, want a panel-relative path", row.URL)
	}
}
