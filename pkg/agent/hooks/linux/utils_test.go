//go:build linux

package linux

import (
	"os"
	"testing"
)

// TestGetSelfInodeNumber verifies that the helper reads /proc/self/ns/pid and
// returns non-zero values for both inode and dev. The dev field is the part
// that breaks across kernel versions when nsfs lands on a different
// unnamed_dev_ida slot (see keploy/enterprise#1940), so an explicit
// non-zero assertion guards against regressions where the field is dropped
// or zeroed before reaching the BPF program.
func TestGetSelfInodeNumber(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/pid"); err != nil {
		t.Skipf("/proc/self/ns/pid not available in this environment: %v", err)
	}

	ino, dev, err := GetSelfInodeNumber()
	if err != nil {
		t.Fatalf("GetSelfInodeNumber returned error: %v", err)
	}
	if ino == 0 {
		t.Errorf("expected non-zero inode for /proc/self/ns/pid, got 0")
	}
	if dev == 0 {
		t.Errorf("expected non-zero dev for /proc/self/ns/pid, got 0")
	}
}
