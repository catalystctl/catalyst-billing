package events_test

import (
	"testing"
	"time"

	"github.com/catalyst/billing/internal/events"
)

func TestPublishAssignsMonotonicIDs(t *testing.T) {
	b := events.NewBroker()
	a := b.Publish(events.Event{Kind: "invoice.paid"})
	c := b.Publish(events.Event{Kind: "invoice.void"})
	if a.ID != 1 || c.ID != 2 {
		t.Fatalf("ids = %d, %d, want 1, 2", a.ID, c.ID)
	}
	if a.At.IsZero() {
		t.Fatal("a published event has no timestamp")
	}
	if b.LastID() != 2 {
		t.Fatalf("LastID = %d, want 2", b.LastID())
	}
}

func TestSubscriberReceivesEvents(t *testing.T) {
	b := events.NewBroker()
	sub := b.Subscribe(4)
	defer sub.Close()

	b.Publish(events.Event{Kind: "service.updated", Data: map[string]any{"id": "s1"}})
	select {
	case ev := <-sub.Events():
		if ev.Kind != "service.updated" {
			t.Fatalf("kind = %q", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no event was delivered")
	}
}

// Closing a subscription twice must be safe, and publishing afterwards must not panic
// by sending on a closed channel.
func TestCloseIsIdempotentAndPublishAfterCloseIsSafe(t *testing.T) {
	b := events.NewBroker()
	sub := b.Subscribe(1)
	sub.Close()
	sub.Close()

	b.Publish(events.Event{Kind: "invoice.paid"})
	if n := b.Subscribers(); n != 0 {
		t.Fatalf("subscribers after close = %d, want 0", n)
	}
}

// A subscriber that stops reading is disconnected instead of blocking the publisher.
// One stalled browser must not be able to stop a payment from being recorded.
func TestSlowSubscriberIsDropped(t *testing.T) {
	b := events.NewBroker()
	sub := b.Subscribe(1)
	defer sub.Close()

	// Fill the buffer, then publish once more.
	b.Publish(events.Event{Kind: "first"})
	done := make(chan struct{})
	go func() {
		b.Publish(events.Event{Kind: "second"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}
	if n := b.Subscribers(); n != 0 {
		t.Fatalf("subscribers = %d, want the slow one dropped", n)
	}
}

// Two subscribers both receive the same event: the feed is a broadcast, not a queue.
func TestBroadcast(t *testing.T) {
	b := events.NewBroker()
	one := b.Subscribe(2)
	two := b.Subscribe(2)
	defer one.Close()
	defer two.Close()

	b.Publish(events.Event{Kind: "invoice.paid"})
	for i, sub := range []*events.Subscription{one, two} {
		select {
		case <-sub.Events():
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}
