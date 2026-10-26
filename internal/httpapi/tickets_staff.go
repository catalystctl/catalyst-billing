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
	// Opening a ticket *for* a customer. It is a write, so it needs ticket:write:all; the
	// customer is named in the body rather than taken from the session, which is the whole
	// point — an agent answering the phone cannot become the customer.
	r.staffMutation(http.MethodPost, "/api/v1/staff/tickets",
		authn.PermTicketWriteAll, r.s.handleStaffTicketCreate)
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
// configured departments, the fixed priority and status lists, the custom fields a new ticket
// asks for, and the settings that change what a support agent may do.
func (s *Server) handleStaffTicketMeta(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	body, err := s.ticketFormMeta(r.Context())
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// createStaffTicketRequest is the body of a ticket the team opens on a customer's behalf.
//
// `customer_id` is explicit and required: unlike the portal route there is no session customer to
// fall back to, and guessing one would be the worst possible guess. `service_id` is optional and,
// as on the portal, must belong to that customer.
type createStaffTicketRequest struct {
	CustomerID string         `json:"customer_id"`
	Subject    string         `json:"subject"`
	Department string         `json:"department"`
	Priority   string         `json:"priority"`
	ServiceID  string         `json:"service_id"`
	Message    messageBody    `json:"message"`
	Fields     map[string]any `json:"fields"`
}

// handleStaffTicketCreate opens a ticket for a named customer.
//
// The ticket records both halves of "who opened this": `opened_by` is the staff account (and
// `opened_by_staff` marks it as staff-opened), and `customer_id` is the account it was opened for.
// The first message is recorded as a staff message, because it is one — the customer's thread says
// "Support team" above it, which is the truth.
func (s *Server) handleStaffTicketCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var req createStaffTicketRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if strings.TrimSpace(req.CustomerID) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_input",
			"a customer_id is required: a ticket the team opens is opened for somebody")
		return
	}
	user, _ := userFrom(r.Context())

	var thread tickets.Thread
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		thread, err = s.ticketStore().CreateTx(ctx, tx, tickets.CreateInput{
			CustomerID:    strings.TrimSpace(req.CustomerID),
			UserID:        user.ID,
			OpenedByStaff: true,
			ServiceID:     strings.TrimSpace(req.ServiceID),
			Subject:       req.Subject,
			Department:    req.Department,
			Priority:      req.Priority,
			Body:          req.Message.Body,
			MessageID:     req.Message.MessageID,
			FieldValues:   req.Fields,
		})
		if err != nil {
			return err
		}
		if thread.Duplicate {
			return nil
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.create_on_behalf", "ticket", thread.Ticket.ID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"number":          thread.Ticket.Number,
			"customer_id":     thread.Ticket.CustomerID,
			"opened_by":       user.ID,
			"opened_by_staff": true,
			"department":      thread.Ticket.Department,
			"priority":        thread.Ticket.Priority,
			"service_id":      thread.Ticket.ServiceID,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}

	// Read the thread back in the staff shape. The store's create returns the customer-agnostic
	// row, and the queue's version of it carries the customer record — which is the first thing
	// an agent needs to see after opening a ticket for someone.
	full, err := s.ticketStore().ThreadForStaff(r.Context(), thread.Ticket.ID)
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	full.Duplicate = thread.Duplicate

	if full.Duplicate {
		body := threadJSON(full, true)
		body["duplicate"] = true
		writeJSON(w, http.StatusOK, body)
		return
	}

	s.publish(events.Event{Kind: "ticket.created", CustomerID: thread.Ticket.CustomerID,
		Permission: string(authn.PermTicketReadAll),
		Data: map[string]any{
			"ticket_id":       thread.Ticket.ID,
			"number":          thread.Ticket.Number,
			"subject":         thread.Ticket.Subject,
			"opened_by_staff": true,
		}})
	writeJSON(w, http.StatusCreated, threadJSON(full, true))
}

// handleStaffTickets lists the queue.
//
// Query: status, priority, department, assignee (a user id, or "unassigned"), customer_id (an
// exact customer id, or "all"), q (a search over the subject and the customer), limit, cursor.
//
// customer_id is the customer-detail screen's filter: it is exact and never a prefix, because a
// partial match on an id would silently mix two customers' tickets. An id that is not a UUID is
// refused rather than passed to SQL.
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
		CustomerID: strings.TrimSpace(query.Get("customer_id")),
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
