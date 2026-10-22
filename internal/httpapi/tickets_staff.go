// Support tickets: the staff side.
//
// These routes are the support queue: every ticket, filtered, assignable, and
// replyable. They are unscoped on purpose — a queue that only shows one customer's
// tickets is not a queue — and the boundary is the permission instead:
// ticket:read:all to read, ticket:write:all to change anything.
//
// The support role holds both, along with customer:read:all, invoice:read:all and
// service:read:all, which is the bundle a first-line agent needs to answer "why is my
// server down". It does not hold payment:refund or service:terminate, so a ticket is
// not a route to money or to infrastructure.
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("56-ticket-staff", registerTicketStaffRoutes)
}

func registerTicketStaffRoutes(r *router) {
	r.staffGet("/api/v1/staff/tickets", authn.PermTicketReadAll, r.s.handleStaffTickets)
	r.staffGet("/api/v1/staff/tickets/meta", authn.PermTicketReadAll, r.s.handleStaffTicketMeta)
	r.staffGet("/api/v1/staff/tickets/{id}", authn.PermTicketReadAll, r.s.handleStaffTicket)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tickets/{id}/replies",
		authn.PermTicketWriteAll, r.s.handleStaffTicketReply)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tickets/{id}/close",
		authn.PermTicketWriteAll, r.s.handleStaffTicketClose)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tickets/{id}/assign",
		authn.PermTicketWriteAll, r.s.handleStaffTicketAssign)
	r.staffMutation(http.MethodPatch, "/api/v1/staff/tickets/{id}",
		authn.PermTicketWriteAll, r.s.handleStaffTicketUpdate)
}

// handleStaffTicketMeta is what the queue's filter bar and edit dialog need: the
// configured departments, the fixed priority and status lists, and the settings that
// change what a support agent may do.
func (s *Server) handleStaffTicketMeta(w http.ResponseWriter, r *http.Request) {
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
		"autoclose_days":          settings.AutoCloseDays,
		"max_attachment_bytes":    tickets.MaxAttachmentBytes,
		"max_subject_length":      tickets.MaxSubjectLen,
	})
}

// handleStaffTickets lists the queue.
//
// Query: status, priority, department, assignee (a user id, or "unassigned"), q (a
// search over the subject and the customer), limit, cursor.
func (s *Server) handleStaffTickets(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	assignee := strings.TrimSpace(query.Get("assignee"))
	if assignee == "me" {
		// The queue's default view is "assigned to me", which a client would otherwise
		// have to resolve from its own session.
		user, _ := userFrom(r.Context())
		assignee = user.ID
	}
	rows, next, err := s.ticketStore().ListForStaff(r.Context(), tickets.StaffFilter{
		Status:     strings.TrimSpace(query.Get("status")),
		Priority:   strings.TrimSpace(query.Get("priority")),
		Department: strings.TrimSpace(query.Get("department")),
		Assignee:   assignee,
		Query:      strings.TrimSpace(query.Get("q")),
	}, limit, cursor)
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, staffTicketJSON(row))
	}
	body := map[string]any{"tickets": out}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

// handleStaffTicket serves one thread.
func (s *Server) handleStaffTicket(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	thread, err := s.ticketStore().ThreadForStaff(r.Context(), r.PathValue("id"))
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, threadJSON(thread, true))
}

// handleStaffTicketReply appends a staff reply.
func (s *Server) handleStaffTicketReply(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
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
		msg, duplicate, err = s.ticketStore().ReplyAsStaffTx(ctx, tx, user.ID, ticketID,
			tickets.ReplyInput{UserID: user.ID, Body: req.Body, MessageID: req.MessageID})
		if err != nil {
			return err
		}
		if duplicate {
			return nil
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.staff_reply", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"message_id": msg.ID,
			"from_staff": true,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	if !duplicate {
		// The customer sees their own thread move, and staff see the queue change. The
		// customer-scoped event is what makes the portal update without a reload.
		s.publish(events.Event{Kind: "ticket.replied", CustomerID: "",
			Permission: string(authn.PermTicketReadAll),
			Data:       map[string]any{"ticket_id": ticketID, "message_id": msg.ID, "from_staff": true}})
	}
	status := http.StatusCreated
	body := messageJSON(msg, true)
	if duplicate {
		status = http.StatusOK
		body["duplicate"] = true
	}
	writeJSON(w, status, body)
}

// handleStaffTicketClose closes any ticket.
func (s *Server) handleStaffTicketClose(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	ticketID := r.PathValue("id")

	var ticket tickets.Ticket
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = s.ticketStore().CloseAsStaffTx(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.close", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{"by": "staff"}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	s.publish(events.Event{Kind: "ticket.closed", Permission: string(authn.PermTicketReadAll),
		Data: map[string]any{"ticket_id": ticketID}})
	writeJSON(w, http.StatusOK, staffTicketJSON(ticket))
}

// assignTicketRequest is the body of an assignment.
//
// An empty assignee unassigns. It is explicit rather than "omit the field" so that a
// client clearing an assignment cannot be confused with one that forgot to send it.
type assignTicketRequest struct {
	Assignee string `json:"assignee"`
}

// handleStaffTicketAssign assigns or unassigns a ticket.
func (s *Server) handleStaffTicketAssign(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var req assignTicketRequest
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	ticketID := r.PathValue("id")

	var ticket tickets.Ticket
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = s.ticketStore().UpdateStaffTx(ctx, tx, ticketID, tickets.StaffUpdate{
			SetAssignee: true,
			Assignee:    strings.TrimSpace(req.Assignee),
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.assign", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{"assignee": ticket.AssignedTo}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	s.publish(events.Event{Kind: "ticket.assigned", Permission: string(authn.PermTicketReadAll),
		Data: map[string]any{"ticket_id": ticketID, "assignee": ticket.AssignedTo}})
	writeJSON(w, http.StatusOK, staffTicketJSON(ticket))
}

// updateTicketRequest is the body of a queue edit. Every field is optional; an
// omitted field means "leave it alone".
type updateTicketRequest struct {
	Status     string `json:"status"`
	Priority   string `json:"priority"`
	Department string `json:"department"`
}

// handleStaffTicketUpdate changes status, priority or department.
func (s *Server) handleStaffTicketUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var req updateTicketRequest
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.Status == "" && req.Priority == "" && req.Department == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_input",
			"send at least one of status, priority or department")
		return
	}
	ticketID := r.PathValue("id")

	var ticket tickets.Ticket
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = s.ticketStore().UpdateStaffTx(ctx, tx, ticketID, tickets.StaffUpdate{
			Status:     strings.TrimSpace(req.Status),
			Priority:   strings.TrimSpace(req.Priority),
			Department: strings.TrimSpace(req.Department),
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.update", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"status":     ticket.Status,
			"priority":   ticket.Priority,
			"department": ticket.Department,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	s.publish(events.Event{Kind: "ticket.updated", Permission: string(authn.PermTicketReadAll),
		Data: map[string]any{"ticket_id": ticketID, "status": ticket.Status}})
	writeJSON(w, http.StatusOK, staffTicketJSON(ticket))
}
