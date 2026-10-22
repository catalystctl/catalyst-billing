package fulfilment_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// A manual invoice that sells a product must provision when it is paid.
//
// This is the defect this file exists for. Fulfilment read what to provision from the invoice's
// *order* (`invoices.order_id` → `order_items` → `price_versions`). A checkout produces that
// chain; an operator writing an invoice for a customer who paid by bank transfer does not. So
// the customer paid, the invoice went to `paid`, and no service was ever created — the only
// trace being the `paid_not_provisioned` exception on the operator overview.
//
// The fix records the product on the invoice line. These tests drive the real applier, so they
// fail if either path stops working.

func usd(t *testing.T) money.Currency {
	t.Helper()
	cur, ok := money.LookupCurrency("USD")
	if !ok {
		t.Fatal("USD is not a known currency")
	}
	return cur
}

// seedManualInvoice creates a customer, a product with a price version, and an issued invoice
// whose line sells that product. No order is created, which is the whole point.
func seedManualInvoice(t *testing.T, pool *store.Pool, withVersion bool) (invoiceID, productID, priceID, customerID string) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email,name) VALUES ('manual@example.test','Manual') RETURNING id`).
			Scan(&customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug,name) VALUES ('manual-vps','Manual VPS') RETURNING id`).
			Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id,version,name,currency,amount_minor,interval,
				anchor_day,anchor_timezone,provision_plan,provision_region)
			VALUES ($1,1,'Monthly','USD',2500,'monthly',1,'UTC','vps.manual','eu-central')
			RETURNING id`, productID).Scan(&priceID); err != nil {
			return err
		}

		line := billing.Line{
			Description:   "Manual VPS",
			QuantityMilli: 1000,
			UnitPrice:     money.New(2500, usd(t)),
			ProductID:     &productID,
		}
		if withVersion {
			line.PriceVersionID = &priceID
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			DueAt:      time.Now().AddDate(0, 0, 7),
			Lines:      []billing.Line{line},
			Customer:   billing.CustomerDetails{ID: customerID},
			Merchant:   billing.Merchant{Name: "Test"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("seed manual invoice: %v", err)
	}
	return invoiceID, productID, priceID, customerID
}

// settle pays the invoice through the real applier, which is what a gateway webhook does.
func settle(t *testing.T, pool *store.Pool, invoiceID string, amount int64) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		applier := fulfilment.NewApplier(pool)
		_, err := applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider:          "manual",
			ProviderAccount:   "acct",
			ProviderPaymentID: "manual-" + invoiceID,
			InvoiceID:         invoiceID,
			AmountMinor:       amount,
			Currency:          "USD",
		})
		return err
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func TestManualInvoiceWithProductProvisionsOnPayment(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID, productID, priceID, _ := seedManualInvoice(t, pool, true)

	// Before payment: nothing to provision.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("%d services exist before payment", before)
	}

	settle(t, pool, invoiceID, 2500)

	var serviceID, status, plan string
	if err := pool.QueryRow(ctx, `
		SELECT id, status, COALESCE(config->>'plan_id','')
		  FROM services WHERE product_id = $1`, productID).Scan(&serviceID, &status, &plan); err != nil {
		t.Fatalf("no service was created for a paid manual invoice: %v", err)
	}
	// The service starts pending: the worker provisions it. Creating it active would claim
	// something that has not happened yet.
	if status != "pending" {
		t.Fatalf("service status = %q, want pending", status)
	}
	// The plan comes from the price version the line named.
	if plan != "vps.manual" {
		t.Fatalf("provision plan = %q, want vps.manual", plan)
	}

	// And a provisioning operation was queued, which is what the worker picks up.
	var ops int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations
		 WHERE service_id = $1 AND kind = 'create' AND status = 'queued'`, serviceID).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 1 {
		t.Fatalf("%d queued create operations, want 1", ops)
	}

	// The price version is recorded on the line, so the provision target is reproducible.
	var recorded string
	if err := pool.QueryRow(ctx,
		`SELECT price_version_id FROM invoice_lines WHERE invoice_id = $1 AND product_id = $2`,
		invoiceID, productID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != priceID {
		t.Fatalf("line price version = %q, want %q", recorded, priceID)
	}
}

