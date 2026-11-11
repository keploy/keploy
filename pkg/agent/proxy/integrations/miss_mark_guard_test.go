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
	"pkg/agent/proxy/proxy.go:Proxy.sendMockNotFoundErrorFrom":                "the sink every parser's reported miss goes through: the report it files is the one the parser built into the error (models.NewMockMismatchError), and each such site is checked here; an error that carries none is a miss built with no report, which the ErrNoMockMatched rule checks",
	"pkg/models/mock_mismatch_reporter.go:ReportAnswer":                       "the sink every answer goes through: it sends a copy of the report its caller passed, and each caller is checked here for stamping that report",
}

// Every miss a parser reports carries the round its call was made in
// (StampMark): the proxy and the agent place a miss by where its call was
// marked, never by when keploy decided it, so a call keploy answers late is
// never charged to the next test. A function is listed here, unless it is
// waived with the reason (missMarkWaivers), when it:
//   - builds a miss with its report (models.NewMockMismatchError), or reports
//     an answered call (models.ReportAnswer), and stamps no round on its
//     report (a report named at the build is stamped by that name): an
//     answer is listed under the test whose read takes it as a miss is, so
//     it is placed by its mark too;
//   - builds a miss with no report (a nil one): the proxy files it with no
//     round, at the time it reached the proxy;
//   - makes an error of models.ErrNoMockMatched and builds no report for it:
//     returned or reported, the proxy files it with no round as well;
//   - builds a models.ParserError, the proxy's own miss, with a report it
//     stamps no round on (as a miss built with its report above), or with
//     no report at all;
//   - stamps a report only under a condition (an if, a case of a switch or
//     a select) that does not build that report itself: where the
//     condition fails, the report goes unstamped.
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
		t.Errorf("these report a miss or an answer with no round stamped on it (StampMark with the call's MarkCallRound, taken when the call is made), so one keploy decides after the call's test ended is charged to the next test: %s", strings.Join(missing, ", "))
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
	noReport := false
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && refers(lit.Type, models, "ParserError") {
			r := fieldOf(lit, "MismatchReport")
			switch r := r.(type) {
			case nil:
				noReport = true
			case *ast.Ident:
				if r.Name == "nil" {
					nilReport = true
				} else {
					built[r.Name] = true
				}
			default:
				builds = true
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case (calls(call, models, "NewMockMismatchError") || calls(call, models, "ReportAnswer")) && len(call.Args) == 2:
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
	case noReport:
		return "a ParserError miss with no report built at it"
	case stampedUnderCondition(body, integ):
		return "a report stamped only under a condition that does not build it"
	case builds && !stamps:
		return "a report with no round"
	case wraps && !builds:
		return "an ErrNoMockMatched error with no report"
	}
	return ""
}

// fieldOf is the value lit gives its field name, nil where it gives none.
func fieldOf(lit *ast.CompositeLit, name string) ast.Expr {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == name {
				return kv.Value
			}
		}
	}
	return nil
}

// stampedUnderCondition reports whether body stamps a report (StampMark)
// inside an if, or a case of a switch or a select, that does not itself
// give the report its value (a name assigned or declared in it, its init
// included): the report is then unstamped where the condition fails. A
// stamp of anything but a plain name under a condition counts too. A
// function literal's body is its own: what encloses the literal is not
// read as its condition.
func stampedUnderCondition(body *ast.BlockStmt, integ string) bool {
	var stack []ast.Node
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if call, ok := n.(*ast.CallExpr); ok && calls(call, integ, "StampMark") && len(call.Args) == 2 {
		up:
			for i := len(stack) - 1; i >= 0; i-- {
				switch b := stack[i].(type) {
				case *ast.FuncLit:
					break up
				case *ast.IfStmt, *ast.CaseClause, *ast.CommClause:
					r, ok := call.Args[0].(*ast.Ident)
					if !ok || !assigns(b, r.Name) {
						found = true
					}
					break up
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return found
}

// assigns reports whether n assigns or declares the name.
func assigns(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		switch m := m.(type) {
		case *ast.AssignStmt:
			for _, l := range m.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
		case *ast.ValueSpec:
			for _, id := range m.Names {
				if id.Name == name {
					found = true
				}
			}
		}
		return !found
	})
	return found
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
