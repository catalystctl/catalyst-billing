package main

// Conformance: the core's own extension client driving this adapter as a separate process, over real
// pipes.
//
// The unit tests in main_test.go answer "is the logic right". This file answers the only question a
// separate process can: is the *wire form* right? That the handshake negotiates a version the core
// offers, that `provisioner.fields` parses into the core's own structs and passes the core's own
// validation, that the capability list matches the handlers, and that a call the adapter did not
// advertise fails as "method not found" rather than hanging.
//
// It is the same shape as extensions/example-hooks/conformance_test.go and
// extensions/stripe/conformance_test.go: build the binary, launch it through extproto.Client, and
// assert on what crosses the pipe.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

var conformanceBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "catalystctl-ext-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalystctl tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "catalystctl")
	build := exec.Command("go", "build", "-o", out, ".")
	if b, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "catalystctl tests: building the adapter: %v\n%s", err, b)
		os.Exit(1)
	}
	conformanceBin = out
	os.Exit(m.Run())
}

// launchProcess starts the adapter the way the panel does.
func launchProcess(t *testing.T, env ...string) *extproto.Client {
	t.Helper()
	client, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             conformanceBin,
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err != nil {
		t.Fatalf("launch the adapter: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// conformanceEnv builds the environment a launched adapter needs.
func conformanceEnv(t *testing.T, panelURL string) []string {
	t.Helper()
	return []string{
		"CCTL_PANEL_URL=" + panelURL,
		"CCTL_API_KEY=catalyst_conformance",
		"CCTL_STATE_FILE=" + filepath.Join(t.TempDir(), "operations.json"),
	}
}

func TestConformanceHandshakeAdvertisesWhatItImplements(t *testing.T) {
	panel := newFakePanel(t)
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	info := client.Info()
	if info.Name != "catalystctl" {
		t.Fatalf("the adapter should identify itself, got %q", info.Name)
	}
	if version := info.Version; version != 1 && version != 2 {
		t.Fatalf("expected a protocol version the core offers, got %d", version)
	}

	// Every capability the core can dispatch on must be advertised, or the core refuses the call
	// before making it and the operation parks as "unsupported".
	for _, want := range []string{
		extproto.CapProvisionerDescribe,
		extproto.CapProvisionerValidate,
		extproto.CapProvisionerCreate,
		extproto.CapProvisionerInspect,
		extproto.CapProvisionerLookup,
		extproto.CapProvisionerSuspend,
		extproto.CapProvisionerResume,
		extproto.CapProvisionerTerminate,
		extproto.CapProvisionerFields,
		extproto.CapProvisionerResource,
		extproto.CapProvisionerPower,
	} {
		if !info.HasCapability(want) {
			t.Fatalf("the handshake does not advertise %q: %v", want, info.Capabilities)
		}
	}
}

func TestConformanceFieldsPassTheCoresOwnValidation(t *testing.T) {
	panel := newFakePanel(t)
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	var res extproto.ProvisionFieldsResult
	if err := client.Call(context.Background(), extproto.MethodProvisionerFields,
		&extproto.ProvisionFieldsParams{}, &res); err != nil {
		t.Fatalf("provisioner.fields: %v", err)
	}
	if len(res.Fields) == 0 {
		t.Fatal("a declaration with no fields would make the purchase form unsubmittable")
	}
	// The core drops a field that fails Valid(), so a declaration that only "looks" right would
	// silently lose a required input. This asserts the core keeps every one.
	for _, f := range res.Fields {
		if !f.Valid() {
			t.Fatalf("field %q would be dropped by the core: %+v", f.Key, f)
		}
	}

	byKey := map[string]extproto.ProvisionField{}
	for _, f := range res.Fields {
		byKey[f.Key] = f
	}
	// The two placements that decide where a server runs must be present and must carry the panel's
	// own options: a dropdown nobody filled is a field an operator cannot answer.
	for _, key := range []string{fieldLocation, fieldTemplate} {
		f, ok := byKey[key]
		if !ok {
			t.Fatalf("the declaration is missing %q", key)
		}
		if f.Type != extproto.FieldDropdown || len(f.Choices) == 0 {
			t.Fatalf("%q should be a populated dropdown, got %s %v", key, f.Type, f.Choices)
		}
	}
}

func TestConformanceResourceReturnsTheCoreStruct(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{
		ID: "srv-1", Name: "Minecraft Server", Status: "running",
		Connection: &panelConnection{Host: "play.example.com", Port: 25565},
	})
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	var res extproto.ProvisionResourceResult
	if err := client.Call(context.Background(), extproto.MethodProvisionerResource,
		&extproto.ProvisionResourceParams{ExternalID: "srv-1"}, &res); err != nil {
		t.Fatalf("provisioner.resource: %v", err)
	}
	if res.Name != "Minecraft Server" {
		t.Fatalf("the name should cross the pipe, got %q", res.Name)
	}
	if got := res.Address(); got != "play.example.com:25565" {
		t.Fatalf("the address should render, got %q", got)
	}
	if res.PanelURL == "" {
		t.Fatal("the deep link should be present")
	}
	if res.Stats == nil || res.Stats.CPUPercent == 0 {
		t.Fatalf("metrics should cross the pipe, got %+v", res.Stats)
	}
}

func TestConformancePowerUsesTheCoresParams(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "stopped"})
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	var res extproto.ProvisionPowerResult
	err := client.Call(context.Background(), extproto.MethodProvisionerPower,
		&extproto.ProvisionPowerParams{
			MutatingMeta: extproto.MutatingMeta{OperationKey: "power:svc-1:start:conformance"},
			ExternalID:   "srv-1",
			Action:       extproto.PowerStart,
		}, &res)
	if err != nil {
		t.Fatalf("provisioner.power: %v", err)
	}
	if res.Action != extproto.PowerStart || !res.Changed {
		t.Fatalf("expected a changed start, got %+v", res)
	}
	if n := panel.callCount("POST", "/servers/srv-1/start"); n != 1 {
		t.Fatalf("expected one start on the panel, got %d", n)
	}
}

