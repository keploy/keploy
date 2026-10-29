//go:build !windows

package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// inRun gives a test the two directories a run sees: a temp directory (TMPDIR)
// and a working directory, the user's project. It returns both.
func inRun(t *testing.T) (tmp, project string) {
	t.Helper()
	dir := t.TempDir()
	tmp = filepath.Join(dir, "tmp")
	project = filepath.Join(dir, "project")
	for _, d := range []string{tmp, project} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	t.Chdir(project)
	return tmp, project
}

// newLog is New with a log file, which the test closes.
func newLog(t *testing.T) (*zap.Logger, *os.File) {
	t.Helper()
	logger, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return logger, f
}

// The log is a fresh file in the temp directory, the run's owner's alone, and
// nothing is created in the directory the run works in: a killed run used to
// leave keploy-logs.txt there for git to commit.
func TestNew_CreatesAFreshTempLogAndNothingInTheWorkingDirectory(t *testing.T) {
	tmp, project := inRun(t)
	logger, f := newLog(t)
	logger.Info("a line the run logs")

	if got := filepath.Dir(f.Name()); got != tmp {
		t.Errorf("the log is in %s, want the temp directory %s", got, tmp)
	}
	if ok, _ := filepath.Match(LogFilePattern, filepath.Base(f.Name())); !ok {
		t.Errorf("the log is named %s, want %s", filepath.Base(f.Name()), LogFilePattern)
	}
	fi, err := os.Lstat(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("the log is %v, want a regular file -rw-------", fi.Mode())
	}
	if b, _ := os.ReadFile(f.Name()); !strings.Contains(string(b), "a line the run logs") {
		t.Errorf("the log holds %q, want the run's line", b)
	}
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the run created %s in its working directory", e.Name())
	}
}

// Two runs never share a log, so neither can remove the other's: a status
// command beside a recording used to take the recording's keploy-logs.txt.
func TestNew_TwoRunsKeepTwoLogs(t *testing.T) {
	inRun(t)
	_, a := newLog(t)
	_, b := newLog(t)
	if a.Name() == b.Name() {
		t.Fatalf("two runs share the log %s", a.Name())
	}
	if err := CloseLogFile(b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.Name()); err != nil {
		t.Errorf("one run's end removed the other's log: %v", err)
	}
}

// A keploy-logs.txt a repository carries -- a symlink to a file of the
// user's, as a checkout can commit -- is no longer the log's name: a run
// neither writes through it nor removes it.
func TestNew_LeavesAKeployLogsTxtInTheWorkingDirectoryAlone(t *testing.T) {
	_, project := inRun(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "keploy-logs.txt")); err != nil {
		t.Fatal(err)
	}
	logger, f := newLog(t)
	logger.Info("a line the run logs")
	if err := CloseLogFile(f); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "original\n" {
		t.Errorf("the file keploy-logs.txt links to holds %q: the run wrote to it", b)
	}
	if fi, err := os.Lstat(filepath.Join(project, "keploy-logs.txt")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the run removed or replaced the keploy-logs.txt link (lstat: %v)", err)
	}
}

// A normal end closes the log and removes it; what another put at its name
// is not the run's to remove.
func TestCloseLogFile(t *testing.T) {
	t.Run("removes this run's log", func(t *testing.T) {
		inRun(t)
		_, f := newLog(t)
		if err := CloseLogFile(f); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(f.Name()); !os.IsNotExist(err) {
			t.Errorf("the run's log outlived it (lstat: %v)", err)
		}
		if _, err := f.Write([]byte("x")); err == nil {
			t.Error("the run's log was left open")
		}
	})
	t.Run("leaves a file that replaced it", func(t *testing.T) {
		inRun(t)
		_, f := newLog(t)
		if err := os.Remove(f.Name()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Name(), []byte("another's\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CloseLogFile(f); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(f.Name()); err != nil || string(b) != "another's\n" {
			t.Errorf("the run removed or changed a file that replaced its log: %q, %v", b, err)
		}
	})
	t.Run("a run with no log", func(t *testing.T) {
		if err := CloseLogFile(nil); err != nil {
			t.Errorf("CloseLogFile(nil) = %v", err)
		}
		if err := RemoveLogFile(nil); err != nil {
			t.Errorf("RemoveLogFile(nil) = %v", err)
		}
	})
}

// A process about to exec into another (the sudo re-exec) removes its log's
// name and keeps writing to the open file, which goes with it.
func TestRemoveLogFile_KeepsTheFileOpen(t *testing.T) {
	inRun(t)
	logger, f := newLog(t)
	if err := RemoveLogFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("the log's name is still there (lstat: %v)", err)
	}
	logger.Info("a line after the name went")
	if _, err := f.Write([]byte("x\n")); err != nil {
		t.Errorf("the log was closed: %v", err)
	}
}

