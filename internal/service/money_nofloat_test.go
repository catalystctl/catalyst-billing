package service_test

// A guard on the one property that cannot be checked by looking at an answer: that no float
// ever enters the money path.
//
// A float bug does not fail a test — it produces a total that is right most of the time and
// wrong on the customer who complains. So the rule is enforced structurally: the packages that
// compute money are parsed, and any float type, float parse, float format or float conversion in
// them fails the build. There is exactly one exception, `money.Ratio.Float64`, which exists for
// display and is asserted to be confined to its own method body.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moneyPackages are the packages that compute an amount, a period or a proration. A float in any
// of them is a defect regardless of whether a test happens to catch it.
var moneyPackages = []string{
	"../calendar",
	"../money",
	"../billing",
	"../service",
	"../renewal",
}

// displayOnlyFile is the single file allowed to mention a float type, and only inside the
// documented display method (see the check in TestFloatIsConfinedToItsDisplayMethod).
const displayOnlyFile = "../money/ratio.go"

// TestMoneyPathHasNoFloatArithmetic parses every money-critical package and fails on any float
// type, float parse or float format.
func TestMoneyPathHasNoFloatArithmetic(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	checked := 0

	for _, dir := range moneyPackages {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			checked++
			allowFloat := path == displayOnlyFile

			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.Ident:
					if node.Name != "float32" && node.Name != "float64" {
						return true
					}
					if allowFloat {
						// The display method is checked separately and more precisely.
						return true
					}
					t.Errorf("%s: %s appears in a money-computing package; amounts are integer "+
						"minor units and proration is an exact rational (money.Ratio)",
						fset.Position(node.Pos()), node.Name)
				case *ast.SelectorExpr:
					switch node.Sel.Name {
					case "ParseFloat", "FormatFloat", "Float64":
						if allowFloat && node.Sel.Name == "Float64" {
							return true
						}
						t.Errorf("%s: .%s is a float conversion in a money-computing package",
							fset.Position(node.Pos()), node.Sel.Name)
					}
				}
				return true
			})
		}
	}

	if checked < 18 {
		// A guard that silently checked nothing is worse than no guard: it reports success
		// while proving nothing. The five packages above held 18 files when this was written;
		// if the walk stops finding them, the guard is not looking where it thinks it is.
		t.Fatalf("only %d files were scanned; the guard is not looking where it thinks it is", checked)
	}
}

// TestFloatIsConfinedToItsDisplayMethod checks the single exception precisely: in
// money/ratio.go, `float64` may appear only as the result type and body of the documented
// `Float64` display method, and nowhere else in the file.
func TestFloatIsConfinedToItsDisplayMethod(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, displayOnlyFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", displayOnlyFile, err)
	}

	var display *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Float64" {
			display = fn
		}
	}
	if display == nil {
		t.Fatalf("%s no longer defines Float64; if the display escape hatch is gone, remove this "+
			"exception rather than leaving a guard that permits what it was written to forbid",
			displayOnlyFile)
	}

	// Everything inside the display method is permitted (it is the escape hatch), and the
	// result type is part of it.
	allowed := map[token.Pos]bool{}
	ast.Inspect(display.Type, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			allowed[id.Pos()] = true
		}
		return true
	})
	ast.Inspect(display.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Ident:
			allowed[node.Pos()] = true
		case *ast.SelectorExpr:
			allowed[node.Sel.Pos()] = true
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || (id.Name != "float32" && id.Name != "float64") {
			return true
		}
		if !allowed[id.Pos()] {
			t.Errorf("%s: a float type outside the Float64 display method; the escape hatch is "+
				"one method, not a licence for the file", fset.Position(id.Pos()))
		}
		return true
	})
}
