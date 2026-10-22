package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- proration --

// A half-month upgrade of a 10.00 plan to a 20.00 plan is exactly 5.00. The numbers are chosen
// so the answer is exact: half of a 10.00 difference is 5.00, and any implementation that
// rounds twice, or uses a float, or assumes 30 days for every month, will miss it.
func TestProrationIsExactForAHalfMonthUpgrade(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mid := start.Add(end.Sub(start) / 2)

	p, err := service.ComputeProration(start, end, mid)
	if err != nil {
		t.Fatalf("compute proration: %v", err)
	}
	if got := p.Ratio.String(); got != "1/2" {
		t.Fatalf("ratio = %s, want 1/2", got)
	}

	from := money.New(1000, usd) // 10.00
	to := money.New(2000, usd)   // 20.00
	got, err := service.ProratedDifference(from, to, p, money.RoundHalfUp)
	if err != nil {
		t.Fatalf("prorated difference: %v", err)
	}
	if got.Minor() != 500 {
		t.Fatalf("prorated = %d minor, want 500 (5.00)", got.Minor())
	}
}

// A month-end case does not drift: a period that ends on the 31st is measured by its real
// length, so the same elapsed time gives a different ratio in a 31-day month than in a 28-day
// one. The assertion is the exact rational, not an approximation.
func TestProrationMonthEndDoesNotDrift(t *testing.T) {
	// January: 31 days. Half-way is 15.5 days, which is an exact rational of 1/2 only at the
	// midpoint; a quarter of the way is exactly 1/4.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	quarter := start.Add(end.Sub(start) / 4)

	p, err := service.ComputeProration(start, end, quarter)
	if err != nil {
		t.Fatalf("compute proration: %v", err)
	}
	if got := p.Ratio.String(); got != "3/4" {
		t.Fatalf("ratio = %s, want 3/4 (three quarters of the period remain)", got)
	}
	if p.Total != 31*24*time.Hour {
		t.Fatalf("total = %v, want 31 days", p.Total)
	}

	// February: 28 days in 2026. The same "one day in" instant is a different fraction, which
	// is the whole point: a fixed 30-day divisor would return the same number for both.
	febStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	febEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	febP, err := service.ComputeProration(febStart, febEnd, febStart.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("compute february proration: %v", err)
	}
	janP, err := service.ComputeProration(start, end, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("compute january proration: %v", err)
	}
	if janP.Ratio.String() == febP.Ratio.String() {
		t.Fatalf("January and February give the same ratio %s; the divisor is not the real period length",
			janP.Ratio.String())
	}
	if got, want := febP.Ratio.String(), "27/28"; got != want {
		t.Fatalf("February ratio = %s, want %s", got, want)
	}
	if got, want := janP.Ratio.String(), "30/31"; got != want {
		t.Fatalf("January ratio = %s, want %s", got, want)
	}

	// A 10.00 difference over 27/28 of February is 9.64 (9.642857… rounded half up).
	amount, err := service.ProratedDifference(money.New(0, usd), money.New(1000, usd),
		febP, money.RoundHalfUp)
	if err != nil {
		t.Fatalf("prorated difference: %v", err)
	}
	if amount.Minor() != 964 {
		t.Fatalf("prorated = %d, want 964", amount.Minor())
	}
}

