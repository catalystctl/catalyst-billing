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

// The config-option tests assert two things: that the resolver sums exactly, and that the
// selection is a priced fact stored on the service rather than a form field re-read at
// renewal. The second is what makes "8 GB renews at the 8 GB price" true.

// seedOptions creates a dropdown RAM option and a checkbox backup option on f's product, with
// prices on f's price version.
func (f *fixture) seedOptions(t *testing.T) (ramID, backupID string) {
	t.Helper()
	ctx := context.Background()

	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ramID, err = f.store.CreateConfigOptionTx(ctx, tx, service.ConfigOptionInput{
			ProductID:   f.productID,
			Name:        "RAM",
			Type:        "dropdown",
			Choices:     []string{"2 GB", "8 GB", "16 GB"},
			EnvVariable: "RAM_MB",
			Required:    true,
			SortOrder:   1,
		})
		if err != nil {
			return err
		}
		backupID, err = f.store.CreateConfigOptionTx(ctx, tx, service.ConfigOptionInput{
			ProductID:   f.productID,
			Name:        "Backup",
			Type:        "checkbox",
			EnvVariable: "BACKUP",
			SortOrder:   2,
		})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed options: %v", err)
	}
	return ramID, backupID
}

// The resolver sums a selection exactly: 4.00 + 1.25 + 0.99 is 6.24, not 6.239999…
func TestConfigOptionPricingSumsExactly(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, backupID := f.seedOptions(t)

	// 8 GB costs 400, backup costs 125, and a hidden third option costs 99.
	var extraID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, hidden, sort_order)
		VALUES ($1, 'Priority', 'checkbox', '[]'::jsonb, true, 3) RETURNING id`,
		f.productID).Scan(&extraID); err != nil {
		t.Fatalf("extra option: %v", err)
	}

	for _, price := range []struct {
		option string
		choice string
		amount int64
		setup  int64
	}{
		{ramID, "8 GB", 400, 0},
		{ramID, "2 GB", 0, 0},
		{ramID, "16 GB", 900, 0},
		{backupID, "on", 125, 50},
		{extraID, "on", 99, 0},
	} {
		if _, err := f.store.SetConfigOptionPrice(ctx, price.option, f.priceID, price.choice,
			price.amount, price.setup); err != nil {
			t.Fatalf("price %s/%s: %v", price.option, price.choice, err)
		}
	}

	resolved, err := f.store.ResolveSelection(ctx, f.productID, f.priceID, map[string]string{
		ramID:    "8 GB",
		backupID: "on",
		extraID:  "on",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if resolved.Total.Minor() != 624 {
		t.Fatalf("total = %d minor, want 624 (400 + 125 + 99)", resolved.Total.Minor())
	}
	if resolved.SetupTotal.Minor() != 50 {
		t.Fatalf("setup total = %d minor, want 50", resolved.SetupTotal.Minor())
	}
	if len(resolved.Options) != 3 {
		t.Fatalf("%d contributions, want 3", len(resolved.Options))
	}
	if resolved.Total.Currency().Code() != "USD" {
		t.Fatalf("currency = %s, want USD", resolved.Total.Currency().Code())
	}

	// The environment the provisioner receives is derived here, not in the provisioner.
	if resolved.Env["RAM_MB"] != "8 GB" {
		t.Fatalf("env RAM_MB = %q, want 8 GB", resolved.Env["RAM_MB"])
	}
	if resolved.Env["BACKUP"] != "on" {
		t.Fatalf("env BACKUP = %q, want on", resolved.Env["BACKUP"])
	}
}

// A choice with no price row is free rather than an error, which is what the migration
// documents.
func TestConfigOptionWithoutPriceIsFree(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	resolved, err := f.store.ResolveSelection(ctx, f.productID, f.priceID, map[string]string{
		ramID: "2 GB",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolved.Total.IsZero() {
		t.Fatalf("total = %d, want 0", resolved.Total.Minor())
	}
}

// A required option that is missing is refused: "8 GB" is a requirement, not a suggestion.
func TestRequiredOptionMustBeChosen(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	f.seedOptions(t)

	_, err := f.store.ResolveSelection(ctx, f.productID, f.priceID, map[string]string{})
	if !errors.Is(err, service.ErrOptionNotAvailable) {
		t.Fatalf("error = %v, want ErrOptionNotAvailable", err)
	}
}

// A value the option does not offer is refused rather than ignored: silently dropping it would
// charge for a selection the customer did not get.
func TestUnknownChoiceIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	_, err := f.store.ResolveSelection(ctx, f.productID, f.priceID, map[string]string{
		ramID: "64 GB",
	})
	if !errors.Is(err, service.ErrOptionNotAvailable) {
		t.Fatalf("error = %v, want ErrOptionNotAvailable", err)
	}
}

// An option id that belongs to another product is refused, so a selection cannot smuggle in a
// price from a product the customer is not buying.
func TestForeignOptionIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	var otherProduct string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('other-product', 'Other') RETURNING id`).
		Scan(&otherProduct); err != nil {
		t.Fatalf("other product: %v", err)
	}

	_, err := f.store.ResolveSelection(ctx, otherProduct, f.priceID, map[string]string{
		ramID: "8 GB",
	})
	if err == nil {
		t.Fatal("a selection naming another product's option was accepted")
	}
}

