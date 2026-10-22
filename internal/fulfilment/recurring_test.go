package fulfilment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// A renewal earns a recurring commission, in the transaction that settled it (GRW-03).
//
// This drives the real applier — the same call a gateway webhook makes — because the claim
// being tested is about *where* the commission is written, not just that the growth package
// can compute one. A renewal has no order, so before this the settlement path recorded
// nothing; the attribution it needs is on the customer, and this proves the applier reaches
// the recurring path with it.
//
// The gate is asserted first: with `affiliates.recurring` off, the same renewal settles and
// records nothing, which is the behaviour a deployment that never enables the feature must
// keep.

// setRecurring writes one affiliates.* setting and restores the previous state.
//
// settings is reference data the harness never truncates, so a test that leaves a row behind
// changes what every later test in this package means.
func setRecurring(t *testing.T, pool *store.Pool, key, value string) {
	t.Helper()
	ctx := context.Background()
	var previous string
	err := pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'affiliates' AND key = $1`, key).Scan(&previous)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := pool.Exec(ctx, `
			INSERT INTO settings (group_key, key, value) VALUES ('affiliates', $1, $2)`,
			key, value); err != nil {
			t.Fatalf("set affiliates.%s: %v", key, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM settings WHERE group_key = 'affiliates' AND key = $1`, key)
		})
	case err != nil:
		t.Fatalf("read affiliates.%s: %v", key, err)
	default:
		if _, err := pool.Exec(ctx, `
			UPDATE settings SET value = $2, updated_at = now()
			WHERE group_key = 'affiliates' AND key = $1`, key, value); err != nil {
			t.Fatalf("set affiliates.%s: %v", key, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `
				UPDATE settings SET value = $2, updated_at = now()
				WHERE group_key = 'affiliates' AND key = $1`, key, previous)
		})
	}
}

// seedReferredRenewal builds a referred customer with a subscription and an issued renewal
// invoice, and returns the invoice id.
func seedReferredRenewal(t *testing.T, pool *store.Pool, totalMinor int64) (invoiceID, customerID string) {
	t.Helper()
	ctx := context.Background()

	var affiliateUser string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('renewal-affiliate@example.test', 'x', now()) RETURNING id`).Scan(&affiliateUser); err != nil {
		t.Fatalf("affiliate user: %v", err)
	}
	store := growth.New(pool)
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Create(ctx, tx, growth.NewAffiliate{
			UserID: affiliateUser, Code: "RENEW10", CommissionRate: "10%", PayoutMethod: "credit",
		})
		return err
	}); err != nil {
		t.Fatalf("create affiliate: %v", err)
	}

	var productID, priceID, subscriptionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('renewal-plan', 'Renewal plan') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Monthly', 'USD', $2, 'monthly', 1, 'UTC') RETURNING id`,
		productID, totalMinor).Scan(&priceID); err != nil {
		t.Fatalf("price version: %v", err)
	}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The signup: customer, user, account and attribution in one transaction, which is
		// the contract growth.AttributeSignup documents.
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('renewal@example.test', 'Renewal')
			RETURNING id`).Scan(&customerID); err != nil {
			return err
		}
		var userID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ('renewal@example.test', 'x', now()) RETURNING id`).Scan(&userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			userID, customerID); err != nil {
			return err
		}
		if _, err := growth.AttributeSignup(ctx, tx, "RENEW10", customerID); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
				timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now(), now() + interval '1 month',
			        now() + interval '1 month') RETURNING id`,
			customerID, priceID).Scan(&subscriptionID); err != nil {
			return err
		}

		// The renewal: issued from the subscription, with no order. That shape is the reason
		// the first-order commission path has nothing to attribute.
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		draft, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     customerID,
			SubscriptionID: subscriptionID,
			PeriodKey:      "2026-11",
			DueAt:          time.Now().AddDate(0, 0, 7),
			Lines: []billing.Line{{
				Description:   "Renewal plan",
				QuantityMilli: 1000,
				UnitPrice:     money.New(totalMinor, usd(t)),
			}},
			Customer: billing.CustomerDetails{ID: customerID},
			Merchant: billing.Merchant{Name: "Test"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, draft, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("seed renewal: %v", err)
	}
	return invoiceID, customerID
}

// commissionTotal reads what the recurring table holds for one customer.
func commissionTotal(t *testing.T, pool *store.Pool, customerID string) (rows int, minor int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*), coalesce(sum(commission_minor), 0)
		FROM affiliate_recurring_commissions WHERE customer_id = $1`, customerID).
		Scan(&rows, &minor); err != nil {
		t.Fatalf("count recurring commissions: %v", err)
	}
	return rows, minor
}

// With the gate off, settling a renewal records nothing — the behaviour before this work.
func TestSettlingARenewalRecordsNothingWhenRecurringIsOff(t *testing.T) {
	pool := storetest.DB(t)
	invoiceID, customerID := seedReferredRenewal(t, pool, 9000)

	settle(t, pool, invoiceID, 9000)

	rows, minor := commissionTotal(t, pool, customerID)
	if rows != 0 || minor != 0 {
		t.Fatalf("%d renewal commissions worth %d with the feature off, want none", rows, minor)
	}
}

// With the gate on, the settlement transaction records the commission: 10% of 9000, once,
// attributed to the referral on the customer.
func TestSettlingARenewalRecordsARecurringCommission(t *testing.T) {
	pool := storetest.DB(t)
	setRecurring(t, pool, "recurring", "true")
	invoiceID, customerID := seedReferredRenewal(t, pool, 9000)

	settle(t, pool, invoiceID, 9000)

	rows, minor := commissionTotal(t, pool, customerID)
	if rows != 1 {
		t.Fatalf("%d renewal commissions after settling one renewal, want 1", rows)
	}
	if minor != 900 {
		t.Fatalf("renewal commission = %d, want 900 (10%% of 9000)", minor)
	}

	// The row names the invoice it was earned on, so the payout report can explain it.
	var linkedInvoice, status string
	if err := pool.QueryRow(context.Background(), `
		SELECT invoice_id::text, status FROM affiliate_recurring_commissions
		WHERE customer_id = $1`, customerID).Scan(&linkedInvoice, &status); err != nil {
		t.Fatalf("load commission: %v", err)
	}
	if linkedInvoice != invoiceID {
		t.Fatalf("commission points at invoice %q, want %q", linkedInvoice, invoiceID)
	}
	if status != "pending" {
		t.Fatalf("commission status = %q, want pending", status)
	}

	// And the payable moved with it.
	var balance int64
	if err := pool.QueryRow(context.Background(), `
		SELECT balance_minor FROM affiliate_balances WHERE currency = 'USD'`).Scan(&balance); err != nil {
		t.Fatalf("load balance: %v", err)
	}
	if balance != 900 {
		t.Fatalf("affiliate balance = %d, want 900", balance)
	}
}
