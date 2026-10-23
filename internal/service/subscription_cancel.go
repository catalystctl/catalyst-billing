package service

// Cancelling a provider-side subscription id (SVC-14, CT-12).
//
// A service can carry two billing relationships. The one this panel owns is
// `services.subscription_id` → `subscriptions`, and its cancellation is the cancellation queue.
// The other is a *legacy* or gateway-side subscription id: somebody else's record, with somebody
// else's identifier, that will keep charging the customer until that somebody is told to stop.
// Ending it is a call to the payment gateway, so it is a queue row like every other remote
// effect, and this file is the transaction that writes it.
//
// # Why the id is recorded before the call, and cleared after it
//
// The operator may be pasting an id from the provider's dashboard. Storing it first makes the
// panel's own record agree with the provider's before anything is attempted, which is what makes
// the operation retryable after a crash: the worker reads the id from the row, not from a screen
// that has since been closed. On success the column is emptied — a column that still names a
// cancelled subscription is an invitation to cancel it twice — and the cancelled id lives on in
// the audit row and in the operation payload.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrNoExternalSubscription is returned when there is no provider-side subscription to cancel.
//
// It is distinct from ErrNotFound because the service exists and the operator needs to be told
// what is missing rather than shown a 404 for a row they are looking at.
var ErrNoExternalSubscription = errors.New("service: this service has no external subscription id")

// ExternalSubscriptionRequest asks to cancel a provider-side subscription.
type ExternalSubscriptionRequest struct {
	ServiceID string
	// SubscriptionID is the provider's id. When empty the service's stored id is used, so a
	// retry or a second operator does not have to retype it.
	SubscriptionID string
	Reason         string
	ActorID        string
}

// SubscriptionCancellation is what was queued.
type SubscriptionCancellation struct {
	ServiceID      string
	SubscriptionID string
	OperationID    string
	// AlreadyCancelled is true when the provider-side subscription had already been cancelled
	// and the panel's record was cleared, so nothing was queued.
	AlreadyCancelled bool
}

// CancelExternalSubscription records the id and queues its cancellation.
//
// It refuses a terminated service (there is nothing left to stop) and it refuses an empty id
// (there is nothing to name), and in both cases it says which, because the two remedies differ.
func (s *Store) CancelExternalSubscription(ctx context.Context,
	req ExternalSubscriptionRequest) (SubscriptionCancellation, error) {
	var out SubscriptionCancellation

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status     string
			stored     string
			generation int64
		)
		err := tx.QueryRow(ctx, `
			SELECT status, COALESCE(external_subscription_id, ''), generation
			FROM services WHERE id = $1 FOR UPDATE`, req.ServiceID).
			Scan(&status, &stored, &generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}
		if status == "terminated" || status == "terminating" {
			return fmt.Errorf("%w: a %s service has nothing left to stop", ErrNotCancellable, status)
		}

		subscriptionID := strings.TrimSpace(req.SubscriptionID)
		if subscriptionID == "" {
			subscriptionID = stored
		}
		if subscriptionID == "" {
			return ErrNoExternalSubscription
		}
		if len(subscriptionID) > 200 {
			return fmt.Errorf("%w: the subscription id is longer than 200 characters", ErrInvalidInput)
		}
		out = SubscriptionCancellation{ServiceID: req.ServiceID, SubscriptionID: subscriptionID}

		if subscriptionID != stored {
			if _, err := tx.Exec(ctx, `
				UPDATE services SET external_subscription_id = $2, updated_at = now()
				WHERE id = $1`, req.ServiceID, subscriptionID); err != nil {
				return fmt.Errorf("service: record external subscription id: %w", err)
			}
		}

		// The key is the id itself, so a double click, a retry and a second operator all
		// resolve to one queue row for one subscription.
		opID, err := enqueueOperationPayload(ctx, tx, req.ServiceID, "cancel_subscription",
			fmt.Sprintf("cancel_subscription:%s:%s", req.ServiceID, subscriptionID), generation,
			map[string]any{
				"subscription_id": subscriptionID,
				"reason":          req.Reason,
			})
		if err != nil {
			return err
		}
		out.OperationID = opID

		return s.audit(ctx, tx, "service.subscription_cancel_requested", "service",
			req.ServiceID, req.ActorID, map[string]any{
				"external_subscription_id": subscriptionID,
				"reason":                   req.Reason,
				"operation_id":             opID,
			})
	})
	if err != nil {
		return SubscriptionCancellation{}, err
	}
	return out, nil
}

// CancelOrderExternalSubscriptions cancels the provider-side subscription of every service of an
// order (CT-12's order-level action).
//
// One order can hold several services and each may carry its own provider subscription, so the
// action is a loop over the order's services rather than one call: an order-level button that
// silently stopped at the first service would leave the customer charged for the rest.
//
// A service with no external subscription is skipped rather than refused — the common case is an
// order whose services are billed by this panel alone, and refusing the whole order for that
// would make the button useless exactly where it matters least. Services that *fail* are
// returned in Failures so the caller can report a partial success honestly.
func (s *Store) CancelOrderExternalSubscriptions(ctx context.Context, orderID, actorID, reason string) (
	[]SubscriptionCancellation, []string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM services
		WHERE order_id = $1 AND COALESCE(external_subscription_id, '') <> ''
		  AND status NOT IN ('terminated', 'terminating')
		ORDER BY created_at`, orderID)
	if err != nil {
		return nil, nil, fmt.Errorf("service: list order services: %w", err)
	}
	var serviceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("service: scan order service: %w", err)
		}
		serviceIDs = append(serviceIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("service: list order services: %w", err)
	}
	rows.Close()
	if len(serviceIDs) == 0 {
		return nil, nil, ErrNoExternalSubscription
	}

	var (
		queued   []SubscriptionCancellation
		failures []string
	)
	for _, id := range serviceIDs {
		out, err := s.CancelExternalSubscription(ctx, ExternalSubscriptionRequest{
			ServiceID: id,
			Reason:    reason,
			ActorID:   actorID,
		})
		if err != nil {
			failures = append(failures, id+": "+err.Error())
			continue
		}
		queued = append(queued, out)
	}
	return queued, failures, nil
}
