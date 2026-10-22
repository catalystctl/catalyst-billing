package tickets_test

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/tickets"
)

// Email piping's tests are the refusals, because the happy path is the easy part.
//
//   - a threaded reply attaches to the ticket the headers name;
//   - the same Message-ID delivered twice creates one reply;
//   - an auto-reply and a message from the panel's own address are dropped;
//   - a stranger's reply to a ticket is refused rather than appended (the one that
//     matters: appending it is how an attacker reads a support thread);
//   - an address that names two customers is a refusal, not a coin flip;
//   - a reply to a closed ticket reopens it.

// setMailSetting writes one mail setting and removes it when the test ends.
//
// The settings table is reference data that the test harness does not truncate, so a test
// that leaves a piping address behind would change the meaning of every later test in this
// package.
func setMailSetting(t *testing.T, pool *store.Pool, key, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('mail', $1, $2)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set mail.%s: %v", key, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM settings WHERE group_key = 'mail' AND key = $1`, key)
	})
}

// messageCount counts the stored messages of a ticket.
func (f *fixture) messageCount(t *testing.T, ticketID string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ticket_messages WHERE ticket_id = $1`, ticketID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return count
}

// addStaffReply appends a staff message with an email Message-ID, which is what a customer's
// reply will thread against.
func (f *fixture) addStaffReply(t *testing.T, ticketID, messageID, body string) {
	t.Helper()
	if _, _, err := f.svc.ReplyAsStaff(context.Background(), f.aliceUser, ticketID,
		tickets.ReplyInput{Body: body, MessageID: messageID}); err != nil {
		t.Fatalf("staff reply: %v", err)
	}
}

// lastMailLog reads the newest inbound mail log row.
type mailLogRow struct {
	status       string
	reason       string
	ticketID     string
	customerID   string
	attributedBy string
}

func (f *fixture) lastMailLog(t *testing.T) mailLogRow {
	t.Helper()
	var row mailLogRow
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status, reason, COALESCE(ticket_id::text, ''), COALESCE(customer_id::text, ''),
		       COALESCE(attributed_by, '')
		FROM ticket_mail_logs
		WHERE direction = 'inbound'
		ORDER BY created_at DESC, id DESC
		LIMIT 1`).
		Scan(&row.status, &row.reason, &row.ticketID, &row.customerID, &row.attributedBy); err != nil {
		t.Fatalf("read mail log: %v", err)
	}
	return row
}

// A reply that threads against a stored message id attaches to that ticket.
func TestPipedReplyAttachesToTheThread(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "printer on fire")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-1@panel.test>", "have you tried water")
	// The outbox templates are seeded at boot in the real binary; a test database is migrated
	// but never booted, so seed them here.
	if _, err := notify.SeedTemplates(ctx, f.pool); err != nil {
		t.Fatalf("seed notification templates: %v", err)
	}
	// An assigned ticket, so the notification path has a recipient.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE tickets SET assigned_to = $2 WHERE id = $1`, thread.Ticket.ID, f.aliceUser); err != nil {
		t.Fatalf("assign ticket: %v", err)
	}

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<customer-1@example.test>",
		From:      "Alice <alice@example.test>",
		To:        []string{"support@panel.test"},
		Subject:   "Re: [TKT-000001] printer on fire",
		InReplyTo: "<staff-1@panel.test>",
		Body:      "yes, now it is worse",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestAttached {
		t.Fatalf("outcome = %q (%s), want attached", result.Outcome, result.Reason)
	}
	if result.TicketID != thread.Ticket.ID {
		t.Fatalf("ticket = %q, want %q", result.TicketID, thread.Ticket.ID)
	}
	if result.AttributedBy != tickets.AttributedByThread {
		t.Fatalf("attributed_by = %q, want thread", result.AttributedBy)
	}

	got, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(got.Messages))
	}
	// The messages share one clock in the test, so the order is the id's; find the piped
	// message rather than assuming it is last.
	var piped *tickets.Message
	for i, m := range got.Messages {
		if strings.Contains(m.Body, "now it is worse") {
			piped = &got.Messages[i]
		}
	}
	if piped == nil {
		t.Fatalf("the piped reply is not in the thread: %+v", got.Messages)
	}
	if piped.FromStaff {
		t.Fatal("a piped customer reply was stored as staff")
	}

	log := f.lastMailLog(t)
	if log.status != "received" || log.ticketID != thread.Ticket.ID || log.attributedBy != "thread" {
		t.Fatalf("log = %+v, want a received row on the ticket attributed by thread", log)
	}

	// The reply is audited, as a system action: an inbound message has no authenticated user.
	var audits int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE action = 'ticket.piped_reply' AND target_id = $1`, thread.Ticket.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit rows = %d, want 1", audits)
	}

	// And the assignee is notified through the outbox, deduped on the Message-ID so a
	// redelivery cannot notify twice.
	var notified int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key = 'new_ticket_message:customer-1@example.test'`).Scan(&notified); err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("notification rows = %d, want 1", notified)
	}
}

