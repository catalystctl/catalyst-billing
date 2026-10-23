package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/service"
)

// A plan change and its config options as one priced change (UPG-02).
//
// The numbers are chosen so the answer is exact and easy to check by hand: a half-period change
// from (10.00 plan + 3.00 option) to (15.00 plan + 5.00 option) is (20.00 - 13.00) / 2 = 3.50.
// Anything that priced only the base, or priced the option against the other plan's price version,
// would land on a different number.

// addOption attaches a single-choice option to a product with prices on one price version.
func (f *fixture) addOption(t *testing.T, productID, priceVersionID, name, choice string,
	amountMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var optionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, sort_order)
		VALUES ($1, $2, 'dropdown', jsonb_build_array($3::text), 1)
		RETURNING id`, productID, name, choice).Scan(&optionID); err != nil {
		t.Fatalf("config option: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, $3, $4)`, optionID, priceVersionID, choice, amountMinor); err != nil {
		t.Fatalf("config option price: %v", err)
	}
	return optionID
}

// addPriceVersion creates another version of a product with a base amount.
func (f *fixture) addPriceVersion(t *testing.T, productID string, version int, amountMinor int64) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, $2::int, 'Version ' || $2::int::text, 'USD', $3, 'monthly', 1, 'UTC',
		        'plan-small', 'eu-west')
		RETURNING id`, productID, version, amountMinor).Scan(&id); err != nil {
		t.Fatalf("price version: %v", err)
	}
	return id
}

// TestChangePlanPricesTheOptionsWithThePlan is the UPG-02 property: one request, one prorated
// amount, covering the plan *and* the options the customer chose on it.
func TestChangePlanPricesTheOptionsWithThePlan(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	// The service the customer already has: the 10.00 plan with the 3.00 option.
	ram := f.addOption(t, f.productID, f.priceID, "RAM", "16 GB", 300)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
		VALUES ($1, $2, '16 GB', 300)`, f.serviceID, ram); err != nil {
		t.Fatalf("stored selection: %v", err)
	}

	// The version they are moving to: 15.00 with the same option at 5.00.
	nextVersion := f.addPriceVersion(t, f.productID, 2, 1500)
	nextRAM := ram // the option row is the product's; only its price is per version
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '16 GB', 500)`, nextRAM, nextVersion); err != nil {
		t.Fatalf("next version price: %v", err)
	}

	res, err := f.store.ChangePlan(ctx, service.PlanChangeRequest{
		ServiceID:      f.serviceID,
		PriceVersionID: nextVersion,
		Selection:      map[string]string{ram: "16 GB"},
		UserID:         f.userID,
		Reason:         "growing",
	})
	if err != nil {
		t.Fatalf("change plan: %v", err)
	}

	// (1500 + 500) - (1000 + 300) = 700 for the whole period; half of it remains = 350.
	if res.ProratedMinor != 350 {
		t.Fatalf("prorated = %d, want 350 (the plan and the option, prorated once)", res.ProratedMinor)
	}
	if res.Direction != "upgrade" {
		t.Fatalf("direction = %q, want upgrade", res.Direction)
	}
	if res.InvoiceID == "" {
		t.Fatal("a positive change issued no invoice")
	}

	var total int64
	if err := f.pool.QueryRow(ctx,
		`SELECT total_minor FROM invoices WHERE id = $1`, res.InvoiceID).Scan(&total); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if total != 350 {
		t.Fatalf("invoice total = %d, want 350", total)
	}

	// The chosen options wait on the upgrade row, not on the service: the service is still
	// running the old plan until the invoice is paid.
	var pending int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_upgrade_configs WHERE upgrade_id = $1`, res.UpgradeID).
		Scan(&pending); err != nil {
		t.Fatalf("count chosen options: %v", err)
	}
	if pending != 1 {
		t.Fatalf("chosen options stored = %d, want 1", pending)
	}
	var stored int64
	if err := f.pool.QueryRow(ctx, `
		SELECT amount_minor FROM service_configs WHERE service_id = $1 AND config_option_id = $2`,
		f.serviceID, ram).Scan(&stored); err != nil {
		t.Fatalf("read stored selection: %v", err)
	}
	if stored != 300 {
		t.Fatalf("the live selection moved to %d before the invoice was paid, want 300", stored)
	}

	// Pay it: the change is applied, and the selection moves with it, priced on the new version.
	if _, err := f.pool.Exec(ctx,
		`UPDATE invoices SET status = 'paid', amount_paid_minor = total_minor WHERE id = $1`,
		res.InvoiceID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if err := f.store.ApplyUpgrade(ctx, res.UpgradeID); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT amount_minor FROM service_configs WHERE service_id = $1 AND config_option_id = $2`,
		f.serviceID, ram).Scan(&stored); err != nil {
		t.Fatalf("read stored selection: %v", err)
	}
	if stored != 500 {
		t.Fatalf("the applied selection is priced at %d, want 500 (the target plan's price)", stored)
	}
	var left int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_upgrade_configs WHERE upgrade_id = $1`, res.UpgradeID).
		Scan(&left); err != nil {
		t.Fatalf("count chosen options: %v", err)
	}
	if left != 0 {
		t.Fatalf("the chosen options were not consumed: %d left", left)
	}
}

