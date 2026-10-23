package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/events"
)

// An event deferred inside a transaction is announced only when the caller flushes.
//
// This is the property CT-13 depends on: `order.paid` and `order.cancelled` are recorded from
// inside somebody else's transaction, and a rollback must announce nothing. The test is the
// two halves of that sentence — buffered but not published, then published once flushed.
func TestDeferredEventIsAnnouncedOnlyOnFlush(t *testing.T) {
	b := events.NewBroker()
	sub := b.Subscribe(4)
	defer sub.Close()

	ctx, flush := events.WithPending(context.Background(), b)
	events.Defer(ctx, events.Event{Kind: "order.paid", CustomerID: "c1"})
	if n := events.PendingLen(ctx); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}

	select {
	case ev := <-sub.Events():
		t.Fatalf("an event was announced before the commit: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	flush()
	select {
	case ev := <-sub.Events():
		if ev.Kind != "order.paid" || ev.CustomerID != "c1" {
			t.Fatalf("event = %+v, want order.paid for c1", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("the deferred event was never announced")
	}
}

// Never flushing is the rollback path, and it must announce nothing. A second flush must not
// announce a second copy.
func TestUnflushedAndDoubleFlushedEvents(t *testing.T) {
	b := events.NewBroker()
	sub := b.Subscribe(4)
	defer sub.Close()

	ctx, _ := events.WithPending(context.Background(), b)
	events.Defer(ctx, events.Event{Kind: "order.cancelled"})
	select {
	case ev := <-sub.Events():
		t.Fatalf("an event was announced without a flush: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	ctx2, flush := events.WithPending(context.Background(), b)
	events.Defer(ctx2, events.Event{Kind: "order.created"})
	flush()
	flush()
	<-sub.Events()
	select {
	case ev := <-sub.Events():
		t.Fatalf("the event was announced twice: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// A package that calls Defer with no buffer attached — a test, a CLI command — must not fail.
// The alternative would make an observability feature into a way to fail a payment.
func TestDeferWithoutABufferIsANoOp(t *testing.T) {
	events.Defer(context.Background(), events.Event{Kind: "order.paid"})
	if n := events.PendingLen(context.Background()); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}
