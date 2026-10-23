package extproto

import (
	"fmt"
	"strconv"
	"time"
)

// The live view of a provisioned resource: what it looks like right now, and the power switch.
//
// Everything else in this protocol answers a billing question — provision it, suspend it, what
// does it cost, is it still there. These two answer the question a *customer* asks, which is the
// one the panel could not answer at all before: "what is my server doing, and how do I restart
// it?".
//
// # Why this is not a service action
//
// The extension UI surface (`ui.describe`) can already contribute a button, and a button is
// enough for "reboot this service". It is not enough for this feature, for three reasons that
// are all about the read rather than the write:
//
//   - A hook call is a *mutation*: POST-only, idempotency key required, audited as a change. The
//     page needs this answer on load, before the customer presses anything.
//   - A hook reply carries a message and an opaque data map. A stats table has a shape, and a
//     shape that only appears in a free-form map is a shape the panel cannot render without
//     knowing the provider.
//   - The panel has to decide what to *offer*. Powering on a running server is not an error the
//     customer should discover by pressing the button, so the adapter — which is the only party
//     that knows its own state machine — reports which actions it would accept.
//
// # Why the names are generic
//
// `provisioner.resource` and `provisioner.power` are about a provisioned resource, not about a
// game panel. Pterodactyl, cPanel and a hypervisor all have a running state, an address and a
// stop button; an adapter that has none of that simply does not advertise the capability, which
// is the same compatibility rule every other method here follows.

const (
	// MethodProvisionerResource is the read: the observed state of one resource.
	MethodProvisionerResource = "provisioner.resource"
	// MethodProvisionerPower is the write: start, stop, restart or kill one resource.
	MethodProvisionerPower = "provisioner.power"
)

// Capability names, for extensions that spell them out.
const (
	CapProvisionerResource = MethodProvisionerResource
	CapProvisionerPower    = MethodProvisionerPower
)

// PowerAction is the closed set of things a customer may do to a running resource.
//
// It is closed deliberately. An open string would let an adapter invent "reinstall" or "wipe",
// which is a destructive operation that must be a queue row with its own approval rather than a
// button on a page. The four here are the ones that change nothing but the process.
type PowerAction string

// The actions.
const (
	// PowerStart boots a stopped resource.
	PowerStart PowerAction = "start"
	// PowerStop asks the resource to shut down cleanly. The provider may take a while.
	PowerStop PowerAction = "stop"
	// PowerRestart is stop followed by start, as one request: a customer who wants their server
	// back should not have to press two buttons and wait in between.
	PowerRestart PowerAction = "restart"
	// PowerKill is the ungraceful stop: the provider terminates the process immediately. It is
	// offered because a hung server cannot be stopped by asking it to stop.
	PowerKill PowerAction = "kill"
)

// PowerActions lists every action, in the order a page should offer them.
func PowerActions() []PowerAction {
	return []PowerAction{PowerStart, PowerStop, PowerRestart, PowerKill}
}

// Valid reports whether a is one of the four.
func (a PowerAction) Valid() bool {
	switch a {
	case PowerStart, PowerStop, PowerRestart, PowerKill:
		return true
	}
	return false
}

// ParsePowerAction converts the wire or query form, refusing anything outside the set.
//
// The bool is what a handler branches on: an unknown action is a 400 naming the four that exist,
// never a request forwarded to the provider in the hope that it understands.
func ParsePowerAction(raw string) (PowerAction, bool) {
	a := PowerAction(raw)
	return a, a.Valid()
}

