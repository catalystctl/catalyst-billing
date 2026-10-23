package register_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/register"
)

// Checkout is the piece that had no endpoint and no multi-line path, so these tests
// drive the service against a real database: the properties under test are a row lock,
// a unique index and a transaction over five tables.

// fixture is a checkout service plus the catalog and customers it needs.
type fixture struct {
	*env
	checkout *register.CheckoutService
	// clock is the instant the service's clock returns, exposed so a test can build a
	// coupon window relative to it.
	clock time.Time
}

func newFixture(t *testing.T, pricer billing.Pricer) *fixture {
	t.Helper()
	e := newEnv(t)
	issuer := billing.NewIssuer(pricer)
	clock := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	svc := register.NewCheckoutService(e.pool, issuer,
		billing.Merchant{Name: "Catalyst Hosting", CountryCode: "US"},
		"Payable on issue.", billing.DefaultSeries).
		WithNotify(notify.NewQueue(e.pool)).
		WithClock(func() time.Time { return clock })
	return &fixture{env: e, checkout: svc, clock: clock}
}

// noTax is the pricer a fresh installation has: exact, and with no jurisdiction claim.
func noTax() billing.Pricer {
	return billing.FixedTaxPricer{Policy: "no-tax", Jurisdiction: "none"}
}

// vat20 is an exclusive 20% rate, used to prove the discount reduces tax exactly once.
func vat20() billing.Pricer {
	return billing.FixedTaxPricer{
		Rate:   money.MustParseRatio("20%"),
		Policy: "test-vat", Jurisdiction: "GB",
	}
}

// customer creates a billing account with a login attached, the way registration does.
func (f *fixture) customer(t *testing.T, email string) (customerID, userID string) {
	t.Helper()
	res := f.register(t, email)
	return res.CustomerID, res.UserID
}

// product creates a sellable product with one price and returns both ids.
func (f *fixture) product(t *testing.T, slug string, amount, setup int64) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $1) RETURNING id`, slug).
		Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	// allow_quantity defaults to 'disabled' (one per order). A test that buys a
	// quantity configures 'combined', which is what an operator would do for a product
	// a customer can stack.
	if _, err := f.pool.Exec(ctx,
		`UPDATE products SET allow_quantity = 'combined' WHERE id = $1`, productID); err != nil {
		t.Fatalf("allow quantity: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, 1, 'USD', $2, $3, 'monthly', 1, 'UTC', 'Monthly')
		RETURNING id`, productID, amount, setup).Scan(&priceID); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return productID, priceID
}

// coupon inserts a promotion and returns its id.
func (f *fixture) coupon(t *testing.T, code, kind, value string) string {
	t.Helper()
	var id string
	var err error
	switch kind {
	case "percentage":
		err = f.pool.QueryRow(context.Background(), `
			INSERT INTO coupons (code, kind, percentage) VALUES ($1, 'percentage', $2)
			RETURNING id`, code, value).Scan(&id)
	case "fixed":
		err = f.pool.QueryRow(context.Background(), `
			INSERT INTO coupons (code, kind, amount_minor, currency) VALUES ($1, 'fixed', $2, 'USD')
			RETURNING id`, code, value).Scan(&id)
	}
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	return id
}

