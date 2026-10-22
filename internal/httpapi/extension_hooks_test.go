package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/httpapi"
)

// Extension hooks over HTTP, end to end against the real database and a real extension
// process.
//
// The properties under test are the ones the feature exists for, and each of them is a way
// the feature could be wrong rather than a way it could be merely ugly:
//
//   - an extension contributes a navigation item and a service action as data, and the
//     panel serves them filtered to what the caller may use;
//   - the guard on a call is the *declared* permission, so a hook cannot be invoked by
//     someone who does not hold what it asked for, and the call is audited;
//   - a committed order event reaches a subscribed version-2 extension;
//   - a dead extension cannot delay a checkout, and what it missed is counted.
//
// The extension is extensions/example-hooks, built and launched the way the panel launches
// one: a separate process, real pipes, the core's own client.

var (
	exampleHooksOnce sync.Once
	exampleHooksBin  string
	exampleHooksErr  error
)

// exampleHooksBinary builds extensions/example-hooks once for this package's tests.
func exampleHooksBinary(t *testing.T) string {
	t.Helper()
	exampleHooksOnce.Do(func() {
		root, err := filepath.Abs("../..")
		if err != nil {
			exampleHooksErr = err
			return
		}
		dir, err := os.MkdirTemp("", "example-hooks-*")
		if err != nil {
			exampleHooksErr = err
			return
		}
		out := filepath.Join(dir, "example-hooks")
		build := exec.Command("go", "build", "-o", out, "./extensions/example-hooks")
		build.Dir = root
		if b, err := build.CombinedOutput(); err != nil {
			exampleHooksErr = fmt.Errorf("go build ./extensions/example-hooks: %v\n%s", err, b)
			return
		}
		exampleHooksBin = out
	})
	if exampleHooksErr != nil {
		t.Fatalf("build example-hooks: %v", exampleHooksErr)
	}
	return exampleHooksBin
}

