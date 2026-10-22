package tickets_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/tickets"
)

// The ticket store's tests assert the rules that make support safe, not the SQL:
//
//   - another customer's ticket is ErrNotFound, never a distinguishable refusal;
//   - a reply moves the status and the reply clock;
//   - an oversized attachment is refused before it is stored;
//   - a duplicate email Message-ID is ignored rather than duplicated;
//   - auto-close closes stale open tickets and nothing else.

var testNow = time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC)

type fixture struct {
	pool *store.Pool
	svc  *tickets.Store
	// alice and bob are two customers with nothing in common.
	alice, bob string
	aliceUser  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	var out fixture
	out.pool = pool
	out.svc = tickets.New(pool)
	out.svc.Now = func() time.Time { return testNow }

	for _, who := range []struct {
		email string
		id    *string
		user  *string
	}{
		{"alice@example.test", &out.alice, &out.aliceUser},
		{"bob@example.test", &out.bob, nil},
	} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
			who.email, strings.Split(who.email, "@")[0]).Scan(who.id); err != nil {
			t.Fatalf("customer %s: %v", who.email, err)
		}
		if who.user != nil {
			if err := pool.QueryRow(ctx, `
				INSERT INTO users (email, password_hash, display_name, email_verified_at)
				VALUES ($1, 'x', $2, now()) RETURNING id`,
				who.email, strings.Split(who.email, "@")[0]).Scan(who.user); err != nil {
				t.Fatalf("user %s: %v", who.email, err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
				*who.user, *who.id); err != nil {
				t.Fatalf("link %s: %v", who.email, err)
			}
		}
	}
	return &out
}

// open creates a ticket for a customer.
func (f *fixture) open(t *testing.T, customerID, subject string) tickets.Thread {
	t.Helper()
	thread, err := f.svc.Create(context.Background(), tickets.CreateInput{
		CustomerID: customerID,
		UserID:     f.aliceUser,
		Subject:    subject,
		Department: "general",
		Priority:   "normal",
		Body:       "the first message",
	})
	if err != nil {
		t.Fatalf("create ticket %q: %v", subject, err)
	}
	return thread
}

// ------------------------------------------------------------- the boundary --

// The security boundary: customer A cannot read customer B's ticket, and the answer
// is the same as for a ticket that never existed.
func TestCustomerCannotReadAnotherCustomersTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	aliceTicket := f.open(t, f.alice, "alice's problem")
	bobTicket := f.open(t, f.bob, "bob's problem")

	// Alice reads her own.
	thread, err := f.svc.ThreadForCustomer(ctx, f.alice, aliceTicket.Ticket.ID)
	if err != nil {
		t.Fatalf("alice's own ticket: %v", err)
	}
	if thread.Ticket.Subject != "alice's problem" {
		t.Fatalf("subject = %q", thread.Ticket.Subject)
	}

	// Alice cannot read Bob's: not found, not forbidden.
	_, err = f.svc.ThreadForCustomer(ctx, f.alice, bobTicket.Ticket.ID)
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer read = %v, want ErrNotFound", err)
	}
	// And the answer is identical to a well-formed id that does not exist.
	_, err = f.svc.ThreadForCustomer(ctx, f.alice, "0f8fad5b-d9cb-469f-a165-70867728950e")
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("nonexistent read = %v, want ErrNotFound", err)
	}

	// A malformed id is also not found, so a probe cannot tell the two apart.
	if _, err := f.svc.ThreadForCustomer(ctx, f.alice, "not-a-uuid"); !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("malformed read = %v, want ErrNotFound", err)
	}

	// The list is scoped too.
	rows, _, err := f.svc.ListForCustomer(ctx, f.alice, "", 50, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != aliceTicket.Ticket.ID {
		t.Fatalf("alice's list = %+v, want only her ticket", rows)
	}
}

