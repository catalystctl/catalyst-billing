package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The reviewer's finding: fulfilment never wrote services.subscription_id, so the
// dunning join found no service and suspension could never happen in production.
// This drives the real fulfilment applier and then dunning, with no hand-wiring.
func TestSuspensionWorksThroughRealFulfilment(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	var customerID, productID, priceID, orderID, invoiceID, subID string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO customers (email,name) VALUES ('r@example.test','R') RETURNING id`).Scan(&customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO products (slug,name) VALUES ('vps','VPS') RETURNING id`).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO price_versions (product_id,version,currency,amount_minor,interval,anchor_day,anchor_timezone) VALUES ($1,1,'USD',1200,'monthly',1,'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO subscriptions (customer_id,price_version_id,status,interval,anchor_day,timezone,current_period_start,current_period_end,next_billing_at) VALUES ($1,$2,'active','monthly',1,'UTC',now()-interval '40 days',now()-interval '10 days',now()-interval '10 days') RETURNING id`, customerID, priceID).Scan(&subID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO orders (customer_id,status,currency,total_minor) VALUES ($1,'paid','USD',1200) RETURNING id`, customerID).Scan(&orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_items (order_id,price_version_id,description,quantity,unit_minor,line_minor,position) VALUES ($1,$2,'VPS',1,1200,1200,1)`, orderID, priceID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none"})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID, OrderID: orderID, SubscriptionID: subID,
			PeriodKey: "p1", DueAt: time.Now().AddDate(0, 0, -20),
			Lines:    []billing.Line{{Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd)}},
			Customer: billing.CustomerDetails{ID: customerID},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID

		// Settle it through the real applier, which must create the service.
		applier := fulfilment.NewApplier(pool)
		_, err = applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider: "stripe", ProviderAccount: "acct", ProviderPaymentID: "pi_1",
			InvoiceID: invoiceID, AmountMinor: 1200, Currency: "USD",
		})
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var linked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services WHERE subscription_id IS NOT NULL`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("%d services linked to a subscription, want 1: fulfilment does not link them, so dunning can never suspend", linked)
	}

	// Now run dunning to exhaustion against a *new* unpaid invoice on the same
	// subscription.
	// The provisioner would normally activate it; do that directly so this test is
	// about the dunning link, not about provisioning.
	var svcID string
	if err := pool.QueryRow(ctx, `SELECT id FROM services LIMIT 1`).Scan(&svcID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE services SET status = 'active', external_id = 'srv_1' WHERE id = $1`, svcID); err != nil {
		t.Fatal(err)
	}

	// A renewal invoice on the same subscription, already overdue.
	issuer2 := billing.NewIssuer(billing.FixedTaxPricer{Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none"})
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer2.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID, SubscriptionID: subID, PeriodKey: "p2",
			DueAt:    time.Now().AddDate(0, 0, -30),
			Lines:    []billing.Line{{Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd)}},
			Customer: billing.CustomerDetails{ID: customerID},
		})
		if err != nil {
			return err
		}
		_, err = issuer2.Issue(ctx, tx, id, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("second invoice: %v", err)
	}

	now := time.Now().UTC()
	engine := dunning.NewEngine(pool, dunning.Policy{ReminderInterval: time.Hour, MaxSteps: 2, SuspendAfter: time.Hour})
	engine.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := engine.Advance(ctx, 100); err != nil {
			t.Fatalf("advance: %v", err)
		}
		now = now.Add(2 * time.Hour)
		engine.Now = func() time.Time { return now }
	}

	elig, err := engine.Evaluate(ctx, svcID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	t.Logf("eligible=%v reason=%s", elig.Eligible, elig.Reason)
	if !elig.Eligible {
		t.Fatalf("service not eligible for suspension through the real path: %s", elig.Reason)
	}
}
