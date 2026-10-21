package openapi_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/openapi"
)

// The API document is generated from the route table, so the two can be compared exactly.
// This test is what makes "the spec is not stale" a fact rather than a hope: it fails when a
// route is added without a description, when a description outlives its route, and when the
// checked-in file differs from what the router would produce today.

// apiDir locates the repository's api directory from the package directory.
func apiDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "api")
		if _, err := os.Stat(filepath.Join(candidate, "openapi.yaml")); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not find api/openapi.yaml above %s", dir)
	return ""
}

// Every served route must be described. A route with no entry produces an empty summary,
// which a generated client renders as a nameless method: the omission has to be an error.
func TestEveryRouteIsDescribed(t *testing.T) {
	ops := openapi.Operations()
	var missing []string
	for _, rt := range httpapi.DescribeRoutes() {
		key := rt.Method + " " + rt.Pattern
		op, ok := ops[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		if strings.TrimSpace(op.Summary) == "" {
			missing = append(missing, key+" (no summary)")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("routes without a description in internal/openapi/descriptions.go:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// The document must describe exactly the served routes: no more, no fewer.
func TestDocumentMatchesRouter(t *testing.T) {
	doc, err := openapi.FromRoutes()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	paths, _ := doc["paths"].(map[string]any)
	documented := make(map[string]struct{})
	for pattern, raw := range paths {
		byMethod, _ := raw.(map[string]any)
		for method := range byMethod {
			documented[strings.ToUpper(method)+" "+pattern] = struct{}{}
		}
	}

	served := make(map[string]struct{})
	for _, rt := range httpapi.DescribeRoutes() {
		served[rt.Method+" "+rt.Pattern] = struct{}{}
	}

	var onlyServed, onlyDocumented []string
	for key := range served {
		if _, ok := documented[key]; !ok {
			onlyServed = append(onlyServed, key)
		}
	}
	for key := range documented {
		if _, ok := served[key]; !ok {
			onlyDocumented = append(onlyDocumented, key)
		}
	}
	if len(onlyServed) > 0 {
		sort.Strings(onlyServed)
		t.Fatalf("served but undocumented:\n  %s", strings.Join(onlyServed, "\n  "))
	}
	if len(onlyDocumented) > 0 {
		sort.Strings(onlyDocumented)
		t.Fatalf("documented but not served:\n  %s", strings.Join(onlyDocumented, "\n  "))
	}
}

// The checked-in file must match the router. Regenerate with `make openapi`.
func TestCheckedInDocumentIsCurrent(t *testing.T) {
	rendered, err := openapi.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := filepath.Join(apiDir(t), "openapi.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(existing) != string(rendered) {
		t.Fatalf("%s is out of date with the router; run `make openapi` and commit the result", path)
	}
}

// Every documented operation must carry the permission and middleware the router actually
// applies. This is what stops the document from becoming a wish list.
func TestDocumentedSecurityMatchesRouter(t *testing.T) {
	doc, err := openapi.FromRoutes()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)

	for _, rt := range httpapi.DescribeRoutes() {
		byMethod, _ := paths[rt.Pattern].(map[string]any)
		if byMethod == nil {
			t.Fatalf("%s is not in the document", rt.Pattern)
		}
		entry, _ := byMethod[strings.ToLower(rt.Method)].(map[string]any)
		if entry == nil {
			t.Fatalf("%s %s is not in the document", rt.Method, rt.Pattern)
		}

		if rt.Permission != "" {
			if got, _ := entry["x-permission"].(string); got != rt.Permission {
				t.Fatalf("%s %s: documented permission %q, router applies %q",
					rt.Method, rt.Pattern, got, rt.Permission)
			}
		}
		if rt.CSRF {
			if v, _ := entry["x-csrf-required"].(bool); !v {
				t.Fatalf("%s %s applies CSRF but the document does not say so", rt.Method, rt.Pattern)
			}
		}
		if rt.StaffOnly {
			if v, _ := entry["x-staff-only"].(bool); !v {
				t.Fatalf("%s %s is staff-only but the document does not say so", rt.Method, rt.Pattern)
			}
		}
		if rt.Idempotent {
			meta, _ := entry["x-idempotency"].(map[string]any)
			if meta == nil {
				t.Fatalf("%s %s accepts an idempotency key but the document does not say so",
					rt.Method, rt.Pattern)
			}
			if meta["header"] != "Idempotency-Key" {
				t.Fatalf("%s %s: idempotency header documented as %v", rt.Method, rt.Pattern, meta["header"])
			}
		}
		// A guarded route must document its authentication requirement, and a public one
		// must not claim one.
		_, hasSecurity := entry["security"]
		if rt.Access == "public" && hasSecurity {
			t.Fatalf("%s %s is public but documents a security requirement", rt.Method, rt.Pattern)
		}
		if rt.Access != "public" && !hasSecurity {
			t.Fatalf("%s %s requires authentication but documents none", rt.Method, rt.Pattern)
		}
	}
}

// Operation ids must be unique and stable: a generated client uses them as method names.
func TestOperationIDsAreUnique(t *testing.T) {
	doc, err := openapi.FromRoutes()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	seen := map[string]string{}
	for pattern, raw := range paths {
		byMethod, _ := raw.(map[string]any)
		for method, entryRaw := range byMethod {
			entry, _ := entryRaw.(map[string]any)
			id, _ := entry["operationId"].(string)
			if id == "" {
				t.Fatalf("%s %s has no operationId", strings.ToUpper(method), pattern)
			}
			if prev, dup := seen[id]; dup {
				t.Fatalf("operationId %q is used by both %s and %s %s", id, prev, strings.ToUpper(method), pattern)
			}
			seen[id] = strings.ToUpper(method) + " " + pattern
		}
	}
}
