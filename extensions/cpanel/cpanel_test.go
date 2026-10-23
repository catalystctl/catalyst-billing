package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The adapter's tests, driven through its own JSON-RPC surface.
//
// Each of these corresponds to a way a provisioner can lose money or strand a
// customer: a create that buys a second account, a lookup that mistakes "I do not
// know" for "it does not exist", a suspend that reports failure because the
// account was already suspended, a terminate the core retries forever.

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

func newTestServer(t *testing.T) (*server, *fakeWHM) {
	t.Helper()
	f := newFakeWHM(t)
	cfg := f.config()
	return newServerFor(cfg), f
}

func newServerFor(cfg config) *server {
	return &server{client: newWHMClient(cfg), cfg: cfg, ops: newLedger(cfg.statePath)}
}

func createParams(key, plan string) map[string]any {
	return map[string]any{
		"operation_key":   key,
		"subscription_id": "sub_1",
		"plan_id":         plan,
		"quantity":        1,
		"metadata": map[string]string{
			"cpanel_domain":        "example.com",
			"cpanel_contact_email": "owner@example.com",
		},
	}
}

func TestInitializeAdvertisesProvisionerMethods(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "initialize", map[string]any{"protocol": "billing.ext/1", "versions": []int{1}})
	var res struct {
		Version      int      `json:"version"`
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 {
		t.Fatalf("version = %d, want 1", res.Version)
	}
	if res.Name != "cpanel" {
		t.Fatalf("name = %q", res.Name)
	}
	want := []string{"provisioner.describe", "provisioner.validate", "provisioner.create",
		"provisioner.inspect", "provisioner.lookup", "provisioner.suspend",
		"provisioner.resume", "provisioner.terminate", "provisioner.upgrade",
		"provisioner.fields"}
	if len(res.Capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v", res.Capabilities, want)
	}
	for i := range want {
		if res.Capabilities[i] != want[i] {
			t.Fatalf("capability %d = %q, want %q", i, res.Capabilities[i], want[i])
		}
	}

	// A handshake with a version this adapter does not speak must be refused, not
	// silently downgraded.
	err := callErr(t, s, "initialize", map[string]any{"protocol": "billing.ext/1", "versions": []int{2}})
	if err.Code != codeVersionMismatch {
		t.Fatalf("code = %d, want %d", err.Code, codeVersionMismatch)
	}
}

// The loop must frame one JSON object per line and ignore notifications, because
// the core writes both.
func TestServeLoopAnswersFramesAndIgnoresNotifications(t *testing.T) {
	s, _ := newTestServer(t)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"health","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":7,"method":"health"}}` + "\n" +
			`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"protocol":"billing.ext/1","versions":[1]}}` + "\n")
	var out bytes.Buffer
	if err := s.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d frames, want 2 (the notification must not be answered): %q", len(lines), out.String())
	}
	var health struct {
		ID     uint64            `json:"id"`
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &health); err != nil {
		t.Fatal(err)
	}
	if health.ID != 7 || health.Result["status"] != "ok" {
		t.Fatalf("health frame = %+v", health)
	}
}

func TestDescribeReportsConfiguredPlansAndTheSingleServer(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "provisioner.describe", map[string]any{})
	var res struct {
		Name    string   `json:"name"`
		Regions []string `json:"regions"`
		Plans   []struct {
			ID     string `json:"id"`
			Limits struct {
				QuotaMB     int64 `json:"quota_mb"`
				BandwidthMB int64 `json:"bandwidth_mb"`
			} `json:"limits"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Plans) != 2 {
		t.Fatalf("plans = %+v, want two", res.Plans)
	}
	if res.Plans[0].ID != "small" || res.Plans[0].Limits.QuotaMB != 1024 || res.Plans[0].Limits.BandwidthMB != 10240 {
		t.Fatalf("first plan = %+v", res.Plans[0])
	}
	// A WHM server is one machine: there is nothing to place an account in, so the
	// only region on offer is the server itself.
	if len(res.Regions) != 1 || res.Regions[0] != f.config().host {
		t.Fatalf("regions = %v, want [%s]", res.Regions, f.config().host)
	}
	if f.requestCount() != 0 {
		t.Fatal("describe reached WHM; it is a pure read of local configuration")
	}
}