// ResourceStats is what a live resource is doing.
//
// These are measurements, not money, so CPUPercent is a float: the providers report fractional
// percentages (0.4% of a core) and rounding one to an integer would report an idle server as
// either 0% busy or 1% busy with no way to tell which. Money in this protocol is still integer
// minor units and never a float — that rule is about value, and this is not value.
//
// Every field is optional in the sense that an adapter which cannot measure it leaves it zero;
// the panel renders a dash rather than a zero, because "0 MB of memory" and "no memory figure"
// are different statements and only one of them is true.
type ResourceStats struct {
	// CPUPercent is percent of one core, as the provider reports it.
	CPUPercent float64 `json:"cpu_percent,omitempty"`
	// MemoryUsedMB and MemoryLimitMB are what the process holds and what it may hold.
	MemoryUsedMB  int64 `json:"memory_used_mb,omitempty"`
	MemoryLimitMB int64 `json:"memory_limit_mb,omitempty"`
	// DiskUsedMB and DiskLimitMB are the same pair for storage.
	DiskUsedMB  int64 `json:"disk_used_mb,omitempty"`
	DiskLimitMB int64 `json:"disk_limit_mb,omitempty"`
	// NetworkRxBytes and NetworkTxBytes are cumulative since the resource started, as the
	// provider counts them. They are bytes, not megabytes, because that is what the provider
	// returns and converting here would lose precision the page does not need to lose.
	NetworkRxBytes int64 `json:"network_rx_bytes,omitempty"`
	NetworkTxBytes int64 `json:"network_tx_bytes,omitempty"`
	// ObservedAt is when the provider took the measurement, which may be older than the call:
	// a metrics pipeline reports the last sample it has, and saying "now" would be a lie.
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// ProvisionResourceParams asks for the live state of one resource. It is a pure read.
type ProvisionResourceParams struct {
	CallMeta
	ExternalID string `json:"external_id"`
}

// ProvisionResourceResult is the live view the customer screen renders.
//
// Name and PanelURL are here rather than in the billing database on purpose: the resource's
// display name and the console address are the provider's facts, and a copy kept in the panel
// would be wrong the moment someone renamed the server in the provider's own UI. The panel asks
// every time and shows the answer.
type ProvisionResourceResult struct {
	ExternalID string `json:"external_id"`
	// Name is the resource's own name at the provider.
	Name string `json:"name"`
	// State is the provider's own word — "running", "stopped", "starting", "installing" — passed
	// through rather than translated. A core that renamed it would have to guess, and the
	// customer's support ticket quotes the provider's word.
	State string `json:"state"`
	// Status is the same fact in the protocol's own lifecycle vocabulary, for callers that want
	// to branch without learning a provider's dictionary.
	Status ResourceStatus `json:"status"`
	// Host and Port are what a customer types into their client. Host is the provider's public
	// address for the resource, which is not always the node's address: a bridge network maps a
	// host port, and only the provider knows which.
	Host string `json:"host,omitempty"`
	Port int32  `json:"port,omitempty"`
	// PanelURL is the deep link to the resource in the provider's own control panel. Empty when
	// the provider has no panel a customer can open.
	PanelURL string `json:"panel_url,omitempty"`
	// PlanID and Region echo what the resource was sold as, when the provider can report it.
	PlanID string `json:"plan_id,omitempty"`
	Region string `json:"region,omitempty"`
	// Stats is nil when the provider cannot measure the resource (a stopped one usually cannot).
	Stats *ResourceStats `json:"stats,omitempty"`
	// CanPower is the set of actions this adapter would accept *now*. It is computed by the
	// adapter because it is the only party that knows its own state machine; a panel that
	// guessed would offer "start" on a running server and turn a disabled button into a 409.
	CanPower []PowerAction `json:"can_power,omitempty"`
	// ObservedAt is when this answer was taken.
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// Address renders "host:port", or empty when the provider reports no address.
//
// It lives here rather than in the web client so that every caller — a page, a ticket macro, an
// API consumer — prints the address the same way, and so a provider that reports one or the other
// is handled in one place.
func (r ProvisionResourceResult) Address() string {
	if r.Host == "" {
		return ""
	}
	if r.Port <= 0 {
		return r.Host
	}
	return r.Host + ":" + strconv.Itoa(int(r.Port))
}

// ProvisionPowerParams asks an adapter to change the power state of one resource.
//
// It is mutating and idempotent by operation key: a customer who double-taps "start" must not
// boot twice, and a retried request after a lost reply must not restart a server that is already
// running. `Changed` in the result is what distinguishes "did it" from "it already was".
type ProvisionPowerParams struct {
	MutatingMeta
	ExternalID string      `json:"external_id"`
	Action     PowerAction `json:"action"`
}

// ProvisionPowerResult reports the state the resource is in after the call.
type ProvisionPowerResult struct {
	ExternalID string      `json:"external_id"`
	Action     PowerAction `json:"action"`
	// State is the provider's own word after the call.
	State string `json:"state"`
	// Status is the lifecycle translation, as in ProvisionResourceResult.
	Status ResourceStatus `json:"status"`
	// Changed is false when the resource was already in the requested state, which is what a
	// replay of a completed action reports.
	Changed bool `json:"changed"`
}

// Validate refuses a power request the protocol cannot express.
//
// It is checked by the core before the call as well as by the adapter after it. Both are
// deliberate: the core check turns a typo into a 400 instead of a round trip, and the adapter
// check is what protects an adapter driven by something that is not this core.
func (p ProvisionPowerParams) Validate() error {
	if p.ExternalID == "" {
		return fmt.Errorf("protocol: external_id is required")
	}
	if !p.Action.Valid() {
		return fmt.Errorf("protocol: unknown power action %q", string(p.Action))
	}
	return ValidateOperationKey(p.OperationKey)
}