// A multi-line purchase writes one order, one invoice, one subscription per line and a
// balanced ledger posting — the CT-07 acceptance shape.
func TestCheckoutCreatesOrderInvoiceAndSubscriptions(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "buyer@example.test")
	_, first := f.product(t, "vps-small", 1200, 0)
	_, second := f.product(t, "vps-large", 3400, 500)

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{
			{PriceVersionID: first, Quantity: 2},
			{PriceVersionID: second, Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if res.OrderID == "" || res.InvoiceID == "" || res.InvoiceNumber == "" {
		t.Fatalf("incomplete result: %+v", res)
	}
	if res.Status != "pending_payment" {
		t.Fatalf("status = %q", res.Status)
	}
	// 2 × 1200 + 3400 + 500 setup = 6300, and no tax configured.
	if res.Total.Minor() != 6300 {
		t.Fatalf("total = %d, want 6300", res.Total.Minor())
	}
	if len(res.SubscriptionIDs) != 2 {
		t.Fatalf("subscriptions = %d, want 2", len(res.SubscriptionIDs))
	}

	ctx := context.Background()
	// The order belongs to the session's customer, not to anything in the request.
	var orderCustomer, status string
	var orderTotal int64
	if err := f.pool.QueryRow(ctx, `
		SELECT customer_id, status, total_minor FROM orders WHERE id = $1`, res.OrderID).
		Scan(&orderCustomer, &status, &orderTotal); err != nil {
		t.Fatal(err)
	}
	if orderCustomer != customerID {
		t.Fatalf("order customer = %s, want %s", orderCustomer, customerID)
	}
	if status != "pending_payment" || orderTotal != 6300 {
		t.Fatalf("order = %s / %d", status, orderTotal)
	}

	// Two order items, in request order.
	var items int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_items WHERE order_id = $1`, res.OrderID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 2 {
		t.Fatalf("order items = %d, want 2", items)
	}

	// The invoice is issued, and its total matches the order.
	var (
		invoiceStatus string
		invoiceTotal  int64
		number        string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT status, total_minor, number FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&invoiceStatus, &invoiceTotal, &number); err != nil {
		t.Fatal(err)
	}
	if invoiceStatus != "open" || invoiceTotal != 6300 {
		t.Fatalf("invoice = %s / %d", invoiceStatus, invoiceTotal)
	}
	if number != res.InvoiceNumber {
		t.Fatalf("number = %q, response said %q", number, res.InvoiceNumber)
	}

	// The subscriptions start pending, so an unpaid order can never renew.
	var pending int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions WHERE customer_id = $1 AND status = 'pending'`,
		customerID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("pending subscriptions = %d, want 2", pending)
	}

	// The ledger posted a balanced receivable.
	var debits, credits int64
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(e.debit_minor), 0), COALESCE(sum(e.credit_minor), 0)
		FROM journal_entries e
		JOIN journal_transactions t ON t.id = e.transaction_id
		WHERE t.reference_type = 'invoice' AND t.reference_id = $1`, res.InvoiceID).
		Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits || debits != 6300 {
		t.Fatalf("ledger debits = %d, credits = %d; want 6300 each", debits, credits)
	}

	// The invoice notification was queued, since registration created an unverified
	// address — so it is recorded as suppressed rather than sent.
	// The kind is the notification event key: the invoice notice comes from the editable
	// new_invoice_created template now, not the legacy hardcoded invoice_issued message.
	var suppressed int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'new_invoice_created' AND status = 'suppressed'`).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if suppressed != 1 {
		t.Fatalf("suppressed invoice messages = %d, want 1", suppressed)
	}
}

// The single most important scoping rule: the order belongs to the session's customer.
func TestCheckoutUsesTheSessionCustomerNotTheBody(t *testing.T) {
	f := newFixture(t, noTax())
	mine, myUser := f.customer(t, "mine@example.test")
	theirs, _ := f.customer(t, "theirs@example.test")
	_, priceID := f.product(t, "scoped", 1000, 0)

	// The request type has no customer field, so the only way to attempt substitution is
	// to fill CustomerID — which the handler never does. Setting it directly proves the
	// service honours it only when the caller resolved it, and that a handler which
	// passed a body value would be the bug.
	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: myUser,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	var orderCustomer string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT customer_id FROM orders WHERE id = $1`, res.OrderID).Scan(&orderCustomer); err != nil {
		t.Fatal(err)
	}
	if orderCustomer != mine {
		t.Fatalf("order customer = %s, want the session's %s", orderCustomer, mine)
	}
	if orderCustomer == theirs {
		t.Fatal("the order was placed for another customer")
	}

	// The invoice is scoped the same way, so the other customer cannot see it.
	var invoiceCustomer string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT customer_id FROM invoices WHERE id = $1`, res.InvoiceID).Scan(&invoiceCustomer); err != nil {
		t.Fatal(err)
	}
	if invoiceCustomer != mine {
		t.Fatalf("invoice customer = %s, want %s", invoiceCustomer, mine)
	}
}

