package arm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestEveryLocatedKindRendersAsItsGET fails when a kind a group's resource
// list shows has no renderOne case, which would list it with envelope alone.
func TestEveryLocatedKindRendersAsItsGET(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var located, cases []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, v := range n.Values {
					if isLocatedKind(v) {
						located = append(located, n.Names[i].Name)
					}
				}
			case *ast.FuncDecl:
				if n.Name.Name == "renderOne" {
					ast.Inspect(n.Body, func(n ast.Node) bool {
						if c, ok := n.(*ast.CaseClause); ok {
							for _, e := range c.List {
								if id, ok := e.(*ast.Ident); ok {
									cases = append(cases, id.Name)
								}
							}
						}
						return true
					})
				}
			}
			return true
		})
	}
	if len(located) < 20 {
		t.Fatalf("found only these located kinds: %v", located)
	}
	for _, name := range located {
		if !slices.Contains(cases, name) {
			t.Errorf("renderOne has no case for %s", name)
		}
	}
}

// isLocatedKind reports whether v is a &kind{...} literal with located: true.
func isLocatedKind(v ast.Expr) bool {
	u, ok := v.(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return false
	}
	lit, ok := u.X.(*ast.CompositeLit)
	if !ok {
		return false
	}
	if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "kind" {
		return false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, _ := kv.Key.(*ast.Ident); k != nil && k.Name == "located" {
				v, _ := kv.Value.(*ast.Ident)
				return v != nil && v.Name == "true"
			}
		}
	}
	return false
}
