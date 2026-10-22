// Package tickets owns support tickets: the ticket, its messages, its attachments,
// and the rules that decide who may see them.
//
// The security boundary is here, not in the HTTP layer. Every customer-facing read
// and write resolves the caller's customer id and filters on it in SQL, so a
// substituted identifier is indistinguishable from one that never existed:
// ErrNotFound, which the API renders as 404. A handler that fetched by id and then
// compared ownership would be one forgotten comparison away from an IDOR; a query
// that can only ever see its own customer's rows cannot leak them at all.
//
// Staff reads are deliberately unscoped, because a support queue is useless
// otherwise. They are gated by ticket:read:all / ticket:write:all instead, which is
// a different boundary and is enforced by the router.
//
// Ticket numbers come from the same sequence table invoices use, under the "ticket"
// scope. A second numbering scheme would eventually produce a duplicate number on
// the one document a customer quotes in an email.
package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors the HTTP layer maps to stable codes. They are values rather than strings so
// a handler cannot silently change a code a client branches on.
var (
	// ErrNotFound is returned when a ticket, message or attachment does not exist *or*
	// belongs to another customer. The two are deliberately indistinguishable.
	ErrNotFound = errors.New("tickets: not found")
	// ErrInvalidInput is returned for a field the caller got wrong: a subject that is
	// empty or too long, an unknown priority, a department not in settings.
	ErrInvalidInput = errors.New("tickets: invalid input")
	// ErrDisabled is returned when the operator has switched tickets off.
	ErrDisabled = errors.New("tickets: support tickets are disabled")
	// ErrClosingDisabled is returned when customers may not close their own tickets.
	ErrClosingDisabled = errors.New("tickets: customers may not close tickets")
	// ErrAttachmentTooLarge is returned before an oversized upload is stored.
	ErrAttachmentTooLarge = errors.New("tickets: attachment too large")
	// ErrDuplicate is returned when an inbound message id has already been imported.
	ErrDuplicate = errors.New("tickets: duplicate message")
)

const (
	// MaxSubjectLen matches the CHECK on tickets.subject.
	MaxSubjectLen = 200
	// MaxMessageBytes bounds one message body. Generous for a support thread, small
	// enough that a thread is not a file transfer.
	MaxMessageBytes = 64 << 10
	// MaxAttachmentBytes is the 10 MB cap Paymenter uses. It is enforced before the
	// whole body is read, and again by the table CHECK.
	MaxAttachmentBytes = 10 << 20
	// DefaultAutoCloseDays is the code-side default when the setting is missing.
	DefaultAutoCloseDays = 14
	// MaxAttachmentNameLen bounds the stored filename.
	MaxAttachmentNameLen = 255
	// MaxMessageIDLen bounds an email Message-ID. RFC 5322 caps a header line at 998
	// octets, so this is generous while still refusing a body-sized "id".
	MaxMessageIDLen = 998
)

// Series is the ticket number series. It shares invoice_number_sequences with
// invoices, so "gap-free, allocated under a row lock" is inherited rather than
// reimplemented.
var Series = billing.NumberSeries{Scope: "ticket", Prefix: "TKT-", Width: 6}

// Priorities is the closed set the schema accepts.
var Priorities = []string{"low", "normal", "high"}

// Statuses is the closed set the schema accepts.
var Statuses = []string{"open", "replied", "closed"}

// DefaultDepartments is what a fresh installation offers when the settings row is
// missing or malformed. A missing row must not break the new-ticket screen.
var DefaultDepartments = []string{"general", "billing", "technical"}

// Settings is the tickets configuration read from the settings table, with
// code-side defaults for every key so an installation that has never opened the
// settings page still has a working support desk.
type Settings struct {
	Disabled              bool
	ClientClosingDisabled bool
	Departments           []string
	AutoCloseDays         int
}

// DefaultSettings returns the configuration a fresh installation behaves as if it
// had. It is a value, so a caller cannot mutate the package defaults.
func DefaultSettings() Settings {
	return Settings{
		Departments:   append([]string(nil), DefaultDepartments...),
		AutoCloseDays: DefaultAutoCloseDays,
	}
}

// DepartmentAllowed reports whether d is configured.
func (s Settings) DepartmentAllowed(d string) bool {
	for _, candidate := range s.Departments {
		if candidate == d {
			return true
		}
	}
	return false
}

// ValidPriority reports whether p is one of the schema's priorities.
func ValidPriority(p string) bool {
	for _, candidate := range Priorities {
		if candidate == p {
			return true
		}
	}
	return false
}

// ValidStatus reports whether s is one of the schema's statuses.
func ValidStatus(s string) bool {
	for _, candidate := range Statuses {
		if candidate == s {
			return true
		}
	}
	return false
}