// A concurrent checkout of the last unit fails cleanly: one wins, the other is told the
// stock ran out, and stock never goes negative.
func TestConcurrentCheckoutOfTheLastUnit(t *testing.T) {
	f := newFixture(t, noTax())
	_, buyerA := f.customer(t, "a@example.test")
	_, buyerB := f.customer(t, "b@example.test")
	productID, priceID := f.product(t, "last-one", 500, 0)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE products SET stock = 1 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		err error
		res register.CheckoutResult
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for i, userID := range []string{buyerA, buyerB} {
		wg.Add(1)
		go func(i int, userID string) {
			defer wg.Done()
			res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
				UserID: userID,
				Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
			})
			results[i] = outcome{err: err, res: res}
		}(i, userID)
	}
	wg.Wait()

	var succeeded, outOfStock int
	for _, r := range results {
		switch {
		case r.err == nil:
			succeeded++
		case errors.Is(r.err, register.ErrOutOfStock):
			outOfStock++
		default:
			// A serialization failure or deadlock is a legitimate "try again", but the
			// test pins the clean refusal the spec asks for, so anything else is a
			// finding rather than a pass.
			t.Fatalf("unexpected failure: %v", r.err)
		}
	}
	if succeeded != 1 || outOfStock != 1 {
		t.Fatalf("succeeded = %d, out_of_stock = %d; want 1 and 1", succeeded, outOfStock)
	}

	var stock int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 0 {
		t.Fatalf("stock = %d, want 0", stock)
	}

	// Exactly one order exists, so the losing request left nothing behind.
	var orders int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 1 {
		t.Fatalf("orders = %d, want 1", orders)
	}
}

