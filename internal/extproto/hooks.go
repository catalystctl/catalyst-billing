package extproto

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Extension-contributed UI, without arbitrary code execution.
//
// The problem: an extension wants a place in the panel — a nav entry, a button on a
// service — and the panel must not become a runtime for third-party code. Serving an
// extension's JavaScript would hand it the operator's session, the panel's DOM and every
// credential the browser can reach. Serving nothing means the extension cannot be seen
// at all.
//
// The surface that resolves this is exactly two things:
//
//	ui.describe   the extension declares, as data, what it would like the panel to show:
//	              navigation items (label, path, icon name, required permission) and
//	              service actions (label, the method to call, a confirmation prompt,
//	              required permission).
//	<method>      the panel calls back *one* method the extension advertised in its
//	              handshake, with the service id, when the operator confirms.
//
// Nothing else. The panel renders a label, an icon *name* from its own set, and a link
// to a path it already routes; the extension never supplies markup, never supplies a
// URL to load, and never runs in the browser. A declared permission that is not in the
// core's own permission table is refused at registration, so a hook cannot invent a
// capability: the worst an extension can do is ask for a permission the operator does
// not hold, and be refused.
//
// Everything a declaration may contain is validated here rather than trusted: ids,
// labels, icon names, paths and permissions are bounded and character-restricted, and a
// service action's method must be one the extension advertised as a capability. A
// declaration that fails validation is refused as a whole — a half-installed hook set is
// worse than none, because the operator cannot tell which half is live.

const (
	// MethodUIDescribe is the call that asks an extension what it would like the panel
	// to show. It is a pure read and may be repeated.
	MethodUIDescribe = "ui.describe"
	// CapUIDescribe is the capability name an extension advertises to be asked.
	CapUIDescribe = MethodUIDescribe

	// MaxNavItems and MaxServiceActions bound one extension's declarations. The limits
	// are generous for a real extension and small enough that a hostile one cannot turn
	// a page load into a thousand-row render.
	MaxNavItems       = 16
	MaxServiceActions = 16

	maxHookID      = 64
	maxHookLabel   = 80
	maxHookPath    = 200
	maxHookIcon    = 40
	maxHookMethod  = 120
	maxHookConfirm = 200

	// DefaultEventQueue is how many undelivered events one extension may accumulate
	// before the core starts dropping them. It is deliberately small: a queue that can
	// grow is a queue that hides a dead extension behind memory.
	DefaultEventQueue = 32
)

// ErrHookNotFound means no installed extension declares that hook.
var ErrHookNotFound = errors.New("extproto: no such hook")

