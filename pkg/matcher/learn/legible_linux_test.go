//go:build linux

package learn

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A write the disk cuts short (here the process's file size limit) fails,
// saying so, and leaves the file as it was and no temporary file.
func TestAWriteTheDiskCutsShortChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "learned.yaml")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Skip(err)
	}
	cut := lim
	cut.Cur = 4
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cut); err != nil {
		t.Skip(err)
	}
	err := writeReplacing(dir, ".tmp-", path, []byte(strings.Repeat("x", 64)))
	if rerr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil || !strings.Contains(err.Error(), "write "+filepath.Join(dir, ".tmp-")) {
		t.Fatalf("a write cut short: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "before\n" {
		t.Fatalf("the file is now %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files, want the one", len(entries))
	}
}