// A change requested before the period starts is worth the whole period; one requested after it
// ends is worth nothing. Neither is an error, because both are legitimate races.
func TestProrationClampsToThePeriod(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	before, err := service.ComputeProration(start, end, start.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if got := before.Ratio.String(); got != "1" {
		t.Fatalf("ratio before the period = %s, want 1", got)
	}

	after, err := service.ComputeProration(start, end, end.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if !after.Ratio.IsZero() {
		t.Fatalf("ratio after the period = %s, want 0", after.Ratio.String())
	}
}

// A zero-length period is an error rather than a division by zero.
func TestProrationRefusesAnEmptyPeriod(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := service.ComputeProration(at, at, at); err == nil {
		t.Fatal("an empty period was accepted")
	}
}

// ----------------------------------------------------------------- upgrades --

// addUpgradeTarget creates a second product reachable from f's product, with one price version.
func (f *fixture) addUpgradeTarget(t *testing.T, amountMinor int64, interval string) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (slug, name) VALUES ($1, 'VPS Large') RETURNING id`,
			"vps-large-"+f.customerID[:8]).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
				anchor_day, anchor_timezone, provision_plan, provision_region)
			VALUES ($1, 1, 'Large', 'USD', $2, $3, 1, 'UTC', 'plan-large', 'eu-west')
			RETURNING id`, productID, amountMinor, interval).Scan(&priceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)`,
			f.productID, productID)
		return err
	})
	if err != nil {
		t.Fatalf("upgrade target: %v", err)
	}
	return productID, priceID
}

// An upgrade issues a prorated invoice for the difference over the remainder of the period, and
// the subscription keeps its old price until that invoice is paid.
func TestUpgradeIssuesProratedInvoiceAndDefersTheReprice(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)

	_, targetPrice := f.addUpgradeTarget(t, 2000, "monthly")

	res, err := f.store.Upgrade(context.Background(), service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID, Reason: "need more",
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	if res.Direction != "upgrade" {
		t.Fatalf("direction = %q, want upgrade", res.Direction)
	}
	if res.InvoiceID == "" {
		t.Fatal("no invoice was issued for an upgrade")
	}
	// 15.5 of 31 days remain, so 10.00 * 15.5/31 = 5.00 exactly.
	if res.ProratedMinor != 500 {
		t.Fatalf("prorated = %d minor, want 500", res.ProratedMinor)
	}

	var (
		status    string
		total     int64
		subPrice  string
		rowStatus string
	)
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, total_minor FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&status, &total); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != "open" {
		t.Fatalf("invoice status = %q, want open", status)
	}
	if total != 500 {
		t.Fatalf("invoice total = %d, want 500", total)
	}

	if err := f.pool.QueryRow(context.Background(),
		`SELECT price_version_id, status FROM subscriptions WHERE id = $1`, f.subID).
		Scan(&subPrice, &status); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != f.priceID {
		t.Fatal("the subscription was repriced before the invoice was paid")
	}

	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM service_upgrades WHERE id = $1`, res.UpgradeID).
		Scan(&rowStatus); err != nil {
		t.Fatalf("read upgrade row: %v", err)
	}
	if rowStatus != service.UpgradePending {
		t.Fatalf("upgrade row status = %q, want pending", rowStatus)
	}

	// No resize operation yet: the provisioner must not be told before the money moved.
	if n := countRows(t, f.pool, "service_operations"); n != 0 {
		t.Fatalf("%d operations exist before the invoice was paid", n)
	}
}

// Applying an unpaid upgrade is refused; applying a paid one moves the subscription and
// enqueues exactly one resize.
func TestApplyPendingUpgradesRepricesAfterPayment(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	_, targetPrice := f.addUpgradeTarget(t, 2000, "monthly")
	res, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	// Unpaid: the worker must not act.
	applied, err := f.store.ApplyPendingUpgrades(ctx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("apply acted on %d unpaid upgrades", len(applied))
	}

	// Pay the invoice the way the payment path would.
	if _, err := f.pool.Exec(ctx, `
		UPDATE invoices SET status = 'paid', amount_paid_minor = total_minor WHERE id = $1`,
		res.InvoiceID); err != nil {
		t.Fatalf("pay invoice: %v", err)
	}

	applied, err = f.store.ApplyPendingUpgrades(ctx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("apply acted on %d upgrades, want 1", len(applied))
	}

	var (
		subPrice  string
		rowStatus string
		appliedAt *time.Time
	)
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != targetPrice {
		t.Fatalf("subscription price = %s, want the target %s", subPrice, targetPrice)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT status, applied_at FROM service_upgrades WHERE id = $1`, res.UpgradeID).
		Scan(&rowStatus, &appliedAt); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if rowStatus != service.UpgradeApplied {
		t.Fatalf("upgrade status = %q, want applied", rowStatus)
	}
	if appliedAt == nil {
		t.Fatal("applied_at was not stamped")
	}

	var kind string
	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT kind, count(*) OVER () FROM service_operations`).Scan(&kind, &n); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d operations, want exactly 1", n)
	}
	if kind != "resize" {
		t.Fatalf("operation kind = %q, want resize", kind)
	}

	// A second run does nothing.
	again, err := f.store.ApplyPendingUpgrades(ctx)
	if err != nil {
		t.Fatalf("apply again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a second apply acted on %d upgrades", len(again))
	}
	if got := countRows(t, f.pool, "service_operations"); got != 1 {
		t.Fatalf("%d operations after a second apply, want 1", got)
	}
}

