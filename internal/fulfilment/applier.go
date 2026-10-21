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

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Applier implements payments.Applier on top of the billing package.
type Applier struct {
	pool *store.Pool
}

// NewApplier constructs an Applier.
func NewApplier(pool *store.Pool) *Applier { return &Applier{pool: pool} }

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
		if err := a.enqueueProvisioning(ctx, tx, p.InvoiceID); err != nil {
			return payments.ApplyResult{}, err
		}
	}
	return out, nil
}

// enqueueProvisioning creates the service and its create-operation for every
// order item behind the invoice.
//
// It is idempotent on the order item: the operation key is derived from the item
// id, so a replayed payment finds the existing operation instead of creating a
// second service.
func (a *Applier) enqueueProvisioning(ctx context.Context, tx pgx.Tx, invoiceID string) error {
	rows, err := tx.Query(ctx, `
		SELECT oi.id, o.customer_id, oi.price_version_id, p.id AS product_id,
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
		return fmt.Errorf("fulfilment: load order items: %w", err)
	}

	type item struct {
		ID         string
		CustomerID string
		PriceID    string
		ProductID  string
		PlanID     string
		Region     string
		// SubscriptionID links the service to its billing schedule. Without it,
		// dunning cannot find which invoice funds the service, so automated
		// suspension never happens.
		SubscriptionID *string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.CustomerID, &it.PriceID, &it.ProductID,
			&it.PlanID, &it.Region, &it.SubscriptionID); err != nil {
			rows.Close()
			return fmt.Errorf("fulfilment: scan order item: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("fulfilment: read order items: %w", err)
	}
	rows.Close()

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

func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
