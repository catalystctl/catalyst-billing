package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

// Order cancellation (CT-11, OPS-02).
//
// An order that was placed and never paid for is not a sale, it is a reservation somebody
// walked away from. Leaving it alone is not neutral: it holds stock a real customer could
// buy, its pending subscription occupies one of the customer's per-product slots, and its
// invoice sits in dunning forever, reminding a customer about a purchase they abandoned.
//
// So after `cronjob.order_cancel_days` the order is cancelled. What "cancelled" means here is
// precise:
//
//   - the order row becomes `cancelled` (guarded on `pending_payment`, so a payment that
//     lands between the scan and the transaction wins);
//   - every open invoice on it is **voided**, through the same `open -> void` transition the
//     staff handler uses. Nothing is deleted and no column is rewritten around the trigger in
//     migration 0008: the number is burned, and the record of the document survives;
//   - the receivable the invoice posted is reversed, because the customer does not owe money
//     for a purchase the panel itself abandoned (see voidInvoice);
//   - the stock the order took is given back through the existing helper, so a product's
//     availability means what it says;
//   - a subscription that is still `pending` is cancelled, which is what releases the
//     customer's per-user-limit slot.
//
// Three cases are never cancelled, and each one is a guard rather than an assumption:
//
//   - an order that is no longer `pending_payment` (it was paid, or fulfilled);
//   - an order with nothing to pay (`total_minor = 0`): the checkout path marks those paid
//     itself, so a zero-total pending order is a stuck row for an operator to look at, not a
//     debt to abandon;
//   - an order whose invoice has taken money, or has a payment attempt still live at the
//     gateway. Voiding an invoice a customer is looking at in a checkout window is how a
//     panel takes money and delivers nothing.

// StockRestore records one product's units going back.
type StockRestore struct {
	ProductID string
	Units     int
}

// CancelledOrder is one order the run cancelled.
type CancelledOrder struct {
	OrderID    string
	CustomerID string
	// InvoiceIDs are the invoices this run voided.
	InvoiceIDs []string
	// Stock is the stock this run returned, per product.
	Stock []StockRestore
	// SubscriptionIDs are the pending subscriptions this run cancelled.
	SubscriptionIDs []string
}

// OrderCancelRun reports what one run did.
//
// It is returned rather than only logged so a test can assert the *absence* of work as
// precisely as the presence of it: "the order younger than the threshold is still
// pending_payment" is only checkable if the run says which orders it touched.
type OrderCancelRun struct {
	// Days is the threshold the run used, and Cutoff the instant derived from it.
	Days   int
	Cutoff time.Time
	// Scanned is how many candidate orders the scan returned.
	Scanned int
	// Cancelled are the orders this run cancelled.
	Cancelled []CancelledOrder
	// Failures are per-order errors. One bad row does not stop the batch: the next tick
	// retries it, and returning early would silently skip every order behind it.
	Failures []string
	// Reason is set when the run deliberately did nothing (see the Reason* constants).
	Reason string
}

