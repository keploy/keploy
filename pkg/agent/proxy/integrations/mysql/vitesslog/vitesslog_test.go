//go:build unix

package vitesslog

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	vtlog "vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/sqlparser"
)

// leakMarker stands for customer content: a card number in a recorded DDL.
const leakMarker = "4111-1111-1111-1111"

// partialDDL is a DDL vitess only partly parses: Parse2 keeps "create table
// customers" and logs "ignoring error parsing DDL '<the whole statement>'" at
// WARNING, which glog writes to a file under os.TempDir().
const partialDDL = "create table customers (card varchar(19) default '" + leakMarker + "', c garbage)"

const childEnv = "KEPLOY_TEST_VITESSLOG_CHILD"

func observe(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	prev := sink.Load()
	SetLogger(zap.New(core))
	t.Cleanup(func() { sink.Store(prev) })
	return logs
}

// Every vitess log function, and a PrefixedLogger's, reaches keploy's logger
// as one line that holds neither its format nor its arguments, and names the
// vitess source line that logged it.
func TestVitessLinesReachTheLoggerWithoutTheirText(t *testing.T) {
	logs := observe(t)
	format := "statement '%s' " + leakMarker
	calls := []func(){
		func() { vtlog.Info("statement ", leakMarker) },
		func() { vtlog.Infof(format, leakMarker) },
		func() { vtlog.InfoDepth(0, leakMarker) },
		func() { vtlog.Warning(leakMarker) },
		func() { vtlog.Warningf(format, leakMarker) },
		func() { vtlog.WarningDepth(0, leakMarker) },
		func() { vtlog.Error(leakMarker) },
		func() { vtlog.Errorf(format, leakMarker) },
		func() { vtlog.ErrorDepth(0, leakMarker) },
		func() { vtlog.NewPrefixedLogger(leakMarker).Warningf(format, leakMarker) },
	}
	for _, call := range calls {
		call()
	}
	if vtlog.V(0) {
		t.Fatal("vitess verbose logging at level 0 is enabled; its lines go straight to glog's files")
	}
	vtlog.V(0).Infof(format, leakMarker)

	entries := logs.AllUntimed()
	if len(entries) != len(calls) {
		t.Fatalf("logger got %d lines for %d vitess log calls: the calls did not all reach it", len(entries), len(calls))
	}
	for i, e := range entries {
		line := fmt.Sprintf("%s %v", e.Message, e.ContextMap())
		if strings.Contains(line, leakMarker) || strings.Contains(line, "'%s'") {
			t.Errorf("line %d holds the logged text: %s", i, line)
		}
		if e.Level != zapcore.DebugLevel || e.Message != withheldMsg {
			t.Errorf("line %d = %v %q, want a debug %q", i, e.Level, e.Message, withheldMsg)
		}
		src, _ := e.ContextMap()["vitessSource"].(string)
		want := "vitesslog/vitesslog_test.go:"
		if i == len(calls)-1 {
			want = "log/log.go:" // a PrefixedLogger logs from its own method
		}
		if !strings.HasPrefix(src, want) {
			t.Errorf("line %d vitessSource = %q, want %s<line>", i, src, want)
		}
	}
}

// Exit and Fatal end the process with glog's codes, after a line that holds
// none of their text.
func TestExitAndFatalEndTheProcessWithoutTheirText(t *testing.T) {
	logs := observe(t)
	var codes []int
	exit = func(code int) { codes = append(codes, code) }
	t.Cleanup(func() { exit = os.Exit })

	vtlog.Exitf("statement '%s'", leakMarker)
	vtlog.ExitDepth(0, leakMarker)
	vtlog.Fatalf("statement '%s'", leakMarker)
	vtlog.FatalDepth(0, leakMarker)

	if fmt.Sprint(codes) != "[1 1 2 2]" {
		t.Fatalf("exit codes = %v, want [1 1 2 2]", codes)
	}
	for i, e := range logs.AllUntimed() {
		line := fmt.Sprintf("%s %v", e.Message, e.ContextMap())
		if strings.Contains(line, leakMarker) || e.Level != zapcore.ErrorLevel {
			t.Errorf("line %d = %v %s, want an error line without the text", i, e.Level, line)
		}
	}
}

// The row: a DDL vitess only partly parses, in a process whose TMPDIR is
// empty, leaves that directory empty and prints no line that holds the
// statement. Without this package's init, glog writes
// vitesslog.test.<host>.<user>.log.WARNING... there, holding the statement,
// and an Errorf prints the statement on stderr too.
func TestPartlyParsedDDLWritesNoFileAndPrintsNoStatement(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		t.Skip("the child runs TestPartlyParsedDDLChild only")
	}
	tmp := t.TempDir()
	out := runChild(t, "TestPartlyParsedDDLChild", tmp)
	if !strings.Contains(out, "vitesslog-child-done") {
		t.Fatalf("child did not finish:\n%s", out)
	}
	if strings.Contains(out, leakMarker) {
		t.Errorf("child printed the statement:\n%s", out)
	}
	assertNoFiles(t, tmp)
}

func TestPartlyParsedDDLChild(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		t.Skip("run by TestPartlyParsedDDLWritesNoFileAndPrintsNoStatement")
	}
	p, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := p.Parse(partialDDL)
	if err != nil {
		t.Fatalf("Parse(%q) = %v, want the partly parsed DDL", partialDDL, err)
	}
	if ddl, ok := stmt.(sqlparser.DDLStatement); !ok || ddl.IsFullyParsed() {
		t.Fatalf("Parse(%q) = %T, want a partly parsed DDL (the path that logs the statement)", partialDDL, stmt)
	}
	// An ERROR line, which glog also prints on stderr.
	vtlog.Errorf("Failed to rewrite expression: %s", partialDDL)
	vtlog.Flush()
	fmt.Println("vitesslog-child-done")
}

// runChild runs one test of this binary in a child process whose TMPDIR is
// tmp, in its own process group, and returns its output.
func runChild(t *testing.T, test, tmp string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+test+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1", "TMPDIR="+tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child %s: %v\n%s", test, err, out.String())
	}
	return out.String()
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		body, _ := os.ReadFile(path)
		t.Errorf("child wrote %s (holds the statement: %v)", path, strings.Contains(string(body), leakMarker))
		return nil
	})
}

// Every non-test file in this module that imports a vitess package also
// imports this one, so its init has run before that vitess code can log.
func TestEveryVitessImporterImportsVitesslog(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	const self = `"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/vitesslog"`
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	importers := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == here {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // not this test's business; the build reports it
		}
		vitess, guarded := false, false
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			vitess = vitess || strings.HasPrefix(p, "vitess.io/vitess/")
			guarded = guarded || imp.Path.Value == self
		}
		if vitess {
			importers++
			if !guarded {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports vitess but not %s: vitess can log a statement to a file before this package's init runs", rel, self)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if importers == 0 {
		t.Fatal("found no file that imports vitess: the walk is broken")
	}
}