// Changing an upgradable option on a live service stores the new selection and charges the
// prorated delta.
func TestChangingAnUpgradableOptionChargesTheProratedDelta(t *testing.T) {
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f := newFixtureWithPeriod(t, 1000, "monthly", now, start, end)
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	for _, p := range []struct {
		choice string
		amount int64
	}{
		{"2 GB", 0},
		{"8 GB", 1000}, // a 10.00 difference
	} {
		if _, err := f.store.SetConfigOptionPrice(ctx, ramID, f.priceID, p.choice, p.amount, 0); err != nil {
			t.Fatalf("price %s: %v", p.choice, err)
		}
	}

	// The service starts on the free 2 GB choice.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
		VALUES ($1, $2, '2 GB', 0, 0)`, f.serviceID, ramID); err != nil {
		t.Fatalf("seed selection: %v", err)
	}

	res, err := f.store.ChangeServiceConfig(ctx, service.ConfigChangeRequest{
		ServiceID: f.serviceID, ConfigOptionID: ramID, Value: "8 GB", UserID: f.userID,
		Reason: "more memory",
	})
	if err != nil {
		t.Fatalf("change config: %v", err)
	}

	if res.Direction != "upgrade" {
		t.Fatalf("direction = %q, want upgrade", res.Direction)
	}
	if res.InvoiceID == "" {
		t.Fatal("no invoice was issued for an option upgrade")
	}
	// 15.5 of 31 days of a 10.00 difference is exactly 5.00.
	if res.ProratedMinor != 500 {
		t.Fatalf("prorated = %d, want 500", res.ProratedMinor)
	}

	// The selection is stored with the price it contributed at selection time, so a later
	// option-price change cannot silently re-price the service.
	var (
		value  string
		amount int64
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT value, amount_minor FROM service_configs
		WHERE service_id = $1 AND config_option_id = $2`, f.serviceID, ramID).
		Scan(&value, &amount); err != nil {
		t.Fatalf("read selection: %v", err)
	}
	if value != "8 GB" {
		t.Fatalf("value = %q, want 8 GB", value)
	}
	if amount != 1000 {
		t.Fatalf("stored amount = %d, want 1000", amount)
	}
}

// An option whose upgradable flag is false cannot be changed on a live service.
func TestNonUpgradableOptionIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	notUpgradable := false
	if _, err := f.store.UpdateConfigOption(ctx, ramID, service.ConfigOptionInput{
		ProductID:   f.productID,
		Name:        "RAM",
		Type:        "dropdown",
		Choices:     []string{"2 GB", "8 GB", "16 GB"},
		EnvVariable: "RAM_MB",
		Required:    true,
		Upgradable:  &notUpgradable,
	}); err != nil {
		t.Fatalf("update option: %v", err)
	}

	_, err := f.store.ChangeServiceConfig(ctx, service.ConfigChangeRequest{
		ServiceID: f.serviceID, ConfigOptionID: ramID, Value: "8 GB", UserID: f.userID,
	})
	if !errors.Is(err, service.ErrOptionNotUpgradable) {
		t.Fatalf("error = %v, want ErrOptionNotUpgradable", err)
	}
}

