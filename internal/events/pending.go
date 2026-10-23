package events

// Events that belong to a transaction (CT-13).
//
// The contract at the top of events.go says a publisher calls Publish *after* its transaction
// returns, because announcing a change that can still roll back is a lie a subscriber acts on.
// Every HTTP handler can honour that literally: it opens the transaction, and publishes on the
// next line after it.
//
// A library cannot. `fulfilment.Applier.ApplySettledPayment` is handed a transaction somebody
// else will commit, and `lifecycle.cancelOrder` commits inside a function whose result the job
// still has to act on. Neither can publish on return without risking an event for work that
// rolled back a moment later.
//
// The resolution is a small buffer tied to the transaction rather than a second broker:
//
//	tx := pool.InTx(...)  {  ...  events.Defer(ctx, ev)  ...  }   // buffered
//	events.Flush(ctx)                                            // announced, once committed
//
// Defer only records. Flush is a no-op unless the caller reaches it, which is why the callers
// that own their transaction call it after the commit succeeds and never in a deferred
// function: a flush in a `defer` would announce the events of a transaction that failed.
//
// The buffer is attached to the context, not to a global keyed by transaction, so two
// concurrent settlements cannot drain each other's events. A context that nobody flushes — a
// caller that did not adopt this — simply drops them, which is the same outcome as a
// deployment with no extensions attached.

import "context"

// txEventsKey is the context key holding the pending buffer. It is unexported and of a private
// type so no other package can collide with it.
type txEventsKey struct{}

// pending is one transaction's unannounced events.
type pending struct {
	broker *Broker
	events []Event
}

// WithPending returns a context that collects deferred events, and a function that announces
// them.
//
// The returned context must be the one the transaction body runs with, and the returned
// function must be called only after the commit succeeded:
//
//	ctx, flush := events.WithPending(ctx, broker)
//	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { ... events.Defer(ctx, ev) ... })
//	if err == nil {
//	    flush()
//	}
func WithPending(ctx context.Context, b *Broker) (context.Context, func()) {
	if b == nil {
		return ctx, func() {}
	}
	p := &pending{broker: b}
	return context.WithValue(ctx, txEventsKey{}, p), func() {
		// Drained exactly once, so a double flush cannot announce twice.
		pending := p.events
		p.events = nil
		for _, ev := range pending {
			b.Publish(ev)
		}
	}
}

// Defer records an event to be announced when the transaction commits.
//
// It is deliberately silent when no buffer is attached: the same library function is called
// from the webhook processor (which runs under its own transaction and adopts this), from a
// test that has no broker, and from the manual-payment path. Failing here would make an
// observability feature into a way to fail a payment.
func Defer(ctx context.Context, ev Event) {
	p, ok := ctx.Value(txEventsKey{}).(*pending)
	if !ok || p == nil {
		return
	}
	p.events = append(p.events, ev)
}

// PendingLen reports how many events are buffered on this context. It exists for tests: "the
// event was recorded but not yet announced" is otherwise unobservable.
func PendingLen(ctx context.Context) int {
	p, ok := ctx.Value(txEventsKey{}).(*pending)
	if !ok || p == nil {
		return 0
	}
	return len(p.events)
}
