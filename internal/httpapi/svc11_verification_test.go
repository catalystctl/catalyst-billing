package httpapi_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// SVC-11: server actions declared by an extension, exposed to the client.
//
// The parity row claimed no web view requests GET /api/v1/extensions/hooks, so an extension
// could declare a button that never rendered. That is stale: web/src/views/service.ts asks for
// the hooks, keeps only the actions the caller holds the permission for, and posts to the hook
// call route. These two tests are the guard for that — one reads the view source (the frontend
// is not compiled by `go test`), the other checks the server hides an action from a caller who
// does not hold its permission.
//
// They deliberately do not rebuild the feature. See internal/httpapi/extension_hooks_test.go
// for the end-to-end behaviour of the routes themselves.

// The service screen must consume the hook list, filter it by permission, and call the hook
// route. Any of those three dropping out makes an extension's button either invisible or
// unclickable, which is exactly the gap SVC-11 described.
func TestServiceScreenRendersExtensionActions(t *testing.T) {
	source, err := os.ReadFile("../../web/src/views/service.ts")
	if err != nil {
		t.Fatalf("read the service view: %v", err)
	}
	view := string(source)

	if !strings.Contains(view, `"/api/v1/extensions/hooks"`) {
		t.Fatal("the service screen never requests /api/v1/extensions/hooks, so a declared action can never render")
	}
	// The permission is checked in the client as well as the server. The server filter is the
	// boundary; the client filter is what stops a button appearing that the hook call would
	// refuse, which reads to the operator as a broken panel.
	if !strings.Contains(view, "ctx.can(action.permission)") {
		t.Fatal("the service screen does not filter extension actions by their declared permission")
	}
	if !strings.Contains(view, "/hooks/") {
		t.Fatal("the service screen renders extension actions but never posts to the hook call route")
	}
}

// The server filters the list by the caller's permissions. The action the example extension
// declares asks for service:write:self, which every signed-in account holds (it is part of the
// customer permission set every user gets), so the control case for the *filter* is the
// operator navigation entry: it asks for extension:manage, which support does not hold, and it
// is absent for them while the action is present.
//
// This is the assertion that fails if the filtering is dropped: without it, the list would be
// the raw declaration and an extension could show a button whose call the server would refuse.
func TestHookListIsFilteredByTheCallersPermissions(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	rec := env.get(t, "/api/v1/extensions/hooks", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("hooks list: status %d, body %s", rec.Code, rec.Body)
	}
	hooks := decodeHooks(t, rec.Body.Bytes())

	if len(hooks.ServiceActions) != 1 {
		t.Fatalf("support sees %d extension action(s), want the one they may call",
			len(hooks.ServiceActions))
	}
	if got := hooks.ServiceActions[0]; got.Method != "service.reboot" {
		t.Fatalf("action = %+v, want the declared reboot", got)
	}
	// The navigation entry needs extension:manage, which support does not hold. If this ever
	// comes back non-empty, the filter is gone and every caller sees every declaration.
	if len(hooks.NavItems) != 0 {
		t.Fatalf("support was shown %d operator nav item(s) without extension:manage: %+v",
			len(hooks.NavItems), hooks.NavItems)
	}

	// And the administrator, who does hold it, still sees it: a filter that hides everything
	// from everyone would pass the assertion above for the wrong reason.
	rec = env.get(t, "/api/v1/extensions/hooks", env.admin)
	if admin := decodeHooks(t, rec.Body.Bytes()); len(admin.NavItems) != 1 {
		t.Fatalf("the administrator sees %d nav item(s), want 1", len(admin.NavItems))
	}
}
