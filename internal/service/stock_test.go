package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/service"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// seedProduct inserts a product with the given rules.
func seedProduct(t *testing.T, pool *store.Pool, slug string, stock, perUserLimit *int, allowQuantity string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO products (slug, name, stock, per_user_limit, allow_quantity)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		slug, slug, stock, perUserLimit, allowQuantity).Scan(&id); err != nil {
		t.Fatalf("product %s: %v", slug, err)
	}
	return id
}

func intPtr(n int) *int { return &n }

// Decrementing stock under concurrency never oversells: exactly `stock` of `stock+10`
// concurrent attempts succeed, and the counter lands on zero rather than going negative.
//
// This is the property the checkout path depends on, and it is the reason the guard is in the
// WHERE clause of a single statement rather than a read followed by a write.
func TestStockDecrementsAtomicallyUnderConcurrency(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	const stock = 5
	const attempts = 15
	productID := seedProduct(t, f.pool, "limited-stock", intPtr(stock), nil, "separated")

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		succeeded  int
		outOfStock int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				return service.DecrementStock(ctx, tx, productID, 1)
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, service.ErrOutOfStock):
				outOfStock++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != stock {
		t.Fatalf("%d decrements succeeded, want exactly %d", succeeded, stock)
	}
	if outOfStock != attempts-stock {
		t.Fatalf("%d attempts were refused as out of stock, want %d", outOfStock, attempts-stock)
	}

	var remaining int
	if err := f.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).
		Scan(&remaining); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("stock = %d after %d decrements of a %d-unit product, want 0",
			remaining, stock, stock)
	}
}

// A product with no stock limit stays unlimited: restoring stock must not turn NULL into a
// number, which would silently make an unlimited product finite.
func TestUnlimitedStockStaysUnlimited(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	productID := seedProduct(t, f.pool, "unlimited", nil, nil, "disabled")

	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.DecrementStock(ctx, tx, productID, 3)
	}); err != nil {
		t.Fatalf("decrement unlimited stock: %v", err)
	}
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.RestoreStock(ctx, tx, productID, 2)
	}); err != nil {
		t.Fatalf("restore unlimited stock: %v", err)
	}

	var stock *int
	if err := f.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).
		Scan(&stock); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if stock != nil {
		t.Fatalf("stock = %d, want NULL (unlimited)", *stock)
	}
}

// RestoreStock puts units back, so a cancellation returns what a purchase took.
func TestRestoreStockReturnsUnits(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	productID := seedProduct(t, f.pool, "restorable", intPtr(2), nil, "separated")

	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.DecrementStock(ctx, tx, productID, 2)
	}); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	// Now empty: another decrement is refused.
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.DecrementStock(ctx, tx, productID, 1)
	}); !errors.Is(err, service.ErrOutOfStock) {
		t.Fatalf("decrementing an empty product: %v, want ErrOutOfStock", err)
	}

	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.RestoreStock(ctx, tx, productID, 2)
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	var stock int
	if err := f.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).
		Scan(&stock); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if stock != 2 {
		t.Fatalf("stock = %d after restoring 2, want 2", stock)
	}
}

// A per-user limit refuses the (N+1)th service and allows the Nth.
func TestPerUserLimitRefusesTheNextOne(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	const limit = 3
	productID := seedProduct(t, f.pool, "capped", nil, intPtr(limit), "separated")

	// Seed `limit` live services for this customer.
	for i := 0; i < limit; i++ {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO services (customer_id, product_id, status) VALUES ($1, $2, 'active')`,
			f.customerID, productID); err != nil {
			t.Fatalf("seed service %d: %v", i, err)
		}
	}

	// The (N+1)th is refused.
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 1)
	})
	if !errors.Is(err, service.ErrPerUserLimit) {
		t.Fatalf("the %dth unit: %v, want ErrPerUserLimit", limit+1, err)
	}

	// A different customer is unaffected.
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID+"-other", 1)
	}); err != nil {
		// The id is not a uuid, so the query would fail on the cast; use a real one instead.
		var other string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('other-limit@example.test', 'Other')
			RETURNING id`).Scan(&other); err != nil {
			t.Fatalf("other customer: %v", err)
		}
		if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return service.CheckPerUserLimit(ctx, tx, productID, other, 1)
		}); err != nil {
			t.Fatalf("another customer was refused: %v", err)
		}
	}

	// A quantity of 2 against one remaining slot is refused rather than half-fulfilled.
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 2)
	}); !errors.Is(err, service.ErrPerUserLimit) {
		t.Fatalf("a quantity of 2 against 0 remaining: %v, want ErrPerUserLimit", err)
	}
}