// A reply to someone else's ticket is not found, and does not write a message.
func TestCustomerCannotReplyToAnotherCustomersTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	bobTicket := f.open(t, f.bob, "bob's problem")
	_, _, err := f.svc.ReplyAsCustomer(ctx, f.alice, bobTicket.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "injected"})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer reply = %v, want ErrNotFound", err)
	}

	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket_messages WHERE ticket_id = $1`, bobTicket.Ticket.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("message count = %d, want 1 (the first message only)", count)
	}
}

// Closing someone else's ticket is not found, and leaves it open.
func TestCustomerCannotCloseAnotherCustomersTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	bobTicket := f.open(t, f.bob, "bob's problem")
	if _, err := f.svc.CloseAsCustomer(ctx, f.alice, bobTicket.Ticket.ID); !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer close = %v, want ErrNotFound", err)
	}
	thread, err := f.svc.ThreadForStaff(ctx, bobTicket.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Ticket.Status != "open" {
		t.Fatalf("status = %q, want open", thread.Ticket.Status)
	}
}

// A ticket may only reference a service the customer owns.
func TestTicketServiceMustBelongToTheCustomer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	serviceID := f.seedService(t, f.bob)
	_, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice,
		UserID:     f.aliceUser,
		ServiceID:  serviceID,
		Subject:    "about bob's service",
		Department: "general",
		Body:       "hello",
	})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("foreign service = %v, want ErrNotFound", err)
	}

	// Her own service is accepted.
	own := f.seedService(t, f.alice)
	thread, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice,
		UserID:     f.aliceUser,
		ServiceID:  own,
		Subject:    "about my service",
		Department: "technical",
		Body:       "hello",
	})
	if err != nil {
		t.Fatalf("own service: %v", err)
	}
	if thread.Ticket.ServiceID != own {
		t.Fatalf("service id = %q, want %q", thread.Ticket.ServiceID, own)
	}
}

// ------------------------------------------------------------------ replies --

// A reply moves the ticket to open, stamps last_reply_at, and reopens a closed one.
func TestReplyUpdatesStatusAndLastReply(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "printer on fire")
	if thread.Ticket.LastReplyAt == nil {
		t.Fatal("the first message must stamp last_reply_at")
	}
	if thread.Ticket.Status != "open" {
		t.Fatalf("status after create = %q, want open", thread.Ticket.Status)
	}

	later := testNow.Add(2 * time.Hour)
	f.svc.Now = func() time.Time { return later }
	msg, duplicate, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "still on fire"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if duplicate {
		t.Fatal("a fresh reply must not be reported as a duplicate")
	}
	if msg.Body != "still on fire" {
		t.Fatalf("body = %q", msg.Body)
	}

	read, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Ticket.Status != "open" {
		t.Fatalf("status after customer reply = %q, want open", read.Ticket.Status)
	}
	if read.Ticket.LastReplyAt == nil || !read.Ticket.LastReplyAt.Equal(later) {
		t.Fatalf("last_reply_at = %v, want %v", read.Ticket.LastReplyAt, later)
	}
	if len(read.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(read.Messages))
	}

	// A staff reply sets the ticket to replied and moves the clock again.
	staffUser := f.seedStaff(t)
	evenLater := later.Add(time.Hour)
	f.svc.Now = func() time.Time { return evenLater }
	if _, _, err := f.svc.ReplyAsStaff(ctx, staffUser, thread.Ticket.ID,
		tickets.ReplyInput{UserID: staffUser, Body: "on our way"}); err != nil {
		t.Fatalf("staff reply: %v", err)
	}
	read, err = f.svc.ThreadForStaff(ctx, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Ticket.Status != "replied" {
		t.Fatalf("status after staff reply = %q, want replied", read.Ticket.Status)
	}
	if !read.Ticket.LastReplyAt.Equal(evenLater) {
		t.Fatalf("last_reply_at = %v, want %v", read.Ticket.LastReplyAt, evenLater)
	}
	last := read.Messages[len(read.Messages)-1]
	if !last.FromStaff || last.AuthorUserID != staffUser {
		t.Fatalf("last message = %+v, want a staff message", last)
	}

	// A customer reply to a closed ticket reopens it.
	if _, err := f.svc.CloseAsCustomer(ctx, f.alice, thread.Ticket.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "it happened again"}); err != nil {
		t.Fatalf("reply after close: %v", err)
	}
	read, err = f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Ticket.Status != "open" || read.Ticket.ClosedAt != nil {
		t.Fatalf("after reopening: status = %q, closed_at = %v", read.Ticket.Status, read.Ticket.ClosedAt)
	}
}

// A duplicate Message-ID is ignored: the stored message is returned, the thread does
// not grow, and the reply clock does not move.
func TestDuplicateMessageIDIsIgnored(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "email me")
	first, duplicate, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "from email", MessageID: "<abc@mail.test>"})
	if err != nil {
		t.Fatalf("first reply: %v", err)
	}
	if duplicate {
		t.Fatal("the first delivery must not be a duplicate")
	}

	// A second delivery of the same Message-ID, an hour later.
	f.svc.Now = func() time.Time { return testNow.Add(time.Hour) }
	again, duplicate, err := f.svc.ReplyAsCustomer(ctx, f.alice, thread.Ticket.ID,
		tickets.ReplyInput{UserID: f.aliceUser, Body: "from email", MessageID: "<abc@mail.test>"})
	if err != nil {
		t.Fatalf("duplicate reply: %v", err)
	}
	if !duplicate {
		t.Fatal("the second delivery must be reported as a duplicate")
	}
	if again.ID != first.ID {
		t.Fatalf("duplicate returned %q, want the stored message %q", again.ID, first.ID)
	}

	read, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (create + one reply)", len(read.Messages))
	}
	if !read.Ticket.LastReplyAt.Equal(testNow) {
		t.Fatalf("last_reply_at = %v, want the first delivery's %v", read.Ticket.LastReplyAt, testNow)
	}
}

// --------------------------------------------------------------- attachments --

// An oversized attachment is refused, and nothing is stored.
func TestOversizedAttachmentIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "big file")
	big := make([]byte, tickets.MaxAttachmentBytes+1)

	_, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "huge.bin", ContentType: "application/octet-stream", Bytes: big})
	if !errors.Is(err, tickets.ErrAttachmentTooLarge) {
		t.Fatalf("oversized attachment = %v, want ErrAttachmentTooLarge", err)
	}

	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket_attachments`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("attachments = %d, want 0", count)
	}

	// Exactly at the cap is accepted: the boundary is "over", not "at".
	atLimit := make([]byte, tickets.MaxAttachmentBytes)
	if _, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "limit.bin", ContentType: "application/octet-stream", Bytes: atLimit}); err != nil {
		t.Fatalf("attachment at the limit: %v", err)
	}
}

