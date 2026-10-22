// Support tickets: the customer side.
//
// The permission pair ticket:read:self / ticket:write:self has been granted to
// customers since the first auth migration. Until this file existed it gated
// nothing; now it gates these routes, and the actual scoping is done in
// internal/tickets with the customer id resolved from the session.
//
// Two rules run through every handler here:
//
//  1. The customer id comes from the session, never from the request. A ticket id in
//     the path is only ever a *filter* on a query that already knows whose rows it
//     may see, so substituting another customer's id yields 404, not 403. A 403
//     would confirm that the id exists.
//  2. Every error is application/problem+json with a stable code, because the
//     frontend branches on the code and not on the prose.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("55-tickets", registerTicketRoutes)
}

// ticketStore returns the ticket store.
//
// It is constructed per call rather than cached on the Server: the value is a pool
// handle and a clock, so caching it would add a field and a mutex to a shared struct
// for no measurable gain, and this package deliberately does not edit Server.
func (s *Server) ticketStore() *tickets.Store { return tickets.New(s.pool) }

func registerTicketRoutes(r *router) {
	// Customer.
	r.get("/api/v1/portal/tickets", authn.PermTicketReadSelf, r.s.handlePortalTickets)
	r.get("/api/v1/portal/tickets/meta", authn.PermTicketReadSelf, r.s.handlePortalTicketMeta)
	r.get("/api/v1/portal/tickets/{id}", authn.PermTicketReadSelf, r.s.handlePortalTicket)
	r.mutation(http.MethodPost, "/api/v1/portal/tickets",
		authn.PermTicketWriteSelf, r.s.handleCreateTicket)
	r.mutation(http.MethodPost, "/api/v1/portal/tickets/{id}/replies",
		authn.PermTicketWriteSelf, r.s.handlePortalTicketReply)
	r.mutation(http.MethodPost, "/api/v1/portal/tickets/{id}/close",
		authn.PermTicketWriteSelf, r.s.handlePortalTicketClose)

	// Attachment upload and download are registered in ticket_attachments.go: the
	// upload deliberately does not use the idempotency wrapper (a 10 MB body cannot
	// be buffered by the replay store), and the download is a plain read.
}

// requireTickets writes 503 when the ticket store has no database.
func (s *Server) requireTickets(w http.ResponseWriter) bool {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "tickets_unavailable",
			"support tickets are not configured")
		return false
	}
	return true
}

// customerFor resolves the caller's billing account.
//
// A caller with no customer record (a staff member using the portal, or an account
// that has never bought anything) gets the same 404 as a ticket that does not exist:
// there is nothing here for them, and saying "you have no customer record" is an
// implementation detail.
func (s *Server) customerFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable",
			"the portal is not configured")
		return "", false
	}
	user, _ := userFrom(r.Context())
	customerID, err := s.portal.CustomerIDFor(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, portal.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return "", false
		}
		s.ticketsError(w, r, err)
		return "", false
	}
	return customerID, true
}

// ---------------------------------------------------------------- wire shapes --

// attachmentJSON is one attachment, with the URL that serves it.
func attachmentJSON(a tickets.Attachment, staff bool) map[string]any {
	base := "/api/v1/portal/tickets/attachments/"
	if staff {
		base = "/api/v1/staff/tickets/attachments/"
	}
	return map[string]any{
		"id":           a.ID,
		"filename":     a.Filename,
		"content_type": a.ContentType,
		"byte_size":    a.ByteSize,
		"digest":       a.Digest,
		"created_at":   a.CreatedAt.UTC(),
		"url":          base + a.ID,
	}
}

// messageJSON is one message in a thread.
func messageJSON(m tickets.Message, staff bool) map[string]any {
	attachments := make([]map[string]any, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		attachments = append(attachments, attachmentJSON(a, staff))
	}
	out := map[string]any{
		"id":          m.ID,
		"ticket_id":   m.TicketID,
		"from_staff":  m.FromStaff,
		"body":        m.Body,
		"created_at":  m.CreatedAt.UTC(),
		"attachments": attachments,
		"author": map[string]any{
			"id":   m.AuthorUserID,
			"name": m.AuthorName,
		},
	}
	if m.MessageID != "" {
		out["message_id"] = m.MessageID
	}
	return out
}

