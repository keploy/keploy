//go:build !windows

package log

import (
	"os"
	"path/filepath"
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