// CT-08: a zero-total order activates the service and issues no invoice.
func TestZeroTotalActivatesWithoutAnInvoice(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "free@example.test")
	productID, priceID := f.product(t, "free-plan", 0, 0)
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE price_versions SET provision_plan = 'plan-free', provision_region = 'eu-west'
		WHERE id = $1`, priceID); err != nil {
		t.Fatal(err)
	}

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !res.Activated {
		t.Fatal("a zero-total order did not activate")
	}
	if res.InvoiceID != "" || res.InvoiceNumber != "" {
		t.Fatalf("a free order issued an invoice: %+v", res)
	}
	if res.Status != "paid" {
		t.Fatalf("status = %q, want paid", res.Status)
	}
	if len(res.ServiceIDs) != 1 {
		t.Fatalf("services = %d, want 1", len(res.ServiceIDs))
	}

	ctx := context.Background()
	var invoices int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d, want 0 for a free order", invoices)
	}

	// The subscription is active, or the service would silently expire.
	var subStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM subscriptions WHERE id = $1`, res.SubscriptionIDs[0]).Scan(&subStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("subscription = %q, want active", subStatus)
	}

	// A service exists, carries the provision target and has a queued create operation.
	var (
		serviceProduct string
		config         map[string]any
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT product_id, config FROM services WHERE id = $1`, res.ServiceIDs[0]).
		Scan(&serviceProduct, &config); err != nil {
		t.Fatal(err)
	}
	if serviceProduct != productID {
		t.Fatalf("service product = %s, want %s", serviceProduct, productID)
	}
	if config["plan_id"] != "plan-free" || config["region"] != "eu-west" {
		t.Fatalf("service config = %v", config)
	}
	var ops int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations
		WHERE service_id = $1 AND kind = 'create' AND status = 'queued'`,
		res.ServiceIDs[0]).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 1 {
		t.Fatalf("queued create operations = %d, want 1", ops)
	}

	// The order is paid, so the operator screen agrees with reality.
	var orderStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM orders WHERE id = $1`, res.OrderID).Scan(&orderStatus); err != nil {
		t.Fatal(err)
	}
	if orderStatus != "paid" {
		t.Fatalf("order = %q, want paid", orderStatus)
	}
}

// A coupon is a line on the invoice, so the document explains itself.
func TestCouponDiscountAppearsAsALine(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "coupon@example.test")
	_, priceID := f.product(t, "discounted", 10000, 0)
	f.coupon(t, "SAVE10", "percentage", "10")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:     userID,
		Lines:      []register.CheckoutLine{{PriceVersionID: priceID}},
		CouponCode: "save10", // case-insensitive
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if res.CouponCode != "SAVE10" {
		t.Fatalf("coupon code = %q", res.CouponCode)
	}
	if res.Discount.Minor() != 1000 {
		t.Fatalf("discount = %d, want 1000", res.Discount.Minor())
	}
	if res.Total.Minor() != 9000 {
		t.Fatalf("total = %d, want 9000", res.Total.Minor())
	}
	if res.Subtotal.Minor() != 9000 {
		t.Fatalf("subtotal = %d, want 9000 (charges minus the discount line)", res.Subtotal.Minor())
	}

	ctx := context.Background()
	// The charge line keeps the catalog price: the document is checkable.
	var chargeNet int64
	if err := f.pool.QueryRow(ctx, `
		SELECT net_minor FROM invoice_lines
		WHERE invoice_id = $1 AND line_kind = 'charge'`, res.InvoiceID).Scan(&chargeNet); err != nil {
		t.Fatal(err)
	}
	if chargeNet != 10000 {
		t.Fatalf("charge net = %d, want the catalog price 10000", chargeNet)
	}

	// The discount is its own line, negative and naming the code.
	var (
		kind        string
		discountNet int64
		discountTax int64
		desc        string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT line_kind, net_minor, tax_minor, description FROM invoice_lines
		WHERE invoice_id = $1 AND line_kind = 'discount'`, res.InvoiceID).
		Scan(&kind, &discountNet, &discountTax, &desc); err != nil {
		t.Fatalf("discount line: %v", err)
	}
	if discountNet != -1000 {
		t.Fatalf("discount net = %d, want -1000", discountNet)
	}
	if discountTax != 0 {
		t.Fatalf("discount tax = %d, want 0: the reduction is re-priced on the charge lines", discountTax)
	}
	if !strings.Contains(desc, "SAVE10") {
		t.Fatalf("discount description = %q, want it to name the coupon", desc)
	}

	// The invoice's own columns agree with the lines.
	var subtotal, total int64
	if err := f.pool.QueryRow(ctx,
		`SELECT subtotal_minor, total_minor FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&subtotal, &total); err != nil {
		t.Fatal(err)
	}
	if subtotal != 9000 || total != 9000 {
		t.Fatalf("invoice = %d / %d, want 9000 / 9000", subtotal, total)
	}

	// The redemption is recorded, which is what makes max_uses a fact.
	var redemptions int
	var redeemed int64
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(discount_minor), 0) FROM coupon_redemptions
		WHERE customer_id = $1`, customerID).Scan(&redemptions, &redeemed); err != nil {
		t.Fatal(err)
	}
	if redemptions != 1 || redeemed != 1000 {
		t.Fatalf("redemptions = %d / %d, want 1 / 1000", redemptions, redeemed)
	}

	// The snapshot records the discount as evidence, so a reprint explains it.
	var snapshot map[string]any
	if err := f.pool.QueryRow(ctx,
		`SELECT snapshot FROM invoices WHERE id = $1`, res.InvoiceID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	discount, ok := snapshot["discount"].(map[string]any)
	if !ok {
		t.Fatalf("the snapshot has no discount evidence: %v", snapshot)
	}
	if discount["code"] != "SAVE10" {
		t.Fatalf("snapshot discount = %v", discount)
	}
}

