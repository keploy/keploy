package log

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// inTempDir points the OS temp directory at a directory of the test's own, on
// every platform: TMPDIR on Unix, TMP and TEMP on Windows.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, dir)
	}
	// The user keploy runs for is the test's own, whatever runs it (a sudo
	// that set SUDO_UID): the logs a test makes are that user's.
	t.Setenv("SUDO_UID", "")
	return dir
}

// A log is closed once. The second close of the same log -- enterprise's end
// of a run whose flags already closed it (a machine-output run) -- is a
// no-op: no error, and nothing now at the log's name is removed. Nothing is
// asked of the closed file: on Windows its Stat is an error of its own, not
// os.ErrClosed, and the second close used to return it.
func TestCloseLogFileOfAClosedLogIsANoOp(t *testing.T) {
	inTempDir(t)
	f, _, err := createLogFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseLogFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.Name()); !os.IsNotExist(err) {
		t.Fatalf("the first close left the log (lstat: %v)", err)
	}
	if err := os.WriteFile(f.Name(), []byte("another's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CloseLogFile(f); err != nil {
		t.Errorf("the second close of a log = %v, want a no-op", err)
	}
	if err := RemoveLogFile(f); err != nil {
		t.Errorf("RemoveLogFile of a closed log = %v, want a no-op", err)
	}
	if b, err := os.ReadFile(f.Name()); err != nil || string(b) != "another's\n" {
		t.Errorf("closing a closed log removed or changed the file now at its name: %q, %v", b, err)
	}
}

