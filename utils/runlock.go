package utils

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RunLockFileName is the lock file a record or test run holds in the keploy folder.
const RunLockFileName = ".keploy.lock"

// ErrRunLocked is returned by AcquireRunLock when another live process holds the lock.
var ErrRunLocked = errors.New("another keploy record or test is already running on this keploy folder")

// AcquireRunLock stops two record/test runs from writing the same keploy folder at
// once. It creates dir/.keploy.lock holding this process's PID; a lock left behind by
// a process that is no longer running is taken over. The returned func removes the
// lock and is safe to call more than once.
func AcquireRunLock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create keploy folder for the run lock: %w", err)
	}
	lockPath := filepath.Join(dir, RunLockFileName)

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, writeErr := f.WriteString(strconv.Itoa(os.Getpid()))
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("failed to write the run lock %s: %w", lockPath, errors.Join(writeErr, closeErr))
			}
			released := false
			return func() {
				if !released {
					released = true
					_ = os.Remove(lockPath)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("failed to create the run lock %s: %w", lockPath, err)
		}

		pid, readErr := readLockPID(lockPath)
		if readErr == nil && pid != os.Getpid() && runLockHolderAlive(pid) {
			return nil, fmt.Errorf("%w (pid %d); if no keploy process is running, delete %s", ErrRunLocked, pid, lockPath)
		}
		// Stale or unreadable lock: remove it and try once more.
		if rmErr := os.Remove(lockPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to remove the stale run lock %s: %w", lockPath, rmErr)
		}
	}
	return nil, fmt.Errorf("%w; if no keploy process is running, delete %s", ErrRunLocked, lockPath)
}

func readLockPID(lockPath string) (int, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