// CancelUnpaidOrders cancels orders older than `cronjob.order_cancel_days` that were never
// paid for.
//
// It is safe to run twice, and safe to run from two workers at once: the scan selects only
// `pending_payment` rows, each cancellation re-reads its order under `FOR UPDATE`, and the
// final update is guarded on the status it read. The second run therefore finds nothing to
// do, and a concurrent one loses the lock and sees the committed `cancelled` status.
func (r *Runner) CancelUnpaidOrders(ctx context.Context) (OrderCancelRun, error) {
	run := OrderCancelRun{}

	if disabled(OrderCancelKillSwitch) {
		run.Reason = ReasonKillSwitch
		return run, nil
	}

	days, reason, err := r.threshold(ctx, orderCancelDaysSetting)
	if err != nil {
		return run, err
	}
	if reason != "" {
		run.Reason = reason
		return run, nil
	}
	run.Days = days
	run.Cutoff = cutoff(r.now(), days)

	ids, err := r.scanUnpaidOrders(ctx, run.Cutoff, r.batch())
	if err != nil {
		return run, err
	}
	run.Scanned = len(ids)

	now := r.now()
	for _, id := range ids {
		cancelled, skipped, err := r.cancelOrder(ctx, id, run.Cutoff, days, now)
		if err != nil {
			run.Failures = append(run.Failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if skipped != "" {
			// A row the scan selected but the transaction refused is normal: a payment may
			// have landed in between. It is recorded, not retried.
			continue
		}
		run.Cancelled = append(run.Cancelled, cancelled)
	}
	return run, nil
}

// scanUnpaidOrders returns the ids of orders that look cancellable.
//
// The predicates are repeated in cancelOrder, deliberately. The scan exists to bound the
// batch; the transaction is where the decision is made, because the scan is a snapshot and
// the transaction is the present.
func (r *Runner) scanUnpaidOrders(ctx context.Context, before time.Time, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT o.id
		FROM orders o
		WHERE o.status = 'pending_payment'
		  AND o.total_minor > 0
		  AND o.created_at < $1
		  -- An order that has taken money is not abandoned, whatever its status column says.
		  AND NOT EXISTS (
		      SELECT 1 FROM invoices i
		      WHERE i.order_id = o.id
		        AND (i.status = 'paid' OR i.amount_paid_minor > 0))
		  -- Nor is one whose gateway session is still open: the customer may be on the
		  -- payment page right now, and voiding the invoice under them would take money for
		  -- nothing.
		  AND NOT EXISTS (
		      SELECT 1 FROM payment_attempts pa
		      JOIN invoices i2 ON i2.id = pa.invoice_id
		      WHERE i2.order_id = o.id
		        AND pa.status IN ('created', 'pending', 'requires_action'))
		ORDER BY o.created_at
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: scan unpaid orders: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lifecycle: scan unpaid order: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lifecycle: scan unpaid orders: %w", err)
	}
	return ids, nil
}

// cancelOrder cancels one order, or explains why it did not.
//
// The second return value is a human-readable reason the order was left alone; it is empty
// when the order was cancelled. A non-empty reason is not an error: the world moved on
// between the scan and this transaction, and that is the normal race a payment wins.
func (r *Runner) cancelOrder(ctx context.Context, orderID string, before time.Time, days int,
	now time.Time) (CancelledOrder, string, error) {

	out := CancelledOrder{OrderID: orderID}
	skip := ""

	err := r.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Everything below is decided against a locked row. The lock is what makes the
		// payment applier and this job mutually exclusive: whoever gets here second sees the
		// other's committed answer.
		var (
			status     string
			totalMinor int64
			createdAt  time.Time
			customerID string
		)
		err := tx.QueryRow(ctx, `
			SELECT status, total_minor, created_at, customer_id
			FROM orders WHERE id = $1 FOR UPDATE`, orderID).
			Scan(&status, &totalMinor, &createdAt, &customerID)
		if errors.Is(err, pgx.ErrNoRows) {
			skip = "the order no longer exists"
			return nil
		}
		if err != nil {
			return fmt.Errorf("lifecycle: lock order: %w", err)
		}
		out.CustomerID = customerID

		switch {
		case status != "pending_payment":
			skip = fmt.Sprintf("its status is %s", status)
			return nil
		case totalMinor <= 0:
			// A zero-total order should have been completed by the checkout path. One that is
			// still pending is a stuck row, not a debt, and cancelling it would hide the bug.
			skip = "it has nothing to pay"
			return nil
		case !createdAt.Before(before):
			skip = "it is younger than the threshold"
			return nil
		}

		// Read the invoices under the same lock. A paid invoice, or one that has taken a
		// partial payment, ends the cancellation before anything is written: the money moved
		// and the correction for that is a credit note, not a void.
		type invoiceRow struct {
			id     string
			status string
			paid   int64
		}
		var invoices []invoiceRow
		invRows, err := tx.Query(ctx, `
			SELECT id, status, amount_paid_minor FROM invoices
			WHERE order_id = $1
			ORDER BY created_at
			FOR UPDATE`, orderID)
		if err != nil {
			return fmt.Errorf("lifecycle: lock invoices: %w", err)
		}
		for invRows.Next() {
			var iv invoiceRow
			if err := invRows.Scan(&iv.id, &iv.status, &iv.paid); err != nil {
				invRows.Close()
				return fmt.Errorf("lifecycle: scan invoice: %w", err)
			}
			invoices = append(invoices, iv)
		}
		if err := invRows.Err(); err != nil {
			invRows.Close()
			return fmt.Errorf("lifecycle: lock invoices: %w", err)
		}
		invRows.Close()

		for _, iv := range invoices {
			if iv.status == "paid" || iv.paid > 0 {
				skip = "an invoice on it has taken money"
				return nil
			}
		}

		reason := fmt.Sprintf("This invoice was cancelled because the order was not paid within %d days.", days)
		for _, iv := range invoices {
			if iv.status != "open" {
				continue
			}
			voided, err := voidInvoice(ctx, tx, iv.id, reason)
			if err != nil {
				return err
			}
			if voided {
				out.InvoiceIDs = append(out.InvoiceIDs, iv.id)
			}
		}

		// Stock goes back through the helper internal/service exports, not through an UPDATE
		// here: that helper is the one place that knows a NULL stock means "unlimited" and
		// must stay NULL. Quantities are aggregated per product first, because a cart may
		// contain two lines of the same product and a restore per line would be two updates
		// where one belongs.
		stockRows, err := tx.Query(ctx, `
			SELECT pv.product_id, SUM(oi.quantity)::int
			FROM order_items oi
			JOIN price_versions pv ON pv.id = oi.price_version_id
			WHERE oi.order_id = $1
			GROUP BY pv.product_id
			ORDER BY pv.product_id`, orderID)
		if err != nil {
			return fmt.Errorf("lifecycle: read order stock: %w", err)
		}
		type productUnits struct {
			productID string
			units     int
		}
		var restores []productUnits
		for stockRows.Next() {
			var pu productUnits
			if err := stockRows.Scan(&pu.productID, &pu.units); err != nil {
				stockRows.Close()
				return fmt.Errorf("lifecycle: scan order stock: %w", err)
			}
			restores = append(restores, pu)
		}
		if err := stockRows.Err(); err != nil {
			stockRows.Close()
			return fmt.Errorf("lifecycle: read order stock: %w", err)
		}
		stockRows.Close()

		for _, pu := range restores {
			if pu.units <= 0 {
				continue
			}
			// An unlimited product has nothing to give back, and a product that no longer
			// exists has nothing to give it back to. Both are checked here rather than left to
			// the helper so the audit row records only stock that actually moved: an audit that
			// says "restored 1" for an unlimited product is a lie a reconciliation would chase.
			var finite bool
			err := tx.QueryRow(ctx, `
				SELECT stock IS NOT NULL FROM products WHERE id = $1`, pu.productID).Scan(&finite)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("lifecycle: read stock flag: %w", err)
			}
			if !finite {
				continue
			}
			if err := service.RestoreStock(ctx, tx, pu.productID, pu.units); err != nil {
				if errors.Is(err, service.ErrNotFound) {
					continue
				}
				return fmt.Errorf("lifecycle: restore stock: %w", err)
			}
			out.Stock = append(out.Stock, StockRestore{ProductID: pu.productID, Units: pu.units})
		}

		// A subscription created by an unpaid order is still `pending`. Left there it would
		// occupy one of the customer's per-product slots forever, which is how a customer who
		// abandoned two carts is told they may not buy a third.
		subRows, err := tx.Query(ctx, `
			UPDATE subscriptions
			SET status = 'cancelled', cancelled_at = COALESCE(cancelled_at, $2),
			    version = version + 1
			WHERE status = 'pending'
			  AND id IN (
			      SELECT subscription_id FROM invoices
			      WHERE order_id = $1 AND subscription_id IS NOT NULL)
			RETURNING id`, orderID, now)
		if err != nil {
			return fmt.Errorf("lifecycle: cancel pending subscriptions: %w", err)
		}
		for subRows.Next() {
			var id string
			if err := subRows.Scan(&id); err != nil {
				subRows.Close()
				return fmt.Errorf("lifecycle: scan cancelled subscription: %w", err)
			}
			out.SubscriptionIDs = append(out.SubscriptionIDs, id)
		}
		if err := subRows.Err(); err != nil {
			subRows.Close()
			return fmt.Errorf("lifecycle: cancel pending subscriptions: %w", err)
		}
		subRows.Close()

		// The status is in the WHERE clause as well as in the switch above. The switch is the
		// explanation; this is the guarantee, and it is what makes a second worker's attempt
		// affect no rows instead of cancelling a paid order.
		tag, err := tx.Exec(ctx, `
			UPDATE orders
			SET status = 'cancelled', updated_at = $2, version = version + 1
			WHERE id = $1 AND status = 'pending_payment'`, orderID, now)
		if err != nil {
			return fmt.Errorf("lifecycle: cancel order: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("lifecycle: order %s changed while it was locked", orderID)
		}

		// The audit row is written inside the same transaction as the change, which is the
		// only version of the trail that cannot lie.
		return audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{Kind: audit.ActorSystem},
			Action:     "order.cancel",
			TargetType: "order",
			TargetID:   orderID,
			Outcome:    audit.OutcomeSuccess,
			Changes: map[string]any{
				"customer_id":      customerID,
				"days":             days,
				"cutoff":           before.Format(time.RFC3339),
				"invoices_voided":  out.InvoiceIDs,
				"stock_restored":   out.Stock,
				"subscriptions":    out.SubscriptionIDs,
				"total_minor":      totalMinor,
				"reason":           "unpaid order cancelled by the order_cancel_days rule",
				"automated_action": true,
			},
		})
	})
	if err != nil {
		return CancelledOrder{}, "", err
	}
	if skip != "" {
		return CancelledOrder{}, skip, nil
	}
	return out, "", nil
}

