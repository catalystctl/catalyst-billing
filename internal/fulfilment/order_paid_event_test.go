package fulfilment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// order.paid, for extensions (CT-13).
//
// The event is emitted from the settlement applier, which runs inside a transaction its caller
// commits. So the property to prove is not "an event is published" — it is that the event is
// announced only when the transaction commits, and never when it rolls back. A subscriber that
// provisions on order.paid would otherwise act on an order that does not exist.

// orderForPayment writes a customer, a product, an order and the issued invoice for it, and
// returns the ids.
func orderForPayment(t *testing.T, pool *store.Pool) (invoiceID, orderID, customerID string) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('order-paid@example.test', 'Paid')
			RETURNING id`).Scan(&customerID); err != nil {
			return err
		}
		var productID, priceID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (slug, name) VALUES ('order-paid-vps', 'Paid VPS') RETURNING id`).
			Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
				anchor_day, anchor_timezone)
			VALUES ($1, 1, 'Monthly', 'USD', 2500, 'monthly', 1, 'UTC') RETURNING id`, productID).
			Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor)
			VALUES ($1, 'pending_payment', 'USD', 2500) RETURNING id`, customerID).Scan(&orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, price_version_id, description, quantity, unit_minor,
				line_minor, setup_minor, position)
			VALUES ($1, $2, 'Paid VPS', 1, 2500, 2500, 0, 1)`, orderID, priceID); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		draftID, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			OrderID:    orderID,
			PeriodKey:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			DueAt:      time.Now().AddDate(0, 0, 7),
			Lines: []billing.Line{{
				Description: "Paid VPS", QuantityMilli: 1000, UnitPrice: money.New(2500, usd(t)),
				ProductID: &productID, PriceVersionID: &priceID,
			}},
			Customer: billing.CustomerDetails{ID: customerID},
			Merchant: billing.Merchant{Name: "Test"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, draftID, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return invoiceID, orderID, customerID
}

// settleOrder runs the applier inside one transaction, optionally failing afterwards so the
// transaction rolls back.
func settleOrder(pool *store.Pool, invoiceID string, fail bool) error {
	return pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		applier := fulfilment.NewApplier(pool)
		if _, err := applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider: "manual", ProviderAccount: "manual", ProviderPaymentID: "paid-" + invoiceID,
			InvoiceID: invoiceID, AmountMinor: 2500, Currency: "USD",
		}); err != nil {
			return err
		}
		if fail {
			// The transaction rolls back *after* the settlement: exactly the case the deferred
			// event exists for.
			return errors.New("a later step failed")
		}
		return nil
	})
}

// A committed settlement announces order.paid, with the order and customer an extension needs to
// act on it.
func TestSettledPaymentAnnouncesOrderPaid(t *testing.T) {
	pool := storetest.DB(t)
	broker := events.NewBroker()
	sub := broker.Subscribe(8)
	defer sub.Close()

	invoiceID, orderID, customerID := orderForPayment(t, pool)
	if err := settleOrder(pool, invoiceID, false); err != nil {
		t.Fatalf("settle: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Kind != extproto.EventOrderPaid {
			t.Fatalf("kind = %q, want %q", ev.Kind, extproto.EventOrderPaid)
		}
		if ev.Data["order_id"] != orderID {
			t.Fatalf("event order = %v, want %s", ev.Data["order_id"], orderID)
		}
		if ev.CustomerID != customerID {
			t.Fatalf("event customer = %q, want %s", ev.CustomerID, customerID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a settled order announced no order.paid event")
	}
}

// A settlement that rolls back announces nothing: the event belongs to the transaction, not to
// the call that recorded it.
func TestRolledBackSettlementAnnouncesNothing(t *testing.T) {
	pool := storetest.DB(t)
	broker := events.NewBroker()
	sub := broker.Subscribe(8)
	defer sub.Close()

	invoiceID, _, _ := orderForPayment(t, pool)
	if err := settleOrder(pool, invoiceID, true); err == nil {
		t.Fatal("the settlement was expected to fail")
	}

	select {
	case ev := <-sub.Events():
		t.Fatalf("a rolled-back settlement announced %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}
