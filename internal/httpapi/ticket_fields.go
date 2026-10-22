// Support tickets: the custom fields an operator configures.
//
// Two audiences share these routes and they need different halves of the same definition:
//
//   - The settings screen (`/ticket-fields`) needs the definitions — what a ticket asks for, what
//     is required to open it, and what is required before it can be closed.
//   - The ticket screens need the definitions *and* one ticket's answers, because "required to
//     close" is only useful if the thread shows which field is still blank.
//
// The routes are staff-only for the definitions (configuring what the desk asks is a policy
// decision, not something a customer does) and split by audience for the answers: a customer
// answers the fields on their own ticket, a staff member answers them on anyone's.
//
// Archiving is the delete. A definition that tickets point at is not deletable — the answers are
// part of a closed ticket's record — so `DELETE` retires it: it stops being asked, it stops being
// required, and the answers already given stay readable. That is what an operator means by
// "remove this field", and it is the only meaning that does not lose history.
package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("58-ticket-fields", registerTicketFieldRoutes)
}

func registerTicketFieldRoutes(r *router) {
	// The definitions.
	r.staffGet("/api/v1/staff/ticket-fields", authn.PermTicketReadAll, r.s.handleTicketFields)
	r.staffMutation(http.MethodPost, "/api/v1/staff/ticket-fields",
		authn.PermTicketWriteAll, r.s.handleTicketFieldCreate)
	r.staffMutation(http.MethodPatch, "/api/v1/staff/ticket-fields/{id}",
		authn.PermTicketWriteAll, r.s.handleTicketFieldUpdate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/ticket-fields/{id}",
		authn.PermTicketWriteAll, r.s.handleTicketFieldArchive)

	// One ticket's answers. A customer may answer the fields on their own ticket; the store
	// scopes the write, so a substituted ticket id is not_found rather than forbidden.
	r.add(Route{
		Method:     http.MethodPut,
		Pattern:    "/api/v1/portal/tickets/{id}/fields",
		Access:     AccessPermission,
		Permission: authn.PermTicketWriteSelf,
		CSRF:       true,
		Rate:       RateMutation,
		Handler:    r.s.handlePortalTicketFields,
	})
	r.staffMutation(http.MethodPut, "/api/v1/staff/tickets/{id}/fields",
		authn.PermTicketWriteAll, r.s.handleStaffTicketFields)
}

// ---------------------------------------------------------------- wire shapes --

// ticketFieldJSON is one definition as the settings screen and the ticket form read it.
func ticketFieldJSON(f tickets.Field) map[string]any {
	options := f.Options
	if options == nil {
		options = []string{}
	}
	return map[string]any{
		"id":                 f.ID,
		"key":                f.Key,
		"label":              f.Label,
		"type":               f.Type,
		"options":            options,
		"required_on_create": f.RequiredOnCreate,
		"required_to_close":  f.RequiredToClose,
		"sort_order":         f.SortOrder,
		"archived":           f.Archived,
		"answer_count":       f.AnswerCount,
		"created_at":         f.CreatedAt.UTC(),
		"updated_at":         f.UpdatedAt.UTC(),
	}
}

// ticketFieldAnswerJSON is a definition plus this ticket's answer.
//
// `value` is a string for text and select and a boolean for checkbox, and `null` when the field is
// unanswered — which is a different thing from "" and from false, and the reason the value column
// is typed rather than a JSON blob.
func ticketFieldAnswerJSON(a tickets.FieldAnswer) map[string]any {
	out := ticketFieldJSON(a.Field)
	out["answered"] = a.Answered
	out["value"] = a.Value
	if a.AnsweredAt != nil {
		out["answered_at"] = a.AnsweredAt.UTC()
	}
	return out
}

// fieldAnswersJSON renders a ticket's fields.
func fieldAnswersJSON(answers []tickets.FieldAnswer) []map[string]any {
	out := make([]map[string]any, 0, len(answers))
	for _, answer := range answers {
		out = append(out, ticketFieldAnswerJSON(answer))
	}
	return out
}

// fieldInputFrom reads a create/update body. Every optional field is a pointer so "leave it
// alone" and "set it to false" are distinguishable.
type fieldBody struct {
	Key              string   `json:"key"`
	Label            string   `json:"label"`
	Type             string   `json:"type"`
	Options          []string `json:"options"`
	RequiredOnCreate *bool    `json:"required_on_create"`
	RequiredToClose  *bool    `json:"required_to_close"`
	SortOrder        *int     `json:"sort_order"`
	Archived         *bool    `json:"archived"`
}

