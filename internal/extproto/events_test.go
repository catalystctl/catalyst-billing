package extproto_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

var (
	v1BinOnce sync.Once
	v1Bin     string
	v1BinErr  error
)

// launchVersionOneGateway starts the core's own v1-only fixture (fakegateway), which is
// the compatibility case that matters: an extension written before version 2 existed.
func launchVersionOneGateway(t *testing.T) *extproto.Client {
	t.Helper()
	v1BinOnce.Do(func() { v1Bin, v1BinErr = buildFixture("fakegateway") })
	if v1BinErr != nil {
		t.Fatalf("build fakegateway: %v", v1BinErr)
	}
	c, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             v1Bin,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err != nil {
		t.Fatalf("Launch(fakegateway): %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Protocol version 2, additively.
//
// The two properties that matter are stated here rather than left to the implementation:
// the core offers version 2 *and* version 1, and a version-1 extension is not asked for
// anything it has never heard of. Everything else in version 2 (the subscription, the
// notification, the bounded queue) is tested in hooks_test.go.

func TestSupportedVersionsOffersTwoAndOne(t *testing.T) {
	got := extproto.SupportedVersions()
	if len(got) != 2 || got[0] != extproto.VersionTwo || got[1] != extproto.Version {
		t.Fatalf("SupportedVersions() = %v, want [%d %d] newest first",
			got, extproto.VersionTwo, extproto.Version)
	}
	if !extproto.SupportsVersion(extproto.Version) {
		t.Fatal("version 1 is no longer supported, so every existing extension is broken")
	}
	if !extproto.SupportsVersion(extproto.VersionTwo) {
		t.Fatal("version 2 is not supported")
	}
	if extproto.SupportsVersion(99) {
		t.Fatal("an invented version is reported as supported")
	}
}

// The version-2 surface is declared as capabilities, so an extension that does not
// implement it is never called with it.
func TestVersionTwoMethodsAreNamedCapabilities(t *testing.T) {
	if extproto.CapUIDescribe != extproto.MethodUIDescribe {
		t.Fatalf("CapUIDescribe = %q", extproto.CapUIDescribe)
	}
	if extproto.CapEventsSubscribe != extproto.MethodEventsSubscribe {
		t.Fatalf("CapEventsSubscribe = %q", extproto.CapEventsSubscribe)
	}
}

// A v1-only fixture still handshakes, and it is not offered the version-2 calls: the core
// asks only what the negotiated version defines.
func TestVersionOneFixtureStillHandshakes(t *testing.T) {
	c := launchVersionOneGateway(t)
	info := c.Info()
	if info.Version != extproto.Version {
		t.Fatalf("negotiated version = %d, want %d", info.Version, extproto.Version)
	}
	// The core must not call ui.describe on an extension that never advertised it.
	var out extproto.UIDescribeResult
	err := c.Call(context.Background(), extproto.MethodUIDescribe, &extproto.UIDescribeParams{}, &out)
	if err == nil {
		t.Fatal("ui.describe succeeded on a version-1 extension")
	}
	if !errors.Is(err, extproto.ErrRemote) {
		t.Fatalf("error = %v, want a remote method-not-found", err)
	}
}

// The kind list is closed: the core refuses a subscription to a kind it cannot emit,
// instead of silently never delivering it.
func TestLifecycleKindsAreClosed(t *testing.T) {
	for _, kind := range []string{
		extproto.EventCartCreated,
		extproto.EventOrderCreated,
		extproto.EventTicketCreated,
	} {
		if !extproto.IsLifecycleKind(kind) {
			t.Fatalf("%q is not a lifecycle kind", kind)
		}
	}
	if extproto.IsLifecycleKind("invoice.paid") {
		t.Fatal("an unrelated broker kind is offered as a lifecycle event")
	}

	err := extproto.EventsSubscribeResult{Accepted: []string{"order.created", "invoice.paid"}}.Validate()
	if err == nil {
		t.Fatal("a subscription to an unknown kind was accepted")
	}
	if !errors.Is(err, extproto.ErrInvalidParams) {
		t.Fatalf("error = %v, want ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "invoice.paid") {
		t.Fatalf("the refusal does not name the kind: %v", err)
	}
}
