package provenswitch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// unsaidSwitch are the ways a message keploy says may not name the switch
// (P222, P012). On Linux the proven model is keploy's default, so a user who
// set nothing must not be told a line is KEPLOY_PROVEN's ("(KEPLOY_PROVEN=1)"
// after it), that it happens "under KEPLOY_PROVEN", or that unsetting the
// variable undoes it. A message names the variable only to say how to set it
// (KEPLOY_PROVEN=0, KEPLOY_PROVEN=1), or that an agent ignores it.
var unsaidSwitch = []struct {
	rx  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`\(` + Env + `=1[);]`), "tags the line as the switch's"},
	{regexp.MustCompile(`\(` + Env + `\)`), "tags the line as the variable's"},
	{regexp.MustCompile(`\b(under|with|without|unset) ` + Env + `([^=A-Z_]|$)`), `says "under/with/without/unset KEPLOY_PROVEN", which an unset variable no longer means on Linux`},
	{regexp.MustCompile(`\bwithout ` + Env + `=1`), "says a run without KEPLOY_PROVEN=1 is off, which it is not on Linux"},
	{regexp.MustCompile(Env + ` is not set`), "says the switch is off because the variable is not set, which on Linux it is not"},
	{regexp.MustCompile(`\bstaging (switch|gate)\b`), "calls the switch a staging gate"},
}

// TestNoMessageNamesTheSwitchItDoesNotSet reads every Go file of the module
// but tests and testdata, and fails for each string it can render (a literal,
// a + chain with provenswitch.Env in it, or a format whose arguments include
// provenswitch.Env, each verb read as the variable's name) that names the
// switch in one of the unsaidSwitch ways. Comments are not read.
func TestNoMessageNamesTheSwitchItDoesNotSet(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source")
	}
	root := filepath.Join(filepath.Dir(self), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("%s: %v", path, perr)
			return nil
		}
		files++
		rel, _ := filepath.Rel(root, path)
		for _, s := range renderedStrings(f) {
			for _, u := range unsaidSwitch {
				if u.rx.MatchString(s.text) {
					t.Errorf("%s:%d: %s: %q", rel, fset.Position(s.pos).Line, u.why, s.text)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files under %s; the walk did not reach the module", files, root)
	}
}

type rendered struct {
	pos  token.Pos
	text string
}

// renderedStrings is every string of f the test checks: each literal, each +
// chain that has provenswitch.Env in it (the chain's literals with the
// variable's name between them), and each call whose arguments include
// provenswitch.Env, its first literal argument with every verb read as the
// variable's name.
func renderedStrings(f *ast.File) []rendered {
	var out []rendered
	// inner are the + expressions inside a chain already rendered whole.
	inner := map[*ast.BinaryExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if s, ok := literal(x); ok {
				out = append(out, rendered{x.Pos(), s})
			}
		case *ast.BinaryExpr:
			if x.Op != token.ADD || inner[x] {
				return true
			}
			var b strings.Builder
			hasEnv := false
			var walk func(e ast.Expr)
			walk = func(e ast.Expr) {
				switch y := e.(type) {
				case *ast.ParenExpr:
					walk(y.X)
				case *ast.BinaryExpr:
					if y.Op == token.ADD {
						inner[y] = true
						walk(y.X)
						walk(y.Y)
						return
					}
					b.WriteString("\x00")
				case *ast.BasicLit:
					s, _ := literal(y)
					b.WriteString(s)
				default:
					if isEnv(e) {
						hasEnv = true
						b.WriteString(Env)
						return
					}
					b.WriteString("\x00")
				}
			}
			walk(x)
			if hasEnv {
				out = append(out, rendered{x.Pos(), b.String()})
			}
		case *ast.CallExpr:
			hasEnv := false
			for _, a := range x.Args {
				if isEnv(a) {
					hasEnv = true
				}
			}
			if !hasEnv {
				return true
			}
			for _, a := range x.Args {
				if lit, ok := a.(*ast.BasicLit); ok {
					if s, ok := literal(lit); ok && strings.Contains(s, "%") {
						out = append(out, rendered{lit.Pos(), verb.ReplaceAllString(s, Env)})
					}
					break
				}
			}
		}
		return true
	})
	return out
}

var verb = regexp.MustCompile(`%(\[\d+\])?[sqv]`)

func literal(l *ast.BasicLit) (string, bool) {
	if l.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(l.Value)
	return s, err == nil
}

// isEnv is whether e is provenswitch.Env, or Env inside this package.
func isEnv(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "provenswitch" && x.Sel.Name == "Env"
	case *ast.Ident:
		return x.Name == "Env"
	}
	return false
}
