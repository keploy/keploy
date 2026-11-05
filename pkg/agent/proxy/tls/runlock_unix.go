//go:build !windows

package tls

import (
	"errors"
	"os"
	"syscall"
)

// defaultCARunsDir is under /run: only root writes there (the native Linux
// install that writes trust-store entries runs as root), no tmp cleaner removes
// a live run's file, and it is emptied at boot, when no run is alive. The macOS
// install writes no entry a sweep removes, and takes no lock.
func defaultCARunsDir() string { return "/run/keploy-ca-runs" }

// tryLockFile takes an exclusive flock on f if no one holds one: true when it
// did, false when someone holds it, and an error when flock failed otherwise.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
