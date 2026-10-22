package tickets

// Custom ticket fields.
//
// A support desk that asks every customer the same four questions either asks too much of most
// of them or too little of some. This file is the operator's answer: a set of named fields, each
// of which may be required when a ticket is opened, required before it can be closed, or
// optional at both points.
//
// The two requirements are deliberately independent, because they answer different questions:
//
//   - *Required at creation* is about triage. "Which server?" is worth refusing a ticket over,
//     because without it the first agent reply is a question and the customer waits a day.
//   - *Required to close* is about the record. "Was this resolved?" or "What was the root cause?"
//     is worthless if it is asked at creation and never enforced afterwards, so it is enforced at
//     the only moment that matters — the close — and the refusal names the field, because "a
//     required field is missing" on a form with nine fields is a puzzle, not an error message.
//
// Storage is typed (see migration 0054): a value row carries its field's type and exactly one of
// a text or a boolean column, with a CHECK that makes an empty answer impossible. "Answered" is
// therefore the existence of a row, and the close-time requirement is a NOT EXISTS rather than a
// comparison against '', 'false' and 'null' — three spellings of "nothing" that a JSON blob would
// have made us get wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// The field types the schema accepts.
const (
	FieldText     = "text"
	FieldSelect   = "select"
	FieldCheckbox = "checkbox"
)

// FieldTypes is the closed set, in the order the settings screen offers them.
var FieldTypes = []string{FieldText, FieldSelect, FieldCheckbox}

const (
	// MaxFieldKeyLen matches the CHECK on ticket_fields.key.
	MaxFieldKeyLen = 64
	// MaxFieldLabelLen matches the CHECK on ticket_fields.label.
	MaxFieldLabelLen = 120
	// MaxFieldOptions bounds a select's option list, matching the CHECK.
	MaxFieldOptions = 50
	// MaxFieldValueLen matches the CHECK on ticket_field_values.value_text.
	MaxFieldValueLen = 2000
	// maxFieldSortOrder keeps a display order an int without letting a caller overflow it.
	maxFieldSortOrder = 1_000_000
)

// reservedFieldKeys are ticket columns, and a field that shares a name with one would be
// ambiguous on the wire. The migration's CHECK refuses them too; this is the sentence an
// operator sees instead of a SQLSTATE.
var reservedFieldKeys = map[string]struct{}{
	"subject": {}, "department": {}, "priority": {}, "status": {}, "service_id": {},
	"message": {}, "body": {}, "customer_id": {}, "assignee": {}, "attachment": {},
}

// ErrRequiredField is the sentinel behind *RequiredFieldError. A handler that only needs "was it
// a missing field" can test for this; one that needs the names uses errors.As.
var ErrRequiredField = errors.New("tickets: a required field is unanswered")

// ErrFieldNotFound is returned for an unknown field id, which the API renders as 404.
var ErrFieldNotFound = errors.New("tickets: no such ticket field")

// RequiredFieldError names the fields that must be answered. It is an error *value* rather than a
// formatted string so the refusal can name the field, which is the difference between "answer the
// required fields" and "answer Server name".
type RequiredFieldError struct {
	// Fields are the unanswered definitions, in display order.
	Fields []Field
	// OnClose is true when the requirement was the close-time one, which changes both the
	// sentence and the HTTP status the handler picks.
	OnClose bool
}

// Error renders the sentence the API returns as the problem detail. It never contains a database
// message: every name in it comes from ticket_fields.label.
func (e *RequiredFieldError) Error() string {
	names := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		names = append(names, fmt.Sprintf("%q", f.Label))
	}
	list := strings.Join(names, ", ")
	verb := "must be answered"
	if len(e.Fields) == 1 {
		verb = "must be answered"
	}
	if e.OnClose {
		if len(e.Fields) == 1 {
			return fmt.Sprintf("the field %s %s before this ticket can be closed", list, verb)
		}
		return fmt.Sprintf("the fields %s %s before this ticket can be closed", list, verb)
	}
	if len(e.Fields) == 1 {
		return fmt.Sprintf("the field %s %s to open a ticket", list, verb)
	}
	return fmt.Sprintf("the fields %s %s to open a ticket", list, verb)
}