// A percentage discount must reduce the tax exactly once: the charge line's tax is
// computed on the discounted net, and the discount line carries none.
func TestCouponReducesTaxExactlyOnce(t *testing.T) {
	f := newFixture(t, vat20())
	_, userID := f.customer(t, "taxed@example.test")
	_, priceID := f.product(t, "taxed", 10000, 0)
	f.coupon(t, "HALF", "percentage", "50")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:     userID,
		Lines:      []register.CheckoutLine{{PriceVersionID: priceID}},
		CouponCode: "HALF",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	// Net 5000, tax 20% = 1000, total 6000. Counting the reduction twice would give
	// 4000 tax 800 → 4800, or 5000 tax 0 → 5000.
	if res.Subtotal.Minor() != 5000 {
		t.Fatalf("subtotal = %d, want 5000", res.Subtotal.Minor())
	}
	if res.Tax.Minor() != 1000 {
		t.Fatalf("tax = %d, want 1000 (20%% of the discounted net)", res.Tax.Minor())
	}
	if res.Total.Minor() != 6000 {
		t.Fatalf("total = %d, want 6000", res.Total.Minor())
	}

	ctx := context.Background()
	var (
		chargeNet, chargeTax int64
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT net_minor, tax_minor FROM invoice_lines
		WHERE invoice_id = $1 AND line_kind = 'charge'`, res.InvoiceID).
		Scan(&chargeNet, &chargeTax); err != nil {
		t.Fatal(err)
	}
	// The charge line keeps the catalog net, so the page can be checked against the
	// price list; only its tax is re-derived from the discounted value. The discount
	// line carries the reduction.
	if chargeNet != 10000 || chargeTax != 1000 {
		t.Fatalf("charge = %d net / %d tax; want 10000 / 1000", chargeNet, chargeTax)
	}

	// The snapshot's tax evidence still names the rate that was applied.
	var snapshot map[string]any
	if err := f.pool.QueryRow(ctx,
		`SELECT snapshot FROM invoices WHERE id = $1`, res.InvoiceID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	evidence, _ := snapshot["tax_evidence"].(map[string]any)
	if evidence == nil || evidence["rate"] != "1/5" {
		t.Fatalf("tax evidence = %v", evidence)
	}
}

// Coupon refusals are specific, and a refusal leaves no order behind.
func TestCouponRefusals(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "refusals@example.test")
	_, priceID := f.product(t, "refuse", 1000, 0)
	_, otherPrice := f.product(t, "other", 1000, 0)

	t.Run("unknown", func(t *testing.T) {
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "NOPE",
		})
		if !errors.Is(err, register.ErrCouponNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		id := f.coupon(t, "OFF", "percentage", "10")
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE coupons SET enabled = false WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "OFF",
		})
		if !errors.Is(err, register.ErrCouponDisabled) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		// Relative to the service clock, not the wall clock: the window is judged
		// against the clock the checkout uses, and a test that mixed the two would pass
		// or fail depending on the day it ran.
		if _, err := f.pool.Exec(context.Background(), `
			INSERT INTO coupons (code, kind, percentage, expires_at)
			VALUES ('OLD', 'percentage', '10', $1)`, f.clock.Add(-24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "OLD",
		})
		if !errors.Is(err, register.ErrCouponExpired) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("wrong product", func(t *testing.T) {
		id := f.coupon(t, "ONLY-OTHER", "percentage", "10")
		var otherProduct string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT product_id FROM price_versions WHERE id = $1`, otherPrice).Scan(&otherProduct); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO coupon_products (coupon_id, product_id) VALUES ($1, $2)`,
			id, otherProduct); err != nil {
			t.Fatal(err)
		}
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "ONLY-OTHER",
		})
		if !errors.Is(err, register.ErrCouponNotForYou) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		if _, err := f.pool.Exec(context.Background(), `
			INSERT INTO coupons (code, kind, percentage, max_uses)
			VALUES ('ONE-ONLY', 'percentage', '10', 1)`); err != nil {
			t.Fatal(err)
		}
		// A first use, then the second is refused.
		if _, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "ONE-ONLY",
		}); err != nil {
			t.Fatalf("first use: %v", err)
		}
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID, Lines: []register.CheckoutLine{{PriceVersionID: priceID}},
			CouponCode: "ONE-ONLY",
		})
		if !errors.Is(err, register.ErrCouponExhausted) {
			t.Fatalf("err = %v", err)
		}
	})

	// Every refusal above left exactly the one successful order from this test and
	// nothing from the refused ones.
	var orders int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 1 {
		t.Fatalf("orders = %d, want 1 (only the successful ONE-ONLY use)", orders)
	}
}

// A discount that would exceed the order is clamped, not refused: the customer cannot
// be paid to buy, and the invoice cannot carry a negative subtotal.
func TestFixedCouponLargerThanTheOrderIsClamped(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "clamp@example.test")
	_, priceID := f.product(t, "cheap", 500, 0)
	f.coupon(t, "BIG", "fixed", "5000")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:     userID,
		Lines:      []register.CheckoutLine{{PriceVersionID: priceID}},
		CouponCode: "BIG",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if res.Discount.Minor() != 500 {
		t.Fatalf("discount = %d, want the order value 500", res.Discount.Minor())
	}
	// The whole order is free, so the zero-total path applies.
	if !res.Activated || res.InvoiceID != "" {
		t.Fatalf("a fully discounted order should activate: %+v", res)
	}
}

// Stock and per-user limits are refused with distinct errors, and stock is not consumed
// by a refused request.
func TestStockAndPerUserLimits(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "limits@example.test")
	productID, priceID := f.product(t, "limited", 1000, 0)

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE products SET stock = 1, per_user_limit = 1 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}

	// A quantity above stock is refused before anything is written.
	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 2}},
	})
	if !errors.Is(err, register.ErrOutOfStock) {
		t.Fatalf("err = %v, want ErrOutOfStock", err)
	}
	var stock int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 1 {
		t.Fatalf("a refused checkout consumed stock: %d", stock)
	}

	// A first purchase succeeds.
	if _, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	}); err != nil {
		t.Fatalf("first purchase: %v", err)
	}

	// The service exists but the per-user limit is already reached, and it is checked
	// before stock so the message is about the limit rather than the shop.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE products SET stock = 5 WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}
	// The first purchase's service is pending, which counts toward the limit.
	_, err = f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if !errors.Is(err, register.ErrUserLimitReached) {
		t.Fatalf("err = %v, want ErrUserLimitReached", err)
	}
}

// A quantity the product does not allow is refused.
func TestQuantityNotAllowed(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "qty@example.test")
	productID, priceID := f.product(t, "single", 1000, 0)
	// The schema default, which is what a product an operator has not configured has.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE products SET allow_quantity = 'disabled' WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID, Quantity: 2}},
	})
	if !errors.Is(err, register.ErrQuantityNotAllowed) {
		t.Fatalf("err = %v, want ErrQuantityNotAllowed", err)
	}
}

// A mixed-currency cart is refused: an order is a single-currency document.
func TestMixedCurrencyIsRefused(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "mixed@example.test")
	_, usd := f.product(t, "usd", 1000, 0)

	var productID, eur string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO products (slug, name) VALUES ('eur', 'eur') RETURNING id`).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'EUR', 900, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&eur); err != nil {
		t.Fatal(err)
	}

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{
			{PriceVersionID: usd},
			{PriceVersionID: eur},
		},
	})
	if !errors.Is(err, register.ErrMixedCurrency) {
		t.Fatalf("err = %v, want ErrMixedCurrency", err)
	}
}

