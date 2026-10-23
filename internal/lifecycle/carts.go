package lifecycle

// Abandoned carts (CT-13).
//
// A cart with something in it that nobody has touched for a day is the most valuable lead a
// shop has, and it is the one it can see: the items are chosen, the price is known, and the
// customer is registered. Paymenter's extensions subscribe to `Cart/Abandoned` for exactly
// this reason — recovery mail, a discount, a nudge — and until now Catalyst declared the kind
// and never emitted it.
//
// Three decisions:
//
//   - **The marker is the idempotence.** The row is stamped `abandoned_at` in the same
//     transaction that emits the event, and the scan only selects rows where it is null. Two
//     workers, or the same worker twice, therefore announce one cart once. An event that
//     repeated every half hour would train its reader to ignore it.
//   - **Touching the cart revives it.** The store clears `abandoned_at` whenever the customer
//     changes the cart, so a customer who comes back and then leaves again is announced
//     twice, which is correct: they abandoned twice.
//   - **Only carts with lines count.** An empty cart is not an abandonment, it is a customer
//     who looked; there is nothing to recover.
//
// The event is deferred to the transaction that stamped the row and is announced by
// store.Pool.inTx after the commit, so a rollback announces nothing.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/jackc/pgx/v5"
)

const (
	// AbandonCartJob is the job name the cron history records.
	AbandonCartJob = "abandoned_carts"
	// AbandonCartInterval is how often the rule looks. A cart does not become abandoned
	// twice in half an hour, so a longer interval than the order rules is honest, but it
	// matches them so an operator has one mental model for "the lifecycle rules".
	AbandonCartInterval = 30 * time.Minute
	// AbandonCartAfterHours is the idle threshold. A day is the smallest window in which
	// "they walked away" is a better explanation than "they are asleep", which is the only
	// question this rule can actually answer.
	AbandonCartAfterHours = 24
	// AbandonCartKillSwitch is the operator's brake.
	AbandonCartKillSwitch = "BILLING_DISABLE_CART_ABANDON"
)

// AbandonedCart is one cart the rule announced.
type AbandonedCart struct {
	CartID     string
	CustomerID string
	ItemCount  int
}

// CartAbandonRun is what one pass did.
type CartAbandonRun struct {
	// Scanned is how many live carts were considered.
	Scanned int
	// Abandoned is what was announced.
	Abandoned []AbandonedCart
	// Reason is set when the rule did nothing: the kill switch, or a bad threshold.
	Reason string
	// Failures names carts whose announcement failed. A failure for one cart does not stop
	// the pass: the next one may well succeed, and a partial run is better than none.
	Failures []string
}

// AbandonStaleCarts announces every live cart that has sat untouched for AbandonCartAfterHours.
func (r *Runner) AbandonStaleCarts(ctx context.Context) (CartAbandonRun, error) {
	run := CartAbandonRun{}
	if disabled(AbandonCartKillSwitch) {
		run.Reason = ReasonKillSwitch
		return run, nil
	}

	cutoff := r.now().Add(-AbandonCartAfterHours * time.Hour)
	ids, err := r.scanLiveCarts(ctx, cutoff, r.batch())
	if err != nil {
		return run, err
	}
	run.Scanned = len(ids)

	for _, id := range ids {
		cart, ok, err := r.abandonCart(ctx, id, cutoff)
		if err != nil {
			run.Failures = append(run.Failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if ok {
			run.Abandoned = append(run.Abandoned, cart)
		}
	}
	return run, nil
}

// scanLiveCarts lists carts eligible for abandonment, oldest first.
//
// The item check is in SQL rather than in Go: a cart with no lines is not a lead, and loading
// every empty cart to discover that would make the scan proportional to the customer list.
func (r *Runner) scanLiveCarts(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id::text
		FROM carts c
		WHERE c.abandoned_at IS NULL
		  AND c.updated_at < $1
		  AND EXISTS (SELECT 1 FROM cart_items ci WHERE ci.cart_id = c.id)
		ORDER BY c.updated_at
		LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: scan live carts: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lifecycle: scan live cart: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// abandonCart stamps one cart and defers its event.
//
// The stamp is the claim: `WHERE abandoned_at IS NULL` means two workers racing on the same
// cart produce one row effect, so one announcement. The second worker's update affects no
// rows and reports "nothing to do", which is not an error.
func (r *Runner) abandonCart(ctx context.Context, cartID string, cutoff time.Time) (AbandonedCart, bool, error) {
	out := AbandonedCart{CartID: cartID}
	var claimed bool
	err := r.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			customerID string
			items      int
		)
		err := tx.QueryRow(ctx, `
			UPDATE carts c
			SET abandoned_at = $2
			WHERE c.id = $1 AND c.abandoned_at IS NULL
			RETURNING c.customer_id::text,
			          (SELECT count(*) FROM cart_items ci WHERE ci.cart_id = c.id)`,
			cartID, r.now()).Scan(&customerID, &items)
		if err == pgx.ErrNoRows {
			// Claimed by another worker, or touched since the scan. Either way, nothing to say.
			return nil
		}
		if err != nil {
			return fmt.Errorf("lifecycle: mark cart abandoned: %w", err)
		}
		out.CustomerID = customerID
		out.ItemCount = items
		claimed = true

		events.Defer(ctx, events.Event{
			Kind:       extproto.EventCartAbandoned,
			CustomerID: customerID,
			Data: map[string]any{
				"cart_id":     cartID,
				"customer_id": customerID,
				"item_count":  items,
				"idle_since":  cutoff.UTC(),
			},
		})
		return nil
	})
	if err != nil {
		return AbandonedCart{}, false, err
	}
	return out, claimed, nil
}

// logCartAbandon writes what a pass did, and nothing when it did nothing.
func logCartAbandon(logger *slog.Logger, run CartAbandonRun, err error) {
	if err != nil {
		logger.Error("cart abandonment failed", "error", err)
	}
	if len(run.Failures) > 0 {
		logger.Warn("cart abandonment had failures", "count", len(run.Failures),
			"first", run.Failures[0])
	}
	if len(run.Abandoned) > 0 {
		logger.Info("announced abandoned carts", "count", len(run.Abandoned),
			"scanned", run.Scanned)
	}
}