// Ticket is one support ticket as the API renders it.
type Ticket struct {
	ID           string
	Number       string
	CustomerID   string
	UserID       string
	ServiceID    string
	Subject      string
	Department   string
	Priority     string
	Status       string
	AssignedTo   string
	OpenedBy     string
	LastReplyAt  *time.Time
	ClosedAt     *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
	// LastMessageFromStaff and LastMessageBody summarise the thread for a list row,
	// so the queue can show "who said what last" without loading every message.
	LastMessageFromStaff *bool
	LastMessageBody      string
	// CustomerEmail, CustomerName and AssigneeName are populated by staff reads only.
	CustomerEmail string
	CustomerName  string
	AssigneeName  string
}

// Message is one message in a thread.
type Message struct {
	ID           string
	TicketID     string
	AuthorUserID string
	AuthorName   string
	FromStaff    bool
	Body         string
	// MessageID is the email Message-ID a piped reply carried, when any. It is what
	// makes a duplicate delivery ignorable.
	MessageID   string
	CreatedAt   time.Time
	Attachments []Attachment
}

// Attachment is one stored file. Bytes is populated only by a download read.
type Attachment struct {
	ID          string
	MessageID   string
	Filename    string
	ContentType string
	ByteSize    int
	Digest      string
	CreatedAt   time.Time
	Bytes       []byte
}

// Thread is a ticket plus its messages, which is what a detail screen renders.
type Thread struct {
	Ticket   Ticket
	Messages []Message
	// Duplicate is true when an inbound email Message-ID had already been imported.
	// The thread is then the one that already exists, unchanged.
	Duplicate bool
}

// querier is the subset of pgx a pool and a transaction both satisfy. Store methods
// come in Tx and non-Tx forms so the HTTP layer can write a change and its audit row
// in one transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the ticket data access.
type Store struct {
	pool *store.Pool
	// Now is injectable for tests. Every write stamps its own time rather than
	// trusting the database clock, so a test can move time without touching the
	// server.
	Now func() time.Time
}

// New constructs a Store.
func New(pool *store.Pool) *Store {
	return &Store{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Settings reads the tickets configuration.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	return settingsFrom(ctx, s.pool)
}

// settingsFrom reads the tickets settings from a querier, folding every missing or
// malformed value into the code-side default.
func settingsFrom(ctx context.Context, q querier) (Settings, error) {
	out := DefaultSettings()
	rows, err := q.Query(ctx, `SELECT key, value FROM settings WHERE group_key = 'tickets'`)
	if err != nil {
		return out, fmt.Errorf("tickets: read settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, fmt.Errorf("tickets: scan setting: %w", err)
		}
		switch key {
		case "disabled":
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				out.Disabled = parsed
			}
		case "client_closing_disabled":
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				out.ClientClosingDisabled = parsed
			}
		case "departments":
			if parsed := parseDepartments(value); len(parsed) > 0 {
				out.Departments = parsed
			}
		case "autoclose_days":
			if days, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && days > 0 {
				out.AutoCloseDays = days
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("tickets: read settings: %w", err)
	}
	return out, nil
}

// parseDepartments decodes the JSON array an operator edits. A value that is not an
// array of strings is treated as absent rather than as a reason to refuse to open
// the support desk.
func parseDepartments(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// CreateInput is everything needed to open a ticket.
type CreateInput struct {
	CustomerID string
	// UserID is the login that opened the ticket. It may be empty for an import.
	UserID string
	// ServiceID is the service the ticket is about, when it is about one.
	ServiceID  string
	Subject    string
	Department string
	Priority   string
	// Body is the first message.
	Body string
	// MessageID is an email Message-ID for a piped ticket, when any.
	MessageID string
	// Attachments are stored with the first message, in the same transaction.
	Attachments []AttachmentInput
	// Now is the creation instant; zero uses the store clock.
	Now time.Time
}

// ReplyInput is a message appended to an existing ticket.
type ReplyInput struct {
	// UserID is the login that wrote the reply. It is empty for an imported message.
	UserID      string
	Body        string
	MessageID   string
	Attachments []AttachmentInput
	// Now is the reply instant; zero uses the store clock.
	Now time.Time
}

// Create opens a ticket and its first message in one transaction.
//
// A Message-ID that was already imported returns the existing thread with
// Duplicate=true instead of a second ticket: the transaction that tried to create it
// rolled back, including its allocated number, so a redelivered email leaves no
// trace.
func (s *Store) Create(ctx context.Context, in CreateInput) (Thread, error) {
	var out Thread
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CreateTx(ctx, tx, in)
		return err
	})
	if errors.Is(err, ErrDuplicate) {
		// The insert lost a race with a concurrent delivery of the same Message-ID. The
		// existing thread is only returned when it belongs to this customer: otherwise
		// the answer is not found, exactly as for a ticket that does not exist.
		existing, lookupErr := s.threadForMessageID(ctx, strings.TrimSpace(in.MessageID))
		if lookupErr != nil {
			return Thread{}, lookupErr
		}
		if existing.Ticket.CustomerID != in.CustomerID {
			return Thread{}, ErrNotFound
		}
		existing.Duplicate = true
		return existing, nil
	}
	return out, err
}

