package extproto_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
)

// The extension-hook surface: what an extension may declare, what is refused, and how an
// event reaches one.
//
// These tests drive the real fixture binary through the real client, so a declaration
// that the panel could not render safely fails here rather than in a browser. The
// validation cases are table-driven on purpose: each one is a rule the panel's safety
// argument depends on, and a rule with no failing case is a rule nobody will keep.

var (
	hookBinOnce sync.Once
	hookBin     string
	hookBinErr  error
)

// hooksBin builds extensions/example-hooks once for this package's tests.
//
// It is built lazily rather than in TestMain because the fixture belongs to another
// package's test list: only the tests in this file need it, and a build failure should
// name the tests that depend on it.
func hooksBin(t *testing.T) string {
	t.Helper()
	hookBinOnce.Do(func() {
		hookBin, hookBinErr = buildFixture("example-hooks")
	})
	if hookBinErr != nil {
		t.Fatalf("build example-hooks: %v", hookBinErr)
	}
	return hookBin
}

// buildFixture compiles one extension binary into a temporary directory.
//
// It builds rather than "go run"s for the same reason the package's own fixtures do: the
// client hands the child nothing but PATH, and "go run" needs HOME and GOCACHE.
func buildFixture(name string) (string, error) {
	root, err := filepath.Abs("../..")
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "extproto-"+name+"-*")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", out, "./extensions/"+name)
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./extensions/%s: %v\n%s", name, err, b)
	}
	return out, nil
}

