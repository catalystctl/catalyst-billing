// Package openapi generates the API description from the route table.
//
// The document is not maintained by hand. A hand-written spec rots silently: the day
// someone adds a route and forgets the spec, the generated client is wrong and nothing
// fails. Generating it from the same table that builds the router makes that impossible —
// a route that is not in the table is not served, and a route in the table is always
// documented.
//
// What the generator knows, and what it does not:
//
//   - It knows the method, the path, the permission, whether CSRF and an idempotency key
//     are required, the rate class, and the uniform error envelope (which really is
//     uniform: contracts_internal_test.go proves it route by route).
//   - It does not know request or response schemas. Those live in the handlers, and a
//     generator that guessed them would produce a document that is confidently wrong.
//     A response schema is therefore declared next to the handler in the registry below,
//     so the description is accurate where it exists and honest where it does not.
//
// The result is checked in and verified by a test that compares it with the live route
// table in both directions: a route missing from the document fails, and a documented
// path that the router does not serve fails.
package openapi

import (
	"encoding/json"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Route is the subset of an HTTP route the document needs.
//
// It mirrors httpapi.Route rather than importing it, so the API description does not
// depend on the server implementation. The test that compares the two is what keeps them
// in step.
type Route struct {
	Method     string
	Pattern    string
	Access     string
	Permission string
	StaffOnly  bool
	CSRF       bool
	Idempotent bool
	Rate       string
}

// Response is a documented response for an operation.
type Response struct {
	Status      int
	Description string
	// Schema is a JSON-Schema fragment, or nil when the handler's body is described only
	// by its status.
	Schema map[string]any
}

// Operation describes what a handler returns. Entries are keyed by "METHOD /pattern".
type Operation struct {
	Summary     string
	Description string
	// RequestSchema is a JSON-Schema fragment for the body, when the route has one.
	RequestSchema map[string]any
	Responses     []Response
	// Permission overrides the route's declared permission in the document, for the rare
	// route whose permission depends on the action (approvals).
	Permission string
}

// Spec is the document plus the operations that describe it.
type Spec struct {
	Info       Info
	Operations map[string]Operation
}

// Info is the document header.
type Info struct {
	Title       string
	Version     string
	Description string
	Servers     []string
}

// ref helpers keep the document readable.

// AmountSchema is the wire shape of money: integer minor units plus an explicit
// currency. There is no float and no decimal string anywhere in the API.
func AmountSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"minor", "currency", "scale"},
		"properties": map[string]any{
			"minor":    map[string]any{"type": "integer", "format": "int64"},
			"currency": map[string]any{"type": "string", "pattern": "^[A-Z0-9]{2,12}$"},
			"scale":    map[string]any{"type": "integer"},
			"display":  map[string]any{"type": "string"},
		},
	}
}

// Ref builds a JSON reference to a component schema, so a repeated shape stays one
// definition in the document instead of a hundred copies.
func Ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// ProblemRef references the error envelope.
func ProblemRef() map[string]any { return Ref("Problem") }

// AmountRef references the money shape.
func AmountRef() map[string]any { return Ref("Amount") }

// OperationRef references the long-action resource.
func OperationRef() map[string]any { return Ref("Operation") }

// ProblemSchema is the error envelope every failing response uses.
func ProblemSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"type", "title", "status", "code"},
		"properties": map[string]any{
			"type":   map[string]any{"type": "string"},
			"title":  map[string]any{"type": "string"},
			"status": map[string]any{"type": "integer"},
			"detail": map[string]any{"type": "string"},
			"code":   map[string]any{"type": "string", "description": "stable machine code"},
		},
	}
}

// PageSchema describes the list envelope.
func PageSchema(item map[string]any) map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"data", "next_cursor"},
		"properties": map[string]any{
			"data":        map[string]any{"type": "array", "items": item},
			"next_cursor": map[string]any{"type": []string{"string", "null"}},
		},
	}
}