// threadForMessageID finds the thread an already-imported message belongs to.
func (s *Store) threadForMessageID(ctx context.Context, messageID string) (Thread, error) {
	msg, err := messageByMessageID(ctx, s.pool, messageID)
	if err != nil {
		return Thread{}, err
	}
	return s.threadTx(ctx, s.pool, msg.TicketID)
}

// CreateTx opens a ticket inside a caller's transaction, so the audit row commits
// with it.
func (s *Store) CreateTx(ctx context.Context, tx pgx.Tx, in CreateInput) (Thread, error) {
	settings, err := settingsFrom(ctx, tx)
	if err != nil {
		return Thread{}, err
	}
	if settings.Disabled {
		return Thread{}, ErrDisabled
	}

	in.Subject = strings.TrimSpace(in.Subject)
	in.Body = strings.TrimSpace(in.Body)
	in.Department = strings.TrimSpace(in.Department)
	in.Priority = strings.TrimSpace(in.Priority)
	if in.Department == "" {
		in.Department = settings.Departments[0]
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if err := validateCreate(in, settings); err != nil {
		return Thread{}, err
	}
	// An inbound email whose Message-ID was already imported must not create a second
	// ticket. Checking before the insert is what keeps a redelivered email from leaving
	// an empty ticket behind: the first delivery's ticket is returned instead.
	if in.MessageID != "" {
		existing, err := messageByMessageID(ctx, tx, strings.TrimSpace(in.MessageID))
		if err == nil {
			thread, terr := s.threadTx(ctx, tx, existing.TicketID)
			if terr != nil {
				return Thread{}, terr
			}
			// The Message-ID may belong to another customer's ticket. Returning that
			// thread would hand one customer another's conversation, so it is the same
			// answer as a ticket that does not exist.
			if thread.Ticket.CustomerID != in.CustomerID {
				return Thread{}, ErrNotFound
			}
			thread.Duplicate = true
			return thread, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Thread{}, err
		}
	}
	if in.CustomerID == "" {
		return Thread{}, fmt.Errorf("%w: a customer is required", ErrInvalidInput)
	}
	if in.ServiceID != "" {
		// A ticket may only reference a service the caller owns. Checking it here
		// makes "that service is someone else's" the same answer as "no such
		// service".
		if !validUUID(in.ServiceID) {
			return Thread{}, fmt.Errorf("%w: no such service", ErrNotFound)
		}
		var owns bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND customer_id = $2)`,
			in.ServiceID, in.CustomerID).Scan(&owns); err != nil {
			return Thread{}, fmt.Errorf("tickets: check service: %w", err)
		}
		if !owns {
			return Thread{}, fmt.Errorf("%w: no such service", ErrNotFound)
		}
	}

	now := in.Now
	if now.IsZero() {
		now = s.clock()
	}
	number, err := Series.Allocate(ctx, tx)
	if err != nil {
		return Thread{}, err
	}

	var ticket Ticket
	err = tx.QueryRow(ctx, `
		INSERT INTO tickets (number, customer_id, user_id, service_id, subject, department,
			priority, status, opened_by, last_reply_at, created_at, updated_at)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, 'open',
			NULLIF($3, '')::uuid, $8, $8, $8)
		RETURNING id, COALESCE(number, ''), customer_id, COALESCE(user_id::text, ''),
			COALESCE(service_id::text, ''), subject, department, priority, status,
			COALESCE(assigned_to::text, ''), COALESCE(opened_by::text, ''),
			last_reply_at, closed_at, created_at, updated_at`,
		number.Format(), in.CustomerID, in.UserID, in.ServiceID, in.Subject, in.Department,
		in.Priority, now,
	).Scan(&ticket.ID, &ticket.Number, &ticket.CustomerID, &ticket.UserID, &ticket.ServiceID,
		&ticket.Subject, &ticket.Department, &ticket.Priority, &ticket.Status,
		&ticket.AssignedTo, &ticket.OpenedBy, &ticket.LastReplyAt, &ticket.ClosedAt,
		&ticket.CreatedAt, &ticket.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Thread{}, ErrDuplicate
		}
		return Thread{}, fmt.Errorf("tickets: insert ticket: %w", err)
	}

	msg, duplicate, err := insertMessage(ctx, tx, ticket.ID, in.UserID, false, in.Body, in.MessageID, now)
	if err != nil {
		return Thread{}, err
	}
	if duplicate {
		// Another delivery won the race between the pre-check above and this insert.
		// Failing the transaction rolls back the ticket and the number with it, so the
		// duplicate leaves no empty ticket behind.
		return Thread{}, ErrDuplicate
	}
	for _, attachment := range in.Attachments {
		if _, err := insertAttachment(ctx, tx, msg.ID, attachment); err != nil {
			return Thread{}, err
		}
	}
	return s.threadTx(ctx, tx, ticket.ID)
}

func validateCreate(in CreateInput, settings Settings) error {
	if in.Subject == "" {
		return fmt.Errorf("%w: a subject is required", ErrInvalidInput)
	}
	if len(in.Subject) > MaxSubjectLen {
		return fmt.Errorf("%w: the subject must be at most %d characters", ErrInvalidInput, MaxSubjectLen)
	}
	if in.Body == "" {
		return fmt.Errorf("%w: the first message cannot be empty", ErrInvalidInput)
	}
	if len(in.Body) > MaxMessageBytes {
		return fmt.Errorf("%w: the message is too long", ErrInvalidInput)
	}
	if !settings.DepartmentAllowed(in.Department) {
		return fmt.Errorf("%w: unknown department %q", ErrInvalidInput, in.Department)
	}
	if !ValidPriority(in.Priority) {
		return fmt.Errorf("%w: unknown priority %q", ErrInvalidInput, in.Priority)
	}
	return nil
}

// insertMessage appends one message and reports whether an email Message-ID was
// already present.
//
// A duplicate delivery is not an error: the caller answers success and the thread
// stays as it was. That is the property email piping needs, and it is enforced by
// the partial unique index rather than by a read-then-write race.
func insertMessage(ctx context.Context, q querier, ticketID, authorUserID string,
	fromStaff bool, body, messageID string, now time.Time) (Message, bool, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Message{}, false, fmt.Errorf("%w: the message cannot be empty", ErrInvalidInput)
	}
	if len(body) > MaxMessageBytes {
		return Message{}, false, fmt.Errorf("%w: the message is too long", ErrInvalidInput)
	}
	messageID = strings.TrimSpace(messageID)
	if len(messageID) > MaxMessageIDLen {
		return Message{}, false, fmt.Errorf("%w: the message id is too long", ErrInvalidInput)
	}

	var out Message
	err := q.QueryRow(ctx, `
		INSERT INTO ticket_messages (ticket_id, author_user_id, from_staff, body, message_id, created_at)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, NULLIF($5, ''), $6)
		ON CONFLICT (message_id) WHERE message_id IS NOT NULL DO NOTHING
		RETURNING id, ticket_id, COALESCE(author_user_id::text, ''), from_staff, body,
			COALESCE(message_id, ''), created_at`,
		ticketID, authorUserID, fromStaff, body, messageID, now,
	).Scan(&out.ID, &out.TicketID, &out.AuthorUserID, &out.FromStaff, &out.Body,
		&out.MessageID, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The unique index found an existing row: this delivery was already imported.
		existing, getErr := messageByMessageID(ctx, q, messageID)
		if getErr != nil {
			return Message{}, false, getErr
		}
		return existing, true, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("tickets: insert message: %w", err)
	}
	return out, false, nil
}

func messageByMessageID(ctx context.Context, q querier, messageID string) (Message, error) {
	var out Message
	err := q.QueryRow(ctx, `
		SELECT m.id, m.ticket_id, COALESCE(m.author_user_id::text, ''), m.from_staff, m.body,
			COALESCE(m.message_id, ''), m.created_at
		FROM ticket_messages m WHERE m.message_id = $1`, messageID,
	).Scan(&out.ID, &out.TicketID, &out.AuthorUserID, &out.FromStaff, &out.Body,
		&out.MessageID, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("tickets: read message: %w", err)
	}
	return out, nil
}

// ReplyAsCustomer appends a customer reply, scoped to the caller's customer.
func (s *Store) ReplyAsCustomer(ctx context.Context, customerID, ticketID string, in ReplyInput) (Message, bool, error) {
	var (
		out       Message
		duplicate bool
	)
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, duplicate, err = s.ReplyAsCustomerTx(ctx, tx, customerID, ticketID, in)
		return err
	})
	return out, duplicate, err
}

// ReplyAsCustomerTx appends a customer reply inside a caller's transaction.
//
// The ticket row is locked with the customer predicate in the WHERE clause, so a
// ticket belonging to someone else is ErrNotFound rather than a write that happens
// to affect zero rows.
func (s *Store) ReplyAsCustomerTx(ctx context.Context, tx pgx.Tx, customerID, ticketID string, in ReplyInput) (Message, bool, error) {
	if !validUUID(ticketID) || !validUUID(customerID) {
		return Message{}, false, ErrNotFound
	}
	var status string
	err := tx.QueryRow(ctx, `
		SELECT status FROM tickets WHERE id = $1 AND customer_id = $2 FOR UPDATE`,
		ticketID, customerID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, ErrNotFound
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("tickets: lock ticket: %w", err)
	}

	now := in.Now
	if now.IsZero() {
		now = s.clock()
	}
	msg, duplicate, err := insertMessage(ctx, tx, ticketID, in.UserID, false, in.Body, in.MessageID, now)
	if err != nil {
		return Message{}, false, err
	}
	if duplicate {
		// A replayed delivery must not move the status or the reply clock. A Message-ID
		// that belongs to a *different* ticket is not a replay of this reply; returning
		// that message would leak another thread's body, so it is not found.
		if msg.TicketID != ticketID {
			return Message{}, false, ErrNotFound
		}
		return msg, true, nil
	}
	for _, attachment := range in.Attachments {
		if _, err := insertAttachment(ctx, tx, msg.ID, attachment); err != nil {
			return Message{}, false, err
		}
	}
	if err := touchTicket(ctx, tx, ticketID, "open", now); err != nil {
		return Message{}, false, err
	}
	return msg, false, nil
}

// ReplyAsStaff appends a staff reply. It is unscoped: the caller holds
// ticket:write:all, which is a different boundary checked by the router.
func (s *Store) ReplyAsStaff(ctx context.Context, authorUserID, ticketID string, in ReplyInput) (Message, bool, error) {
	var (
		out       Message
		duplicate bool
	)
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, duplicate, err = s.ReplyAsStaffTx(ctx, tx, authorUserID, ticketID, in)
		return err
	})
	return out, duplicate, err
}

// ReplyAsStaffTx appends a staff reply inside a caller's transaction.
func (s *Store) ReplyAsStaffTx(ctx context.Context, tx pgx.Tx, authorUserID, ticketID string, in ReplyInput) (Message, bool, error) {
	if !validUUID(ticketID) {
		return Message{}, false, ErrNotFound
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM tickets WHERE id = $1 FOR UPDATE`, ticketID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, ErrNotFound
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("tickets: lock ticket: %w", err)
	}

	now := in.Now
	if now.IsZero() {
		now = s.clock()
	}
	msg, duplicate, err := insertMessage(ctx, tx, ticketID, authorUserID, true, in.Body, in.MessageID, now)
	if err != nil {
		return Message{}, false, err
	}
	if duplicate {
		// A Message-ID that belongs to another ticket is not a replay of this one.
		if msg.TicketID != ticketID {
			return Message{}, false, fmt.Errorf("%w: the message id belongs to another ticket", ErrInvalidInput)
		}
		return msg, true, nil
	}
	for _, attachment := range in.Attachments {
		if _, err := insertAttachment(ctx, tx, msg.ID, attachment); err != nil {
			return Message{}, false, err
		}
	}
	if err := touchTicket(ctx, tx, ticketID, "replied", now); err != nil {
		return Message{}, false, err
	}
	return msg, false, nil
}