// A negotiated price on a standard product: no version on the line, so the plan is read from
// the product's current version while the amount charged stays what the operator typed.
func TestManualInvoiceWithoutVersionUsesCurrentPlan(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID, productID, _, _ := seedManualInvoice(t, pool, false)

	settle(t, pool, invoiceID, 2500)

	var plan string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(config->>'plan_id','') FROM services WHERE product_id = $1`, productID).
		Scan(&plan); err != nil {
		t.Fatalf("no service created: %v", err)
	}
	if plan != "vps.manual" {
		t.Fatalf("plan = %q, want the product's current version's plan (vps.manual)", plan)
	}
}

// A line with no product is an ordinary invoice line and must provision nothing. Support time
// and prorated differences are legitimate lines, and inventing a service for them would be a
// worse bug than the one being fixed.
func TestManualInvoiceWithoutProductProvisionsNothing(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	var invoiceID string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var customerID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email,name) VALUES ('support@example.test','Support') RETURNING id`).
			Scan(&customerID); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			DueAt:      time.Now().AddDate(0, 0, 7),
			Lines: []billing.Line{{
				Description: "One hour of support", QuantityMilli: 1000,
				UnitPrice: money.New(9000, usd(t)),
			}},
			Customer: billing.CustomerDetails{ID: customerID},
			Merchant: billing.Merchant{Name: "Test"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	settle(t, pool, invoiceID, 9000)

	var services int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if services != 0 {
		t.Fatalf("%d services created for a support-time invoice, want 0", services)
	}
}

// A replayed payment must not create a second service. The operation key is derived from the
// line id, so the second apply finds the first operation and stops — the same rule the order
// path follows.
func TestManualInvoiceProvisioningIsIdempotent(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID, productID, _, _ := seedManualInvoice(t, pool, true)

	settle(t, pool, invoiceID, 2500)
	settle(t, pool, invoiceID, 2500)

	var services, ops int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services WHERE product_id = $1`, productID).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_operations WHERE kind = 'create'`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if services != 1 {
		t.Fatalf("%d services after a replayed payment, want 1", services)
	}
	if ops != 1 {
		t.Fatalf("%d create operations after a replayed payment, want 1", ops)
	}
}

// An invoice raised by a checkout must keep using its order items, even if a line also names a
// product. Otherwise a checkout invoice could provision twice — once from the order and once
// from the line.
func TestOrderPathWinsOverLinePath(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	var invoiceID, productID string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var customerID, priceID, orderID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email,name) VALUES ('order@example.test','Order') RETURNING id`).
			Scan(&customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug,name) VALUES ('order-vps','Order VPS') RETURNING id`).
			Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id,version,currency,amount_minor,interval,
				anchor_day,anchor_timezone,provision_plan)
			VALUES ($1,1,'USD',3300,'monthly',1,'UTC','vps.order') RETURNING id`, productID).
			Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id,status,currency,total_minor)
			VALUES ($1,'paid','USD',3300) RETURNING id`, customerID).Scan(&orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id,price_version_id,description,quantity,unit_minor,line_minor,position)
			VALUES ($1,$2,'Order VPS',1,3300,3300,1)`, orderID, priceID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID, OrderID: orderID,
			DueAt: time.Now().AddDate(0, 0, 7),
			Lines: []billing.Line{{
				Description: "Order VPS", QuantityMilli: 1000,
				UnitPrice: money.New(3300, usd(t)),
				// The line also names the product, which must not double-provision.
				ProductID: &productID,
			}},
			Customer: billing.CustomerDetails{ID: customerID},
			Merchant: billing.Merchant{Name: "Test"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	settle(t, pool, invoiceID, 3300)

	var services int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if services != 1 {
		t.Fatalf("%d services, want exactly 1: the order path must win, not both", services)
	}
}