// A file this process did not create as its log is not its to remove.
func TestCloseLogFileLeavesAFileThatIsNotALog(t *testing.T) {
	dir := inTempDir(t)
	other, err := os.Create(filepath.Join(dir, "not-a-log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if err := RemoveLogFile(other); err != nil {
		t.Errorf("RemoveLogFile of a file that is no log = %v, want a no-op", err)
	}
	if err := CloseLogFile(other); err != nil {
		t.Errorf("CloseLogFile of a file that is no log = %v, want a no-op", err)
	}
	if _, err := os.Lstat(other.Name()); err != nil {
		t.Errorf("closing a file that is no log removed it: %v", err)
	}
}

// A run that failed keeps its log where it is, closed, for the bug report it
// is wanted for, and gets its name to say. (TestKeepLogFile_NamesNothingElse:
// only while its name is still that file.)
func TestKeepLogFile(t *testing.T) {
	t.Run("keeps this run's log at its name", func(t *testing.T) {
		inTempDir(t)
		f, _, err := createLogFile()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("why the run failed\n"); err != nil {
			t.Fatal(err)
		}
		// Its last line two days old: kept now, it is the newest kept log,
		// which the pruning of kept logs ranks by that time.
		lastWritten := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(f.Name(), lastWritten, lastWritten); err != nil {
			t.Fatal(err)
		}
		keptAt := time.Now()
		path, err := KeepLogFile(f)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.Abs(f.Name())
		if path != want {
			t.Errorf("KeepLogFile named %q, want the log's own path %q", path, want)
		}
		if fi, err := os.Lstat(want); err != nil || fi.ModTime().Before(keptAt.Add(-time.Minute)) {
			t.Errorf("the kept log is not stamped with the time it was kept (%v): %v", err, fi)
		}
		if b, err := os.ReadFile(want); err != nil || string(b) != "why the run failed\n" {
			t.Errorf("the kept log holds %q (%v), want the run's line", b, err)
		}
		if _, err := f.WriteString("x"); err == nil {
			t.Error("the kept log was left open")
		}
		if err := CloseLogFile(f); err != nil {
			t.Errorf("closing a kept log again = %v, want a no-op", err)
		}
		if _, err := os.Lstat(want); err != nil {
			t.Errorf("a close after the keep removed the kept log: %v", err)
		}
	})
	t.Run("a run with no log", func(t *testing.T) {
		if path, err := KeepLogFile(nil); path != "" || err != nil {
			t.Errorf("KeepLogFile(nil) = %q, %v", path, err)
		}
	})
}

// deadLog puts, in dir, a log a run kept (by its name), last written age ago.
func deadLog(t *testing.T, dir string, i int, age time.Duration) string {
	t.Helper()
	name := filepath.Join(dir, "keploy-logs-"+strconv.Itoa(1000+i)+".txt")
	if err := os.WriteFile(name, []byte("a failed run's log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(name, at, at); err != nil {
		t.Fatal(err)
	}
	return name
}

func exists(name string) bool {
	_, err := os.Lstat(name)
	return err == nil
}

// The logs failed runs keep are pruned at each run's start, bounded: one goes
// once it is a week old or once ten newer ones are kept beside it, and one
// start removes at most a hundred. A live run's log is never pruned, however
// old its last line, nor one written to in the last minutes, nor a file that
// only resembles a log.
func TestPruneLogFiles(t *testing.T) {
	t.Run("the newest ten are kept", func(t *testing.T) {
		dir := inTempDir(t)
		var logs []string
		for i := 0; i < 12; i++ {
			logs = append(logs, deadLog(t, dir, i, time.Duration(i+1)*time.Hour))
		}
		if n := pruneLogFiles(dir, time.Now()); n != 2 {
			t.Errorf("pruned %d logs, want the 2 past the newest 10", n)
		}
		for i, l := range logs {
			if exists(l) != (i < keptLogsMax) {
				t.Errorf("log %d (%dh old) exists=%v, want %v", i, i+1, exists(l), i < keptLogsMax)
			}
		}
	})
	t.Run("a week old goes", func(t *testing.T) {
		dir := inTempDir(t)
		old := deadLog(t, dir, 0, keptLogsFor+time.Hour)
		recent := deadLog(t, dir, 1, time.Hour)
		if n := pruneLogFiles(dir, time.Now()); n != 1 || exists(old) || !exists(recent) {
			t.Errorf("pruned %d; the week-old log exists=%v, the hour-old one exists=%v; want only the week-old one gone", n, exists(old), exists(recent))
		}
	})
	t.Run("a hundred at most a start", func(t *testing.T) {
		dir := inTempDir(t)
		for i := 0; i < pruneMax+15; i++ {
			deadLog(t, dir, i, keptLogsFor+time.Duration(i+1)*time.Minute)
		}
		if n := pruneLogFiles(dir, time.Now()); n != pruneMax {
			t.Errorf("one start pruned %d logs, want at most %d", n, pruneMax)
		}
		left, _ := filepath.Glob(filepath.Join(dir, LogFilePattern))
		if len(left) != 15 {
			t.Errorf("%d logs left, want 15 for the next start", len(left))
		}
	})
	t.Run("a sealed log: a live run's keeps its seal, a dead one goes", func(t *testing.T) {
		dir := inTempDir(t)
		live, _, err := createLogFile()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Close() })
		dead := deadLog(t, dir, 0, 2*keptLogsFor)
		at := time.Now().Add(-2 * keptLogsFor)
		for _, name := range []string{live.Name(), dead} {
			if err := os.Chtimes(name, at, at); err != nil {
				t.Fatal(err)
			}
			// A full log is sealed read-only (logFileRoom).
			if err := os.Chmod(name, 0o444); err != nil {
				t.Fatal(err)
			}
		}
		if n := pruneLogFiles(dir, time.Now()); n != 1 || exists(dead) {
			t.Errorf("pruned %d; the dead sealed log exists=%v, want it gone", n, exists(dead))
		}
		fi, err := os.Lstat(live.Name())
		if err != nil {
			t.Fatalf("a live run's sealed log was pruned: %v", err)
		}
		if fi.Mode().Perm()&0o200 != 0 {
			t.Errorf("a live run's sealed log lost its seal (mode %v)", fi.Mode())
		}
	})
	t.Run("a live run's log, a quiet one, and a file that is no log", func(t *testing.T) {
		dir := inTempDir(t)
		live, _, err := createLogFile()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Close() })
		at := time.Now().Add(-2 * keptLogsFor)
		if err := os.Chtimes(live.Name(), at, at); err != nil {
			t.Fatal(err)
		}
		quiet := deadLog(t, dir, 0, pruneQuiet/2)
		var older []string
		for i := 1; i <= keptLogsMax; i++ {
			older = append(older, deadLog(t, dir, i, time.Duration(i)*time.Hour))
		}
		other := filepath.Join(dir, "keploy-logs-old.log")
		if err := os.WriteFile(other, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(other, at, at); err != nil {
			t.Fatal(err)
		}
		if n := pruneLogFiles(dir, time.Now()); n != 0 {
			t.Errorf("pruned %d logs, want none", n)
		}
		for _, l := range append([]string{live.Name(), quiet, other}, older...) {
			if !exists(l) {
				t.Errorf("%s was pruned", filepath.Base(l))
			}
		}
	})
}

// One start tries at most a hundred kept logs, removable or not: a temp
// directory of logs it cannot remove (on a file system that refuses the
// lock, say) costs each start no more than one that it can.
func TestPruneLogFilesTriesAHundredAStart(t *testing.T) {
	dir := inTempDir(t)
	for i := 0; i < pruneMax+15; i++ {
		deadLog(t, dir, i, keptLogsFor+time.Duration(i+1)*time.Minute)
	}
	tried := 0
	prev := removeDead
	removeDead = func(string, os.FileInfo) error { tried++; return errors.New("cannot lock it") }
	t.Cleanup(func() { removeDead = prev })
	if n := pruneLogFiles(dir, time.Now()); n != 0 || tried != pruneMax {
		t.Errorf("pruned %d after %d tries, want none after %d", n, tried, pruneMax)
	}
}

// A run whose own log cannot be marked live (a file system that takes no such
// mark) prunes nothing: the other runs' logs there are unmarked too, live ones
// among them.
func TestNew_PrunesNothingWhereItsOwnLogCannotBeMarked(t *testing.T) {
	dir := inTempDir(t)
	old := deadLog(t, dir, 0, 2*keptLogsFor)
	prev := markLive
	markLive = func(*os.File) bool { return false }
	t.Cleanup(func() { markLive = prev })
	_, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	t.Cleanup(func() { _ = CloseLogFile(f) })
	if !exists(old) {
		t.Error("a run whose own log could not be marked pruned another's")
	}
}
