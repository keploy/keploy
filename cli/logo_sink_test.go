package cli

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every logo/version-line writer must go through log.PrimarySink(), never a
// hardcoded os.Stdout.
//
// This is a STRUCTURAL pin on purpose. Today the two decisions coincide: the
// only mode that moves the sink to stderr (`--json`, `keploy report --format
// json|junit`) is the same mode that suppresses the logo outright, so a
// hardcoded os.Stdout has no observable effect and no behavioural test can
// catch it — utils/log's own TestPrimarySinkResolvesStdoutLazily and
// provider.TestPrintLogoFollowsTheLoggerSink cover the sink and the function,
// but not these call sites. The invariant is still worth holding: it is what
// makes the stdout/stderr split ONE mechanism instead of a suppression list
// that every future machine-output mode has to remember to extend, which is
// precisely how `--format json|junit` came to print the logo over its own
// NDJSON in the first place.
func TestLogoWritersUseThePrimarySink(t *testing.T) {
	dirs := []string{".", "provider"}

	found := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isPrintLogoCall(call) || len(call.Args) == 0 {
					return true
				}
				found++
				var sb strings.Builder
				if err := printer.Fprint(&sb, fset, call.Args[0]); err != nil {
					t.Fatalf("print %s: %v", path, err)
				}
				if got := sb.String(); got != "log.PrimarySink()" {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: PrintLogo writes to %s; it must write to log.PrimarySink() so the "+
						"logo follows the logger's stdout/stderr split", path, pos.Line, got)
				}
				return true
			})
		}
	}

	// Guard against the test silently covering nothing after a rename.
	if found == 0 {
		t.Fatal("no PrintLogo call sites found; this test is asserting nothing")
	}
	t.Logf("checked %d PrintLogo call sites", found)
}

func isPrintLogoCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "PrintLogo"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "PrintLogo"
	}
	return false
}