// A change that moves the plan to a *different product* replaces the old product's options
// rather than leaving them attached to a service that no longer has them.
func TestChangePlanReplacesTheOldProductsOptions(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	oldRAM := f.addOption(t, f.productID, f.priceID, "RAM", "16 GB", 0)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
		VALUES ($1, $2, '16 GB', 0)`, f.serviceID, oldRAM); err != nil {
		t.Fatalf("stored selection: %v", err)
	}

	productID, priceID := f.addUpgradeTarget(t, 2000, "monthly")
	newRAM := f.addOption(t, productID, priceID, "RAM", "32 GB", 400)

	res, err := f.store.ChangePlan(ctx, service.PlanChangeRequest{
		ServiceID:      f.serviceID,
		PriceVersionID: priceID,
		Selection:      map[string]string{newRAM: "32 GB"},
		UserID:         f.userID,
	})
	if err != nil {
		t.Fatalf("change plan: %v", err)
	}
	// (2000 + 400) - (1000 + 0) = 1400, half the period left = 700.
	if res.ProratedMinor != 700 {
		t.Fatalf("prorated = %d, want 700", res.ProratedMinor)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE invoices SET status = 'paid', amount_paid_minor = total_minor WHERE id = $1`,
		res.InvoiceID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if err := f.store.ApplyUpgrade(ctx, res.UpgradeID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Exactly one selection remains, and it is the new product's. The old product's option would
	// be a row nothing on the target plan can price.
	var (
		count  int
		option string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(min(config_option_id::text), '')
		FROM service_configs WHERE service_id = $1`, f.serviceID).Scan(&count, &option); err != nil {
		t.Fatalf("read selections: %v", err)
	}
	if count != 1 || option != newRAM {
		t.Fatalf("selections = %d (one is %s), want exactly the new option %s", count, option, newRAM)
	}
}

// A plan change onto a plan with no options takes the old options away, and the prorated
// difference reflects it. An empty selection is a statement, not an omission.
func TestChangePlanWithAnEmptySelectionRemovesTheOptions(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	ram := f.addOption(t, f.productID, f.priceID, "RAM", "16 GB", 400)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
		VALUES ($1, $2, '16 GB', 400)`, f.serviceID, ram); err != nil {
		t.Fatalf("stored selection: %v", err)
	}

	// The same base price, no options: (1000 + 0) - (1000 + 400) = -400, half left = -200.
	res, err := f.store.ChangePlan(ctx, service.PlanChangeRequest{
		ServiceID:      f.serviceID,
		PriceVersionID: f.priceID,
		Selection:      map[string]string{},
		UserID:         f.userID,
	})
	if err != nil {
		t.Fatalf("change plan: %v", err)
	}
	if res.ProratedMinor != -200 {
		t.Fatalf("prorated = %d, want -200 (dropping a paid option is a credit)", res.ProratedMinor)
	}
	if !res.CreditPending || res.CreditMinor != 200 {
		t.Fatalf("credit = %d (pending %v), want 200 pending", res.CreditMinor, res.CreditPending)
	}
}

// An upgrade invoice is settled from the customer's credit at issuance (CRD-03).
//
// `credits.auto_use` is a promise about *invoices*, not about the checkout page: a customer with
// credit on account should not be charged by the gateway for a change their balance already
// covers. This test grants credit through the real grant path and then changes the plan, so what
// it proves is the posting rule at the moment the invoice is issued.
func TestChangePlanIsSettledFromCreditWhenTheBalanceCoversIt(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	targetProduct, _ := f.addUpgradeTarget(t, 2000, "monthly")
	var targetPrice string
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM price_versions WHERE product_id = $1`, targetProduct).Scan(&targetPrice); err != nil {
		t.Fatalf("target price: %v", err)
	}

	if _, err := credit.New(f.pool).Grant(ctx, credit.GrantRequest{
		CustomerID:  f.customerID,
		AmountMinor: 5000,
		Currency:    "USD",
		Reason:      "goodwill",
		Key:         "plan-change-credit-" + f.customerID,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	res, err := f.store.ChangePlan(ctx, service.PlanChangeRequest{
		ServiceID:      f.serviceID,
		PriceVersionID: targetPrice,
		UserID:         f.userID,
	})
	if err != nil {
		t.Fatalf("change plan: %v", err)
	}
	if res.InvoiceID == "" {
		t.Fatal("no invoice was issued")
	}

	var total, paid int64
	var status string
	if err := f.pool.QueryRow(ctx, `
		SELECT total_minor, amount_paid_minor, status FROM invoices WHERE id = $1`,
		res.InvoiceID).Scan(&total, &paid, &status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != "paid" || paid != total || total != 500 {
		t.Fatalf("invoice = %d/%d %s, want the 500 prorated difference paid in full from credit",
			paid, total, status)
	}
	if total == 0 {
		t.Fatal("the fixture produced a zero invoice, so the assertion proves nothing")
	}

	// And the balance keeps the rest: the difference between what was granted and what was spent.
	amounts, err := credit.BalanceFor(ctx, f.pool, f.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	var remaining int64
	for _, a := range amounts {
		if a.Currency().Code() == "USD" {
			remaining = a.Minor()
		}
	}
	if remaining != 5000-total {
		t.Fatalf("remaining credit = %d, want %d", remaining, 5000-total)
	}
}
