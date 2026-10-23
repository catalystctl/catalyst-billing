// Package fulfilment turns a paid invoice into the work that delivers it.
//
// It is the join between money and infrastructure, and the join is deliberately
// transactional: the payment, its allocation, the subledger postings, the invoice
// transition and the provisioning operation all commit together. If the
// provisioning job were enqueued separately, a crash between the two writes would
// produce a paid invoice with no service and no record of what was owed -- exactly
// the "paid but unprovisioned" state the operator dashboard is supposed to surface
// but should never have to.
package fulfilment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Applier implements payments.Applier on top of the billing package.
type Applier struct {
	pool *store.Pool
	// notify is the outbox the "your invoice is paid" message is written to. It is optional so
	// a test or a deployment without mail configured still works; when it is nil the payment
	// path is unchanged, because telling the customer is not a precondition for taking money.
	notify *notify.Queue
}

// NewApplier constructs an Applier.
func NewApplier(pool *store.Pool) *Applier { return &Applier{pool: pool} }

// WithNotifier attaches the outbox used to tell a customer their invoice was paid.
//
// It is a builder rather than a constructor argument so every existing caller — including the
// e2e slice and the fulfilment tests — keeps compiling and keeps its current behaviour. The
// notification is written inside the settlement transaction, so a paid invoice and the message
// about it commit together or not at all.
func (a *Applier) WithNotifier(q *notify.Queue) *Applier {
	a.notify = q
	return a
}

// ApplySettledPayment records a provider-confirmed payment and enqueues
// provisioning for anything the paid invoice bought.
func (a *Applier) ApplySettledPayment(ctx context.Context, tx pgx.Tx, p payments.SettledPayment) (payments.ApplyResult, error) {
	cur, err := currencyOf(p.Currency)
	if err != nil {
		return payments.ApplyResult{}, err
	}

	res, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
		Provider:          p.Provider,
		ProviderAccount:   p.ProviderAccount,
		ProviderPaymentID: p.ProviderPaymentID,
		InvoiceID:         p.InvoiceID,
		Amount:            money.New(p.AmountMinor, cur),
	})
	if err != nil {
		return payments.ApplyResult{}, err
	}

	out := payments.ApplyResult{
		Duplicate:   res.Duplicate,
		InvoicePaid: res.InvoicePaid,
		PaymentID:   res.PaymentID,
	}

	// Provisioning is enqueued only when this payment actually settled the
	// invoice. A partial payment moves money but buys nothing yet.
	if res.InvoicePaid {
		// A proforma carries no number until it is paid, which is what makes it provisional.
		// Converting here — in the same transaction that settled the payment — is what keeps the
		// two facts inseparable: an invoice cannot be paid without becoming real, and a real
		// invoice cannot appear without the money that earned it. A conversion failure rolls the
		// payment back, which is the right direction to fail: a settled payment with an unnumbered
		// document is a reconciliation problem nobody can reconstruct later.
		//
		// ConvertProformaOnPayment is a no-op for an ordinary invoice, so this does not branch on
		// whether the invoice was provisional.
		numbering, err := invoicepdf.LoadNumbering(ctx, tx)
		if err != nil {
			return payments.ApplyResult{}, err
		}
		series, err := numbering.InvoiceSeries(time.Now())
		if err != nil {
			return payments.ApplyResult{}, err
		}
		if _, err := invoicepdf.ConvertProformaOnPayment(ctx, tx, p.InvoiceID, series); err != nil {
			return payments.ApplyResult{}, err
		}

		// Tell the customer, in this transaction. A payment that settled and a message that says
		// so are one fact, so they commit together: if the outbox write fails the money is not
		// taken either, which is the honest ordering for a customer-facing promise.
		//
		// This is deliberately best-effort with respect to *content*: a missing recipient or a
		// disabled template records a reason on the outcome rather than failing the payment. The
		// notify package's own rule is that a missing template is a hard error, and it is — inside
		// dispatch — but the distinction that matters here is that a customer who has not finished
		// registering has no address to send to, and that must not stop their payment landing.
		if err := a.notifyInvoicePaid(ctx, tx, p.InvoiceID); err != nil {
			return payments.ApplyResult{}, err
		}

		// Pay the affiliate who referred this customer, in the same transaction that took the
		// money. Commission is a liability the moment the payment settles, so recording it later
		// would leave a window in which the books say the shop earned the full amount and it did
		// not. An invoice with no referral is a no-op, which is the overwhelmingly common case.
		//
		// RecordSettledInvoiceCommission covers both kinds of commission, so a renewal is paid
		// for in this same transaction too: the first order's commission is earned on the
		// invoice's order, and a renewal's is attributed to the referral recorded on the
		// customer. With `affiliates.recurring` off — the default — a renewal still records
		// nothing, which is exactly the behaviour before the recurring work landed.
		if _, err := growth.RecordSettledInvoiceCommission(ctx, tx, p.InvoiceID); err != nil &&
			!errors.Is(err, growth.ErrNotReferred) {
			return payments.ApplyResult{}, fmt.Errorf("record affiliate commission: %w", err)
		}

		if err := a.activateSubscription(ctx, tx, p.InvoiceID); err != nil {
			return payments.ApplyResult{}, err
		}
		if err := a.enqueueProvisioning(ctx, tx, p.InvoiceID); err != nil {
			return payments.ApplyResult{}, err
		}

		// CT-13: order.paid, for the order this invoice belongs to. It is *deferred* rather
		// than published: this function runs inside a transaction the caller commits, so
		// publishing here would announce a payment that can still roll back. The buffer is
		// flushed by store.Pool.inTx after the commit, which is the one place that knows the
		// transaction succeeded. A renewal invoice with no order behind it emits nothing:
		// "this order was paid" is a statement about an order, and inventing one would give an
		// extension an id it cannot look up.
		if err := deferOrderPaid(ctx, tx, p.InvoiceID, res.Duplicate); err != nil {
			return payments.ApplyResult{}, err
		}
	}
	return out, nil
}

