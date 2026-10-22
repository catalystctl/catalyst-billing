package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/portal"
)

// The ticket HTTP surface has four properties worth pinning, because each one is a
// promise the API makes to a frontend:
//
//   - another customer's ticket is 404, never 403: a 403 would confirm the identifier
//     exists, which turns the endpoint into an enumeration oracle;
//   - every failure is application/problem+json with a stable code, because the UI
//     branches on the code and not on the prose;
//   - a staff route without the ticket permission is refused, so the support queue is
//     not readable by infrastructure staff who happen to be signed in;
//   - an attachment download carries the type we stored, not the type the uploader
//     claimed, and never renders as a document on our origin.

// ticketEnv is an auth env with the portal wired, which the ticket handlers need to
// resolve a customer from the session.
func newTicketEnv(t *testing.T) *authEnv {
	t.Helper()
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	return env
}

// postTicket sends a JSON mutation with a CSRF token and an idempotency key.
func (e *authEnv) postTicket(t *testing.T, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return e.postJSON(t, path, cookie, csrf, body)
}

// patchTicket sends a JSON PATCH, which postJSON does not cover.
func (e *authEnv) patchTicket(t *testing.T, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	// A unique key per call: reusing one for a different body is correctly refused with
	// 422 idempotency_key_reuse, which would make this helper test the wrong thing.
	req.Header.Set("Idempotency-Key", "ticket-patch-"+t.Name()+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// uploadAttachment posts a raw file body to a ticket.
func (e *authEnv) uploadAttachment(t *testing.T, path string, cookie *http.Cookie, csrf,
	contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// problemOf decodes a problem document and fails when the body is not one.
func problemOf(t *testing.T, rec *httptest.ResponseRecorder) (code string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json; body: %s", ct, rec.Body)
	}
	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body is not JSON: %v (%s)", err, rec.Body)
	}
	if body.Code == "" || body.Title == "" || body.Type == "" {
		t.Fatalf("incomplete problem document: %s", rec.Body)
	}
	if body.Status != rec.Code {
		t.Fatalf("body status %d != HTTP status %d", body.Status, rec.Code)
	}
	return body.Code
}

// openTicket creates a ticket through the API and returns its id.
func (e *authEnv) openTicket(t *testing.T, cookie *http.Cookie, csrf, subject string) (ticketID, number string) {
	t.Helper()
	rec := e.postTicket(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{
		"subject":    subject,
		"department": "general",
		"priority":   "normal",
		"message":    map[string]any{"body": "please help"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ticket = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Ticket struct {
			ID     string `json:"id"`
			Number string `json:"number"`
		} `json:"ticket"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode thread: %v (%s)", err, rec.Body)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("a new ticket must carry its first message, got %d", len(body.Messages))
	}
	return body.Ticket.ID, body.Ticket.Number
}

// ------------------------------------------------------------------ boundary --

// Customer A cannot read, reply to or close customer B's ticket, and every refusal is
// 404 not_found — never 403, which would confirm the id exists.
func TestTicketCrossCustomerIsNotFound(t *testing.T) {
	env := newTicketEnv(t)

	env.seedCustomer(t, "alice@example.test", "Alice")
	env.seedCustomer(t, "bob@example.test", "Bob")
	aliceCookie, aliceCSRF := env.login(t, "alice@example.test", "customer password 1")
	bobCookie, bobCSRF := env.login(t, "bob@example.test", "customer password 1")

	bobTicket, _ := env.openTicket(t, bobCookie, bobCSRF, "bob's problem")

	// Alice's own list is empty: she cannot see Bob's ticket at all.
	rec := env.get(t, "/api/v1/portal/tickets", aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice list = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Tickets []map[string]any `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 0 {
		t.Fatalf("alice sees %d tickets, want 0", len(list.Tickets))
	}

	// Reading it is 404 with the problem envelope.
	rec = env.get(t, "/api/v1/portal/tickets/"+bobTicket, aliceCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer read = %d, want 404: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}

	// So is replying.
	rec = env.postTicket(t, "/api/v1/portal/tickets/"+bobTicket+"/replies", aliceCookie, aliceCSRF,
		map[string]any{"body": "injected"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer reply = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// So is closing.
	rec = env.postTicket(t, "/api/v1/portal/tickets/"+bobTicket+"/close", aliceCookie, aliceCSRF, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer close = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Bob's ticket is untouched and still open.
	rec = env.get(t, "/api/v1/portal/tickets/"+bobTicket, bobCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob's own ticket = %d: %s", rec.Code, rec.Body)
	}
	var thread struct {
		Ticket struct {
			Status       string `json:"status"`
			MessageCount int    `json:"message_count"`
		} `json:"ticket"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Ticket.Status != "open" || len(thread.Messages) != 1 {
		t.Fatalf("bob's thread = %+v, want one message and open", thread.Ticket)
	}
}

// A malformed ticket id is 404 too, so a probe cannot tell "not a uuid" from "not
// yours".
func TestTicketMalformedIDIsNotFound(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, _ := env.login(t, "alice@example.test", "customer password 1")

	rec := env.get(t, "/api/v1/portal/tickets/not-a-uuid", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
	rec = env.get(t, "/api/v1/portal/tickets/0f8fad5b-d9cb-469f-a165-70867728950e", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent id = %d, want 404: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// ------------------------------------------------------------- problem+json --

// Every error path uses the same envelope with a stable code.
func TestTicketErrorsAreProblemJSON(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")

	// An invalid body.
	rec := env.postTicket(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{"subject": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty subject = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}

	// An unknown department, which comes from settings validation.
	rec = env.postTicket(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{
		"subject": "hello", "department": "sales", "message": map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown department = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// A malformed JSON body.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/tickets", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "ticket-invalid-body")
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", rec.Code)
	}
	if code := problemOf(t, rec); code != "invalid_body" {
		t.Fatalf("code = %q, want invalid_body", code)
	}

	// A mutation without a CSRF token is refused with the envelope.
	rec = env.postTicket(t, "/api/v1/portal/tickets", cookie, "", map[string]any{
		"subject": "hello", "message": map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no CSRF = %d, want 403: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
}

// A customer who has never bought anything has no billing account, and asking for
// their tickets is a 404 rather than an empty page with a broken identity behind it.
func TestTicketWithoutCustomerRecordIsNotFound(t *testing.T) {
	env := newTicketEnv(t)
	env.createUser(t, "nolink@example.test", "a long enough password", "")
	cookie, _ := env.login(t, "nolink@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/tickets", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no customer record = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
}

// ---------------------------------------------------------------- the thread --

// The full customer journey over HTTP: create, reply, close, and the meta endpoint the
// form needs.
func TestTicketCustomerJourney(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")

	// The form's configuration.
	rec := env.get(t, "/api/v1/portal/tickets/meta", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta = %d: %s", rec.Code, rec.Body)
	}
	var meta struct {
		Enabled       bool     `json:"enabled"`
		Departments   []string `json:"departments"`
		Priorities    []string `json:"priorities"`
		MaxAttachment int      `json:"max_attachment_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.Enabled {
		t.Fatal("tickets must be enabled by default")
	}
	if len(meta.Departments) == 0 || len(meta.Priorities) != 3 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.MaxAttachment != 10<<20 {
		t.Fatalf("max attachment = %d, want 10485760", meta.MaxAttachment)
	}

	ticketID, number := env.openTicket(t, cookie, csrf, "my server is down")
	if !strings.HasPrefix(number, "TKT-") {
		t.Fatalf("number = %q, want a TKT- prefix", number)
	}

	// A reply.
	rec = env.postTicket(t, "/api/v1/portal/tickets/"+ticketID+"/replies", cookie, csrf,
		map[string]any{"body": "still down"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply = %d: %s", rec.Code, rec.Body)
	}
	var msg struct {
		ID        string `json:"id"`
		Body      string `json:"body"`
		FromStaff bool   `json:"from_staff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Body != "still down" || msg.FromStaff {
		t.Fatalf("reply = %+v", msg)
	}

	// The status filter finds it.
	rec = env.get(t, "/api/v1/portal/tickets?status=open", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list open = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Tickets []map[string]any `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 1 {
		t.Fatalf("open tickets = %d, want 1", len(list.Tickets))
	}
	// A status nobody has is an empty page, not an error.
	rec = env.get(t, "/api/v1/portal/tickets?status=closed", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list closed = %d: %s", rec.Code, rec.Body)
	}
	// An unknown status is a caller error.
	rec = env.get(t, "/api/v1/portal/tickets?status=bogus", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus status = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Close.
	rec = env.postTicket(t, "/api/v1/portal/tickets/"+ticketID+"/close", cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body)
	}
	var closed struct {
		Status   string `json:"status"`
		ClosedAt string `json:"closed_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &closed); err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" || closed.ClosedAt == "" {
		t.Fatalf("closed = %+v", closed)
	}
}

// A duplicate message_id over HTTP is a 200 with the stored message, not a second
// message and not an error.
func TestTicketDuplicateMessageIDOverHTTP(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")

	ticketID, _ := env.openTicket(t, cookie, csrf, "piped")
	body := map[string]any{"body": "imported", "message_id": "<one@mail.test>"}

	rec := env.postTicket(t, "/api/v1/portal/tickets/"+ticketID+"/replies", cookie, csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first delivery = %d: %s", rec.Code, rec.Body)
	}
	rec = env.postTicket(t, "/api/v1/portal/tickets/"+ticketID+"/replies", cookie, csrf, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate delivery = %d, want 200: %s", rec.Code, rec.Body)
	}
	var msg struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.Duplicate {
		t.Fatalf("duplicate flag = false: %s", rec.Body)
	}

	rec = env.get(t, "/api/v1/portal/tickets/"+ticketID, cookie)
	var thread struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(thread.Messages))
	}
}

// When the operator disables tickets, creation is refused with a stable code and the
// meta endpoint says so, so the UI can hide the form rather than fail on submit.
func TestTicketDisabledRefusesCreation(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")

	if _, err := env.pool.Exec(context.Background(),
		`UPDATE settings SET value = 'true' WHERE group_key = 'tickets' AND key = 'disabled'`); err != nil {
		t.Fatal(err)
	}

	rec := env.get(t, "/api/v1/portal/tickets/meta", cookie)
	var meta struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Enabled {
		t.Fatal("meta must report tickets as disabled")
	}

	rec = env.postTicket(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{
		"subject": "hello", "message": map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create while disabled = %d, want 403: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "tickets_disabled" {
		t.Fatalf("code = %q, want tickets_disabled", code)
	}
}

// Client closing can be reserved to staff, and then the close endpoint refuses.
func TestTicketClientClosingDisabled(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "closable")

	if _, err := env.pool.Exec(context.Background(),
		`UPDATE settings SET value = 'true' WHERE group_key = 'tickets' AND key = 'client_closing_disabled'`); err != nil {
		t.Fatal(err)
	}
	rec := env.postTicket(t, "/api/v1/portal/tickets/"+ticketID+"/close", cookie, csrf, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client close while disabled = %d, want 403: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "client_closing_disabled" {
		t.Fatalf("code = %q, want client_closing_disabled", code)
	}
}

// ------------------------------------------------------------------- staff --

// The support queue is readable and writable by support, and refused to a role that
// does not hold the ticket permissions.
func TestTicketStaffPermission(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "customer ticket")

	// Support sees the queue.
	rec := env.get(t, "/api/v1/staff/tickets", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("support queue = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Tickets []struct {
			ID       string `json:"id"`
			Customer struct {
				Email string `json:"email"`
			} `json:"customer"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 1 || list.Tickets[0].ID != ticketID {
		t.Fatalf("queue = %+v", list.Tickets)
	}
	if list.Tickets[0].Customer.Email != "alice@example.test" {
		t.Fatalf("customer email = %q", list.Tickets[0].Customer.Email)
	}

	// Infrastructure does not hold ticket:read:all, so it is refused.
	env.createUser(t, "infra@example.test", "infra password 1", "infrastructure")
	infra, _ := env.login(t, "infra@example.test", "infra password 1")
	rec = env.get(t, "/api/v1/staff/tickets", infra)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure queue = %d, want 403: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// And it cannot write either.
	rec = env.postTicket(t, "/api/v1/staff/tickets/"+ticketID+"/replies", infra, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure reply = %d, want 403: %s", rec.Code, rec.Body)
	}

	// A customer session cannot reach the staff surface at all.
	rec = env.get(t, "/api/v1/staff/tickets", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer on the staff queue = %d, want 403: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
}

// A staff reply moves the ticket to replied, and the queue's filters and mutations
// work over HTTP.
func TestTicketStaffQueueWorkflow(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "needs an operator")

	// Reply as staff.
	rec := env.postTicket(t, "/api/v1/staff/tickets/"+ticketID+"/replies",
		env.support, env.supportCSRF, map[string]any{"body": "on it"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff reply = %d: %s", rec.Code, rec.Body)
	}
	var msg struct {
		FromStaff bool `json:"from_staff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.FromStaff {
		t.Fatal("a staff reply must be marked from_staff")
	}

	// The ticket is now replied and shows in the filter.
	rec = env.get(t, "/api/v1/staff/tickets?status=replied", env.support)
	var list struct {
		Tickets []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 1 || list.Tickets[0].Status != "replied" {
		t.Fatalf("replied filter = %+v", list.Tickets)
	}

	// Change priority and department.
	rec = env.patchTicket(t, "/api/v1/staff/tickets/"+ticketID, env.support, env.supportCSRF,
		map[string]any{"priority": "high", "department": "billing"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body)
	}
	var updated struct {
		Priority   string `json:"priority"`
		Department string `json:"department"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Priority != "high" || updated.Department != "billing" {
		t.Fatalf("updated = %+v", updated)
	}

	// A patch with no fields is a caller error, not a no-op.
	rec = env.patchTicket(t, "/api/v1/staff/tickets/"+ticketID, env.support, env.supportCSRF, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Assign to the support agent. The id comes from the session the env logged in
	// with, which is the same identity the queue's assignee filter names.
	var supportID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'support@example.test'`).Scan(&supportID); err != nil {
		t.Fatal(err)
	}
	rec = env.postTicket(t, "/api/v1/staff/tickets/"+ticketID+"/assign",
		env.support, env.supportCSRF, map[string]any{"assignee": supportID})
	if rec.Code != http.StatusOK {
		t.Fatalf("assign = %d: %s", rec.Code, rec.Body)
	}
	var assigned struct {
		AssignedTo   string `json:"assigned_to"`
		AssigneeName string `json:"assignee_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &assigned); err != nil {
		t.Fatal(err)
	}
	if assigned.AssignedTo != supportID || assigned.AssigneeName == "" {
		t.Fatalf("assigned = %+v", assigned)
	}

	// Assigning to a customer login is refused: a ticket belongs to an operator.
	var aliceUser string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'alice@example.test'`).Scan(&aliceUser); err != nil {
		t.Fatal(err)
	}
	rec = env.postTicket(t, "/api/v1/staff/tickets/"+ticketID+"/assign",
		env.support, env.supportCSRF, map[string]any{"assignee": aliceUser})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assign to a customer = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Close as staff.
	rec = env.postTicket(t, "/api/v1/staff/tickets/"+ticketID+"/close",
		env.support, env.supportCSRF, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff close = %d: %s", rec.Code, rec.Body)
	}

	// The queue's configuration carries the departments and the auto-close threshold.
	rec = env.get(t, "/api/v1/staff/tickets/meta", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff meta = %d: %s", rec.Code, rec.Body)
	}
	var meta struct {
		Departments   []string `json:"departments"`
		Priorities    []string `json:"priorities"`
		AutoCloseDays int      `json:"autoclose_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Departments) == 0 || len(meta.Priorities) != 3 || meta.AutoCloseDays != 14 {
		t.Fatalf("staff meta = %+v", meta)
	}

	// The queue's "assigned to me" shortcut resolves from the session.
	rec = env.get(t, "/api/v1/staff/tickets?assignee=me", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("assignee=me = %d: %s", rec.Code, rec.Body)
	}
	var mine struct {
		Tickets []struct {
			ID string `json:"id"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Tickets) != 1 || mine.Tickets[0].ID != ticketID {
		t.Fatalf("assignee=me = %+v, want the assigned ticket", mine.Tickets)
	}

	// An unknown ticket is 404 with the envelope.
	rec = env.get(t, "/api/v1/staff/tickets/0f8fad5b-d9cb-469f-a165-70867728950e", env.support)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown ticket = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
}

// ------------------------------------------------------------------ uploads --

// An 11 MB upload is refused with 413 and the stable code, and nothing is stored. A
// 9 MB upload is accepted.
func TestTicketAttachmentSizeCap(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "with files")

	path := "/api/v1/portal/tickets/" + ticketID + "/attachments?filename=big.bin"

	rec := env.uploadAttachment(t, path, cookie, csrf, "application/octet-stream",
		make([]byte, 11<<20))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("11 MB upload = %d, want 413: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "attachment_too_large" {
		t.Fatalf("code = %q, want attachment_too_large", code)
	}

	// Nothing was stored.
	rec = env.get(t, "/api/v1/portal/tickets/"+ticketID, cookie)
	var thread struct {
		Messages []struct {
			Attachments []map[string]any `json:"attachments"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	for _, m := range thread.Messages {
		if len(m.Attachments) != 0 {
			t.Fatalf("a refused upload was stored: %+v", m.Attachments)
		}
	}

	// A 9 MB file is accepted.
	rec = env.uploadAttachment(t, "/api/v1/portal/tickets/"+ticketID+"/attachments?filename=nine.bin",
		cookie, csrf, "application/octet-stream", make([]byte, 9<<20))
	if rec.Code != http.StatusCreated {
		t.Fatalf("9 MB upload = %d, want 201: %s", rec.Code, rec.Body)
	}
}

// The download is served with the stored (normalised) type, nosniff and an attachment
// disposition, so an uploaded page can never run on our origin.
//
// Since the allowlist landed, "never run on our origin" is enforced twice: a type a browser
// renders (text/html, image/svg+xml, JavaScript, XML) is refused outright with 415, and every
// download is served as an attachment with nosniff and a sandboxing CSP whatever its type is.
// This test pins both halves, because the second is what protects an *accepted* type that a
// browser would still treat as a document if the headers were missing.
func TestTicketAttachmentContentType(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	env.seedCustomer(t, "bob@example.test", "Bob")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	bobCookie, _ := env.login(t, "bob@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "screenshot")

	// A type a browser would render is refused, not stored. That is the allowlist.
	rec := env.uploadAttachment(t,
		"/api/v1/portal/tickets/"+ticketID+"/attachments?filename=evil.html",
		cookie, csrf, "text/html", []byte("<script>alert(1)</script>"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html upload = %d, want 415: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "attachment_type_not_allowed" {
		t.Fatalf("code = %q, want attachment_type_not_allowed", code)
	}

	// An accepted type is stored as itself — the *claimed* type, not the extension: a text file
	// named .html is a text file, and the download below proves the browser is told so.
	rec = env.uploadAttachment(t,
		"/api/v1/portal/tickets/"+ticketID+"/attachments?filename=evil.html",
		cookie, csrf, "text/plain", []byte("<script>alert(1)</script>"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	var stored struct {
		ID          string `json:"id"`
		ContentType string `json:"content_type"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ContentType != "text/plain" {
		t.Fatalf("stored content type = %q, want text/plain", stored.ContentType)
	}
	if stored.URL == "" {
		t.Fatal("the attachment body must carry its download URL")
	}

	// The owner downloads it.
	rec = env.get(t, stored.URL, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d: %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	if got := h.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want attachment", got)
	}
	if !strings.Contains(h.Get("Content-Disposition"), "evil.html") {
		t.Fatalf("Content-Disposition = %q, want the filename", h.Get("Content-Disposition"))
	}
	if got := h.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Fatalf("Content-Security-Policy = %q, want a sandbox", got)
	}
	if !strings.Contains(h.Get("Cache-Control"), "private") {
		t.Fatalf("Cache-Control = %q, want private", h.Get("Cache-Control"))
	}
	if rec.Body.String() != "<script>alert(1)</script>" {
		t.Fatalf("bytes = %q", rec.Body.String())
	}

	// Another customer cannot download it: 404, not 403.
	rec = env.get(t, stored.URL, bobCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer download = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// A log file is preserved as itself.
	rec = env.uploadAttachment(t, "/api/v1/portal/tickets/"+ticketID+"/attachments?filename=log.txt",
		cookie, csrf, "text/plain", []byte("line one"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("text upload = %d: %s", rec.Code, rec.Body)
	}
	var text struct {
		ID          string `json:"id"`
		ContentType string `json:"content_type"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &text); err != nil {
		t.Fatal(err)
	}
	if text.ContentType != "text/plain" {
		t.Fatalf("stored content type = %q, want text/plain", text.ContentType)
	}
	rec = env.get(t, text.URL, cookie)
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
}

// An upload without a filename is refused before the body is considered, and a
// customer cannot attach a file to another customer's ticket.
func TestTicketAttachmentValidation(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	env.seedCustomer(t, "bob@example.test", "Bob")
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	bobCookie, bobCSRF := env.login(t, "bob@example.test", "customer password 1")

	aliceTicket, _ := env.openTicket(t, cookie, csrf, "alice's")
	bobTicket, _ := env.openTicket(t, bobCookie, bobCSRF, "bob's")

	// No filename.
	rec := env.uploadAttachment(t, "/api/v1/portal/tickets/"+aliceTicket+"/attachments",
		cookie, csrf, "text/plain", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no filename = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// An empty body.
	rec = env.uploadAttachment(t, "/api/v1/portal/tickets/"+aliceTicket+"/attachments?filename=a.txt",
		cookie, csrf, "text/plain", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty upload = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Someone else's ticket.
	rec = env.uploadAttachment(t, "/api/v1/portal/tickets/"+bobTicket+"/attachments?filename=a.txt",
		cookie, csrf, "text/plain", []byte("x"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer upload = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Staff can attach to any ticket, including one that is not theirs.
	staff := newStaffEnv(t)
	staff.srv.WithPortal(portal.New(staff.pool))
	staff.seedCustomer(t, "carol@example.test", "Carol")
	carolCookie, carolCSRF := staff.login(t, "carol@example.test", "customer password 1")
	carolTicket, _ := staff.openTicket(t, carolCookie, carolCSRF, "carol's")
	rec = staff.uploadAttachment(t, "/api/v1/staff/tickets/"+carolTicket+"/attachments?filename=note.txt",
		staff.support, staff.supportCSRF, "text/plain", []byte("a staff note"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff upload = %d: %s", rec.Code, rec.Body)
	}
	// And the customer sees it on the thread.
	rec = staff.get(t, "/api/v1/portal/tickets/"+carolTicket, carolCookie)
	var thread struct {
		Messages []struct {
			Attachments []map[string]any `json:"attachments"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 1 || len(thread.Messages[0].Attachments) != 1 {
		t.Fatalf("thread after a staff attachment = %+v", thread.Messages)
	}
}
