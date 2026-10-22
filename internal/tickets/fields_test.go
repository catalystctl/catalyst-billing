package tickets_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/tickets"
	"github.com/jackc/pgx/v5"
)

// Custom fields: the two requirements, and the storage rules that make them enforceable.
//
// The tests here are about *semantics*, because the semantics are the feature: a field that is
// required at creation must refuse a ticket without it, a field that is required to close must
// refuse a close without it and name itself, and neither requirement may be satisfied by an empty
// answer. The last one is the reason the value table has a CHECK instead of a JSON blob: "" and
// false and null are three spellings of "nothing", and only the database can rule them all out.

// boolPtr is a helper for the optional flags on a field definition.
func boolPtr(v bool) *bool { return &v }

// intPtr is a helper for the optional sort order.
func intPtr(v int) *int { return &v }

// createField adds a definition through the store.
func (f *fixture) createField(t *testing.T, in tickets.FieldInput) tickets.Field {
	t.Helper()
	var out tickets.Field
	err := f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.CreateFieldTx(ctx, tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("create field %q: %v", in.Key, err)
	}
	return out
}

// answer writes answers to a ticket as the customer who owns it.
func (f *fixture) answer(t *testing.T, customerID, ticketID string, values map[string]any) []tickets.FieldAnswer {
	t.Helper()
	var out []tickets.FieldAnswer
	err := f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.SetValuesAsCustomerTx(ctx, tx, customerID, ticketID, values)
		return err
	})
	if err != nil {
		t.Fatalf("answer fields: %v", err)
	}
	return out
}

// ------------------------------------------------------------------ the definitions --

// A field is text, select or checkbox, and the option list is refused for the types that do not
// have one — an option list on a text field is a value nobody reads.
func TestFieldTypesAndOptions(t *testing.T) {
	f := newFixture(t)

	text := f.createField(t, tickets.FieldInput{Key: "server_name", Label: "Server name", Type: tickets.FieldText})
	if text.Type != tickets.FieldText || text.Options != nil {
		t.Fatalf("text field = %+v", text)
	}

	choice := f.createField(t, tickets.FieldInput{
		Key: "region", Label: "Region", Type: tickets.FieldSelect,
		Options: []string{"eu-west", "us-east"},
	})
	if len(choice.Options) != 2 || choice.Options[0] != "eu-west" {
		t.Fatalf("select options = %+v", choice.Options)
	}

	flag := f.createField(t, tickets.FieldInput{Key: "confirmed", Label: "Confirmed", Type: tickets.FieldCheckbox})
	if flag.Type != tickets.FieldCheckbox {
		t.Fatalf("checkbox field = %+v", flag)
	}

	// A select with no options is a question with no answers.
	var err error
	err = f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.CreateFieldTx(ctx, tx, tickets.FieldInput{
			Key: "empty_select", Label: "Empty", Type: tickets.FieldSelect,
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("select without options = %v, want ErrInvalidInput", err)
	}

	// Options on a text field are refused rather than ignored.
	err = f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.CreateFieldTx(ctx, tx, tickets.FieldInput{
			Key: "text_with_options", Label: "Text", Type: tickets.FieldText,
			Options: []string{"a"},
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("text field with options = %v, want ErrInvalidInput", err)
	}

	// A duplicate key is a sentence, not a unique-violation 500.
	err = f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.CreateFieldTx(ctx, tx, tickets.FieldInput{
			Key: "server_name", Label: "Again", Type: tickets.FieldText,
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate key = %v, want an ErrInvalidInput naming the collision", err)
	}

	// A key that collides with a ticket column would be ambiguous on the wire.
	err = f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.CreateFieldTx(ctx, tx, tickets.FieldInput{
			Key: "subject", Label: "Subject again", Type: tickets.FieldText,
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("reserved key = %v, want ErrInvalidInput", err)
	}
}

// ---------------------------------------------------------- required at creation --

// A field required at creation refuses a ticket that does not answer it, and the refusal names
// the field rather than the word "required".
func TestRequiredOnCreateRefusesTicket(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createField(t, tickets.FieldInput{
		Key: "server_name", Label: "Server name", Type: tickets.FieldText,
		RequiredOnCreate: boolPtr(true),
	})

	_, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, Subject: "no answer",
		Department: "general", Priority: "normal", Body: "help",
	})
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("create without the required field = %v, want ErrRequiredField", err)
	}
	var required *tickets.RequiredFieldError
	if !errors.As(err, &required) {
		t.Fatalf("error is not a *RequiredFieldError: %v", err)
	}
	if len(required.Fields) != 1 || required.Fields[0].Key != "server_name" {
		t.Fatalf("refusal names %+v, want server_name", required.Fields)
	}
	if !strings.Contains(err.Error(), `"Server name"`) {
		t.Fatalf("refusal %q does not name the field", err)
	}

	// An empty string is not an answer.
	_, err = f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, Subject: "blank answer",
		Department: "general", Priority: "normal", Body: "help",
		FieldValues: map[string]any{"server_name": "   "},
	})
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("blank answer = %v, want ErrRequiredField", err)
	}

	// A real answer is accepted, and the thread carries it.
	thread, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, Subject: "answered",
		Department: "general", Priority: "normal", Body: "help",
		FieldValues: map[string]any{"server_name": "web-01"},
	})
	if err != nil {
		t.Fatalf("create with the answer: %v", err)
	}
	var answered *tickets.FieldAnswer
	for i := range thread.Fields {
		if thread.Fields[i].Key == "server_name" {
			answered = &thread.Fields[i]
		}
	}
	if answered == nil || !answered.Answered || answered.Value != "web-01" {
		t.Fatalf("thread fields = %+v, want server_name answered", thread.Fields)
	}
}