// deferOrderPaid records an order.paid event for the order a settled invoice belongs to.
//
// A duplicate payment is skipped: the first settlement already announced the order, and an
// extension that provisions on order.paid must not be told twice for one payment.
func deferOrderPaid(ctx context.Context, tx pgx.Tx, invoiceID string, duplicate bool) error {
	if duplicate {
		return nil
	}
	var (
		orderID    string
		customerID string
		totalMinor int64
		currency   string
	)
	err := tx.QueryRow(ctx, `
		SELECT o.id::text, o.customer_id::text, o.total_minor, o.currency
		FROM invoices i
		JOIN orders o ON o.id = i.order_id
		WHERE i.id = $1`, invoiceID).Scan(&orderID, &customerID, &totalMinor, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		// A renewal or a manually issued invoice has no order. Nothing to announce.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read order for invoice: %w", err)
	}
	events.Defer(ctx, events.Event{
		Kind:       extproto.EventOrderPaid,
		CustomerID: customerID,
		Data: map[string]any{
			"order_id":    orderID,
			"invoice_id":  invoiceID,
			"customer_id": customerID,
			"total_minor": totalMinor,
			"currency":    currency,
		},
	})
	return nil
}

// activateSubscription starts the renewal schedule of a subscription that a paid
// order created.
//
// Orders are written with a 'pending' subscription on purpose: an unpaid order must
// never renew. The renewal scanner only looks at 'active' and 'past_due', so without
// this transition a customer would pay once and never be billed again. It is
// conditional on 'pending' so a replayed payment cannot resurrect a cancelled
// subscription.
func (a *Applier) activateSubscription(ctx context.Context, tx pgx.Tx, invoiceID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions SET status = 'active', version = version + 1
		WHERE status = 'pending'
		  AND id = (SELECT subscription_id FROM invoices WHERE id = $1)`, invoiceID); err != nil {
		return fmt.Errorf("fulfilment: activate subscription: %w", err)
	}
	return nil
}

// enqueueProvisioning creates the service and its create-operation for every
// order item behind the invoice.
//
// It is idempotent on the order item: the operation key is derived from the item
// id, so a replayed payment finds the existing operation instead of creating a
// second service.
func (a *Applier) enqueueProvisioning(ctx context.Context, tx pgx.Tx, invoiceID string) error {
	// Two sources of "what does this invoice sell", in priority order.
	//
	// An invoice raised by a checkout has an order, and the order items are authoritative: they
	// carry the price version the customer actually bought. A manual invoice has no order at
	// all — an operator wrote it for a bank transfer — so the product it sells is recorded on
	// the invoice line instead. Without this second query a manual invoice would take the
	// customer's money and provision nothing, which is the defect this exists to fix.
	//
	// The order path is tried first and, when it returns rows, the line path is skipped
	// entirely. Falling through on "no rows" rather than merging the two is deliberate: an
	// invoice must never create the same service twice because both paths matched.
	items, err := a.provisioningItemsFromOrder(ctx, tx, invoiceID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if items, err = a.provisioningItemsFromLines(ctx, tx, invoiceID); err != nil {
			return err
		}
	}

	for _, it := range items {
		key := "create:" + it.ID

		// Check before inserting: creating the service first and cleaning up on
		// conflict would leave churn in the table and an id sequence that moves
		// on every replay.
		var existing string
		err := tx.QueryRow(ctx, `SELECT id FROM service_operations WHERE operation_key = $1`, key).
			Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("fulfilment: look up provisioning operation: %w", err)
		}

		// The adapter's plan identifier is configuration, captured on the service
		// when it is created so a later catalog edit cannot change what an
		// in-flight provisioning call asks for.
		config := map[string]any{}
		if it.PlanID != "" {
			config["plan_id"] = it.PlanID
		}
		if it.Region != "" {
			config["region"] = it.Region
		}

		var serviceID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation, config)
			VALUES ($1, $2, $3, 'pending', 1, $4)
			RETURNING id`, it.CustomerID, it.SubscriptionID, it.ProductID, config).Scan(&serviceID); err != nil {
			return fmt.Errorf("fulfilment: create service: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'create', $2, 1, 'queued')`, serviceID, key); err != nil {
			return fmt.Errorf("fulfilment: enqueue provisioning: %w", err)
		}
	}
	return nil
}

