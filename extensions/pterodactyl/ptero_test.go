package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The adapter's tests, driven through its own JSON-RPC surface.
//
// Each of these corresponds to a way a provisioner can lose money: a create that
// buys a second server, a lookup that mistakes "I do not know" for "it does not
// exist", a terminate that leaves the data behind, a suspend that reports failure
// because it was already suspended.

func call(t *testing.T, s *server, method string, params any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	id := uint64(1)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if rep.rpcErr != nil {
		t.Fatalf("%s: rpc error %d: %s", method, rep.rpcErr.Code, rep.rpcErr.Message)
	}
	out, err := json.Marshal(rep.result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return out
}

func callErr(t *testing.T, s *server, method string, params any) *rpcError {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	id := uint64(1)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if rep.rpcErr == nil {
		t.Fatalf("%s: expected an error, got %v", method, rep.result)
	}
	return rep.rpcErr
}

func newTestServer(t *testing.T) (*server, *fakePtero) {
	t.Helper()
	f := newFakePtero(t)
	return &server{client: newPteroClient(f.config()), cfg: f.config()}, f
}

func createParams(key, plan string) map[string]any {
	return map[string]any{
		"operation_key":   key,
		"subscription_id": "sub_1",
		"plan_id":         plan,
		"region":          "3",
		"quantity":        1,
		"metadata": map[string]string{
			"pterodactyl_user_id":   "7",
			"name":                  "survival-1",
			"env.MINECRAFT_VERSION": "1.21.1",
			"env.MAX_PLAYERS":       "20",
		},
	}
}

func TestInitializeAdvertisesProvisionerMethods(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "initialize", map[string]any{"protocol": "billing.ext/1", "versions": []int{1}})
	var res struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Name != "pterodactyl" {
		t.Fatalf("name = %q", res.Name)
	}
	want := []string{"provisioner.describe", "provisioner.validate", "provisioner.create",
		"provisioner.inspect", "provisioner.lookup", "provisioner.suspend",
		"provisioner.resume", "provisioner.terminate"}
	if len(res.Capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v", res.Capabilities, want)
	}
}

