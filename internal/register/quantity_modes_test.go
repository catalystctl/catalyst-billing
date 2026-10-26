// SF-09: `disabled`, `combined` and `separated` are three different purchase shapes, not
// two spellings of one.
//
// These tests run against the real database because the properties under test are rows:
// how many order items, subscriptions and services a quantity produces, which schedule a
// service is linked to, and whether a refused request consumed stock. A mock would prove
// none of that.
package register_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/register"
	"github.com/jackc/pgx/v5"
)

// setPolicy sets a product's quantity policy, which is what an operator does from the
// merchandising screen.
func (f *fixture) setPolicy(t *testing.T, productID, policy string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE products SET allow_quantity = $2 WHERE id = $1`, productID, policy); err != nil {
		t.Fatalf("set allow_quantity=%s: %v", policy, err)
	}
}

// Combined: quantity N is one order line, one subscription and one service carrying N.
func TestCombinedQuantityIsOneServiceOfN(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "combined@example.test")
	productID, priceID := f.product(t, "combined-box", 0, 0) // free, so it activates now
	f.setPolicy(t, productID, "combined")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 3}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !res.Activated || len(res.ServiceIDs) != 1 {
		t.Fatalf("combined quantity 3 = activated %v, %d service(s); want 1 service",
			res.Activated, len(res.ServiceIDs))
	}
	if len(res.SubscriptionIDs) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(res.SubscriptionIDs))
	}

	ctx := context.Background()
	var items int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_items WHERE order_id = $1`, res.OrderID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Fatalf("order items = %d, want 1", items)
	}
	var quantity int
	var itemSubscription string
	if err := f.pool.QueryRow(ctx, `
		SELECT quantity, COALESCE(subscription_id::text, '')
		  FROM order_items WHERE order_id = $1`, res.OrderID).
		Scan(&quantity, &itemSubscription); err != nil {
		t.Fatal(err)
	}
	if quantity != 3 {
		t.Fatalf("order line quantity = %d, want 3", quantity)
	}
	if itemSubscription != res.SubscriptionIDs[0] {
		t.Fatalf("order line subscription = %q, want %q", itemSubscription, res.SubscriptionIDs[0])
	}

	// The service is the one that carries the count, and it is linked to its own schedule
	// and to the order that sold it.
	var (
		serviceQty   int
		orderID      string
		subscription string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT quantity, COALESCE(order_id::text, ''), COALESCE(subscription_id::text, '')
		  FROM services WHERE id = $1`, res.ServiceIDs[0]).
		Scan(&serviceQty, &orderID, &subscription); err != nil {
		t.Fatal(err)
	}
	if serviceQty != 3 {
		t.Fatalf("service quantity = %d, want 3", serviceQty)
	}
	if orderID != res.OrderID || subscription != res.SubscriptionIDs[0] {
		t.Fatalf("service order/subscription = %q/%q", orderID, subscription)
	}
	if customerID == "" { // the fixture's customer is the order's owner
		t.Fatal("no customer")
	}
}

// Separated: quantity N is N independent order lines, subscriptions and services.
func TestSeparatedQuantityIsNIndependentUnits(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "separated@example.test")
	productID, priceID := f.product(t, "separated-box", 0, 0) // free, so it activates now
	f.setPolicy(t, productID, "separated")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 3}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !res.Activated || res.InvoiceID != "" {
		t.Fatalf("a free separated order should activate with no invoice: %+v", res)
	}
	if len(res.ServiceIDs) != 3 || len(res.SubscriptionIDs) != 3 {
		t.Fatalf("services/subscriptions = %d/%d, want 3/3",
			len(res.ServiceIDs), len(res.SubscriptionIDs))
	}

	ctx := context.Background()
	// Three order lines of one, each with its own schedule, in a deterministic order.
	rows, err := f.pool.Query(ctx, `
		SELECT position, quantity, COALESCE(subscription_id::text, '')
		  FROM order_items WHERE order_id = $1 ORDER BY position`, res.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lines := 0
	seen := map[string]bool{}
	for rows.Next() {
		var position, quantity int
		var subscription string
		if err := rows.Scan(&position, &quantity, &subscription); err != nil {
			t.Fatal(err)
		}
		if position != lines+1 {
			t.Fatalf("position = %d on line %d, want %d", position, lines+1, lines+1)
		}
		if quantity != 1 {
			t.Fatalf("separated line quantity = %d, want 1", quantity)
		}
		if subscription == "" || seen[subscription] {
			t.Fatalf("line %d has subscription %q (duplicate or empty)", position, subscription)
		}
		seen[subscription] = true
		lines++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != 3 {
		t.Fatalf("order lines = %d, want 3", lines)
	}

	// Three services, each linked to the corresponding subscription, quantity one.
	rows, err = f.pool.Query(ctx, `
		SELECT quantity, COALESCE(order_id::text, ''), COALESCE(subscription_id::text, '')
		  FROM services WHERE customer_id = $1 ORDER BY created_at, id`, customerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	services := 0
	serviceSubs := map[string]bool{}
	for rows.Next() {
		var quantity int
		var orderID, subscription string
		if err := rows.Scan(&quantity, &orderID, &subscription); err != nil {
			t.Fatal(err)
		}
		if quantity != 1 {
			t.Fatalf("separated service quantity = %d, want 1", quantity)
		}
		if orderID != res.OrderID {
			t.Fatalf("service order = %q, want %q", orderID, res.OrderID)
		}
		if subscription == "" || serviceSubs[subscription] {
			t.Fatalf("service subscription %q is empty or shared", subscription)
		}
		serviceSubs[subscription] = true
		services++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if services != 3 {
		t.Fatalf("services = %d, want 3", services)
	}
	if len(serviceSubs) != 3 {
		t.Fatalf("distinct service subscriptions = %d, want 3", len(serviceSubs))
	}
}

// A paid separated order prices every unit, taxes it, discounts it once, and provisions
// one service per unit when the invoice settles — without duplicating anything on replay.
func TestSeparatedPaidOrderPricingTaxCouponAndProvisioning(t *testing.T) {
	f := newFixture(t, vat20())
	customerID, userID := f.customer(t, "separated-paid@example.test")
	productID, priceID := f.product(t, "separated-paid", 1200, 300)
	f.setPolicy(t, productID, "separated")
	f.coupon(t, "TEN", "percentage", "10")

	// A priced option is per unit, exactly as the catalog says: separated must charge it
	// once per independent service, not once per cart line.
	ctx := context.Background()
	var optionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, 'RAM', 'dropdown', '["8 GB"]', true) RETURNING id`,
		productID).Scan(&optionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice,
			amount_minor, setup_minor)
		VALUES ($1, $2, '8 GB', 200, 100)`, optionID, priceID); err != nil {
		t.Fatal(err)
	}

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{{
			PriceVersionID: priceID,
			Quantity:       3,
			Config:         map[string]string{optionID: "8 GB"},
		}},
		CouponCode: "TEN",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if res.Status != "pending_payment" || res.InvoiceID == "" {
		t.Fatalf("paid separated order = %+v", res)
	}
	if len(res.SubscriptionIDs) != 3 {
		t.Fatalf("subscriptions = %d, want 3", len(res.SubscriptionIDs))
	}
	// 3 × (1200 + 200 + 300 + 100) = 5400; 10% off = 540; net 4860; 20% VAT = 972;
	// total 5832.
	if res.Discount.Minor() != 540 || res.Subtotal.Minor() != 4860 ||
		res.Tax.Minor() != 972 || res.Total.Minor() != 5832 {
		t.Fatalf("money = discount %d, subtotal %d, tax %d, total %d; want 540/4860/972/5832",
			res.Discount.Minor(), res.Subtotal.Minor(), res.Tax.Minor(), res.Total.Minor())
	}

	// Every charge line names the option and carries the option's amount.
	// Three price lines and three setup lines, each of quantity one, plus one discount line.
	var charges, discounts int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE line_kind = 'charge'),
		       count(*) FILTER (WHERE line_kind = 'discount')
		  FROM invoice_lines WHERE invoice_id = $1`, res.InvoiceID).
		Scan(&charges, &discounts); err != nil {
		t.Fatal(err)
	}
	if charges != 6 || discounts != 1 {
		t.Fatalf("invoice lines = %d charge(s), %d discount(s); want 6 and 1", charges, discounts)
	}
	var maxQuantity int
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(max(quantity), 0)::int FROM invoice_lines
		 WHERE invoice_id = $1 AND line_kind = 'charge'`, res.InvoiceID).Scan(&maxQuantity); err != nil {
		t.Fatal(err)
	}
	if maxQuantity != 1 {
		t.Fatalf("separated charge line quantity = %d, want 1", maxQuantity)
	}
	var configured int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM invoice_lines
		 WHERE invoice_id = $1 AND line_kind = 'charge' AND description LIKE '%8 GB%'`,
		res.InvoiceID).Scan(&configured); err != nil {
		t.Fatal(err)
	}
	if configured != 6 {
		t.Fatalf("charge lines naming the option = %d, want 6", configured)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_items WHERE order_id = $1`, res.OrderID).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if charges != 3 {
		t.Fatalf("order lines = %d, want 3", charges)
	}

	// Settle the invoice through the real applier, which is what a gateway webhook does.
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		applier := fulfilment.NewApplier(f.pool)
		_, err := applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider:          "manual",
			ProviderAccount:   "acct",
			ProviderPaymentID: "separated-" + res.InvoiceID,
			InvoiceID:         res.InvoiceID,
			AmountMinor:       res.Total.Minor(),
			Currency:          "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	countServices := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM services WHERE customer_id = $1`, customerID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countServices(); n != 3 {
		t.Fatalf("services after payment = %d, want 3", n)
	}
	// Each order line carries the option's price and setup for its one unit.
	var priced int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM order_items
		 WHERE order_id = $1 AND quantity = 1 AND unit_minor = 1400 AND setup_minor = 400`,
		res.OrderID).Scan(&priced); err != nil {
		t.Fatal(err)
	}
	if priced != 3 {
		t.Fatalf("order lines priced 1400 + 400 setup = %d, want 3", priced)
	}
	// Each service funded by its own line's subscription, and each quantity one.
	var distinct int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(DISTINCT s.subscription_id)
		  FROM services s
		  JOIN order_items oi ON oi.subscription_id = s.subscription_id
		 WHERE s.customer_id = $1 AND oi.order_id = $2`, customerID, res.OrderID).
		Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 3 {
		t.Fatalf("services with a distinct subscription = %d, want 3", distinct)
	}

	// Replaying the same provider payment must not provision a second set: the operation
	// key is anchored to the order line, so the applier finds the existing work.
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		applier := fulfilment.NewApplier(f.pool)
		_, err := applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider:          "manual",
			ProviderAccount:   "acct",
			ProviderPaymentID: "separated-" + res.InvoiceID,
			InvoiceID:         res.InvoiceID,
			AmountMinor:       res.Total.Minor(),
			Currency:          "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("replay settlement: %v", err)
	}
	if n := countServices(); n != 3 {
		t.Fatalf("services after a replayed payment = %d, want 3", n)
	}
}

