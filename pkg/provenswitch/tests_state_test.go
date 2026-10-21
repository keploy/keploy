package provenswitch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoTestTurnsTheSwitchOffWithAnEmptyValue reads every test file of the
// module, and fails for each call that sets KEPLOY_PROVEN (provenswitch.Env,
// or the name spelled out) to an empty or blank string.
// Before the flip an empty switch was off, and a test set it so to run the
// switch-off paths. On Linux an empty switch is now the platform's default,
// the proven model, so such a test runs the paths it was not written for,
// and fails or, worse, passes on them. A test of the switch-off paths sets
// "0", off on every platform; one of the default unsets the variable
// (os.Unsetenv after t.Setenv). This package's own tests set an empty value
// on purpose, to check that it is the default, and name the switch Env,
// which is not read as it.
func TestNoTestTurnsTheSwitchOffWithAnEmptyValue(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source")
	}
	root := filepath.Join(filepath.Dir(self), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	files, sets := 0, 0
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
		if !strings.HasSuffix(path, "_test.go") {
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
		ast.Inspect(f, func(n ast.Node) bool {
			if setsSwitch(n) {
				sets++
			}
			if v, ok := emptySwitch(n); ok {
				t.Errorf("%s:%d: sets %s to %q, which on Linux is the proven model's default, on: set it to \"0\" to test the switch-off paths, or unset it (os.Unsetenv after t.Setenv) to test the default", rel, fset.Position(n.Pos()).Line, Env, v)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read %d test files under %s; the walk did not reach the module", files, root)
	}
	// The module's tests set the switch some hundreds of times: fewer means
	// the walk did not read them.
	if sets < 100 {
		t.Fatalf("found %d settings of %s in %d files under %s; the walk did not read the module's tests", sets, Env, files, root)
	}
}

// setsSwitch is whether n is a Setenv call that sets the switch.
func setsSwitch(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Setenv" && namesSwitch(call.Args[0])
}

// emptySwitch is whether n is a Setenv call (t.Setenv, os.Setenv) that sets
// the switch to a blank string, and that string.
func emptySwitch(n ast.Node) (string, bool) {
	if !setsSwitch(n) {
		return "", false
	}
	l, ok := n.(*ast.CallExpr).Args[1].(*ast.BasicLit)
	if !ok {
		return "", false
	}
	v, ok := literal(l)
	if !ok || strings.TrimSpace(v) != "" {
		return "", false
	}
	return v, true
}

// namesSwitch is whether e names the switch outside this package:
// provenswitch.Env, or the literal "KEPLOY_PROVEN".
func namesSwitch(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "provenswitch" && x.Sel.Name == "Env"
	case *ast.BasicLit:
		v, ok := literal(x)
		return ok && v == Env
	}
	return false
}

// emptySwitch finds each spelling of an empty switch, and nothing else.
func TestEmptySwitchFindsEachSpellingOfAnEmptySwitch(t *testing.T) {
	for _, c := range []struct {
		call string
		want bool
	}{
		{`t.Setenv(provenswitch.Env, "")`, true},
		{`os.Setenv(provenswitch.Env, "")`, true},
		{`t.Setenv("KEPLOY_PROVEN", "")`, true},
		{`t.Setenv(provenswitch.Env, "  ")`, true},
		{"t.Setenv(provenswitch.Env, ``)", true},
		{`t.Setenv(provenswitch.Env, "0")`, false},
		{`t.Setenv("KEPLOY_PROVEN", "1")`, false},
		{`t.Setenv(provenswitch.Env, v)`, false},
		{`t.Setenv(Env, "")`, false}, // another package's Env
		{`t.Setenv(other.Env, "")`, false},
		{`t.Setenv("KEPLOY_PROVEN_X", "")`, false},
		{`t.Setenv(provenswitch.Other, "")`, false},
		{`os.Unsetenv(provenswitch.Env)`, false},
		{`t.Getenv(provenswitch.Env, "")`, false},
		{`Setenv(provenswitch.Env, "")`, false}, // not a method or package call
		{`t.Setenv(provenswitch.Env)`, false},
		{`x.Setenv(provenswitch.Env, "", y)`, false},
	} {
		e, err := parser.ParseExpr(c.call)
		if err != nil {
			t.Fatalf("%s: %v", c.call, err)
		}
		if _, got := emptySwitch(e); got != c.want {
			t.Errorf("emptySwitch(%s) = %v, want %v", c.call, got, c.want)
		}
	}
}