// installHooks launches the fixture, registers it, and attaches the registry to the
// server, which is exactly what a deployment does at boot.
func (e *authEnv) installHooks(t *testing.T, configure func(*extproto.Hooks), env ...string) (*extproto.Hooks, *extproto.Client) {
	t.Helper()
	c, err := extproto.Launch(context.Background(), extproto.Spec{
		Path:             exampleHooksBinary(t),
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	})
	if err != nil {
		t.Fatalf("launch example-hooks (%v): %v", env, err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The registry is built the way a deployment builds it, so the permission check under
	// test is the real one and not a test double that happens to accept everything.
	reg := httpapi.NewExtensionHooks()
	if configure != nil {
		configure(reg)
	}
	t.Cleanup(reg.Close)
	if err := reg.Register(context.Background(), c); err != nil {
		t.Fatalf("register example-hooks: %v", err)
	}
	e.srv.WithExtensionHooks(reg)
	return reg, c
}

// hookList is the decoded response of GET /api/v1/extensions/hooks.
type hookList struct {
	NavItems []struct {
		Extension  string `json:"extension"`
		ID         string `json:"id"`
		Label      string `json:"label"`
		Path       string `json:"path"`
		Icon       string `json:"icon"`
		Permission string `json:"permission"`
	} `json:"nav_items"`
	ServiceActions []struct {
		Extension  string `json:"extension"`
		ID         string `json:"id"`
		Label      string `json:"label"`
		Method     string `json:"method"`
		Confirm    string `json:"confirm"`
		Permission string `json:"permission"`
	} `json:"service_actions"`
}

func decodeHooks(t *testing.T, body []byte) hookList {
	t.Helper()
	var out hookList
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode hooks: %v\n%s", err, body)
	}
	return out
}

// callHook posts to a hook route with a CSRF token and an explicit idempotency key: the
// replay case is the point, so the key cannot be generated per call.
func (e *authEnv) callHook(t *testing.T, path string, cookie *http.Cookie, csrf, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The list is the menu the client renders, so it must be filtered per item: a support
// agent sees the action (they hold service:write:self) but not the operator navigation
// entry (they do not hold extension:manage).
func TestExtensionHooksListIsFilteredByPermission(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	// Administrator: both.
	rec := env.get(t, "/api/v1/extensions/hooks", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list: status %d, body %s", rec.Code, rec.Body)
	}
	admin := decodeHooks(t, rec.Body.Bytes())
	if len(admin.NavItems) != 1 {
		t.Fatalf("admin nav items = %d, want 1: %s", len(admin.NavItems), rec.Body)
	}
	if got := admin.NavItems[0]; got.Extension != "example-hooks" || got.Path != "/staff/extensions/example-hooks" ||
		got.Icon != "plug" || got.Permission != string(authn.PermExtensionManage) {
		t.Fatalf("admin nav item = %+v", got)
	}
	if len(admin.ServiceActions) != 1 {
		t.Fatalf("admin service actions = %d, want 1", len(admin.ServiceActions))
	}
	if got := admin.ServiceActions[0]; got.Method != "service.reboot" || got.Confirm == "" ||
		got.Permission != string(authn.PermServiceWriteSelf) {
		t.Fatalf("admin service action = %+v", got)
	}

	// Support holds neither extension:manage nor any staff hook permission.
	rec = env.get(t, "/api/v1/extensions/hooks", env.support)
	support := decodeHooks(t, rec.Body.Bytes())
	if len(support.NavItems) != 0 {
		t.Fatalf("support was shown %d operator nav item(s)", len(support.NavItems))
	}
	if len(support.ServiceActions) != 1 {
		t.Fatalf("support service actions = %d, want 1", len(support.ServiceActions))
	}

	// A customer holds service:write:self, so the action is theirs and the operator
	// navigation entry is not.
	customerID, userID := env.seedCustomer(t, "hooks-list@example.test", "Hook Customer")
	_ = customerID
	cookie, _ := env.issueSessionFor(t, userID)
	rec = env.get(t, "/api/v1/extensions/hooks", cookie)
	customer := decodeHooks(t, rec.Body.Bytes())
	if len(customer.NavItems) != 0 {
		t.Fatalf("a customer was shown %d operator nav item(s)", len(customer.NavItems))
	}
	if len(customer.ServiceActions) != 1 {
		t.Fatalf("customer service actions = %d, want 1", len(customer.ServiceActions))
	}

	// Anonymous: no identity, no hooks.
	if rec := env.get(t, "/api/v1/extensions/hooks", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d, want 401", rec.Code)
	}
}

// Calling a hook is a mutation: it runs the extension method, it is audited, and the
// Idempotency-Key reaches the extension as the operation key so a retried click returns
// the first result.
func TestStaffHookCallRunsAuditsAndIsIdempotent(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	tenant := env.createTenant(t, "hook-staff@example.test", "10.00")
	path := "/api/v1/staff/services/" + tenant.serviceID + "/hooks/example-hooks/reboot"

	rec := env.callHook(t, path, env.admin, env.adminCSRF, "hook-call-key-0001")
	if rec.Code != http.StatusOK {
		t.Fatalf("hook call: status %d, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["extension"] != "example-hooks" || body["method"] != "service.reboot" {
		t.Fatalf("body = %v", body)
	}
	if body["service_id"] != tenant.serviceID {
		t.Fatalf("service_id = %v, want %s", body["service_id"], tenant.serviceID)
	}
	data, _ := body["data"].(map[string]any)
	callID, _ := data["call_id"].(string)
	if callID == "" {
		t.Fatalf("the extension returned no call id: %v", body)
	}

	// The replay: the stored response is returned and the extension is not called again.
	rec = env.callHook(t, path, env.admin, env.adminCSRF, "hook-call-key-0001")
	if rec.Code != http.StatusOK {
		t.Fatalf("replayed hook call: status %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("a replay was not marked as one")
	}
	var replay map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &replay); err != nil {
		t.Fatalf("replay body: %v", err)
	}
	replayData, _ := replay["data"].(map[string]any)
	if replayData["call_id"] != callID {
		t.Fatalf("a replayed click acted twice: %v then %v", callID, replayData["call_id"])
	}

	// A fresh key is a fresh action.
	rec = env.callHook(t, path, env.admin, env.adminCSRF, "hook-call-key-0002")
	if rec.Code != http.StatusOK {
		t.Fatalf("second hook call: status %d, body %s", rec.Code, rec.Body)
	}
	var second map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	secondData, _ := second["data"].(map[string]any)
	if secondData["call_id"] == callID {
		t.Fatal("a new operation key was treated as a replay")
	}

	// The audit row: append-only, naming the actor, the extension and the service.
	var actor, outcome, method string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COALESCE(actor_id::text, ''), outcome, changes->>'method'
		FROM audit_events
		WHERE action = 'extension.hook_call' AND target_id = $1
		ORDER BY id DESC LIMIT 1`, tenant.serviceID).Scan(&actor, &outcome, &method); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("audit outcome = %q", outcome)
	}
	if method != "service.reboot" {
		t.Fatalf("audit method = %q", method)
	}
	if actor == "" {
		t.Fatal("the audit row names no actor")
	}
}

// The guard is the permission the extension declared, not the route's. A hook that asks
// for service:terminate must be refused to an operator who does not hold it, even though
// the route itself is reachable.
func TestHookCallIsGuardedByTheDeclaredPermission(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil, "EXAMPLE_HOOKS_ACTION_PERMISSION=service:terminate")

	tenant := env.createTenant(t, "hook-guard@example.test", "10.00")
	path := "/api/v1/staff/services/" + tenant.serviceID + "/hooks/example-hooks/reboot"

	// Support does not hold service:terminate.
	rec := env.callHook(t, path, env.support, env.supportCSRF, "hook-guard-key-0001")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support call: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	var denied struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &denied)
	if denied.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", denied.Code)
	}

	// The refusal is audited: a denied attempt is a fact an investigation needs.
	var outcome string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT outcome FROM audit_events
		WHERE action = 'extension.hook_call' AND target_id = $1
		ORDER BY id DESC LIMIT 1`, tenant.serviceID).Scan(&outcome); err != nil {
		t.Fatalf("denied audit row: %v", err)
	}
	if outcome != "denied" {
		t.Fatalf("audit outcome = %q, want denied", outcome)
	}

	// An administrator holds it, so the same call succeeds.
	rec = env.callHook(t, path, env.admin, env.adminCSRF, "hook-guard-key-0002")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin call: status %d, body %s", rec.Code, rec.Body)
	}
}

// A customer may call an action on their own service and on nobody else's. The scoping is
// in SQL, so another customer's service id is a 404 rather than a 403 that confirms it
// exists.
func TestCustomerHookCallIsScopedToTheirOwnService(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	mine := env.createTenant(t, "hook-mine@example.test", "10.00")
	theirs := env.createTenant(t, "hook-theirs@example.test", "10.00")
	cookie, csrf := env.login(t, "hook-mine@example.test", "a long enough password")

	rec := env.callHook(t, "/api/v1/portal/services/"+mine.serviceID+"/hooks/example-hooks/reboot",
		cookie, csrf, "customer-hook-key-0001")
	if rec.Code != http.StatusOK {
		t.Fatalf("own service: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.callHook(t, "/api/v1/portal/services/"+theirs.serviceID+"/hooks/example-hooks/reboot",
		cookie, csrf, "customer-hook-key-0002")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another customer's service: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// An action the extension never declared is not callable, and the answer does not reveal
// which extensions are installed.
func TestHookCallForAnUndeclaredActionIsNotFound(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	tenant := env.createTenant(t, "hook-404@example.test", "10.00")
	paths := []string{
		"/api/v1/staff/services/" + tenant.serviceID + "/hooks/example-hooks/self-destruct",
		"/api/v1/staff/services/" + tenant.serviceID + "/hooks/nobody/reboot",
	}
	for i, path := range paths {
		// A distinct key per path: the idempotency store is keyed by route pattern and
		// body, so reusing one key across two paths would replay the first answer.
		rec := env.callHook(t, path, env.admin, env.adminCSRF, fmt.Sprintf("hook-404-key-%04d", i))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404; body %s", path, rec.Code, rec.Body)
		}
		if code := problemCode(t, rec); code != "hook_not_found" {
			t.Fatalf("%s: code = %q", path, code)
		}
	}
}

// A refusal by the extension is reported as one: 422 with a code a client can branch on,
// not a 500 that suggests the panel is broken.
func TestHookCallReportsAnExtensionRefusal(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil, "EXAMPLE_HOOKS_MODE=reject")

	tenant := env.createTenant(t, "hook-reject@example.test", "10.00")
	rec := env.callHook(t, "/api/v1/staff/services/"+tenant.serviceID+"/hooks/example-hooks/reboot",
		env.admin, env.adminCSRF, "hook-reject-key-0001")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "hook_rejected" {
		t.Fatalf("code = %q, want hook_rejected", code)
	}
}

// The acceptance case for event emission: an order placed through the customer checkout
// reaches a subscribed version-2 extension as a notification.
func TestOrderEventReachesASubscribedExtension(t *testing.T) {
	env := shopEnv(t)
	_, client := env.installHooks(t, nil)

	_, priceID := env.seedProduct(t, "hooks-event", 2500, 0)
	_, _, cookie, csrf := env.buyer(t, "hooks-event@example.test")

	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID, "quantity": 1}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("checkout: status %d, body %s", rec.Code, rec.Body)
	}
	order := decodeOrder(t, rec)

	event := waitForExtensionEvent(t, client, extproto.EventOrderCreated)
	if got, _ := event.Data["order_id"].(string); got != order.ID {
		t.Fatalf("the extension was told about order %q, want %q", got, order.ID)
	}
	if event.CustomerID == "" {
		t.Fatal("the event reached the extension with no customer id")
	}
}