func TestReadLogFile_ReadsOnlyThisRunsLog(t *testing.T) {
	inRun(t)
	logger, f := newLog(t)
	logger.Info("a line the run logs")
	if b, err := ReadLogFile(f); err != nil || !strings.Contains(string(b), "a line the run logs") {
		t.Fatalf("ReadLogFile of this run's log = %q, %v", b, err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("not the log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.Name()); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadLogFile(f); err == nil {
		t.Errorf("ReadLogFile read %q through a symlink put at the log's name", b)
	}
	if _, err := ReadLogFile(nil); err == nil {
		t.Error("ReadLogFile(nil) read a log")
	}
}

// A named pipe put at the log's name does not hang the crash report reading
// the log: it is told it is no longer this run's log.
func TestReadLogFile_DoesNotWaitOnANamedPipe(t *testing.T) {
	inRun(t)
	_, f := newLog(t)
	if err := os.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.Name(), 0o644); err != nil {
		t.Skipf("cannot make a named pipe here: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadLogFile(f); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("ReadLogFile read a named pipe as this run's log")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReadLogFile is waiting on the named pipe")
	}
}

// A run that cannot create its log loses the file, never the command: New
// logs to the console only, and the first debug-level logger says why, once.
// A run that keeps its log says nothing about it.
func TestNew_LogsToTheConsoleWhereItCannotCreateTheLog(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, tmp string)
		wantFile bool
	}{
		{name: "a temp directory it may write", setup: func(*testing.T, string) {}, wantFile: true},
		{name: "a temp directory it may not write", setup: func(t *testing.T, tmp string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes a directory whatever its mode")
			}
			if err := os.Chmod(tmp, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })
		}},
		{name: "a temp directory that is missing", setup: func(t *testing.T, tmp string) {
			if err := os.Remove(tmp); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			console, err := os.CreateTemp(t.TempDir(), "console")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(TestOnlyResetSink)
			setPrimarySink(console)
			tmp, _ := inRun(t)
			c.setup(t, tmp)

			logger, f, err := New()
			if err != nil {
				t.Fatalf("New failed, so the command would: %v", err)
			}
			if f != nil {
				_ = CloseLogFile(f)
			}
			if (f != nil) != c.wantFile {
				t.Fatalf("New kept a log file: %v, want %v", f != nil, c.wantFile)
			}
			logger.Info("a line the run logs")
			for i := 0; i < 2; i++ {
				debug, err := ChangeLogLevel(zap.DebugLevel)
				if err != nil {
					t.Fatal(err)
				}
				debug.Debug("a debug line")
			}

			out, err := os.ReadFile(console.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "a line the run logs") {
				t.Errorf("the console lacks the run's line:\n%s", out)
			}
			want := 1
			if c.wantFile {
				want = 0
			}
			if n := strings.Count(string(out), "keeping no log file"); n != want {
				t.Errorf("the console says %d times that no log is kept, want %d:\n%s", n, want, out)
			}
			if !c.wantFile && !strings.Contains(string(out), tmp) {
				t.Errorf("the note does not say why (the create's error, naming %s):\n%s", tmp, out)
			}
		})
	}
}