var (
	hookIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	hookIconPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// NavItem is one client navigation entry an extension contributes.
//
// It is data, not code. The client renders Label next to Icon and links to Path, which
// must be a path the panel already serves; the client shows it only to a caller holding
// Permission.
type NavItem struct {
	// ID is stable within the extension and is what the client keys on.
	ID string `json:"id"`
	// Label is the text shown. It is plain text: no markup is rendered.
	Label string `json:"label"`
	// Path is an absolute in-panel path, e.g. "/staff/extensions/example-hooks".
	Path string `json:"path"`
	// Icon is the name of an icon in the panel's own set, not a URL and not SVG.
	Icon string `json:"icon"`
	// Permission is the capability required to see and use the item. It must be a
	// permission the core already declares.
	Permission string `json:"permission"`
}

// ServiceAction is one action an extension contributes to a service.
//
// Pressing it asks the operator to confirm Confirm, then the panel calls Method with the
// service id. Method must be a capability the extension advertised in its handshake, and
// it is called as a mutating method: it carries an operation key, so a retried click
// returns the first result instead of acting twice.
type ServiceAction struct {
	// ID is stable within the extension and is what the client sends back.
	ID string `json:"id"`
	// Label is the text on the button.
	Label string `json:"label"`
	// Method is the extension method the panel calls. It must be advertised as a
	// capability and must not be one of the core's own protocol methods.
	Method string `json:"method"`
	// Confirm is the question shown before the call, e.g. "Reboot this service?".
	// Empty means the action is not destructive and needs no confirmation.
	Confirm string `json:"confirm,omitempty"`
	// Permission is the capability required to press it.
	Permission string `json:"permission"`
}

// UIDescribeParams is the payload of ui.describe. It carries no filters: the core asks
// once, after the handshake, and stores the answer.
type UIDescribeParams struct {
	CallMeta
}

// UIDescribeResult is what an extension would like the panel to show.
//
// The first two fields are the original surface (a menu entry and a service button). The rest
// are the operator surface GRW-05 was missing — an admin resource, a page, and the inputs a
// checkout asks for — declared in surface.go with the same rules.
type UIDescribeResult struct {
	NavItems       []NavItem       `json:"nav_items"`
	ServiceActions []ServiceAction `json:"service_actions"`
	AdminResources []AdminResource `json:"admin_resources"`
	Pages          []Page          `json:"pages"`
	// CheckoutFields is accepted as `checkout_config.fields` on the wire and as a bare
	// `checkout_fields` list, because both readings of "checkout config" are natural to an
	// extension author and refusing one would be a puzzle rather than a rule.
	CheckoutFields []CheckoutField `json:"checkout_fields"`
}

// Validate refuses a declaration the panel cannot render safely.
//
// advertised reports whether the extension listed a method as a capability in its
// handshake; known reports whether a permission is one the core declares. Both are
// callbacks rather than imports so this package stays independent of the permission
// table, which is the core's business and not the protocol's.
func (r UIDescribeResult) Validate(advertised func(method string) bool, known func(permission string) bool) error {
	if len(r.NavItems) > MaxNavItems {
		return fmt.Errorf("%w: %d navigation items, the limit is %d", ErrInvalidParams, len(r.NavItems), MaxNavItems)
	}
	if len(r.ServiceActions) > MaxServiceActions {
		return fmt.Errorf("%w: %d service actions, the limit is %d", ErrInvalidParams, len(r.ServiceActions), MaxServiceActions)
	}

	seen := make(map[string]struct{}, len(r.NavItems)+len(r.ServiceActions))
	for i, item := range r.NavItems {
		if err := item.validate(known); err != nil {
			return fmt.Errorf("%w: nav_items[%d]: %w", ErrInvalidParams, i, err)
		}
		// One id space per extension: a nav item and a service action sharing an id
		// would make the client's keying ambiguous, and the ambiguity would show up as
		// the wrong action firing.
		key := "nav:" + item.ID
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: nav_items[%d]: duplicate id %q", ErrInvalidParams, i, item.ID)
		}
		seen[key] = struct{}{}
	}
	for i, action := range r.ServiceActions {
		if err := action.validate(advertised, known); err != nil {
			return fmt.Errorf("%w: service_actions[%d]: %w", ErrInvalidParams, i, err)
		}
		key := "action:" + action.ID
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: service_actions[%d]: duplicate id %q", ErrInvalidParams, i, action.ID)
		}
		seen[key] = struct{}{}
	}
	// The operator surfaces are validated by the same call, so a declaration that is valid in
	// one half and broken in the other is refused as a whole: a half-installed hook set is
	// worse than none, because the operator cannot tell which half is live.
	if _, err := r.ValidateSurface(advertised, known); err != nil {
		return err
	}
	return nil
}

