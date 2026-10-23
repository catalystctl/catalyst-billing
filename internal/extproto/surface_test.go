package extproto_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/extproto"
)

// The operator surfaces an extension may declare (GRW-05).
//
// Every case here is a declaration the panel would otherwise render badly or dangerously: a
// column type it cannot draw, a method it was never told about, a path that is a link
// injection, a checkout select nobody can answer. The rule under test is that an invalid
// declaration refuses the whole set, because a half-installed hook set is worse than none.

// declared builds the capability and permission lookups a registration would pass in.
func declared(methods ...string) func(string) bool {
	return func(m string) bool {
		for _, want := range methods {
			if m == want {
				return true
			}
		}
		return false
	}
}

func knownPermissions(perms ...string) func(string) bool {
	return func(p string) bool {
		for _, want := range perms {
			if p == want {
				return true
			}
		}
		return false
	}
}

func validResource() extproto.AdminResource {
	return extproto.AdminResource{
		ID:         "records",
		Label:      "Record",
		Plural:     "Records",
		Path:       "/staff/extensions/demo/records",
		Icon:       "plug",
		Permission: "extension:manage",
		Method:     "demo.records",
		Columns: []extproto.AdminColumn{
			{Key: "name", Label: "Name", Type: extproto.ColumnText, Primary: true},
		},
	}
}

func TestValidateSurfaceAcceptsAWellFormedDeclaration(t *testing.T) {
	result := extproto.UIDescribeResult{
		AdminResources: []extproto.AdminResource{validResource()},
		Pages: []extproto.Page{{
			ID: "overview", Label: "Overview", Path: "/staff/extensions/demo/overview",
			Icon: "plug", Permission: "extension:manage", Method: "demo.records",
		}},
		CheckoutFields: []extproto.CheckoutField{{
			Key: "demo.size", Label: "Size", Type: "select",
			Choices: []string{"small", "large"},
		}},
	}
	out, err := result.ValidateSurface(
		declared("demo.records"), knownPermissions("extension:manage"))
	if err != nil {
		t.Fatalf("a valid declaration was refused: %v", err)
	}
	if len(out.AdminResources) != 1 || len(out.Pages) != 1 || len(out.CheckoutFields) != 1 {
		t.Fatalf("normalised declaration = %+v", out)
	}
}

// Each field that makes a declaration unsafe or unrenderable is refused, one case at a time, so
// a failure names the rule rather than "something was wrong".
func TestValidateSurfaceRefusesUnsafeDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*extproto.UIDescribeResult)
		want   string
	}{
		{
			name: "a column type the panel cannot render",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Columns[0].Type = "markdown"
			},
			want: "cannot render",
		},
		{
			name: "a method the extension never advertised",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Method = "demo.secret"
			},
			want: "was not advertised",
		},
		{
			name: "a core protocol method as a resource reader",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Method = "initialize"
			},
			want: "core protocol method",
		},
		{
			name: "a permission the core does not declare",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Permission = "records:read"
			},
			want: "not a permission this core declares",
		},
		{
			name: "a path that is a scheme",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Path = "javascript:alert(1)"
			},
			// Refused for the first rule it breaks (a path must be in-panel, so it starts with
			// a slash); the scheme rule is what catches "/x:y".
			want: "must start with /",
		},
		{
			name: "an in-panel path carrying a scheme",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Path = "/staff/javascript:alert(1)"
			},
			want: "scheme",
		},
		{
			name: "an icon that is a URL",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Icon = "https://evil.example/x.png"
			},
			want: "icon name",
		},
		{
			name: "a resource with no columns",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Columns = nil
			},
			want: "no columns",
		},
		{
			name: "two primary columns",
			mutate: func(r *extproto.UIDescribeResult) {
				r.AdminResources[0].Columns = append(r.AdminResources[0].Columns,
					extproto.AdminColumn{Key: "status", Label: "Status", Type: extproto.ColumnStatus, Primary: true})
			},
			want: "primary",
		},
		{
			name: "a page and a resource on the same path",
			mutate: func(r *extproto.UIDescribeResult) {
				r.Pages = []extproto.Page{{
					ID: "overview", Label: "Overview", Path: r.AdminResources[0].Path,
					Icon: "plug", Permission: "extension:manage", Method: "demo.records",
				}}
			},
			want: "already declared",
		},
		{
			name: "a select with no choices",
			mutate: func(r *extproto.UIDescribeResult) {
				r.CheckoutFields = []extproto.CheckoutField{{
					Key: "demo.size", Label: "Size", Type: "select",
				}}
			},
			want: "no choices",
		},
		{
			name: "a field type the checkout cannot draw",
			mutate: func(r *extproto.UIDescribeResult) {
				r.CheckoutFields = []extproto.CheckoutField{{
					Key: "demo.when", Label: "When", Type: "datetime-local",
				}}
			},
			want: "accepted types",
		},
		{
			name: "two checkout fields with one key",
			mutate: func(r *extproto.UIDescribeResult) {
				r.CheckoutFields = []extproto.CheckoutField{
					{Key: "demo.host", Label: "Host", Type: "text"},
					{Key: "demo.host", Label: "Host again", Type: "text"},
				}
			},
			want: "duplicate key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := extproto.UIDescribeResult{
				AdminResources: []extproto.AdminResource{validResource()},
			}
			tc.mutate(&result)
			_, err := result.ValidateSurface(
				declared("demo.records"), knownPermissions("extension:manage"))
			if err == nil {
				t.Fatalf("the declaration was accepted")
			}
			if !errors.Is(err, extproto.ErrInvalidParams) {
				t.Fatalf("error = %v, want ErrInvalidParams", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The bounds are real: an extension cannot turn a page load into a thousand-row render.
func TestValidateSurfaceBoundsTheDeclarations(t *testing.T) {
	many := make([]extproto.AdminResource, extproto.MaxAdminResources+1)
	for i := range many {
		many[i] = validResource()
		many[i].ID = "records" + strings.Repeat("x", i+1)
		many[i].Path = "/staff/extensions/demo/r" + strings.Repeat("x", i+1)
	}
	result := extproto.UIDescribeResult{AdminResources: many}
	_, err := result.ValidateSurface(declared("demo.records"), knownPermissions("extension:manage"))
	if err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("error = %v, want a limit refusal", err)
	}
}