// OperationSchema is the long-action resource (E5).
func OperationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":               map[string]any{"type": "string", "format": "uuid"},
			"kind":             map[string]any{"type": "string"},
			"status":           map[string]any{"type": "string"},
			"attempts":         map[string]any{"type": "integer"},
			"last_error":       map[string]any{"type": "string"},
			"lease_expires_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"status_url":       map[string]any{"type": "string"},
		},
	}
}

// AcceptedSchema is the 202 body every enqueuing action returns.
func AcceptedSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"operation_id", "status_url"},
		"properties": map[string]any{
			"operation_id": map[string]any{"type": "string", "format": "uuid"},
			"status_url":   map[string]any{"type": "string"},
		},
	}
}

// DefaultResponses is what every route can return regardless of what it does.
func DefaultResponses() []Response {
	return []Response{
		{Status: 400, Description: "The request is malformed", Schema: ProblemRef()},
		{Status: 401, Description: "Authentication is required", Schema: ProblemRef()},
		{Status: 403, Description: "The caller lacks the permission, or the CSRF token is missing",
			Schema: ProblemRef()},
		{Status: 404, Description: "No such record", Schema: ProblemRef()},
		{Status: 429, Description: "The request budget for this route class is exhausted",
			Schema: ProblemRef()},
		{Status: 500, Description: "An internal error occurred", Schema: ProblemRef()},
	}
}

// securityFor renders the security requirement for a route.
func securityFor(r Route) []map[string][]string {
	if r.Access == "public" {
		return nil
	}
	return []map[string][]string{{"sessionCookie": {}}, {"bearerKey": {}}}
}

// operationID derives a stable id from the method and path.
//
// It is derived rather than authored so it cannot drift: "GET /api/v1/staff/customers/{id}"
// becomes "getStaffCustomersById". A generated client uses it as the method name.
func operationID(r Route) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(r.Method))
	for i, seg := range strings.Split(strings.Trim(r.Pattern, "/"), "/") {
		if seg == "" {
			continue
		}
		// The "/api/v1" prefix is the same on almost every route, so repeating it in every
		// method name is noise: "GetPortalInvoices" reads better than
		// "GetApiV1PortalInvoices" and says the same thing.
		if i < 2 && (seg == "api" || seg == "v1") {
			continue
		}
		if strings.HasPrefix(seg, "{") {
			name := strings.Trim(seg, "{}")
			b.WriteString("By")
			b.WriteString(strings.ToUpper(name[:1]))
			b.WriteString(name[1:])
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]))
		b.WriteString(seg[1:])
	}
	// Sanitise anything that is not an identifier character.
	out := make([]rune, 0, b.Len())
	for _, ch := range b.String() {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			out = append(out, ch)
		}
	}
	// An exported identifier, so a generated client can expose it.
	if len(out) > 0 && out[0] >= 'a' && out[0] <= 'z' {
		out[0] = out[0] - 'a' + 'A'
	}
	return string(out)
}