// provisioningItem is one thing to provision, from either source.
type provisioningItem struct {
	// ID is the order item id or the invoice line id. It is the idempotency anchor: the
	// operation key is "create:" + ID, so a replayed payment finds the existing operation.
	ID         string
	CustomerID string
	ProductID  string
	PlanID     string
	Region     string
	// SubscriptionID links the service to its billing schedule. Without it, dunning cannot
	// find which invoice funds the service, so automated suspension never happens.
	SubscriptionID *string
}

// provisioningItemsFromOrder reads what a checkout invoice sells.
func (a *Applier) provisioningItemsFromOrder(ctx context.Context, tx pgx.Tx, invoiceID string) ([]provisioningItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT oi.id, o.customer_id, p.id AS product_id,
		       COALESCE(pv.provision_plan, ''), COALESCE(pv.provision_region, ''),
		       i.subscription_id
		FROM invoices i
		JOIN orders o ON o.id = i.order_id
		JOIN order_items oi ON oi.order_id = o.id
		JOIN price_versions pv ON pv.id = oi.price_version_id
		JOIN products p ON p.id = pv.product_id
		WHERE i.id = $1
		ORDER BY oi.position`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("fulfilment: load order items: %w", err)
	}
	defer rows.Close()
	var items []provisioningItem
	for rows.Next() {
		var it provisioningItem
		if err := rows.Scan(&it.ID, &it.CustomerID, &it.ProductID,
			&it.PlanID, &it.Region, &it.SubscriptionID); err != nil {
			return nil, fmt.Errorf("fulfilment: scan order item: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fulfilment: read order items: %w", err)
	}
	return items, nil
}

// provisioningItemsFromLines reads what a manual invoice sells.
//
// The plan and region come from the line's price version when it has one, and otherwise from
// the product's current (highest) version. That fallback is what lets an operator sell a
// standard product at a negotiated price: the amount charged is whatever they typed, while the
// provisioning target is still a real plan rather than an empty string.
func (a *Applier) provisioningItemsFromLines(ctx context.Context, tx pgx.Tx, invoiceID string) ([]provisioningItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT il.id, i.customer_id, il.product_id,
		       COALESCE(pv.provision_plan, cur.provision_plan, ''),
		       COALESCE(pv.provision_region, cur.provision_region, ''),
		       i.subscription_id
		  FROM invoice_lines il
		  JOIN invoices i ON i.id = il.invoice_id
		  JOIN products p ON p.id = il.product_id
		  LEFT JOIN price_versions pv ON pv.id = il.price_version_id
		  LEFT JOIN LATERAL (
		        SELECT provision_plan, provision_region
		          FROM price_versions
		         WHERE product_id = il.product_id
		         ORDER BY version DESC
		         LIMIT 1
		  ) cur ON true
		 WHERE il.invoice_id = $1 AND il.product_id IS NOT NULL
		 ORDER BY il.position`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("fulfilment: load invoice line products: %w", err)
	}
	defer rows.Close()
	var items []provisioningItem
	for rows.Next() {
		var it provisioningItem
		if err := rows.Scan(&it.ID, &it.CustomerID, &it.ProductID,
			&it.PlanID, &it.Region, &it.SubscriptionID); err != nil {
			return nil, fmt.Errorf("fulfilment: scan invoice line product: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fulfilment: read invoice line products: %w", err)
	}
	return items, nil
}