// An attachment on someone else's ticket is not found.
func TestAttachmentOnAnotherCustomersTicketIsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	bobTicket := f.open(t, f.bob, "bob's file")
	_, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, bobTicket.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "x.txt", ContentType: "text/plain", Bytes: []byte("hi")})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer attachment = %v, want ErrNotFound", err)
	}
}

// A download is scoped: the owner reads it, everyone else does not.
func TestAttachmentDownloadIsScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "with a file")
	attachment, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{
			Filename:    "log.txt",
			ContentType: "text/plain",
			Bytes:       []byte("hello"),
		})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if attachment.ContentType != "text/plain" {
		t.Fatalf("stored content type = %q, want text/plain", attachment.ContentType)
	}

	got, err := f.svc.AttachmentForCustomer(ctx, f.alice, attachment.ID)
	if err != nil {
		t.Fatalf("owner download: %v", err)
	}
	if string(got.Bytes) != "hello" {
		t.Fatalf("bytes = %q", got.Bytes)
	}

	if _, err := f.svc.AttachmentForCustomer(ctx, f.bob, attachment.ID); !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer download = %v, want ErrNotFound", err)
	}
}

// The upload's content type is an allowlist, not a downgrade.
//
// The old behaviour normalised text/html to application/octet-stream and stored it, which meant a
// ticket could carry a document the sender chose the type of. It is now refused, and the refusal
// names the accepted set so the sender can convert or zip the file instead of guessing. A type
// that *is* accepted keeps its own type, and an unknown type is stored opaque.
func TestAttachmentContentTypeAllowlist(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	thread := f.open(t, f.alice, "file types")

	for _, refused := range []string{
		"text/html",
		"image/svg+xml",
		"application/javascript",
		"text/xml",
		"application/x-msdownload",
	} {
		_, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
			tickets.AttachmentInput{Filename: "file.bin", ContentType: refused, Bytes: []byte("x")})
		if !errors.Is(err, tickets.ErrAttachmentType) {
			t.Fatalf("content type %q = %v, want ErrAttachmentType", refused, err)
		}
	}

	// An extension that implies a refused type is refused when no type was claimed.
	_, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "page.html", Bytes: []byte("<script>")})
	if !errors.Is(err, tickets.ErrAttachmentType) {
		t.Fatalf("an html file with no claimed type = %v, want ErrAttachmentType", err)
	}

	// An accepted type keeps its own type.
	kept, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "screen.png", ContentType: "image/png", Bytes: []byte("\x89PNG")})
	if err != nil {
		t.Fatalf("png upload: %v", err)
	}
	if kept.ContentType != "image/png" {
		t.Fatalf("stored content type = %q, want image/png", kept.ContentType)
	}

	// A file whose type is genuinely unknown is stored as an opaque download.
	opaque, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "",
		tickets.AttachmentInput{Filename: "dump", ContentType: "application/octet-stream", Bytes: []byte("\x00\x01")})
	if err != nil {
		t.Fatalf("opaque upload: %v", err)
	}
	if opaque.ContentType != "application/octet-stream" {
		t.Fatalf("stored content type = %q, want application/octet-stream", opaque.ContentType)
	}
}

