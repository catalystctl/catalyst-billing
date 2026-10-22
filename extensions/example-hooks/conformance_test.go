package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
)

// Conformance: the core's own extension client driving this extension as a separate
// process, over real pipes.
//
// The unit-level questions about a declaration ("is this path safe?", "is this permission
// one the core declares?") are answered by internal/extproto's own tests. What only a
// separate process can prove is that the wire form is right: that the handshake negotiates
// version 2, that ui.describe's answer parses into the core's structs and passes the core's
// validation, that the subscription is honoured, and that an event arrives as a
// notification on stdin rather than as a call.
//
// This is the same shape as extensions/stripe/conformance_test.go: build the binary, launch
// it through extproto.Client, and assert on what crosses the pipe.

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "example-hooks-ext-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "example-hooks tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "example-hooks")
	build := exec.Command("go", "build", "-o", out, ".")
	if b, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "example-hooks tests: building the extension: %v\n%s", err, b)
		os.Exit(1)
	}
	binPath = out
	os.Exit(m.Run())
}

// launch starts the extension the way the panel does and registers its cleanup.
func launch(t *testing.T, env ...string) *extproto.Client {
	t.Helper()
	c, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             binPath,
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// corePermission reports whether p is a permission the core declares.
func corePermission(p string) bool {
	for _, declared := range authn.AllPermissions() {
		if string(declared) == p {
			return true
		}
	}
	return false
}

// The handshake must select version 2 and advertise both version-2 methods: the core calls
// nothing that was not advertised.
func TestConformanceNegotiatesVersionTwo(t *testing.T) {
	c := launch(t)
	info := c.Info()
	if info.Version != extproto.VersionTwo {
		t.Fatalf("version = %d, want %d", info.Version, extproto.VersionTwo)
	}
	if info.Name != "example-hooks" {
		t.Fatalf("name = %q", info.Name)
	}
	for _, method := range []string{extproto.MethodUIDescribe, extproto.MethodEventsSubscribe} {
		if !info.HasCapability(method) {
			t.Fatalf("the extension did not advertise %s", method)
		}
	}
}

// ui.describe must produce a declaration the core accepts, and it must be exactly one nav
// item and one service action: that is what the fixture claims to be.
func TestConformanceDescribeDeclaresOneNavItemAndOneAction(t *testing.T) {
	c := launch(t)
	ctx := context.Background()

	var described extproto.UIDescribeResult
	if err := c.Call(ctx, extproto.MethodUIDescribe, &extproto.UIDescribeParams{}, &described); err != nil {
		t.Fatalf("ui.describe: %v", err)
	}
	if err := described.Validate(c.Info().HasCapability, corePermission); err != nil {
		t.Fatalf("the core refused the declaration: %v", err)
	}
	if len(described.NavItems) != 1 {
		t.Fatalf("nav items = %d, want 1", len(described.NavItems))
	}
	if len(described.ServiceActions) != 1 {
		t.Fatalf("service actions = %d, want 1", len(described.ServiceActions))
	}
	action := described.ServiceActions[0]
	if !c.Info().HasCapability(action.Method) {
		t.Fatalf("the action calls %q, which the extension did not advertise", action.Method)
	}
	if action.Confirm == "" {
		t.Fatal("the action is destructive but declares no confirmation prompt")
	}
}

// The whole point of version 2: a committed lifecycle event arrives as a notification, and
// the extension can see it.
func TestConformanceReceivesAnOrderEventAsANotification(t *testing.T) {
	c := launch(t)
	ctx := context.Background()

	var subscribed extproto.EventsSubscribeResult
	if err := c.Call(ctx, extproto.MethodEventsSubscribe, &extproto.EventsSubscribeParams{
		Kinds: extproto.LifecycleKinds,
	}, &subscribed); err != nil {
		t.Fatalf("events.subscribe: %v", err)
	}
	if err := subscribed.Validate(); err != nil {
		t.Fatalf("subscription refused: %v", err)
	}
	if len(subscribed.Accepted) == 0 {
		t.Fatal("the extension subscribed to nothing")
	}

	// A notification, not a call: there is no reply to wait for.
	if err := c.Notify(extproto.MethodEventDeliver, &extproto.Event{
		Kind:       extproto.EventOrderCreated,
		ID:         7,
		CustomerID: "cust_conformance",
		Data:       map[string]any{"order_id": "ord_conformance"},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

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
		if err := c.Call(ctx, "fixture.received", nil, &received); err != nil {
			t.Fatalf("fixture.received: %v", err)
		}
		if len(received.Events) > 0 {
			ev := received.Events[0]
			if ev.Kind != extproto.EventOrderCreated || ev.ID != 7 || ev.CustomerID != "cust_conformance" {
				t.Fatalf("event arrived mangled: %+v", ev)
			}
			if ev.Data["order_id"] != "ord_conformance" {
				t.Fatalf("payload arrived mangled: %v", ev.Data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the extension never received the event")
}

// The action is idempotent by operation key: a retried call returns the first result.
func TestConformanceServiceActionIsIdempotent(t *testing.T) {
	c := launch(t)
	ctx := context.Background()

	params := func(key string) *extproto.ServiceActionParams {
		return &extproto.ServiceActionParams{
			MutatingMeta: extproto.MutatingMeta{OperationKey: key},
			Action:       "reboot",
			ServiceID:    "svc_conformance",
		}
	}
	var first, replay, second extproto.ServiceActionResult
	if err := c.Call(ctx, "service.reboot", params("conformance-key-1"), &first); err != nil {
		t.Fatalf("service.reboot: %v", err)
	}
	if first.Data["service_id"] != "svc_conformance" {
		t.Fatalf("service id = %v", first.Data["service_id"])
	}
	if err := c.Call(ctx, "service.reboot", params("conformance-key-1"), &replay); err != nil {
		t.Fatalf("replayed service.reboot: %v", err)
	}
	if replay.Data["call_id"] != first.Data["call_id"] {
		t.Fatalf("a replay acted twice: %v then %v", first.Data["call_id"], replay.Data["call_id"])
	}
	if err := c.Call(ctx, "service.reboot", params("conformance-key-2"), &second); err != nil {
		t.Fatalf("second service.reboot: %v", err)
	}
	if second.Data["call_id"] == first.Data["call_id"] {
		t.Fatal("a new operation key was treated as a replay")
	}
}

// A refusal must reach the core as a rejection, not as a transport failure: the panel shows
// "the extension said no" differently from "the extension is broken".
func TestConformanceRefusalIsARejection(t *testing.T) {
	c := launch(t, "EXAMPLE_HOOKS_MODE=reject")
	var out extproto.ServiceActionResult
	err := c.Call(context.Background(), "service.reboot", &extproto.ServiceActionParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "conformance-reject"},
		Action:       "reboot",
		ServiceID:    "svc_conformance",
	}, &out)
	if err == nil {
		t.Fatal("a refused action was reported as a success")
	}
	if !errors.Is(err, extproto.ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected", err)
	}
}

// A v1-only build of the same extension is still accepted, and it advertises neither
// version-2 method, so the core will not call them.
func TestConformanceVersionOneOnlyIsAccepted(t *testing.T) {
	c := launch(t, "EXAMPLE_HOOKS_VERSIONS=1")
	info := c.Info()
	if info.Version != extproto.Version {
		t.Fatalf("version = %d, want %d", info.Version, extproto.Version)
	}
	for _, method := range []string{extproto.MethodUIDescribe, extproto.MethodEventsSubscribe} {
		if info.HasCapability(method) {
			t.Fatalf("a v1 extension advertised %s", method)
		}
	}
}

// Registering with the core's registry is the end-to-end acceptance of the declaration: the
// registry validates it, stores it, and serves it back.
func TestConformanceRegistersWithTheCore(t *testing.T) {
	c := launch(t)
	reg := extproto.NewHooks(corePermission)
	defer reg.Close()
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("register: %v", err)
	}
	exts := reg.Extensions()
	if len(exts) != 1 {
		t.Fatalf("extensions = %d", len(exts))
	}
	ext := exts[0]
	if len(ext.NavItems) != 1 || len(ext.ServiceActions) != 1 {
		t.Fatalf("registered %+v", ext)
	}
	if _, ok := reg.Action(ext.Name, ext.ServiceActions[0].ID); !ok {
		t.Fatal("the registered action is not callable")
	}

	// The raw declaration is what the panel serves, so assert on the wire form too.
	raw, err := json.Marshal(ext.NavItems[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"id", "label", "path", "icon", "permission"} {
		if _, ok := fields[want]; !ok {
			t.Fatalf("a nav item is missing %q on the wire: %s", want, raw)
		}
	}
}
