// Package audit records who did what, to which record, and why.
//
// It is separate from the request log on purpose. A request log says "POST /void
// returned 200"; this says "alice voided INV-000042 because the customer was billed
// twice". Only the second one is evidence.
//
// The rules that shape it:
//
//   - The writer takes a transaction. An audit row and the change it describes must
//     commit together: an audit row for a rolled-back change is a lie, and a change
//     with no audit row is a gap.
//   - Secrets never enter the table. Redaction runs over the changes map before the
//     insert, so a caller that puts a token in `changes` still cannot store it.
//   - The table is append-only in the database, so a bug cannot rewrite history even
//     if it tries.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// ActorKind says what sort of principal performed the action.
const (
	ActorUser   = "user"
	ActorAPIKey = "api_key"
	ActorSystem = "system"
)

// Outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
)

// Actor is the principal behind an event.
type Actor struct {
	// ID is the user id. It is empty for a system action.
	ID   string
	Kind string
	// ImpersonatedBy is the operator's user id when this action happened inside an impersonated
	// session, and empty otherwise.
	//
	// It is separate from ID because the two answer different questions. ID answers "whose account
	// was this done to", which is what a customer's support thread needs; ImpersonatedBy answers
	// "which human did it", which is what an investigation needs. A row recording only the customer
	// would make an operator's actions indistinguishable from the customer's own — exactly the
	// confusion impersonation must not create.
	ImpersonatedBy string
}

// Event is one audit row.
type Event struct {
	Actor Actor
	// Action is a dotted verb: invoice.void, refund.create, service.suspend.
	Action string
	// TargetType is the record's kind, e.g. "invoice".
	TargetType string
	// TargetID is the record's id, when it has one.
	TargetID string
	// Outcome is one of OutcomeSuccess, OutcomeFailure, OutcomeDenied.
	Outcome string
	// Changes is the redacted diff. It may be nil.
	Changes map[string]any
	// CorrelationID ties the row to the request that caused it.
	CorrelationID string
	// IP is the caller's address, when known.
	IP string
}

// Row is a stored audit event.
type Row struct {
	ID            int64
	ActorID       string
	ActorKind     string
	Action        string
	TargetType    string
	TargetID      string
	Outcome       string
	Changes       map[string]any
	CorrelationID string
	IP            string
	CreatedAt     time.Time
}

// Querier is the read surface List needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Filter narrows an audit query. Every field is optional.
type Filter struct {
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	From       *time.Time
	To         *time.Time
}

// ListResult is one page of audit rows.
type ListResult struct {
	Rows       []Row
	NextCursor string
}

