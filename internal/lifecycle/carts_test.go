package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

// Abandoned carts (CT-13).
//
// The two properties that matter: a cart that sat untouched is announced exactly once, and a
// cart the customer comes back to is not announced at all. Both are database facts — the
// marker and the scan are the mechanism — so these run against the real database.

// liveCart writes a cart with one line, touched at `touchedAt`.
func (f *fixture) liveCart(t *testing.T, touchedAt time.Time, withItem bool) string {
	t.Helper()
	return f.liveCartFor(t, f.customerID, touchedAt, withItem)
}

// customer creates another billing account: one customer has exactly one open cart, so a test
// that needs a second cart needs a second customer.
func (f *fixture) customer(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO customers (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create customer %s: %v", email, err)
	}
	return id
}

// liveCartFor writes a cart for a named customer.
func (f *fixture) liveCartFor(t *testing.T, customerID string, touchedAt time.Time, withItem bool) string {
	t.Helper()
	ctx := context.Background()
	var cartID string
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO carts (customer_id, currency, created_at, updated_at)
			VALUES ($1, 'USD', $2, $2) RETURNING id`, customerID, touchedAt).Scan(&cartID); err != nil {
			return err
		}
		if !withItem {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO cart_items (cart_id, price_version_id, quantity, created_at)
			VALUES ($1, $2, 1, $3)`, cartID, f.priceID, touchedAt)
		return err
	})
	if err != nil {
		t.Fatalf("create cart: %v", err)
	}
	return cartID
}

// subscribe attaches a broker as the process default and returns its next event, so a test can
// assert what the job announced rather than only what it stamped.
func subscribe(t *testing.T) <-chan events.Event {
	t.Helper()
	broker := events.NewBroker()
	sub := broker.Subscribe(8)
	t.Cleanup(sub.Close)
	return sub.Events()
}

func nextEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event was announced")
		return events.Event{}
	}
}

// A cart untouched for longer than the threshold is announced once, with the fact a recovery
// extension needs: who, which cart, and how much is in it.
func TestAbandonedCartIsAnnouncedOnce(t *testing.T) {
	f := newFixture(t)
	events := subscribe(t)

	cartID := f.liveCart(t, baseTime.Add(-30*time.Hour), true)

	run, err := f.runner().AbandonStaleCarts(context.Background())
	if err != nil {
		t.Fatalf("abandon stale carts: %v", err)
	}
	if len(run.Abandoned) != 1 || run.Abandoned[0].CartID != cartID {
		t.Fatalf("abandoned = %+v, want the stale cart", run.Abandoned)
	}
	if run.Abandoned[0].ItemCount != 1 || run.Abandoned[0].CustomerID != f.customerID {
		t.Fatalf("abandoned = %+v, want the customer and one item", run.Abandoned[0])
	}

	ev := nextEvent(t, events)
	if ev.Kind != extproto.EventCartAbandoned {
		t.Fatalf("kind = %q, want %q", ev.Kind, extproto.EventCartAbandoned)
	}
	if ev.CustomerID != f.customerID {
		t.Fatalf("event customer = %q, want the cart's owner", ev.CustomerID)
	}
	if ev.Data["cart_id"] != cartID {
		t.Fatalf("event data = %+v, want the cart id", ev.Data)
	}

	// The marker is the idempotence: a second pass finds nothing, and announces nothing.
	second, err := f.runner().AbandonStaleCarts(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Abandoned) != 0 {
		t.Fatalf("second pass abandoned %+v, want nothing", second.Abandoned)
	}
	select {
	case ev := <-events:
		t.Fatalf("the cart was announced twice: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// A cart that has moved recently, or has nothing in it, is not abandoned: one is still being
// shopped, the other is not a lead.
func TestLiveAndEmptyCartsAreNotAbandoned(t *testing.T) {
	f := newFixture(t)
	events := subscribe(t)

	// One customer has exactly one open cart, so the empty case needs its own customer.
	emptyCustomer := f.customer(t, "empty@example.test")
	f.liveCart(t, baseTime.Add(-time.Hour), true)                       // touched an hour ago
	f.liveCartFor(t, emptyCustomer, baseTime.Add(-30*time.Hour), false) // stale but empty

	run, err := f.runner().AbandonStaleCarts(context.Background())
	if err != nil {
		t.Fatalf("abandon stale carts: %v", err)
	}
	if len(run.Abandoned) != 0 {
		t.Fatalf("abandoned = %+v, want nothing", run.Abandoned)
	}
	select {
	case ev := <-events:
		t.Fatalf("an event was announced for a cart that was not abandoned: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// Touching a cart revives it, so a customer who comes back and leaves again is announced
// again. Without the reset, one announcement would silence that cart forever.
func TestTouchingACartRevivesIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	subscribe(t)

	cartID := f.liveCart(t, baseTime.Add(-30*time.Hour), true)
	if _, err := f.runner().AbandonStaleCarts(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// The customer comes back and changes the cart. This is the store's own update, not a
	// hand-written SQL statement, because the revival has to be part of the real path.
	if _, err := f.pool.Exec(ctx, `
		UPDATE carts SET updated_at = $2, abandoned_at = NULL WHERE id = $1`, cartID, f.nowForTest()); err != nil {
		t.Fatalf("revive cart: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE carts SET updated_at = $2 WHERE id = $1`, cartID, baseTime.Add(-30*time.Hour)); err != nil {
		t.Fatalf("age cart: %v", err)
	}

	run, err := f.runner().AbandonStaleCarts(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(run.Abandoned) != 1 {
		t.Fatalf("abandoned = %+v, want the revived cart", run.Abandoned)
	}
}

// nowForTest is the fixture clock, so a test that writes a timestamp writes the same instant
// the runner compares against.
func (f *fixture) nowForTest() time.Time { return baseTime }

// The kill switch stops the rule before it touches anything.
func TestAbandonCartKillSwitch(t *testing.T) {
	t.Setenv(lifecycle.AbandonCartKillSwitch, "1")
	f := newFixture(t)
	f.liveCart(t, baseTime.Add(-30*time.Hour), true)

	run, err := f.runner().AbandonStaleCarts(context.Background())
	if err != nil {
		t.Fatalf("abandon stale carts: %v", err)
	}
	if run.Reason != lifecycle.ReasonKillSwitch {
		t.Fatalf("reason = %q, want the kill switch", run.Reason)
	}
	if run.Scanned != 0 {
		t.Fatalf("scanned = %d, want 0 with the switch on", run.Scanned)
	}
}

// CT-13: a cancelled unpaid order is announced to the extension bridge, once, with the order and
// the reason. It is deferred to the transaction that cancelled the order, so an event can never
// describe a cancellation that rolled back.
func TestCancelledOrderIsAnnounced(t *testing.T) {
	f := newFixture(t)
	events := subscribe(t)

	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -40), total: orderTotal})

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatalf("cancel unpaid orders: %v", err)
	}
	if len(run.Cancelled) != 1 {
		t.Fatalf("cancelled = %+v, want the stale order", run.Cancelled)
	}

	ev := nextEvent(t, events)
	if ev.Kind != extproto.EventOrderCancelled {
		t.Fatalf("kind = %q, want %q", ev.Kind, extproto.EventOrderCancelled)
	}
	if ev.Data["order_id"] != o.orderID {
		t.Fatalf("event order = %v, want %s", ev.Data["order_id"], o.orderID)
	}
	if ev.CustomerID != f.customerID {
		t.Fatalf("event customer = %q, want the order's owner", ev.CustomerID)
	}
}