// touchTicket records that the thread moved and reopens a closed ticket.
func touchTicket(ctx context.Context, q querier, ticketID, status string, now time.Time) error {
	if _, err := q.Exec(ctx, `
		UPDATE tickets
		SET status = $2, last_reply_at = $3, closed_at = NULL, updated_at = $3
		WHERE id = $1`, ticketID, status, now); err != nil {
		return fmt.Errorf("tickets: update ticket: %w", err)
	}
	return nil
}

// CloseAsCustomer closes the caller's own ticket.
func (s *Store) CloseAsCustomer(ctx context.Context, customerID, ticketID string) (Ticket, error) {
	var out Ticket
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CloseAsCustomerTx(ctx, tx, customerID, ticketID)
		return err
	})
	return out, err
}

// CloseAsCustomerTx closes a customer's ticket inside a caller's transaction.
func (s *Store) CloseAsCustomerTx(ctx context.Context, tx pgx.Tx, customerID, ticketID string) (Ticket, error) {
	settings, err := settingsFrom(ctx, tx)
	if err != nil {
		return Ticket{}, err
	}
	if settings.ClientClosingDisabled {
		return Ticket{}, ErrClosingDisabled
	}
	if !validUUID(ticketID) || !validUUID(customerID) {
		return Ticket{}, ErrNotFound
	}
	now := s.clock()
	tag, err := tx.Exec(ctx, `
		UPDATE tickets SET status = 'closed', closed_at = $3, updated_at = $3
		WHERE id = $1 AND customer_id = $2`, ticketID, customerID, now)
	if err != nil {
		return Ticket{}, fmt.Errorf("tickets: close ticket: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Ticket{}, ErrNotFound
	}
	return s.ticketByCustomerTx(ctx, tx, customerID, ticketID)
}

// CloseAsStaff closes any ticket.
func (s *Store) CloseAsStaff(ctx context.Context, ticketID string) (Ticket, error) {
	var out Ticket
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CloseAsStaffTx(ctx, tx, ticketID)
		return err
	})
	return out, err
}

