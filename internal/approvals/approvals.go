// Package approvals implements the second signature on high-value actions.
//
// The policy the product needs is not "administrators may do anything". It is "one
// person may not move a large amount of money, or destroy a batch of infrastructure,
// alone". That is a property of the *action*, not of the role: an administrator is
// exactly the person who most needs a second signature, because an administrator is who
// an attacker with a stolen session becomes.
//
// Two details make it more than a checkbox:
//
//   - The threshold is configuration, so raising it is an operational decision with a
//     paper trail, not a code change.
//   - The database refuses a self-approval (approvals_distinct_actors). The service
//     checks it too, with a better error, but the constraint is what makes it true.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Action names. They match the CHECK on the table.
const (
	ActionRefund         = "refund"
	ActionTerminateBatch = "terminate_batch"
	ActionHoldOverride   = "hold_override"
)

// Statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
)

var (
	// ErrNotFound is returned for an unknown approval.
	ErrNotFound = errors.New("approvals: not found")
	// ErrSelfApproval is returned when the requester tries to approve their own action.
	ErrSelfApproval = errors.New("approvals: the requester cannot approve their own action")
	// ErrNotPending is returned when the approval has already been decided.
	ErrNotPending = errors.New("approvals: approval is no longer pending")
)

// Defaults for the configured thresholds.
const (
	// DefaultRefundThresholdMinor is 500.00 in a two-decimal currency.
	DefaultRefundThresholdMinor int64 = 50000
	// DefaultTerminationBatchThreshold is the number of services in one batch above
	// which a second signature is required.
	DefaultTerminationBatchThreshold = 10
)

// Approval is one request for a second signature.
type Approval struct {
	ID          string     `json:"id"`
	Action      string     `json:"action"`
	ReferenceID string     `json:"reference_id"`
	RequestedBy string     `json:"requested_by"`
	ApprovedBy  string     `json:"approved_by,omitempty"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	CreatedAt   time.Time  `json:"created_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// Service reads and writes approvals.
type Service struct {
	pool *store.Pool
	// RefundThresholdMinor is the amount at or above which a refund needs a second
	// signature. Zero disables the check, which an installation may legitimately want
	// (a single-operator deployment) but must choose explicitly.
	RefundThresholdMinor int64
	// TerminationBatchThreshold is the batch size at or above which a termination batch
	// needs a second signature.
	TerminationBatchThreshold int
	Now                       func() time.Time
}

// New constructs a Service, reading the thresholds from the environment.
//
// A malformed threshold is an error rather than a silent fallback to the default:
// falling back would quietly *lower* the bar for a second signature if the operator
// typed the value wrong.
func New(pool *store.Pool) (*Service, error) {
	refund := DefaultRefundThresholdMinor
	if raw := strings.TrimSpace(os.Getenv("BILLING_REFUND_APPROVAL_THRESHOLD_MINOR")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("approvals: BILLING_REFUND_APPROVAL_THRESHOLD_MINOR must be a "+
				"non-negative integer, got %q", raw)
		}
		refund = n
	}
	batch := DefaultTerminationBatchThreshold
	if raw := strings.TrimSpace(os.Getenv("BILLING_TERMINATION_BATCH_APPROVAL")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("approvals: BILLING_TERMINATION_BATCH_APPROVAL must be a "+
				"non-negative integer, got %q", raw)
		}
		batch = n
	}
	return &Service{
		pool:                      pool,
		RefundThresholdMinor:      refund,
		TerminationBatchThreshold: batch,
		Now:                       func() time.Time { return time.Now().UTC() },
	}, nil
}

// RefundNeedsApproval reports whether an amount requires a second signature.
func (s *Service) RefundNeedsApproval(minor int64) bool {
	return s.RefundThresholdMinor > 0 && minor >= s.RefundThresholdMinor
}

// Request records a pending approval inside the caller's transaction.
//
// A second request for the same (action, reference) returns the existing pending row
// rather than creating a second one: two operators asking at once should end up
// approving the same decision, and a duplicate pending row is how an action gets
// approved twice.
func (s *Service) Request(ctx context.Context, tx pgx.Tx, action, referenceID, requestedBy, reason string) (Approval, error) {
	if !knownAction(action) {
		return Approval{}, fmt.Errorf("approvals: unknown action %q", action)
	}
	if referenceID == "" || requestedBy == "" {
		return Approval{}, errors.New("approvals: reference and requester are required")
	}

	existing, err := s.pendingFor(ctx, tx, action, referenceID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Approval{}, err
	}

	var a Approval
	err = tx.QueryRow(ctx, `
		INSERT INTO approvals (action, reference_id, requested_by, status, reason)
		VALUES ($1, $2, $3, 'pending', $4)
		RETURNING id, action, reference_id, requested_by, status, reason, created_at`,
		action, referenceID, requestedBy, reason).Scan(
		&a.ID, &a.Action, &a.ReferenceID, &a.RequestedBy, &a.Status, &a.Reason, &a.CreatedAt)
	if err != nil {
		return Approval{}, fmt.Errorf("approvals: request: %w", err)
	}
	return a, nil
}

