package dunning

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Hold overrides.
//
// A manual hold exists to stop automation from touching a service. That is the whole point:
// an operator who places a hold because a customer is in a dispute must not have the
// nightly dunning run suspend the service anyway, because the suspension is the thing the
// dispute is about.
//
// So overriding a hold is a different act from suspending a service. It is not "suspend
// harder"; it is "ignore the operator who asked us not to". That is why it is gated on a
// second staff signature rather than on a permission: the person asking for it is usually
// the person who placed the hold.
//
// This file is the enqueue path. The decision to allow it lives in the HTTP layer (which
// owns the approval), and the record of who overrode what lives in suspension_decisions.

// ErrHoldInPlace is returned when a suspension would override a manual hold without the
// override being requested.
var ErrHoldInPlace = errors.New("dunning: a manual hold is in place")

// OverrideRequest describes a suspension that overrides a manual hold.
type OverrideRequest struct {
	ServiceID string
	// Reason is the operator's justification. It is required: an override with no stated
	// reason is indistinguishable from a mistake after the fact.
	Reason string
	// RequestedBy is the first staff actor.
	RequestedBy string
	// ApprovedBy is the second, distinct staff actor who approved the override.
	ApprovedBy string
}

// OverrideHold enqueues a suspension even though a manual hold is in place, and records the
// decision with both actors.
//
// It re-checks everything inside one transaction: the service's generation and status, the
// hold still existing, and the two actors being distinct. A hold that was cleared a second
// ago makes this a no-op rather than a recorded override of a hold nobody has.
func (e *Engine) OverrideHold(ctx context.Context, req OverrideRequest) (Eligibility, error) {
	if req.ServiceID == "" {
		return Eligibility{}, errors.New("dunning: an override needs a service")
	}
	if req.Reason == "" {
		return Eligibility{}, errors.New("dunning: an override needs a reason")
	}
	if req.RequestedBy == "" || req.ApprovedBy == "" {
		return Eligibility{}, errors.New("dunning: an override needs a requester and an approver")
	}
	if req.RequestedBy == req.ApprovedBy {
		// The database refuses this in the approvals table; refusing it here means the
		// error names the problem instead of surfacing a constraint violation.
		return Eligibility{}, errors.New("dunning: the requester cannot approve their own override")
	}

	out := Eligibility{ServiceID: req.ServiceID}
	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status     string
			generation int64
			customerID string
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, generation, customer_id FROM services WHERE id = $1 FOR UPDATE`,
			req.ServiceID).Scan(&status, &generation, &customerID); err != nil {
			return fmt.Errorf("dunning: lock service: %w", err)
		}
		out.Generation = generation
		if status != "active" {
			return fmt.Errorf("%w: the service is %s", ErrNotEligible, status)
		}

		// The hold must still exist. If an operator cleared it, the ordinary suspend path
		// applies and no override needs recording.
		var holdID string
		err := tx.QueryRow(ctx, `
			SELECT id FROM service_holds
			WHERE service_id = $1 AND (expires_at IS NULL OR expires_at > $2)
			ORDER BY created_at DESC LIMIT 1`, req.ServiceID, e.Now()).Scan(&holdID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no hold is in place, so there is nothing to override",
				ErrNotEligible)
		}
		if err != nil {
			return fmt.Errorf("dunning: read hold: %w", err)
		}

		// Record the decision. It names both actors so "who decided this" survives the
		// person leaving the company.
		if _, err := tx.Exec(ctx, `
			INSERT INTO suspension_decisions
				(service_id, decision, reason, generation, decided_by, override_hold, approved_by)
			VALUES ($1, 'suspend', $2, $3, $4, true, $5)`,
			req.ServiceID, "hold override: "+req.Reason, generation,
			req.RequestedBy, req.ApprovedBy); err != nil {
			return fmt.Errorf("dunning: record override decision: %w", err)
		}

		key := fmt.Sprintf("suspend:%s:%d", req.ServiceID, generation)
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'suspend', $2, $3, 'queued')
			ON CONFLICT (operation_key) DO NOTHING`, req.ServiceID, key, generation); err != nil {
			return fmt.Errorf("dunning: enqueue override suspension: %w", err)
		}

		out.Eligible = true
		out.Reason = "hold override approved by a second staff actor"
		return nil
	})
	if err != nil {
		return Eligibility{}, err
	}
	return out, nil
}

// ActiveHold reports whether a service has an unexpired manual hold, and its id.
//
// The staff suspend path needs this to decide between "suspend" and "an override is
// required"; asking the engine keeps the definition of an active hold in one place.
func (e *Engine) ActiveHold(ctx context.Context, serviceID string) (string, bool, error) {
	var id string
	err := e.pool.QueryRow(ctx, `
		SELECT id FROM service_holds
		WHERE service_id = $1 AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY created_at DESC LIMIT 1`, serviceID, e.Now()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("dunning: read active hold: %w", err)
	}
	return id, true, nil
}

// OperationIDFor returns the id of the operation a request enqueued.
//
// The engine's enqueue paths are idempotent by operation key, which means they cannot
// return the id directly (a duplicate is a no-op, not a new row). The caller reads it back
// by key. Putting that lookup here keeps the key format — which is a contract between the
// engine and the worker — in the package that owns it.
func (e *Engine) OperationIDFor(ctx context.Context, kind, serviceID string, generation int64) (string, error) {
	key := fmt.Sprintf("%s:%s:%d", kind, serviceID, generation)
	var id string
	err := e.pool.QueryRow(ctx, `
		SELECT id FROM service_operations WHERE operation_key = $1`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("dunning: no %s operation for service %s at generation %d",
			kind, serviceID, generation)
	}
	if err != nil {
		return "", fmt.Errorf("dunning: read operation id: %w", err)
	}
	return id, nil
}
