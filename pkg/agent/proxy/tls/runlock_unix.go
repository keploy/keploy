//go:build !windows

package tls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
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

// runsDirWhat names the runs directory in what keploy says of it.
const runsDirWhat = "the directory of keploy runs' locks"

// makeCARunsDir makes dir, this host's runs directory, when it is missing, and
// says whether this run can use it (makeCARunsDirFor).
func makeCARunsDir(dir string) error {
	return makeCARunsDirFor(runtime.GOOS, os.Geteuid(), dir)
}

// makeCARunsDirFor makes dir, the runs directory of a run on goos of the user
// euid, when it is missing, and returns nil only when no other user than
// euid and root can change it: a directory, no link, that euid or root owns
// and that no one but its owner can write (openCARunsDirFor). In it no other
// user can put a link where a lock file goes, or a lock file of their own to
// hold (openLockFile).
//
// A run whose runs directory is in its temp directory (caRunsInTempDir) also
// needs that temp directory, dir's parent, to be the user's alone in the same
// way (and not root's: it is the user's temp directory), or another user
// could put a directory of their own in dir's place. macOS gives each user
// one ($TMPDIR); with TMPDIR unset or empty, as under env -i, sudo -u or a
// launchd job, the temp directory is /tmp, which every user can write, and
// the run takes no lock. That check comes first, so a run makes nothing in a
// temp directory it does not use. The temp directory is checked as opened
// (openOwned), and dir is made and opened from that opened directory, so the
// directory checked is the one dir is in, whoever can change the directories
// above it.
func makeCARunsDirFor(goos string, euid int, dir string) error {
	var d *os.File
	if caRunsInTempDir(goos, euid) {
		tmp, err := openOwned(unix.AT_FDCWD, filepath.Dir(dir), "the temp directory", filepath.Dir(dir), unix.O_RDONLY|unix.O_DIRECTORY, 0, 0o022, euid, false)
		if err != nil {
			return fmt.Errorf("%w; keploy's runs directory goes in this user's temp directory, which must be this user's alone: set TMPDIR to the one macOS gives this user (getconf DARWIN_USER_TEMP_DIR)", err)
		}
		defer func() { _ = tmp.Close() }()
		name := filepath.Base(dir)
		if err := unix.Mkdirat(int(tmp.Fd()), name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: dir, Err: err}
		}
		d, err = openCARunsDirAt(int(tmp.Fd()), name, dir, euid)
		if err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		var err error
		if d, err = openCARunsDirFor(euid, dir); err != nil {
			return err
		}
	}
	return d.Close()
}

// openCARunsDirFor opens dir, a runs directory, for a run of the user euid,
// and returns it only when, as opened (openOwned), it is a directory, no link,
// that euid or root owns and that no one but its owner can write: whatever
// the directories above it are, no other user than euid and root can add,
// remove or rename a file in the directory it returns.
func openCARunsDirFor(euid int, dir string) (*os.File, error) {
	return openCARunsDirAt(unix.AT_FDCWD, dir, dir, euid)
}

// openCARunsDirAt opens name in the directory dirfd, the runs directory dir,
// as openCARunsDirFor opens dir.
func openCARunsDirAt(dirfd int, name, dir string, euid int) (*os.File, error) {
	return openOwned(dirfd, name, runsDirWhat, dir, unix.O_RDONLY|unix.O_DIRECTORY, 0, 0o022, euid, true)
}

// openLockFile opens path, a lock file in the runs directory, with flag,
// creating it (0600) when missing, for this run (openLockFileFor).
func openLockFile(path string, flag int) (*os.File, error) {
	return openLockFileFor(os.Geteuid(), path, flag|os.O_CREATE)
}

// openRunLockToTest opens path, another run's lock file, to test its lock
// (caRunLive): as openLockFile does, but never creating it. A lock file it
// does not trust is one it cannot open, and so counts as live.
func openRunLockToTest(path string) (*os.File, error) {
	return openLockFileFor(os.Geteuid(), path, os.O_RDONLY)
}

// openLockFileFor opens path, a lock file in the runs directory, with flag
// (os.O_CREATE: creating it, 0600, when missing) for a run of the user euid.
// It opens the runs directory first (openCARunsDirFor), and the file from
// that opened directory (openLockFileAt), so no other user can put a link or
// a file of their own where the file is opened.
func openLockFileFor(euid int, path string, flag int) (*os.File, error) {
	d, err := openCARunsDirFor(euid, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return openLockFileAt(d, euid, path, flag)
}

// openLockFileAt opens path, a lock file in the runs directory d (as
// opened), with flag for a run of the user euid. It follows no link at path
// (O_NOFOLLOW), so no one makes the run create or lock a file elsewhere, and
// it returns the file only when it is a regular file that euid owns and that
// no other user can read or write: one another user owns, or can open, is
// one they may hold, and keep the runs' changes waiting. keploy makes every
// lock file 0600, as the user of the run that makes it (root's, for root's
// runs).
func openLockFileAt(d *os.File, euid int, path string, flag int) (*os.File, error) {
	return openOwned(int(d.Fd()), filepath.Base(path), "the lock file", path, flag, 0o600, 0o077, euid, false)
}

// openOwned opens name in the directory dirfd (unix.AT_FDCWD: name is a path),
// at path, with flag (perm when it creates it), following no link at name
// (O_NOFOLLOW), and returns it only when checkOwned trusts it as opened (its
// fstat): a directory when flag has O_DIRECTORY, a regular file otherwise,
// with none of the bits deny, owned by euid, or by root when orRoot is true.
// What it checks is what it returns, whatever is put at path after. what
// names path in its errors.
func openOwned(dirfd int, name, what, path string, flag int, perm uint32, deny os.FileMode, euid int, orRoot bool) (*os.File, error) {
	dir := flag&unix.O_DIRECTORY != 0
	fd, err := unix.Openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		// Why, from what is at path, for the error only: a link there fails
		// the open (ELOOP, or ENOTDIR for a directory), which says less.
		if fi, lerr := os.Lstat(path); lerr == nil {
			if cerr := checkOwned(what, path, fi, dir, deny, euid, orRoot); cerr != nil {
				return nil, cerr
			}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err == nil {
		err = checkOwned(what, path, fi, dir, deny, euid, orRoot)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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