// CloseAsStaffTx closes a ticket inside a caller's transaction.
func (s *Store) CloseAsStaffTx(ctx context.Context, tx pgx.Tx, ticketID string) (Ticket, error) {
	if !validUUID(ticketID) {
		return Ticket{}, ErrNotFound
	}
	now := s.clock()
	tag, err := tx.Exec(ctx, `
		UPDATE tickets SET status = 'closed', closed_at = $2, updated_at = $2
		WHERE id = $1`, ticketID, now)
	if err != nil {
		return Ticket{}, fmt.Errorf("tickets: close ticket: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Ticket{}, ErrNotFound
	}
	return s.staffTicketTx(ctx, tx, ticketID)
}

// ThreadForCustomer returns a ticket and its messages, scoped to the caller.
func (s *Store) ThreadForCustomer(ctx context.Context, customerID, ticketID string) (Thread, error) {
	if !validUUID(ticketID) || !validUUID(customerID) {
		return Thread{}, ErrNotFound
	}
	ticket, err := s.ticketByCustomer(ctx, customerID, ticketID)
	if err != nil {
		return Thread{}, err
	}
	messages, err := s.messagesTx(ctx, s.pool, ticketID)
	if err != nil {
		return Thread{}, err
	}
	return Thread{Ticket: ticket, Messages: messages}, nil
}

func (s *Store) ticketByCustomer(ctx context.Context, customerID, ticketID string) (Ticket, error) {
	return s.ticketByCustomerTx(ctx, s.pool, customerID, ticketID)
}

func (s *Store) ticketByCustomerTx(ctx context.Context, q querier, customerID, ticketID string) (Ticket, error) {
	row := q.QueryRow(ctx, `SELECT `+ticketSelect+ticketFrom+`
		WHERE t.id = $1 AND t.customer_id = $2`, ticketID, customerID)
	ticket, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// ListForCustomer lists a customer's tickets, newest first, optionally by status.
func (s *Store) ListForCustomer(ctx context.Context, customerID, status string, limit int, cursor string) ([]Ticket, string, error) {
	if !validUUID(customerID) {
		return nil, "", nil
	}
	limit = clampLimit(limit)
	args := []any{customerID}
	where := "t.customer_id = $1"

	if status != "" && status != "all" {
		if !ValidStatus(status) {
			return nil, "", fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
		}
		args = append(args, status)
		where += fmt.Sprintf(" AND t.status = $%d", len(args))
	}
	if cursor != "" {
		when, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, when, id)
		where += fmt.Sprintf(" AND (t.created_at, t.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	stmt := `SELECT ` + ticketSelect + ticketFrom + ` WHERE ` + where +
		` ORDER BY t.created_at DESC, t.id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", fmt.Errorf("tickets: list tickets: %w", err)
	}
	defer rows.Close()

	out := make([]Ticket, 0, limit)
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, ticket)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tickets: read tickets: %w", err)
	}
	trimmed, next := page(out, limit)
	return trimmed, next, nil
}

// ThreadForStaff returns any ticket and its messages, with the customer record the
// queue needs to answer "who is this".
func (s *Store) ThreadForStaff(ctx context.Context, ticketID string) (Thread, error) {
	if !validUUID(ticketID) {
		return Thread{}, ErrNotFound
	}
	var out Thread
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ticket, err := s.staffTicketTx(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		messages, err := s.messagesTx(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		out = Thread{Ticket: ticket, Messages: messages}
		return nil
	})
	return out, err
}

// staffTicketTx reads a ticket in the staff shape.
func (s *Store) staffTicketTx(ctx context.Context, q querier, ticketID string) (Ticket, error) {
	row := q.QueryRow(ctx, `SELECT `+staffTicketSelect+staffTicketFrom+` WHERE t.id = $1`, ticketID)
	ticket, err := scanStaffTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

func (s *Store) ticketTx(ctx context.Context, q querier, ticketID string) (Ticket, error) {
	row := q.QueryRow(ctx, `SELECT `+ticketSelect+ticketFrom+` WHERE t.id = $1`, ticketID)
	ticket, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// StaffFilter narrows the support queue.
type StaffFilter struct {
	Status     string
	Priority   string
	Department string
	// Assignee is a staff user id, or "unassigned" / "none" for tickets nobody owns.
	Assignee string
	// Query searches the subject and the customer's email/name.
	Query string
}

// ListForStaff lists every ticket, newest first, with the queue's filters.
func (s *Store) ListForStaff(ctx context.Context, f StaffFilter, limit int, cursor string) ([]Ticket, string, error) {
	limit = clampLimit(limit)
	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, "", err
	}

	args := make([]any, 0, 8)
	where := "true"
	if f.Status != "" && f.Status != "all" {
		if !ValidStatus(f.Status) {
			return nil, "", fmt.Errorf("%w: unknown status %q", ErrInvalidInput, f.Status)
		}
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND t.status = $%d", len(args))
	}
	if f.Priority != "" && f.Priority != "all" {
		if !ValidPriority(f.Priority) {
			return nil, "", fmt.Errorf("%w: unknown priority %q", ErrInvalidInput, f.Priority)
		}
		args = append(args, f.Priority)
		where += fmt.Sprintf(" AND t.priority = $%d", len(args))
	}
	if f.Department != "" && f.Department != "all" {
		if !settings.DepartmentAllowed(f.Department) {
			return nil, "", fmt.Errorf("%w: unknown department %q", ErrInvalidInput, f.Department)
		}
		args = append(args, f.Department)
		where += fmt.Sprintf(" AND t.department = $%d", len(args))
	}
	switch strings.TrimSpace(f.Assignee) {
	case "":
	case "unassigned", "none":
		where += " AND t.assigned_to IS NULL"
	default:
		if !validUUID(f.Assignee) {
			return nil, "", fmt.Errorf("%w: unknown assignee", ErrInvalidInput)
		}
		args = append(args, f.Assignee)
		where += fmt.Sprintf(" AND t.assigned_to = $%d", len(args))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) < 2 {
			return nil, "", fmt.Errorf("%w: the search needs at least two characters", ErrInvalidInput)
		}
		pattern := "%" + escapeLike(strings.ToLower(q)) + "%"
		args = append(args, pattern)
		where += fmt.Sprintf(
			" AND (lower(t.subject) LIKE $%d ESCAPE '\\' OR lower(c.email) LIKE $%d ESCAPE '\\'"+
				" OR lower(c.name) LIKE $%d ESCAPE '\\')", len(args), len(args), len(args))
	}
	if cursor != "" {
		when, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, when, id)
		where += fmt.Sprintf(" AND (t.created_at, t.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	stmt := `SELECT ` + staffTicketSelect + staffTicketFrom + ` WHERE ` + where +
		` ORDER BY t.created_at DESC, t.id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", fmt.Errorf("tickets: list queue: %w", err)
	}
	defer rows.Close()

	out := make([]Ticket, 0, limit)
	for rows.Next() {
		ticket, err := scanStaffTicket(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, ticket)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tickets: read queue: %w", err)
	}
	trimmed, next := page(out, limit)
	return trimmed, next, nil
}

// StaffUpdate changes a ticket's status, priority, department or assignee.
type StaffUpdate struct {
	// Empty strings mean "leave unchanged".
	Status     string
	Priority   string
	Department string
	// SetAssignee distinguishes "no change" from "clear the assignee".
	SetAssignee bool
	Assignee    string
}

// UpdateStaff applies a staff change. Every field is optional.
func (s *Store) UpdateStaff(ctx context.Context, ticketID string, in StaffUpdate) (Ticket, error) {
	var out Ticket
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateStaffTx(ctx, tx, ticketID, in)
		return err
	})
	return out, err
}

