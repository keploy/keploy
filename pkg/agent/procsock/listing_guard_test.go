package procsock_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// listingMIBs are the kern.proc sysctls that list processes: each waits,
// uninterruptibly, for every process it lists that is held in an exec or a
// posix_spawn, whoever owns it. kern.proc.pid reads one process the caller
// names, and is not one of them.
var listingMIBs = map[string]bool{
	"KERN_PROC_ALL": true, "KERN_PROC_PGRP": true, "KERN_PROC_SESSION": true, "KERN_PROC_TTY": true,
	"KERN_PROC_UID": true, "KERN_PROC_RUID": true, "KERN_PROC_LCID": true,
}

// No keploy code lists processes through a kern.proc sysctl: not by name
// (kern.proc.all, kern.proc.pgrp...), not through x/sys/unix's
// SysctlKinfoProcSlice, which reads a list, and not by MIB number. keploy
// lists them through proc_info (procsock.PIDs, GroupPIDs and Info). One
// held spawn anywhere on a Mac held every such listing, and keploy's agent
// with it, until the machine restarted. Tests may still use them, to show
// the hold.
func TestNoCodeListsProcessesThroughKernProc(t *testing.T) {
	root := moduleRoot(t)
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "SysctlKinfoProcSlice" {
					t.Errorf("%s: calls %s, a process list read through a kern.proc sysctl", rel, n.Sel.Name)
				}
			case *ast.Ident:
				if listingMIBs[n.Name] {
					t.Errorf("%s: names %s, a kern.proc sysctl that lists processes", rel, n.Name)
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err == nil && strings.HasPrefix(s, "kern.proc.") && s != "kern.proc.pid" {
					t.Errorf("%s: names sysctl %q, which lists processes", rel, s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files under %s: not the module", files, root)
	}
}

// moduleRoot is the directory of the go.mod above this package.
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
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = parent
	}
}