// An optional field is optional, and an answer of the wrong type is refused with the field named.
func TestFieldValueValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createField(t, tickets.FieldInput{
		Key: "region", Label: "Region", Type: tickets.FieldSelect,
		Options: []string{"eu-west", "us-east"},
	})
	f.createField(t, tickets.FieldInput{Key: "confirmed", Label: "Confirmed", Type: tickets.FieldCheckbox})
	f.createField(t, tickets.FieldInput{Key: "notes", Label: "Notes", Type: tickets.FieldText})

	thread := f.open(t, f.alice, "optional fields")

	// Nothing answered is fine: none of them is required.
	if len(thread.Fields) != 3 {
		t.Fatalf("thread fields = %d, want 3 definitions", len(thread.Fields))
	}
	for _, field := range thread.Fields {
		if field.Answered {
			t.Fatalf("field %q is answered on a fresh ticket", field.Key)
		}
	}

	// A select value outside the options is refused.
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.SetValuesAsCustomerTx(ctx, tx, f.alice, thread.Ticket.ID,
			map[string]any{"region": "mars"})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) || !strings.Contains(err.Error(), `"Region"`) {
		t.Fatalf("unknown option = %v, want ErrInvalidInput naming the field", err)
	}

	// So is a boolean sent to a text field, and a string sent to a checkbox.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.SetValuesAsCustomerTx(ctx, tx, f.alice, thread.Ticket.ID,
			map[string]any{"notes": true})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("bool to text = %v, want ErrInvalidInput", err)
	}
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.SetValuesAsCustomerTx(ctx, tx, f.alice, thread.Ticket.ID,
			map[string]any{"confirmed": "yes"})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("string to checkbox = %v, want ErrInvalidInput", err)
	}

	// An unknown key is a caller error, not a silent no-op.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.SetValuesAsCustomerTx(ctx, tx, f.alice, thread.Ticket.ID,
			map[string]any{"nope": "x"})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("unknown key = %v, want ErrInvalidInput", err)
	}

	// A checkbox answered false is an answer, not a gap: `false` is a value, which is exactly
	// what a JSON blob would have got wrong.
	answers := f.answer(t, f.alice, thread.Ticket.ID, map[string]any{"confirmed": false, "region": "us-east"})
	for _, answer := range answers {
		switch answer.Key {
		case "confirmed":
			if !answer.Answered || answer.Value != false {
				t.Fatalf("checkbox answer = %+v, want answered false", answer)
			}
		case "region":
			if !answer.Answered || answer.Value != "us-east" {
				t.Fatalf("select answer = %+v", answer)
			}
		}
	}

	// A null clears an answer again.
	answers = f.answer(t, f.alice, thread.Ticket.ID, map[string]any{"region": nil})
	for _, answer := range answers {
		if answer.Key == "region" && answer.Answered {
			t.Fatalf("cleared field is still answered: %+v", answer)
		}
	}

	// Another customer's ticket is not found, exactly as for a read.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.SetValuesAsCustomerTx(ctx, tx, f.bob, thread.Ticket.ID,
			map[string]any{"notes": "injected"})
		return e
	})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("cross-customer answer = %v, want ErrNotFound", err)
	}
}

