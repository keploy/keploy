//go:build !windows

package safeyaml

import (
	"testing"

	"golang.org/x/sys/unix"
)

// mkfifo makes a named pipe at p, for the tests that a FIFO in a checkout is
// refused rather than blocked on.
func mkfifo(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mkfifo(p, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
}