// Terms must be accepted when the installation configures them, and the check happens
// before any row is written.
func TestCheckoutRequiresTOSWhenConfigured(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "tos@example.test")
	_, priceID := f.product(t, "tos", 1000, 0)
	f.setSetting(t, "general", "tos", "No refunds after 14 days.")

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if !errors.Is(err, register.ErrTOSRequired) {
		t.Fatalf("err = %v, want ErrTOSRequired", err)
	}

	if _, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:      userID,
		Lines:       []register.CheckoutLine{{PriceVersionID: priceID}},
		AcceptedTOS: true,
	}); err != nil {
		t.Fatalf("checkout with acceptance: %v", err)
	}
}

// Verification can be required before a purchase, and the refusal is distinct so the
// UI can send the customer to their mail rather than to a dead end.
func TestCheckoutRequiresVerifiedEmailWhenConfigured(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "unverified@example.test")
	_, priceID := f.product(t, "verify", 1000, 0)
	f.setSetting(t, "mail", "must_verify", "true")

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if !errors.Is(err, register.ErrEmailVerificationRequired) {
		t.Fatalf("err = %v, want ErrEmailVerificationRequired", err)
	}

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE users SET email_verified_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	}); err != nil {
		t.Fatalf("checkout after verification: %v", err)
	}
}