// ------------------------------------------------------------ required to close --

// Closing is refused while a close-required field is unanswered, the refusal names the field, and
// answering it makes the close work — for the customer and for staff, because a rule the person
// closing the ticket can ignore is not a rule.
func TestRequiredToCloseRefusesClose(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createField(t, tickets.FieldInput{
		Key: "resolution", Label: "Root cause", Type: tickets.FieldText,
		RequiredToClose: boolPtr(true),
	})

	thread := f.open(t, f.alice, "closable")

	_, err := f.svc.CloseAsCustomer(ctx, f.alice, thread.Ticket.ID)
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("close without the field = %v, want ErrRequiredField", err)
	}
	var required *tickets.RequiredFieldError
	if !errors.As(err, &required) || !required.OnClose {
		t.Fatalf("error = %v, want a close-time *RequiredFieldError", err)
	}
	if !strings.Contains(err.Error(), "before this ticket can be closed") {
		t.Fatalf("refusal %q does not explain itself", err)
	}
	if !strings.Contains(err.Error(), `"Root cause"`) {
		t.Fatalf("refusal %q does not name the field", err)
	}

	// The refusal changed nothing.
	after, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Ticket.Status != "open" || after.Ticket.ClosedAt != nil {
		t.Fatalf("ticket after a refused close = %+v, want still open", after.Ticket)
	}

	// Staff cannot bypass it either.
	_, err = f.svc.CloseAsStaff(ctx, thread.Ticket.ID)
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("staff close without the field = %v, want ErrRequiredField", err)
	}
	// And neither can the queue's status edit.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.UpdateStaffTx(ctx, tx, thread.Ticket.ID, tickets.StaffUpdate{Status: "closed"})
		return e
	})
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("staff PATCH to closed = %v, want ErrRequiredField", err)
	}

	// Answer it, and the close works.
	f.answer(t, f.alice, thread.Ticket.ID, map[string]any{"resolution": "a bad config line"})
	closed, err := f.svc.CloseAsCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("close after answering: %v", err)
	}
	if closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatalf("closed = %+v", closed)
	}
}

// An archived field stops being required, but the answers already given stay readable.
func TestArchivedFieldKeepsAnswersAndStopsBeingRequired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	field := f.createField(t, tickets.FieldInput{
		Key: "resolution", Label: "Root cause", Type: tickets.FieldText,
		RequiredToClose: boolPtr(true),
	})

	thread := f.open(t, f.alice, "answered then archived")
	f.answer(t, f.alice, thread.Ticket.ID, map[string]any{"resolution": "disk full"})

	// Archive it. A close is now allowed even on a ticket that never answered it.
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.ArchiveFieldTx(ctx, tx, field.ID)
		return e
	})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	live, err := f.svc.Fields(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("archived field is still offered: %+v", live)
	}

	// The answered ticket still shows its answer: a retired field is not a lost record.
	read, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, answer := range read.Fields {
		if answer.Key == "resolution" {
			found = true
			if !answer.Answered || answer.Value != "disk full" {
				t.Fatalf("archived field's answer = %+v", answer)
			}
		}
	}
	if !found {
		t.Fatal("the archived field vanished from a ticket that answered it")
	}

	// A new ticket is not asked for it and can close without it.
	fresh := f.open(t, f.alice, "fresh")
	for _, answer := range fresh.Fields {
		if answer.Key == "resolution" {
			t.Fatalf("an archived field is offered on a new ticket: %+v", answer)
		}
	}
	if _, err := f.svc.CloseAsCustomer(ctx, f.alice, fresh.Ticket.ID); err != nil {
		t.Fatalf("close with an archived requirement: %v", err)
	}
}

