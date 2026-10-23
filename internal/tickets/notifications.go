package tickets

// Ticket reply notifications (TKT-13).
//
// A reply is only useful if the other side hears about it. Paymenter sends two notifications
// per reply and so does this: a customer's reply tells the assignee, and a staff reply tells
// the customer. The customer half used to be missing entirely — a customer's reply reached
// the assignee through the email pipe, but a staff reply from the panel notified nobody, so
// the thread went quiet from the customer's side until they thought to look.
//
// Three decisions shape this file:
//
//  1. **The notification is written in the same transaction as the reply.** The outbox row,
//     the reply and its audit row commit together or not at all. A notification written after
//     the commit is a notification that can be lost by a crash between the two, and the row it
//     describes would then exist with nobody told.
//
//  2. **The dedupe key is the message id.** A replayed delivery, a double-clicked reply button
//     and an IMAP refetch all produce the same message id, so they produce one notification.
//     That is the same rule the outbox already applies to every other event. A reply typed in
//     the panel carries no email Message-ID, so the stored message row's id stands in for it —
//     still one key per message either way.
//
//  3. **The recipient is resolved here, from the ticket, not passed in.** The customer's
//     address and the assignee's address live on different tables, and a caller that names
//     the recipient itself can name the wrong one. One function per direction, each reading
//     the ticket under the caller's transaction, is what makes that impossible to get wrong.

import (
	"context"
	"errors"
	"fmt"

	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

// ticketRecipient is who a reply notification is for, resolved from the ticket itself.
type ticketRecipient struct {
	// UserID is the login to notify. Empty means there is no login to reach — a customer
	// record with no portal account — and the notification is skipped rather than sent to an
	// address nobody can use an inbox with.
	UserID   string
	Email    string
	Name     string
	Number   string
	Subject  string
	OwnerID  string
	Assignee bool
}

// replyParticipants reads both sides of a ticket's conversation.
//
// The customer side resolves through customer_accounts rather than customers.email: the
// address on a customer record is what invoices are mailed to, which is not necessarily
// where a person signs in, and notifying the wrong one is how a support reply ends up in an
// accounts-payable mailbox.
func (s *Store) replyParticipants(ctx context.Context, tx pgx.Tx, ticketID string) (customer, assignee ticketRecipient, err error) {
	var (
		assigneeID string
	)
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(t.number, ''), t.subject, COALESCE(t.assigned_to::text, ''),
		       c.id::text, c.email, COALESCE(c.name, '')
		FROM tickets t
		JOIN customers c ON c.id = t.customer_id
		WHERE t.id = $1`, ticketID).
		Scan(&customer.Number, &customer.Subject, &assigneeID,
			&customer.OwnerID, &customer.Email, &customer.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return customer, assignee, ErrNotFound
	}
	if err != nil {
		return customer, assignee, fmt.Errorf("tickets: resolve reply participants: %w", err)
	}
	customer.Assignee = false

	// The customer's login, when the record has one. The oldest account link wins: a customer
	// with two portal users is an edge case, and picking the earliest is deterministic rather
	// than whatever the planner returns.
	var userID string
	err = tx.QueryRow(ctx, `
		SELECT u.id::text
		FROM customer_accounts ca
		JOIN users u ON u.id = ca.user_id
		WHERE ca.customer_id = $1
		ORDER BY ca.created_at, ca.id
		LIMIT 1`, customer.OwnerID).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No portal login: the address still receives the mail, which is the only channel left.
		customer.UserID = ""
	case err != nil:
		return customer, assignee, fmt.Errorf("tickets: resolve customer login: %w", err)
	default:
		customer.UserID = userID
	}

	if assigneeID == "" {
		return customer, assignee, nil
	}
	assignee.Assignee = true
	assignee.UserID = assigneeID
	assignee.Number = customer.Number
	assignee.Subject = customer.Subject
	if err := tx.QueryRow(ctx, `
		SELECT u.email, COALESCE(NULLIF(u.display_name, ''), '')
		FROM users u WHERE u.id = $1`, assigneeID).
		Scan(&assignee.Email, &assignee.Name); err != nil {
		return customer, assignee, fmt.Errorf("tickets: resolve assignee: %w", err)
	}
	return customer, assignee, nil
}

// NotifyStaffReply tells the customer their ticket has an answer.
//
// It runs inside the reply's transaction and after the message is written, so a notification
// can never announce a message that rolled back. A duplicate reply notifies nobody: the
// dedupe key is the message id, and the first delivery already said it.
func (s *Store) NotifyStaffReply(ctx context.Context, tx pgx.Tx, ticketID string, msg Message, author string) error {
	if tx == nil {
		return nil
	}
	customer, _, err := s.replyParticipants(ctx, tx, ticketID)
	if err != nil {
		return err
	}
	if customer.Email == "" {
		return nil
	}
	if _, err := notify.TicketMessage(ctx, tx, notify.NewQueue(s.pool), notify.TicketMessageInput{
		UserID:    customer.UserID,
		Email:     customer.Email,
		TicketID:  ticketID,
		Reference: customer.Number,
		Subject:   customer.Subject,
		Author:    s.authorName(ctx, tx, msg.AuthorUserID, author, "Support"),
		Excerpt:   msg.Body,
		TicketURL: "/tickets/" + ticketID,
		MessageID: dedupeMessageKey(msg),
	}); err != nil {
		return fmt.Errorf("tickets: notify customer of staff reply: %w", err)
	}
	return nil
}

// NotifyCustomerReply tells the assignee that the customer answered.
//
// An unassigned ticket notifies nobody on purpose: it is in the queue, which is where an
// unowned ticket is supposed to be visible, and mailing every agent about every unclaimed
// reply is how a shared mailbox becomes noise.
func (s *Store) NotifyCustomerReply(ctx context.Context, tx pgx.Tx, ticketID string, msg Message, author string) error {
	if tx == nil {
		return nil
	}
	_, assignee, err := s.replyParticipants(ctx, tx, ticketID)
	if err != nil {
		return err
	}
	if assignee.UserID == "" || assignee.Email == "" {
		return nil
	}
	if _, err := notify.TicketMessage(ctx, tx, notify.NewQueue(s.pool), notify.TicketMessageInput{
		UserID:    assignee.UserID,
		Email:     assignee.Email,
		TicketID:  ticketID,
		Reference: assignee.Number,
		Subject:   assignee.Subject,
		Author:    s.authorName(ctx, tx, msg.AuthorUserID, author, "Customer"),
		Excerpt:   msg.Body,
		TicketURL: "/support/" + ticketID,
		MessageID: dedupeMessageKey(msg),
	}); err != nil {
		return fmt.Errorf("tickets: notify assignee of customer reply: %w", err)
	}
	return nil
}

// dedupeMessageKey is the outbox dedupe key for one reply: the email Message-ID when the
// reply arrived by mail, otherwise the stored message row's id.
func dedupeMessageKey(msg Message) string {
	if msg.MessageID != "" {
		return msg.MessageID
	}
	return msg.ID
}

// authorName renders who wrote the reply, for the sentence "X replied to your ticket".
//
// A piped reply has no author user, so the fallback is used: the message's From header is not
// an identity this package trusts, and a display name is not worth a spoofed one.
func (s *Store) authorName(ctx context.Context, tx pgx.Tx, userID, hint, fallback string) string {
	if hint != "" {
		return hint
	}
	if userID == "" {
		return fallback
	}
	var name string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(display_name, ''), email) FROM users WHERE id = $1`, userID).
		Scan(&name)
	if err != nil || name == "" {
		return fallback
	}
	return name
}
