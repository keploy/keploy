package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"

	"github.com/spf13/cobra"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A logger that cannot start stops the CLI with exit code 1 (#4441). log.New
// no longer fails where it cannot open keploy-logs.txt -- it logs to the
// console only -- so the failure is injected.
func TestStart_LoggerInitFailureSetsErrCode(t *testing.T) {
	orig := newLogger
	t.Cleanup(func() { newLogger = orig })
	newLogger = func() (*zap.Logger, *os.File, error) {
		return nil, nil, errors.New("injected logger failure")
	}

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	start(context.Background())

	if utils.ErrCode != 1 {
		t.Fatalf("expected utils.ErrCode = 1 on logger failure, got %d", utils.ErrCode)
	}
}

// A chown -R of the working directory is a destructive thing to do on the
// strength of a guess.
//
// It used to be gated on conf.Path being non-empty, which is true only once
// ValidateFlags has run -- an incidental signal, and one that turned `sudo
// keploy --version` into a recursive chown of the whole tree the moment the
// config default for path stopped being the empty string. Only the commands
// that can CREATE files under that path have anything to restore.
func TestWritesKeployFolder(t *testing.T) {
	// A real command tree, because argv cannot be read by eye: a root flag
	// that takes a separate value puts a non-flag word before the command.
	root := func() *cobra.Command {
		r := &cobra.Command{Use: "keploy"}
		r.PersistentFlags().String("storage-format", "yaml", "")
		r.PersistentFlags().Bool("debug", false, "")
		for _, n := range []string{"record", "test", "normalize", "templatize", "contract", "import", "export", "sanitize", "diff", "config", "login", "update"} {
			c := &cobra.Command{Use: n, Run: func(*cobra.Command, []string) {}}
			r.AddCommand(c)
		}
		mock := &cobra.Command{Use: "mock", Run: func(*cobra.Command, []string) {}}
		mock.AddCommand(&cobra.Command{Use: "record", Run: func(*cobra.Command, []string) {}})
		r.AddCommand(mock)
		return r
	}
	for _, c := range []struct {
		args []string
		want bool
	}{
		// A root flag with a separate value used to be read as the command.
		{[]string{"keploy", "--storage-format", "json", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "--debug", "--storage-format", "json", "test"}, true},
		{[]string{"keploy", "--storage-format", "json", "config", "--generate"}, false},
		{[]string{"keploy", "mock", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "test"}, true},
		{[]string{"keploy", "mock", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "--debug", "record"}, true},
		{[]string{"keploy", "--version"}, false},
		{[]string{"keploy", "--help"}, false},
		{[]string{"keploy"}, false},
		{[]string{"keploy", "config", "--generate"}, false},
		{[]string{"keploy", "config", "defaults"}, false},
		{[]string{"keploy", "login"}, false},
		{[]string{"keploy", "update"}, false},
	} {
		if got := writesKeployFolder(root(), c.args[1:]); got != c.want {
			t.Errorf("writesKeployFolder(%v) = %v, want %v", c.args[1:], got, c.want)
		}
	}
}

// main says its agent records the steps of its whole teardown before the
// command runs, and that the detach is done once the command has returned and
// before the process exits (utils.DeclareTeardownSteps, utils.MarkDetachDone):
// the CLI that stops an agent kills it for want of progress only when it says
// the first, and once it has stopped detaching only when it says the second.
func TestMainReportsItsTeardownSteps(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" {
			body = fn.Body.List
		}
	}
	at := func(pkg, name string) int {
		for i, st := range body {
			es, ok := st.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := es.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && x.Name == pkg && fun.Sel.Name == name {
					return i
				}
			case *ast.Ident:
				if pkg == "" && fun.Name == name {
					return i
				}
			}
		}
		return -1
	}
	declare, start, done, exit := at("utils", "DeclareTeardownSteps"), at("", "start"), at("utils", "MarkDetachDone"), at("os", "Exit")
	if start < 0 || exit < 0 {
		t.Fatalf("main no longer calls start and os.Exit as statements (start=%d exit=%d)", start, exit)
	}
	if declare < 0 || declare > start {
		t.Errorf("main does not call utils.DeclareTeardownSteps before start (at %d, start at %d)", declare, start)
	}
	if done < 0 || done < start || done > exit {
		t.Errorf("main does not call utils.MarkDetachDone after start and before os.Exit (at %d, start %d, exit %d)", done, start, exit)
	}
}