// The same Message-ID delivered twice creates one reply, not two.
func TestDuplicateMessageIDCreatesOneReply(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "duplicate delivery")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-2@panel.test>", "answer")

	msg := tickets.InboundMessage{
		MessageID: "<customer-2@example.test>",
		From:      "alice@example.test",
		To:        []string{"support@panel.test"},
		Subject:   "Re: duplicate delivery",
		InReplyTo: "<staff-2@panel.test>",
		Body:      "one reply, please",
		Source:    "imap",
	}
	first, err := f.svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.Outcome != tickets.IngestAttached {
		t.Fatalf("first outcome = %q (%s)", first.Outcome, first.Reason)
	}
	before := f.messageCount(t, thread.Ticket.ID)

	second, err := f.svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Outcome != tickets.IngestDuplicate {
		t.Fatalf("second outcome = %q (%s), want duplicate", second.Outcome, second.Reason)
	}
	if after := f.messageCount(t, thread.Ticket.ID); after != before {
		t.Fatalf("message count = %d after a redelivery, want %d", after, before)
	}

	// And the log has one row, not two.
	var rows int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM ticket_mail_logs
		WHERE direction = 'inbound' AND message_id = 'customer-2@example.test'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("inbound log rows = %d, want 1", rows)
	}
}

// An auto-reply must not become a ticket reply, or two automated systems talk forever.
func TestAutoReplyIsDropped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "out of office")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-3@panel.test>", "hello")
	before := f.messageCount(t, thread.Ticket.ID)

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID:     "<ooo-1@example.test>",
		From:          "alice@example.test",
		To:            []string{"support@panel.test"},
		Subject:       "Automatic reply: out of office",
		InReplyTo:     "<staff-3@panel.test>",
		Body:          "I am away until Monday.",
		AutoSubmitted: "auto-replied",
		Source:        "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestIgnored || result.Reason != tickets.ReasonAutoReply {
		t.Fatalf("outcome = %q/%q, want ignored/auto_reply", result.Outcome, result.Reason)
	}
	if after := f.messageCount(t, thread.Ticket.ID); after != before {
		t.Fatalf("message count = %d, want %d (nothing appended)", after, before)
	}
	log := f.lastMailLog(t)
	if log.status != "ignored" || log.reason != tickets.ReasonAutoReply {
		t.Fatalf("log = %+v, want ignored/auto_reply", log)
	}

	// The same marker in a header map, which is how a provider usually presents it.
	headerResult, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<ooo-2@example.test>",
		From:      "alice@example.test",
		Subject:   "Automatic reply",
		Body:      "away",
		Headers:   map[string]string{"Auto-Submitted": "auto-generated; owner-email=alice@example.test"},
	})
	if err != nil {
		t.Fatalf("header ingest: %v", err)
	}
	if headerResult.Reason != tickets.ReasonAutoReply {
		t.Fatalf("header reason = %q, want auto_reply", headerResult.Reason)
	}
}

// A message whose From is the panel's own piping address is a loop, and is dropped.
func TestMessageFromThePanelAddressIsDropped(t *testing.T) {
	f := newFixture(t)
	setMailSetting(t, f.pool, "piping_address", "support@panel.test")
	ctx := context.Background()

	thread := f.open(t, f.alice, "loop")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-4@panel.test>", "our own mail")
	before := f.messageCount(t, thread.Ticket.ID)

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<loop-1@panel.test>",
		From:      "Support <SUPPORT@panel.test>",
		To:        []string{"support@panel.test"},
		Subject:   "Re: loop",
		InReplyTo: "<staff-4@panel.test>",
		Body:      "our own notification came back",
		Source:    "imap",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestIgnored || result.Reason != tickets.ReasonOwnAddress {
		t.Fatalf("outcome = %q/%q, want ignored/own_address", result.Outcome, result.Reason)
	}
	if after := f.messageCount(t, thread.Ticket.ID); after != before {
		t.Fatalf("message count = %d, want %d", after, before)
	}

	// The panel's own outgoing header is the second loop signal, whatever the From says.
	headerResult, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<loop-2@panel.test>",
		From:      "alice@example.test",
		Subject:   "Re: loop",
		Body:      "forwarded our own mail",
		Headers:   map[string]string{"X-Billing-Panel-Outgoing": "ticket"},
	})
	if err != nil {
		t.Fatalf("header ingest: %v", err)
	}
	if headerResult.Reason != tickets.ReasonOwnHeader {
		t.Fatalf("header reason = %q, want own_header", headerResult.Reason)
	}
}

