package procsock_test

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// listingMIBs are the kern.proc sysctls that list processes: each waits,
// uninterruptibly, for every process it lists that is held in an exec or a
// posix_spawn, whoever owns it. kern.proc.pid reads one process the caller
// names, and is not one of them.
var listingMIBs = map[string]bool{
	"KERN_PROC_ALL": true, "KERN_PROC_PGRP": true, "KERN_PROC_SESSION": true, "KERN_PROC_TTY": true,
	"KERN_PROC_UID": true, "KERN_PROC_RUID": true, "KERN_PROC_LCID": true,
}

// mibNumbers are the MIB names a numeric sysctl name is read through, with
// their numbers in <sys/sysctl.h>: {CTL_KERN, KERN_PROC, KERN_PROC_PID, pid}
// reads one process, and {CTL_KERN, KERN_PROC} with any other third number,
// or none, lists them.
var mibNumbers = map[string]int64{"CTL_KERN": 1, "KERN_PROC": 14, "KERN_PROC_PID": 1}

// listingTools are the programs that list processes through kern.proc.all
// on macOS: run from keploy, they wait as keploy's own listing would.
var listingTools = map[string]bool{"ps": true, "pgrep": true, "pkill": true}

// shells run a script given after -c, which may run a listing tool.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// No keploy code lists processes through a kern.proc sysctl: not by name
// (kern.proc.all, kern.proc.pgrp..., or such a name built from pieces), not
// through x/sys/unix's SysctlKinfoProcSlice, which reads a list, not by MIB
// number ({1, 14, 0}, or CTL_KERN and KERN_PROC), and not by running ps,
// pgrep or pkill in code macOS builds. keploy lists them through proc_info
// (procsock.PIDs, GroupPIDs, Info and InfoOrZombie). One held spawn anywhere
// on a Mac held every such listing, and keploy's agent with it, until the
// machine restarted. Tests may still use them, to show the hold.
func TestNoCodeListsProcessesThroughKernProc(t *testing.T) {
	root := moduleRoot(t)
	files := 0
	darwin := build.Default
	darwin.GOOS, darwin.GOARCH, darwin.CgoEnabled = "darwin", "arm64", true
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
		onDarwin, err := darwin.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		for _, problem := range listingsIn(f, onDarwin) {
			t.Errorf("%s: %s", rel, problem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files under %s: not the module", files, root)
	}
}

// listingsIn is every process listing through kern.proc that file f names;
// onDarwin is whether macOS builds it, where a listing tool run from it
// counts too.
func listingsIn(f *ast.File, onDarwin bool) []string {
	consts := intConsts(f)
	var problems []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "SysctlKinfoProcSlice" {
				problems = append(problems, fmt.Sprintf("calls %s, a process list read through a kern.proc sysctl", n.Sel.Name))
			}
		case *ast.Ident:
			if listingMIBs[n.Name] {
				problems = append(problems, fmt.Sprintf("names %s, a kern.proc sysctl that lists processes", n.Name))
			}
		case *ast.BinaryExpr:
			if s, ok := stringValue(n); ok && listsByName(s) {
				problems = append(problems, fmt.Sprintf("builds sysctl name %q, which lists processes", s))
				return false
			}
		case *ast.BasicLit:
			if s, ok := stringValue(n); ok && (listsByName(s) || isPieceOfListingName(s)) {
				problems = append(problems, fmt.Sprintf("names sysctl %q, which lists processes", s))
			}
		case *ast.CompositeLit:
			if listsByNumber(n, consts) {
				problems = append(problems, "names a kern.proc sysctl that lists processes by its MIB numbers")
			}
		case *ast.CallExpr:
			if onDarwin {
				if tool, ok := runsListingTool(n); ok {
					problems = append(problems, fmt.Sprintf("runs %s, which lists processes through kern.proc.all on macOS", tool))
				}
			}
		}
		return true
	})
	return problems
}