// launchHooks starts the fixture and registers its cleanup.
func launchHooks(t *testing.T, env ...string) *extproto.Client {
	t.Helper()
	c, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             hooksBin(t),
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err != nil {
		t.Fatalf("Launch(example-hooks, %v): %v", env, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// corePermissions is the real permission table, so a test that passes does so against
// what the panel actually declares.
func corePermissions(p string) bool {
	for _, declared := range authn.AllPermissions() {
		if string(declared) == p {
			return true
		}
	}
	return false
}

// A version-2 extension is asked what it would like the panel to show, and the answer is
// stored against it.
func TestRegisterStoresWhatTheExtensionDeclared(t *testing.T) {
	c := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()

	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	exts := reg.Extensions()
	if len(exts) != 1 {
		t.Fatalf("extensions = %d, want 1", len(exts))
	}
	ext := exts[0]
	if ext.Name != "example-hooks" {
		t.Fatalf("name = %q", ext.Name)
	}
	if ext.Version != extproto.VersionTwo {
		t.Fatalf("version = %d, want %d", ext.Version, extproto.VersionTwo)
	}
	if len(ext.NavItems) != 1 {
		t.Fatalf("nav items = %d, want 1", len(ext.NavItems))
	}
	nav := ext.NavItems[0]
	if nav.Label == "" || nav.Path != "/staff/extensions/example-hooks" || nav.Icon != "plug" {
		t.Fatalf("nav item = %+v", nav)
	}
	if nav.Permission != string(authn.PermExtensionManage) {
		t.Fatalf("nav permission = %q", nav.Permission)
	}
	if len(ext.ServiceActions) != 1 {
		t.Fatalf("service actions = %d, want 1", len(ext.ServiceActions))
	}
	action := ext.ServiceActions[0]
	if action.Method != "service.reboot" {
		t.Fatalf("action method = %q", action.Method)
	}
	if action.Confirm == "" {
		t.Fatal("a destructive action was declared with no confirmation prompt")
	}
	if action.Permission != string(authn.PermServiceWriteSelf) {
		t.Fatalf("action permission = %q", action.Permission)
	}
}

// A v1-only extension is accepted and contributes nothing. This is the compatibility
// promise: the version-2 surface must be additive, not a requirement.
func TestVersionOneExtensionIsAcceptedAndHasNoHooks(t *testing.T) {
	c := launchHooks(t, "EXAMPLE_HOOKS_VERSIONS=1")
	if got := c.Info().Version; got != extproto.Version {
		t.Fatalf("negotiated version = %d, want %d", got, extproto.Version)
	}
	if c.Info().HasCapability(extproto.MethodUIDescribe) {
		t.Fatal("a v1 extension advertised ui.describe")
	}

	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register(v1): %v", err)
	}
	exts := reg.Extensions()
	if len(exts) != 1 {
		t.Fatalf("extensions = %d, want 1", len(exts))
	}
	if len(exts[0].NavItems) != 0 || len(exts[0].ServiceActions) != 0 || len(exts[0].Events) != 0 {
		t.Fatalf("a v1 extension contributed %+v", exts[0])
	}

	// And it must never be sent an event, even if the core emits one it would have
	// subscribed to had it been asked.
	reg.Emit(extproto.Event{Kind: extproto.EventOrderCreated, Data: map[string]any{"order_id": "o1"}})
	if stats := reg.Stats(); stats.Delivered != 0 {
		t.Fatalf("delivered %d events to a v1 extension", stats.Delivered)
	}
}

// A hook cannot invent a capability: a permission the core does not declare is refused,
// and the refusal is the whole registration, not one item.
func TestRegisterRefusesAPermissionTheCoreDoesNotDeclare(t *testing.T) {
	c := launchHooks(t, "EXAMPLE_HOOKS_NAV_PERMISSION=service:teleport")
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()

	err := reg.Register(context.Background(), c)
	if err == nil {
		t.Fatal("a hook declaring an undeclared permission was registered")
	}
	if !errors.Is(err, extproto.ErrInvalidParams) {
		t.Fatalf("error = %v, want ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "service:teleport") {
		t.Fatalf("the refusal does not name the permission: %v", err)
	}
	if n := len(reg.Extensions()); n != 0 {
		t.Fatalf("a refused declaration left %d extension(s) installed", n)
	}
}

// The declared permission is what guards the action, so the panel must be able to look
// it up before it calls anything.
func TestActionLookupReturnsTheDeclaredAction(t *testing.T) {
	c := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	action, ok := reg.Action("example-hooks", "reboot")
	if !ok {
		t.Fatal("the declared action was not found")
	}
	if action.Permission != string(authn.PermServiceWriteSelf) {
		t.Fatalf("permission = %q", action.Permission)
	}
	if _, ok := reg.Action("example-hooks", "self-destruct"); ok {
		t.Fatal("an undeclared action was found")
	}
	if _, ok := reg.Action("nobody", "reboot"); ok {
		t.Fatal("an action of an uninstalled extension was found")
	}
}

// Calling an action is a call to the declared method with the service id, and it is
// idempotent on the operation key: a retried click must not reboot twice.
func TestCallServiceActionIsIdempotentOnTheOperationKey(t *testing.T) {
	c := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := context.Background()

	first, err := reg.Call(ctx, "example-hooks", "reboot", "svc_1", "cust_1", "idem-key-0001")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if first.Message == "" {
		t.Fatal("the action returned no message")
	}
	if got, _ := first.Data["service_id"].(string); got != "svc_1" {
		t.Fatalf("service_id = %v, want the service the panel passed", first.Data["service_id"])
	}
	callID, _ := first.Data["call_id"].(string)

	// The same key again: the same answer, and the fixture performed one reboot.
	second, err := reg.Call(ctx, "example-hooks", "reboot", "svc_1", "cust_1", "idem-key-0001")
	if err != nil {
		t.Fatalf("replayed Call: %v", err)
	}
	if second.Data["call_id"] != callID {
		t.Fatalf("a replayed call produced a second effect: %v then %v", callID, second.Data["call_id"])
	}

	// A different key is a different action.
	third, err := reg.Call(ctx, "example-hooks", "reboot", "svc_1", "cust_1", "idem-key-0002")
	if err != nil {
		t.Fatalf("Call with a new key: %v", err)
	}
	if third.Data["call_id"] == callID {
		t.Fatal("a new operation key was treated as a replay")
	}
}

// A missing operation key is refused before anything is sent: the protocol will not let a
// mutating call out without one.
func TestCallRequiresAnOperationKey(t *testing.T) {
	c := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := reg.Call(context.Background(), "example-hooks", "reboot", "svc_1", "cust_1", "")
	if !errors.Is(err, extproto.ErrInvalidParams) {
		t.Fatalf("error = %v, want ErrInvalidParams", err)
	}
}

// A business refusal from the extension is a rejection, not a transport failure: the panel
// must be able to tell "the extension said no" from "the extension is broken".
func TestActionRejectionIsReportedAsARejection(t *testing.T) {
	c := launchHooks(t, "EXAMPLE_HOOKS_MODE=reject")
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := reg.Call(context.Background(), "example-hooks", "reboot", "svc_1", "cust_1", "idem-key-0003")
	if err == nil {
		t.Fatal("a rejected action was reported as a success")
	}
	if !errors.Is(err, extproto.ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected", err)
	}
}

// The subscription is explicit: the extension answers with the kinds it wants, and only
// those are delivered.
func TestSubscribeIsHonoured(t *testing.T) {
	c := launchHooks(t, "EXAMPLE_HOOKS_EVENT_KINDS=ticket.created")
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	exts := reg.Extensions()
	if len(exts[0].Events) != 1 || exts[0].Events[0] != extproto.EventTicketCreated {
		t.Fatalf("subscription = %v", exts[0].Events)
	}

	// An order event is not wanted, so it is not delivered.
	reg.Emit(extproto.Event{Kind: extproto.EventOrderCreated, Data: map[string]any{"order_id": "o1"}})
	if stats := reg.Stats(); stats.Delivered != 0 {
		t.Fatalf("an unsubscribed kind was delivered %d time(s)", stats.Delivered)
	}
}

// A subscribed event reaches the extension as a notification, and the extension can see
// it: this is the end-to-end proof that the notification path works over real pipes.
func TestSubscribedEventReachesTheExtension(t *testing.T) {
	c := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reg.Emit(extproto.Event{
		Kind:       extproto.EventOrderCreated,
		CustomerID: "cust_1",
		Data:       map[string]any{"order_id": "ord_1"},
		ID:         42,
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var received struct {
			Events []struct {
				Kind       string         `json:"kind"`
				ID         int64          `json:"id"`
				CustomerID string         `json:"customer_id"`
				Data       map[string]any `json:"data"`
			} `json:"events"`
		}
		if err := c.Call(context.Background(), "fixture.received", nil, &received); err != nil {
			t.Fatalf("fixture.received: %v", err)
		}
		if len(received.Events) > 0 {
			ev := received.Events[0]
			if ev.Kind != extproto.EventOrderCreated {
				t.Fatalf("kind = %q", ev.Kind)
			}
			if ev.Data["order_id"] != "ord_1" || ev.CustomerID != "cust_1" || ev.ID != 42 {
				t.Fatalf("event arrived mangled: %+v", ev)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the extension never received the event")
}

// A dead extension must not be able to block the core. Emit never blocks, and what cannot
// be delivered is dropped and counted.
func TestEmitDropsInsteadOfBlockingWhenTheExtensionIsDead(t *testing.T) {
	c := launchHooks(t, "EXAMPLE_HOOKS_MODE=stall")
	reg := extproto.NewHooks(corePermissions)
	reg.SetQueueDepth(2)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The extension has stopped reading, so the client's write queue and the pipe fill
	// up and the delivery goroutine blocks. Emit must still return immediately.
	payload := strings.Repeat("x", 4096)
	start := time.Now()
	for i := 0; i < 400; i++ {
		reg.Emit(extproto.Event{
			Kind: extproto.EventOrderCreated,
			Data: map[string]any{"order_id": "ord", "filler": payload},
		})
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("400 emits to a dead extension took %s; emission is on the caller's path", elapsed)
	}

	stats := reg.Stats()
	if stats.Dropped == 0 {
		t.Fatalf("nothing was dropped, so the bounded queue is not bounding: %+v", stats)
	}
	if stats.Dropped+stats.Delivered != 400 {
		t.Fatalf("delivered %d + dropped %d != 400 emitted", stats.Delivered, stats.Dropped)
	}
}

// Re-registering an extension (a restart) replaces its declaration instead of duplicating
// it, so the panel does not show one nav item twice and the extension does not receive
// every event twice.
func TestRegisterReplacesAnExistingExtension(t *testing.T) {
	first := launchHooks(t)
	reg := extproto.NewHooks(corePermissions)
	defer reg.Close()
	if err := reg.Register(context.Background(), first); err != nil {
		t.Fatalf("Register: %v", err)
	}
	second := launchHooks(t)
	if err := reg.Register(context.Background(), second); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if n := len(reg.Extensions()); n != 1 {
		t.Fatalf("extensions = %d after a restart, want 1", n)
	}
}

// ------------------------------------------------------------------ validation --

// Each of these declarations is one the panel must refuse, because rendering it would
// either run code, link somewhere the panel does not route, or claim a capability the core
// does not have.
func TestDescribeValidation(t *testing.T) {
	advertised := func(method string) bool { return method == "service.reboot" }
	known := corePermissions

	nav := func(mutate func(*extproto.NavItem)) extproto.UIDescribeResult {
		item := extproto.NavItem{
			ID: "example", Label: "Example", Path: "/staff/extensions/example",
			Icon: "plug", Permission: "extension:manage",
		}
		mutate(&item)
		return extproto.UIDescribeResult{NavItems: []extproto.NavItem{item}}
	}
	action := func(mutate func(*extproto.ServiceAction)) extproto.UIDescribeResult {
		item := extproto.ServiceAction{
			ID: "reboot", Label: "Reboot", Method: "service.reboot",
			Confirm: "Reboot this service?", Permission: "service:write:self",
		}
		mutate(&item)
		return extproto.UIDescribeResult{ServiceActions: []extproto.ServiceAction{item}}
	}

	valid := extproto.UIDescribeResult{
		NavItems:       nav(func(*extproto.NavItem) {}).NavItems,
		ServiceActions: action(func(*extproto.ServiceAction) {}).ServiceActions,
	}
	if err := valid.Validate(advertised, known); err != nil {
		t.Fatalf("a valid declaration was refused: %v", err)
	}

	cases := []struct {
		name   string
		decl   extproto.UIDescribeResult
		expect string
	}{
		{"unknown permission", nav(func(n *extproto.NavItem) { n.Permission = "service:teleport" }), "service:teleport"},
		{"empty permission", nav(func(n *extproto.NavItem) { n.Permission = "" }), "permission is required"},
		{"javascript path", nav(func(n *extproto.NavItem) { n.Path = "javascript:alert(1)" }), "must start with /"},
		{"absolute url path", nav(func(n *extproto.NavItem) { n.Path = "https://evil.example/x" }), "must start with /"},
		{"protocol-relative path", nav(func(n *extproto.NavItem) { n.Path = "//evil.example/x" }), "protocol-relative"},
		{"traversal path", nav(func(n *extproto.NavItem) { n.Path = "/staff/../../admin" }), "must not contain"},
		{"svg icon", nav(func(n *extproto.NavItem) { n.Icon = "<svg onload=alert(1)>" }), "icon"},
		{"empty label", nav(func(n *extproto.NavItem) { n.Label = "  " }), "label is required"},
		{"control character in label", nav(func(n *extproto.NavItem) { n.Label = "a\nb" }), "control character"},
		{"bad id", nav(func(n *extproto.NavItem) { n.ID = "Example Hooks" }), "id"},
		{"duplicate nav id", extproto.UIDescribeResult{NavItems: append(
			nav(func(*extproto.NavItem) {}).NavItems, nav(func(*extproto.NavItem) {}).NavItems...)}, "duplicate id"},
		{"unadvertised method", action(func(a *extproto.ServiceAction) { a.Method = "service.selfdestruct" }), "not advertised"},
		{"core method", action(func(a *extproto.ServiceAction) { a.Method = "gateway.refund" }), "core protocol method"},
		{"initialize as a hook", action(func(a *extproto.ServiceAction) { a.Method = "initialize" }), "core protocol method"},
		{"action with no permission", action(func(a *extproto.ServiceAction) { a.Permission = "" }), "permission is required"},
		{"too many nav items", extproto.UIDescribeResult{NavItems: manyNav(extproto.MaxNavItems + 1)}, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decl.Validate(advertised, known)
			if err == nil {
				t.Fatal("an unsafe declaration was accepted")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("error %q does not mention %q", err, tc.expect)
			}
		})
	}
}

// manyNav builds n distinct nav items.
func manyNav(n int) []extproto.NavItem {
	out := make([]extproto.NavItem, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, extproto.NavItem{
			ID:    "item" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Label: "Item", Path: "/staff/x", Icon: "plug", Permission: "extension:manage",
		})
	}
	return out
}
