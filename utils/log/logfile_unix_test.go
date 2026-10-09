//go:build !windows

package log

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// inDir runs the test in a fresh directory holding target, a file that stands
// for one the run must never touch (/etc/passwd), and returns target's path.
func inDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	return target
}

// requireUntouched fails unless target still has its mode and contents.
func requireUntouched(t *testing.T, target string) {
	t.Helper()
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("target mode is %v, want -rw-r--r--: the log's open changed it", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(target); string(b) != "original\n" {
		t.Errorf("target holds %q: the log was written to it", b)
	}
}

// A keploy-logs.txt that is a symlink -- one a cloned repository can carry --
// is not followed: its target keeps its mode and contents, and the run logs
// to the console only.
func TestNew_DoesNotFollowASymlinkedLogFile(t *testing.T) {
	target := inDir(t)
	if err := os.Symlink(target, LogFileName); err != nil {
		t.Fatal(err)
	}
	logger, f, err := New()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("a line the run logs")
	if f != nil {
		_ = f.Close()
		t.Fatal("New opened the symlinked log file")
	}
	requireUntouched(t, target)
	if fi, err := os.Lstat(LogFileName); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced or removed: %v, %v", fi, err)
	}
}

// A keploy-logs.txt that is a hard link to another file is that file: it is
// refused too.
func TestNew_RefusesAHardLinkedLogFile(t *testing.T) {
	target := inDir(t)
	if err := os.Link(target, LogFileName); err != nil {
		t.Skipf("cannot make a hard link here: %v", err)
	}
	logger, f, err := New()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("a line the run logs")
	if f != nil {
		_ = f.Close()
		t.Fatal("New opened a hard link to another file")
	}
	requireUntouched(t, target)
}

