package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The piping webhook is the one public endpoint that writes into a support thread, so its
// tests are about the door rather than the handler: it must be shut when the installation has
// no secret, shut to a caller who does not present the right one, and open to a provider that
// does — and a message that arrives through it must land in the right ticket.

// setPipingSecret writes mail.piping_secret for the duration of one test.
//
// The settings table is reference data the harness does not truncate, so the row is removed
// afterwards: a secret left behind would change what every later test in this package sees.
func (e *authEnv) setPipingSecret(t *testing.T, secret string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('mail', 'piping_secret', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, secret); err != nil {
		t.Fatalf("set piping secret: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM settings WHERE group_key = 'mail' AND key = 'piping_secret'`)
	})
}

// postPiping sends a webhook body with the given secret.
func (e *authEnv) postPiping(t *testing.T, secret string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/mail/piping", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Piping-Secret", secret)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// pipingMessage is a threaded reply from Alice.
func pipingMessage(messageID, inReplyTo string) map[string]any {
	return map[string]any{
		"message_id":  messageID,
		"from":        "alice@example.test",
		"to":          []string{"support@panel.test"},
		"subject":     "Re: please help",
		"in_reply_to": inReplyTo,
		"body":        "here is the extra detail",
	}
}

// An installation with no piping secret refuses every request, including one that presents
// the empty string. This is the "fail closed" test: an unauthenticated ingest endpoint is an
// open door.
func TestPipingWebhookRefusesWhenNoSecretIsConfigured(t *testing.T) {
	env := newTicketEnv(t)

	// No row at all: the setting's default is empty.
	rec := env.postPiping(t, "", pipingMessage("<n1@example.test>", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret = %d, want 503: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "piping_not_configured" {
		t.Fatalf("code = %q, want piping_not_configured", code)
	}

	// An explicit empty value is the same refusal, not an accidental pass.
	env.setPipingSecret(t, "")
	rec = env.postPiping(t, "", pipingMessage("<n2@example.test>", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty secret = %d, want 503: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "piping_not_configured" {
		t.Fatalf("code = %q, want piping_not_configured", code)
	}

	// Nothing was stored: a refused request must not become a mail log row either.
	var rows int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ticket_mail_logs WHERE direction = 'inbound'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("inbound rows = %d, want 0", rows)
	}
}

// A wrong or missing secret is 401 with the problem envelope, and writes nothing.
func TestPipingWebhookRefusesAWrongSecret(t *testing.T) {
	env := newTicketEnv(t)
	env.setPipingSecret(t, "s3cret-piping-value")

	// A secret is case-sensitive and compared whole: a prefix, a longer value and a
	// different case are all refusals.
	for _, presented := range []string{"", "wrong", "s3cret-piping-valu", "s3cret-piping-values", "S3cret-piping-value"} {
		rec := env.postPiping(t, presented, pipingMessage("<w-"+presented+"@example.test>", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("secret %q = %d, want 401: %s", presented, rec.Code, rec.Body)
		}
		if code := problemOf(t, rec); code != "unauthorized" {
			t.Fatalf("secret %q code = %q, want unauthorized", presented, code)
		}
	}
}

// A correctly authenticated, threaded reply lands in the right ticket.
func TestPipingWebhookIngestsAThreadedReply(t *testing.T) {
	env := newTicketEnv(t)
	env.setPipingSecret(t, "s3cret-piping-value")

	env.seedCustomer(t, "alice@example.test", "Alice")
	aliceCookie, aliceCSRF := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, aliceCookie, aliceCSRF, "please help")

	// A staff reply is what the customer's reply threads against.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO ticket_messages (ticket_id, from_staff, body, message_id)
		VALUES ($1, true, 'what is wrong?', '<staff-1@panel.test>')`, ticketID); err != nil {
		t.Fatalf("staff message: %v", err)
	}

	rec := env.postPiping(t, "s3cret-piping-value", pipingMessage("<c1@example.test>", "<staff-1@panel.test>"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d, want 201: %s", rec.Code, rec.Body)
	}
	var body struct {
		Outcome      string `json:"outcome"`
		TicketID     string `json:"ticket_id"`
		AttributedBy string `json:"attributed_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Outcome != "attached" || body.TicketID != ticketID || body.AttributedBy != "thread" {
		t.Fatalf("body = %+v, want attached to %s by thread", body, ticketID)
	}

	// The reply is in the thread, as a customer message.
	rec = env.get(t, "/api/v1/portal/tickets/"+ticketID, aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("thread = %d: %s", rec.Code, rec.Body)
	}
	var thread struct {
		Messages []struct {
			Body      string `json:"body"`
			FromStaff bool   `json:"from_staff"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range thread.Messages {
		if m.Body == "here is the extra detail" && !m.FromStaff {
			found = true
		}
	}
	if !found {
		t.Fatalf("the piped reply is not in the thread: %+v", thread.Messages)
	}

	// A redelivery is 200 with outcome=duplicate and adds nothing.
	rec = env.postPiping(t, "s3cret-piping-value", pipingMessage("<c1@example.test>", "<staff-1@panel.test>"))
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery = %d, want 200: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Outcome != "duplicate" {
		t.Fatalf("redelivery outcome = %q, want duplicate", body.Outcome)
	}
}

// A refused message answers 202 with its reason, and is visible to staff.
func TestPipingRefusalIsAcceptedAndSurfaced(t *testing.T) {
	env := newTicketEnv(t)
	env.setPipingSecret(t, "s3cret-piping-value")

	rec := env.postPiping(t, "s3cret-piping-value", map[string]any{
		"message_id": "<stranger-1@example.test>",
		"from":       "stranger@elsewhere.test",
		"subject":    "hello",
		"body":       "who are you",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("refusal = %d, want 202: %s", rec.Code, rec.Body)
	}
	var body struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Outcome != "unattributed" || body.Reason != "no_customer" {
		t.Fatalf("body = %+v, want unattributed/no_customer", body)
	}

	// A support agent can see it, with the reason, so nothing is silently dropped.
	env.createUser(t, "agent@example.test", "agent password 1", "support")
	agentCookie, _ := env.login(t, "agent@example.test", "agent password 1")
	rec = env.get(t, "/api/v1/staff/tickets/piping?status=ignored", agentCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff view = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Messages []struct {
			Reason string `json:"reason"`
			From   string `json:"from"`
			Body   string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Messages) != 1 {
		t.Fatalf("messages = %+v, want one refusal", list.Messages)
	}
	if list.Messages[0].Reason != "no_customer" || list.Messages[0].From != "stranger@elsewhere.test" {
		t.Fatalf("row = %+v", list.Messages[0])
	}
	if list.Messages[0].Body != "who are you" {
		t.Fatalf("body = %q, want the stored text so an agent can answer by hand", list.Messages[0].Body)
	}
}

// The staff view requires the ticket permission: a customer cannot read the mailbox log.
func TestPipingMailRequiresStaff(t *testing.T) {
	env := newTicketEnv(t)
	env.seedCustomer(t, "alice@example.test", "Alice")
	cookie, _ := env.login(t, "alice@example.test", "customer password 1")

	rec := env.get(t, "/api/v1/staff/tickets/piping", cookie)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("customer reading the piping log = %d, want a refusal", rec.Code)
	}
}
