package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireRunLock_SecondRunIsRefusedUntilReleased(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Pretend the lock belongs to another live process: this test's parent.
	lockPath := filepath.Join(dir, RunLockFileName)
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getppid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock(dir); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("second lock: got %v, want ErrRunLocked", err)
	}

	release()
	release() // safe to call twice
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file still present after release: %v", err)
	}

	release2, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

func TestAcquireRunLock_TakesOverStaleLock(t *testing.T) {
	testCases := []struct {
		name    string
		content string
	}{
		{"dead pid", "-1"},
		{"garbage", "not-a-pid"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, RunLockFileName), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			release, err := AcquireRunLock(dir)
			if err != nil {
				t.Fatalf("expected to take over the stale lock, got %v", err)
			}
			release()
		})
	}
}

func TestAcquireRunLock_CreatesMissingFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keploy")
	release, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("lock in a missing folder: %v", err)
	}
	release()
}