// listsByName reports whether s is the name of a kern.proc sysctl other than
// kern.proc.pid: a listing, or one keploy has not read as none.
func listsByName(s string) bool {
	return strings.HasPrefix(s, "kern.proc.") && s != "kern.proc.pid"
}

// isPieceOfListingName reports whether s is the start of such a name and
// nothing more, a piece a listing's name is built from ("kern.", "kern.proc").
func isPieceOfListingName(s string) bool {
	return len(s) >= len("kern.") && strings.HasPrefix("kern.proc.", s)
}

// stringValue is the string a literal, or a sum of literals, is.
func stringValue(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return stringValue(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, okX := stringValue(e.X)
		y, okY := stringValue(e.Y)
		return x + y, okX && okY
	}
	return "", false
}

// intConsts is every integer constant f declares as a literal, or as one of
// the MIB names, by name.
func intConsts(f *ast.File) map[string]int64 {
	consts := map[string]int64{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if v, ok := intValue(vs.Values[i], consts); ok {
						consts[name.Name] = v
					}
				}
			}
		}
	}
	return consts
}

// intValue is the integer e is: a literal, a MIB name, a constant of the
// file, or a conversion of one.
func intValue(e ast.Expr, consts map[string]int64) (int64, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return 0, false
		}
		v, err := strconv.ParseInt(strings.ReplaceAll(e.Value, "_", ""), 0, 64)
		return v, err == nil
	case *ast.Ident:
		if v, ok := mibNumbers[e.Name]; ok {
			return v, true
		}
		v, ok := consts[e.Name]
		return v, ok
	case *ast.SelectorExpr:
		v, ok := mibNumbers[e.Sel.Name]
		return v, ok
	case *ast.ParenExpr:
		return intValue(e.X, consts)
	case *ast.CallExpr:
		if len(e.Args) == 1 {
			return intValue(e.Args[0], consts)
		}
	}
	return 0, false
}

// listsByNumber reports whether lit is a MIB that starts {CTL_KERN,
// KERN_PROC} and is not kern.proc.pid's: its third number is not
// KERN_PROC_PID, or not one this test can read.
func listsByNumber(lit *ast.CompositeLit, consts map[string]int64) bool {
	elts := map[int64]ast.Expr{}
	for i, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			k, ok := intValue(kv.Key, consts)
			if !ok {
				return false
			}
			elts[k] = kv.Value
			continue
		}
		elts[int64(i)] = e
	}
	at := func(i int64) (int64, bool) {
		e, ok := elts[i]
		if !ok {
			return 0, false
		}
		return intValue(e, consts)
	}
	if v, ok := at(0); !ok || v != mibNumbers["CTL_KERN"] {
		return false
	}
	if v, ok := at(1); !ok || v != mibNumbers["KERN_PROC"] {
		return false
	}
	v, ok := at(2)
	return !ok || v != mibNumbers["KERN_PROC_PID"]
}

// runsListingTool reports whether call starts a listing tool: a command
// (exec.Command, CommandContext, StartProcess, ForkExec, Exec) whose program,
// or the first word of the command line it is given, is ps, pgrep or pkill,
// or a shell whose -c script names one.
func runsListingTool(call *ast.CallExpr) (string, bool) {
	var name string
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	case *ast.Ident:
		name = fn.Name
	}
	at := 0
	switch name {
	case "Command", "StartProcess", "ForkExec", "Exec":
	case "CommandContext":
		at = 1
	default:
		return "", false
	}
	if len(call.Args) <= at {
		return "", false
	}
	line, ok := stringValue(call.Args[at])
	if words := strings.Fields(line); !ok || len(words) == 0 {
		return "", false
	} else if program := path.Base(words[0]); listingTools[program] {
		return program, true
	} else if !shells[program] {
		return "", false
	}
	for i := at + 1; i+1 < len(call.Args); i++ {
		// -c, or short flags that hold c (-lc, -ec), takes the script.
		if flag, ok := stringValue(call.Args[i]); !ok || !strings.HasPrefix(flag, "-") ||
			strings.HasPrefix(flag, "--") || !strings.ContainsRune(flag, 'c') {
			continue
		}
		script, _ := stringValue(call.Args[i+1])
		for _, word := range strings.FieldsFunc(script, func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(";|&()`$", r)
		}) {
			if listingTools[path.Base(word)] {
				return path.Base(word), true
			}
		}
	}
	return "", false
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

