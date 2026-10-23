package service

// The queue rows and the operator actions that need one (SVC-13, SVC-14, SVC-16, SVC-22,
// CT-12).
//
// Everything here obeys one rule: an action that changes something outside this database is a
// *row* in `service_operations`, written in the same transaction as the local change. The
// worker is the only component that talks to a provisioner or a gateway, which is what keeps the
// fenced, leased, reconciled queue the only path to a remote effect.
//
// The helper is here rather than in internal/provisioning because the enqueue must be part of
// the caller's transaction: a resize written after the commit would be a subscription repriced
// with no server change queued whenever the process died in between.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// enqueueOperationPayload inserts a provisioning operation with the instruction it carries.
//
// `payload` is the operation's own immutable intent — the plan a resize must reach, the
// subscription id a cancellation must end — and it is why the worker never has to derive the
// target from a table that has already moved on. It is merged into the row on replay
// (`ON CONFLICT`), because a second enqueue under the same key is the same instruction or a
// corrected one, and the newest statement of it is the one that must run.
func enqueueOperationPayload(ctx context.Context, tx pgx.Tx, serviceID, kind, key string,
	generation int64, payload map[string]any) (string, error) {
	encoded := []byte("{}")
	if len(payload) > 0 {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return "", fmt.Errorf("service: encode %s operation payload: %w", kind, err)
		}
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status, payload)
		VALUES ($1, $2, $3, $4, 'queued', $5::jsonb)
		ON CONFLICT (operation_key) DO UPDATE
			SET payload = EXCLUDED.payload
		RETURNING id`, serviceID, kind, key, generation, encoded).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("service: enqueue %s operation: %w", kind, err)
	}
	return id, nil
}

// enqueueResize tells the provisioner which plan the service is now sold as.
//
// It is separate from `enqueueOperation` because a resize without a target is an operation that
// cannot be executed: the worker would have to guess, and guessing a plan means resizing a paid
// server to the wrong size and reporting success.
func enqueueResize(ctx context.Context, tx pgx.Tx, serviceID string, target priceVersion,
	generation int64) (string, error) {
	payload := map[string]any{"price_version_id": target.ID}
	if target.ProvisionPlan != "" {
		payload["plan_id"] = target.ProvisionPlan
	}
	if target.ProvisionRegion != "" {
		payload["region"] = target.ProvisionRegion
	}
	return enqueueOperationPayload(ctx, tx, serviceID, "resize",
		fmt.Sprintf("resize:%s:%s:%d", serviceID, target.ID, generation), generation, payload)
}
