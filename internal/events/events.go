// Package events is the in-process fan-out behind the server-sent events endpoint.
//
// The contract it implements is deliberately narrow, because a "real-time" feed is
// easy to get subtly wrong:
//
//   - Only committed state is published. Publishers call Publish *after* their
//     transaction returns; publishing inside a transaction would tell a client about a
//     change that can still roll back.
//   - Every event carries a monotonic id, so a client can tell a gap from a delay.
//   - There is no replay. A reconnecting client presents Last-Event-ID and receives
//     everything newer than it; nothing older is re-sent. Replay would need a durable
//     log, and the durable log is the database the client can already query.
//   - A slow subscriber is disconnected rather than silently losing events. Being told
//     to reconnect and re-read is honest; missing an event and not knowing is not.
//
// The broker is process-local. With more than one instance a client is connected to
// one of them, so an event published on instance A is not seen by a client on instance
// B. That limitation is documented in docs/ui-gate.md rather than hidden: making it
// correct needs a durable log and a cursor per client, which is a different design.
package events

import (
	"sync"
	"time"
)

// Event is one published change.
type Event struct {
	// ID is assigned by the broker. It is monotonic within one process.
	ID int64
	// Kind is the event name a client subscribes to, e.g. "invoice.paid".
	Kind string
	// CustomerID scopes a customer-visible event to its owner. Empty means the event
	// is staff-only.
	CustomerID string
	// Permission is the staff permission required to see the event. Empty means any
	// staff member may see it (still staff-only when CustomerID is empty).
	Permission string
	// Data is the JSON payload.
	//
	// The map is shared, not copied: Publish hands the same reference to every subscriber,
	// and each subscriber renders it on its own goroutine. Neither side may write to it
	// after Publish returns — a write there races the other subscribers' encoding, and the
	// runtime kills the process rather than recovering. A reader that needs to decorate the
	// payload must copy it first.
	Data map[string]any
	// At is when the change happened.
	At time.Time
}

// Subscription is one client's stream.
type Subscription struct {
	broker *Broker
	id     uint64
	ch     chan Event

	mu     sync.Mutex
	closed bool
}

// Events returns the receive channel. It is closed when the broker drops the
// subscriber.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close unsubscribes. It is safe to call more than once and concurrently with
// Publish.
func (s *Subscription) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	s.broker.remove(s.id)
}

// send delivers one event. It reports false when the subscriber is gone or its buffer
// is full, which makes the caller drop it.
//
// The send and the close are serialised by the same mutex: sending on a closed channel
// panics, and a publish racing a disconnect is the normal case, not an exotic one.
func (s *Subscription) send(ev Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

// Broker fans events out to subscribers.
type Broker struct {
	mu      sync.Mutex
	nextID  int64
	nextSub uint64
	subs    map[uint64]*Subscription
	// buffer is the per-subscriber queue depth.
	buffer int
}

// NewBroker constructs a Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[uint64]*Subscription), buffer: 64}
}

// Publish assigns an id and delivers to every subscriber.
//
// A subscriber whose buffer is full is disconnected: the alternative is blocking the
// request that publishes, which would let one stalled browser stop a payment from
// being recorded.
func (b *Broker) Publish(ev Event) Event {
	b.mu.Lock()
	b.nextID++
	ev.ID = b.nextID
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	targets := make([]*Subscription, 0, len(b.subs))
	for _, sub := range b.subs {
		targets = append(targets, sub)
	}
	b.mu.Unlock()

	for _, sub := range targets {
		if !sub.send(ev) {
			// The subscriber is gone or too slow. Disconnecting is honest: it reconnects
			// and re-reads state. Silently dropping the event would leave the operator
			// looking at stale state with no signal that it is stale.
			sub.Close()
		}
	}
	return ev
}

// Subscribe registers a subscriber. buffer <= 0 uses the broker default.
func (b *Broker) Subscribe(buffer int) *Subscription {
	if buffer <= 0 {
		buffer = b.buffer
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSub++
	sub := &Subscription{broker: b, id: b.nextSub, ch: make(chan Event, buffer)}
	b.subs[sub.id] = sub
	return sub
}

// LastID returns the most recently assigned id.
func (b *Broker) LastID() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID
}

// Subscribers reports how many clients are connected, for diagnostics.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *Broker) remove(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}