// Deleting an option a live service has selected is refused: the selection is a priced fact,
// and a renewal that cannot re-price is a defect.
func TestDeletingASelectedOptionIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
		VALUES ($1, $2, '8 GB', 400, 0)`, f.serviceID, ramID); err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	if err := f.store.DeleteConfigOption(ctx, ramID); err == nil {
		t.Fatal("an option a live service has selected was deleted")
	}
}

// A price for an option and a price version on different products is refused, so a typo cannot
// create a price no selection can ever match.
func TestConfigOptionPriceMustMatchTheProduct(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	var otherProduct, otherPrice string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('mismatch', 'Mismatch') RETURNING id`).
		Scan(&otherProduct); err != nil {
		t.Fatalf("other product: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 1000, 'monthly', 1, 'UTC') RETURNING id`, otherProduct).
		Scan(&otherPrice); err != nil {
		t.Fatalf("other price: %v", err)
	}

	_, err := f.store.SetConfigOptionPrice(ctx, ramID, otherPrice, "8 GB", 400, 0)
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

// An option edit that omits `upgradable` preserves the current value, so a screen that does not
// show the flag cannot silently turn a changeable option into a fixed one.
func TestUpdatableFlagIsPreservedWhenOmitted(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	// It defaults to true.
	if _, err := f.store.UpdateConfigOption(ctx, ramID, service.ConfigOptionInput{
		ProductID: f.productID, Name: "Memory", Type: "dropdown",
		Choices: []string{"2 GB", "8 GB"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var upgradable bool
	if err := f.pool.QueryRow(ctx, `SELECT upgradable FROM config_options WHERE id = $1`, ramID).
		Scan(&upgradable); err != nil {
		t.Fatalf("read option: %v", err)
	}
	if !upgradable {
		t.Fatal("an edit that omitted `upgradable` reset it to false")
	}
}

// A dropdown with no choices is refused at validation time rather than at checkout.
func TestConfigOptionValidation(t *testing.T) {
	cases := []service.ConfigOptionInput{
		{ProductID: "p", Name: "", Type: "dropdown", Choices: []string{"a"}},
		{ProductID: "p", Name: "x", Type: "wat"},
		{ProductID: "p", Name: "x", Type: "dropdown"},
		{ProductID: "p", Name: "x", Type: "dropdown", Choices: []string{"a", "a"}},
		{ProductID: "p", Name: "x", Type: "dropdown", Choices: []string{""}},
		{ProductID: "", Name: "x", Type: "text"},
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d was accepted: %+v", i, c)
		}
	}
	valid := service.ConfigOptionInput{ProductID: "p", Name: "x", Type: "checkbox"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid checkbox was refused: %v", err)
	}
}

// The stored selection is readable through the service, joined to the option's definition.
func TestServiceConfigsAreReadable(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
		VALUES ($1, $2, '8 GB', 400, 25)`, f.serviceID, ramID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	configs, err := f.store.ServiceConfigs(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("%d rows, want 1", len(configs))
	}
	c := configs[0]
	if c.Name != "RAM" || c.Type != "dropdown" || c.EnvVariable != "RAM_MB" {
		t.Fatalf("the row is missing its option definition: %+v", c)
	}
	if c.AmountMinor != 400 || c.SetupMinor != 25 {
		t.Fatalf("amounts = %d/%d, want 400/25", c.AmountMinor, c.SetupMinor)
	}
	if len(c.Choices) != 3 {
		t.Fatalf("%d choices, want 3", len(c.Choices))
	}
}

// A money assertion in this file must never rely on a float; this pins the exact value the
// resolver produces for a deliberately awkward sum.
func TestConfigOptionSumIsExactForAwkwardAmounts(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, backupID := f.seedOptions(t)

	// 0.01 + 0.02 + 0.10 = 0.13 exactly. A float implementation would produce
	// 0.13000000000000003 and could round to 13 or 14 depending on when it converted.
	for _, p := range []struct {
		id     string
		choice string
		amount int64
	}{
		{ramID, "2 GB", 1},
		{ramID, "8 GB", 2},
		{backupID, "on", 10},
	} {
		if _, err := f.store.SetConfigOptionPrice(ctx, p.id, f.priceID, p.choice, p.amount, 0); err != nil {
			t.Fatalf("price: %v", err)
		}
	}

	resolved, err := f.store.ResolveSelection(ctx, f.productID, f.priceID, map[string]string{
		ramID:    "8 GB",
		backupID: "on",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := resolved.Total.Minor(), int64(12); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
	if got, want := resolved.Total.String(), "0.12"; got != want {
		t.Fatalf("rendered total = %q, want %q", got, want)
	}
}

// The transaction variant prices inside a caller's transaction, which is what checkout needs so
// the quote and the order commit together.
func TestResolveSelectionTxUsesTheCallersTransaction(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	if _, err := f.store.SetConfigOptionPrice(ctx, ramID, f.priceID, "8 GB", 400, 0); err != nil {
		t.Fatalf("price: %v", err)
	}

	var total int64
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		resolved, err := f.store.ResolveSelectionTx(ctx, tx, f.productID, f.priceID,
			map[string]string{ramID: "8 GB"})
		if err != nil {
			return err
		}
		total = resolved.Total.Minor()
		return nil
	})
	if err != nil {
		t.Fatalf("resolve in tx: %v", err)
	}
	if total != 400 {
		t.Fatalf("total = %d, want 400", total)
	}
	_ = money.Zero
}

// Changing a service's option to a value the option does not offer is refused. Storing it would
// price as free (no matching row) and leave the provisioner with a selection it cannot act on.
func TestChangingToAnUnavailableValueIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	ramID, _ := f.seedOptions(t)

	_, err := f.store.ChangeServiceConfig(ctx, service.ConfigChangeRequest{
		ServiceID: f.serviceID, ConfigOptionID: ramID, Value: "64 GB", UserID: f.userID,
	})
	if !errors.Is(err, service.ErrOptionNotAvailable) {
		t.Fatalf("error = %v, want ErrOptionNotAvailable", err)
	}

	// Nothing was stored.
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM service_configs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d selections were stored by a refused change", n)
	}
}
