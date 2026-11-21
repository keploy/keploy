package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The application client reads keploy's agent's address only through
// agentAddress, the agent client's one accessor, which a move of the agent's
// port changes (SetOnAgentPortRefused): a read of the address Setup handed
// (opts.AgentURI) elsewhere would reach the agent where it was before the
// move. A source scan of the package's code, its own row beside it.
func TestTheAppReadsTheAgentsAddressOnlyThroughItsAccessor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var reads []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, pos := range setupAddressReads(f) {
			reads = append(reads, fset.Position(pos).String())
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned %d files; the scan did not read the package", scanned)
	}
	if len(reads) > 0 {
		t.Fatalf("the agent's address is read from opts.AgentURI, not through agentAddress, at:\n  %s", strings.Join(reads, "\n  "))
	}
}

// setupAddressReads are the reads of an `opts.AgentURI` field in f outside
// func agentAddress.
func setupAddressReads(f *ast.File) []token.Pos {
	var reads []token.Pos
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "agentAddress" {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AgentURI" {
				return true
			}
			if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "opts" {
				reads = append(reads, sel.Pos())
			}
			return true
		})
	}
	return reads
}

// The scan finds a read of opts.AgentURI anywhere but in agentAddress.
func TestTheAddressScanFindsAReadOutsideTheAccessor(t *testing.T) {
	const src = `package app
func (a *App) agentAddress() string { return a.opts.AgentURI }
func (a *App) other() string { return a.opts.AgentURI }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := setupAddressReads(f); len(got) != 1 {
		t.Fatalf("the scan found %d reads, want the one outside agentAddress", len(got))
	}
}
