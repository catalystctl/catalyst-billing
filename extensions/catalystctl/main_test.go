package main

// What the adapter must get right, and why each test exists.
//
// The three properties that cost money when they are wrong:
//
//  1. **A create is never repeated.** A retried purchase must not buy a second server, so `lookup`
//     has to be authoritative and `create` has to replay from the ledger.
//  2. **Placement is explicit.** A server lands where the service said, and a request that cannot
//     say where is refused rather than guessed.
//  3. **A power action reports what happened.** "Started" for a server that was already running is a
//     lie that hides a broken button; "restarted" for a request that never reached the panel is worse.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/extproto"
)

// testServer builds an adapter pointed at a fake panel.
func testServer(t *testing.T, panel *fakePanel, tweak ...func(*config)) *server {
	t.Helper()
	cfg := config{
		panelURL:  panel.start(),
		apiKey:    "catalyst_test",
		timeout:   defaultTestTimeout,
		stateFile: filepath.Join(t.TempDir(), "operations.json"),
		verifyTLS: true,
		panelPath: "/servers/{id}",
		plansByID: map[string]plan{},
	}
	for _, apply := range tweak {
		apply(&cfg)
	}
	return &server{client: newPanelClient(cfg), cfg: cfg, ledger: newLedger(cfg.stateFile), byKey: map[string]any{}}
}

// call drives one method the way the protocol does.
func call(t *testing.T, s *server, method string, params any) reply {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	return s.handle(&request{JSONRPC: "2.0", Method: method, Params: raw})
}