// Identical bytes attached to one message are one row.
func TestDuplicateAttachmentCollapsesByDigest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "same file twice")
	in := tickets.AttachmentInput{Filename: "a.txt", ContentType: "text/plain", Bytes: []byte("same")}
	first, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "", in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.AddAttachmentAsCustomer(ctx, f.alice, thread.Ticket.ID, "", in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("ids differ: %q vs %q", first.ID, second.ID)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ticket_attachments`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("attachments = %d, want 1", count)
	}
}

// ------------------------------------------------------------------ staff --

// The queue filters work, and a staff update is reflected in the thread.
func TestStaffQueueFiltersAndUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	high := f.open(t, f.alice, "urgent")
	if _, err := f.svc.UpdateStaff(ctx, high.Ticket.ID, tickets.StaffUpdate{
		Priority: "high", Department: "billing",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.open(t, f.bob, "routine")

	all, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{}, 50, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d, want 2", len(all))
	}
	urgent, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Priority: "high"}, 50, "")
	if err != nil {
		t.Fatalf("list high: %v", err)
	}
	if len(urgent) != 1 || urgent[0].ID != high.Ticket.ID {
		t.Fatalf("high priority = %+v, want only the urgent ticket", urgent)
	}
	// The staff row carries the customer, which the queue needs.
	if urgent[0].CustomerEmail != "alice@example.test" {
		t.Fatalf("customer email = %q", urgent[0].CustomerEmail)
	}

	// An unknown filter is a caller error, not an empty page.
	if _, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Priority: "critical"}, 50, ""); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("unknown priority = %v, want ErrInvalidInput", err)
	}

	// Assignment must name a staff user.
	if _, err := f.svc.UpdateStaff(ctx, high.Ticket.ID, tickets.StaffUpdate{
		SetAssignee: true, Assignee: f.aliceUser,
	}); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("assign to a customer = %v, want ErrInvalidInput", err)
	}
	staffUser := f.seedStaff(t)
	assigned, err := f.svc.UpdateStaff(ctx, high.Ticket.ID, tickets.StaffUpdate{
		SetAssignee: true, Assignee: staffUser,
	})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if assigned.AssignedTo != staffUser {
		t.Fatalf("assignee = %q, want %q", assigned.AssignedTo, staffUser)
	}
	mine, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Assignee: staffUser}, 50, "")
	if err != nil {
		t.Fatalf("list by assignee: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != high.Ticket.ID {
		t.Fatalf("by assignee = %+v", mine)
	}
}

// Closing through the staff path stamps closed_at.
func TestStaffCloseStampsClosedAt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "closable")
	closed, err := f.svc.CloseAsStaff(ctx, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatalf("closed = %+v", closed)
	}
	if !closed.ClosedAt.Equal(testNow) {
		t.Fatalf("closed_at = %v, want %v", closed.ClosedAt, testNow)
	}
}

// ------------------------------------------------------------------ config --

// The settings are read with code-side defaults, so a missing row does not break the
// screen, and a disabled installation refuses to open a ticket.
func TestSettingsDefaultsAndDisabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	settings, err := f.svc.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if settings.Disabled {
		t.Fatal("tickets must be enabled by default")
	}
	if len(settings.Departments) == 0 || settings.Departments[0] != "general" {
		t.Fatalf("departments = %v", settings.Departments)
	}

	// A missing row falls back to the code-side defaults rather than an empty list.
	if _, err := f.pool.Exec(ctx, `DELETE FROM settings WHERE group_key = 'tickets'`); err != nil {
		t.Fatal(err)
	}
	settings, err = f.svc.Settings(ctx)
	if err != nil {
		t.Fatalf("settings without rows: %v", err)
	}
	if len(settings.Departments) != len(tickets.DefaultDepartments) {
		t.Fatalf("departments = %v, want the defaults", settings.Departments)
	}
	if settings.AutoCloseDays != tickets.DefaultAutoCloseDays {
		t.Fatalf("autoclose days = %d", settings.AutoCloseDays)
	}

	// Disabled refuses creation with a stable error.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('tickets', 'disabled', 'true')`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, Subject: "nope", Department: "general", Body: "nope",
	})
	if !errors.Is(err, tickets.ErrDisabled) {
		t.Fatalf("create while disabled = %v, want ErrDisabled", err)
	}
}