// Each shape of a listing the guard knows is caught, in a file macOS builds,
// and what reads one process, or runs ps as a docker subcommand, is not.
func TestTheGuardCatchesEachShapeOfAListing(t *testing.T) {
	listings := map[string]string{
		"slice":           `unix.SysctlKinfoProcSlice("kern.proc.all")`,
		"name":            `unix.SysctlRaw("kern.proc.pgrp", 1)`,
		"name pieces":     `unix.SysctlRaw("kern." + "proc.all")`,
		"name piece":      `unix.SysctlRaw(k + "proc.all")` + "\nconst k = \"kern.\"",
		"name prefix":     `unix.SysctlRaw("kern.proc")`,
		"MIB name":        `unix.KERN_PROC_PGRP`,
		"MIB numbers":     `[]int32{1, 14, 0}`,
		"MIB, no third":   `[2]int32{1, 14}`,
		"MIB, unread":     `[]int32{1, 14, which, 0}`,
		"MIB names":       `[]int32{unix.CTL_KERN, unix.KERN_PROC, 2, 0}`,
		"MIB consts":      `[]_C_int{kern, 0xe, int32(0)}` + "\nconst kern = 1",
		"MIB keyed":       `[3]int32{1: 14, 0: 1, 2: 5}`,
		"ps":              `exec.Command("ps", "-ax")`,
		"ps by path":      `exec.CommandContext(ctx, "/bin/ps", "-ax")`,
		"pgrep":           `exec.Command("/usr/bin/pgrep", "-f", "x")`,
		"pkill":           `syscall.Exec("/usr/bin/pkill", nil, nil)`,
		"command line":    `utils.CommandContext(ctx, "ps -ax -o pid")`,
		"shell script":    `exec.Command("/bin/sh", "-c", "sleep 1; ps -ax | grep x")`,
		"bash pgrep":      `exec.CommandContext(ctx, "bash", "-lc", "pgrep keploy")`,
		"zsh substituted": `exec.Command("zsh", "-c", "kill $(pgrep x)")`,
	}
	for what, src := range listings {
		if problems := listingsIn(parseExpr(t, src), true); len(problems) == 0 {
			t.Errorf("%s: %s was not caught", what, src)
		}
	}
	reads := map[string]string{
		"one process":         `unix.SysctlKinfoProc("kern.proc.pid", pid)`,
		"its arguments":       `unix.SysctlRaw("kern.procargs2", pid)`,
		"one process by MIB":  `[]int32{1, 14, 1, pid}`,
		"another kern MIB":    `[]int32{1, 15, 0}`,
		"another sysctl":      `unix.SysctlRaw("kern.osrelease")`,
		"docker ps":           `exec.Command("docker", "ps", "-a")`,
		"a program in a var":  `exec.Command(docker, "ps")`,
		"a shell, no listing": `exec.Command("sh", "-c", "echo ps-free")`,
		"a sum of numbers":    `x := 1 + 14`,
	}
	for what, src := range reads {
		if problems := listingsIn(parseExpr(t, src), true); len(problems) != 0 {
			t.Errorf("%s: %s was caught: %v", what, src, problems)
		}
	}
	if problems := listingsIn(parseExpr(t, `exec.Command("ps", "-e")`), false); len(problems) != 0 {
		t.Errorf("ps in a file macOS does not build was caught: %v", problems)
	}
}

// parseExpr parses src, an expression or statement with any declarations
// after it, as the body of a function in a file of its own.
func parseExpr(t *testing.T, src string) *ast.File {
	t.Helper()
	body, decls, _ := strings.Cut(src, "\n")
	f, err := parser.ParseFile(token.NewFileSet(), "probe.go", "package probe\nfunc probe() {\n"+body+"\n}\n"+decls+"\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return f
}
