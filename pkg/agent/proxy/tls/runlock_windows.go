//go:build windows

package tls

import (
	"errors"
	"fmt"
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

// makeCARunsDir makes dir, this host's runs directory, when it is missing.
// Every user of the host adds files to it (defaultCARunsDir), so who owns it
// decides nothing here.
func makeCARunsDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// openLockFile opens path, a lock file in the runs directory, with flag,
// creating it when missing, and returns it only when it is a regular file:
// what holds the name of a lock file in its place, as a directory, is no lock
// file of a run. Go's os has no O_NOFOLLOW on Windows (castore_windows.go).
func openLockFile(path string, flag int) (*os.File, error) {
	// #nosec G304 -- path is a lock file in caRunsDir.
	f, err := os.OpenFile(path, flag|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("the lock file %s is not a regular file", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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