// ticketJSON is one ticket row.
//
// It is the same shape for both audiences; the staff version adds the customer and
// the assignee name, because a queue without "who is this" is unusable. The
// customer's own view must never include another customer's identity, and it cannot:
// every field here is either the ticket's own or the customer's own.
func ticketJSON(t tickets.Ticket) map[string]any {
	out := map[string]any{
		"id":            t.ID,
		"number":        t.Number,
		"subject":       t.Subject,
		"status":        t.Status,
		"priority":      t.Priority,
		"department":    t.Department,
		"message_count": t.MessageCount,
		"created_at":    t.CreatedAt.UTC(),
		"updated_at":    t.UpdatedAt.UTC(),
	}
	if t.ServiceID != "" {
		out["service_id"] = t.ServiceID
	}
	if t.AssignedTo != "" {
		out["assigned_to"] = t.AssignedTo
	}
	if t.LastReplyAt != nil {
		out["last_reply_at"] = t.LastReplyAt.UTC()
	}
	if t.ClosedAt != nil {
		out["closed_at"] = t.ClosedAt.UTC()
	}
	if t.LastMessageFromStaff != nil {
		out["last_message"] = map[string]any{
			"from_staff": *t.LastMessageFromStaff,
			"body":       t.LastMessageBody,
		}
	}
	return out
}

// staffTicketJSON adds the customer and the assignee's name.
func staffTicketJSON(t tickets.Ticket) map[string]any {
	out := ticketJSON(t)
	out["customer"] = map[string]any{
		"id":    t.CustomerID,
		"email": t.CustomerEmail,
		"name":  t.CustomerName,
	}
	if t.AssignedTo != "" {
		out["assignee_name"] = t.AssigneeName
	}
	return out
}

// threadJSON is a ticket and its messages.
func threadJSON(th tickets.Thread, staff bool) map[string]any {
	row := ticketJSON(th.Ticket)
	if staff {
		row = staffTicketJSON(th.Ticket)
	}
	messages := make([]map[string]any, 0, len(th.Messages))
	for _, m := range th.Messages {
		messages = append(messages, messageJSON(m, staff))
	}
	return map[string]any{
		"ticket":   row,
		"messages": messages,
	}
}

// ---------------------------------------------------------------- customer --

// handlePortalTickets lists the caller's tickets.
//
// Query: status=open|replied|closed|all, limit, cursor.
func (s *Server) handlePortalTickets(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))

	rows, next, err := s.ticketStore().ListForCustomer(r.Context(), customerID, status, limit, cursor)
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, ticketJSON(row))
	}
	body := map[string]any{"tickets": out}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

// handlePortalTicketMeta is what the new-ticket form needs before it can be drawn:
// the configured departments, the fixed priority list, and whether the operator has
// switched the feature off.
func (s *Server) handlePortalTicketMeta(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	settings, err := s.ticketStore().Settings(r.Context())
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                 !settings.Disabled,
		"client_closing_disabled": settings.ClientClosingDisabled,
		"departments":             settings.Departments,
		"priorities":              tickets.Priorities,
		"statuses":                tickets.Statuses,
		"max_attachment_bytes":    tickets.MaxAttachmentBytes,
		"max_subject_length":      tickets.MaxSubjectLen,
	})
}

// handlePortalTicket serves one thread.
func (s *Server) handlePortalTicket(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	thread, err := s.ticketStore().ThreadForCustomer(r.Context(), customerID, r.PathValue("id"))
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, threadJSON(thread, false))
}

// createTicketRequest is the body of a new ticket.
//
// There is no customer id: it comes from the session. A caller cannot open a ticket
// on someone else's account.
type createTicketRequest struct {
	Subject    string `json:"subject"`
	Department string `json:"department"`
	Priority   string `json:"priority"`
	ServiceID  string `json:"service_id"`
	// Message is the first message. It is a nested object rather than a bare `body`
	// so the shape matches a reply, and so a future field (an email Message-ID from a
	// piped ticket) has somewhere to live.
	Message messageBody `json:"message"`
}

type messageBody struct {
	Body string `json:"body"`
	// MessageID is an email Message-ID. A duplicate is ignored rather than refused,
	// which is what makes a retried email delivery harmless.
	MessageID string `json:"message_id"`
}