// A downgrade records the credit owed and does not issue an invoice.
func TestDowngradeRecordsCreditAndIssuesNoInvoice(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 2000, "monthly", now, start, end)

	_, targetPrice := f.addUpgradeTarget(t, 1000, "monthly")

	res, err := f.store.Upgrade(context.Background(), service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if res.Direction != "downgrade" {
		t.Fatalf("direction = %q, want downgrade", res.Direction)
	}
	if res.InvoiceID != "" {
		t.Fatal("a downgrade issued an invoice")
	}
	if !res.CreditPending {
		t.Fatal("the credit was not reported as pending")
	}
	if res.CreditMinor != 500 {
		t.Fatalf("credit = %d minor, want 500", res.CreditMinor)
	}
	if n := countRows(t, f.pool, "invoices"); n != 0 {
		t.Fatalf("%d invoices exist after a downgrade", n)
	}

	// The obligation is recorded, which is the part that must not be lost.
	var prorated int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT prorated_minor FROM service_upgrades WHERE id = $1`, res.UpgradeID).
		Scan(&prorated); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if prorated != -500 {
		t.Fatalf("prorated_minor = %d, want -500 (negative is a credit)", prorated)
	}
}

// A target outside the upgrade graph is refused: the graph is the promise the storefront made
// about which products are comparable.
func TestUpgradeOutsideTheGraphIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	// A product with a price, but no product_upgrades edge.
	var productID, priceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('unrelated', 'Unrelated') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}

	_, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: priceID, UserID: f.userID,
	})
	if err == nil {
		t.Fatal("an upgrade to an unreachable product succeeded")
	}
	if !errors.Is(err, service.ErrNotUpgradable) {
		t.Fatalf("error = %v, want ErrNotUpgradable", err)
	}
}

// A price in another currency is refused: there is no exchange rate in this system, so the
// difference is not a number that means anything.
func TestUpgradeAcrossCurrenciesIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	var productID, priceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('eur-plan', 'Euro Plan') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'EUR', 2000, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)`,
		f.productID, productID); err != nil {
		t.Fatalf("graph: %v", err)
	}

	_, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: priceID, UserID: f.userID,
	})
	if err == nil {
		t.Fatal("an upgrade across currencies succeeded")
	}
	if !errors.Is(err, service.ErrCurrencyMismatch) {
		t.Fatalf("error = %v, want ErrCurrencyMismatch", err)
	}
}

// The upgrade options list only prices in the service's currency and interval, with their
// prorated consequence.
func TestUpgradeOptionsListsReachablePrices(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	_, targetPrice := f.addUpgradeTarget(t, 2000, "monthly")

	// A price on the same product in another currency, and one on another interval. Neither is
	// a valid target, so neither may be offered.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ((SELECT product_id FROM price_versions WHERE id = $1), 2, 'EUR', 9000, 'monthly', 1, 'UTC')`,
		targetPrice); err != nil {
		t.Fatalf("eur price: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ((SELECT product_id FROM price_versions WHERE id = $1), 3, 'USD', 90000, 'annual', 1, 'UTC')`,
		targetPrice); err != nil {
		t.Fatalf("annual price: %v", err)
	}

	options, err := f.store.UpgradeOptions(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("%d options, want 1", len(options))
	}
	if len(options[0].Prices) != 1 {
		t.Fatalf("%d prices, want only the one in the service's currency and interval", len(options[0].Prices))
	}
	price := options[0].Prices[0]
	if price.PriceVersionID != targetPrice {
		t.Fatalf("price = %s, want %s", price.PriceVersionID, targetPrice)
	}
	if price.Prorated.Minor() != 500 {
		t.Fatalf("prorated = %d, want 500", price.Prorated.Minor())
	}
}

