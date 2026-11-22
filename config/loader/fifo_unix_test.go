//go:build !windows

package loader

import (
	"testing"

	"golang.org/x/sys/unix"
)

func mkfifo(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mkfifo(p, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
}
