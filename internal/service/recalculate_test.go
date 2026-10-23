package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

// "Recalculate price" (SVC-13): the catalog says one thing and the subscription says another,
// and an operator needs to see the difference and then be able to fix it.

// addConfigOptionWithPrice inserts a text option priced on one price version and selects it on
// the service at the amount the customer paid.
func (f *fixture) addConfigOptionWithPrice(t *testing.T, priceID string, amount int64, value string) string {
	t.Helper()
	ctx := context.Background()
	var optionID string
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO config_options (product_id, name, option_type, choices)
			VALUES ($1, 'RAM', 'text', '[]'::jsonb) RETURNING id`, f.productID).Scan(&optionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
			VALUES ($1, $2, $3, $4)`, optionID, priceID, value, amount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
			VALUES ($1, $2, $3, $4)`, f.serviceID, optionID, value, amount)
		return err
	})
	if err != nil {
		t.Fatalf("add option: %v", err)
	}
	return optionID
}

// TestRecalculatePriceAppliesTheCatalogPrice is the gap the parity row names: nothing recomputed
// a service's price from the catalogue.
//
// The assertions are the two halves that must agree afterwards: the subscription names the new
// price version, and the stored selection carries the new option amount. Either one alone is a
// service billed a number that matches neither the catalog nor the customer's choice.
func TestRecalculatePriceAppliesTheCatalogPrice(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	optionID := f.addConfigOptionWithPrice(t, f.priceID, 500, "8")

	// The catalog gains a new version: 12.00 instead of 10.00, a 7.00 option instead of 5.00,
	// and a bigger provisioner plan.
	var newPriceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 2, 'Small (2026)', 'USD', 1200, 'monthly', 1, 'UTC', 'plan-large', 'eu-west')
		RETURNING id`, f.productID).Scan(&newPriceID); err != nil {
		t.Fatalf("insert new price: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '8', 700)`, optionID, newPriceID); err != nil {
		t.Fatalf("insert new option price: %v", err)
	}

	// apply=false is the read: it reports the difference and changes nothing.
	preview, err := f.store.RecalculatePrice(ctx, f.serviceID, "", false)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !preview.Changed {
		t.Fatal("preview says nothing changed, but the catalog gained a version")
	}
	if preview.From.TotalMinor != 1500 || preview.To.TotalMinor != 1900 {
		t.Fatalf("preview totals = %d -> %d, want 1500 -> 1900",
			preview.From.TotalMinor, preview.To.TotalMinor)
	}
	if !preview.PlanChanged {
		t.Fatal("plan_changed = false, but the provisioner plan moved from plan-small to plan-large")
	}
	var stillOld string
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&stillOld); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if stillOld != f.priceID {
		t.Fatal("a preview moved the subscription")
	}

	applied, err := f.store.RecalculatePrice(ctx, f.serviceID, f.userID, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Changed || applied.RepricedOptions != 1 {
		t.Fatalf("applied = %+v, want a change with one repriced option", applied)
	}

	var subPrice string
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != newPriceID {
		t.Fatalf("subscription price = %s, want the catalog's newest %s", subPrice, newPriceID)
	}
	var optionAmount int64
	if err := f.pool.QueryRow(ctx, `
		SELECT amount_minor FROM service_configs
		WHERE service_id = $1 AND config_option_id = $2`, f.serviceID, optionID).Scan(&optionAmount); err != nil {
		t.Fatalf("read selection: %v", err)
	}
	if optionAmount != 700 {
		t.Fatalf("stored option amount = %d, want 700 (the new version's price)", optionAmount)
	}

	// The plan moved, so the provisioner has to be told — as a queue row, not a call.
	if applied.OperationID == "" {
		t.Fatal("no resize was queued for a change of plan")
	}
	var kind, payload string
	if err := f.pool.QueryRow(ctx, `
		SELECT kind, payload->>'plan_id' FROM service_operations WHERE id = $1`,
		applied.OperationID).Scan(&kind, &payload); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if kind != "resize" || payload != "plan-large" {
		t.Fatalf("queued operation = %s/%s, want resize to plan-large", kind, payload)
	}

	// A second recalculation finds nothing to do: the service is at the catalog price.
	again, err := f.store.RecalculatePrice(ctx, f.serviceID, "", true)
	if err != nil {
		t.Fatalf("recalculate again: %v", err)
	}
	if again.Changed {
		t.Fatalf("a second recalculation changed something: %+v", again)
	}
}

// A price-only correction must not tell the provisioner anything: nothing about the server
// changed, and a queued resize would restart a customer's service to say so.
func TestRecalculatePriceWithoutAPlanChangeQueuesNothing(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	// Same plan id, a corrected amount.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 2, 'Small', 'USD', 1100, 'monthly', 1, 'UTC', 'plan-small', 'eu-west')`,
		f.productID); err != nil {
		t.Fatalf("insert new price: %v", err)
	}

	res, err := f.store.RecalculatePrice(ctx, f.serviceID, "", true)
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	if !res.Changed || res.PlanChanged {
		t.Fatalf("result = %+v, want a price change without a plan change", res)
	}
	if res.OperationID != "" {
		t.Fatal("a price-only correction queued a provisioning operation")
	}
	if n := countRows(t, f.pool, "service_operations"); n != 0 {
		t.Fatalf("%d operations queued", n)
	}
}

// A service with no billing schedule has nothing to recalculate, and saying so is better than a
// zero answer that looks like a price.
func TestRecalculatePriceRefusesAServiceWithoutASubscription(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`UPDATE services SET subscription_id = NULL WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("detach subscription: %v", err)
	}
	_, err := f.store.RecalculatePrice(ctx, f.serviceID, "", true)
	if !errors.Is(err, service.ErrNotUpgradable) {
		t.Fatalf("err = %v, want ErrNotUpgradable", err)
	}
}