// voidInvoice voids one open, unpaid invoice.
//
// It is the same transition the staff void handler performs — `UPDATE ... SET status =
// 'void' WHERE status = 'open'`, guarded so the 0008 trigger has the last word, with the
// reason recorded in `void_reason` so a customer who downloads the document reads why. It
// returns false (and writes nothing) when the invoice is not in a state a void may touch.
//
// # Why it also reverses the receivable
//
// Issuing an invoice posts a debit to `ar` and credits to `revenue` and `tax_payable`. A void
// is the economic inverse of that issue: nothing was sold and nothing is owed. If the
// subledger keeps the original posting, the trial balance overstates both receivables and
// revenue for as long as the installation runs, and `ledger.ReceivableForCustomer` — which
// dunning treats as the authority on what a customer owes — keeps answering with money
// nobody owes. A credit note cannot fix it either, because a credit note refuses a void
// invoice.
//
// The reversal is keyed on (adjustment, invoice, this invoice's id), so it is idempotent: a
// replayed cancellation posts nothing the second time. See the package report: the staff void
// handlers in internal/httpapi do not post this reversal yet, and moving this helper into a
// shared package is the fix.
func voidInvoice(ctx context.Context, tx pgx.Tx, invoiceID, reason string) (bool, error) {
	var (
		status     string
		customerID string
		currency   string
		number     *string
		subtotal   int64
		tax        int64
		total      int64
		paid       int64
	)
	err := tx.QueryRow(ctx, `
		SELECT status, customer_id, currency, number, subtotal_minor, tax_minor,
		       total_minor, amount_paid_minor
		FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).
		Scan(&status, &customerID, &currency, &number, &subtotal, &tax, &total, &paid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lifecycle: lock invoice: %w", err)
	}
	// Only an open, untouched invoice may be voided. A partly paid one is a debt that a
	// credit note corrects; a draft has no number to burn.
	if status != "open" || paid > 0 {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET status = 'void', void_reason = $2
		WHERE id = $1 AND status = 'open'`, invoiceID, reason); err != nil {
		return false, fmt.Errorf("lifecycle: void invoice: %w", err)
	}

	if total > 0 {
		cur, ok := money.LookupCurrency(currency)
		if !ok {
			cur, err = money.NewCurrency(currency, 2)
			if err != nil {
				return false, fmt.Errorf("lifecycle: currency %q: %w", currency, err)
			}
		}
		memo := "invoice voided"
		if number != nil && *number != "" {
			memo = "invoice " + *number + " voided"
		}
		posting := ledger.Posting{
			Kind:        ledger.KindAdjustment,
			Reference:   ledger.RefInvoice,
			ReferenceID: invoiceID,
			CustomerID:  customerID,
			Currency:    cur,
			Credits: []ledger.Entry{{
				Account: ledger.AccountReceivable,
				Amount:  money.New(total, cur),
				Memo:    memo,
			}},
		}
		if subtotal > 0 {
			posting.Debits = append(posting.Debits, ledger.Entry{
				Account: ledger.AccountRevenue,
				Amount:  money.New(subtotal, cur),
				Memo:    memo,
			})
		}
		if tax > 0 {
			posting.Debits = append(posting.Debits, ledger.Entry{
				Account: ledger.AccountTaxPayable,
				Amount:  money.New(tax, cur),
				Memo:    memo,
			})
		}
		if _, _, err := ledger.Post(ctx, tx, posting); err != nil {
			return false, err
		}
	}

	return true, audit.Record(ctx, tx, audit.Event{
		Actor:      audit.Actor{Kind: audit.ActorSystem},
		Action:     "invoice.void",
		TargetType: "invoice",
		TargetID:   invoiceID,
		Outcome:    audit.OutcomeSuccess,
		Changes: map[string]any{
			"reason":           reason,
			"automated_action": true,
			"from":             "open",
			"to":               "void",
		},
	})
}
