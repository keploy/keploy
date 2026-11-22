//go:build !windows

package tls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// defaultCARunsDir is this host's runs directory (caRunsDirOn).
func defaultCARunsDir() string { return caRunsDirOn(runtime.GOOS, os.Geteuid(), os.TempDir()) }

// caRunsDirOn is the runs directory on goos for a run of the user euid whose
// temp directory is tempDir.
//
// On Linux it is under /run: only root writes there (the native Linux install
// that writes trust-store entries runs as root), no tmp cleaner removes a live
// run's file, and it is emptied at boot, when no run is alive.
//
// macOS has no /run and its root volume is sealed, so no run could make a
// directory there, and the store lock its legacy sweep's keytool delete takes
// (SweepLegacyJavaAlias, withStoreLock) was never taken. There root's runs use
// /var/run, which macOS keeps as Linux keeps /run (only root writes there, and
// it is emptied at boot); a run of another user cannot write /var/run, so it
// uses a directory in its temp directory (caRunsInTempDir), and only while
// that temp directory is the user's alone (makeCARunsDirFor). Root's runs and
// another user's do not share the lock, and need not: the macOS install
// writes no trust-store entry (setupNativeDarwin), so the one change its runs
// make is that delete, the same in every run, and two runs that make it at
// once do not undo each other's.
func caRunsDirOn(goos string, euid int, tempDir string) string {
	switch {
	case caRunsInTempDir(goos, euid):
		return filepath.Join(tempDir, "keploy-ca-runs")
	case goos == "darwin":
		return "/var/run/keploy-ca-runs"
	default:
		return "/run/keploy-ca-runs"
	}
}

// caRunsInTempDir reports whether a run on goos of the user euid makes its
// runs directory in its temp directory: a run on macOS of a user other than
// root (caRunsDirOn).
func caRunsInTempDir(goos string, euid int) bool { return goos == "darwin" && euid != 0 }

// makeCARunsDir makes dir, this host's runs directory, when it is missing, and
// says whether this run can use it (makeCARunsDirFor).
func makeCARunsDir(dir string) error {
	return makeCARunsDirFor(runtime.GOOS, os.Geteuid(), dir)
}

// makeCARunsDirFor makes dir, the runs directory of a run on goos of the user
// euid, when it is missing, and returns nil only when no other user than
// euid and root can change it: a directory, no link, that euid or root owns
// and that no one but its owner can write. In it no other user can put a
// link where a lock file goes, or a lock file of their own to hold (openLockFile).
//
// A run whose runs directory is in its temp directory (caRunsInTempDir) also
// needs that temp directory, dir's parent, to be the user's alone in the same
// way (and not root's: it is the user's temp directory), or another user
// could put a directory of their own in dir's place. macOS gives each user
// one ($TMPDIR); with TMPDIR unset or empty, as under env -i, sudo -u or a
// launchd job, the temp directory is /tmp, which every user can write, and
// the run takes no lock. That check comes first, so a run makes nothing in a
// temp directory it does not use.
func makeCARunsDirFor(goos string, euid int, dir string) error {
	if caRunsInTempDir(goos, euid) {
		tmp := filepath.Dir(dir)
		fi, err := os.Lstat(tmp)
		if err == nil {
			err = checkOwned("the temp directory", tmp, fi, true, 0o022, euid, false)
		}
		if err != nil {
			return fmt.Errorf("%w; keploy's runs directory goes in this user's temp directory, which must be this user's alone: set TMPDIR to the one macOS gives this user (getconf DARWIN_USER_TEMP_DIR)", err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return checkOwned("the directory of keploy runs' locks", dir, fi, true, 0o022, euid, true)
}

// openLockFile opens path, a lock file in the runs directory, with flag,
// creating it (0600) when missing, for this run (openLockFileFor).
func openLockFile(path string, flag int) (*os.File, error) {
	return openLockFileFor(os.Geteuid(), path, flag)
}

// openLockFileFor opens path, a lock file in the runs directory, with flag
// for a run of the user euid, creating it (0600) when missing. It follows no
// link at path (O_NOFOLLOW), so no one makes the run create or lock a file
// elsewhere, and it returns the file only when it is a regular file that
// euid or root owns: one another user made is one they may hold.
func openLockFileFor(euid int, path string, flag int) (*os.File, error) {
	// #nosec G304 -- path is a lock file in caRunsDir.
	f, err := os.OpenFile(path, flag|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = checkOwned("the lock file", path, fi, false, 0, euid, true)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// openRunLockToTest opens path, another run's lock file, to test its lock
// (caRunLive).
func openRunLockToTest(path string) (*os.File, error) {
	// #nosec G304 -- a lock file in caRunsDir.
	return os.Open(path)
}

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