// handleCreateTicket opens a ticket.
func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	var req createTicketRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	var thread tickets.Thread
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		thread, err = s.ticketStore().CreateTx(ctx, tx, tickets.CreateInput{
			CustomerID: customerID,
			UserID:     user.ID,
			ServiceID:  strings.TrimSpace(req.ServiceID),
			Subject:    req.Subject,
			Department: req.Department,
			Priority:   req.Priority,
			Body:       req.Message.Body,
			MessageID:  req.Message.MessageID,
		})
		if err != nil {
			return err
		}
		if thread.Duplicate {
			// A redelivered email created nothing, so there is nothing to audit: the
			// first delivery already recorded the creation.
			return nil
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.create", "ticket", thread.Ticket.ID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"number":     thread.Ticket.Number,
			"department": thread.Ticket.Department,
			"priority":   thread.Ticket.Priority,
			"service_id": thread.Ticket.ServiceID,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}

	if thread.Duplicate {
		// A redelivered email: the ticket already exists, so this is a success that
		// created nothing, not a second ticket. 200 rather than 201 says so.
		body := threadJSON(thread, false)
		body["duplicate"] = true
		writeJSON(w, http.StatusOK, body)
		return
	}

	s.publish(events.Event{Kind: "ticket.created", CustomerID: customerID,
		Permission: string(authn.PermTicketReadAll),
		Data: map[string]any{
			"ticket_id": thread.Ticket.ID,
			"number":    thread.Ticket.Number,
			"subject":   thread.Ticket.Subject,
		}})
	writeJSON(w, http.StatusCreated, threadJSON(thread, false))
}

// replyTicketRequest is the body of a customer or staff reply.
type replyTicketRequest struct {
	Body      string `json:"body"`
	MessageID string `json:"message_id"`
}

// handlePortalTicketReply appends a customer reply.
func (s *Server) handlePortalTicketReply(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	var req replyTicketRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())
	ticketID := r.PathValue("id")

	var (
		msg       tickets.Message
		duplicate bool
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		msg, duplicate, err = s.ticketStore().ReplyAsCustomerTx(ctx, tx, customerID, ticketID,
			tickets.ReplyInput{UserID: user.ID, Body: req.Body, MessageID: req.MessageID})
		if err != nil {
			return err
		}
		if duplicate {
			// A replayed email delivery is not a new message, so it is not an audit
			// event either: the first import already recorded it.
			return nil
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.reply", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"message_id": msg.ID,
			"from_staff": false,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}

	if !duplicate {
		s.publish(events.Event{Kind: "ticket.replied", CustomerID: customerID,
			Permission: string(authn.PermTicketReadAll),
			Data:       map[string]any{"ticket_id": ticketID, "message_id": msg.ID}})
	}
	status := http.StatusCreated
	body := messageJSON(msg, false)
	if duplicate {
		status = http.StatusOK
		body["duplicate"] = true
	}
	writeJSON(w, status, body)
}

// handlePortalTicketClose closes the caller's own ticket.
func (s *Server) handlePortalTicketClose(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	ticketID := r.PathValue("id")

	var ticket tickets.Ticket
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = s.ticketStore().CloseAsCustomerTx(ctx, tx, customerID, ticketID)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.close", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{"by": "customer"}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	s.publish(events.Event{Kind: "ticket.closed", CustomerID: customerID,
		Permission: string(authn.PermTicketReadAll),
		Data:       map[string]any{"ticket_id": ticketID}})
	writeJSON(w, http.StatusOK, ticketJSON(ticket))
}

// ---------------------------------------------------------------- errors --

// ticketsError maps a domain error to a stable code. A client branches on the code,
// so the mapping lives in one place rather than in each handler.
func (s *Server) ticketsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tickets.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, tickets.ErrDisabled):
		writeProblem(w, http.StatusForbidden, "tickets_disabled",
			"support tickets are disabled on this installation")
	case errors.Is(err, tickets.ErrClosingDisabled):
		writeProblem(w, http.StatusForbidden, "client_closing_disabled",
			"customers may not close tickets; ask support to close it")
	case errors.Is(err, tickets.ErrDuplicate):
		// A Message-ID that was imported by a concurrent delivery between the check and
		// the insert. The caller's retry will find the existing thread; answering 409
		// with a stable code is honest about what happened.
		writeProblem(w, http.StatusConflict, "duplicate_message",
			"this message has already been imported")
	case errors.Is(err, tickets.ErrAttachmentTooLarge):
		writeProblem(w, http.StatusRequestEntityTooLarge, "attachment_too_large",
			"the attachment exceeds 10 MB")
	case errors.Is(err, tickets.ErrInvalidInput):
		writeProblem(w, http.StatusBadRequest, "invalid_input", invalidInputDetail(err))
	case errors.Is(err, paginate.ErrInvalidCursor):
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "ticket request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// invalidInputDetail strips the package prefix from a validation error so the detail
// reads as a sentence rather than as a log line. It never contains a database
// message: ErrInvalidInput is only produced by the validation in internal/tickets.
func invalidInputDetail(err error) string {
	detail := strings.TrimPrefix(err.Error(), "tickets: invalid input: ")
	detail = strings.TrimPrefix(detail, "tickets: ")
	if detail == "" {
		return "the request is invalid"
	}
	return detail
}