func (n NavItem) validate(known func(string) bool) error {
	if err := validateHookID(n.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if err := validateHookText("label", n.Label, maxHookLabel); err != nil {
		return err
	}
	if err := validateHookPath(n.Path); err != nil {
		return err
	}
	if n.Icon == "" {
		return fmt.Errorf("%w: icon is required", ErrInvalidParams)
	}
	if len(n.Icon) > maxHookIcon || !hookIconPattern.MatchString(n.Icon) {
		return fmt.Errorf("%w: icon %q must be a lower-case icon name (a-z, 0-9, -), not a URL or markup",
			ErrInvalidParams, n.Icon)
	}
	return validateHookPermission(n.Permission, known)
}

func (a ServiceAction) validate(advertised func(string) bool, known func(string) bool) error {
	if err := validateHookID(a.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if err := validateHookText("label", a.Label, maxHookLabel); err != nil {
		return err
	}
	if a.Confirm != "" {
		if err := validateHookText("confirm", a.Confirm, maxHookConfirm); err != nil {
			return err
		}
	}
	if a.Method == "" {
		return fmt.Errorf("%w: method is required", ErrInvalidParams)
	}
	if len(a.Method) > maxHookMethod {
		return fmt.Errorf("%w: method longer than %d bytes", ErrInvalidParams, maxHookMethod)
	}
	if isCoreMethod(a.Method) {
		return fmt.Errorf("%w: method %q is a core protocol method and cannot be called as a hook",
			ErrInvalidParams, a.Method)
	}
	if advertised == nil || !advertised(a.Method) {
		// This is the rule that keeps the surface at "call one method you told us you
		// have". Without it a hook could ask the core to call something the extension
		// never claimed to implement, and the failure would be a method-not-found at the
		// worst possible moment: after the operator confirmed.
		return fmt.Errorf("%w: method %q was not advertised as a capability in the handshake",
			ErrInvalidParams, a.Method)
	}
	return validateHookPermission(a.Permission, known)
}

// isCoreMethod reports whether method belongs to the core's own protocol surface. An
// extension must not be able to route a hook into initialize, a gateway call or another
// extension's reserved namespace.
func isCoreMethod(method string) bool {
	switch method {
	case MethodInitialize, MethodHealth, CancelMethod, MethodUIDescribe,
		MethodEventsSubscribe, MethodEventDeliver:
		return true
	}
	for _, prefix := range []string{"gateway.", "provisioner.", "fixture."} {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return false
}

func validateHookID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidParams)
	}
	if len(id) > maxHookID || !hookIDPattern.MatchString(id) {
		return fmt.Errorf("%w: id %q must be lower-case letters, digits, dot, dash or underscore",
			ErrInvalidParams, id)
	}
	return nil
}

// validateHookText checks a human-readable field: present, bounded, printable.
func validateHookText(field, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidParams, field)
	}
	if len(value) > max {
		return fmt.Errorf("%w: %s longer than %d bytes", ErrInvalidParams, field, max)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidParams, field)
		}
	}
	return nil
}