// A terminated service no longer occupies a slot, which is what makes the limit a policy a
// customer can act on rather than a lifetime cap.
func TestPerUserLimitIgnoresTerminatedServices(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	productID := seedProduct(t, f.pool, "recycled", nil, intPtr(1), "separated")

	var serviceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status) VALUES ($1, $2, 'active')
		RETURNING id`, f.customerID, productID).Scan(&serviceID); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 1)
	}); !errors.Is(err, service.ErrPerUserLimit) {
		t.Fatalf("at the limit: %v, want ErrPerUserLimit", err)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE services SET status = 'terminated' WHERE id = $1`,
		serviceID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 1)
	}); err != nil {
		t.Fatalf("after terminating the held service: %v", err)
	}
}

// A subscription that has not yet produced a service still occupies a slot.
//
// This is the rule the checkout path derived independently and the one that won: without it a
// customer can place ten unpaid orders for the same product and take delivery of ten services
// the moment the last payment settles, which is exactly what the limit exists to prevent.
// Counting only services looks correct in a unit test that never places an unpaid order.
func TestPerUserLimitCountsUnpaidSubscriptions(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	const limit = 1
	productID := seedProduct(t, f.pool, "one-per-customer", nil, intPtr(limit), "separated")

	// A price version and a pending subscription: the state an order leaves behind between
	// placement and payment. No service row exists yet, because nothing has been provisioned.
	var priceVersionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 1000, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
			anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'pending', 'monthly', 1, 'UTC', now(), now() + interval '1 month',
		        now() + interval '1 month')`, f.customerID, priceVersionID); err != nil {
		t.Fatalf("pending subscription: %v", err)
	}

	// No service exists, so a rule that counted only services would allow this.
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 1)
	})
	if !errors.Is(err, service.ErrPerUserLimit) {
		t.Fatalf("a pending subscription did not occupy a slot: %v, want ErrPerUserLimit", err)
	}

	// Once it is cancelled the slot is free again, because a cancelled subscription will never
	// produce a service.
	if _, err := f.pool.Exec(ctx, `
		UPDATE subscriptions SET status = 'cancelled' WHERE customer_id = $1`, f.customerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPerUserLimit(ctx, tx, productID, f.customerID, 1)
	}); err != nil {
		t.Fatalf("a cancelled subscription still occupied a slot: %v", err)
	}
}

// CheckPurchaseAllowed enforces all three rules the migration added.
func TestCheckPurchaseAllowedCoversQuantityAndStock(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	// A quantity-disabled product refuses a quantity of 2.
	disabled := seedProduct(t, f.pool, "single-only", nil, nil, "disabled")
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPurchaseAllowed(ctx, tx, disabled, f.customerID, 2)
	})
	if err == nil {
		t.Fatal("a quantity of 2 was accepted for a quantity-disabled product")
	}

	// A stock of 1 refuses a quantity of 2.
	tight := seedProduct(t, f.pool, "one-left", intPtr(1), nil, "combined")
	err = f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPurchaseAllowed(ctx, tx, tight, f.customerID, 2)
	})
	if !errors.Is(err, service.ErrOutOfStock) {
		t.Fatalf("quantity 2 against stock 1: %v, want ErrOutOfStock", err)
	}

	// A combined product with enough stock and no per-user limit allows it.
	fine := seedProduct(t, f.pool, "plenty", intPtr(10), nil, "combined")
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPurchaseAllowed(ctx, tx, fine, f.customerID, 2)
	}); err != nil {
		t.Fatalf("a valid purchase was refused: %v", err)
	}
}

// The per-user limit is checked before stock, so a customer at their limit is told that rather
// than "sold out" — which would send them to support for nothing.
func TestPurchaseAllowedReportsTheLimitBeforeStock(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	productID := seedProduct(t, f.pool, "both-rules", intPtr(0), intPtr(1), "separated")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status) VALUES ($1, $2, 'active')`,
		f.customerID, productID); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.CheckPurchaseAllowed(ctx, tx, productID, f.customerID, 1)
	})
	if !errors.Is(err, service.ErrPerUserLimit) {
		t.Fatalf("error = %v, want ErrPerUserLimit (not out_of_stock)", err)
	}
}

// A non-positive decrement is a programming error, not a silent no-op.
func TestStockHelpersRefuseNonPositiveQuantities(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	productID := seedProduct(t, f.pool, "zero-qty", intPtr(5), nil, "disabled")

	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.DecrementStock(ctx, tx, productID, 0)
	}); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("decrement by 0: %v, want ErrInvalidInput", err)
	}
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return service.RestoreStock(ctx, tx, productID, -1)
	}); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("restore by -1: %v, want ErrInvalidInput", err)
	}
}