func TestConformanceCreateRejectsBadParamsBeforeCallingThePanel(t *testing.T) {
	panel := newFakePanel(t)
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	// A mutating call with no operation key is refused by the core's own client before it reaches the
	// adapter, which is the property that makes every replay safe.
	var res extproto.ProvisionCreateResult
	err := client.Call(context.Background(), extproto.MethodProvisionerCreate,
		&extproto.ProvisionCreateParams{PlanID: "x"}, &res)
	if err == nil {
		t.Fatal("a create with no operation key must be refused")
	}
	if !strings.Contains(err.Error(), "operation_key") {
		t.Fatalf("the refusal should name the missing key, got %q", err.Error())
	}
	if n := panel.callCount("POST", "/servers"); n != 0 {
		t.Fatalf("the panel must not be called, it was called %d times", n)
	}
}

func TestConformanceAnUnadvertisedMethodIsNotFound(t *testing.T) {
	panel := newFakePanel(t)
	client := launchProcess(t, conformanceEnv(t, panel.start())...)

	// The client lets this through (the method is not a core method), so the adapter's own dispatch is
	// what has to answer "I do not implement that" instead of hanging or crashing.
	err := client.Call(context.Background(), "provisioner.reinstall", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("an unimplemented method must fail")
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("expected method not found, got %q", err.Error())
	}
}

func TestConformanceMissingCredentialsFailAtStartup(t *testing.T) {
	// A launched adapter with no key must refuse to start rather than answering the handshake and
	// failing every operation afterwards. The reason is on stderr, which is where an operator looks:
	// the core's own error can only say "the extension closed stdout".
	_, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             conformanceBin,
		Env:              []string{"CCTL_PANEL_URL=https://panel.example.com"},
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err == nil {
		t.Fatal("an adapter with no API key must not start")
	}

	// Run it directly to read the message the operator gets.
	cmd := exec.Command(conformanceBin)
	cmd.Env = []string{"CCTL_PANEL_URL=https://panel.example.com", "PATH=" + os.Getenv("PATH")}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatal("expected a non-zero exit")
	}
	if !strings.Contains(string(out), "CCTL_API_KEY") {
		t.Fatalf("the startup message should name the missing variable, got %q", string(out))
	}
}
