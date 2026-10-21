// Command apiclient generates a typed Go client from api/openapi.yaml.
//
// It exists to prove the document is machine-consumable rather than decorative. A spec
// that no tool reads is a spec that can say anything; a spec a generator compiles against
// is a spec that has to be true.
//
// What it generates, and why it stops there:
//
//   - One method per operation, with path parameters as typed arguments and query
//     parameters as url.Values. The method name comes from the document's operationId, so
//     it cannot drift from the route.
//   - A Result type carrying the status, the raw body and a decoded *Problem. Every
//     failing response in this API is the same envelope, so the client can turn any non-2xx
//     into a typed error without a per-route switch.
//   - The shared schemas the document declares (Amount, Problem, Page, Operation), as
//     structs.
//
// It deliberately does not generate request bodies. The document describes bodies by
// example and rule in prose, not by schema, and a generator that guessed a struct from a
// prose description would produce a client that is confidently wrong. Callers pass a body
// value and get json.Marshal, which is honest about what the generator knows.
package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// support is the hand-written part of the generated package: the shared types and the
// request plumbing. It is kept in a separate file so this generator only has to produce
// the per-operation methods.
//
//go:embed support.go.tmpl
var support string

// document is the subset of the OpenAPI document the generator reads.
type document struct {
	OpenAPI string                          `yaml:"openapi"`
	Info    struct{ Title, Version string } `yaml:"info"`
	Paths   map[string]map[string]operation `yaml:"paths"`
}

type operation struct {
	OperationID string   `yaml:"operationId"`
	Summary     string   `yaml:"summary"`
	Parameters  []param  `yaml:"parameters"`
	Permission  string   `yaml:"x-permission"`
	StaffOnly   bool     `yaml:"x-staff-only"`
	CSRF        bool     `yaml:"x-csrf-required"`
	RateClass   string   `yaml:"x-rate-class"`
	Idempotency *idem    `yaml:"x-idempotency"`
	Tags        []string `yaml:"tags"`
}

type param struct {
	Name     string `yaml:"name"`
	In       string `yaml:"in"`
	Required bool   `yaml:"required"`
}

type idem struct {
	Header   string `yaml:"header"`
	Required bool   `yaml:"required"`
}

func main() {
	out := flag.String("out", filepath.Join("api", "client"), "output directory")
	docPath := flag.String("doc", filepath.Join("api", "openapi.yaml"), "the API document")
	check := flag.Bool("check", false, "verify the generated files are current instead of writing them")
	flag.Parse()

	raw, err := os.ReadFile(*docPath)
	if err != nil {
		fail("read %s: %v", *docPath, err)
	}
	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		fail("parse %s: %v", *docPath, err)
	}
	if len(doc.Paths) == 0 {
		fail("%s declares no paths", *docPath)
	}

	src, err := generate(doc)
	if err != nil {
		fail("generate: %v", err)
	}

	target := filepath.Join(*out, "client.go")
	if *check {
		existing, err := os.ReadFile(target)
		if err != nil {
			fail("%s is missing; run `make client`", target)
		}
		if !bytes.Equal(existing, src) {
			fail("%s is out of date with %s; run `make client`", target, *docPath)
		}
		fmt.Printf("apiclient: %s is current (%d bytes)\n", target, len(src))
		return
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail("mkdir: %v", err)
	}
	if err := os.WriteFile(target, src, 0o644); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", target, len(src))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "apiclient: "+format+"\n", args...)
	os.Exit(1)
}

// method is one generated method.
type method struct {
	ID         string
	Doc        string
	HTTPMethod string
	Path       string
	PathParams []string
	Permission string
	StaffOnly  bool
	CSRF       bool
	// NeedsKey is true when the operation accepts an Idempotency-Key.
	NeedsKey bool
}