// Unwrap makes errors.Is(err, ErrRequiredField) work.
func (e *RequiredFieldError) Unwrap() error { return ErrRequiredField }

// Field is one configured field.
type Field struct {
	ID               string
	Key              string
	Label            string
	Type             string
	Options          []string
	RequiredOnCreate bool
	RequiredToClose  bool
	SortOrder        int
	Archived         bool
	// AnswerCount is how many tickets carry an answer. It is what tells an operator that
	// archiving keeps the history rather than deleting it.
	AnswerCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FieldAnswer is a field and one ticket's answer to it.
type FieldAnswer struct {
	Field
	Answered bool
	// Value is the answer: a string for text and select, a bool for checkbox. It is nil when the
	// field is unanswered, so a caller cannot mistake "" or false for "nothing".
	Value      any
	AnsweredAt *time.Time
}

// FieldInput is a field definition being created or changed.
//
// Every field is a pointer on update so "leave it alone" and "set it to the zero value" are
// distinguishable: clearing a required flag and forgetting to send it must not look the same.
type FieldInput struct {
	Key              string
	Label            string
	Type             string
	Options          []string
	RequiredOnCreate *bool
	RequiredToClose  *bool
	SortOrder        *int
	// Archived retires or restores a field.
	Archived *bool
}

// validFieldKey reports whether key matches the CHECK on ticket_fields.key.
func validFieldKey(key string) bool {
	if len(key) < 2 || len(key) > MaxFieldKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		case c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// normalizeFieldOptions trims, refuses blanks and refuses duplicates.
func normalizeFieldOptions(options []string) ([]string, error) {
	out := make([]string, 0, len(options))
	seen := make(map[string]struct{}, len(options))
	for _, raw := range options {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("%w: an option cannot be empty", ErrInvalidInput)
		}
		if len(value) > MaxFieldLabelLen {
			return nil, fmt.Errorf("%w: an option must be at most %d characters",
				ErrInvalidInput, MaxFieldLabelLen)
		}
		if _, dup := seen[value]; dup {
			return nil, fmt.Errorf("%w: the option %q appears twice", ErrInvalidInput, value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) > MaxFieldOptions {
		return nil, fmt.Errorf("%w: a field offers at most %d options", ErrInvalidInput, MaxFieldOptions)
	}
	return out, nil
}

// validateFieldInput checks a definition before it reaches the database.
func validateFieldInput(in FieldInput) (FieldInput, error) {
	in.Key = strings.TrimSpace(in.Key)
	in.Label = strings.TrimSpace(in.Label)
	in.Type = strings.TrimSpace(in.Type)

	if in.Key != "" {
		if !validFieldKey(in.Key) {
			return in, fmt.Errorf(
				"%w: the key must be lower-case letters, digits and underscores, start with a letter, and be 2 to %d characters",
				ErrInvalidInput, MaxFieldKeyLen)
		}
		if _, reserved := reservedFieldKeys[in.Key]; reserved {
			return in, fmt.Errorf("%w: %q is a ticket field already; choose another key",
				ErrInvalidInput, in.Key)
		}
	}
	if in.Label != "" {
		if len(in.Label) > MaxFieldLabelLen {
			return in, fmt.Errorf("%w: the label must be at most %d characters",
				ErrInvalidInput, MaxFieldLabelLen)
		}
	}
	if in.Type != "" {
		valid := false
		for _, candidate := range FieldTypes {
			if in.Type == candidate {
				valid = true
				break
			}
		}
		if !valid {
			return in, fmt.Errorf("%w: unknown field type %q", ErrInvalidInput, in.Type)
		}
	}
	if in.Options != nil {
		options, err := normalizeFieldOptions(in.Options)
		if err != nil {
			return in, err
		}
		in.Options = options
		if in.Type != "" && in.Type != FieldSelect && len(options) > 0 {
			return in, fmt.Errorf("%w: only a select field has options", ErrInvalidInput)
		}
		if in.Type == FieldSelect && len(options) == 0 {
			return in, fmt.Errorf("%w: a select field needs at least one option", ErrInvalidInput)
		}
	}
	if in.SortOrder != nil {
		if *in.SortOrder < 0 || *in.SortOrder > maxFieldSortOrder {
			return in, fmt.Errorf("%w: the sort order must be between 0 and %d",
				ErrInvalidInput, maxFieldSortOrder)
		}
	}
	return in, nil
}

// ---------------------------------------------------------------- reading --

// fieldSelect is the definition row, with the count of tickets that answered it.
const fieldSelect = `
	f.id, f.key, f.label, f.field_type, f.options, f.required_on_create, f.required_to_close,
	f.sort_order, f.archived_at IS NOT NULL, f.created_at, f.updated_at,
	(SELECT count(*) FROM ticket_field_values v WHERE v.field_id = f.id)`

func scanField(sc rowScanner) (Field, error) {
	var (
		out     Field
		options []byte
	)
	err := sc.Scan(&out.ID, &out.Key, &out.Label, &out.Type, &options,
		&out.RequiredOnCreate, &out.RequiredToClose, &out.SortOrder, &out.Archived,
		&out.CreatedAt, &out.UpdatedAt, &out.AnswerCount)
	if err != nil {
		return Field{}, err
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &out.Options); err != nil {
			return Field{}, fmt.Errorf("tickets: decode field options: %w", err)
		}
	}
	return out, nil
}

// Fields lists the configured fields in display order.
//
// Archived fields are excluded unless asked for: the create form and the close check both want
// the live set, and the settings screen is the one place that wants everything.
func (s *Store) Fields(ctx context.Context, includeArchived bool) ([]Field, error) {
	return fieldsFrom(ctx, s.pool, includeArchived)
}

func fieldsFrom(ctx context.Context, q querier, includeArchived bool) ([]Field, error) {
	where := "WHERE f.archived_at IS NULL"
	if includeArchived {
		where = ""
	}
	rows, err := q.Query(ctx, `SELECT `+fieldSelect+`
		FROM ticket_fields f `+where+`
		ORDER BY f.sort_order, f.key`)
	if err != nil {
		return nil, fmt.Errorf("tickets: list fields: %w", err)
	}
	defer rows.Close()

	out := make([]Field, 0, 8)
	for rows.Next() {
		field, err := scanField(rows)
		if err != nil {
			return nil, fmt.Errorf("tickets: scan field: %w", err)
		}
		out = append(out, field)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets: read fields: %w", err)
	}
	return out, nil
}

// Field reads one definition by id.
func (s *Store) Field(ctx context.Context, id string) (Field, error) {
	return fieldFrom(ctx, s.pool, id)
}

func fieldFrom(ctx context.Context, q querier, id string) (Field, error) {
	if !validUUID(id) {
		return Field{}, ErrFieldNotFound
	}
	field, err := scanField(q.QueryRow(ctx, `SELECT `+fieldSelect+` FROM ticket_fields f WHERE f.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Field{}, ErrFieldNotFound
	}
	if err != nil {
		return Field{}, fmt.Errorf("tickets: read field: %w", err)
	}
	return field, nil
}

// --------------------------------------------------------------- writing --

// CreateFieldTx adds a definition.
func (s *Store) CreateFieldTx(ctx context.Context, tx pgx.Tx, in FieldInput) (Field, error) {
	validated, err := validateFieldInput(in)
	if err != nil {
		return Field{}, err
	}
	if validated.Key == "" {
		return Field{}, fmt.Errorf("%w: a key is required", ErrInvalidInput)
	}
	if validated.Label == "" {
		return Field{}, fmt.Errorf("%w: a label is required", ErrInvalidInput)
	}
	if validated.Type == "" {
		return Field{}, fmt.Errorf("%w: a type is required", ErrInvalidInput)
	}
	if validated.Type == FieldSelect && len(validated.Options) == 0 {
		return Field{}, fmt.Errorf("%w: a select field needs at least one option", ErrInvalidInput)
	}

	requiredOnCreate := validated.RequiredOnCreate != nil && *validated.RequiredOnCreate
	requiredToClose := validated.RequiredToClose != nil && *validated.RequiredToClose
	sortOrder := 100
	if validated.SortOrder != nil {
		sortOrder = *validated.SortOrder
	}

	var options any
	if validated.Type == FieldSelect {
		encoded, err := json.Marshal(validated.Options)
		if err != nil {
			return Field{}, fmt.Errorf("tickets: encode options: %w", err)
		}
		options = string(encoded)
	}

	id, err := scanFieldID(tx.QueryRow(ctx, `
		INSERT INTO ticket_fields (key, label, field_type, options, required_on_create,
			required_to_close, sort_order, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $8)
		RETURNING id`,
		validated.Key, validated.Label, validated.Type, options, requiredOnCreate,
		requiredToClose, sortOrder, s.clock()))
	if err != nil {
		if isUniqueViolation(err) {
			return Field{}, fmt.Errorf("%w: the key %q is already used by another field",
				ErrInvalidInput, validated.Key)
		}
		return Field{}, fmt.Errorf("tickets: create field: %w", err)
	}
	return fieldFrom(ctx, tx, id)
}

// UpdateFieldTx changes a definition.
//
// A pointer that is nil is left alone. The database refuses the two changes that would strand an
// answer — a type change and an option removal under an answered ticket — and this maps that
// refusal to a sentence an operator can act on.
func (s *Store) UpdateFieldTx(ctx context.Context, tx pgx.Tx, id string, in FieldInput) (Field, error) {
	current, err := fieldFrom(ctx, tx, id)
	if err != nil {
		return Field{}, err
	}
	validated, err := validateFieldInput(in)
	if err != nil {
		return Field{}, err
	}

	args := []any{id}
	set := "updated_at = now()"
	if validated.Label != "" {
		args = append(args, validated.Label)
		set += fmt.Sprintf(", label = $%d", len(args))
	}
	if validated.Type != "" {
		args = append(args, validated.Type)
		set += fmt.Sprintf(", field_type = $%d", len(args))
	}
	if validated.Options != nil || (validated.Type != "" && validated.Type != FieldSelect) {
		var encoded any
		if validated.Type != "" && validated.Type != FieldSelect {
			// Switching away from select must clear the options: the CHECK refuses a text
			// field that still carries a list.
			encoded = nil
		} else {
			raw, err := json.Marshal(validated.Options)
			if err != nil {
				return Field{}, fmt.Errorf("tickets: encode options: %w", err)
			}
			encoded = string(raw)
		}
		args = append(args, encoded)
		set += fmt.Sprintf(", options = $%d::jsonb", len(args))
	}
	if validated.RequiredOnCreate != nil {
		args = append(args, *validated.RequiredOnCreate)
		set += fmt.Sprintf(", required_on_create = $%d", len(args))
	}
	if validated.RequiredToClose != nil {
		args = append(args, *validated.RequiredToClose)
		set += fmt.Sprintf(", required_to_close = $%d", len(args))
	}
	if validated.SortOrder != nil {
		args = append(args, *validated.SortOrder)
		set += fmt.Sprintf(", sort_order = $%d", len(args))
	}
	if validated.Archived != nil {
		if *validated.Archived {
			set += ", archived_at = COALESCE(archived_at, now())"
		} else {
			set += ", archived_at = NULL"
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE ticket_fields SET `+set+` WHERE id = $1`, args...); err != nil {
		if storeSQLState(err) == store.StateCheckViolation {
			return Field{}, fmt.Errorf(
				"%w: tickets already answer this field, so its type and its options cannot change; add an option, or archive it and create a new field",
				ErrInvalidInput)
		}
		return Field{}, fmt.Errorf("tickets: update field: %w", err)
	}

	updated, err := fieldFrom(ctx, tx, id)
	if err != nil {
		return Field{}, err
	}
	// A select that lost its options by a type change back to select is refused by the CHECK;
	// a select that would be left with none is caught here.
	if updated.Type == FieldSelect && len(updated.Options) == 0 && current.Type != FieldSelect {
		return Field{}, fmt.Errorf("%w: a select field needs at least one option", ErrInvalidInput)
	}
	return updated, nil
}

// ArchiveFieldTx retires a definition. Answers already given are kept and stay readable.
func (s *Store) ArchiveFieldTx(ctx context.Context, tx pgx.Tx, id string) (Field, error) {
	if _, err := fieldFrom(ctx, tx, id); err != nil {
		return Field{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ticket_fields SET archived_at = COALESCE(archived_at, now()) WHERE id = $1`, id); err != nil {
		return Field{}, fmt.Errorf("tickets: archive field: %w", err)
	}
	return fieldFrom(ctx, tx, id)
}

func scanFieldID(row pgx.Row) (string, error) {
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// ----------------------------------------------------------------- values --

// valuesForTicket reads a ticket's answers, joined onto the definitions.
//
// Archived fields are included: a closed ticket's answers are the record, and a field that was
// retired last month must not make last year's ticket look as though it was never filled in.
func valuesForTicket(ctx context.Context, q querier, ticketID string) ([]FieldAnswer, error) {
	rows, err := q.Query(ctx, `
		SELECT `+fieldSelect+`, v.value_text, v.value_bool, v.answered_at
		FROM ticket_fields f
		LEFT JOIN ticket_field_values v ON v.field_id = f.id AND v.ticket_id = $1
		WHERE f.archived_at IS NULL OR v.ticket_id IS NOT NULL
		ORDER BY f.sort_order, f.key`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("tickets: read field values: %w", err)
	}
	defer rows.Close()

	out := make([]FieldAnswer, 0, 8)
	for rows.Next() {
		var (
			answer    FieldAnswer
			options   []byte
			valueText *string
			valueBool *bool
		)
		if err := rows.Scan(&answer.ID, &answer.Key, &answer.Label, &answer.Type, &options,
			&answer.RequiredOnCreate, &answer.RequiredToClose, &answer.SortOrder, &answer.Archived,
			&answer.CreatedAt, &answer.UpdatedAt, &answer.AnswerCount,
			&valueText, &valueBool, &answer.AnsweredAt); err != nil {
			return nil, fmt.Errorf("tickets: scan field value: %w", err)
		}
		if len(options) > 0 {
			if err := json.Unmarshal(options, &answer.Options); err != nil {
				return nil, fmt.Errorf("tickets: decode field options: %w", err)
			}
		}
		switch {
		case valueText != nil:
			answer.Answered = true
			answer.Value = *valueText
		case valueBool != nil:
			answer.Answered = true
			answer.Value = *valueBool
		}
		out = append(out, answer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets: read field values: %w", err)
	}
	return out, nil
}

// normalizeAnswers turns a wire map of key → value into what the database will store.
//
// The rules are the field's own: a text or select answer is a string (trimmed, non-empty), a
// checkbox answer is a boolean, and an empty string, a null or an omitted key clears the answer.
// Anything else is a caller error that names the field, because "invalid input" against a form
// with nine fields is not an error message.
func normalizeAnswers(fields []Field, values map[string]any) (map[string]any, error) {
	byKey := make(map[string]Field, len(fields))
	for _, field := range fields {
		byKey[field.Key] = field
	}
	out := make(map[string]any, len(values))
	for rawKey, rawValue := range values {
		key := strings.TrimSpace(rawKey)
		field, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("%w: there is no field called %q", ErrInvalidInput, key)
		}
		if field.Archived {
			return nil, fmt.Errorf("%w: the field %q is no longer in use", ErrInvalidInput, field.Label)
		}
		if rawValue == nil {
			out[key] = nil
			continue
		}
		switch field.Type {
		case FieldText:
			text, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("%w: %q takes text", ErrInvalidInput, field.Label)
			}
			text = strings.TrimSpace(text)
			if len(text) > MaxFieldValueLen {
				return nil, fmt.Errorf("%w: %q must be at most %d characters",
					ErrInvalidInput, field.Label, MaxFieldValueLen)
			}
			if text == "" {
				out[key] = nil
				continue
			}
			out[key] = text
		case FieldSelect:
			text, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("%w: %q takes one of its options", ErrInvalidInput, field.Label)
			}
			text = strings.TrimSpace(text)
			if text == "" {
				out[key] = nil
				continue
			}
			allowed := false
			for _, option := range field.Options {
				if option == text {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("%w: %q does not offer %q (choose one of: %s)",
					ErrInvalidInput, field.Label, text, strings.Join(field.Options, ", "))
			}
			out[key] = text
		case FieldCheckbox:
			flag, ok := rawValue.(bool)
			if !ok {
				return nil, fmt.Errorf("%w: %q is a checkbox and takes true or false",
					ErrInvalidInput, field.Label)
			}
			out[key] = flag
		default:
			return nil, fmt.Errorf("%w: unknown field type %q", ErrInvalidInput, field.Type)
		}
	}
	return out, nil
}

// SetValuesAsCustomerTx answers fields on the caller's own ticket.
func (s *Store) SetValuesAsCustomerTx(ctx context.Context, tx pgx.Tx, customerID, ticketID string, values map[string]any) ([]FieldAnswer, error) {
	if !validUUID(ticketID) || !validUUID(customerID) {
		return nil, ErrNotFound
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM tickets WHERE id = $1 AND customer_id = $2 FOR UPDATE`,
		ticketID, customerID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tickets: lock ticket: %w", err)
	}
	if err := s.setValuesTx(ctx, tx, ticketID, values); err != nil {
		return nil, err
	}
	return valuesForTicket(ctx, tx, ticketID)
}

// SetValuesAsStaffTx answers fields on any ticket. The caller holds ticket:write:all.
func (s *Store) SetValuesAsStaffTx(ctx context.Context, tx pgx.Tx, ticketID string, values map[string]any) ([]FieldAnswer, error) {
	if !validUUID(ticketID) {
		return nil, ErrNotFound
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM tickets WHERE id = $1 FOR UPDATE`, ticketID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tickets: lock ticket: %w", err)
	}
	if err := s.setValuesTx(ctx, tx, ticketID, values); err != nil {
		return nil, err
	}
	return valuesForTicket(ctx, tx, ticketID)
}

// setValuesTx writes the answers of an already-locked ticket.
func (s *Store) setValuesTx(ctx context.Context, tx pgx.Tx, ticketID string, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	fields, err := fieldsFrom(ctx, tx, true)
	if err != nil {
		return err
	}
	answers, err := normalizeAnswers(fields, values)
	if err != nil {
		return err
	}
	byKey := make(map[string]Field, len(fields))
	for _, field := range fields {
		byKey[field.Key] = field
	}
	now := s.clock()

	// Sorted so two concurrent writers that touch the same rows take their locks in one order.
	keys := make([]string, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		field := byKey[key]
		switch value := answers[key].(type) {
		case nil:
			// Clearing an answer is a DELETE, because an empty answer is the absence of a row.
			if _, err := tx.Exec(ctx,
				`DELETE FROM ticket_field_values WHERE ticket_id = $1 AND field_id = $2`,
				ticketID, field.ID); err != nil {
				return fmt.Errorf("tickets: clear field value: %w", err)
			}
		case string:
			if _, err := tx.Exec(ctx, `
				INSERT INTO ticket_field_values (ticket_id, field_id, field_type, value_text, answered_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (ticket_id, field_id) DO UPDATE
				SET field_type = EXCLUDED.field_type, value_text = EXCLUDED.value_text,
				    value_bool = NULL, answered_at = EXCLUDED.answered_at`,
				ticketID, field.ID, field.Type, value, now); err != nil {
				return fieldValueError(err, field)
			}
		case bool:
			if _, err := tx.Exec(ctx, `
				INSERT INTO ticket_field_values (ticket_id, field_id, field_type, value_bool, answered_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (ticket_id, field_id) DO UPDATE
				SET field_type = EXCLUDED.field_type, value_bool = EXCLUDED.value_bool,
				    value_text = NULL, answered_at = EXCLUDED.answered_at`,
				ticketID, field.ID, field.Type, value, now); err != nil {
				return fieldValueError(err, field)
			}
		}
	}
	return nil
}

// fieldValueError turns the trigger's refusal into a sentence. The trigger is the guarantee; this
// is the explanation, and it never quotes the SQLSTATE.
func fieldValueError(err error, field Field) error {
	if state := storeSQLState(err); state == store.StateCheckViolation || state == store.StateForeignKeyViolation {
		return fmt.Errorf("%w: %q could not be stored as sent", ErrInvalidInput, field.Label)
	}
	return fmt.Errorf("tickets: store field value: %w", err)
}

// ------------------------------------------------------- required answers --

// requiredForCreate returns the unarchived fields a new ticket must answer.
func requiredForCreate(ctx context.Context, q querier) ([]Field, error) {
	fields, err := fieldsFrom(ctx, q, false)
	if err != nil {
		return nil, err
	}
	out := make([]Field, 0, 2)
	for _, field := range fields {
		if field.RequiredOnCreate {
			out = append(out, field)
		}
	}
	return out, nil
}

// checkCreateAnswers refuses a creation that leaves a required field unanswered.
//
// The answers have already been normalized, so "answered" here means "there is a non-empty value
// in the map" — the same rule the value table enforces with a CHECK.
func checkCreateAnswers(ctx context.Context, q querier, answers map[string]any) error {
	required, err := requiredForCreate(ctx, q)
	if err != nil {
		return err
	}
	var missing []Field
	for _, field := range required {
		value, present := answers[field.Key]
		if !present || value == nil {
			missing = append(missing, field)
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return &RequiredFieldError{Fields: missing}
	}
	return nil
}

// unansweredForClose returns the close-required fields this ticket has not answered.
func unansweredForClose(ctx context.Context, q querier, ticketID string) ([]Field, error) {
	rows, err := q.Query(ctx, `
		SELECT `+fieldSelect+`
		FROM ticket_fields f
		WHERE f.archived_at IS NULL
		  AND f.required_to_close
		  AND NOT EXISTS (
			SELECT 1 FROM ticket_field_values v
			WHERE v.ticket_id = $1 AND v.field_id = f.id)
		ORDER BY f.sort_order, f.key`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("tickets: read close requirements: %w", err)
	}
	defer rows.Close()

	out := make([]Field, 0, 2)
	for rows.Next() {
		field, err := scanField(rows)
		if err != nil {
			return nil, fmt.Errorf("tickets: scan field: %w", err)
		}
		out = append(out, field)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets: read close requirements: %w", err)
	}
	return out, nil
}

// requireCloseAnswers is the close-time gate. It is called inside the transaction that closes the
// ticket, after the row lock, so a concurrent answer cannot slip between the check and the write.
func requireCloseAnswers(ctx context.Context, q querier, ticketID string) error {
	missing, err := unansweredForClose(ctx, q, ticketID)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &RequiredFieldError{Fields: missing, OnClose: true}
	}
	return nil
}

// --------------------------------------------------------------- helpers --

// storeSQLState reads the SQLSTATE of a database error. The classifiers live in internal/store
// (the package that owns the migration runner) so a check constraint and the code that explains
// it cannot disagree about a number.
func storeSQLState(err error) string { return store.SQLState(err) }
