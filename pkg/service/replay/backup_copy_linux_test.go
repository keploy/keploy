package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCopyDirContentsHoldsOneFileOpenAtATime is the bound the copy owes a big
// test set: it holds one source and one destination open at a time, not one
// pair per file. With the process's open file limit set just above what the walk
// itself needs, a set of 200 files still copies. The old code deferred both
// handles of every file to the end of the walk, so it ran out of descriptors
// after a handful of them and failed with "too many open files".
func TestCopyDirContentsHoldsOneFileOpenAtATime(t *testing.T) {
	const files = 200

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("read the open file limit: %v", err)
	}
	open, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("count the open descriptors: %v", err)
	}
	// Room for the two handles of the file being copied plus the walk's own.
	bound := uint64(len(open) + 32)
	if lim.Cur < bound {
		t.Skipf("the open file limit is already %d, below the %d this case needs", lim.Cur, bound)
	}

	src, dst := t.TempDir(), t.TempDir()
	want := make(map[string]string, files)
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("test-%d.yaml", i)
		want[name] = fmt.Sprintf("name: %s\n", name)
		if err := os.WriteFile(filepath.Join(src, name), []byte(want[name]), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: bound, Max: lim.Max}); err != nil {
		t.Skipf("lower the open file limit to %d: %v", bound, err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			t.Errorf("restore the open file limit: %v", err)
		}
	})

	r := &Replayer{}
	if err := r.copyDirContents(src, dst); err != nil {
		t.Fatalf("copy %d files with %d descriptors allowed: %v", files, bound, err)
	}

	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("read the copy of %s: %v", name, err)
		}
		if string(got) != content {
			t.Fatalf("the copy of %s is %q, want %q", name, got, content)
		}
	}
}