// UpdateStaffTx applies a staff change inside a caller's transaction.
func (s *Store) UpdateStaffTx(ctx context.Context, tx pgx.Tx, ticketID string, in StaffUpdate) (Ticket, error) {
	if !validUUID(ticketID) {
		return Ticket{}, ErrNotFound
	}
	settings, err := settingsFrom(ctx, tx)
	if err != nil {
		return Ticket{}, err
	}

	// Lock first: a status change made against a row read outside the transaction is
	// a change to a row that may already be different.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM tickets WHERE id = $1 FOR UPDATE`, ticketID).
		Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	} else if err != nil {
		return Ticket{}, fmt.Errorf("tickets: lock ticket: %w", err)
	}

	now := s.clock()
	args := []any{ticketID, now}
	set := "updated_at = $2"
	if in.Status != "" {
		if !ValidStatus(in.Status) {
			return Ticket{}, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, in.Status)
		}
		args = append(args, in.Status)
		set += fmt.Sprintf(", status = $%d", len(args))
		if in.Status == "closed" {
			set += ", closed_at = $2"
		} else {
			set += ", closed_at = NULL"
		}
	}
	if in.Priority != "" {
		if !ValidPriority(in.Priority) {
			return Ticket{}, fmt.Errorf("%w: unknown priority %q", ErrInvalidInput, in.Priority)
		}
		args = append(args, in.Priority)
		set += fmt.Sprintf(", priority = $%d", len(args))
	}
	if in.Department != "" {
		if !settings.DepartmentAllowed(in.Department) {
			return Ticket{}, fmt.Errorf("%w: unknown department %q", ErrInvalidInput, in.Department)
		}
		args = append(args, in.Department)
		set += fmt.Sprintf(", department = $%d", len(args))
	}
	if in.SetAssignee {
		if in.Assignee == "" {
			set += ", assigned_to = NULL"
		} else {
			if !validUUID(in.Assignee) {
				return Ticket{}, fmt.Errorf("%w: unknown assignee", ErrInvalidInput)
			}
			// A ticket is assigned to an operator, not to a customer login.
			var isStaff bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM staff_roles WHERE user_id = $1)`, in.Assignee).
				Scan(&isStaff); err != nil {
				return Ticket{}, fmt.Errorf("tickets: check assignee: %w", err)
			}
			if !isStaff {
				return Ticket{}, fmt.Errorf("%w: the assignee is not staff", ErrInvalidInput)
			}
			args = append(args, in.Assignee)
			set += fmt.Sprintf(", assigned_to = $%d", len(args))
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE tickets SET `+set+` WHERE id = $1`, args...); err != nil {
		return Ticket{}, fmt.Errorf("tickets: update ticket: %w", err)
	}
	return s.staffTicketTx(ctx, tx, ticketID)
}