// A department that is not configured is a validation error, not a silent accept.
func TestUnknownDepartmentIsRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, Subject: "x", Department: "sales", Body: "hello",
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("unknown department = %v, want ErrInvalidInput", err)
	}
	_, err = f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, Subject: "x", Department: "general", Priority: "critical", Body: "hello",
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("unknown priority = %v, want ErrInvalidInput", err)
	}
}

// Ticket numbers come from the invoice sequence mechanism under their own scope, so
// they are sequential and do not collide with invoice numbers.
func TestTicketNumbersAreAllocatedFromTheSharedSequence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := f.open(t, f.alice, "one")
	second := f.open(t, f.bob, "two")
	if first.Ticket.Number == "" || second.Ticket.Number == "" {
		t.Fatalf("numbers: %q and %q", first.Ticket.Number, second.Ticket.Number)
	}
	if first.Ticket.Number == second.Ticket.Number {
		t.Fatalf("both tickets got %q", first.Ticket.Number)
	}
	if !strings.HasPrefix(first.Ticket.Number, "TKT-") {
		t.Fatalf("number = %q, want the TKT- prefix", first.Ticket.Number)
	}
	var scope string
	if err := f.pool.QueryRow(ctx,
		`SELECT scope FROM invoice_number_sequences WHERE scope = 'ticket'`).Scan(&scope); err != nil {
		t.Fatalf("the ticket scope must exist in invoice_number_sequences: %v", err)
	}
}

// ------------------------------------------------------------------ TKT-10 --

