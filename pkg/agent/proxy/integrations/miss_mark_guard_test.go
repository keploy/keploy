package integrations

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// missMarkWaivers are the functions outside tests that build a miss with its
// report (models.NewMockMismatchError) and stamp no round on it themselves
// (StampMark), each with where its round is stamped instead, or why it
// carries none. Keyed by the file, from the module's root, and the function
// ("Recv.Name" for a method).
var missMarkWaivers = map[string]string{
	"pkg/agent/proxy/integrations/mysql/replayer/report.go:newConnectionMiss": "a connection-phase miss: the replayer stamps the connection's round on it with markConnectionMiss (replay.go) before it reaches the proxy",
	"pkg/models/mock_mismatch_reporter.go:ReportAnswer":                       "an answered call's report (ErrAnswered), no miss: it counts against no test",
}

// Every miss a parser reports carries the round its call was made in
// (StampMark): the proxy and the agent place a miss by where its call was
// marked, never by when keploy decided it, so a call keploy answers late is
// never charged to the next test. A function that builds a miss with its
// report and stamps no round is listed here, unless it is waived with the
// reason (missMarkWaivers); a waiver of a function that no longer builds an
// unstamped miss fails too, so the list stays true.
func TestEveryMissReportIsStampedWithItsCallsRound(t *testing.T) {
	root := moduleRoot(t)
	const (
		modelsPath       = "go.keploy.io/server/v3/pkg/models"
		integrationsPath = "go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	)
	unstamped := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		// The names this file calls the two packages by; "" for the
		// package itself, whose functions are called bare.
		models, integ := importName(f, modelsPath), importName(f, integrationsPath)
		if dir == "pkg/models" {
			models = ""
		}
		if dir == "pkg/agent/proxy/integrations" {
			integ = ""
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			builds, stamps := false, false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch {
				case calls(call, models, "NewMockMismatchError") && len(call.Args) == 2 && !isNil(call.Args[1]):
					builds = true
				case calls(call, integ, "StampMark"):
					stamps = true
				}
				return true
			})
			if builds && !stamps {
				unstamped[rel+":"+funcName(fd)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, stale []string
	for fn := range unstamped {
		if _, ok := missMarkWaivers[fn]; !ok {
			missing = append(missing, fn)
		}
	}
	for fn := range missMarkWaivers {
		if !unstamped[fn] {
			stale = append(stale, fn)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("these build a miss with its report and stamp no round on it (StampMark with the call's MarkCallRound, taken when the call is made), so a miss keploy decides after the call's test ended is charged to the next test: %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("these waivers name no function that builds an unstamped miss any more: drop them: %s", strings.Join(stale, ", "))
	}
}

// moduleRoot is the directory of the go.mod above the test's.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = up
	}
}

// importName is the name file f calls the package at path by, "-" when it
// does not import it.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return "-"
}

// calls reports whether call calls the function name of the package pkg
// calls by (bare for "").
func calls(call *ast.CallExpr, pkg, name string) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return pkg == "" && fn.Name == name
	case *ast.SelectorExpr:
		x, ok := fn.X.(*ast.Ident)
		return ok && pkg != "" && x.Name == pkg && fn.Sel.Name == name
	}
	return false
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// funcName is fd's name, "Recv.Name" for a method.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