func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// notifyInvoicePaid writes the "your invoice is paid" message for a settled invoice.
//
// It resolves the recipient from the billing account rather than assuming the customer has a
// login: a staff-created customer may have an email and no user, and that customer still deserves
// to be told. When there is no address at all the call is skipped — notify.Target treats an
// empty address as "do not send" rather than guessing one.
//
// The invoice number is read after the proforma conversion above, so a provisional document that
// just became real is announced with the number it actually has.
func (a *Applier) notifyInvoicePaid(ctx context.Context, tx pgx.Tx, invoiceID string) error {
	if a.notify == nil {
		return nil
	}

	var (
		number   string
		total    int64
		currency string
		email    string
		name     string
		userID   *string
	)
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(i.number, ''), i.total_minor, i.currency,
		       c.email, c.name,
		       (SELECT ca.user_id::text FROM customer_accounts ca WHERE ca.customer_id = c.id LIMIT 1)
		  FROM invoices i
		  JOIN customers c ON c.id = i.customer_id
		 WHERE i.id = $1`, invoiceID).
		Scan(&number, &total, &currency, &email, &name, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("notify invoice paid: read invoice: %w", err)
	}

	cur, err := currencyOf(currency)
	if err != nil {
		return fmt.Errorf("notify invoice paid: %w", err)
	}

	in := notify.InvoicePaidInput{
		Email:      email,
		InvoiceID:  invoiceID,
		Number:     number,
		Total:      money.New(total, cur).Format(),
		PaidAt:     time.Now().UTC(),
		InvoiceURL: "/portal/invoices/" + invoiceID,
	}
	if userID != nil {
		in.UserID = *userID
	}
	_ = name // the template addresses the customer generically; a name is not required.

	_, err = notify.InvoicePaid(ctx, tx, a.notify, in)
	if err != nil {
		return fmt.Errorf("notify invoice paid: %w", err)
	}
	return nil
}
