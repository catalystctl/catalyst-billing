package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The price-change queue (UPG-05, UPG-06, CRD-06).
//
// These are database tests because each rule is a state transition that must be visible to an
// operator afterwards: a row cancelled, a note issued, a subscription repriced. A test that only
// asserted "the job returned no error" would pass with the job doing nothing at all — which is
// exactly the state this file was written to end.

// upgradeQueue is one customer with an active service and subscription, ready for an upgrade row.
type upgradeQueue struct {
	pool         *store.Pool
	customerID   string
	serviceID    string
	subscription string
	priceID      string
}

func newUpgradeQueue(t *testing.T) *upgradeQueue {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	q := &upgradeQueue{pool: pool}
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('queue@example.test', 'Queue Customer')
		RETURNING id`).Scan(&q.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	var productID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('queue-product', 'VPS Small') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'Small', 'USD', 1000, 'monthly', 1, 'UTC', 'plan-small', 'eu-west')
		RETURNING id`, productID).Scan(&q.priceID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			anchor_hour, anchor_minute, timezone, current_period_start, current_period_end,
			next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 0, 0, 'UTC', $3, $4, $4)
		RETURNING id`, q.customerID, q.priceID,
		now.AddDate(0, 0, -15), now.AddDate(0, 0, 15)).Scan(&q.subscription); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, subscription_id, product_id, status)
		VALUES ($1, $2, $3, 'active') RETURNING id`,
		q.customerID, q.subscription, productID).Scan(&q.serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return q
}

// addUpgrade records a pending `service_upgrades` row with an explicit age.
func (q *upgradeQueue) addUpgrade(t *testing.T, proratedMinor int64, age time.Duration) string {
	t.Helper()
	var id string
	if err := q.pool.QueryRow(context.Background(), `
		INSERT INTO service_upgrades
			(service_id, from_price_version_id, to_price_version_id, prorated_minor, currency,
			 status, created_at)
		VALUES ($1, $2, $2, $3, 'USD', 'pending', $4)
		RETURNING id`, q.serviceID, q.priceID, proratedMinor,
		time.Now().UTC().Add(-age)).Scan(&id); err != nil {
		t.Fatalf("upgrade row: %v", err)
	}
	return id
}

// runServiceUpgrades finds the queue's job and runs one tick.
func runServiceUpgrades(t *testing.T, pool *store.Pool) {
	t.Helper()
	var run func(context.Context) error
	for _, job := range lifecycle.ServiceJobs(pool, nil) {
		if job.Name == lifecycle.ServiceUpgradesJob {
			run = job.Run
		}
	}
	if run == nil {
		t.Fatal("the service-upgrades job is not registered; nothing would ever finish a price change")
	}
	if err := run(context.Background()); err != nil {
		t.Fatalf("run service upgrades: %v", err)
	}
}

// An upgrade invoice nobody paid, older than the stale window, is voided and the row cancelled.
// Without the job the row stays pending for ever and the service keeps a document that disagrees
// with it (UPG-06).
func TestServiceUpgradesJobCancelsAnUnpaidOldUpgrade(t *testing.T) {
	q := newUpgradeQueue(t)
	id := q.addUpgrade(t, 500, 20*24*time.Hour)

	runServiceUpgrades(t, q.pool)

	var status string
	if err := q.pool.QueryRow(context.Background(),
		`SELECT status FROM service_upgrades WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("status = %q, want cancelled; the stale row was left pending", status)
	}
}

// A pending upgrade inside the stale window is left alone: a customer mid-checkout must not have
// their change cancelled under them.
func TestServiceUpgradesJobLeavesAFreshUpgradeAlone(t *testing.T) {
	q := newUpgradeQueue(t)
	id := q.addUpgrade(t, 500, time.Hour)

	runServiceUpgrades(t, q.pool)

	var status string
	if err := q.pool.QueryRow(context.Background(),
		`SELECT status FROM service_upgrades WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
}

// A downgrade is credited and then applied: the balance moves, the note exists, the subscription
// takes the cheaper price. Before the job existed the row said "owed" and nothing paid it
// (CRD-06), and the service renewed at the old price.
func TestServiceUpgradesJobCreditsAndAppliesADowngrade(t *testing.T) {
	q := newUpgradeQueue(t)
	// A cheaper target price version, so applying the row is a real repricing.
	var cheapID string
	if err := q.pool.QueryRow(context.Background(), `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		SELECT product_id, 2, 'Small (cheaper)', 'USD', 600, 'monthly', 1, 'UTC',
		       'plan-small', 'eu-west'
		FROM price_versions WHERE id = $1
		RETURNING id`, q.priceID).Scan(&cheapID); err != nil {
		t.Fatalf("cheap price version: %v", err)
	}
	id := q.addUpgrade(t, -400, time.Hour)
	if _, err := q.pool.Exec(context.Background(),
		`UPDATE service_upgrades SET to_price_version_id = $2 WHERE id = $1`, id, cheapID); err != nil {
		t.Fatalf("retarget upgrade: %v", err)
	}

	runServiceUpgrades(t, q.pool)

	var status string
	var noteID *string
	if err := q.pool.QueryRow(context.Background(), `
		SELECT status, credit_note_id::text FROM service_upgrades WHERE id = $1`, id).
		Scan(&status, &noteID); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if noteID == nil {
		t.Fatal("the downgrade was not credited; the customer is still owed the prorated difference")
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}

	var balance int64
	if err := q.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = 'customer_credit'`, q.customerID).
		Scan(&balance); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 400 {
		t.Fatalf("credit balance = %d, want 400", balance)
	}

	var priceVersion string
	if err := q.pool.QueryRow(context.Background(),
		`SELECT price_version_id FROM subscriptions WHERE id = $1`, q.subscription).
		Scan(&priceVersion); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if priceVersion != cheapID {
		t.Fatalf("subscription still bills %s, want the cheaper %s", priceVersion, cheapID)
	}
}