// Every logger a run rebuilds keeps writing the run's log: --debug
// (ChangeLogLevel), the agent's and --enable-testing's mode prefix (AddMode),
// --disable-ansi (ChangeColorEncoding) and --json (RedirectToStderr) each
// build a new core, and one built on the console alone left the log with
// only the lines logged before the flags were read: an agent's, none.
func TestRebuiltLoggersKeepWritingTheLog(t *testing.T) {
	t.Cleanup(TestOnlyResetSink)
	inRun(t)
	_, f := newLog(t)
	rebuilds := []struct {
		name  string
		build func() (*zap.Logger, error)
	}{
		{"ChangeLogLevel", func() (*zap.Logger, error) { return ChangeLogLevel(zap.DebugLevel) }},
		{"AddMode", func() (*zap.Logger, error) { return AddMode("agent") }},
		{"ChangeColorEncoding", ChangeColorEncoding},
		{"RedirectToStderr", RedirectToStderr},
	}
	for _, r := range rebuilds {
		logger, err := r.build()
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		logger.Info("a line after " + r.name)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rebuilds {
		if !strings.Contains(string(b), "a line after "+r.name) {
			t.Errorf("a logger %s rebuilt does not write the run's log; it holds:\n%s", r.name, b)
		}
	}
}

// asAgentOf makes this test's New the native agent's of a CLI whose log is
// path (ClientLogFileFlag), and gives back the package state New sets.
func asAgentOf(t *testing.T, path string) {
	t.Helper()
	prev := commandLine
	commandLine = func() []string {
		return []string{"keploy", "agent", "--" + ClientLogFileFlag + "=" + path, "--port", "1"}
	}
	t.Cleanup(func() {
		commandLine = prev
		borrowedLog.Store(nil)
		logFileSink.Store(nil)
	})
}

// consoleTo points the logger's console at a file of the test's, which it
// returns, until the test ends.
func consoleTo(t *testing.T) *os.File {
	t.Helper()
	console, err := os.CreateTemp(t.TempDir(), "console")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(TestOnlyResetSink)
	setPrimarySink(console)
	return console
}

func mustWrite(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// One native run is one file: the agent the CLI starts appends its lines to
// the CLI's log, which the CLI goes on appending to too, so neither writes
// over the other's lines, and the agent creates no log of its own and leaves
// the CLI's in place at its end: the CLI removes it at its own.
func TestNew_AgentAppendsToTheCLIsLogAndLeavesItThere(t *testing.T) {
	for _, c := range []struct{ name, sudoUID string }{
		{"started by the CLI's own user", ""},
		{"started by sudo for the CLI's user", strconv.Itoa(os.Getuid())},
	} {
		t.Run(c.name, func(t *testing.T) {
			console := consoleTo(t)
			tmp, _ := inRun(t)
			t.Setenv("SUDO_UID", c.sudoUID)
			cli, notHanded, err := createLogFile()
			if err != nil || notHanded != nil {
				t.Fatal(err, notHanded)
			}
			t.Cleanup(func() { _ = CloseLogFile(cli) })
			mustWrite(t, cli, "cli line 1\n")

			asAgentOf(t, cli.Name())
			logger, f, err := New()
			if err != nil || f == nil || f.Name() != cli.Name() {
				t.Fatalf("New = %v, %v; want the CLI's log %s", f, err, cli.Name())
			}
			logger.Info("agent line 1")
			mustWrite(t, cli, "cli line 2\n")
			logger.Info("agent line 2")

			got := mustRead(t, cli.Name())
			at := -1
			for _, line := range []string{"cli line 1", "agent line 1", "cli line 2", "agent line 2"} {
				i := strings.Index(got, line)
				if i <= at {
					t.Fatalf("the CLI's log does not hold %q after the lines before it, so one writer wrote over the other; it holds:\n%s", line, got)
				}
				at = i
			}
			if logs, _ := filepath.Glob(filepath.Join(tmp, LogFilePattern)); len(logs) != 1 {
				t.Errorf("one run keeps %d logs, want the CLI's alone: %v", len(logs), logs)
			}
			if said := mustRead(t, console.Name()); strings.Contains(said, "a log of its own") {
				t.Errorf("an agent that took the CLI's log says it keeps one of its own:\n%s", said)
			}

			if err := RemoveLogFile(f); err != nil {
				t.Fatal(err)
			}
			if err := CloseLogFile(f); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(cli.Name()); err != nil {
				t.Fatalf("the agent's end removed the CLI's log: %v", err)
			}
			if err := CloseLogFile(cli); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(cli.Name()); !os.IsNotExist(err) {
				t.Errorf("the CLI's end left its log (lstat: %v)", err)
			}
		})
	}
}

// A name the agent must not write is refused, and the agent keeps a log of
// its own, says where, once, and why at debug. A root agent writes the CLI's
// log, so the name must already be a regular file -- a link, a pipe or a
// device is not even opened -- opened without following a link, and the file
// opened must be a regular file of the user that started the agent. A name
// can change between those checks; each case that changes it does so just
// before the open (openFile).
func TestNew_AgentRefusesALogItMayNotWrite(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the owner rows need a uid that is not root's")
	}
	other := strconv.Itoa(os.Getuid() + 1)
	type paths struct{ name, target string }
	regular := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// fifo makes a named pipe at path with a reader on it, so a writer's
	// open would succeed.
	fifo := func(t *testing.T, path string) {
		t.Helper()
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
	}
	cases := []struct {
		name     string
		sudoUID  string
		setup    func(t *testing.T, p paths)
		beforeOp func(t *testing.T, p paths) // just before the open, if one is made
		opened   bool                        // whether the name is opened at all
	}{
		{name: "a link to the CLI's log", setup: func(t *testing.T, p paths) {
			regular(t, p.target)
			if err := os.Symlink(p.target, p.name); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a named pipe", setup: func(t *testing.T, p paths) { fifo(t, p.name) }},
		{name: "a name that is not there", setup: func(*testing.T, paths) {}},
		{name: "a file of another user", sudoUID: other, opened: true, setup: func(t *testing.T, p paths) { regular(t, p.name) }},
		{name: "a SUDO_UID that is no uid", sudoUID: "nobody", setup: func(t *testing.T, p paths) { regular(t, p.name) }},
		{name: "a name removed before it is opened", opened: true,
			setup: func(t *testing.T, p paths) { regular(t, p.name) },
			beforeOp: func(t *testing.T, p paths) {
				if err := os.Remove(p.name); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a name that turns into a link before it is opened", opened: true,
			setup: func(t *testing.T, p paths) { regular(t, p.name); regular(t, p.target) },
			beforeOp: func(t *testing.T, p paths) {
				if err := os.Remove(p.name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p.target, p.name); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a name that turns into a named pipe before it is opened", opened: true,
			setup: func(t *testing.T, p paths) { regular(t, p.name) },
			beforeOp: func(t *testing.T, p paths) {
				if err := os.Remove(p.name); err != nil {
					t.Fatal(err)
				}
				fifo(t, p.name)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			console := consoleTo(t)
			tmp, _ := inRun(t)
			t.Setenv("SUDO_UID", c.sudoUID)
			dir := t.TempDir()
			p := paths{name: filepath.Join(dir, "cli.log"), target: filepath.Join(dir, "target")}
			c.setup(t, p)
			opens := 0
			prev := openFile
			openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if name == p.name {
					opens++
					if c.beforeOp != nil {
						c.beforeOp(t, p)
					}
				}
				return prev(name, flag, perm)
			}
			t.Cleanup(func() { openFile = prev })

			asAgentOf(t, p.name)
			logger, f, err := New()
			if err != nil || f == nil {
				t.Fatalf("New = %v, %v; want a log of the agent's own", f, err)
			}
			t.Cleanup(func() { _ = CloseLogFile(f) })
			if f.Name() == p.name || filepath.Dir(f.Name()) != tmp {
				t.Fatalf("the agent took %s; want a log of its own in %s", f.Name(), tmp)
			}
			if got := opens > 0; got != c.opened {
				t.Errorf("the agent opened %s: %v, want %v", p.name, got, c.opened)
			}
			logger.Info("an agent line")
			if got := mustRead(t, f.Name()); !strings.Contains(got, "an agent line") {
				t.Errorf("the agent's own log holds %q, want its line", got)
			}
			for _, path := range []string{p.name, p.target} {
				// Only a regular file is read: a pipe's open would wait.
				if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
					continue
				}
				if b := mustRead(t, path); b != "kept\n" {
					t.Errorf("the agent wrote %s: it holds %q", path, b)
				}
			}
			if _, err := os.Lstat(p.name); c.name == "a name that is not there" || c.name == "a name removed before it is opened" {
				if !os.IsNotExist(err) {
					t.Errorf("the agent created %s (lstat: %v)", p.name, err)
				}
			}

			said := mustRead(t, console.Name())
			if n := strings.Count(said, "this agent keeps a log of its own"); n != 1 || !strings.Contains(said, f.Name()) {
				t.Errorf("the agent said %d times that it keeps a log of its own, want once, naming %s:\n%s", n, f.Name(), said)
			}
			if _, err := ChangeLogLevel(zap.DebugLevel); err != nil {
				t.Fatal(err)
			}
			said = mustRead(t, console.Name())
			if !strings.Contains(said, "not appending to the CLI's log") || !strings.Contains(said, "reason") {
				t.Errorf("the first debug logger did not say why the CLI's log was refused:\n%s", said)
			}
		})
	}
}

// Only `keploy agent` reads ClientLogFileFlag from its command line, in
// either form, the last one before a "--".
func TestClientLogFileArg(t *testing.T) {
	flag := "--" + ClientLogFileFlag
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"keploy", "record", flag + "=/a"}, ""},
		{[]string{"keploy"}, ""},
		{[]string{"keploy", "agent", "--port", "1"}, ""},
		{[]string{"keploy", "agent", flag + "=/a"}, "/a"},
		{[]string{"keploy", "agent", flag, "/a"}, "/a"},
		{[]string{"keploy", "agent", flag + "=/a", flag, "/b"}, "/b"},
		{[]string{"keploy", "agent", flag, "/a", flag + "=/b"}, "/b"},
		{[]string{"keploy", "agent", flag}, ""},
		{[]string{"keploy", "agent", flag + "="}, ""},
		{[]string{"keploy", "agent", "--", flag + "=/a"}, ""},
	} {
		got, ok := clientLogFileArg(c.args)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("clientLogFileArg(%q) = %q, %v; want %q", c.args, got, ok, c.want)
		}
	}
}

