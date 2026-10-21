// Command openapigen writes api/openapi.yaml from the live route table.
//
// It exists so the document can be regenerated with one command (`make openapi`) and
// verified with another (`make openapi-check`). Regenerating by hand is how a spec rots;
// regenerating by command, in CI, is how it cannot.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/catalyst/billing/internal/openapi"
)

func main() {
	out := flag.String("out", filepath.Join("api", "openapi.yaml"), "output path")
	check := flag.Bool("check", false, "verify the file matches the router instead of writing it")
	flag.Parse()

	if *check {
		// Validation first: a document that is current but describes a route that does not
		// exist is worse than a stale one, because it is confidently wrong.
		if err := openapi.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "openapigen:", err)
			os.Exit(1)
		}
	}

	rendered, err := openapi.Render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}

	if *check {
		existing, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "openapigen: %v\nrun `make openapi` to generate it\n", err)
			os.Exit(1)
		}
		if string(existing) != string(rendered) {
			fmt.Fprintf(os.Stderr,
				"openapigen: %s is out of date with the router\nrun `make openapi` and commit the result\n", *out)
			os.Exit(1)
		}
		fmt.Println("openapigen: the document matches the router")
		return
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, rendered, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(rendered))
}
