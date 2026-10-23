package httpapi_test

import (
	"net/http"
	"testing"
)

// Extension hook surface: admin resources, pages and checkout configuration (GRW-05).
//
// Paymenter extensions register an admin resource, a page and checkout fields as in-process
// classes. Catalyst's extension is a separate process, so parity here means the same three
// things *declared as data* and validated by the core. These tests fail if a declared surface
// stops reaching the client or stops being permission-filtered.
//
// What is deliberately not covered, because it is not built: no web view renders an admin
// resource or a page yet. The declarations and their filtering are the server half; the
// rendering is a separate frontend task (see the GRW-05 report).

// The example extension declares one resource, one page and one checkout field; an operator
// holding the declared permission sees all three, with the columns the panel needs to draw a
// table.
func TestExtensionSurfacesReachTheClient(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	rec := env.get(t, "/api/v1/extensions/hooks", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("hooks list: status %d, body %s", rec.Code, rec.Body)
	}
	hooks := decodeHooks(t, rec.Body.Bytes())

	if len(hooks.AdminResources) != 1 {
		t.Fatalf("admin resources = %d, want 1: %s", len(hooks.AdminResources), rec.Body)
	}
	res := hooks.AdminResources[0]
	if res.Extension != "example-hooks" || res.ID != "records" ||
		res.Path != "/staff/extensions/example-hooks/records" {
		t.Fatalf("resource = %+v", res)
	}
	if res.Method != "example.records" {
		t.Fatalf("resource method = %q, want the declared capability", res.Method)
	}
	if len(res.Columns) != 2 || res.Columns[0].Key != "name" || res.Columns[0].Type != "text" ||
		!res.Columns[0].Primary {
		t.Fatalf("columns = %+v, want a primary text column and a status column", res.Columns)
	}

	if len(hooks.Pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(hooks.Pages))
	}
	if page := hooks.Pages[0]; page.Path != "/staff/extensions/example-hooks/overview" ||
		page.Permission == "" {
		t.Fatalf("page = %+v", page)
	}

	if len(hooks.CheckoutConfig.Fields) != 1 {
		t.Fatalf("checkout fields = %d, want 1", len(hooks.CheckoutConfig.Fields))
	}
	field := hooks.CheckoutConfig.Fields[0]
	if field.Key != "example.hostname" || field.Type != "text" || !field.Required {
		t.Fatalf("checkout field = %+v", field)
	}
}

// The operator surfaces are filtered by their declared permission, exactly like the navigation
// entries: support does not hold extension:manage, so the resource and the page are absent. The
// checkout field is not filtered, because the person who needs it is a buyer and not an
// operator.
func TestExtensionSurfacesAreFilteredByPermission(t *testing.T) {
	env := shopEnv(t)
	env.installHooks(t, nil)

	rec := env.get(t, "/api/v1/extensions/hooks", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("hooks list: status %d, body %s", rec.Code, rec.Body)
	}
	hooks := decodeHooks(t, rec.Body.Bytes())

	if len(hooks.AdminResources) != 0 {
		t.Fatalf("support was shown %d admin resource(s) without extension:manage",
			len(hooks.AdminResources))
	}
	if len(hooks.Pages) != 0 {
		t.Fatalf("support was shown %d page(s) without extension:manage", len(hooks.Pages))
	}
	if len(hooks.CheckoutConfig.Fields) != 1 {
		t.Fatalf("checkout fields = %d, want 1: checkout configuration is not operator-gated",
			len(hooks.CheckoutConfig.Fields))
	}
}