func TestDescribeReportsConfiguredPlans(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "provisioner.describe", map[string]any{})
	var res struct {
		Name  string `json:"name"`
		Plans []struct {
			ID     string `json:"id"`
			Limits struct {
				MemoryMB int64 `json:"memory_mb"`
				DiskMB   int64 `json:"disk_mb"`
			} `json:"limits"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Plans) != 2 {
		t.Fatalf("plans = %+v, want two", res.Plans)
	}
	if res.Plans[0].ID != "small" || res.Plans[0].Limits.MemoryMB != 2048 {
		t.Fatalf("first plan = %+v", res.Plans[0])
	}
}

// An unknown plan is refused by validate, not guessed at: guessing means creating a
// server of the wrong size and billing for it.
func TestValidateRefusesAnUnknownPlan(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "provisioner.validate", map[string]any{"plan_id": "enormous", "quantity": 1})
	var res struct {
		Valid    bool     `json:"valid"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Valid {
		t.Fatal("an unknown plan was reported as valid")
	}
	if len(res.Problems) == 0 || !strings.Contains(res.Problems[0], "unknown plan") {
		t.Fatalf("problems = %v", res.Problems)
	}
	if f.requestCount() != 0 {
		t.Fatal("validate reached Pterodactyl; it is a pure read of local configuration")
	}
}

func TestValidateAcceptsAKnownPlan(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "provisioner.validate", map[string]any{"plan_id": "medium", "quantity": 1})
	var res struct {
		Valid    bool     `json:"valid"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Valid {
		t.Fatalf("a known plan was refused: %v", res.Problems)
	}
}

// The create request must be shaped the way Pterodactyl requires: the operation key
// in external_id (that is what makes reconciliation survive a restart), the plan's
// limits, the egg, and the config options as environment variables.
func TestCreateSendsPterodactylShapedRequest(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "provisioner.create", createParams("op-key-1", "small"))
	var res struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.ExternalID == "" {
		t.Fatalf("no external id: %+v", res)
	}
	if res.Status != "active" {
		t.Fatalf("status = %q, want active", res.Status)
	}

	req := f.lastRequest()
	if req.Auth != "Bearer ptla_test_key" {
		t.Fatalf("authorization = %q", req.Auth)
	}
	if got := stringField(req.Body, "external_id"); got != "op-key-1" {
		t.Fatalf("external_id = %q, want the operation key", got)
	}
	if got := stringField(req.Body, "egg"); got != "15" {
		t.Fatalf("egg = %q", got)
	}
	if got := stringField(req.Body, "user"); got != "7" {
		t.Fatalf("user = %q", got)
	}
	if got := stringField(req.Body, "name"); got != "survival-1" {
		t.Fatalf("name = %q", got)
	}

	limits, ok := req.Body["limits"].(map[string]any)
	if !ok {
		t.Fatalf("limits missing: %v", req.Body)
	}
	// A JSON number decodes to float64, which is fine here: these are memory sizes
	// and percentages, not money, and the protocol carries them as integers.
	if limits["memory"] != float64(2048) || limits["disk"] != float64(10240) {
		t.Fatalf("limits = %v, want the plan's 2048 MB / 10240 MB", limits)
	}

	// Config options become the egg's environment (UPG-12), and nothing else does.
	env, ok := req.Body["environment"].(map[string]any)
	if !ok {
		t.Fatalf("environment missing: %v", req.Body)
	}
	if env["MINECRAFT_VERSION"] != "1.21.1" || env["MAX_PLAYERS"] != "20" {
		t.Fatalf("environment = %v", env)
	}
	if _, leaked := env["pterodactyl_user_id"]; leaked {
		t.Fatal("panel bookkeeping leaked into the server's environment")
	}
	if _, leaked := env["name"]; leaked {
		t.Fatal("panel bookkeeping leaked into the server's environment")
	}
}

// A replay of the same operation key must return the server that already exists.
// This is the whole point of the external_id: an in-memory map would forget it.
func TestCreateIsIdempotentByOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	first := call(t, s, "provisioner.create", createParams("same-op", "small"))
	before := f.requestCount()
	second := call(t, s, "provisioner.create", createParams("same-op", "small"))

	var a, b struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	if a.ExternalID != b.ExternalID {
		t.Fatalf("a replay created a second server: %q then %q", a.ExternalID, b.ExternalID)
	}
	// The second call must have looked up and returned, not created.
	last := f.lastRequest()
	if last.Method == "POST" && strings.HasSuffix(last.Path, "/servers") {
		t.Fatalf("the replay created a server instead of finding it (requests before: %d)", before)
	}
}

// Lookup must be authoritative and must distinguish "no such resource" from
// "cannot answer". A found=false answer lets the core provision; an error makes it
// wait.
func TestLookupAnswersAuthoritatively(t *testing.T) {
	s, f := newTestServer(t)

	raw := call(t, s, "provisioner.lookup", map[string]any{"operation_key": "never-seen"})
	var missing struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(raw, &missing); err != nil {
		t.Fatal(err)
	}
	if missing.Found {
		t.Fatal("lookup found a server for a key that was never created")
	}

	created := call(t, s, "provisioner.create", createParams("known-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	raw = call(t, s, "provisioner.lookup", map[string]any{"operation_key": "known-op"})
	var found struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if !found.Found || found.ExternalID != c.ExternalID {
		t.Fatalf("lookup = %+v, want the created server", found)
	}

	// Now make Pterodactyl unreachable: the adapter must report an error, never
	// found=false, or the core would provision a duplicate.
	f.failWith(500, `{"errors":{"server":["internal error"]}}`)
	err := callErr(t, s, "provisioner.lookup", map[string]any{"operation_key": "known-op"})
	if err.Code == codeNotFound {
		t.Fatal("an unreachable panel was reported as not-found, so the core would provision a duplicate")
	}
}

// Suspend and resume are idempotent: asking for the state a server is already in
// succeeds with changed=false rather than failing.
func TestSuspendAndResumeAreIdempotent(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("state-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	raw := call(t, s, "provisioner.suspend", map[string]any{"operation_key": "s1", "external_id": c.ExternalID})
	var res struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "suspended" || !res.Changed {
		t.Fatalf("suspend = %+v", res)
	}

	raw = call(t, s, "provisioner.suspend", map[string]any{"operation_key": "s2", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("suspending an already-suspended server reported a change")
	}

	raw = call(t, s, "provisioner.resume", map[string]any{"operation_key": "r1", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "active" || !res.Changed {
		t.Fatalf("resume = %+v", res)
	}
	_ = f
}

// Inspect reports the observed state, including the suspended flag, which is what
// reconciliation compares against.
func TestInspectReportsObservedState(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("inspect-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	raw := call(t, s, "provisioner.inspect", map[string]any{"external_id": c.ExternalID})
	var res struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "active" {
		t.Fatalf("status = %q, want active", res.Status)
	}

	f.suspend(1)
	raw = call(t, s, "provisioner.inspect", map[string]any{"external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "suspended" {
		t.Fatalf("status = %q, want suspended after the panel suspended it", res.Status)
	}
}

// Terminate destroys the server and its files, and terminating one that is already
// gone is the requested end state rather than an error the core would retry forever.
func TestTerminateIsForcefulAndIdempotent(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("term-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	raw := call(t, s, "provisioner.terminate", map[string]any{"operation_key": "t1", "external_id": c.ExternalID})
	var res struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "terminated" || !res.Changed {
		t.Fatalf("terminate = %+v", res)
	}
	// force=true is what removes the files; without it the data stays and the disk
	// stays billable after the customer asked for it gone.
	if !strings.Contains(f.lastRequest().Query, "force=true") {
		t.Fatalf("terminate did not force: query = %q", f.lastRequest().Query)
	}

	raw = call(t, s, "provisioner.terminate", map[string]any{"operation_key": "t2", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "terminated" || res.Changed {
		t.Fatalf("terminating a gone server = %+v, want terminated with changed=false", res)
	}
}

// A create with no panel user is refused before anything is sent: Pterodactyl
// requires one and a 422 from the panel is a less useful message than ours.
func TestCreateRequiresAPanelUser(t *testing.T) {
	s, f := newTestServer(t)
	p := createParams("nouser", "small")
	p["metadata"] = map[string]string{"name": "no-user"}
	err := callErr(t, s, "provisioner.create", p)
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "pterodactyl_user_id") {
		t.Fatalf("message %q does not say what is missing", err.Message)
	}
	if f.requestCount() != 0 {
		t.Fatal("a create with no user reached the panel")
	}
}

// A missing operation key is a core bug and must be refused, not sent without the
// idempotency protection the whole queue depends on.
func TestCreateRequiresOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	p := createParams("", "small")
	if err := callErr(t, s, "provisioner.create", p); err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
	if f.requestCount() != 0 {
		t.Fatal("a create with no operation key reached the panel")
	}
}

// A node with no free port is an actionable refusal, not a crash and not a server
// created without an allocation.
func TestCreateReportsAnExhaustedNode(t *testing.T) {
	s, f := newTestServer(t)
	f.freeAllocations = nil
	err := callErr(t, s, "provisioner.create", createParams("no-ports", "small"))
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "no free allocation") {
		t.Fatalf("message %q is not actionable", err.Message)
	}
}

// A 404 from the panel is not_found, which the core treats as "the provider has
// never heard of this" rather than a transient failure.
func TestUnknownServerIsNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	err := callErr(t, s, "provisioner.inspect", map[string]any{"external_id": "does-not-exist"})
	if err.Code != codeNotFound {
		t.Fatalf("code = %d, want %d", err.Code, codeNotFound)
	}
}

// An environment variable with a NUL byte would corrupt Pterodactyl's stored JSON
// rather than fail, so it is refused here.
func TestCreateRefusesANulInAnEnvironmentVariable(t *testing.T) {
	s, _ := newTestServer(t)
	p := createParams("nul-op", "small")
	p["metadata"] = map[string]string{
		"pterodactyl_user_id": "7",
		"env.SEED":            "abc\x00def",
	}
	err := callErr(t, s, "provisioner.create", p)
	if err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
}

// --- configuration -------------------------------------------------------------

func TestConfigRequiresAnApplicationKey(t *testing.T) {
	_, err := configFromEnv([]string{
		"PTERODACTYL_PANEL_URL=https://panel.test",
		"PTERODACTYL_API_KEY=ptlc_client_key",
		"PTERODACTYL_NEST_ID=1",
		"PTERODACTYL_EGG_ID=15",
	})
	if err == nil {
		t.Fatal("a client key was accepted; it cannot create servers")
	}
	if !strings.Contains(err.Error(), "application key") {
		t.Fatalf("error %q does not say what kind of key is needed", err)
	}
}

func TestConfigReportsEveryMissingValueAtOnce(t *testing.T) {
	_, err := configFromEnv([]string{})
	if err == nil {
		t.Fatal("an empty environment produced a config")
	}
	for _, name := range []string{"PTERODACTYL_PANEL_URL", "PTERODACTYL_API_KEY",
		"PTERODACTYL_NEST_ID", "PTERODACTYL_EGG_ID"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name %s", err, name)
		}
	}
}

func TestParsePlans(t *testing.T) {
	plans, err := parsePlans("small:2048:10240:100,medium:4096:20480:200")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %+v", plans)
	}
	if plans[1].ID != "medium" || plans[1].MemoryMB != 4096 || plans[1].CPUPercent != 200 {
		t.Fatalf("second plan = %+v", plans[1])
	}

	for _, bad := range []string{
		"small:2048:10240",   // too few fields
		"small:0:10240:100",  // zero memory
		"small:2048:10240:x", // non-numeric cpu
		"a:1:1:1,a:2:2:2",    // duplicate id
		":2048:10240:100",    // no id
	} {
		if _, err := parsePlans(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}

	// An unconfigured installation gets one usable default rather than none.
	plans, err = parsePlans("")
	if err != nil || len(plans) != 1 || plans[0].ID != "default" {
		t.Fatalf("default plans = %+v, %v", plans, err)
	}
}

func TestPlanLimitsReachTheRequest(t *testing.T) {
	s, f := newTestServer(t)
	call(t, s, "provisioner.create", createParams("limits-op", "medium"))
	limits, ok := f.lastRequest().Body["limits"].(map[string]any)
	if !ok {
		t.Fatal("no limits in the create request")
	}
	if limits["memory"] != float64(4096) || limits["cpu"] != float64(200) {
		t.Fatalf("limits = %v, want the medium plan's 4096 MB / 200%%", limits)
	}
}

// The whole point of the external_id design: a lookup works for a server this
// process did not create, which is what happens after a restart.
func TestLookupWorksAcrossARestart(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("restart-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	// A brand-new adapter with no memory of anything.
	fresh := &server{client: newPteroClient(f.config()), cfg: f.config()}
	raw := call(t, fresh, "provisioner.lookup", map[string]any{"operation_key": "restart-op"})
	var res struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.ExternalID != c.ExternalID {
		t.Fatalf("a fresh adapter could not find the server: %+v", res)
	}

	// And a create replay on the fresh adapter must not buy a second one.
	replay := call(t, fresh, "provisioner.create", createParams("restart-op", "small"))
	var b struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(replay, &b); err != nil {
		t.Fatal(err)
	}
	if b.ExternalID != c.ExternalID {
		t.Fatalf("a fresh adapter provisioned a duplicate: %q then %q", c.ExternalID, b.ExternalID)
	}
}

var _ = time.Now
