package tickets_test

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/tickets"
)

// Attachments over email piping (TKT-11).
//
// Two properties, and the second is the one the security review cares about: a file that
// arrives by mail is stored like any other attachment — capped, allowlisted, digest-deduped —
// and a file the portal would refuse is refused here too, so email is not a way around the
// upload checks.

// A reply carrying a screenshot stores it on the message, and the thread can serve it.
func TestPipedReplyImportsItsAttachment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "screenshot attached")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-att@panel.test>", "send me a screenshot")

	// The bytes are the sender's; validation is what decides whether they are stored.
	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<customer-att@example.test>",
		From:      "alice@example.test",
		To:        []string{"support@panel.test"},
		Subject:   "Re: screenshot attached",
		InReplyTo: "<staff-att@panel.test>",
		Body:      "here it is",
		Attachments: []tickets.AttachmentInput{{
			Filename:    "screen.png",
			ContentType: "image/png",
			Bytes:       []byte("\x89PNG\r\n\x1a\nnot-really-a-png-but-the-type-is-allowed"),
		}},
		Source: "webhook",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestAttached {
		t.Fatalf("outcome = %q (%s), want attached", result.Outcome, result.Reason)
	}

	var (
		filename string
		size     int
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT a.filename, a.byte_size
		FROM ticket_attachments a
		JOIN ticket_messages m ON m.id = a.message_id
		WHERE m.ticket_id = $1`, thread.Ticket.ID).Scan(&filename, &size); err != nil {
		t.Fatalf("read stored attachment: %v", err)
	}
	if filename != "screen.png" || size == 0 {
		t.Fatalf("stored attachment = %q (%d bytes), want screen.png", filename, size)
	}

	// The message the customer can read carries the attachment's metadata, which is what the
	// thread screen renders as a download link.
	got, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	var found bool
	for _, m := range got.Messages {
		for _, a := range m.Attachments {
			if a.Filename == "screen.png" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the thread does not offer the stored attachment")
	}
}

// A file the portal upload would refuse is refused here, not stored and not silently dropped:
// the count is recorded so an agent can see something was sent.
func TestPipedAttachmentOfRefusedTypeIsCounted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "malicious attachment")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-bad@panel.test>", "send it over")

	result, err := f.svc.Ingest(ctx, tickets.InboundMessage{
		MessageID: "<customer-bad@example.test>",
		From:      "alice@example.test",
		To:        []string{"support@panel.test"},
		Subject:   "Re: malicious attachment",
		InReplyTo: "<staff-bad@panel.test>",
		Body:      "as requested",
		Attachments: []tickets.AttachmentInput{{
			Filename:    "invoice.html",
			ContentType: "text/html",
			Bytes:       []byte("<script>alert(1)</script>"),
		}},
		Source: "imap",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Outcome != tickets.IngestAttached {
		t.Fatalf("outcome = %q (%s), want the reply itself attached", result.Outcome, result.Reason)
	}

	var stored int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM ticket_attachments a
		JOIN ticket_messages m ON m.id = a.message_id
		WHERE m.ticket_id = $1`, thread.Ticket.ID).Scan(&stored); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if stored != 0 {
		t.Fatalf("attachments stored = %d, want 0: text/html is not on the allowlist", stored)
	}

	var dropped int
	if err := f.pool.QueryRow(ctx, `
		SELECT attachments_dropped FROM ticket_mail_logs ORDER BY created_at DESC, id DESC LIMIT 1`).
		Scan(&dropped); err != nil {
		t.Fatalf("read mail log: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("attachments_dropped = %d, want 1: a refusal the agent can see", dropped)
	}
}

// The parser refuses an executable part as an attachment in its own right, without needing the
// ingest path: this is the boundary that stops a MIME message from bypassing the allowlist.
func TestParseInboundRefusesAnExecutablePart(t *testing.T) {
	raw := strings.Join([]string{
		`From: alice@example.test`,
		`Subject: look at this`,
		`Message-ID: <evil@example.test>`,
		`Content-Type: multipart/mixed; boundary="b1"`,
		``,
		`--b1`,
		`Content-Type: text/plain`,
		``,
		`please open the attachment`,
		`--b1`,
		`Content-Type: text/html; name="invoice.html"`,
		`Content-Disposition: attachment; filename="invoice.html"`,
		``,
		`<script>alert(1)</script>`,
		`--b1--`,
		``,
	}, "\r\n")

	msg, err := tickets.ParseInbound([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msg.Attachments) != 0 {
		t.Fatalf("attachments = %d, want 0: text/html cannot be stored on a ticket", len(msg.Attachments))
	}
	if msg.AttachmentsDropped != 1 {
		t.Fatalf("attachments dropped = %d, want 1", msg.AttachmentsDropped)
	}
	if strings.TrimSpace(msg.Body) != "please open the attachment" {
		t.Fatalf("body = %q, want only the reply text", msg.Body)
	}
}