// List returns audit rows newest first, using keyset pagination.
//
// The cursor carries (created_at, id) so a row inserted between two pages cannot shift
// the window and make the reviewer miss an entry.
func List(ctx context.Context, q Querier, f Filter, limit int, cursor string) (ListResult, error) {
	limit = paginate.Clamp(limit)

	args := []any{}
	where := []string{"true"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.ActorID != "" {
		add("actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.TargetType != "" {
		add("target_type = $%d", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if f.From != nil {
		add("created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("created_at <= $%d", *f.To)
	}

	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return ListResult{}, err
	}
	if cursor != "" {
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			return ListResult{}, fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	// One extra row tells us whether another page exists without a second query.
	args = append(args, limit+1)
	stmt := `
		SELECT id, COALESCE(actor_id::text, ''), actor_kind, action, target_type,
		       COALESCE(target_id::text, ''), outcome, changes, correlation_id,
		       COALESCE(host(ip), ''), created_at
		FROM audit_events
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := q.Query(ctx, stmt, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("audit: list events: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var (
			r       Row
			changes []byte
		)
		if err := rows.Scan(&r.ID, &r.ActorID, &r.ActorKind, &r.Action, &r.TargetType,
			&r.TargetID, &r.Outcome, &changes, &r.CorrelationID, &r.IP, &r.CreatedAt); err != nil {
			return ListResult{}, fmt.Errorf("audit: scan event: %w", err)
		}
		if len(changes) > 0 {
			if err := json.Unmarshal(changes, &r.Changes); err != nil {
				return ListResult{}, fmt.Errorf("audit: decode changes: %w", err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("audit: read events: %w", err)
	}

	res := ListResult{}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		res.NextCursor = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(last.ID, 10))
	}
	res.Rows = out
	return res, nil
}

// withChanges returns a copy of the event with a diff attached.
//
// It returns a value rather than mutating, so an event can be built inline in a transaction
// without a shared variable that a second caller could see half-populated.
func (e Event) WithChanges(changes map[string]any) Event {
	e.Changes = changes
	return e
}

// Auditor is implemented by a caller that wants a domain service to write its audit row inside the
// service's own transaction.
//
// It exists because the alternative — the service building the row itself — loses the request's
// correlation id and source address, which are exactly what an incident review uses to tie a change
// to the request that caused it. The service calls Record inside its transaction; the caller decides
// who the actor is and what the diff says.
type Auditor interface {
	// RecordAction records one action against one target.
	RecordAction(ctx context.Context, tx pgx.Tx, action, targetType, targetID string,
		changes map[string]any) error
}

// Recorder writes audit events.
type Recorder struct {
	tx pgx.Tx
}

// InTx returns a recorder bound to a transaction.
func InTx(tx pgx.Tx) *Recorder { return &Recorder{tx: tx} }

// Record validates and writes one event.
//
// Validation is deliberately strict. An audit row with an empty action is noise that
// cannot be filtered; failing the transaction makes the caller fix it while the bug is
// still cheap.
func (r *Recorder) Record(ctx context.Context, e Event) error {
	actorKind := e.Actor.Kind
	if actorKind == "" {
		actorKind = ActorSystem
	}
	if actorKind != ActorUser && actorKind != ActorAPIKey && actorKind != ActorSystem {
		return fmt.Errorf("audit: unknown actor kind %q", actorKind)
	}
	if e.Actor.ID != "" && actorKind == ActorSystem {
		return fmt.Errorf("audit: a system event must not name an actor id")
	}
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("audit: action is required")
	}
	if strings.TrimSpace(e.TargetType) == "" {
		return fmt.Errorf("audit: target type is required")
	}
	switch e.Outcome {
	case OutcomeSuccess, OutcomeFailure, OutcomeDenied:
	default:
		return fmt.Errorf("audit: unknown outcome %q", e.Outcome)
	}

	changes := Redact(e.Changes)
	payload, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("audit: encode changes: %w", err)
	}

	_, err = r.tx.Exec(ctx, `
		INSERT INTO audit_events
			(actor_id, actor_kind, action, target_type, target_id, outcome,
			 changes, correlation_id, ip, impersonated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		nullableID(e.Actor.ID), actorKind, e.Action, e.TargetType,
		nullableID(e.TargetID), e.Outcome, payload, e.CorrelationID, nullableIP(e.IP),
		nullableID(e.Actor.ImpersonatedBy))
	if err != nil {
		return fmt.Errorf("audit: insert event: %w", err)
	}
	return nil
}

// Record is the convenience form for a single event.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	return InTx(tx).Record(ctx, e)
}

// secretMarkers are the substrings that make a field name a credential.
//
// The match is on the *name*, not the value: a heuristic on values would both miss a
// token that does not look like one and redact an innocent description.
var secretMarkers = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey", "private_key",
	"authorization", "cookie", "credential", "recovery_code", "totp", "signature",
	"key_hash", "card_number", "cvv", "cvc",
}

// RedactedMarker replaces a redacted value.
const RedactedMarker = "[redacted]"

// IsSecretField reports whether a field name looks like a credential.
func IsSecretField(name string) bool {
	lower := strings.ToLower(name)
	for _, m := range secretMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Redact returns a copy of changes with credential-looking fields replaced.
//
// It recurses into nested maps and slices so a token inside `{"user": {...}}` is
// caught as well as a top-level one.
func Redact(changes map[string]any) map[string]any {
	if changes == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(changes))
	for k, v := range changes {
		if IsSecretField(k) {
			out[k] = RedactedMarker
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		return Redact(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactValue(item)
		}
		return out
	default:
		return v
	}
}

func nullableID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableIP(s string) any {
	if s == "" {
		return nil
	}
	return s
}
