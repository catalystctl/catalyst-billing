package extproto

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Protocol version 2: event emission.
//
// Version 1 of this protocol is request/response only: the core calls, the extension
// answers. That is the right shape for a gateway or a provisioner, whose work the core
// initiates, and the wrong shape for an extension that wants to *react* — a fulfilment
// hook that wants to know an order was placed, or a chat extension that wants to know a
// ticket was opened.
//
// Version 2 adds exactly one thing: the core may send the extension notifications about
// committed lifecycle events. It is additive in both directions:
//
//   - The wire format does not change. A notification is the JSON-RPC notification the
//     envelope already supports (no id, no reply), so an extension that ignores it, or
//     a core that never sends one, is still conformant.
//   - Version 1 is still offered and still selected by an extension that only speaks 1.
//     A v1 extension is accepted, works exactly as before, and is never sent an event.
//   - Delivery is one-way. The core never waits for an answer to a notification, so a
//     slow or dead extension cannot slow a request: see Hooks in hooks.go, which owns
//     the bounded queue and the drop counter.
//
// The subscription is explicit rather than assumed. After the handshake the core calls
// events.subscribe with the closed list of kinds it knows how to emit, and the extension
// answers with the ones it wants. An extension that advertises events.subscribe and
// then refuses everything receives nothing; one that does not advertise it is not asked.
// Guessing would be worse: an extension that does not know a kind would have to ignore
// it silently, and silent ignoring is indistinguishable from a delivery bug.

const (
	// VersionTwo is the protocol version that adds event emission. Version (in
	// spec.go) remains the version every extension must speak.
	VersionTwo = 2

	// MethodEventsSubscribe is the call the core makes after the handshake to learn
	// which lifecycle events an extension wants. It is a pure read.
	MethodEventsSubscribe = "events.subscribe"
	// MethodEventDeliver is the notification the core sends for one committed event.
	// It has no reply: delivery is fire-and-forget by design.
	MethodEventDeliver = "events.deliver"

	// CapEventsSubscribe is the capability name an extension advertises to be asked.
	CapEventsSubscribe = MethodEventsSubscribe
)

// SupportedVersions returns the protocol versions this core offers, newest first.
//
// The order matters: the handshake sends them in this order and an extension that
// simply picks the first one it understands gets the newest behaviour.
func SupportedVersions() []int { return []int{VersionTwo, Version} }

// SupportsVersion reports whether the core offered v.
func SupportsVersion(v int) bool {
	for _, offered := range SupportedVersions() {
		if v == offered {
			return true
		}
	}
	return false
}

// Lifecycle event kinds the core can emit to a subscribed extension.
//
// The list is closed on purpose. An extension subscribes by kind, and a kind the core
// cannot emit is a promise nobody keeps; a kind that is not on this list is refused at
// subscription time rather than silently never delivered.
//
// The cart kinds are declared here ahead of the cart publishing them: the storefront's
// cart is the next thing to publish, and an extension author should be able to write
// the subscription today. A kind that the core does not yet publish is simply never
// delivered, which is visible in the extension's own logs and in nothing else.
const (
	EventCartCreated   = "cart.created"
	EventCartUpdated   = "cart.updated"
	EventCartAbandoned = "cart.abandoned"
	EventCartChecked   = "cart.checked_out"

	EventOrderCreated   = "order.created"
	EventOrderPaid      = "order.paid"
	EventOrderCancelled = "order.cancelled"

	EventTicketCreated  = "ticket.created"
	EventTicketReplied  = "ticket.replied"
	EventTicketClosed   = "ticket.closed"
	EventTicketAssigned = "ticket.assigned"
	EventTicketUpdated  = "ticket.updated"
)

// LifecycleKinds is the closed set of kinds offered in events.subscribe.
var LifecycleKinds = []string{
	EventCartCreated,
	EventCartUpdated,
	EventCartAbandoned,
	EventCartChecked,
	EventOrderCreated,
	EventOrderPaid,
	EventOrderCancelled,
	EventTicketCreated,
	EventTicketReplied,
	EventTicketClosed,
	EventTicketAssigned,
	EventTicketUpdated,
}

// IsLifecycleKind reports whether kind is one the core may deliver.
func IsLifecycleKind(kind string) bool {
	for _, k := range LifecycleKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Event is one committed lifecycle event, as delivered to an extension.
//
// It is the payload of the events.deliver notification. The fields mirror the core's
// in-process broker so that the bridge is a copy rather than a translation: a field
// that exists in one and not the other would be a silent difference between what the
// browser sees and what the extension sees.
type Event struct {
	// Kind is one of LifecycleKinds.
	Kind string `json:"kind"`
	// CustomerID scopes a customer-visible event to its owner. Empty for a
	// staff-only event.
	CustomerID string `json:"customer_id,omitempty"`
	// Permission is the staff permission the event was published under. It is
	// informational for the extension: the core has already decided the event is
	// deliverable.
	Permission string `json:"permission,omitempty"`
	// Data is the event payload. It is the same map the browser's stream carries.
	Data map[string]any `json:"data,omitempty"`
	// At is when the change happened.
	At time.Time `json:"at,omitzero"`
	// ID is the broker's monotonic id, for correlation with the browser's stream.
	ID int64 `json:"id,omitempty"`
}

// EventsSubscribeParams is the subscription offer.
type EventsSubscribeParams struct {
	CallMeta
	// Kinds is every kind the core can emit. The extension answers with the subset
	// it wants.
	Kinds []string `json:"kinds"`
}

// EventsSubscribeResult is the extension's answer.
type EventsSubscribeResult struct {
	// Accepted is the subset of the offered kinds the extension wants delivered.
	// Unknown kinds are refused by the core rather than stored.
	Accepted []string `json:"accepted"`
}

// Validate checks that every accepted kind was offered and is a lifecycle kind.
func (r EventsSubscribeResult) Validate() error {
	var unknown []string
	for _, k := range r.Accepted {
		if !IsLifecycleKind(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%w: subscribed to unknown event kind(s) %s; the core offers %s",
			ErrInvalidParams, strings.Join(unknown, ", "), strings.Join(LifecycleKinds, ", "))
	}
	return nil
}
