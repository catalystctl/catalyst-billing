package openapi

import (
	"fmt"
	"sort"

	"github.com/catalyst/billing/internal/httpapi"
)

// Routes returns the live route table in the generator's neutral form.
func Routes() []Route {
	descriptions := httpapi.DescribeRoutes()
	out := make([]Route, 0, len(descriptions))
	for _, d := range descriptions {
		out = append(out, Route{
			Method: d.Method, Pattern: d.Pattern, Access: d.Access,
			Permission: d.Permission, StaffOnly: d.StaffOnly,
			CSRF: d.CSRF, Idempotent: d.Idempotent, Rate: d.Rate,
		})
	}
	return out
}

// Stale returns descriptions for routes that are not served.
//
// It is separate from rendering so the two have different consequences: rendering must
// always succeed (a document that covers the routes that exist is useful), while a stale
// description is an error the test reports, because it means the document describes an
// endpoint nobody can call.
func Stale() []string {
	served := make(map[string]struct{})
	for _, r := range Routes() {
		served[r.Method+" "+r.Pattern] = struct{}{}
	}
	var out []string
	for key := range Operations() {
		if _, ok := served[key]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Missing returns served routes that have no description.
func Missing() []string {
	ops := Operations()
	var out []string
	for _, r := range Routes() {
		key := r.Method + " " + r.Pattern
		if op, ok := ops[key]; !ok || op.Summary == "" {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// FromRoutes renders the document for the live route table.
func FromRoutes() (map[string]any, error) {
	routes := Routes()
	if len(routes) == 0 {
		return nil, fmt.Errorf("openapi: the route table is empty")
	}
	return Build(Spec{Info: DefaultInfo(), Operations: Operations()}, routes), nil
}

// Render returns the document as YAML.
func Render() ([]byte, error) {
	doc, err := FromRoutes()
	if err != nil {
		return nil, err
	}
	return MarshalYAML(doc)
}

// Validate reports every way the document and the router disagree.
//
// It returns one error listing all of the problems rather than the first: a stale document
// usually has several symptoms, and fixing them one per build is worse than fixing them at
// once.
func Validate() error {
	var problems []string
	if stale := Stale(); len(stale) > 0 {
		problems = append(problems, fmt.Sprintf("descriptions for routes that are not served: %v", stale))
	}
	if missing := Missing(); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("served routes with no description: %v", missing))
	}
	if len(problems) > 0 {
		return fmt.Errorf("openapi: %v", problems)
	}
	return nil
}