// chownCall is one call of chownFile: the file it changed and to whom.
type chownCall struct {
	f        *os.File
	uid, gid int
}

// recordChown swaps chownFile, for the rest of the test, for one that records
// each call and returns ret, and gives back the calls.
func recordChown(t *testing.T, ret error) *[]chownCall {
	t.Helper()
	calls := &[]chownCall{}
	prev := chownFile
	chownFile = func(f *os.File, uid, gid int) error {
		*calls = append(*calls, chownCall{f, uid, gid})
		return ret
	}
	t.Cleanup(func() { chownFile = prev })
	return calls
}

// Under sudo the log a run creates is handed, at once and by its descriptor,
// to the user sudo ran keploy for (SUDO_UID, with SUDO_GID's group where it is
// set), and stays 0600: that user can read the log the run names, and the
// agent, which appends only to that user's file, appends to it. Without
// SUDO_UID nothing is changed. A hand-over that fails keeps the log, and says
// why (New says it at debug).
func TestCreateLogFile_UnderSudoHandsTheLogToTheSudoUser(t *testing.T) {
	for _, c := range []struct {
		name, uid, gid string
		want           *chownCall
		fail           bool
	}{
		{name: "under sudo", uid: "4242", gid: "4343", want: &chownCall{uid: 4242, gid: 4343}},
		{name: "under sudo, no SUDO_GID", uid: "4242", want: &chownCall{uid: 4242, gid: -1}},
		{name: "without sudo"},
		{name: "a hand-over that fails", uid: "4242", gid: "4343", want: &chownCall{uid: 4242, gid: 4343}, fail: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			inRun(t)
			t.Setenv("SUDO_UID", c.uid)
			t.Setenv("SUDO_GID", c.gid)
			var ret error
			if c.fail {
				ret = syscall.EPERM
			}
			calls := recordChown(t, ret)
			f, notHanded, err := createLogFile()
			if err != nil || f == nil {
				t.Fatalf("createLogFile = %v, %v; want a log", f, err)
			}
			t.Cleanup(func() { _ = CloseLogFile(f) })
			switch {
			case c.want == nil && len(*calls) != 0:
				t.Errorf("a run without SUDO_UID changed its log's owner: %+v", *calls)
			case c.want != nil && (len(*calls) != 1 || (*calls)[0].f != f || (*calls)[0].uid != c.want.uid || (*calls)[0].gid != c.want.gid):
				t.Errorf("the log %s was handed over by %+v; want once, by its descriptor, to uid %d gid %d", f.Name(), *calls, c.want.uid, c.want.gid)
			}
			if (notHanded != nil) != c.fail {
				t.Errorf("createLogFile says %v of the hand-over; want an error: %v", notHanded, c.fail)
			}
			fi, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("the log is %v; want 0600", fi.Mode().Perm())
			}
		})
	}
}

