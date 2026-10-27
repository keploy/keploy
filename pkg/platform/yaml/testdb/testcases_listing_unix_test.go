//go:build unix

package testdb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// A recorded test set whose tests folder cannot be looked at is an error,
// not a set with no tests: replay used to report it as having nothing to
// run.
func TestGetTestCases_AnUnreadableTestsFolderIsAnError(t *testing.T) {
	t.Run("permission denied", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		tests := filepath.Join(set, "tests")
		if err := os.MkdirAll(tests, 0o755); err != nil {
			t.Fatal(err)
		}
		// 0600: the set folder can be listed but not searched, so stat on
		// its tests folder fails with EACCES.
		if err := os.Chmod(set, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(set, 0o755) })
		if _, err := os.Stat(tests); err == nil {
			t.Skipf("this process (euid %d) can still search a mode-0600 folder, so the permission error cannot be made here; the symlink-loop case covers the same path", os.Geteuid())
		}
		tcs, err := New(zap.NewNop(), root).GetTestCases(context.Background(), "test-set-0")
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("GetTestCases = %d cases, %v; want the permission error", len(tcs), err)
		}
	})
	t.Run("symlink loop", func(t *testing.T) {
		root := t.TempDir()
		tests := filepath.Join(root, "test-set-0", "tests")
		if err := os.MkdirAll(filepath.Dir(tests), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tests, tests); err != nil {
			t.Fatal(err)
		}
		tcs, err := New(zap.NewNop(), root).GetTestCases(context.Background(), "test-set-0")
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("GetTestCases = %d cases, %v; want ELOOP", len(tcs), err)
		}
	})
}

// A set with no tests folder still has no tests, and no error.
func TestGetTestCases_AMissingTestsFolderIsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "test-set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	tcs, err := New(zap.NewNop(), root).GetTestCases(context.Background(), "test-set-0")
	if err != nil || len(tcs) != 0 {
		t.Fatalf("GetTestCases on a set without tests = %d cases, %v; want none, nil", len(tcs), err)
	}
}