func TestValidateRefusesWhatCannotBeServed(t *testing.T) {
	s, f := newTestServer(t)
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"unknown plan", map[string]any{"plan_id": "enormous", "quantity": 1}, "unknown plan"},
		{"quantity above one", map[string]any{"plan_id": "small", "quantity": 2}, "one cPanel account"},
		{"negative quantity", map[string]any{"plan_id": "small", "quantity": -1}, "negative"},
		{"region we do not have", map[string]any{"plan_id": "small", "quantity": 1, "region": "eu-west-1"}, "cannot be served"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := call(t, s, "provisioner.validate", tc.params)
			var res struct {
				Valid    bool     `json:"valid"`
				Problems []string `json:"problems"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatal(err)
			}
			if res.Valid {
				t.Fatalf("%+v was reported as valid", tc.params)
			}
			if len(res.Problems) == 0 || !strings.Contains(strings.Join(res.Problems, " "), tc.want) {
				t.Fatalf("problems = %v, want one mentioning %q", res.Problems, tc.want)
			}
		})
	}
	// A known plan on the server we have is valid.
	raw := call(t, s, "provisioner.validate", map[string]any{"plan_id": "large", "quantity": 1, "region": f.config().host})
	var ok struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		t.Fatal(err)
	}
	if !ok.Valid {
		t.Fatal("a known plan on the configured server was refused")
	}
	if f.requestCount() != 0 {
		t.Fatal("validate reached WHM; it is a pure read of local configuration")
	}
}

// The create request must be shaped the way WHM's json-api expects: a query string
// with the plan's quota and bandwidth, the API token in a header, and the account
// name derived from the operation key so that a lookup can recompute it.
func TestCreateSendsWHMShapedRequest(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "provisioner.create", createParams("op-key-1", "small"))
	var res struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
		PlanID     string `json:"plan_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	want := usernameForKey("op-key-1")
	if res.ExternalID != want {
		t.Fatalf("external_id = %q, want the derived account name %q", res.ExternalID, want)
	}
	if res.Status != "active" {
		t.Fatalf("status = %q, want active", res.Status)
	}
	if res.PlanID != "small" {
		t.Fatalf("plan_id = %q, want small", res.PlanID)
	}

	req := f.requestFor("createacct")
	if req.Auth != "whm root:"+testToken {
		t.Fatalf("authorization = %q, want the whm root:TOKEN header", req.Auth)
	}
	if got := req.Query.Get("api.version"); got != "1" {
		t.Fatalf("api.version = %q", got)
	}
	if got := req.Query.Get("username"); got != want {
		t.Fatalf("username = %q, want %q", got, want)
	}
	if got := req.Query.Get("domain"); got != "example.com" {
		t.Fatalf("domain = %q", got)
	}
	if got := req.Query.Get("quota"); got != "1024" {
		t.Fatalf("quota = %q, want the small plan's 1024 MB", got)
	}
	if got := req.Query.Get("bwlimit"); got != "10240" {
		t.Fatalf("bwlimit = %q, want the small plan's 10240 MB", got)
	}
	if got := req.Query.Get("contactemail"); got != "owner@example.com" {
		t.Fatalf("contactemail = %q", got)
	}
	// The token must not travel in the URL: a query string ends up in WHM's access
	// log and in every proxy log in between.
	for key, values := range req.Query {
		for _, v := range values {
			if strings.Contains(v, testToken) {
				t.Fatalf("%s carries the API token in the query string", key)
			}
		}
	}
}

// A replay of the same operation key must return the account that already exists,
// and must not call createacct a second time.
func TestCreateIsIdempotentByOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	first := call(t, s, "provisioner.create", createParams("same-op", "small"))
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
		t.Fatalf("a replay created a second account: %q then %q", a.ExternalID, b.ExternalID)
	}
	if n := f.countFor("createacct"); n != 1 {
		t.Fatalf("createacct was called %d times, want 1", n)
	}
	if f.accountCount() != 1 {
		t.Fatalf("WHM holds %d accounts, want 1", f.accountCount())
	}
}