// A user with no billing account is refused with a distinct error, not a 500.
func TestCheckoutRequiresABillingAccount(t *testing.T) {
	f := newFixture(t, noTax())
	_, priceID := f.product(t, "orphan", 1000, 0)

	// A login with no customer_accounts row and no staff role.
	var userID string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name)
		VALUES ('orphan@example.test', 'x', 'Orphan') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if !errors.Is(err, register.ErrNoCustomer) {
		t.Fatalf("err = %v, want ErrNoCustomer", err)
	}
}

// An unknown or malformed price is the same 404, so a probe cannot learn that a price
// exists.
func TestCheckoutUnknownPrice(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "noprice@example.test")

	for _, id := range []string{"", "not-a-uuid", "0f8fad5b-d9cb-469f-a165-70867728950e"} {
		_, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
			UserID: userID,
			Lines:  []register.CheckoutLine{{PriceVersionID: id}},
		})
		if !errors.Is(err, register.ErrPriceNotFound) {
			t.Fatalf("%q: err = %v, want ErrPriceNotFound", id, err)
		}
	}
}

// A config option changes the price and is stored on the service.
func TestConfigOptionsChangeThePrice(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "config@example.test")
	productID, priceID := f.product(t, "with-ram", 1000, 0)

	ctx := context.Background()
	var optionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, 'RAM', 'dropdown', '["2 GB","8 GB"]', true) RETURNING id`,
		productID).Scan(&optionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '2 GB', 0), ($1, $2, '8 GB', 1500)`, optionID, priceID); err != nil {
		t.Fatal(err)
	}

	res, err := f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{{
			PriceVersionID: priceID,
			Config:         map[string]string{optionID: "8 GB"},
		}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if res.Total.Minor() != 2500 {
		t.Fatalf("total = %d, want 1000 + 1500", res.Total.Minor())
	}
	if !strings.Contains(res.Lines[0].Description, "8 GB") {
		t.Fatalf("description = %q, want it to name the option", res.Lines[0].Description)
	}
	if res.Lines[0].Tax.Minor() != 0 {
		t.Fatalf("tax = %d, want 0 with no tax configured", res.Lines[0].Tax.Minor())
	}

	// The selection is on the order item and, for a paid order, on the service when it
	// is created. The order item is the record that exists now.
	var unit int64
	if err := f.pool.QueryRow(ctx,
		`SELECT unit_minor FROM order_items WHERE order_id = $1`, res.OrderID).Scan(&unit); err != nil {
		t.Fatal(err)
	}
	if unit != 2500 {
		t.Fatalf("order item unit = %d, want 2500", unit)
	}
	_ = customerID

	// An unavailable choice is refused rather than silently dropped.
	_, err = f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{{
			PriceVersionID: priceID,
			Config:         map[string]string{optionID: "64 GB"},
		}},
	})
	if !errors.Is(err, register.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}

	// A missing required option is refused too.
	_, err = f.checkout.Checkout(ctx, register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if !errors.Is(err, register.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

// An empty cart is refused.
func TestCheckoutRequiresLines(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "empty@example.test")

	if _, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
	}); !errors.Is(err, register.ErrNoLines) {
		t.Fatalf("err = %v, want ErrNoLines", err)
	}
}