// Stock and the per-user limit count units, not order lines: a separated quantity of
// three consumes three of the shop's stock and three of the customer's allowance.
func TestSeparatedQuantityRespectsStockAndPerUserLimit(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "separated-limits@example.test")
	productID, priceID := f.product(t, "separated-limited", 1000, 0)
	f.setPolicy(t, productID, "separated")

	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`UPDATE products SET stock = 2 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}

	// Above stock: refused, and nothing is consumed.
	_, err := f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 3}},
	})
	if !errors.Is(err, register.ErrOutOfStock) {
		t.Fatalf("err = %v, want ErrOutOfStock", err)
	}
	var stock int
	if err := f.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 2 {
		t.Fatalf("a refused separated checkout consumed stock: %d", stock)
	}
	var orders int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("a refused separated checkout wrote %d order line(s)", orders)
	}

	// Stock is enough for two, but the per-user limit is two and the request is three.
	if _, err := f.pool.Exec(ctx,
		`UPDATE products SET stock = 10, per_user_limit = 2 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}
	_, err = f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 3}},
	})
	if !errors.Is(err, register.ErrUserLimitReached) {
		t.Fatalf("err = %v, want ErrUserLimitReached", err)
	}

	// Two units succeed, leaving no room for a third.
	if _, err := f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 2}},
	}); err != nil {
		t.Fatalf("buying the allowed two: %v", err)
	}
	_, err = f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 1}},
	})
	if !errors.Is(err, register.ErrUserLimitReached) {
		t.Fatalf("a third unit: err = %v, want ErrUserLimitReached", err)
	}
}