// A replay whose metadata has changed must still return the account the first call
// created: the ledger, not the new request, says which account belongs to the key.
func TestCreateReplayWithChangedMetadataReturnsTheOriginalAccount(t *testing.T) {
	s, f := newTestServer(t)
	first := call(t, s, "provisioner.create", createParams("drift-op", "small"))
	var a struct {
		ExternalID string `json:"external_id"`
		PlanID     string `json:"plan_id"`
	}
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}

	changed := createParams("drift-op", "large")
	changed["metadata"] = map[string]string{"cpanel_domain": "somewhere-else.example", "cpanel_username": ""}
	second := call(t, s, "provisioner.create", changed)
	var b struct {
		ExternalID string `json:"external_id"`
		PlanID     string `json:"plan_id"`
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	if b.ExternalID != a.ExternalID {
		t.Fatalf("the replay provisioned a different account: %q then %q", a.ExternalID, b.ExternalID)
	}
	if b.PlanID != "small" {
		t.Fatalf("plan_id = %q, want the originally provisioned small", b.PlanID)
	}
	if n := f.countFor("createacct"); n != 1 {
		t.Fatalf("createacct was called %d times, want 1", n)
	}
}

// Lookup must be authoritative and must distinguish "no such account" from "cannot
// answer". found=false lets the core provision; an error makes it wait.
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
		t.Fatal("lookup found an account for a key that was never created")
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
	if !found.Found || found.ExternalID != c.ExternalID || found.Status != "active" {
		t.Fatalf("lookup = %+v, want the created account", found)
	}

	// Now make WHM fail. The adapter must report an error, never found=false, or
	// the core would provision a duplicate.
	f.failWith(500, "<html><body>500 Internal Server Error</body></html>")
	err := callErr(t, s, "provisioner.lookup", map[string]any{"operation_key": "known-op"})
	if err.Code == codeNotFound {
		t.Fatal("an unreachable WHM was reported as not-found, so the core would provision a duplicate")
	}
	if err.Code != codeInternal {
		t.Fatalf("code = %d, want %d (internal: we could not get an answer)", err.Code, codeInternal)
	}
}

// A refused listaccts is also "I do not know", not "there is none".
func TestLookupTreatsARefusedListacctsAsUnknown(t *testing.T) {
	s, f := newTestServer(t)
	f.refuseWith("listaccts", "Sorry, you do not have permission to run this function")
	err := callErr(t, s, "provisioner.lookup", map[string]any{"operation_key": "any-op"})
	if err.Code == codeNotFound {
		t.Fatal("a refused listaccts was reported as not-found")
	}
}