// A reply from an address that is not the ticket's customer is refused, not appended.
//
// This is the attack the whole attribution core exists to stop: Bob quotes a Message-ID from
// Alice's thread and replies to it. Appending his message would show him the thread; even
// appending only his message would put a stranger's words in her support conversation.
func TestStrangerReplyIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "alice's private thread")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-5@panel.test>", "your invoice is attached")
	before := f.messageCount(t, thread.Ticket.ID)

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<bob-1@example.test>",
		From:      "bob@example.test",
		To:        []string{"support@panel.test"},
		Subject:   "Re: alice's private thread",
		InReplyTo: "<staff-5@panel.test>",
		Body:      "please send me her invoice too",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestUnattributed {
		t.Fatalf("outcome = %q, want unattributed", result.Outcome)
	}
	if result.Reason != tickets.ReasonSenderMismatch {
		t.Fatalf("reason = %q, want sender_mismatch", result.Reason)
	}
	if after := f.messageCount(t, thread.Ticket.ID); after != before {
		t.Fatalf("message count = %d, want %d: a stranger was appended", after, before)
	}

	// The refusal is stored, not dropped, and it names the ticket so an operator can see it.
	log := f.lastMailLog(t)
	if log.status != "ignored" || log.reason != tickets.ReasonSenderMismatch {
		t.Fatalf("log = %+v, want ignored/sender_mismatch", log)
	}
	if log.ticketID != thread.Ticket.ID {
		t.Fatalf("log ticket = %q, want %q", log.ticketID, thread.Ticket.ID)
	}

	// A refusal that names someone else's ticket is audited as a denied attempt, so an
	// investigation can find it without reading every mail log row.
	var audits int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE action = 'ticket.piping_refused' AND target_id = $1 AND outcome = 'denied'`,
		thread.Ticket.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("refusal audits = %d, want 1", audits)
	}

	// The same refusal applies when the ticket id is in the reply address instead of the
	// headers: a forged address is no better than a forged header.
	addressResult, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<bob-2@example.test>",
		From:      "bob@example.test",
		To:        []string{"support+ticket-" + thread.Ticket.ID + "@panel.test"},
		Subject:   "Re: alice's private thread",
		Body:      "and again",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("address ingest: %v", err)
	}
	if addressResult.Reason != tickets.ReasonSenderMismatch {
		t.Fatalf("address reason = %q, want sender_mismatch", addressResult.Reason)
	}
	if after := f.messageCount(t, thread.Ticket.ID); after != before {
		t.Fatalf("message count = %d after a forged address, want %d", after, before)
	}
}

// An address that matches two customers is a refusal, not a guess.
func TestAmbiguousSenderIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Alice's verified login is now linked to Bob's customer as well, so her address belongs
	// to two billing accounts.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		f.aliceUser, f.bob); err != nil {
		t.Fatalf("link alice's login to bob: %v", err)
	}

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<ambiguous-1@example.test>",
		From:      "alice@example.test",
		Subject:   "who am i",
		Body:      "hello",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestUnattributed || result.Reason != tickets.ReasonAmbiguousSender {
		t.Fatalf("outcome = %q/%q, want unattributed/ambiguous_sender", result.Outcome, result.Reason)
	}
}

// An unknown sender opens nothing and is recorded.
func TestUnknownSenderIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<stranger-1@example.test>",
		From:      "stranger@elsewhere.test",
		Subject:   "buy cheap watches",
		Body:      "click here",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestUnattributed || result.Reason != tickets.ReasonNoCustomer {
		t.Fatalf("outcome = %q/%q, want unattributed/no_customer", result.Outcome, result.Reason)
	}
	var ticketsCreated int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM tickets`).Scan(&ticketsCreated); err != nil {
		t.Fatal(err)
	}
	if ticketsCreated != 0 {
		t.Fatalf("tickets = %d, want 0: an unknown sender opened a ticket", ticketsCreated)
	}
}

// A message from exactly one customer, with no thread, opens a ticket for that customer.
func TestKnownSenderWithoutAThreadOpensATicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<new-1@example.test>",
		From:      "Alice <alice@example.test>",
		To:        []string{"support@panel.test"},
		Subject:   "cannot log in",
		Body:      "the reset link expired",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestCreated {
		t.Fatalf("outcome = %q (%s), want created", result.Outcome, result.Reason)
	}
	if result.CustomerID != f.alice {
		t.Fatalf("customer = %q, want alice", result.CustomerID)
	}
	thread, err := f.svc.ThreadForCustomer(ctx, f.alice, result.TicketID)
	if err != nil {
		t.Fatalf("read created ticket: %v", err)
	}
	if thread.Ticket.Subject != "cannot log in" {
		t.Fatalf("subject = %q", thread.Ticket.Subject)
	}
	if len(thread.Messages) != 1 || !strings.Contains(thread.Messages[0].Body, "reset link expired") {
		t.Fatalf("messages = %+v", thread.Messages)
	}
}