// CustomerOf returns the billing account a ticket belongs to.
//
// It exists so a staff reply can publish an event the *customer* sees: the handler
// knows the ticket id, and the customer id is what scopes a portal event.
func (s *Store) CustomerOf(ctx context.Context, ticketID string) (string, error) {
	if !validUUID(ticketID) {
		return "", ErrNotFound
	}
	var id string
	err := s.pool.QueryRow(ctx, `SELECT customer_id FROM tickets WHERE id = $1`, ticketID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tickets: resolve ticket customer: %w", err)
	}
	return id, nil
}

// ------------------------------------------------------------------ scanning --

const ticketSelect = `
	t.id, COALESCE(t.number, ''), t.customer_id, COALESCE(t.user_id::text, ''),
	COALESCE(t.service_id::text, ''), t.subject, t.department, t.priority, t.status,
	COALESCE(t.assigned_to::text, ''), COALESCE(t.opened_by::text, ''),
	t.last_reply_at, t.closed_at, t.created_at, t.updated_at,
	COALESCE(m.n, 0), m.last_staff, COALESCE(m.last_body, '')`

const ticketLateral = `
	LEFT JOIN LATERAL (
		SELECT count(*) AS n,
		       (array_agg(tm.from_staff ORDER BY tm.created_at DESC, tm.id DESC))[1] AS last_staff,
		       (array_agg(tm.body ORDER BY tm.created_at DESC, tm.id DESC))[1] AS last_body
		FROM ticket_messages tm
		WHERE tm.ticket_id = t.id
	) m ON true`

