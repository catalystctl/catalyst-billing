package tickets_test

import (
	"context"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/tickets"
)

// Reply notifications (TKT-13).
//
// Each of these fails if the notification is removed from the reply transaction, which is the
// whole point: the customer half of the pair was missing for so long because nothing asserted
// that a staff reply reached the customer at all.

// seedTemplates installs the outbox templates a migrated-but-never-booted test database lacks.
func seedTemplates(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := notify.SeedTemplates(context.Background(), f.pool); err != nil {
		t.Fatalf("seed notification templates: %v", err)
	}
}

// deliveryCount counts outbox rows for one dedupe key, and returns the recipient of the first.
func deliveryCount(t *testing.T, f *fixture, dedupeKey string) (int, string) {
	t.Helper()
	var (
		n  int
		to string
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(min(to_address), '') FROM notification_deliveries
		WHERE dedupe_key = $1`, dedupeKey).Scan(&n, &to); err != nil {
		t.Fatalf("count deliveries for %q: %v", dedupeKey, err)
	}
	return n, to
}

// inboxCount counts in-app notifications for one user and event.
func inboxCount(t *testing.T, f *fixture, userID, eventKey string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications WHERE user_id = $1 AND event_key = $2`,
		userID, eventKey).Scan(&n); err != nil {
		t.Fatalf("count in-app notifications: %v", err)
	}
	return n
}

// A staff reply tells the customer: one mail to their address and one entry in their inbox.
func TestStaffReplyNotifiesTheCustomer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedTemplates(t, f)

	thread := f.open(t, f.alice, "the invoice is wrong")

	msg, duplicate, err := f.svc.ReplyAsStaff(ctx, f.aliceUser, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "I have corrected it."})
	if err != nil {
		t.Fatalf("staff reply: %v", err)
	}
	if duplicate {
		t.Fatal("a fresh reply was reported as a duplicate")
	}

	n, to := deliveryCount(t, f, "new_ticket_message:"+msg.ID)
	if n != 1 {
		t.Fatalf("deliveries = %d, want 1: a staff reply must mail the customer", n)
	}
	if to != "alice@example.test" {
		t.Fatalf("delivery went to %q, want the ticket's customer", to)
	}
	if got := inboxCount(t, f, f.aliceUser, "new_ticket_message"); got != 1 {
		t.Fatalf("in-app entries = %d, want 1: the customer must see the reply without a mail", got)
	}
}

// A customer reply from the portal tells the assignee. Without this the queue is a screen
// somebody has to remember to refresh.
func TestPortalReplyNotifiesTheAssignee(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedTemplates(t, f)

	thread := f.open(t, f.alice, "still broken")
	var staffID, staffEmail string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ('agent@example.test', 'x', 'Agent Smith', now()) RETURNING id, email`).
		Scan(&staffID, &staffEmail); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tickets SET assigned_to = $2 WHERE id = $1`,
		thread.Ticket.ID, staffID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	msg, _, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "it is still broken"})
	if err != nil {
		t.Fatalf("customer reply: %v", err)
	}

	n, to := deliveryCount(t, f, "new_ticket_message:"+msg.ID)
	if n != 1 {
		t.Fatalf("deliveries = %d, want 1: a customer reply must notify the assignee", n)
	}
	if to != staffEmail {
		t.Fatalf("delivery went to %q, want the assignee", to)
	}
	if got := inboxCount(t, f, staffID, "new_ticket_message"); got != 1 {
		t.Fatalf("assignee inbox entries = %d, want 1", got)
	}
}

// An unassigned ticket notifies nobody: it is in the queue, and mailing every agent about
// every unclaimed reply is how a shared mailbox becomes noise.
func TestUnassignedTicketNotifiesNobody(t *testing.T) {
	f := newFixture(t)
	seedTemplates(t, f)
	ctx := context.Background()

	thread := f.open(t, f.alice, "nobody owns this")
	msg, _, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "anyone there?"})
	if err != nil {
		t.Fatalf("customer reply: %v", err)
	}
	if n, _ := deliveryCount(t, f, "new_ticket_message:"+msg.ID); n != 0 {
		t.Fatalf("deliveries = %d, want 0 for an unassigned ticket", n)
	}
}

// A customer record with no portal account still gets the mail, and gets no inbox entry:
// there is nowhere to put one.
func TestStaffReplyMailsACustomerWithoutALogin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedTemplates(t, f)

	thread := f.open(t, f.bob, "no portal account")

	msg, _, err := f.svc.ReplyAsStaff(ctx, f.aliceUser, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "answered by mail"})
	if err != nil {
		t.Fatalf("staff reply: %v", err)
	}
	n, to := deliveryCount(t, f, "new_ticket_message:"+msg.ID)
	if n != 1 || to != "bob@example.test" {
		t.Fatalf("deliveries = %d to %q, want one to bob@example.test", n, to)
	}
	var inbox int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE event_key = 'new_ticket_message'`).Scan(&inbox); err != nil {
		t.Fatalf("count in-app entries: %v", err)
	}
	if inbox != 0 {
		t.Fatalf("in-app entries = %d, want 0 when the customer has no login", inbox)
	}
}

// A duplicate staff reply (a replayed Message-ID) notifies nobody a second time.
func TestDuplicateStaffReplyNotifiesOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedTemplates(t, f)

	thread := f.open(t, f.alice, "duplicate reply")
	first, _, err := f.svc.ReplyAsStaff(ctx, f.aliceUser, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "sent twice", MessageID: "<staff-dup@panel.test>"})
	if err != nil {
		t.Fatalf("first reply: %v", err)
	}
	if _, duplicate, err := f.svc.ReplyAsStaff(ctx, f.aliceUser, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "sent twice", MessageID: "<staff-dup@panel.test>"}); err != nil {
		t.Fatalf("second reply: %v", err)
	} else if !duplicate {
		t.Fatal("the replay was not reported as a duplicate")
	}

	if n, _ := deliveryCount(t, f, "new_ticket_message:"+first.MessageID); n != 1 {
		t.Fatalf("deliveries = %d, want 1: a replayed reply must not notify twice", n)
	}
	if got := inboxCount(t, f, f.aliceUser, "new_ticket_message"); got != 1 {
		t.Fatalf("in-app entries = %d, want 1", got)
	}
}