// Build renders the document.
func Build(spec Spec, routes []Route) map[string]any {
	paths := map[string]any{}
	// A deterministic key order keeps the checked-in document diffable: a rebuild that
	// reordered everything would make every real change unreviewable.
	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Pattern != sorted[j].Pattern {
			return sorted[i].Pattern < sorted[j].Pattern
		}
		return sorted[i].Method < sorted[j].Method
	})

	for _, r := range sorted {
		op := spec.Operations[r.Method+" "+r.Pattern]
		permission := r.Permission
		if op.Permission != "" {
			permission = op.Permission
		}

		entry := map[string]any{
			"operationId":  operationID(r),
			"summary":      op.Summary,
			"responses":    renderResponses(op.Responses, r),
			"x-rate-class": r.Rate,
		}
		if op.Description != "" {
			entry["description"] = op.Description
		}
		if permission != "" {
			entry["x-permission"] = permission
		}
		if r.StaffOnly {
			entry["x-staff-only"] = true
		}
		if r.CSRF {
			entry["x-csrf-required"] = true
		}
		if r.Idempotent {
			entry["x-idempotency"] = map[string]any{
				"header":   "Idempotency-Key",
				"required": r.Rate == "mutation" && permission != "",
			}
		}
		if sec := securityFor(r); sec != nil {
			entry["security"] = sec
		}
		if op.RequestSchema != nil {
			entry["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{"schema": op.RequestSchema},
				},
			}
		}
		if strings.Contains(r.Pattern, "{") {
			entry["parameters"] = pathParameters(r.Pattern)
		}

		byMethod, _ := paths[r.Pattern].(map[string]any)
		if byMethod == nil {
			byMethod = map[string]any{}
			paths[r.Pattern] = byMethod
		}
		byMethod[strings.ToLower(r.Method)] = entry
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       spec.Info.Title,
			"version":     spec.Info.Version,
			"description": spec.Info.Description,
		},
		"servers": func() []map[string]any {
			out := make([]map[string]any, 0, len(spec.Info.Servers))
			for _, s := range spec.Info.Servers {
				out = append(out, map[string]any{"url": s})
			}
			return out
		}(),
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"sessionCookie": map[string]any{
					"type": "apiKey", "in": "cookie", "name": "billing_session",
					"description": "Server-side session; the CSRF cookie must be echoed in X-CSRF-Token on mutations.",
				},
				"bearerKey": map[string]any{
					"type": "http", "scheme": "bearer",
					"description": "An API key. It can only narrow its owner's permissions, never widen them.",
				},
			},
			"schemas": map[string]any{
				"Amount":    AmountSchema(),
				"Problem":   ProblemSchema(),
				"Operation": OperationSchema(),
			},
		},
		"paths": paths,
	}
}

// renderResponses merges the declared responses with the defaults a route can always
// return.
func renderResponses(declared []Response, r Route) map[string]any {
	out := map[string]any{}
	for _, d := range DefaultResponses() {
		// A public route cannot answer 401 for a missing session; leaving the default in
		// would describe behaviour the route does not have.
		if r.Access == "public" && (d.Status == 401 || d.Status == 403) {
			continue
		}
		out[statusKey(d.Status)] = responseObject(d)
	}
	for _, d := range declared {
		out[statusKey(d.Status)] = responseObject(d)
	}
	return out
}

func responseObject(d Response) map[string]any {
	obj := map[string]any{"description": d.Description}
	if d.Schema != nil {
		obj["content"] = map[string]any{
			"application/json": map[string]any{"schema": d.Schema},
		}
	}
	return obj
}

func statusKey(status int) string {
	switch {
	case status < 200:
		return "1XX"
	case status < 300:
		return string(rune('0'+status/100)) + "XX"
	default:
		return itoa(status)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// pathParameters extracts the {name} segments of a pattern.
func pathParameters(pattern string) []map[string]any {
	var out []map[string]any
	for _, seg := range strings.Split(pattern, "/") {
		if !strings.HasPrefix(seg, "{") {
			continue
		}
		name := strings.Trim(seg, "{}")
		out = append(out, map[string]any{
			"name": name, "in": "path", "required": true,
			"schema": map[string]any{"type": "string"},
		})
	}
	return out
}

// MarshalJSON renders the document as indented JSON.
func MarshalJSON(doc map[string]any) ([]byte, error) {
	return json.MarshalIndent(doc, "", "  ")
}

// MarshalYAML renders the document as YAML.
//
// YAML is the checked-in form because it is what a reviewer can read in a diff: a JSON
// document with every property quoted is unreviewable at this size. The test compares the
// YAML against a freshly built document, so the readable form cannot drift from the
// router.
func MarshalYAML(doc map[string]any) ([]byte, error) {
	return yaml.Marshal(doc)
}