// A stale unpaid upgrade is cancelled by the cron, and its invoice is voided with it.
func TestCancelStaleUpgradesVoidsTheInvoice(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	_, targetPrice := f.addUpgradeTarget(t, 2000, "monthly")
	res, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	// Not stale yet.
	cancelled, err := f.store.CancelStaleUpgrades(ctx)
	if err != nil {
		t.Fatalf("cancel stale: %v", err)
	}
	if len(cancelled) != 0 {
		t.Fatalf("cancel stale acted on %d fresh upgrades", len(cancelled))
	}

	// Age the row past the threshold, measured against the store's clock rather than the
	// database's: the fixture pins the clock, so "30 days ago" has to mean the same thing.
	if _, err := f.pool.Exec(ctx, `
		UPDATE service_upgrades SET created_at = $2 WHERE id = $1`,
		res.UpgradeID, now.AddDate(0, 0, -30)); err != nil {
		t.Fatalf("age upgrade: %v", err)
	}

	cancelled, err = f.store.CancelStaleUpgrades(ctx)
	if err != nil {
		t.Fatalf("cancel stale: %v", err)
	}
	if len(cancelled) != 1 {
		t.Fatalf("cancel stale acted on %d upgrades, want 1", len(cancelled))
	}

	var (
		upgradeStatus string
		invoiceStatus string
	)
	if err := f.pool.QueryRow(ctx, `SELECT status FROM service_upgrades WHERE id = $1`,
		res.UpgradeID).Scan(&upgradeStatus); err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if upgradeStatus != service.UpgradeCancelled {
		t.Fatalf("upgrade status = %q, want cancelled", upgradeStatus)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1`,
		res.InvoiceID).Scan(&invoiceStatus); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if invoiceStatus != "void" {
		t.Fatalf("invoice status = %q, want void", invoiceStatus)
	}

	// The subscription never moved.
	var subPrice string
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != f.priceID {
		t.Fatal("a cancelled upgrade repriced the subscription")
	}
}

// A stale *paid* upgrade is never voided: the money moved, and the worker owns that case.
func TestCancelStaleUpgradesLeavesPaidInvoicesAlone(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()

	_, targetPrice := f.addUpgradeTarget(t, 2000, "monthly")
	res, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE invoices SET status = 'paid', amount_paid_minor = total_minor WHERE id = $1`,
		res.InvoiceID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE service_upgrades SET created_at = $2 WHERE id = $1`,
		res.UpgradeID, now.AddDate(0, 0, -30)); err != nil {
		t.Fatalf("age: %v", err)
	}

	cancelled, err := f.store.CancelStaleUpgrades(ctx)
	if err != nil {
		t.Fatalf("cancel stale: %v", err)
	}
	if len(cancelled) != 0 {
		t.Fatalf("cancel stale voided %d paid upgrades", len(cancelled))
	}

	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1`,
		res.InvoiceID).Scan(&status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != "paid" {
		t.Fatalf("invoice status = %q, want paid", status)
	}
}

// Applying a downgrade before its credit is posted is refused: the customer would get the
// cheaper price and keep the money they are owed.
func TestApplyingADowngradeNeedsTheCreditPosted(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 2000, "monthly", now, start, end)
	ctx := context.Background()

	_, targetPrice := f.addUpgradeTarget(t, 1000, "monthly")
	res, err := f.store.Upgrade(ctx, service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}

	// ApplyPendingUpgrades skips it: there is no paid invoice to wait for.
	applied, err := f.store.ApplyPendingUpgrades(ctx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("apply moved %d downgrades without a credit", len(applied))
	}

	// The direct call is refused too.
	err = f.store.ApplyUpgrade(ctx, res.UpgradeID)
	if !errors.Is(err, service.ErrCreditNotPosted) {
		t.Fatalf("error = %v, want ErrCreditNotPosted", err)
	}

	var subPrice string
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != f.priceID {
		t.Fatal("a downgrade repriced the subscription before its credit was posted")
	}

	// Once the credits package has posted the credit, ApplyDowngrade moves the service.
	if err := f.store.ApplyDowngrade(ctx, res.UpgradeID); err != nil {
		t.Fatalf("apply downgrade: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != targetPrice {
		t.Fatalf("subscription price = %s, want the target %s", subPrice, targetPrice)
	}
}