// As root under sudo for another user, the run's log is that user's, 0600, and
// the agent sudo starts for that user appends to it: one run, one file.
// Before, the log was root's, which the user could not read and the agent's
// owner check refused.
func TestNew_UnderSudoTheAgentAppendsToTheSudoUsersLog(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("handing a file to another user needs root")
	}
	const user = 65534
	console := consoleTo(t)
	tmp, _ := inRun(t)
	t.Setenv("SUDO_UID", strconv.Itoa(user))
	t.Setenv("SUDO_GID", strconv.Itoa(user))
	cli, notHanded, err := createLogFile()
	if err != nil || notHanded != nil {
		t.Fatal(err, notHanded)
	}
	t.Cleanup(func() { _ = CloseLogFile(cli) })
	fi, err := cli.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != user || st.Gid != user || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the log is uid %d gid %d %v; want uid %d gid %d 0600", st.Uid, st.Gid, fi.Mode().Perm(), user, user)
	}
	mustWrite(t, cli, "cli line\n")

	asAgentOf(t, cli.Name())
	logger, f, err := New()
	if err != nil || f == nil || f.Name() != cli.Name() {
		t.Fatalf("New = %v, %v; want the CLI's log %s", f, err, cli.Name())
	}
	logger.Info("agent line")
	if got := mustRead(t, cli.Name()); !strings.Contains(got, "cli line\n") || !strings.Contains(got, "agent line") {
		t.Errorf("the log holds %q; want the CLI's line and the agent's", got)
	}
	if logs, _ := filepath.Glob(filepath.Join(tmp, LogFilePattern)); len(logs) != 1 {
		t.Errorf("one run keeps %d logs, want the CLI's alone: %v", len(logs), logs)
	}
	if said := mustRead(t, console.Name()); strings.Contains(said, "a log of its own") {
		t.Errorf("the agent keeps a log of its own:\n%s", said)
	}
	if err := CloseLogFile(f); err != nil {
		t.Fatal(err)
	}
}