func generate(doc document) ([]byte, error) {
	methods := make([]method, 0, 64)
	for path, byMethod := range doc.Paths {
		for verb, op := range byMethod {
			if op.OperationID == "" {
				return nil, fmt.Errorf("%s %s has no operationId", strings.ToUpper(verb), path)
			}
			m := method{
				ID:         op.OperationID,
				HTTPMethod: strings.ToUpper(verb),
				Path:       path,
				Permission: op.Permission,
				StaffOnly:  op.StaffOnly,
				CSRF:       op.CSRF,
				NeedsKey:   op.Idempotency != nil,
			}
			for _, p := range op.Parameters {
				if p.In == "path" {
					m.PathParams = append(m.PathParams, p.Name)
				}
			}
			doc := op.Summary
			if m.Permission != "" {
				doc += "\n//\n// Permission: " + m.Permission + "."
			}
			if m.StaffOnly {
				doc += " Staff only."
			}
			if m.CSRF {
				doc += "\n// The CSRF token is sent from Client.CSRFToken; a session without one is refused."
			}
			if m.NeedsKey {
				doc += "\n// An Idempotency-Key is sent from Client.IdempotencyKey; a money action refuses the request without one."
			}
			m.Doc = doc
			methods = append(methods, m)
		}
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].ID < methods[j].ID })

	// Duplicate operation ids would produce a file that does not compile, which is a
	// confusing way to learn the document is wrong.
	seen := map[string]string{}
	for _, m := range methods {
		if prev, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("operationId %q is used by both %s and %s %s", m.ID, prev, m.HTTPMethod, m.Path)
		}
		seen[m.ID] = m.HTTPMethod + " " + m.Path
	}

	var b strings.Builder
	// The template uses a placeholder for a backtick so the template itself can be a raw
	// string; a struct tag inside a raw string is otherwise unrepresentable.
	b.WriteString(support)
	b.WriteString("\n")

	for _, m := range methods {
		writeMethod(&b, m)
	}

	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		// Returning the unformatted source in the error makes a generator bug diagnosable
		// instead of a mystery "invalid Go".
		return nil, fmt.Errorf("generated source is not valid Go: %w", err)
	}
	return formatted, nil
}

func writeMethod(b *strings.Builder, m method) {
	// Signature: path parameters become typed string arguments, then a query and a body.
	args := make([]string, 0, 4)
	args = append(args, "ctx context.Context")
	paramDocs := make([]string, 0, len(m.PathParams))
	for _, p := range m.PathParams {
		args = append(args, goName(p)+" string")
		paramDocs = append(paramDocs, goName(p)+" is the "+p+" path parameter.")
	}
	args = append(args, "query url.Values", "body any")

	fmt.Fprintf(b, "// %s calls %s %s.\n//\n// %s\n", m.ID, m.HTTPMethod, m.Path, m.Doc)
	for _, d := range paramDocs {
		fmt.Fprintf(b, "// %s\n", d)
	}
	fmt.Fprintf(b, "func (c *Client) %s(%s) (*Result, error) {\n", m.ID, strings.Join(args, ", "))

	// Path substitution, with every parameter escaped: a value that contains a slash must
	// not be able to reach a different route.
	fmt.Fprintf(b, "\tpath := %q\n", m.Path)
	for _, p := range m.PathParams {
		fmt.Fprintf(b, "\tpath = strings.ReplaceAll(path, %q, url.PathEscape(%s))\n",
			"{"+p+"}", goName(p))
	}

	fmt.Fprintf(b, "\treturn c.do(ctx, %q, path, query, body, %v, %v)\n",
		m.HTTPMethod, m.CSRF, m.NeedsKey)
	b.WriteString("}\n\n")
}

// goName turns a path parameter into an exported-ish local name.
func goName(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	name := strings.Join(parts, "")
	if name == "" {
		return "arg"
	}
	// A local variable is lower-cased: "id", not "Id", so a lint pass does not flag it.
	name = strings.ToLower(name[:1]) + name[1:]
	// It must not shadow a package name the method body uses.
	switch name {
	case "path", "query", "body", "ctx", "url", "http", "strings", "client", "result":
		return name + "Value"
	}
	return name
}
