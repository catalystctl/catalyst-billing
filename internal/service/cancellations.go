package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Cancellations (CAN-01…CAN-06, SVC-09).
//
// The workflow is deliberately two-step for the destructive case:
//
//	customer asks  ->  a row appears in the staff queue  ->  staff or the cron executes it
//
// An end-of-period request needs no second step: it changes nothing that is already
// happening (the schedule stops at a date the customer has paid to), so it is applied
// immediately and recorded as `scheduled`. An immediate request destroys time the customer
// has already paid for, so it is recorded as `pending` and waits for an operator. That is
// the design note in the parity backlog, and it is also the only version that leaves an
// audit trail of who decided to take the service away.
//
// `ExecuteDue` is the automation half: at the paid-through date a scheduled cancellation
// becomes an execution. It enqueues the *existing* terminate operation through the same
// operation key the staff termination handler uses, so a service can never be terminated
// twice by two code paths.

// MaxReasonLength matches the CHECK on service_cancellations.reason. Validated here as well
// so a caller gets a clear domain error instead of a constraint violation.
const MaxReasonLength = 255

// Cancellation is one cancellation request.
type Cancellation struct {
	ID          string
	ServiceID   string
	RequestedBy string
	Reason      string
	Type        string
	Status      string
	EffectiveAt *time.Time
	ExecutedAt  *time.Time
	CreatedAt   time.Time

	// The queue shows who and what, not just an id. These are joined, not stored, so a
	// rename of a product does not rewrite history.
	CustomerID    string
	CustomerName  string
	CustomerEmail string
	ProductName   string
	ServiceStatus string
}

// CancellationRequest is a request to cancel a service.
type CancellationRequest struct {
	ServiceID string
	// UserID is the actor. On the customer path it is also the scope: the service is
	// matched through customer_accounts in SQL, so substituting another customer's service
	// id returns ErrNotFound rather than acting on it.
	UserID string
	Reason string
	Type   string
	// Staff bypasses the customer scoping. It is a separate field rather than a separate
	// method so the validation cannot drift between the two paths.
	Staff bool
}

// CancellationFilter is the staff queue's filter.
type CancellationFilter struct {
	// Status is one of pending/scheduled/executed/cancelled/rejected; empty means "all
	// open ones" (pending and scheduled), which is what a queue shows by default.
	Status string
	// ServiceID narrows the queue to one service.
	ServiceID string
	Limit     int
}

// ExecutionResult reports what an execution did.
type ExecutionResult struct {
	CancellationID string
	ServiceID      string
	// OperationID is the terminate operation that was enqueued, empty when the service was
	// already terminated and there was nothing to do.
	OperationID string
	// AlreadyTerminated is true when the service was already gone. The request is still
	// marked executed: the customer's intent has been satisfied.
	AlreadyTerminated bool
}

// validate checks a request before any row is touched.
func (r CancellationRequest) validate() error {
	if strings.TrimSpace(r.ServiceID) == "" {
		return fmt.Errorf("%w: a service id is required", ErrInvalidInput)
	}
	switch r.Type {
	case CancelImmediate, CancelEndOfPeriod:
	case "":
		return fmt.Errorf("%w: type must be immediate or end_of_period", ErrInvalidInput)
	default:
		return fmt.Errorf("%w: type must be immediate or end_of_period", ErrInvalidInput)
	}
	if utf8.RuneCountInString(r.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: the reason is longer than %d characters", ErrInvalidInput, MaxReasonLength)
	}
	return nil
}

// RequestCancellation records a customer's cancellation request.
//
// The service is resolved through the caller's customer account *in the same query that
// locks it*, which is what makes an identifier substitution return ErrNotFound instead of
// cancelling somebody else's service.
func (s *Store) RequestCancellation(ctx context.Context, req CancellationRequest) (Cancellation, error) {
	if err := req.validate(); err != nil {
		return Cancellation{}, err
	}
	req.Staff = false
	return s.requestCancellation(ctx, req)
}

// RequestCancellationAsStaff records a request on any service, on a customer's behalf.
func (s *Store) RequestCancellationAsStaff(ctx context.Context, req CancellationRequest) (Cancellation, error) {
	if err := req.validate(); err != nil {
		return Cancellation{}, err
	}
	req.Staff = true
	return s.requestCancellation(ctx, req)
}