func (b fieldBody) input() tickets.FieldInput {
	return tickets.FieldInput{
		Key:              b.Key,
		Label:            b.Label,
		Type:             b.Type,
		Options:          b.Options,
		RequiredOnCreate: b.RequiredOnCreate,
		RequiredToClose:  b.RequiredToClose,
		SortOrder:        b.SortOrder,
		Archived:         b.Archived,
	}
}

// fieldValuesBody is the body of an answer write.
//
// `values` is keyed by field key and is deliberately a map rather than a list: a client that
// answers one field on a ticket with nine must not have to resend the other eight, and a map
// makes "omit this field" mean "leave it alone" without a per-item flag.
type fieldValuesBody struct {
	Values map[string]any `json:"values"`
}

// ---------------------------------------------------------------- definitions --

// handleTicketFields lists the configured fields.
//
// Query: include_archived=true for the settings screen's full history.
func (s *Server) handleTicketFields(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	includeArchived := strings.TrimSpace(r.URL.Query().Get("include_archived")) == "true"
	fields, err := s.ticketStore().Fields(r.Context(), includeArchived)
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		out = append(out, ticketFieldJSON(field))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": out,
		// The closed set, so the settings screen's type picker is drawn from the server rather
		// than from a list in the frontend that could drift.
		"types": tickets.FieldTypes,
	})
}

// handleTicketFieldCreate adds a field.
func (s *Server) handleTicketFieldCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var body fieldBody
	if !s.decodeJSON(w, r, &body, 32<<10) {
		return
	}

	var field tickets.Field
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		field, err = s.ticketStore().CreateFieldTx(ctx, tx, body.input())
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.field_create", "ticket_field", field.ID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"key":                field.Key,
			"type":               field.Type,
			"required_on_create": field.RequiredOnCreate,
			"required_to_close":  field.RequiredToClose,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ticketFieldJSON(field))
}

// handleTicketFieldUpdate changes a definition.
func (s *Server) handleTicketFieldUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var body fieldBody
	if !s.decodeJSON(w, r, &body, 32<<10) {
		return
	}
	id := r.PathValue("id")

	var field tickets.Field
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		field, err = s.ticketStore().UpdateFieldTx(ctx, tx, id, body.input())
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.field_update", "ticket_field", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"key":                field.Key,
			"label":              field.Label,
			"required_on_create": field.RequiredOnCreate,
			"required_to_close":  field.RequiredToClose,
			"archived":           field.Archived,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ticketFieldJSON(field))
}

// handleTicketFieldArchive retires a field. It is the DELETE the settings screen offers, and it
// keeps every answer already given.
func (s *Server) handleTicketFieldArchive(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	id := r.PathValue("id")

	var field tickets.Field
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		field, err = s.ticketStore().ArchiveFieldTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.field_archive", "ticket_field", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{"key": field.Key}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ticketFieldJSON(field))
}

// ------------------------------------------------------------------- answers --

// handlePortalTicketFields answers fields on the caller's own ticket.
func (s *Server) handlePortalTicketFields(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	var body fieldValuesBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	if len(body.Values) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_input", "send at least one field value")
		return
	}
	ticketID := r.PathValue("id")

	var answers []tickets.FieldAnswer
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		answers, err = s.ticketStore().SetValuesAsCustomerTx(ctx, tx, customerID, ticketID, body.Values)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.fields", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"fields": fieldKeys(body.Values),
			"by":     "customer",
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": fieldAnswersJSON(answers)})
}

// handleStaffTicketFields answers fields on any ticket.
func (s *Server) handleStaffTicketFields(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	var body fieldValuesBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	if len(body.Values) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_input", "send at least one field value")
		return
	}
	ticketID := r.PathValue("id")

	var answers []tickets.FieldAnswer
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		answers, err = s.ticketStore().SetValuesAsStaffTx(ctx, tx, ticketID, body.Values)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.fields", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"fields": fieldKeys(body.Values),
			"by":     "staff",
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": fieldAnswersJSON(answers)})
}

// fieldKeys is the sorted list of keys a request touched, for the audit row. The values are not
// recorded: an audit trail that copies a customer's answer is a second place it has to be
// protected.
func fieldKeys(values map[string]any) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	// A stable order keeps two identical requests' audit rows identical.
	sort.Strings(out)
	return out
}