// pendingFor returns the existing pending approval for an action and reference.
func (s *Service) pendingFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, action, referenceID string) (Approval, error) {
	var a Approval
	err := q.QueryRow(ctx, `
		SELECT id, action, reference_id, requested_by, status, reason, created_at
		FROM approvals
		WHERE action = $1 AND reference_id = $2 AND status = 'pending'
		ORDER BY created_at DESC
		LIMIT 1`, action, referenceID).Scan(
		&a.ID, &a.Action, &a.ReferenceID, &a.RequestedBy, &a.Status, &a.Reason, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("approvals: read pending: %w", err)
	}
	return a, nil
}

// Get returns one approval.
func (s *Service) Get(ctx context.Context, id string) (Approval, error) {
	return scanOne(s.pool.QueryRow(ctx, selectColumns+` WHERE id = $1`, id))
}

// PendingFor returns the pending approval for an action and reference, if any.
func (s *Service) PendingFor(ctx context.Context, action, referenceID string) (Approval, bool, error) {
	a, err := s.pendingFor(ctx, s.pool, action, referenceID)
	if errors.Is(err, ErrNotFound) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	return a, true, nil
}

// Approve records the second signature.
//
// The approver must be a different person from the requester. That is checked here for a
// clear error and enforced by the table's CHECK so a bug cannot bypass it.
func (s *Service) Approve(ctx context.Context, id, approvedBy string) (Approval, error) {
	return s.decide(ctx, id, approvedBy, StatusApproved, "")
}

// Reject records a refusal.
func (s *Service) Reject(ctx context.Context, id, rejectedBy, reason string) (Approval, error) {
	return s.decide(ctx, id, rejectedBy, StatusRejected, reason)
}

func (s *Service) decide(ctx context.Context, id, actor, status, reason string) (Approval, error) {
	if actor == "" {
		return Approval{}, errors.New("approvals: an actor is required")
	}
	var out Approval
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			requestedBy string
			current     string
		)
		err := tx.QueryRow(ctx, `
			SELECT requested_by, status FROM approvals WHERE id = $1 FOR UPDATE`, id).
			Scan(&requestedBy, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("approvals: lock: %w", err)
		}
		if current != StatusPending {
			return fmt.Errorf("%w: it is %s", ErrNotPending, current)
		}
		if requestedBy == actor {
			return ErrSelfApproval
		}

		reasonExpr := "reason"
		args := []any{id, status, actor, s.Now()}
		if reason != "" {
			reasonExpr = "reason || ' | rejected: ' || $5"
			args = append(args, reason)
		}
		stmt := fmt.Sprintf(`
			UPDATE approvals
			SET status = $2, approved_by = $3, decided_at = $4, reason = %s
			WHERE id = $1
			RETURNING id, action, reference_id, requested_by, approved_by, status,
			          reason, created_at, decided_at`, reasonExpr)
		row := tx.QueryRow(ctx, stmt, args...)
		var (
			a         Approval
			approved  *string
			decidedAt *time.Time
		)
		if err := row.Scan(&a.ID, &a.Action, &a.ReferenceID, &a.RequestedBy, &approved,
			&a.Status, &a.Reason, &a.CreatedAt, &decidedAt); err != nil {
			return fmt.Errorf("approvals: decide: %w", err)
		}
		if approved != nil {
			a.ApprovedBy = *approved
		}
		a.DecidedAt = decidedAt
		out = a
		return nil
	})
	if err != nil {
		return Approval{}, err
	}
	return out, nil
}

// Filter narrows a list query.
type Filter struct {
	Status string
	Action string
	Actor  string
}

// List returns approvals newest first.
func (s *Service) List(ctx context.Context, f Filter, limit int, cursor string) ([]Approval, string, error) {
	limit = paginate.Clamp(limit)
	args := []any{}
	where := []string{"true"}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Actor != "" {
		args = append(args, f.Actor)
		where = append(where, fmt.Sprintf("(requested_by = $%d OR approved_by = $%d)", len(args), len(args)))
	}
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	if cursor != "" {
		when, perr := time.Parse(time.RFC3339Nano, sortKey)
		if perr != nil {
			return nil, "", fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	stmt := selectColumns + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", fmt.Errorf("approvals: list: %w", err)
	}
	defer rows.Close()

	var out []Approval
	for rows.Next() {
		a, err := scanRow(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("approvals: list: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

const selectColumns = `
	SELECT id, action, reference_id, requested_by, COALESCE(approved_by::text, ''),
	       status, reason, created_at, decided_at
	FROM approvals`

type scanner interface {
	Scan(dest ...any) error
}

func scanOne(row pgx.Row) (Approval, error) {
	a, err := scanRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

func scanRow(row scanner) (Approval, error) {
	var (
		a         Approval
		decidedAt *time.Time
	)
	if err := row.Scan(&a.ID, &a.Action, &a.ReferenceID, &a.RequestedBy, &a.ApprovedBy,
		&a.Status, &a.Reason, &a.CreatedAt, &decidedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("approvals: scan: %w", err)
	}
	a.DecidedAt = decidedAt
	return a, nil
}

func knownAction(a string) bool {
	switch a {
	case ActionRefund, ActionTerminateBatch, ActionHoldOverride:
		return true
	}
	return false
}