// createLogFile opens each name it tries with O_EXCL: a file, a symlink or a
// named pipe already at a name, which any user of a shared /tmp can put there,
// is left as it is, and the next name is used.
func TestCreateLogFile_LeavesWhatIsAtANameAloneAndTriesTheNext(t *testing.T) {
	kept := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, path, target string)
	}{
		{"a regular file", func(t *testing.T, path, _ string) { kept(t, path) }},
		{"a symlink", func(t *testing.T, path, target string) {
			kept(t, target)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a named pipe", func(t *testing.T, path, _ string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp, _ := inRun(t)
			calls := 0
			prev := logFileInfix
			logFileInfix = func() string { calls++; return strconv.Itoa(calls) }
			t.Cleanup(func() { logFileInfix = prev })
			first := filepath.Join(tmp, "keploy-logs-1.txt")
			target := filepath.Join(t.TempDir(), "target")
			c.plant(t, first, target)
			planted, err := os.Lstat(first)
			if err != nil {
				t.Fatal(err)
			}

			f, _, err := createLogFile()
			if err != nil {
				t.Fatalf("createLogFile: %v", err)
			}
			t.Cleanup(func() { _ = CloseLogFile(f) })
			if want := filepath.Join(tmp, "keploy-logs-2.txt"); f.Name() != want {
				t.Errorf("the log is %s, want %s, the next name", f.Name(), want)
			}
			now, err := os.Lstat(first)
			if err != nil || !os.SameFile(planted, now) || now.Mode() != planted.Mode() {
				t.Errorf("what was at %s changed: %v (%v), was %v", first, now, err, planted)
			}
			for _, path := range []string{first, target} {
				if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
					if b := mustRead(t, path); b != "kept\n" {
						t.Errorf("%s holds %q, want what was put there", path, b)
					}
				}
			}
		})
	}
}

