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
	"go.uber.org/zap/zaptest/observer"
)

// On unix, an unreadable tests directory or an unreadable test case file must
// return an error from HasTestCases without emitting any ERROR log, so replay's
// pre-scan does not duplicate GetTestCases's ERROR line.
func TestHasTestCases_UnreadableDirAndFileReturnErrorWithoutLogging(t *testing.T) {
	t.Run("unreadable tests directory (permission denied)", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		tests := filepath.Join(set, "tests")
		if err := os.MkdirAll(tests, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(set, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(set, 0o755) })
		if _, err := os.Stat(tests); err == nil {
			t.Skipf("euid %d can still search a mode-0600 folder; symlink-loop covers the same path", os.Geteuid())
		}

		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), root).HasTestCases(context.Background(), "test-set-0")
		if !errors.Is(err, fs.ErrPermission) || has {
			t.Fatalf("HasTestCases = (%v, %v), want (false, fs.ErrPermission)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})

	t.Run("unreadable tests directory (symlink loop)", func(t *testing.T) {
		root := t.TempDir()
		tests := filepath.Join(root, "test-set-0", "tests")
		if err := os.MkdirAll(filepath.Dir(tests), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tests, tests); err != nil {
			t.Fatal(err)
		}

		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), root).HasTestCases(context.Background(), "test-set-0")
		if !errors.Is(err, syscall.ELOOP) || has {
			t.Fatalf("HasTestCases = (%v, %v), want (false, ELOOP)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})

	t.Run("unreadable test case file after a zero-byte file", func(t *testing.T) {
		root := t.TempDir()
		tests := filepath.Join(root, "test-set-0", "tests")
		if err := os.MkdirAll(tests, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tests, "test-0.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		unreadable := filepath.Join(tests, "test-1.yaml")
		if err := os.WriteFile(unreadable, []byte("kind: Http\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
		if f, err := os.Open(unreadable); err == nil {
			_ = f.Close()
			t.Skipf("euid %d can still read a mode-0000 file", os.Geteuid())
		}

		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), root).HasTestCases(context.Background(), "test-set-0")
		if !errors.Is(err, fs.ErrPermission) || has {
			t.Fatalf("HasTestCases = (%v, %v), want (false, fs.ErrPermission)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})
}