// result decodes a successful reply into out.
func result(t *testing.T, rep reply, out any) {
	t.Helper()
	if rep.rpcErr != nil {
		t.Fatalf("expected success, got error %d: %s", rep.rpcErr.Code, rep.rpcErr.Message)
	}
	raw, err := json.Marshal(rep.result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
}

// expectError asserts an error reply with a specific JSON-RPC code.
func expectError(t *testing.T, rep reply, code int) {
	t.Helper()
	if rep.rpcErr == nil {
		t.Fatalf("expected error %d, got success: %#v", code, rep.result)
	}
	if rep.rpcErr.Code != code {
		t.Fatalf("expected error %d, got %d: %s", code, rep.rpcErr.Code, rep.rpcErr.Message)
	}
}

// ------------------------------------------------------------------- config --

func TestConfigRequiresOnlyTheCredentials(t *testing.T) {
	// The whole point of the split: a catalogue of products shares one adapter deployment, so the
	// template, location and node are not startup requirements.
	cfg, err := configFromEnv([]string{
		"CCTL_PANEL_URL=https://panel.example.com/",
		"CCTL_API_KEY=catalyst_abc",
	})
	if err != nil {
		t.Fatalf("expected a credential-only configuration to start: %v", err)
	}
	if cfg.panelURL != "https://panel.example.com" {
		t.Fatalf("the trailing slash should be trimmed, got %q", cfg.panelURL)
	}
	if cfg.panelPath != "/servers/{id}" {
		t.Fatalf("the default panel path should be /servers/{id}, got %q", cfg.panelPath)
	}
}

func TestConfigRefusals(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "no panel url",
			env:  []string{"CCTL_API_KEY=catalyst_abc"},
			want: "CCTL_PANEL_URL",
		},
		{
			name: "no api key",
			env:  []string{"CCTL_PANEL_URL=https://panel.example.com"},
			want: "CCTL_API_KEY",
		},
		{
			name: "a client-looking key",
			env: []string{
				"CCTL_PANEL_URL=https://panel.example.com",
				"CCTL_API_KEY=ptla_abc",
			},
			want: "catalyst_",
		},
		{
			name: "a bad network mode",
			env: []string{
				"CCTL_PANEL_URL=https://panel.example.com",
				"CCTL_API_KEY=catalyst_abc",
				"CCTL_NETWORK_MODE=overlay",
			},
			want: "CCTL_NETWORK_MODE",
		},
		{
			name: "a bad plan",
			env: []string{
				"CCTL_PANEL_URL=https://panel.example.com",
				"CCTL_API_KEY=catalyst_abc",
				"CCTL_PLANS=small:100:100:1",
			},
			want: "at least 512 MB",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configFromEnv(tc.env)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// ------------------------------------------------------------------- fields --

func TestFieldsAreFilledFromThePanel(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-9", Name: "existing", Status: "stopped"})
	s := testServer(t, panel)

	var out struct {
		Fields []struct {
			Key      string   `json:"key"`
			Type     string   `json:"type"`
			Required bool     `json:"required"`
			Choices  []string `json:"choices"`
		} `json:"fields"`
	}
	result(t, call(t, s, "provisioner.fields", map[string]any{}), &out)

	byKey := map[string]int{}
	for i, f := range out.Fields {
		byKey[f.Key] = i
	}
	for _, want := range []string{fieldLocation, fieldNode, fieldTemplate, fieldMemoryMB, fieldDiskMB, fieldCPUCores} {
		if _, ok := byKey[want]; !ok {
			t.Fatalf("the declaration is missing %q", want)
		}
	}

	location := out.Fields[byKey[fieldLocation]]
	if location.Type != string(extproto.FieldDropdown) {
		t.Fatalf("location should be a dropdown, got %q", location.Type)
	}
	if len(location.Choices) != 1 || location.Choices[0] != "US East 1" {
		t.Fatalf("location choices should come from the panel, got %v", location.Choices)
	}
	if !location.Required {
		t.Fatal("location must be required: a create cannot happen without one")
	}

	node := out.Fields[byKey[fieldNode]]
	if node.Required {
		t.Fatal("node must not be required: empty means load-balanced")
	}
	if len(node.Choices) == 0 || node.Choices[0] != autoNode {
		t.Fatalf("the first node choice should be %q, got %v", autoNode, node.Choices)
	}

	template := out.Fields[byKey[fieldTemplate]]
	if len(template.Choices) != 1 || template.Choices[0] != "Paper" {
		t.Fatalf("template choices should come from the panel, got %v", template.Choices)
	}
}

func TestFieldsDegradeToTextWhenThePanelIsDown(t *testing.T) {
	panel := newFakePanel(t)
	panel.unauthorized = true
	s := testServer(t, panel)

	var out struct {
		Fields []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"fields"`
	}
	result(t, call(t, s, "provisioner.fields", map[string]any{}), &out)

	for _, f := range out.Fields {
		if f.Key == fieldLocation || f.Key == fieldTemplate {
			if f.Type != string(extproto.FieldText) {
				t.Fatalf("%s should fall back to text when the panel cannot be listed, got %q", f.Key, f.Type)
			}
		}
	}
	if len(out.Fields) == 0 {
		t.Fatal("a declaration with no fields would make the form unsubmittable")
	}
}

// ----------------------------------------------------------------- placement --

func TestPlacementPrefersTheServiceFieldsOverProductOptions(t *testing.T) {
	panel := newFakePanel(t)
	panel.locations = append(panel.locations, panelLocation{ID: "loc-2", Name: "EU West"})
	s := testServer(t, panel)

	place, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation:  "US East 1", // the service's own field
		"env.LOCATION": "EU West",   // the product's config option
		fieldTemplate:  "Paper",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.LocationID != "loc-1" {
		t.Fatalf("the service field should win over the product option, got %q", place.LocationID)
	}
	if place.TemplateID != "tpl-paper" {
		t.Fatalf("template should resolve by name, got %q", place.TemplateID)
	}
}

func TestPlacementAcceptsProductConfigOptions(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	// A product can declare a config option whose environment variable is LOCATION; every service
	// bought from it inherits the placement with no operator action.
	place, err := s.resolvePlacement(t.Context(), map[string]string{
		"env.location":  "us east 1", // case-insensitive both sides
		"env.TEMPLATE":  "tpl-paper", // by id
		"env.MEMORY_MB": "4096",
		"env.DISK_MB":   "20480",
		"env.CPU_CORES": "2",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.LocationID != "loc-1" {
		t.Fatalf("location should resolve from the product option, got %q", place.LocationID)
	}
	if place.MemoryMB != 4096 || place.DiskMB != 20480 || place.CPUCores != 2 {
		t.Fatalf("sizes should come from the product options, got %d/%d/%d",
			place.MemoryMB, place.DiskMB, place.CPUCores)
	}
}

func TestPlacementAcceptsTheRenderedChoiceForm(t *testing.T) {
	panel := newFakePanel(t)
	// Two locations with the same name: the rendered form is what disambiguates them, and picking
	// one silently would be a coin flip.
	panel.locations = []panelLocation{
		{ID: "loc-1", Name: "US East"},
		{ID: "loc-2", Name: "US East"},
	}
	panel.nodes = append(panel.nodes, panelNode{ID: "node-2", Name: "node two", LocationID: "loc-2", IsOnline: true})
	panel.nodeStats["node-2"] = panelNodeStats{NodeID: "node-2", Resources: struct {
		MaxMemoryMb       int64 `json:"maxMemoryMb"`
		MaxCpuCores       int64 `json:"maxCpuCores"`
		AllocatedMemoryMb int64 `json:"allocatedMemoryMb"`
		AllocatedCpuCores int64 `json:"allocatedCpuCores"`
	}{MaxMemoryMb: 16384, MaxCpuCores: 8}}
	s := testServer(t, panel)

	place, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East (loc-2)",
		fieldTemplate: "Paper",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.LocationID != "loc-2" {
		t.Fatalf("the id in parentheses should be authoritative, got %q", place.LocationID)
	}
}

func TestPlacementRefusesAnUnknownLocation(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	_, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "Mars",
		fieldTemplate: "Paper",
	})
	if err == nil {
		t.Fatal("an unknown location must be refused, not guessed")
	}
	if !strings.Contains(err.Error(), "US East 1") {
		t.Fatalf("the refusal should list the known locations, got %q", err.Error())
	}
}

func TestPlacementLoadBalancesAcrossOnlineNodes(t *testing.T) {
	panel := newFakePanel(t)
	// A second node with more free memory. The busy one must not be chosen.
	panel.nodes = append(panel.nodes, panelNode{
		ID: "node-2", Name: "node two", LocationID: "loc-1",
		PublicAddress: "10.0.0.2", IsOnline: true, MaxMemoryMb: 32768, MaxCpuCores: 16,
	})
	panel.nodeStats["node-2"] = panelNodeStats{NodeID: "node-2", IsOnline: true, Resources: struct {
		MaxMemoryMb       int64 `json:"maxMemoryMb"`
		MaxCpuCores       int64 `json:"maxCpuCores"`
		AllocatedMemoryMb int64 `json:"allocatedMemoryMb"`
		AllocatedCpuCores int64 `json:"allocatedCpuCores"`
	}{MaxMemoryMb: 32768, MaxCpuCores: 16, AllocatedMemoryMb: 2048, AllocatedCpuCores: 2}}
	s := testServer(t, panel)

	place, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East 1",
		fieldTemplate: "Paper",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.NodeID != "node-2" {
		t.Fatalf("load balancing should pick the node with more free memory, got %q", place.NodeID)
	}
}

func TestPlacementSkipsOfflineNodes(t *testing.T) {
	panel := newFakePanel(t)
	panel.nodes = append(panel.nodes, panelNode{
		ID: "node-2", Name: "node two", LocationID: "loc-1",
		IsOnline: false, MaxMemoryMb: 65536, MaxCpuCores: 32,
	})
	panel.nodeStats["node-2"] = panelNodeStats{NodeID: "node-2", Resources: struct {
		MaxMemoryMb       int64 `json:"maxMemoryMb"`
		MaxCpuCores       int64 `json:"maxCpuCores"`
		AllocatedMemoryMb int64 `json:"allocatedMemoryMb"`
		AllocatedCpuCores int64 `json:"allocatedCpuCores"`
	}{MaxMemoryMb: 65536, MaxCpuCores: 32}}
	s := testServer(t, panel)

	place, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East 1",
		fieldTemplate: "Paper",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.NodeID != "node-1" {
		t.Fatalf("an offline node must never receive a server, got %q", place.NodeID)
	}
}

func TestPlacementRefusesWhenNoNodeHasCapacity(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	_, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East 1",
		fieldTemplate: "Paper",
		fieldMemoryMB: "65536", // more than the node has
	})
	if err == nil {
		t.Fatal("a purchase that cannot fit must be refused")
	}
	if !strings.Contains(err.Error(), "65536") {
		t.Fatalf("the refusal should name the size that did not fit, got %q", err.Error())
	}
}

func TestPlacementRefusesANodeFromAnotherLocation(t *testing.T) {
	panel := newFakePanel(t)
	panel.locations = append(panel.locations, panelLocation{ID: "loc-2", Name: "EU West"})
	panel.nodes = append(panel.nodes, panelNode{ID: "node-eu", Name: "eu one", LocationID: "loc-2", IsOnline: true})
	s := testServer(t, panel)

	_, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East 1",
		fieldNode:     "eu one",
		fieldTemplate: "Paper",
	})
	if err == nil {
		t.Fatal("pinning a node outside the chosen location must be refused")
	}
	if !strings.Contains(err.Error(), "US East 1") {
		t.Fatalf("the refusal should name the location the operator picked, got %q", err.Error())
	}
}

func TestPlacementPinsANamedNode(t *testing.T) {
	panel := newFakePanel(t)
	panel.nodes = append(panel.nodes, panelNode{
		ID: "node-2", Name: "node two", LocationID: "loc-1", IsOnline: true, MaxMemoryMb: 32768, MaxCpuCores: 16,
	})
	s := testServer(t, panel)

	place, err := s.resolvePlacement(t.Context(), map[string]string{
		fieldLocation: "US East 1",
		fieldNode:     "node one",
		fieldTemplate: "Paper",
	})
	if err != nil {
		t.Fatalf("resolve placement: %v", err)
	}
	if place.NodeID != "node-1" {
		t.Fatalf("a pinned node must be honoured, got %q", place.NodeID)
	}
}

// -------------------------------------------------------------------- create --

// purchase is the metadata a service sends to create.
func purchase(extra map[string]string) map[string]string {
	out := map[string]string{
		"service_id":  "svc-1",
		"customer_id": "cus-1",
		fieldLocation: "US East 1",
		fieldTemplate: "Paper",
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestCreateUsesTheServicePlacement(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	var out struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
	}
	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"plan_id":       "price-version-42",
		"metadata":      purchase(map[string]string{fieldMemoryMB: "4096"}),
	}), &out)

	if out.ExternalID != "srv-1" {
		t.Fatalf("expected the panel's server id, got %q", out.ExternalID)
	}
	if len(panel.created) != 1 {
		t.Fatalf("expected one create, got %d", len(panel.created))
	}
	sent := panel.created[0]
	if sent.TemplateID != "tpl-paper" || sent.NodeID != "node-1" || sent.LocationID != "loc-1" {
		t.Fatalf("wrong placement sent: %+v", sent)
	}
	if sent.AllocatedMemoryMb != 4096 {
		t.Fatalf("the service's memory field should size the server, got %d", sent.AllocatedMemoryMb)
	}
	if sent.AllocationID != "alloc-1" || sent.PrimaryPort != 25565 {
		t.Fatalf("a free allocation should be claimed, got %q:%d", sent.AllocationID, sent.PrimaryPort)
	}
	if sent.Description != "billing:create:svc-1:1" {
		t.Fatalf("the description marker is what makes lookup survive a lost ledger, got %q", sent.Description)
	}
	if sent.Name != "Server svc-1" {
		t.Fatalf("the name should default from the service id, got %q", sent.Name)
	}
}

func TestCreatePassesProductOptionsAsTheServerEnvironment(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata": purchase(map[string]string{
			"env.MINECRAFT_VERSION": "1.21.4",
			// Placement options are consumed, not forwarded: a game process has no use for a
			// location id and an operator did not ask for it there.
			"env.LOCATION": "US East 1",
		}),
	}), &struct{}{})

	sent := panel.created[0]
	if sent.Environment["MINECRAFT_VERSION"] != "1.21.4" {
		t.Fatalf("a product's env option should reach the server, got %v", sent.Environment)
	}
	if _, ok := sent.Environment["LOCATION"]; ok {
		t.Fatalf("placement options must not leak into the game environment: %v", sent.Environment)
	}
}

func TestCreateReplaysFromTheLedgerWithoutBuyingASecondServer(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	first := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	})
	result(t, first, &struct{}{})

	// The same key again, as a retry after a lost reply.
	second := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	})
	var out struct {
		ExternalID string `json:"external_id"`
	}
	result(t, second, &out)

	if n := panel.callCount("POST", "/servers"); n != 1 {
		t.Fatalf("a replay must not create a second server, panel saw %d creates", n)
	}
	if out.ExternalID != "srv-1" {
		t.Fatalf("a replay must return the first server, got %q", out.ExternalID)
	}
}

func TestCreateRecordsIntentBeforeCallingThePanel(t *testing.T) {
	panel := newFakePanel(t)
	panel.failCreateStatus = 500
	panel.failCreateCode = "SERVER_CREATE_FAILED"
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	})
	expectError(t, rep, codeInternal)

	// The intent survives, so a lookup after a crash can still ask the panel whether the server
	// exists rather than concluding it does not.
	rec, found, err := s.ledger.get("create:svc-1:1")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !found || rec.State != recordCreating {
		t.Fatalf("expected a 'creating' record, got %+v (found=%t)", rec, found)
	}
}

func TestCreateReportsARefusalAsARejection(t *testing.T) {
	panel := newFakePanel(t)
	panel.failCreateStatus = 409
	panel.failCreateCode = "PORT_ALREADY_IN_USE"
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	})
	// Retrying a refusal unchanged cannot help, so it must not be a retryable code.
	expectError(t, rep, codeRejected)
}

func TestCreateRefusesAPurchaseWithNoPlacement(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      map[string]string{"service_id": "svc-1"},
	})
	expectError(t, rep, codeRejected)
	if n := panel.callCount("POST", "/servers"); n != 0 {
		t.Fatalf("a purchase with no location must not reach the panel, it made %d calls", n)
	}
}

func TestCreateClaimsASpecificPortWhenAsked(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{fieldPort: "25566"}),
	}), &struct{}{})

	if panel.created[0].PrimaryPort != 25566 || panel.created[0].AllocationID != "alloc-2" {
		t.Fatalf("the requested port should be claimed, got %q port %d",
			panel.created[0].AllocationID, panel.created[0].PrimaryPort)
	}
}

func TestCreateRefusesAPortThatIsTaken(t *testing.T) {
	panel := newFakePanel(t)
	// alloc-2 is already assigned to a server.
	taken := "srv-9"
	panel.allocations["node-1"][1].ServerID = &taken
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{fieldPort: "25566"}),
	})
	expectError(t, rep, codeRejected)
}

// -------------------------------------------------------------------- lookup --

func TestLookupFindsAServerByItsDescriptionMarker(t *testing.T) {
	panel := newFakePanel(t)
	// A server created by an earlier process: the ledger knows nothing about it.
	found := "billing:create:svc-7:1"
	panel.addServer(panelServer{ID: "srv-77", Name: "old", Status: "running", Description: &found})
	s := testServer(t, panel)

	var out struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
	}
	result(t, call(t, s, "provisioner.lookup", map[string]any{
		"operation_key": "create:svc-7:1",
	}), &out)

	if !out.Found || out.ExternalID != "srv-77" {
		t.Fatalf("a restart must not lose the association, got %+v", out)
	}
	// The scan is the expensive path; it must re-record so the next lookup is a single read.
	if _, ok, _ := s.ledger.get("create:svc-7:1"); !ok {
		t.Fatal("a found server should be recorded so the next lookup is cheap")
	}
}

func TestLookupReportsNotFoundOnlyAfterAScan(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "other", Status: "running"})
	s := testServer(t, panel)

	var out struct {
		Found bool `json:"found"`
	}
	result(t, call(t, s, "provisioner.lookup", map[string]any{
		"operation_key": "create:svc-7:1",
	}), &out)
	if out.Found {
		t.Fatal("nothing exists for this key, so found must be false")
	}
}

func TestLookupRefusesToGuessWhenTheScanIsTruncated(t *testing.T) {
	panel := newFakePanel(t)
	// A panel that always returns a full page, forever. The adapter's page bound must stop it and
	// report an error: "not found" is authoritative and would provision a duplicate.
	panel.repeatsPage = true
	panel.addServer(panelServer{ID: "srv-1", Name: "a", Status: "running"})
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.lookup", map[string]any{"operation_key": "create:svc-7:1"})
	if rep.rpcErr == nil {
		t.Fatalf("an incomplete scan must not answer found=false, got %#v", rep.result)
	}
	if rep.rpcErr.Code != codeInternal {
		t.Fatalf("an incomplete scan is retryable, expected %d got %d", codeInternal, rep.rpcErr.Code)
	}
}

func TestLookupVerifiesARecordedServerStillExists(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)
	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	}), &struct{}{})

	// Somebody deleted the server in the panel's own UI. The ledger still names it, but a lookup
	// must not claim it exists.
	panel.mu.Lock()
	delete(panel.servers, "srv-1")
	panel.mu.Unlock()

	var out struct {
		Found bool `json:"found"`
	}
	result(t, call(t, s, "provisioner.lookup", map[string]any{
		"operation_key": "create:svc-1:1",
	}), &out)
	if out.Found {
		t.Fatal("a server deleted by hand must not be reported as found")
	}
}

// --------------------------------------------------------------------- power --

func TestPowerStartsAStoppedServer(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "stopped"})
	s := testServer(t, panel)

	var out struct {
		Action  string `json:"action"`
		State   string `json:"state"`
		Changed bool   `json:"changed"`
	}
	result(t, call(t, s, "provisioner.power", map[string]any{
		"operation_key": "power:svc-1:start:abc",
		"external_id":   "srv-1",
		"action":        "start",
	}), &out)

	if n := panel.callCount("POST", "/servers/srv-1/start"); n != 1 {
		t.Fatalf("expected one start call, got %d", n)
	}
	if !out.Changed || out.State != "starting" {
		t.Fatalf("expected starting/changed, got %+v", out)
	}
}

func TestPowerDoesNotStartARunningServerAgain(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "running"})
	s := testServer(t, panel)

	var out struct {
		Changed bool `json:"changed"`
	}
	result(t, call(t, s, "provisioner.power", map[string]any{
		"operation_key": "power:svc-1:start:abc",
		"external_id":   "srv-1",
		"action":        "start",
	}), &out)

	if n := panel.callCount("POST", "/servers/srv-1/start"); n != 0 {
		t.Fatalf("starting a running server must not call the panel, it called %d times", n)
	}
	if out.Changed {
		t.Fatal("nothing changed, so changed must be false")
	}
}

func TestPowerIsIdempotentByOperationKey(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "running"})
	s := testServer(t, panel)

	params := map[string]any{
		"operation_key": "power:svc-1:restart:abc",
		"external_id":   "srv-1",
		"action":        "restart",
	}
	result(t, call(t, s, "provisioner.power", params), &struct{}{})
	result(t, call(t, s, "provisioner.power", params), &struct{}{})

	if n := panel.callCount("POST", "/servers/srv-1/restart"); n != 1 {
		t.Fatalf("a replayed press must not restart twice, it restarted %d times", n)
	}
}

func TestPowerRefusesASuspendedServer(t *testing.T) {
	panel := newFakePanel(t)
	stamp := "2026-10-16T00:00:00Z"
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "stopped", SuspendedAt: &stamp})
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.power", map[string]any{
		"operation_key": "power:svc-1:start:abc",
		"external_id":   "srv-1",
		"action":        "start",
	})
	// A suspension is a policy decision: retrying will not help, so it is a rejection with a
	// sentence rather than a retryable failure.
	expectError(t, rep, codeRejected)
}

func TestPowerRefusesAnUnknownAction(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "running"})
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.power", map[string]any{
		"operation_key": "power:svc-1:nuke:abc",
		"external_id":   "srv-1",
		"action":        "nuke",
	})
	expectError(t, rep, codeInvalidParams)
}

// ------------------------------------------------------------------ resource --

func TestResourceReportsTheLiveState(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{
		ID: "srv-1", Name: "Minecraft Server", Status: "running",
		AllocatedMemoryMb: 1024, AllocatedDiskMb: 10240,
		Connection: &panelConnection{Host: "play.example.com", Port: 25565},
	})
	s := testServer(t, panel)

	var out struct {
		Name     string   `json:"name"`
		State    string   `json:"state"`
		Host     string   `json:"host"`
		Port     int32    `json:"port"`
		PanelURL string   `json:"panel_url"`
		CanPower []string `json:"can_power"`
		Stats    *struct {
			CPUPercent    float64 `json:"cpu_percent"`
			MemoryUsedMB  int64   `json:"memory_used_mb"`
			MemoryLimitMB int64   `json:"memory_limit_mb"`
			NetworkRx     int64   `json:"network_rx_bytes"`
		} `json:"stats"`
	}
	result(t, call(t, s, "provisioner.resource", map[string]any{"external_id": "srv-1"}), &out)

	if out.Name != "Minecraft Server" || out.State != "running" {
		t.Fatalf("unexpected name/state: %q/%q", out.Name, out.State)
	}
	if out.Host != "play.example.com" || out.Port != 25565 {
		t.Fatalf("the address should come from the panel's connection block, got %q:%d", out.Host, out.Port)
	}
	if !strings.HasSuffix(out.PanelURL, "/servers/srv-1") {
		t.Fatalf("the deep link should point at the server, got %q", out.PanelURL)
	}
	// A running server offers stop/restart/kill, never start.
	joined := strings.Join(out.CanPower, ",")
	if joined != "stop,restart,kill" {
		t.Fatalf("a running server should offer stop,restart,kill, got %q", joined)
	}
	if out.Stats == nil {
		t.Fatal("a running server should report metrics")
	}
	if out.Stats.CPUPercent != 12.5 || out.Stats.MemoryUsedMB != 512 || out.Stats.MemoryLimitMB != 1024 {
		t.Fatalf("unexpected metrics: %+v", out.Stats)
	}
	// The panel reports byte counts as strings because they can exceed a JS integer.
	if out.Stats.NetworkRx != 1234 {
		t.Fatalf("a string byte count should decode, got %d", out.Stats.NetworkRx)
	}
}

func TestResourceOmitsMetricsThePanelDoesNotHave(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "stopped"})
	s := testServer(t, panel)

	var out map[string]any
	result(t, call(t, s, "provisioner.resource", map[string]any{"external_id": "srv-1"}), &out)

	// "No metrics available yet" must not become a page full of zeroes.
	if _, ok := out["stats"]; ok {
		t.Fatalf("a stopped server has no sample, so stats must be absent: %v", out["stats"])
	}
	if out["can_power"] == nil {
		t.Fatal("a stopped server must still report that it can be started")
	}
}

func TestResourceMapsAMissingServerToNotFound(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	rep := call(t, s, "provisioner.resource", map[string]any{"external_id": "nope"})
	// -32004 lets the core conclude the resource is gone; a retryable code would not.
	expectError(t, rep, codeNotFound)
}

// ------------------------------------------------------------------- suspend --

func TestSuspendAndResumeAreIdempotent(t *testing.T) {
	panel := newFakePanel(t)
	panel.addServer(panelServer{ID: "srv-1", Name: "one", Status: "running"})
	s := testServer(t, panel)

	var out struct {
		Changed bool `json:"changed"`
	}
	result(t, call(t, s, "provisioner.suspend", map[string]any{
		"operation_key": "suspend:srv-1", "external_id": "srv-1", "reason": "unpaid",
	}), &out)
	if !out.Changed {
		t.Fatal("the first suspend should report a change")
	}

	// Suspending again is success with changed=false: the requested end state is what was asked.
	result(t, call(t, s, "provisioner.suspend", map[string]any{
		"operation_key": "suspend:srv-1", "external_id": "srv-1", "reason": "unpaid",
	}), &out)
	if out.Changed {
		t.Fatal("a second suspend should report no change")
	}
	if n := panel.callCount("POST", "/servers/srv-1/suspend"); n != 1 {
		t.Fatalf("the panel should have been asked once, it was asked %d times", n)
	}
}

func TestTerminateSucceedsWhenTheServerIsAlreadyGone(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	var out struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	// Destructive operations must converge on the end state, not on "I found something to delete".
	result(t, call(t, s, "provisioner.terminate", map[string]any{
		"operation_key": "terminate:srv-9", "external_id": "srv-9",
	}), &out)
	if out.Status != string(resourceTerminated) || out.Changed {
		t.Fatalf("expected terminated/changed=false, got %+v", out)
	}
}

// ------------------------------------------------------------------- ledger --

func TestTheLedgerSurvivesAProcessRestart(t *testing.T) {
	panel := newFakePanel(t)
	path := filepath.Join(t.TempDir(), "operations.json")
	makeServer := func() *server {
		return &server{
			client: newPanelClient(config{panelURL: panel.start(), apiKey: "catalyst_test",
				timeout: defaultTestTimeout, verifyTLS: true, panelPath: "/servers/{id}"}),
			cfg:    config{panelURL: panel.start(), apiKey: "catalyst_test", panelPath: "/servers/{id}"},
			ledger: newLedger(path), byKey: map[string]any{},
		}
	}

	result(t, call(t, makeServer(), "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	}), &struct{}{})

	// A new process, same ledger file: the association must still be known without a scan.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the ledger should have been written: %v", err)
	}
	var out struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
	}
	result(t, call(t, makeServer(), "provisioner.lookup", map[string]any{
		"operation_key": "create:svc-1:1",
	}), &out)
	if !out.Found || out.ExternalID != "srv-1" {
		t.Fatalf("a restart must not lose the association, got %+v", out)
	}
}

func TestACorruptLedgerIsRefusedRatherThanIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	l := newLedger(path)

	// An empty ledger silently turns "I already created this" into "create another one", so a
	// corrupt one must be an error on every call.
	if _, _, err := l.get("anything"); err == nil {
		t.Fatal("a corrupt ledger must not read as empty")
	}
	if err := l.put(operationRecord{OperationKey: "k", State: recordCreating}); err == nil {
		t.Fatal("a corrupt ledger must not accept writes over the top")
	}
}

// ------------------------------------------------------------- panel accounts --

func TestCreateGivesTheServerToTheCustomersPanelAccount(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata": purchase(map[string]string{
			"customer_email": "Alice@Example.test",
			"customer_name":  "Alice",
		}),
	}), &struct{}{})

	// An account is created for the customer, and the server is owned by it. Without this the server
	// would live in the API key's own account and the console link we show would not open it.
	if len(panel.userCreates) != 1 {
		t.Fatalf("expected one panel account, got %d", len(panel.userCreates))
	}
	created := panel.userCreates[0]
	if created.Email != "alice@example.test" {
		t.Fatalf("the account should use the customer's address, got %q", created.Email)
	}
	if created.Username != "alice" {
		t.Fatalf("username = %q, want alice", created.Username)
	}
	// The password must be unguessable and never the customer's own: billing has no password to give.
	if len(created.Password) < 20 {
		t.Fatalf("the generated password is too short to be unguessable (%d chars)", len(created.Password))
	}
	if panel.created[0].OwnerID == "" {
		t.Fatal("the server was created with no owner")
	}
	if panel.created[0].OwnerID != "user-2" {
		t.Fatalf("owner = %q, want the new customer account", panel.created[0].OwnerID)
	}
}

func TestASecondServiceReusesTheCustomersPanelAccount(t *testing.T) {
	panel := newFakePanel(t)
	// The account already exists, as it would after the first purchase or a manual invite.
	panel.users["user-alice"] = panelUser{ID: "user-alice", Email: "alice@example.test", Username: "alice"}
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{"customer_email": "alice@example.test"}),
	}), &struct{}{})
	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-2:1",
		"metadata":      purchase(map[string]string{"customer_email": "ALICE@example.test"}),
	}), &struct{}{})

	if len(panel.userCreates) != 0 {
		t.Fatalf("an existing account must be reused, it tried to create %d", len(panel.userCreates))
	}
	for i, created := range panel.created {
		if created.OwnerID != "user-alice" {
			t.Fatalf("server %d owner = %q, want user-alice", i, created.OwnerID)
		}
	}
}

func TestAnExplicitOwnerBeatsTheCustomersAccount(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	// A reseller product places servers in a panel account of its own.
	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata": purchase(map[string]string{
			"customer_email": "alice@example.test",
			"env.OWNER_ID":   "user-reseller",
		}),
	}), &struct{}{})

	if len(panel.userCreates) != 0 {
		t.Fatal("an explicit owner must not trigger an account creation")
	}
	if panel.created[0].OwnerID != "user-reseller" {
		t.Fatalf("owner = %q, want user-reseller", panel.created[0].OwnerID)
	}
}

func TestUserModeNoneLeavesOwnershipToThePanel(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel, func(cfg *config) { cfg.userMode = userModeNone })

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{"customer_email": "alice@example.test"}),
	}), &struct{}{})

	if len(panel.userCreates) != 0 {
		t.Fatal("CCTL_USER_MODE=none must not create accounts")
	}
	if panel.created[0].OwnerID != "" {
		t.Fatalf("owner = %q, want none: the panel decides", panel.created[0].OwnerID)
	}
}

func TestAClashingUsernameIsMadeUnique(t *testing.T) {
	panel := newFakePanel(t)
	// Another customer already took "alice".
	panel.users["user-other"] = panelUser{ID: "user-other", Email: "other@example.test", Username: "alice"}
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{"customer_email": "alice@example.test"}),
	}), &struct{}{})

	if len(panel.userCreates) != 1 {
		t.Fatalf("expected one account, got %d", len(panel.userCreates))
	}
	if got := panel.userCreates[0].Username; got == "alice" {
		t.Fatal("a duplicate username would be refused by the panel")
	}
}

func TestAnAccountRaceIsRepairedByReReading(t *testing.T) {
	panel := newFakePanel(t)
	// The list is capped, so the lookup cannot see alice's account, but the create is then refused
	// because it exists: exactly the race two workers produce.
	panel.users["user-alice"] = panelUser{ID: "user-alice", Email: "alice@example.test", Username: "alice"}
	panel.userPageLimit = 1
	s := testServer(t, panel)

	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(map[string]string{"customer_email": "alice@example.test"}),
	}), &struct{}{})

	if panel.created[0].OwnerID != "user-alice" {
		t.Fatalf("owner = %q, want the account the race created", panel.created[0].OwnerID)
	}
}

func TestACustomerWithoutAnAddressStillGetsAServer(t *testing.T) {
	panel := newFakePanel(t)
	s := testServer(t, panel)

	// No customer_email means no person: an import or a back-office fixture. Creating an account for
	// nobody would litter the panel, and refusing would block work that has nothing to do with one.
	result(t, call(t, s, "provisioner.create", map[string]any{
		"operation_key": "create:svc-1:1",
		"metadata":      purchase(nil),
	}), &struct{}{})

	if len(panel.userCreates) != 0 {
		t.Fatal("no address, no account")
	}
	if panel.created[0].OwnerID != "" {
		t.Fatalf("owner = %q, want none", panel.created[0].OwnerID)
	}
}