// A dead extension must not delay a checkout, and what it could not receive must be
// dropped and counted rather than queued without bound.
func TestDeadExtensionDoesNotDelayACheckout(t *testing.T) {
	env := shopEnv(t)
	// "stall" answers registration and then stops reading its stdin: from the core's
	// point of view it is alive but will never consume another notification.
	reg, _ := env.installHooks(t, func(h *extproto.Hooks) { h.SetQueueDepth(2) },
		"EXAMPLE_HOOKS_MODE=stall")

	_, priceID := env.seedProduct(t, "hooks-stall", 2500, 0)
	_, _, cookie, csrf := env.buyer(t, "hooks-stall@example.test")

	start := time.Now()
	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": priceID, "quantity": 1}},
	})
	elapsed := time.Since(start)
	if rec.Code != http.StatusCreated {
		t.Fatalf("checkout: status %d, body %s", rec.Code, rec.Body)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a checkout took %s with a dead extension installed; emission is on the request path", elapsed)
	}

	// The queue is bounded: pushing past it drops instead of growing or blocking.
	filler := strings.Repeat("x", 4096)
	start = time.Now()
	for i := 0; i < 400; i++ {
		reg.Emit(extproto.Event{
			Kind: extproto.EventOrderCreated,
			Data: map[string]any{"order_id": "ord", "filler": filler},
		})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("emitting 400 events to a dead extension took %s", elapsed)
	}
	stats := reg.Stats()
	if stats.Dropped == 0 {
		t.Fatalf("nothing was dropped for a dead extension: %+v", stats)
	}
}

// ------------------------------------------------------------------- helpers --

// receivedEvent is one event the fixture reports having received.
type receivedEvent struct {
	Kind       string         `json:"kind"`
	ID         int64          `json:"id"`
	CustomerID string         `json:"customer_id"`
	Data       map[string]any `json:"data"`
}

// waitForExtensionEvent polls the extension until it has received an event of the given
// kind. Delivery is asynchronous by design, so a test has to wait for it rather than
// assert immediately.
func waitForExtensionEvent(t *testing.T, client *extproto.Client, kind string) receivedEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var received struct {
			Events []receivedEvent `json:"events"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := client.Call(ctx, "fixture.received", nil, &received)
		cancel()
		if err != nil {
			t.Fatalf("fixture.received: %v", err)
		}
		for _, ev := range received.Events {
			if ev.Kind == kind {
				return ev
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the extension never received a %q event", kind)
	return receivedEvent{}
}
