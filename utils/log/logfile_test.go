package log

import (
	"os"
	"path/filepath"
	"testing"
)

// inTempDir points the OS temp directory at a directory of the test's own, on
// every platform: TMPDIR on Unix, TMP and TEMP on Windows.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, dir)
	}
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