// validateHookPath checks that a nav item's path is an in-panel path.
//
// The rules are deliberately narrow. A path is rendered as a link by the client, so
// "javascript:alert(1)" or "https://elsewhere.example" would be a link injection; a
// path containing ".." or a backslash is an attempt to escape the panel's router.
func validateHookPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidParams)
	}
	if len(path) > maxHookPath {
		return fmt.Errorf("%w: path longer than %d bytes", ErrInvalidParams, maxHookPath)
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: path %q must start with /", ErrInvalidParams, path)
	}
	if strings.HasPrefix(path, "//") {
		return fmt.Errorf("%w: path %q must not be protocol-relative", ErrInvalidParams, path)
	}
	if strings.Contains(path, "..") || strings.Contains(path, `\`) {
		return fmt.Errorf("%w: path %q must not contain .. or a backslash", ErrInvalidParams, path)
	}
	for _, r := range path {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("%w: path contains whitespace or a control character", ErrInvalidParams)
		}
		if r == ':' {
			// Kills javascript:, data: and every other scheme.
			return fmt.Errorf("%w: path %q must not contain a scheme", ErrInvalidParams, path)
		}
	}
	return nil
}

func validateHookPermission(permission string, known func(string) bool) error {
	if strings.TrimSpace(permission) == "" {
		return fmt.Errorf("%w: permission is required: a hook the panel cannot guard is a hook it must not show",
			ErrInvalidParams)
	}
	if known == nil || !known(permission) {
		return fmt.Errorf("%w: permission %q is not a permission this core declares, so a hook cannot grant it",
			ErrInvalidParams, permission)
	}
	return nil
}

// ServiceActionParams is what the core sends when it invokes a declared action.
//
// It is a mutating payload: the operation key is the panel's Idempotency-Key, so the
// extension can make a retried click return the first result rather than acting twice.
type ServiceActionParams struct {
	MutatingMeta
	// Action is the declared action id, so an extension that routes several actions to
	// one method can tell them apart.
	Action string `json:"action"`
	// ServiceID is the core's service identifier.
	ServiceID string `json:"service_id"`
	// CustomerID is the billing account the service belongs to. It is informational:
	// the core has already decided the caller may act on this service.
	CustomerID string `json:"customer_id,omitempty"`
}

// ServiceActionResult is what the extension reports back.
type ServiceActionResult struct {
	// Message is shown to the operator after a successful call.
	Message string `json:"message,omitempty"`
	// Data is extension-specific detail, passed through untouched.
	Data map[string]any `json:"data,omitempty"`
}

// Extension is a snapshot of one installed extension's declarations.
type Extension struct {
	// Name is the name from the handshake.
	Name string
	// Version is the negotiated protocol version.
	Version int
	// NavItems are the navigation entries it contributes.
	NavItems []NavItem
	// ServiceActions are the service actions it contributes.
	ServiceActions []ServiceAction
	// AdminResources, Pages and CheckoutFields are the operator surfaces it declares
	// (GRW-05). They are data: the panel renders them, the extension ships no code to it.
	AdminResources []AdminResource
	Pages          []Page
	CheckoutFields []CheckoutField
	// Events are the lifecycle kinds it subscribed to.
	Events []string
}

// Stats is a point-in-time view of the registry, for the operator overview and for
// tests that need to prove a drop happened rather than assume it.
type Stats struct {
	// Extensions is how many extensions are installed.
	Extensions int
	// Delivered is how many events were handed to an extension.
	Delivered int64
	// Dropped is how many events were dropped because an extension's queue was full.
	// A non-zero value means an extension is not keeping up, which is a fact an
	// operator should be able to see rather than guess at.
	Dropped int64
	// Failed is how many notifications could not be written to the extension.
	Failed int64
}

// Hooks is the core's registry of installed extensions and what they declared.
//
// It is the whole of the extension-contributed UI surface: registration, lookup, and
// the two calls the panel makes (list the hooks, call one). It is safe for concurrent
// use; the request path only ever reads.
//
// Storage is in memory, against the running extension. That is deliberate: a
// declaration is only meaningful while the process that declared it is running, and
// persisting it would let a stale nav item outlive the extension that serves it. The
// installed-extension configuration is itself configuration (a path and its
// environment), so there is no table to hang a hook set off.
type Hooks struct {
	known      func(string) bool
	queueDepth int

	mu   sync.RWMutex
	exts map[string]*installed

	delivered atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
}

// installed is one registered extension.
type installed struct {
	owner   *Hooks
	name    string
	version int
	client  *Client

	navItems []NavItem
	actions  map[string]ServiceAction
	kinds    map[string]struct{}
	// The operator surfaces. They are held for the list endpoint and for nothing else: nothing
	// in the core calls an admin resource or a page method today, because no web view renders
	// them yet (see the report on GRW-05). Keeping them on the snapshot means the list endpoint
	// answers from one read of the registry rather than a second call per extension.
	resources []AdminResource
	pages     []Page
	checkout  []CheckoutField

	queue     chan Event
	delivered atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
	stop      chan struct{}
	stopOnce  sync.Once
}

// NewHooks constructs a registry.
//
// knownPermission reports whether a permission is one the core declares. It is required:
// a registry that cannot check permissions must not accept hooks, because the whole
// safety argument is that a hook cannot invent a capability.
func NewHooks(knownPermission func(string) bool) *Hooks {
	return &Hooks{
		known:      knownPermission,
		queueDepth: DefaultEventQueue,
		exts:       make(map[string]*installed),
	}
}

// SetQueueDepth overrides the per-extension queue depth. It exists for tests that need
// to prove the drop behaviour without generating a megabyte of events; a deployment
// leaves it at DefaultEventQueue.
func (h *Hooks) SetQueueDepth(n int) {
	if n <= 0 {
		return
	}
	h.mu.Lock()
	h.queueDepth = n
	h.mu.Unlock()
}

// Register installs an extension: it reads the handshake, asks a version-2 extension to
// describe its hooks and subscribe to events, validates both answers, and records what
// the extension may do.
//
// A version-1 extension is accepted and simply contributes nothing: it is not asked to
// describe anything it has never heard of, and it is never sent an event.
//
// Registration is all-or-nothing. If the extension's declaration is invalid the whole
// set is refused and the error says why; the caller keeps running the extension for its
// other capabilities, it just has no hooks.
func (h *Hooks) Register(ctx context.Context, c *Client) error {
	if c == nil {
		return fmt.Errorf("%w: nil client", ErrInvalidSpec)
	}
	info := c.Info()
	if strings.TrimSpace(info.Name) == "" {
		return fmt.Errorf("%w: the handshake did not name the extension", ErrProtocol)
	}
	// A restart re-registers the same name; the old entry's dispatcher must stop, or
	// the extension would receive every event twice.
	h.Unregister(info.Name)

	h.mu.RLock()
	depth := h.queueDepth
	h.mu.RUnlock()
	if depth <= 0 {
		depth = DefaultEventQueue
	}
	inst := &installed{
		owner:   h,
		name:    info.Name,
		version: info.Version,
		client:  c,
		actions: make(map[string]ServiceAction),
		kinds:   make(map[string]struct{}),
		queue:   make(chan Event, depth),
		stop:    make(chan struct{}),
	}

	if info.Version >= VersionTwo && info.HasCapability(MethodUIDescribe) {
		var described UIDescribeResult
		if err := c.Call(ctx, MethodUIDescribe, &UIDescribeParams{}, &described); err != nil {
			return fmt.Errorf("extproto: %s: %s: %w", info.Name, MethodUIDescribe, err)
		}
		if err := described.Validate(info.HasCapability, h.known); err != nil {
			return fmt.Errorf("extproto: %s: %s refused: %w", info.Name, MethodUIDescribe, err)
		}
		inst.navItems = described.NavItems
		for _, action := range described.ServiceActions {
			inst.actions[action.ID] = action
		}
		inst.resources = described.AdminResources
		inst.pages = described.Pages
		inst.checkout = described.CheckoutFields
	}

	if info.Version >= VersionTwo && info.HasCapability(MethodEventsSubscribe) {
		var subscribed EventsSubscribeResult
		err := c.Call(ctx, MethodEventsSubscribe, &EventsSubscribeParams{Kinds: LifecycleKinds}, &subscribed)
		if err != nil {
			return fmt.Errorf("extproto: %s: %s: %w", info.Name, MethodEventsSubscribe, err)
		}
		if err := subscribed.Validate(); err != nil {
			return fmt.Errorf("extproto: %s: %s refused: %w", info.Name, MethodEventsSubscribe, err)
		}
		for _, kind := range subscribed.Accepted {
			inst.kinds[kind] = struct{}{}
		}
	}

	h.mu.Lock()
	h.exts[inst.name] = inst
	h.mu.Unlock()

	if len(inst.kinds) > 0 {
		go inst.deliver()
	}
	return nil
}

// Unregister removes an extension and stops its delivery goroutine. It is safe to call
// for an extension that was never registered.
func (h *Hooks) Unregister(name string) {
	h.mu.Lock()
	inst, ok := h.exts[name]
	delete(h.exts, name)
	h.mu.Unlock()
	if !ok {
		return
	}
	// The queue is deliberately not closed: a concurrent Emit may still hold a reference
	// to it, and sending on a closed channel would panic. The dispatcher exits on stop, so
	// anything still queued is dropped, which is what the drop counter means anyway.
	inst.stopOnce.Do(func() { close(inst.stop) })
}

// Close unregisters everything. The clients themselves are owned by the caller that
// launched them and are not closed here.
func (h *Hooks) Close() {
	h.mu.Lock()
	names := make([]string, 0, len(h.exts))
	for name := range h.exts {
		names = append(names, name)
	}
	h.mu.Unlock()
	for _, name := range names {
		h.Unregister(name)
	}
}

// Extensions returns a snapshot of every installed extension's declarations, ordered by
// name so the panel's menu does not reshuffle between requests.
func (h *Hooks) Extensions() []Extension {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Extension, 0, len(h.exts))
	for _, inst := range h.exts {
		kinds := make([]string, 0, len(inst.kinds))
		for kind := range inst.kinds {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		actions := make([]ServiceAction, 0, len(inst.actions))
		for _, action := range inst.actions {
			actions = append(actions, action)
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i].ID < actions[j].ID })
		out = append(out, Extension{
			Name:           inst.name,
			Version:        inst.version,
			NavItems:       append([]NavItem(nil), inst.navItems...),
			ServiceActions: actions,
			AdminResources: append([]AdminResource(nil), inst.resources...),
			Pages:          append([]Page(nil), inst.pages...),
			CheckoutFields: append([]CheckoutField(nil), inst.checkout...),
			Events:         kinds,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Action looks up one declared service action.
func (h *Hooks) Action(extension, action string) (ServiceAction, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	inst, ok := h.exts[extension]
	if !ok {
		return ServiceAction{}, false
	}
	found, ok := inst.actions[action]
	return found, ok
}

// Call invokes a declared service action on the extension that declared it.
//
// The method called is the one the extension named in its declaration, the payload is
// the service id plus the caller's operation key, and the deadline is the caller's. It
// is the only call the panel ever makes on an extension's behalf outside the gateway and
// provisioner surfaces, and it exists only for methods the extension advertised.
func (h *Hooks) Call(ctx context.Context, extension, action, serviceID, customerID, operationKey string) (ServiceActionResult, error) {
	h.mu.RLock()
	inst, ok := h.exts[extension]
	if !ok {
		h.mu.RUnlock()
		return ServiceActionResult{}, fmt.Errorf("%w: extension %q is not installed", ErrHookNotFound, extension)
	}
	declared, ok := inst.actions[action]
	client := inst.client
	h.mu.RUnlock()
	if !ok {
		return ServiceActionResult{}, fmt.Errorf("%w: extension %q declares no action %q",
			ErrHookNotFound, extension, action)
	}

	var out ServiceActionResult
	err := client.Call(ctx, declared.Method, &ServiceActionParams{
		MutatingMeta: MutatingMeta{OperationKey: operationKey},
		Action:       declared.ID,
		ServiceID:    serviceID,
		CustomerID:   customerID,
	}, &out)
	if err != nil {
		return ServiceActionResult{}, err
	}
	return out, nil
}

// Emit offers one committed event to every extension that subscribed to its kind.
//
// It never blocks. A subscriber whose queue is full loses the event, and the loss is
// counted: a checkout must not wait for an extension, and a silent drop is
// indistinguishable from a delivery bug, so the counter is the signal.
func (h *Hooks) Emit(ev Event) {
	if !IsLifecycleKind(ev.Kind) {
		// The bridge filters these out before they arrive; ignoring rather than
		// counting keeps the drop counter meaning "the extension could not keep up".
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	h.mu.RLock()
	targets := make([]*installed, 0, len(h.exts))
	for _, inst := range h.exts {
		if _, ok := inst.kinds[ev.Kind]; ok {
			targets = append(targets, inst)
		}
	}
	h.mu.RUnlock()

	for _, inst := range targets {
		select {
		case inst.queue <- ev:
			inst.delivered.Add(1)
			h.delivered.Add(1)
		default:
			inst.dropped.Add(1)
			h.dropped.Add(1)
		}
	}
}

// Stats reports what the registry has done.
func (h *Hooks) Stats() Stats {
	h.mu.RLock()
	n := len(h.exts)
	h.mu.RUnlock()
	return Stats{
		Extensions: n,
		Delivered:  h.delivered.Load(),
		Dropped:    h.dropped.Load(),
		Failed:     h.failed.Load(),
	}
}

// deliver drains one extension's queue into notifications.
//
// It is one goroutine per extension so a dead extension blocks only its own queue: a
// shared dispatcher would let one wedged pipe stop every other extension's events.
func (in *installed) deliver() {
	for {
		select {
		case <-in.stop:
			return
		case ev := <-in.queue:
			// Notify can block: it hands the frame to the client's writer, and a
			// process that has stopped reading its stdin will eventually fill the pipe.
			// That is why the queue is bounded and separate per extension — the block
			// is contained here, and the next Emit drops instead of waiting.
			if err := in.client.Notify(MethodEventDeliver, &ev); err != nil {
				in.failed.Add(1)
				in.owner.failed.Add(1)
			}
		}
	}
}