// A log is created 0600 whatever the umask. A log whose owner's write bit is
// gone is one keploy sealed when it was full (logFileRoom), so under a umask
// that takes that bit away the run's log would look full from its first line
// and hold nothing.
func TestNew_TheLogIsWrittenWhateverTheUmask(t *testing.T) {
	inRun(t)
	prev := syscall.Umask(0o277)
	t.Cleanup(func() { syscall.Umask(prev) })
	logger, f := newLog(t)
	logger.Info("a line under a strict umask")
	fi, err := os.Stat(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != logFileMode {
		t.Errorf("the log's mode is %v under umask 0277, want %v", fi.Mode().Perm(), logFileMode)
	}
	if got := mustRead(t, f.Name()); !strings.Contains(got, "a line under a strict umask") {
		t.Errorf("the log holds %q, want its line", got)
	}
}

// A name the agent found a regular file can be a named pipe with no reader by
// the time it opens it. The agent's open does not wait for a reader that
// never comes (O_NONBLOCK): it refuses the pipe and keeps a log of its own.
func TestNew_AgentDoesNotWaitOnANamedPipeWithNoReader(t *testing.T) {
	console := consoleTo(t)
	tmp, _ := inRun(t)
	t.Setenv("SUDO_UID", "")
	path := filepath.Join(t.TempDir(), "cli.log")
	if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	prev := openFile
	openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == path {
			if swapErr = os.Remove(path); swapErr == nil {
				swapErr = syscall.Mkfifo(path, 0o600)
			}
		}
		return prev(name, flag, perm)
	}
	t.Cleanup(func() { openFile = prev })
	asAgentOf(t, path)

	type made struct {
		f   *os.File
		err error
	}
	done := make(chan made, 1)
	go func() { _, f, err := New(); done <- made{f, err} }()
	var m made
	select {
	case m = <-done:
	case <-time.After(30 * time.Second):
		// Give the waiting open the reader it waits for, so the test ends.
		if r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			t.Cleanup(func() { _ = r.Close() })
		}
		m = <-done
		t.Errorf("the agent's open of %s waited on a named pipe with no reader", path)
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if m.err != nil || m.f == nil {
		t.Fatalf("New = %v, %v; want a log of the agent's own", m.f, m.err)
	}
	t.Cleanup(func() { _ = CloseLogFile(m.f) })
	if m.f.Name() == path || filepath.Dir(m.f.Name()) != tmp {
		t.Errorf("the agent took %s; want a log of its own in %s", m.f.Name(), tmp)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the named pipe at %s changed: %v, %v", path, fi, err)
	}
	if said := mustRead(t, console.Name()); !strings.Contains(said, "this agent keeps a log of its own") {
		t.Errorf("the agent did not say it keeps a log of its own:\n%s", said)
	}
}

// A crash report carries the end of the log (ReadLogFileTail): no more than
// the bytes asked for, from the start of a line, or the whole log where it is
// shorter; read through its name only while that is still this run's log.
func TestReadLogFileTail(t *testing.T) {
	inRun(t)
	logger, f := newLog(t)
	for i := 0; i < 100; i++ {
		logger.Info(fmt.Sprintf("line %03d of the run", i))
	}
	whole := mustRead(t, f.Name())
	if b, err := ReadLogFileTail(f, int64(len(whole))+10); err != nil || string(b) != whole {
		t.Errorf("the tail of a log shorter than asked for = %q, %v; want the whole log", b, err)
	}
	b, err := ReadLogFileTail(f, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || len(b) > 500 || !strings.HasSuffix(whole, string(b)) || whole[len(whole)-len(b)-1] != '\n' {
		t.Errorf("the last 500 bytes of the log = %q; want the log's end, from the start of a line, within 500 bytes", b)
	}
	if err := os.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Name(), []byte("not the log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadLogFileTail(f, 500); err == nil {
		t.Errorf("ReadLogFileTail read %q, a file put at the log's name", b)
	}
	if _, err := ReadLogFileTail(nil, 500); err == nil {
		t.Error("ReadLogFileTail(nil) read a log")
	}
}
