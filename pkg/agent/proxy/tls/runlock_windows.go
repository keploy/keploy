//go:build windows

package tls

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// defaultCARunsDir is under %ProgramData%, which every user of the host can
// read and add files to: a run's per-user CA alias sits in a JDK keystore other
// users' runs sweep too, so they must see its lock. A lock file another user
// cannot open counts as live (runLive).
func defaultCARunsDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Keploy", "ca-runs")
}

// tryLockFile takes an exclusive lock on f's first byte if no one holds one:
// true when it did, false when someone holds it, and an error when
// LockFileEx failed otherwise.
func tryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