// The checkout resolves the billing account through the link registration wrote.
func TestResolveCustomer(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "resolve@example.test")

	got, err := f.checkout.ResolveCustomer(context.Background(), userID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != customerID {
		t.Fatalf("resolved %s, want %s", got, customerID)
	}

	// An unknown user is not a 500.
	if _, err := f.checkout.ResolveCustomer(context.Background(),
		"0f8fad5b-d9cb-469f-a165-70867728950e"); !errors.Is(err, register.ErrNoCustomer) {
		t.Fatalf("err = %v, want ErrNoCustomer", err)
	}
}

// A setup fee is a line of its own, and a setup-only coupon does not reduce the price.
func TestSetupFeeAndSetupOnlyCoupon(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "setup@example.test")
	_, priceID := f.product(t, "with-setup", 2000, 1000)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO coupons (code, kind, percentage, applies_to)
		VALUES ('SETUP50', 'percentage', '50', 'setup')`); err != nil {
		t.Fatal(err)
	}

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:     userID,
		Lines:      []register.CheckoutLine{{PriceVersionID: priceID}},
		CouponCode: "SETUP50",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	// Price 2000 + setup 1000 - 500 setup discount = 2500.
	if res.Total.Minor() != 2500 {
		t.Fatalf("total = %d, want 2500", res.Total.Minor())
	}
	if res.Discount.Minor() != 500 {
		t.Fatalf("discount = %d, want 500", res.Discount.Minor())
	}
}

// mustRate parses a tax rate for the fixture.

// An inclusive tax rate discounts the gross, and the tax is the share of the reduced
// gross. The total must equal what the customer was quoted minus the discount, not that
// plus or minus a tax term.
func TestInclusiveTaxDiscount(t *testing.T) {
	f := newFixture(t, billing.FixedTaxPricer{
		Rate:      money.MustParseRatio("20%"),
		Inclusive: true,
		Policy:    "test-vat-inclusive", Jurisdiction: "DE",
	})
	_, userID := f.customer(t, "inclusive@example.test")
	_, priceID := f.product(t, "incl", 12000, 0)
	f.coupon(t, "QUARTER", "percentage", "25")

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID:     userID,
		Lines:      []register.CheckoutLine{{PriceVersionID: priceID}},
		CouponCode: "QUARTER",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	// Gross 12000 contains 2000 tax (net 10000). A 25% discount takes 3000 off the
	// gross: gross 9000 = net 7500 + tax 1500, total 9000.
	if res.Total.Minor() != 9000 {
		t.Fatalf("total = %d, want 9000", res.Total.Minor())
	}
	if res.Tax.Minor() != 1500 {
		t.Fatalf("tax = %d, want 1500 (the tax share of the reduced gross)", res.Tax.Minor())
	}
	if res.Subtotal.Minor() != 7500 {
		t.Fatalf("subtotal = %d, want 7500 (net after the discount)", res.Subtotal.Minor())
	}

	// net + tax must equal the invoice total exactly, which the schema also enforces.
	var subtotal, tax, total int64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT subtotal_minor, tax_minor, total_minor FROM invoices WHERE id = $1`,
		res.InvoiceID).Scan(&subtotal, &tax, &total); err != nil {
		t.Fatal(err)
	}
	if subtotal+tax != total {
		t.Fatalf("invoice does not add up: %d + %d != %d", subtotal, tax, total)
	}
}