func (s *Store) requestCancellation(ctx context.Context, req CancellationRequest) (Cancellation, error) {
	var out Cancellation
	now := s.clock()

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status         string
			subscriptionID *string
			customerID     string
		)

		var err error
		if req.Staff {
			err = tx.QueryRow(ctx, `
				SELECT s.status, s.subscription_id, s.customer_id
				FROM services s WHERE s.id = $1
				FOR UPDATE OF s`, req.ServiceID).
				Scan(&status, &subscriptionID, &customerID)
		} else {
			err = tx.QueryRow(ctx, `
				SELECT s.status, s.subscription_id, s.customer_id
				FROM services s
				JOIN customer_accounts ca ON ca.customer_id = s.customer_id
				WHERE s.id = $1 AND ca.user_id = $2
				FOR UPDATE OF s`, req.ServiceID, req.UserID).
				Scan(&status, &subscriptionID, &customerID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}

		switch status {
		case "active", "suspended":
			// Both are cancellable: a suspended service is still a service the customer
			// pays for and may want to stop.
		default:
			return fmt.Errorf("%w: its status is %s", ErrNotCancellable, status)
		}

		// One open request per service. A second one would be a second reason and a second
		// answer to the same question, which is how a queue grows contradictory rows.
		var existing string
		err = tx.QueryRow(ctx, `
			SELECT id FROM service_cancellations
			WHERE service_id = $1 AND status IN ('pending', 'scheduled')
			LIMIT 1`, req.ServiceID).Scan(&existing)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrAlreadyRequested, existing)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("service: read open cancellation: %w", err)
		}

		var effectiveAt *time.Time
		cancelStatus := StatusPending

		if req.Type == CancelEndOfPeriod {
			if subscriptionID == nil {
				return fmt.Errorf("%w: this service has no billing schedule to stop", ErrNotCancellable)
			}
			sub, err := scanSubscription(tx.QueryRow(ctx, `
				SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`,
				*subscriptionID))
			if err != nil {
				return err
			}
			end := sub.CurrentPeriodEnd
			effectiveAt = &end
			cancelStatus = StatusScheduled

			// The schedule stops at the paid-through date. This is the only state change an
			// end-of-period request makes, and it is what makes the renewal engine skip it.
			if _, err := tx.Exec(ctx, `
				UPDATE subscriptions
				SET cancel_at_period_end = true,
				    cancelled_at = COALESCE(cancelled_at, $2),
				    version = version + 1
				WHERE id = $1`, sub.ID, now); err != nil {
				return fmt.Errorf("service: schedule cancellation: %w", err)
			}
		} else {
			// Immediate: the request takes effect when somebody executes it. Recording the
			// request time here would claim an effect that has not happened.
			effectiveAt = &now
		}

		var (
			id        string
			createdAt time.Time
		)
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_cancellations
				(service_id, requested_by, reason, cancellation_type, status, effective_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, created_at`,
			req.ServiceID, nullable(req.UserID), strings.TrimSpace(req.Reason),
			req.Type, cancelStatus, effectiveAt).Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("service: record cancellation: %w", err)
		}

		if err := s.audit(ctx, tx, "service.cancellation_request", "service", req.ServiceID,
			req.UserID, map[string]any{
				"cancellation_id": id,
				"type":            req.Type,
				"status":          cancelStatus,
				"reason":          req.Reason,
			}); err != nil {
			return err
		}

		out = Cancellation{
			ID: id, ServiceID: req.ServiceID, RequestedBy: req.UserID,
			Reason: strings.TrimSpace(req.Reason), Type: req.Type, Status: cancelStatus,
			EffectiveAt: effectiveAt, CreatedAt: createdAt, CustomerID: customerID,
		}
		return nil
	})
	if err != nil {
		return Cancellation{}, err
	}
	return out, nil
}

// Cancellations lists the staff queue.
func (s *Store) Cancellations(ctx context.Context, filter CancellationFilter) ([]Cancellation, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// The status is a closed set, so an unknown value is an empty result rather than an
	// error: a filter bar that sends a stale value should show nothing, not fail.
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.service_id, COALESCE(c.requested_by::text, ''), c.reason,
		       c.cancellation_type, c.status, c.effective_at, c.executed_at, c.created_at,
		       s.customer_id, cu.name, cu.email, p.name, s.status
		FROM service_cancellations c
		JOIN services s ON s.id = c.service_id
		JOIN customers cu ON cu.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		WHERE ($1 = '' AND c.status IN ('pending', 'scheduled') OR c.status = $1)
		  AND ($2 = '' OR c.service_id::text = $2)
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT $3`, filter.Status, filter.ServiceID, limit)
	if err != nil {
		return nil, fmt.Errorf("service: list cancellations: %w", err)
	}
	defer rows.Close()

	out := []Cancellation{}
	for rows.Next() {
		var c Cancellation
		if err := rows.Scan(&c.ID, &c.ServiceID, &c.RequestedBy, &c.Reason, &c.Type, &c.Status,
			&c.EffectiveAt, &c.ExecutedAt, &c.CreatedAt, &c.CustomerID, &c.CustomerName,
			&c.CustomerEmail, &c.ProductName, &c.ServiceStatus); err != nil {
			return nil, fmt.Errorf("service: scan cancellation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Cancellation returns one request, or ErrNotFound.
func (s *Store) Cancellation(ctx context.Context, id string) (Cancellation, error) {
	var c Cancellation
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.service_id, COALESCE(c.requested_by::text, ''), c.reason,
		       c.cancellation_type, c.status, c.effective_at, c.executed_at, c.created_at,
		       s.customer_id, cu.name, cu.email, p.name, s.status
		FROM service_cancellations c
		JOIN services s ON s.id = c.service_id
		JOIN customers cu ON cu.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		WHERE c.id = $1`, id).
		Scan(&c.ID, &c.ServiceID, &c.RequestedBy, &c.Reason, &c.Type, &c.Status,
			&c.EffectiveAt, &c.ExecutedAt, &c.CreatedAt, &c.CustomerID, &c.CustomerName,
			&c.CustomerEmail, &c.ProductName, &c.ServiceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cancellation{}, ErrNotFound
	}
	if err != nil {
		return Cancellation{}, fmt.Errorf("service: load cancellation: %w", err)
	}
	return c, nil
}

// CancellationForService returns the open request for a service, if there is one.
//
// A non-empty userID scopes the read to that user's customer account *in SQL*, so a customer
// substituting another customer's service id gets ErrNotFound rather than a peek at their
// cancellation state. An empty userID is the staff read, which is what the queue uses.
//
// The portal uses it to show "cancellation requested" instead of offering the button again.
func (s *Store) CancellationForService(ctx context.Context, serviceID, userID string) (Cancellation, error) {
	var c Cancellation
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.service_id, COALESCE(c.requested_by::text, ''), c.reason,
		       c.cancellation_type, c.status, c.effective_at, c.executed_at, c.created_at,
		       s.customer_id, cu.name, cu.email, p.name, s.status
		FROM service_cancellations c
		JOIN services s ON s.id = c.service_id
		JOIN customers cu ON cu.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		WHERE c.service_id = $1 AND c.status IN ('pending', 'scheduled')
		  AND ($2 = '' OR EXISTS (
			SELECT 1 FROM customer_accounts ca
			WHERE ca.customer_id = s.customer_id AND ca.user_id::text = $2))
		ORDER BY c.created_at DESC LIMIT 1`, serviceID, userID).
		Scan(&c.ID, &c.ServiceID, &c.RequestedBy, &c.Reason, &c.Type, &c.Status,
			&c.EffectiveAt, &c.ExecutedAt, &c.CreatedAt, &c.CustomerID, &c.CustomerName,
			&c.CustomerEmail, &c.ProductName, &c.ServiceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cancellation{}, ErrNotFound
	}
	if err != nil {
		return Cancellation{}, fmt.Errorf("service: load cancellation: %w", err)
	}
	return c, nil
}

// ExecuteCancellation executes a pending immediate request.
//
// It enqueues the existing terminate operation rather than calling the adapter, because
// termination is not reconcilable: if the adapter call is made and the process dies, the
// resource may or may not be gone. The queue is what makes that recoverable.
//
// Executing a *scheduled* request early is refused: an end-of-period cancellation means the
// customer keeps the service until the date they paid to, and destroying it before then is a
// refund event, not an execution.
func (s *Store) ExecuteCancellation(ctx context.Context, id, actorID, reason string) (ExecutionResult, error) {
	var out ExecutionResult
	now := s.clock()

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			serviceID string
			status    string
			ctype     string
		)
		err := tx.QueryRow(ctx, `
			SELECT service_id, status, cancellation_type
			FROM service_cancellations WHERE id = $1 FOR UPDATE`, id).
			Scan(&serviceID, &status, &ctype)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock cancellation: %w", err)
		}
		if status != StatusPending {
			return fmt.Errorf("%w: its status is %s", ErrNotPending, status)
		}
		if ctype != CancelImmediate {
			return fmt.Errorf("%w: only an immediate cancellation is executed; a scheduled one runs at its effective date", ErrNotPending)
		}

		out.CancellationID = id
		out.ServiceID = serviceID

		var (
			serviceStatus string
			generation    int64
			externalID    string
		)
		err = tx.QueryRow(ctx, `
			SELECT status, generation, COALESCE(external_id, '')
			FROM services WHERE id = $1 FOR UPDATE`, serviceID).
			Scan(&serviceStatus, &generation, &externalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}

		if serviceStatus == "terminated" {
			// Nothing to destroy: the intent is already satisfied. Recording it as executed
			// keeps the queue honest instead of leaving a request open forever.
			out.AlreadyTerminated = true
		} else {
			opID, err := enqueueOperation(ctx, tx, serviceID, "terminate",
				fmt.Sprintf("terminate:%s:%d", serviceID, generation), generation)
			if err != nil {
				return err
			}
			out.OperationID = opID
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_cancellations
			SET status = 'executed', executed_at = $2
			WHERE id = $1`, id, now); err != nil {
			return fmt.Errorf("service: mark cancellation executed: %w", err)
		}

		// Billing stops now as well: a terminated service must not renew.
		if _, err := tx.Exec(ctx, `
			UPDATE subscriptions sub
			SET cancel_at_period_end = true,
			    cancelled_at = COALESCE(sub.cancelled_at, $2),
			    version = sub.version + 1
			WHERE sub.id = (SELECT subscription_id FROM services WHERE id = $1)`,
			serviceID, now); err != nil {
			return fmt.Errorf("service: stop billing: %w", err)
		}

		return s.audit(ctx, tx, "service.cancellation_execute", "service", serviceID, actorID,
			map[string]any{
				"cancellation_id":    id,
				"operation_id":       out.OperationID,
				"already_terminated": out.AlreadyTerminated,
				"reason":             reason,
			})
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	return out, nil
}

// RejectCancellation rejects a request and clears the schedule it may have set.
//
// An end-of-period request has already changed `cancel_at_period_end`, so rejecting it must
// undo that. Leaving the flag set would make the queue say "rejected" while the renewal
// engine quietly ended the subscription — the customer would be cancelled by a record that
// says they were not.
func (s *Store) RejectCancellation(ctx context.Context, id, actorID, reason string) (Cancellation, error) {
	var out Cancellation

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			serviceID string
			status    string
			ctype     string
		)
		err := tx.QueryRow(ctx, `
			SELECT service_id, status, cancellation_type
			FROM service_cancellations WHERE id = $1 FOR UPDATE`, id).
			Scan(&serviceID, &status, &ctype)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock cancellation: %w", err)
		}
		if status != StatusPending && status != StatusScheduled {
			return fmt.Errorf("%w: its status is %s", ErrNotPending, status)
		}

		// Only clear the schedule when no other open request still wants it.
		if ctype == CancelEndOfPeriod {
			if _, err := tx.Exec(ctx, `
				UPDATE subscriptions sub
				SET cancel_at_period_end = false,
				    cancelled_at = NULL,
				    version = sub.version + 1
				WHERE sub.id = (SELECT subscription_id FROM services WHERE id = $1)
				  AND NOT EXISTS (
					SELECT 1 FROM service_cancellations c2
					WHERE c2.service_id = $1 AND c2.id <> $2
					  AND c2.status IN ('pending', 'scheduled'))`,
				serviceID, id); err != nil {
				return fmt.Errorf("service: clear schedule: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_cancellations SET status = 'rejected' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("service: reject cancellation: %w", err)
		}

		if err := s.audit(ctx, tx, "service.cancellation_reject", "service", serviceID, actorID,
			map[string]any{"cancellation_id": id, "reason": reason}); err != nil {
			return err
		}

		out = Cancellation{
			ID: id, ServiceID: serviceID, Type: ctype, Status: StatusRejected,
			Reason: reason,
		}
		return nil
	})
	if err != nil {
		return Cancellation{}, err
	}
	return out, nil
}

// ExecuteDueBatch bounds how many scheduled cancellations one run executes. A backlog
// drains over several ticks rather than in one long transaction that holds row locks.
const ExecuteDueBatch = 100

// ExecuteDue executes scheduled cancellations whose effective date has passed.
//
// This is the automation half of the workflow: an end-of-period cancellation stops billing
// when it is requested (the renewal engine skips it), and this ends the *service* at the
// paid-through date. Wire it into cmd/billing/jobs.go as a jobSpec:
//
//	svc.ExecuteDue(ctx)
//
// It is idempotent: the terminate operation key is derived from the service and its
// generation, so a second run cannot enqueue a second termination, and a row already marked
// executed is not selected again.
func (s *Store) ExecuteDue(ctx context.Context) ([]ExecutionResult, error) {
	now := s.clock()

	rows, err := s.pool.Query(ctx, `
		SELECT id, service_id FROM service_cancellations
		WHERE status = 'scheduled' AND effective_at IS NOT NULL AND effective_at <= $1
		ORDER BY effective_at
		LIMIT $2`, now, ExecuteDueBatch)
	if err != nil {
		return nil, fmt.Errorf("service: list due cancellations: %w", err)
	}
	var due []struct{ id, serviceID string }
	for rows.Next() {
		var id, serviceID string
		if err := rows.Scan(&id, &serviceID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("service: scan due cancellation: %w", err)
		}
		due = append(due, struct{ id, serviceID string }{id, serviceID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("service: list due cancellations: %w", err)
	}
	rows.Close()

	results := make([]ExecutionResult, 0, len(due))
	for _, d := range due {
		res, err := s.executeScheduled(ctx, d.id, d.serviceID, now)
		if err != nil {
			// One failed row must not stop the batch: the next tick retries it and the
			// operator sees the same error, whereas returning early would silently skip
			// every cancellation after it.
			results = append(results, ExecutionResult{CancellationID: d.id, ServiceID: d.serviceID})
			continue
		}
		results = append(results, res)
	}
	return results, nil
}

// executeScheduled performs one due end-of-period cancellation.
func (s *Store) executeScheduled(ctx context.Context, id, serviceID string, now time.Time) (ExecutionResult, error) {
	out := ExecutionResult{CancellationID: id, ServiceID: serviceID}

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Re-read under the lock: another worker may have executed it between the scan and
		// this transaction.
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM service_cancellations WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock cancellation: %w", err)
		}
		if status != StatusScheduled {
			return fmt.Errorf("%w: its status is %s", ErrNotPending, status)
		}

		var (
			serviceStatus string
			generation    int64
		)
		err = tx.QueryRow(ctx, `
			SELECT status, generation FROM services WHERE id = $1 FOR UPDATE`, serviceID).
			Scan(&serviceStatus, &generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}

		if serviceStatus == "terminated" {
			out.AlreadyTerminated = true
		} else {
			opID, err := enqueueOperation(ctx, tx, serviceID, "terminate",
				fmt.Sprintf("terminate:%s:%d", serviceID, generation), generation)
			if err != nil {
				return err
			}
			out.OperationID = opID
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_cancellations SET status = 'executed', executed_at = $2
			WHERE id = $1`, id, now); err != nil {
			return fmt.Errorf("service: mark cancellation executed: %w", err)
		}

		return s.audit(ctx, tx, "service.cancellation_due", "service", serviceID, "",
			map[string]any{"cancellation_id": id, "operation_id": out.OperationID})
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	return out, nil
}

// nullable renders an empty string as SQL NULL.
func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ServiceOwnedBy reports whether a service belongs to the customer account a user holds.
//
// It exists so every customer-facing route can scope *before* it prices, lists or changes
// anything: the check is one query with the ownership predicate in the WHERE clause, so a
// substituted identifier returns ErrNotFound rather than 403 — the caller cannot tell a
// service that is not theirs from one that does not exist.
func (s *Store) ServiceOwnedBy(ctx context.Context, serviceID, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return ErrNotFound
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM services s
			JOIN customer_accounts ca ON ca.customer_id = s.customer_id
			WHERE s.id = $1 AND ca.user_id = $2)`, serviceID, userID).Scan(&ok); err != nil {
		return fmt.Errorf("service: check ownership: %w", err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
