package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/portal"
)

// Cart events for extensions (CT-13).
//
// The property under test is not "a handler returns 200" — other tests cover that — it is that
// the change reaches the event fan-out the extension bridge subscribes to, with the kind an
// extension declared and the customer id that scopes it. Each assertion fails if the publish
// call is deleted, which is the whole reason the kinds existed unused.

// nextEvent reads one event, or fails.
func nextEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event reached the broker")
		return events.Event{}
	}
}

// noEvent fails if an event arrives, which is how "this was not announced" is asserted.
func noEvent(t *testing.T, ch <-chan events.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("an event was announced that should not have been: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

// Adding the first line creates the cart; a second change updates it; clearing it updates it
// again. An extension that runs cart recovery has to tell those apart.
func TestCartEventsReachTheBroker(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	// A broker this test owns, so the assertion is about what the server published rather than
	// about the transport that would normally carry it.
	broker := events.NewBroker()
	env.srv.WithEvents(broker)
	sub := broker.Subscribe(16)
	defer sub.Close()

	env.seedCommerceCustomer(t, "cart-events@example.test")
	cookie, csrf := env.login(t, "cart-events@example.test", "customer password 1")
	_, priceID := env.seedCartPrice(t, "cart-events-vps", 1000, 0, "combined")

	rec := env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/items", cookie, csrf,
		map[string]any{"price_version_id": priceID, "quantity": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add item: %d %s", rec.Code, rec.Body)
	}
	ev := nextEvent(t, sub.Events())
	if ev.Kind != extproto.EventCartCreated {
		t.Fatalf("first add published %q, want %q", ev.Kind, extproto.EventCartCreated)
	}
	if ev.CustomerID == "" {
		t.Fatal("the event carries no customer id, so it would reach nobody's browser")
	}
	if ev.Data["cart_id"] == nil || ev.Data["item_count"] == nil {
		t.Fatalf("event data = %+v, want the cart and its size", ev.Data)
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/portal/cart/items", cookie, csrf,
		map[string]any{"price_version_id": priceID, "quantity": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("second add: %d %s", rec.Code, rec.Body)
	}
	if ev = nextEvent(t, sub.Events()); ev.Kind != extproto.EventCartUpdated {
		t.Fatalf("second add published %q, want %q", ev.Kind, extproto.EventCartUpdated)
	}

	rec = env.doJSON(t, http.MethodDelete, "/api/v1/portal/cart", cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear cart: %d %s", rec.Code, rec.Body)
	}
	if ev = nextEvent(t, sub.Events()); ev.Kind != extproto.EventCartUpdated {
		t.Fatalf("clear published %q, want %q", ev.Kind, extproto.EventCartUpdated)
	}
}

// A read is not a change: the cart screen must not emit an event every time it is opened, or
// the feed becomes noise an extension has to filter.
func TestReadingTheCartPublishesNothing(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	broker := events.NewBroker()
	env.srv.WithEvents(broker)
	sub := broker.Subscribe(16)
	defer sub.Close()

	env.seedCommerceCustomer(t, "cart-read@example.test")
	cookie, _ := env.login(t, "cart-read@example.test", "customer password 1")

	if rec := env.get(t, "/api/v1/portal/cart", cookie); rec.Code != http.StatusOK {
		t.Fatalf("get cart: %d %s", rec.Code, rec.Body)
	}
	noEvent(t, sub.Events())
}