// A named pipe planted at the name does not hang the run waiting for a reader.
func TestNew_DoesNotWaitOnANamedPipe(t *testing.T) {
	inDir(t)
	if err := syscall.Mkfifo(LogFileName, 0o644); err != nil {
		t.Skipf("cannot make a named pipe here: %v", err)
	}
	done := make(chan *os.File, 1)
	go func() {
		_, f, _ := New()
		done <- f
	}()
	select {
	case f := <-done:
		if f != nil {
			_ = f.Close()
			t.Fatal("New opened a named pipe as its log")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("New is waiting on the named pipe")
	}
}

// A new log is created owner-writable, readable by others, never 0777.
func TestNew_CreatesTheLogFileNotWorldWritable(t *testing.T) {
	inDir(t)
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	_, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := os.Stat(LogFileName)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != logFileMode {
		t.Errorf("new log mode %v, want %v", fi.Mode().Perm(), os.FileMode(logFileMode))
	}
}

// A log an older keploy left world-writable is narrowed; a stricter one is
// left as it is.
func TestNew_NarrowsAnOldWorldWritableLog(t *testing.T) {
	for _, c := range []struct{ was, want os.FileMode }{{0o777, 0o644}, {0o666, 0o644}, {0o600, 0o600}} {
		t.Run(c.was.String(), func(t *testing.T) {
			inDir(t)
			if err := os.WriteFile(LogFileName, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(LogFileName, c.was); err != nil {
				t.Fatal(err)
			}
			_, f, err := New()
			if err != nil || f == nil {
				t.Fatalf("New = %v, %v; want the log file", f, err)
			}
			_ = f.Close()
			fi, _ := os.Stat(LogFileName)
			if fi.Mode().Perm() != c.want {
				t.Errorf("log mode %v, want %v", fi.Mode().Perm(), c.want)
			}
		})
	}
}

// A log owned by someone other than this user, the user who ran sudo and the
// directory's owner is not this run's to write.
func TestMayKeepLog(t *testing.T) {
	inDir(t)
	t.Setenv("SUDO_UID", "")
	if !mayKeepLog(uint32(os.Geteuid())) {
		t.Error("this user's own log refused")
	}
	other := uint32(os.Geteuid()) + 4242
	if mayKeepLog(other) {
		t.Errorf("a log owned by uid %d, neither this user nor the directory's owner, was kept", other)
	}
}

// The crash report reads the log only while its name is still this run's log:
// a symlink put there afterwards is not followed.
func TestReadLogFile_ReadsOnlyThisRunsLog(t *testing.T) {
	target := inDir(t)
	logger, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	defer func() { _ = f.Close() }()
	logger.Info("a line the run logs")
	if b, err := ReadLogFile(f); err != nil || len(b) == 0 {
		t.Fatalf("ReadLogFile of this run's log = %q, %v", b, err)
	}
	own, _ := f.Stat()
	if !IsLogFile(own) {
		t.Fatal("IsLogFile does not know this run's own log")
	}

	if err := os.Remove(LogFileName); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, LogFileName); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadLogFile(f); err == nil {
		t.Errorf("ReadLogFile read %q through a symlink put at the log's name", b)
	}
	if IsLogFile(own) {
		t.Error("IsLogFile takes a symlink put at the log's name for this run's log")
	}
	if IsLogFile(nil) {
		t.Error("IsLogFile(nil) is true")
	}
}

// A named pipe put at the log's name does not hang the crash report reading
// the log: it is told it is no longer this run's log.
func TestReadLogFile_DoesNotWaitOnANamedPipe(t *testing.T) {
	inDir(t)
	_, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	defer func() { _ = f.Close() }()
	if err := os.Remove(LogFileName); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(LogFileName, 0o644); err != nil {
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

// The name is this run's log only while it IS the log, not a symlink to it:
// a link someone put there is not this run's to remove.
func TestIsLogFile_ALinkToTheLogIsNotTheLog(t *testing.T) {
	inDir(t)
	_, f, err := New()
	if err != nil || f == nil {
		t.Fatalf("New = %v, %v; want a log file", f, err)
	}
	defer func() { _ = f.Close() }()
	own, _ := f.Stat()
	moved := filepath.Join(t.TempDir(), "moved-log")
	if err := os.Rename(LogFileName, moved); err != nil {
		t.Skipf("cannot move the log across directories here: %v", err)
	}
	if err := os.Symlink(moved, LogFileName); err != nil {
		t.Fatal(err)
	}
	if IsLogFile(own) {
		t.Error("IsLogFile takes a symlink to this run's log for the log")
	}
}

// requireRoot skips unless the test runs as root, as the agent does: only root
// can put a file owned by another user in front of New.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root, as the agent runs")
	}
}

// As root, a log another user left in a directory that is not theirs is not
// written: not appended to, not narrowed, not taken over.
func TestNew_AsRootRefusesAnotherUsersLog(t *testing.T) {
	requireRoot(t)
	inDir(t)
	t.Setenv("SUDO_UID", "")
	t.Setenv("SUDO_GID", "")
	if err := os.WriteFile(LogFileName, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(LogFileName, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(LogFileName, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	logger, f, err := New()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("a line the run logs")
	if f != nil {
		_ = f.Close()
		t.Fatal("root opened another user's log")
	}
	fi, _ := os.Stat(LogFileName)
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o666 || st.Uid != 65534 || fi.Size() != 0 {
		t.Errorf("another user's log became %v uid %d size %d; want it untouched", fi.Mode().Perm(), st.Uid, fi.Size())
	}
}

// As root under sudo, the log of the user who ran sudo is kept, and a log root
// creates is theirs, so their next run without sudo can write it.
func TestNew_AsRootUnderSudoTheLogIsTheInvokingUsers(t *testing.T) {
	requireRoot(t)
	t.Setenv("SUDO_UID", "65534")
	t.Setenv("SUDO_GID", "65534")

	t.Run("a new log", func(t *testing.T) {
		inDir(t)
		_, f, err := New()
		if err != nil || f == nil {
			t.Fatalf("New = %v, %v; want a log file", f, err)
		}
		_ = f.Close()
		fi, _ := os.Stat(LogFileName)
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 65534 || st.Gid != 65534 {
			t.Errorf("root's new log belongs to %d:%d, want the sudo user 65534:65534", st.Uid, st.Gid)
		}
	})
	t.Run("the invoking user's log", func(t *testing.T) {
		inDir(t)
		if err := os.WriteFile(LogFileName, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(LogFileName, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		_, f, err := New()
		if err != nil || f == nil {
			t.Fatalf("New = %v, %v; want the sudo user's log kept", f, err)
		}
		_ = f.Close()
	})
}