// A definition that tickets answer cannot change its type or lose an option, because that would
// strand the answers. The service refuses it with a sentence; the trigger is the guarantee.
func TestFieldChangeCannotStrandAnswers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	field := f.createField(t, tickets.FieldInput{
		Key: "region", Label: "Region", Type: tickets.FieldSelect,
		Options: []string{"eu-west", "us-east"},
	})

	thread := f.open(t, f.alice, "answered")
	f.answer(t, f.alice, thread.Ticket.ID, map[string]any{"region": "us-east"})

	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.UpdateFieldTx(ctx, tx, field.ID, tickets.FieldInput{
			Type:    tickets.FieldText,
			Options: []string{},
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("type change under an answer = %v, want ErrInvalidInput", err)
	}

	// Removing the answered option is refused too.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.UpdateFieldTx(ctx, tx, field.ID, tickets.FieldInput{
			Options: []string{"eu-west"},
		})
		return e
	})
	if !errors.Is(err, tickets.ErrInvalidInput) {
		t.Fatalf("option removal under an answer = %v, want ErrInvalidInput", err)
	}

	// Adding an option is allowed: it strands nothing.
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := f.svc.UpdateFieldTx(ctx, tx, field.ID, tickets.FieldInput{
			Options: []string{"eu-west", "us-east", "ap-south"},
		})
		return e
	})
	if err != nil {
		t.Fatalf("adding an option: %v", err)
	}
}

// ------------------------------------------------------------- staff-opened tickets --

// A ticket the team opens records both halves of "who opened this": the staff account, and the
// customer it was opened for.
func TestStaffOpenedTicketRecordsBothSides(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID:    f.alice,
		UserID:        f.aliceUser,
		OpenedByStaff: true,
		Subject:       "phoned in",
		Department:    "general",
		Priority:      "high",
		Body:          "the customer called",
	})
	if err != nil {
		t.Fatalf("staff create: %v", err)
	}
	if !thread.Ticket.OpenedByStaff {
		t.Fatal("the ticket does not record that staff opened it")
	}
	if thread.Ticket.OpenedBy != f.aliceUser {
		t.Fatalf("opened_by = %q, want the staff user", thread.Ticket.OpenedBy)
	}
	if thread.Ticket.CustomerID != f.alice {
		t.Fatalf("customer_id = %q, want the customer it was opened for", thread.Ticket.CustomerID)
	}
	if len(thread.Messages) != 1 || !thread.Messages[0].FromStaff {
		t.Fatalf("first message = %+v, want a staff message", thread.Messages)
	}

	// The customer sees it in their own list, and it is theirs to read.
	rows, _, err := f.svc.ListForCustomer(ctx, f.alice, "all", 25, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].OpenedByStaff {
		t.Fatalf("customer list = %+v", rows)
	}

	// A staff-opened ticket for a customer that does not exist is not found, not a 500.
	_, err = f.svc.Create(ctx, tickets.CreateInput{
		CustomerID:    "0f8fad5b-d9cb-469f-a165-70867728950e",
		UserID:        f.aliceUser,
		OpenedByStaff: true,
		Subject:       "nobody",
		Department:    "general",
		Priority:      "normal",
		Body:          "help",
	})
	if !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("staff create for an unknown customer = %v, want ErrNotFound", err)
	}
}

// The requirement a staff member sets is the requirement a staff member meets: a field required at
// creation refuses a staff-opened ticket too.
func TestRequiredOnCreateAppliesToStaffTickets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createField(t, tickets.FieldInput{
		Key: "callback_number", Label: "Callback number", Type: tickets.FieldText,
		RequiredOnCreate: boolPtr(true),
	})

	_, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, OpenedByStaff: true,
		Subject: "phoned in", Department: "general", Priority: "normal", Body: "called",
	})
	if !errors.Is(err, tickets.ErrRequiredField) {
		t.Fatalf("staff create without the required field = %v, want ErrRequiredField", err)
	}

	if _, err := f.svc.Create(ctx, tickets.CreateInput{
		CustomerID: f.alice, UserID: f.aliceUser, OpenedByStaff: true,
		Subject: "phoned in", Department: "general", Priority: "normal", Body: "called",
		FieldValues: map[string]any{"callback_number": "+44 20 7946 0000"},
	}); err != nil {
		t.Fatalf("staff create with the answer: %v", err)
	}
}

// intPtr is used by the sort-order test the API layer owns; keeping it here documents that the
// sort order is an optional int on the input.
var _ = intPtr