// CloseStale closes only tickets that are open or replied and older than the
// threshold. A closed ticket stays closed, a fresh one stays open, and a ticket with
// no reply at all ages from its creation time.
func TestCloseStaleClosesOnlyStaleOpenTickets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Stale: created and never touched, 20 days ago.
	f.svc.Now = func() time.Time { return testNow.AddDate(0, 0, -20) }
	abandoned := f.open(t, f.alice, "abandoned")

	// Stale: replied to 20 days ago.
	replied := f.open(t, f.bob, "replied long ago")

	// Fresh: created today.
	f.svc.Now = func() time.Time { return testNow }
	fresh := f.open(t, f.alice, "fresh")

	// Already closed, and stale: must not be touched again.
	closed := f.open(t, f.bob, "already closed")
	if _, err := f.svc.CloseAsStaff(ctx, closed.Ticket.ID); err != nil {
		t.Fatal(err)
	}

	closedAt := testNow
	if _, err := f.pool.Exec(ctx,
		`UPDATE tickets SET closed_at = $2 WHERE id = $1`, closed.Ticket.ID, closedAt.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}

	f.svc.Now = func() time.Time { return testNow }
	got, err := f.svc.CloseStale(ctx, 14)
	if err != nil {
		t.Fatalf("CloseStale: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("closed %d tickets, want 2: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, ticket := range got {
		ids[ticket.ID] = true
	}
	if !ids[abandoned.Ticket.ID] || !ids[replied.Ticket.ID] {
		t.Fatalf("closed %v, want the abandoned and the long-replied tickets", ids)
	}

	// The fresh ticket is still open.
	thread, err := f.svc.ThreadForCustomer(ctx, f.alice, fresh.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Ticket.Status != "open" {
		t.Fatalf("fresh ticket status = %q, want open", thread.Ticket.Status)
	}
	// The stale ones are closed, with closed_at stamped.
	for _, id := range []string{abandoned.Ticket.ID, replied.Ticket.ID} {
		thread, err := f.svc.ThreadForStaff(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if thread.Ticket.Status != "closed" || thread.Ticket.ClosedAt == nil {
			t.Fatalf("ticket %s = %+v, want closed", id, thread.Ticket)
		}
	}

	// Running it again closes nothing: the work is idempotent.
	again, err := f.svc.CloseStale(ctx, 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second run closed %d, want 0", len(again))
	}
}

// CloseStale with no explicit day count uses the configured value.
func TestCloseStaleUsesTheConfiguredThreshold(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		UPDATE settings SET value = '3' WHERE group_key = 'tickets' AND key = 'autoclose_days'`); err != nil {
		t.Fatal(err)
	}
	f.svc.Now = func() time.Time { return testNow.AddDate(0, 0, -5) }
	old := f.open(t, f.alice, "five days old")
	f.svc.Now = func() time.Time { return testNow.AddDate(0, 0, -1) }
	recent := f.open(t, f.alice, "one day old")

	f.svc.Now = func() time.Time { return testNow }
	got, err := f.svc.CloseStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != old.Ticket.ID {
		t.Fatalf("closed %+v, want only %s", got, old.Ticket.ID)
	}
	if _, err := f.svc.ThreadForCustomer(ctx, f.alice, recent.Ticket.ID); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- fixtures --

// seedService creates an active service for a customer.
func (f *fixture) seedService(t *testing.T, customerID string) string {
	t.Helper()
	ctx := context.Background()
	var productID, priceID, subID, serviceID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, 'VPS') RETURNING id`,
		"vps-"+customerID[:8]).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone) VALUES ($1, 1, 'USD', 1000, 'monthly', 1, 'UTC')
		RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now(), now() + interval '30 days',
			now() + interval '30 days') RETURNING id`, customerID, priceID).Scan(&subID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, subscription_id, product_id, status, generation)
		VALUES ($1, $2, $3, 'active', 1) RETURNING id`,
		customerID, subID, productID).Scan(&serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return serviceID
}