// The whole point of deriving the name from the key: a lookup works for an account
// this process did not create, which is what happens after a restart.
func TestLookupFromAFreshAdapterFindsAnAccountItDidNotCreate(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("restart-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	// A brand-new adapter: no memory, and a ledger path it has never written.
	freshCfg := f.config()
	fresh := newServerFor(freshCfg)
	raw := call(t, fresh, "provisioner.lookup", map[string]any{"operation_key": "restart-op"})
	var res struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.ExternalID != c.ExternalID {
		t.Fatalf("a fresh adapter could not find the account: %+v", res)
	}

	// And a create replay on the fresh adapter must not buy a second account.
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
	if n := f.countFor("createacct"); n != 1 {
		t.Fatalf("createacct was called %d times, want 1", n)
	}
}

// A corrupt ledger is not an authority, so it must not make a lookup lie: the
// answer comes from WHM.
func TestCorruptLedgerDoesNotBreakLookup(t *testing.T) {
	f := newFakeWHM(t)
	cfg := f.config()
	if err := os.WriteFile(cfg.statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newServerFor(cfg)
	f.addAccount(usernameForKey("corrupt-op"), "example.com", false)

	raw := call(t, s, "provisioner.lookup", map[string]any{"operation_key": "corrupt-op"})
	var res struct {
		Found      bool   `json:"found"`
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.ExternalID != usernameForKey("corrupt-op") {
		t.Fatalf("lookup with a corrupt ledger = %+v, want the account WHM has", res)
	}

	// A create, on the other hand, is refused before any request: an adapter that
	// cannot write down what it is about to do must not do it.
	err := callErr(t, s, "provisioner.create", createParams("corrupt-op", "small"))
	if err.Code != codeInternal {
		t.Fatalf("code = %d, want %d", err.Code, codeInternal)
	}
	if n := f.countFor("createacct"); n != 0 {
		t.Fatalf("createacct was called %d times despite an unreadable ledger", n)
	}
}

// A create whose ledger cannot be written is refused before anything is sent.
func TestCreateRefusesWhenTheLedgerCannotBeWritten(t *testing.T) {
	f := newFakeWHM(t)
	cfg := f.config()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.statePath = filepath.Join(blocker, "operations.json")
	s := newServerFor(cfg)

	err := callErr(t, s, "provisioner.create", createParams("noledger-op", "small"))
	if err.Code != codeInternal {
		t.Fatalf("code = %d, want %d", err.Code, codeInternal)
	}
	if f.requestCount() != 0 {
		t.Fatalf("WHM was called %d times before the ledger could be written", f.requestCount())
	}
}

// Suspend and resume are idempotent: asking for the state an account is already in
// succeeds with changed=false, and does not call WHM again.
func TestSuspendAndResumeAreIdempotent(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("state-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	raw := call(t, s, "provisioner.suspend", map[string]any{"operation_key": "s1", "external_id": c.ExternalID, "reason": "invoice unpaid"})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "suspended" || !res.Changed {
		t.Fatalf("suspend = %+v", res)
	}
	if got := f.requestFor("suspendacct").Query.Get("reason"); got != "invoice unpaid" {
		t.Fatalf("suspend reason = %q", got)
	}

	raw = call(t, s, "provisioner.suspend", map[string]any{"operation_key": "s2", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Status != "suspended" {
		t.Fatalf("suspending a suspended account = %+v, want suspended with changed=false", res)
	}
	if n := f.countFor("suspendacct"); n != 1 {
		t.Fatalf("suspendacct was called %d times, want 1", n)
	}

	raw = call(t, s, "provisioner.resume", map[string]any{"operation_key": "r1", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "active" || !res.Changed {
		t.Fatalf("resume = %+v", res)
	}
	raw = call(t, s, "provisioner.resume", map[string]any{"operation_key": "r2", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Status != "active" {
		t.Fatalf("resuming an active account = %+v, want active with changed=false", res)
	}
	if n := f.countFor("unsuspendacct"); n != 1 {
		t.Fatalf("unsuspendacct was called %d times, want 1", n)
	}
}

// Suspending an account WHM does not have is not something to paper over: the core
// has to learn it is gone and reconcile.
func TestSuspendOfAnUnknownAccountIsNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	err := callErr(t, s, "provisioner.suspend", map[string]any{"operation_key": "s", "external_id": "cnosuchaccount"})
	if err.Code != codeNotFound {
		t.Fatalf("code = %d, want %d", err.Code, codeNotFound)
	}
}

// Terminate removes the account, and terminating one that is already gone is the
// requested end state rather than an error the core would retry forever.
func TestTerminateIsIdempotent(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("term-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	raw := call(t, s, "provisioner.terminate", map[string]any{"operation_key": "t1", "external_id": c.ExternalID, "reason": "cancelled"})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "terminated" || !res.Changed {
		t.Fatalf("terminate = %+v", res)
	}
	if f.hasAccount(c.ExternalID) {
		t.Fatal("the account is still on the server after a terminate")
	}
	if got := f.requestFor("removeacct").Query.Get("keepdns"); got != "0" {
		t.Fatalf("keepdns = %q, want 0: a terminated account must not keep its DNS zone", got)
	}

	raw = call(t, s, "provisioner.terminate", map[string]any{"operation_key": "t2", "external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "terminated" || res.Changed {
		t.Fatalf("terminating a gone account = %+v, want terminated with changed=false", res)
	}
	if n := f.countFor("removeacct"); n != 1 {
		t.Fatalf("removeacct was called %d times, want 1", n)
	}

	// After a termination a lookup for the key must say "gone", which is the truth,
	// not "unknown".
	raw = call(t, s, "provisioner.lookup", map[string]any{"operation_key": "term-op"})
	var found struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if found.Found {
		t.Fatal("lookup still reports a terminated account as existing")
	}
}

// Terminating an account the adapter never created, and WHM has never heard of, is
// a success: the requested end state is already true.
func TestTerminateOfAGoneAccountIsSuccess(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "provisioner.terminate", map[string]any{"operation_key": "t", "external_id": "clonggone"})
	var res struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "terminated" || res.Changed {
		t.Fatalf("terminate of a gone account = %+v", res)
	}
	if n := f.countFor("removeacct"); n != 0 {
		t.Fatalf("removeacct was called %d times for an account that does not exist", n)
	}
}

func TestInspectReportsObservedState(t *testing.T) {
	s, f := newTestServer(t)
	created := call(t, s, "provisioner.create", createParams("inspect-op", "small"))
	var c struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(created, &c); err != nil {
		t.Fatal(err)
	}

	var res struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
		PlanID     string `json:"plan_id"`
	}
	raw := call(t, s, "provisioner.inspect", map[string]any{"external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "active" || res.PlanID != "small" {
		t.Fatalf("inspect = %+v, want active on the small plan", res)
	}

	// A suspension done by staff in WHM must be visible: inspect reads the state,
	// it does not remember it.
	f.addAccount(c.ExternalID, "example.com", true)
	raw = call(t, s, "provisioner.inspect", map[string]any{"external_id": c.ExternalID})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "suspended" {
		t.Fatalf("status = %q, want suspended after WHM suspended it", res.Status)
	}

	err := callErr(t, s, "provisioner.inspect", map[string]any{"external_id": "cnosuchaccount"})
	if err.Code != codeNotFound {
		t.Fatalf("code = %d, want %d", err.Code, codeNotFound)
	}
}

// An account name the adapter cannot recompute from the key would make lookup
// non-authoritative, so it is refused locally, before any request, with an
// explanation rather than WHM's sentence about usernames.
func TestInvalidAccountNameIsRefusedLocally(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]string
		want     string
	}{
		{
			name:     "syntax",
			metadata: map[string]string{"cpanel_domain": "example.com", "cpanel_username": "Bob Smith"},
			want:     "not a valid cPanel username",
		},
		{
			name:     "too long",
			metadata: map[string]string{"cpanel_domain": "example.com", "cpanel_username": "averylongusername17"},
			want:     "at most 16",
		},
		{
			name:     "reserved",
			metadata: map[string]string{"cpanel_domain": "example.com", "cpanel_username": "root"},
			want:     "system or cPanel service account",
		},
		{
			name:     "valid but not derivable",
			metadata: map[string]string{"cpanel_domain": "example.com", "cpanel_username": "acme01"},
			want:     "operation key",
		},
		{
			name:     "no domain",
			metadata: map[string]string{},
			want:     "needs a domain",
		},
		{
			name:     "not a domain",
			metadata: map[string]string{"cpanel_domain": "https://example.com/path"},
			want:     "not a domain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, f := newTestServer(t)
			p := createParams("name-op", "small")
			p["metadata"] = tc.metadata
			err := callErr(t, s, "provisioner.create", p)
			if err.Code != codeRejected {
				t.Fatalf("code = %d, want %d", err.Code, codeRejected)
			}
			if !strings.Contains(err.Message, tc.want) {
				t.Fatalf("message %q does not mention %q", err.Message, tc.want)
			}
			if f.requestCount() != 0 {
				t.Fatalf("an unusable request reached WHM (%v)", f.commands())
			}
		})
	}
}

// WHM answering HTTP 500 is not a not-found: the account may well exist, and
// saying it does not is how a duplicate gets provisioned.
func TestWHM500IsNeverNotFound(t *testing.T) {
	for _, method := range []string{"provisioner.lookup", "provisioner.inspect"} {
		t.Run(method, func(t *testing.T) {
			s, f := newTestServer(t)
			f.failWith(500, "internal error")
			params := map[string]any{"external_id": "cwhatever"}
			if method == "provisioner.lookup" {
				params = map[string]any{"operation_key": "whatever"}
			}
			err := callErr(t, s, method, params)
			if err.Code == codeNotFound {
				t.Fatalf("%s reported a 500 as not-found", method)
			}
			if err.Code != codeInternal {
				t.Fatalf("code = %d, want %d", err.Code, codeInternal)
			}
		})
	}
}

// A rejected API token is a configuration error, not a business refusal and not an
// absence.
func TestRejectedAPITokenIsNotNotFound(t *testing.T) {
	f := newFakeWHM(t)
	cfg := f.config()
	cfg.apiToken = "WRONG"
	s := newServerFor(cfg)
	err := callErr(t, s, "provisioner.lookup", map[string]any{"operation_key": "any"})
	if err.Code == codeNotFound {
		t.Fatal("a rejected API token was reported as not-found")
	}
	if !strings.Contains(err.Message, "CPANEL_API_TOKEN") {
		t.Fatalf("message %q does not name the setting to fix", err.Message)
	}
}

// metadata.result 0 is WHM understanding the request and saying no: a rejection on
// the merits (-32005), not a crash and not a retryable failure.
func TestResultZeroIsARejection(t *testing.T) {
	s, f := newTestServer(t)
	f.refuseWith("createacct", "The domain example.com is already in use by another account")
	err := callErr(t, s, "provisioner.create", createParams("refused-op", "small"))
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "already in use") {
		t.Fatalf("message %q does not carry WHM's reason", err.Message)
	}
	// The refusal must not leave a half-created account behind.
	if f.accountCount() != 0 {
		t.Fatalf("WHM holds %d accounts after a refused create", f.accountCount())
	}
}

// Two workers can race on one key. WHM refuses the second createacct because the
// name is taken; the adapter must adopt the account rather than report a failure,
// because the name is a function of the key, so the account is this operation's.
func TestCreateAdoptsAnAccountThatAppearedMeanwhile(t *testing.T) {
	s, f := newTestServer(t)
	f.raceOnCreate = true
	raw := call(t, s, "provisioner.create", createParams("race-op", "small"))
	var res struct {
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.ExternalID != usernameForKey("race-op") || res.Status != "active" {
		t.Fatalf("create after a lost race = %+v", res)
	}
	if f.accountCount() != 1 {
		t.Fatalf("WHM holds %d accounts, want 1", f.accountCount())
	}
}

func TestCreateRequiresOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	p := createParams("", "small")
	if err := callErr(t, s, "provisioner.create", p); err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
	if f.requestCount() != 0 {
		t.Fatal("a create with no operation key reached WHM")
	}
}

// WHM's user search is a substring match. A returned row whose user merely contains
// the name must never be mistaken for a match: that would hand the core somebody
// else's account.
func TestFindAccountIgnoresSubstringMatches(t *testing.T) {
	f := newFakeWHM(t)
	cfg := f.config()
	c := newWHMClient(cfg)
	f.addAccount("cabcdefgh", "example.com", false)
	f.addAccount("cabc", "other.example", false)

	ctx := context.Background()
	found, err := c.findAccount(ctx, "cabc")
	if err != nil {
		t.Fatalf("findAccount: %v", err)
	}
	if found == nil || found.User != "cabc" {
		t.Fatalf("findAccount = %+v, want the exact account", found)
	}
	// "cab" is a prefix of both, so listaccts returns rows and the exact comparison
	// has to reject all of them.
	found, err = c.findAccount(ctx, "cab")
	if err != nil {
		t.Fatalf("findAccount: %v", err)
	}
	if found != nil {
		t.Fatalf("a substring match was returned as %q", found.User)
	}
}

// --- names -------------------------------------------------------------------

func TestUsernameForKeySatisfiesCPanelRules(t *testing.T) {
	seen := map[string]string{}
	for _, key := range []string{"op-1", "op-2", "subscription:42:create", strings.Repeat("x", 200), "Ünïcode-key"} {
		name := usernameForKey(key)
		if err := validateUsername(name); err != nil {
			t.Fatalf("usernameForKey(%q) = %q: %v", key, name, err)
		}
		if name != strings.ToLower(name) {
			t.Fatalf("%q is not lower case", name)
		}
		if len(name) > maxUsernameLen {
			t.Fatalf("%q is %d characters, cPanel allows %d", name, len(name), maxUsernameLen)
		}
		if name[0] < 'a' || name[0] > 'z' {
			t.Fatalf("%q does not start with a letter", name)
		}
		// Deterministic: the same key must always produce the same name, which is
		// the property the whole lookup design rests on.
		if again := usernameForKey(key); again != name {
			t.Fatalf("usernameForKey(%q) is not deterministic: %q then %q", key, name, again)
		}
		if other, dup := seen[name]; dup {
			t.Fatalf("keys %q and %q derive the same name %q", other, key, name)
		}
		seen[name] = key
	}
}

func TestValidateUsernameRejectsCPanelViolations(t *testing.T) {
	for _, bad := range []string{"", "Acme", "1acme", "acme-01", "acme.01", "acme 01", "averylongusername17", "root", "mail"} {
		if err := validateUsername(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"a", "acme01", "c0ffee1234567890", "abc"} {
		if err := validateUsername(good); err != nil {
			t.Fatalf("%q was rejected: %v", good, err)
		}
	}
}

// --- configuration -----------------------------------------------------------

func TestConfigNamesEveryMissingValueAtOnce(t *testing.T) {
	_, err := configFromEnv([]string{})
	if err == nil {
		t.Fatal("an empty environment produced a config")
	}
	for _, name := range []string{"CPANEL_WHM_URL", "CPANEL_API_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name %s", err, name)
		}
	}

	// The token alone is enough to be missing for the message to say so.
	_, err = configFromEnv([]string{"CPANEL_WHM_URL=https://whm.example.com:2087"})
	if err == nil || !strings.Contains(err.Error(), "CPANEL_API_TOKEN") {
		t.Fatalf("a missing token produced %v", err)
	}
	if strings.Contains(err.Error(), "CPANEL_WHM_URL,") {
		t.Fatalf("the message names a value that was provided: %v", err)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	base := []string{
		"CPANEL_WHM_URL=https://whm.example.com:2087/",
		"CPANEL_API_TOKEN=token",
	}
	cfg, err := configFromEnv(base)
	if err != nil {
		t.Fatalf("minimal config: %v", err)
	}
	if !cfg.verifyTLS {
		t.Fatal("TLS verification must default to on")
	}
	if cfg.timeout != 30*time.Second {
		t.Fatalf("timeout = %s, want 30s", cfg.timeout)
	}
	if cfg.host != "whm.example.com" {
		t.Fatalf("host = %q", cfg.host)
	}
	if strings.HasSuffix(cfg.whmURL, "/") {
		t.Fatalf("whmURL = %q keeps a trailing slash", cfg.whmURL)
	}
	if cfg.statePath != defaultStatePath {
		t.Fatalf("statePath = %q, want the default", cfg.statePath)
	}
	// One default plan, so a single-size installation needs no plan configuration.
	if len(cfg.plans) != 1 || cfg.defaultPlan != cfg.plans[0].ID {
		t.Fatalf("plans = %+v, default = %q", cfg.plans, cfg.defaultPlan)
	}
	if len(cfg.warnings) != 0 {
		t.Fatalf("warnings = %v", cfg.warnings)
	}

	// Turning TLS verification off is allowed but warned about, loudly.
	off, err := configFromEnv(append(base, "CPANEL_VERIFY_TLS=0"))
	if err != nil {
		t.Fatalf("CPANEL_VERIFY_TLS=0: %v", err)
	}
	if off.verifyTLS {
		t.Fatal("CPANEL_VERIFY_TLS=0 did not disable verification")
	}
	if len(off.warnings) == 0 || !strings.Contains(strings.Join(off.warnings, " "), "CPANEL_VERIFY_TLS") {
		t.Fatalf("warnings = %v, want one naming CPANEL_VERIFY_TLS", off.warnings)
	}

	for _, bad := range [][]string{
		append(base, "CPANEL_VERIFY_TLS=maybe"),
		append(base, "CPANEL_TIMEOUT_MS=0"),
		append(base, "CPANEL_TIMEOUT_MS=soon"),
		append(base, "CPANEL_DEFAULT_PLAN=nope"),
		append(base, "CPANEL_PLANS=small:1024"),
		{"CPANEL_WHM_URL=whm.example.com:2087", "CPANEL_API_TOKEN=token"},
		{"CPANEL_WHM_URL=ftp://whm.example.com", "CPANEL_API_TOKEN=token"},
	} {
		if _, err := configFromEnv(bad); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

func TestParsePlans(t *testing.T) {
	plans, err := parsePlans("small:1024:10240,large:10240:102400")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plans) != 2 || plans[1].ID != "large" || plans[1].QuotaMB != 10240 || plans[1].BandwidthMB != 102400 {
		t.Fatalf("plans = %+v", plans)
	}
	for _, bad := range []string{
		"small:1024",         // too few fields
		"small:1024:10240:1", // too many
		"small:0:10240",      // a zero quota is cPanel's "unlimited"
		"small:1024:x",       // non-numeric bandwidth
		"a:1:1,a:2:2",        // duplicate id
		":1024:10240",        // no id
		"bad id:1024:10240",  // separator in the id
	} {
		if _, err := parsePlans(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// The plan a call names reaches the request as quota and bandwidth, which is the
// only thing cPanel knows about a size.
func TestPlanLimitsReachTheRequest(t *testing.T) {
	s, f := newTestServer(t)
	call(t, s, "provisioner.create", createParams("limits-op", "large"))
	q := f.requestFor("createacct").Query
	if q.Get("quota") != "10240" || q.Get("bwlimit") != "102400" {
		t.Fatalf("quota/bwlimit = %q/%q, want the large plan's 10240/102400", q.Get("quota"), q.Get("bwlimit"))
	}
}

// ---------------------------------------------------------------- upgrade --

// A paid upgrade must reach WHM: modifyacct sets the target plan's quota and bandwidth, in
// place, without rebuilding the account.
//
// The "in place" half is the point. An upgrade implemented as removeacct+createacct would
// delete the customer's files, databases and mailboxes and still report success.
func TestUpgradeChangesTheQuotaInPlace(t *testing.T) {
	s, f := newTestServer(t)
	call(t, s, "provisioner.create", createParams("create:1", "small"))

	raw := call(t, s, "provisioner.upgrade", map[string]any{
		"operation_key": "resize:1:price-2:1",
		"external_id":   usernameForKey("create:1"),
		"plan_id":       "large",
	})
	var res struct {
		PlanID  string `json:"plan_id"`
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.PlanID != "large" || !res.Changed {
		t.Fatalf("result = %+v, want the large plan and changed", res)
	}

	name := usernameForKey("create:1")
	f.mu.Lock()
	acct := f.accounts[name]
	f.mu.Unlock()
	if acct == nil {
		t.Fatal("the account disappeared during an upgrade")
	}
	if acct.Quota != 10240 {
		t.Fatalf("quota = %d, want 10240 (the large plan)", acct.Quota)
	}
	if acct.Domain != "example.com" {
		t.Fatalf("domain = %q, want the original domain to survive an upgrade", acct.Domain)
	}
	// No account was removed and re-created.
	for _, rec := range f.requests {
		if rec.Command == "removeacct" {
			t.Fatal("an upgrade removed the account")
		}
	}
}

// An unknown target plan is refused before WHM is asked to change anything.
func TestUpgradeRefusesAnUnknownPlan(t *testing.T) {
	s, f := newTestServer(t)
	call(t, s, "provisioner.create", createParams("create:1", "small"))
	before := len(f.requests)

	// A plan id WHM has never heard of must not reach it: WHM would silently accept the
	// parameters and the account would keep its old quota.
	err := callErr(t, s, "provisioner.upgrade", map[string]any{
		"operation_key": "resize:1:x:1",
		"external_id":   usernameForKey("create:1"),
		"plan_id":       "enormous",
	})
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want rejected", err.Code)
	}
	if len(f.requests) != before {
		t.Fatal("an unknown plan reached WHM")
	}
}

// ---------------------------------------------------------------- fields --

// The declared fields must include the domain a create actually refuses to run without.
func TestDeclaredFieldsMatchWhatCreateRequires(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "provisioner.fields", map[string]any{})
	var res struct {
		Fields []struct {
			Key      string `json:"key"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	required := ""
	for _, field := range res.Fields {
		if field.Required {
			required = field.Key
		}
		if field.Type == "" {
			t.Fatalf("field %q declares no type, so a form cannot render it", field.Key)
		}
	}
	if required != "domain" {
		t.Fatalf("required field = %q, want domain", required)
	}

	params := map[string]any{
		"operation_key": "create:no-domain",
		"plan_id":       "small",
		"quantity":      1,
		"metadata":      map[string]string{},
	}
	rpcErr := callErr(t, s, "provisioner.create", params)
	if !strings.Contains(rpcErr.Message, "domain") {
		t.Fatalf("create without a domain said %q", rpcErr.Message)
	}
}