// A separated expansion is bounded by the invoice-line limit rather than writing an
// order nobody can print.
func TestSeparatedExpansionIsBounded(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "separated-bound@example.test")
	productID, priceID := f.product(t, "separated-bound", 100, 0)
	f.setPolicy(t, productID, "separated")

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 51}},
	})
	if !errors.Is(err, register.ErrTooManyLines) {
		t.Fatalf("err = %v, want ErrTooManyLines", err)
	}
}

// Two carts that name the same products in opposite orders must not deadlock: checkout
// locks products in sorted id order, and the separated expansion does not change that.
func TestSeparatedConcurrentCheckoutsDoNotDeadlock(t *testing.T) {
	f := newFixture(t, noTax())
	_, firstUser := f.customer(t, "race-a@example.test")
	_, secondUser := f.customer(t, "race-b@example.test")
	firstProduct, firstPrice := f.product(t, "race-a", 100, 0)
	secondProduct, secondPrice := f.product(t, "race-b", 200, 0)
	f.setPolicy(t, firstProduct, "separated")
	f.setPolicy(t, secondProduct, "separated")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: firstUser,
			Lines: []register.CheckoutLine{
				{PriceVersionID: firstPrice, Quantity: 2},
				{PriceVersionID: secondPrice, Quantity: 2},
			},
		})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: secondUser,
			Lines: []register.CheckoutLine{
				{PriceVersionID: secondPrice, Quantity: 2},
				{PriceVersionID: firstPrice, Quantity: 2},
			},
		})
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent checkout: %v", err)
		}
	}

	var lines int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM order_items`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines != 8 { // two orders × (2 + 2) separated units
		t.Fatalf("order lines = %d, want 8", lines)
	}
}
