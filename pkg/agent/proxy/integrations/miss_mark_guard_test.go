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

// missMarkWaivers are the functions outside tests that report a miss and
// stamp no round on it themselves (StampMark), each with where its round is
// stamped instead, or why it carries none. Keyed by the file, from the module's root, and the function
// ("Recv.Name" for a method).
var missMarkWaivers = map[string]string{
	"pkg/agent/proxy/integrations/mysql/replayer/report.go:newConnectionMiss": "a connection-phase miss: the replayer stamps the connection's round on it with markConnectionMiss (replay.go) before it reaches the proxy",
	"pkg/models/mock_mismatch_reporter.go:ReportAnswer":                       "an answered call's report (ErrAnswered), no miss: it counts against no test",
}

// Every miss a parser reports carries the round its call was made in
// (StampMark): the proxy and the agent place a miss by where its call was
// marked, never by when keploy decided it, so a call keploy answers late is
// never charged to the next test. A function is listed here, unless it is
// waived with the reason (missMarkWaivers), when it:
//   - builds a miss with its report (models.NewMockMismatchError) and stamps
//     no round on it (a report named at the build is stamped by that name);
//   - builds a miss with no report (a nil one): the proxy files it with no
//     round, at the time it reached the proxy;
//   - makes an error of models.ErrNoMockMatched and builds no report for it:
//     returned or reported, the proxy files it with no round as well.
//
// A waiver of a function that does none of these fails too, so the list
// stays true.
func TestEveryMissReportIsStampedWithItsCallsRound(t *testing.T) {
	root := moduleRoot(t)
	const (
		modelsPath       = "go.keploy.io/server/v3/pkg/models"
		integrationsPath = "go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	)
	unstamped := map[string]string{}
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
			if why := missUnstamped(fd.Body, models, integ); why != "" {
				unstamped[rel+":"+funcName(fd)] = why
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, stale []string
	for fn, why := range unstamped {
		if _, ok := missMarkWaivers[fn]; !ok {
			missing = append(missing, fn+" ("+why+")")
		}
	}
	for fn := range missMarkWaivers {
		if _, ok := unstamped[fn]; !ok {
			stale = append(stale, fn)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("these report a miss with no round stamped on it (StampMark with the call's MarkCallRound, taken when the call is made), so a miss keploy decides after the call's test ended is charged to the next test: %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("these waivers name no function that reports an unstamped miss any more: drop them: %s", strings.Join(stale, ", "))
	}
}

// missUnstamped is how body reports a miss with no round stamped on it, ""
// when it reports none so. models and integ are the names its file calls the
// models and integrations packages by ("" for the package itself).
func missUnstamped(body *ast.BlockStmt, models, integ string) string {
	builds, stamps, nilReport, wraps := false, false, false, false
	// The reports built into a miss by name, and those stamped by name: a
	// report built by name is stamped by name, so one site's stamp does not
	// cover another site's report in the same function.
	built, stamped := map[string]bool{}, map[string]bool{}
	// The references to ErrNoMockMatched that only test an error for it
	// (errors.Is): no error is made of them.
	tests := map[ast.Expr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case calls(call, models, "NewMockMismatchError") && len(call.Args) == 2:
			switch r := call.Args[1].(type) {
			case *ast.Ident:
				if r.Name == "nil" {
					nilReport = true
				} else {
					built[r.Name] = true
				}
			default:
				builds = true
			}
		case calls(call, integ, "StampMark") && len(call.Args) == 2:
			stamps = true
			if r, ok := call.Args[0].(*ast.Ident); ok {
				stamped[r.Name] = true
			}
		case calls(call, "errors", "Is") && len(call.Args) == 2:
			tests[call.Args[1]] = true
		}
		return true
	})
	for r := range built {
		builds = true
		if !stamped[r] {
			stamps = false
			break
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && !tests[e] && refers(e, models, "ErrNoMockMatched") {
			wraps = true
		}
		return true
	})
	switch {
	case nilReport:
		return "a miss with a nil report"
	case builds && !stamps:
		return "a report with no round"
	case wraps && !builds:
		return "an ErrNoMockMatched error with no report"
	}
	return ""
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

// calls reports whether call calls the function name of the package its file
// calls pkg (bare for "").
func calls(call *ast.CallExpr, pkg, name string) bool {
	return refers(call.Fun, pkg, name)
}

// refers reports whether e is pkg.name (name, bare, for "").
func refers(e ast.Expr, pkg, name string) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return pkg == "" && e.Name == name
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && pkg != "" && x.Name == pkg && e.Sel.Name == name
	}
	return false
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