const ticketFrom = `
	FROM tickets t` + ticketLateral

const staffTicketSelect = ticketSelect + `, c.email, c.name,
	COALESCE(a.display_name, a.email, '')`

const staffTicketFrom = ticketFrom + ` JOIN customers c ON c.id = t.customer_id
	LEFT JOIN users a ON a.id = t.assigned_to`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTicket(sc rowScanner) (Ticket, error) {
	var t Ticket
	err := sc.Scan(&t.ID, &t.Number, &t.CustomerID, &t.UserID, &t.ServiceID, &t.Subject,
		&t.Department, &t.Priority, &t.Status, &t.AssignedTo, &t.OpenedBy,
		&t.LastReplyAt, &t.ClosedAt, &t.CreatedAt, &t.UpdatedAt,
		&t.MessageCount, &t.LastMessageFromStaff, &t.LastMessageBody)
	if err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func scanStaffTicket(sc rowScanner) (Ticket, error) {
	var t Ticket
	err := sc.Scan(&t.ID, &t.Number, &t.CustomerID, &t.UserID, &t.ServiceID, &t.Subject,
		&t.Department, &t.Priority, &t.Status, &t.AssignedTo, &t.OpenedBy,
		&t.LastReplyAt, &t.ClosedAt, &t.CreatedAt, &t.UpdatedAt,
		&t.MessageCount, &t.LastMessageFromStaff, &t.LastMessageBody,
		&t.CustomerEmail, &t.CustomerName, &t.AssigneeName)
	if err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func (s *Store) threadTx(ctx context.Context, q querier, ticketID string) (Thread, error) {
	ticket, err := s.ticketTx(ctx, q, ticketID)
	if err != nil {
		return Thread{}, err
	}
	messages, err := s.messagesTx(ctx, q, ticketID)
	if err != nil {
		return Thread{}, err
	}
	return Thread{Ticket: ticket, Messages: messages}, nil
}

func (s *Store) messagesTx(ctx context.Context, q querier, ticketID string) ([]Message, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id, m.ticket_id, COALESCE(m.author_user_id::text, ''),
		       COALESCE(NULLIF(u.display_name, ''), u.email, ''),
		       m.from_staff, m.body, COALESCE(m.message_id, ''), m.created_at
		FROM ticket_messages m
		LEFT JOIN users u ON u.id = m.author_user_id
		WHERE m.ticket_id = $1
		ORDER BY m.created_at, m.id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("tickets: read messages: %w", err)
	}
	defer rows.Close()

	out := make([]Message, 0, 8)
	index := make(map[string]int, 8)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.TicketID, &m.AuthorUserID, &m.AuthorName,
			&m.FromStaff, &m.Body, &m.MessageID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("tickets: scan message: %w", err)
		}
		out = append(out, m)
		index[m.ID] = len(out) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets: read messages: %w", err)
	}

	attachments, err := attachmentsForTicket(ctx, q, ticketID)
	if err != nil {
		return nil, err
	}
	for _, a := range attachments {
		if i, ok := index[a.MessageID]; ok {
			out[i].Attachments = append(out[i].Attachments, a)
		}
	}
	return out, nil
}

// -------------------------------------------------------------------- helpers --

// page trims the extra row used to detect a next page and returns its cursor.
//
// The query asks for limit+1 rows: the extra one is the proof that another page
// exists, and its predecessor is the position the next cursor names.
func page(rows []Ticket, limit int) ([]Ticket, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	last := rows[limit-1]
	return rows[:limit], encodeCursor(last.CreatedAt, last.ID)
}

// clampLimit applies the default and the maximum to a requested page size.
func clampLimit(limit int) int { return paginate.Clamp(limit) }

// validUUID reports whether s has the shape of a UUID. It is a shape check, not an
// existence check: it exists so a malformed identifier is a 404 that never reaches
// the database, rather than a driver error that becomes a 500.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

// escapeLike escapes the LIKE metacharacters in a user-supplied search term.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}

func isUniqueViolation(err error) bool {
	return store.IsUniqueViolation(err)
}