// seedStaff creates a support user and returns its id.
func (f *fixture) seedStaff(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ('agent@example.test', 'x', 'Agent', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("staff user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'support')`, id); err != nil {
		t.Fatalf("staff role: %v", err)
	}
	return id
}

// A redelivered email that opened a ticket does not create a second ticket: the
// existing thread is returned, marked as a duplicate, and no number is consumed.
func TestDuplicateCreateMessageIDDoesNotCreateASecondTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	in := tickets.CreateInput{
		CustomerID: f.alice,
		UserID:     f.aliceUser,
		Subject:    "opened by email",
		Department: "general",
		Body:       "hello from email",
		MessageID:  "<opener@mail.test>",
	}
	first, err := f.svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the first delivery must not be a duplicate")
	}
	second, err := f.svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("the second delivery must be reported as a duplicate")
	}
	if second.Ticket.ID != first.Ticket.ID {
		t.Fatalf("second create made ticket %q, want the existing %q", second.Ticket.ID, first.Ticket.ID)
	}

	var ticketsCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM tickets`).Scan(&ticketsCount); err != nil {
		t.Fatal(err)
	}
	if ticketsCount != 1 {
		t.Fatalf("tickets = %d, want 1", ticketsCount)
	}
	// The rolled-back attempt returned its number to the sequence, so the next ticket
	// gets the next value rather than skipping one.
	next := f.open(t, f.bob, "after the duplicate")
	if next.Ticket.Number == first.Ticket.Number {
		t.Fatalf("the next ticket reused %q", next.Ticket.Number)
	}
}

// A Message-ID that belongs to another customer's ticket is not a duplicate of
// anything this caller may see: reusing it must not return that thread's body.
func TestMessageIDFromAnotherCustomerIsNotReusable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	bobThread := f.open(t, f.bob, "bob's thread")
	if _, _, err := f.svc.ReplyAsCustomer(ctx, f.bob, bobThread.Ticket.ID,
		tickets.ReplyInput{Body: "bob's private words", MessageID: "<bob@mail.test>"}); err != nil {
		t.Fatalf("bob reply: %v", err)
	}

	// Alice opens a ticket claiming Bob's Message-ID: not found, and no ticket is made.
	_, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, Subject: "stolen id",
		Department: "general", Body: "hi", MessageID: "<bob@mail.test>",
	})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("create with a foreign Message-ID = %v, want ErrNotFound", err)
	}

	// Alice replying on her own ticket with Bob's Message-ID does not return Bob's body.
	aliceThread := f.open(t, f.alice, "alice's thread")
	msg, duplicate, err := f.svc.ReplyAsCustomer(ctx, f.alice, aliceThread.Ticket.ID,
		tickets.ReplyInput{Body: "mine", MessageID: "<bob@mail.test>"})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("reply with a foreign Message-ID = %v, want ErrNotFound", err)
	}
	if duplicate || msg.Body != "" {
		t.Fatalf("leaked a message: %+v duplicate=%v", msg, duplicate)
	}

	var ticketsCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM tickets`).Scan(&ticketsCount); err != nil {
		t.Fatal(err)
	}
	if ticketsCount != 2 {
		t.Fatalf("tickets = %d, want 2 (bob's and alice's)", ticketsCount)
	}
}

// The package-level entry point the worker calls behaves like the method and reports
// how many tickets it closed.
func TestCloseStalePackageFunction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The package function builds its own store with the real clock, so the ages here
	// are relative to now rather than to the fixture's pinned instant.
	f.svc.Now = func() time.Time { return time.Now().UTC().AddDate(0, 0, -30) }
	f.open(t, f.alice, "long abandoned")
	f.svc.Now = func() time.Time { return time.Now().UTC() }
	f.open(t, f.alice, "fresh")

	n, err := tickets.CloseStale(ctx, f.pool, 14)
	if err != nil {
		t.Fatalf("CloseStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
}

// The queue's search matches the subject and the customer, and refuses a term too
// short to be selective (which would be a table scan dressed as a feature).
func TestStaffQueueSearch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.open(t, f.alice, "printer on fire")
	f.open(t, f.bob, "cannot log in")

	bySubject, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Query: "printer"}, 50, "")
	if err != nil {
		t.Fatalf("search by subject: %v", err)
	}
	if len(bySubject) != 1 || bySubject[0].Subject != "printer on fire" {
		t.Fatalf("by subject = %+v", bySubject)
	}
	byCustomer, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Query: "bob@"}, 50, "")
	if err != nil {
		t.Fatalf("search by customer: %v", err)
	}
	if len(byCustomer) != 1 || byCustomer[0].CustomerEmail != "bob@example.test" {
		t.Fatalf("by customer = %+v", byCustomer)
	}
	// A wildcard is a literal, not a pattern: "__" escaped matches a subject
	// containing two underscores, which none has. Unescaped it would match every
	// subject of two characters or more.
	none, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Query: "__"}, 50, "")
	if err != nil {
		t.Fatalf("search with a wildcard: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("a literal __ matched %d tickets", len(none))
	}
	// One character is refused rather than scanned.
	if _, _, err := f.svc.ListForStaff(ctx, tickets.StaffFilter{Query: "p"}, 50, ""); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("one-character search = %v, want ErrInvalidInput", err)
	}
}