// A reply to a closed ticket reopens it. That is the behaviour a customer expects and the
// reason piping exists at all.
func TestReplyToAClosedTicketReopensIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "closed but not solved")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-6@panel.test>", "anything else?")
	if _, err := f.svc.CloseAsStaff(ctx, thread.Ticket.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<reopen-1@example.test>",
		From:      "alice@example.test",
		To:        []string{"support@panel.test"},
		Subject:   "Re: closed but not solved",
		InReplyTo: "<staff-6@panel.test>",
		Body:      "actually it is still broken",
		Source:    "imap",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestAttached {
		t.Fatalf("outcome = %q (%s), want attached", result.Outcome, result.Reason)
	}
	reopened, err := f.svc.ThreadForStaff(ctx, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	if reopened.Ticket.Status != "open" {
		t.Fatalf("status = %q, want open", reopened.Ticket.Status)
	}
	if reopened.Ticket.ClosedAt != nil {
		t.Fatalf("closed_at = %v, want nil", reopened.Ticket.ClosedAt)
	}
}

// The reply address names the ticket when the headers do not.
func TestReplyAddressNamesTheTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "reply address")

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<address-1@example.test>",
		From:      "alice@example.test",
		To:        []string{"support+ticket-" + thread.Ticket.ID + "@panel.test"},
		Subject:   "re: reply address",
		Body:      "the headers were stripped by my client",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestAttached {
		t.Fatalf("outcome = %q (%s), want attached", result.Outcome, result.Reason)
	}
	if result.AttributedBy != tickets.AttributedByReplyAddress {
		t.Fatalf("attributed_by = %q, want reply_address", result.AttributedBy)
	}
	if result.TicketID != thread.Ticket.ID {
		t.Fatalf("ticket = %q, want %q", result.TicketID, thread.Ticket.ID)
	}
}

// A References chain that names two tickets is ambiguous, and is refused.
func TestAmbiguousThreadIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := f.open(t, f.alice, "first thread")
	f.addStaffReply(t, first.Ticket.ID, "<one@panel.test>", "first")
	second := f.open(t, f.alice, "second thread")
	f.addStaffReply(t, second.Ticket.ID, "<two@panel.test>", "second")

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID:  "<merge-1@example.test>",
		From:       "alice@example.test",
		Subject:    "two threads in one chain",
		Body:       "which ticket is this?",
		References: []string{"<one@panel.test>", "<two@panel.test>"},
		Source:     "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestUnattributed || result.Reason != tickets.ReasonAmbiguousThread {
		t.Fatalf("outcome = %q/%q, want unattributed/ambiguous_thread", result.Outcome, result.Reason)
	}
}

// The staff view lists refusals with their reason, so a reply that could not be attributed is
// visible rather than lost.
func TestInboundMailListsRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<listed-1@example.test>",
		From:      "stranger@elsewhere.test",
		Subject:   "unattributable",
		Body:      "body",
		Source:    "webhook",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rows, _, err := f.svc.ListInboundMail(ctx, tickets.MailLogFilter{Status: "ignored"}, 50, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Reason != tickets.ReasonNoCustomer || rows[0].Status != "ignored" {
		t.Fatalf("row = %+v, want ignored/no_customer", rows[0])
	}
}

// A message with no usable From cannot be attributed to anyone, and is dropped.
func TestMessageWithoutSenderIsDropped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<anonymous-1@example.test>",
		Subject:   "no sender",
		Body:      "who sent this?",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestIgnored || result.Reason != tickets.ReasonNoSender {
		t.Fatalf("outcome = %q/%q, want ignored/no_sender", result.Outcome, result.Reason)
	}
}

// A reply address naming a ticket that does not exist is refused rather than turned into a
// new ticket: the sender said which thread they meant, and the answer is "that thread is
// gone", not a fresh one.
func TestUnknownTicketInReplyAddressIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<unknown-ticket-1@example.test>",
		From:      "alice@example.test",
		To:        []string{"support+ticket-0f8fad5b-d9cb-469f-a165-70867728950e@panel.test"},
		Subject:   "re: something",
		Body:      "hello",
		Source:    "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestUnattributed || result.Reason != tickets.ReasonUnknownTicket {
		t.Fatalf("outcome = %q/%q, want unattributed/unknown_ticket", result.Outcome, result.Reason)
	}
}
