//go:build unix

package tls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// runFileLocking: flock(2) locks a whole file per open file description and
// the kernel drops them when the process ends, however it ends.
const runFileLocking = true

// privateDirName is the name of this user's private dir for run files in the
// temp dir: keploy-<euid>, so root's (keploy-0) and each user's are apart.
func privateDirName() (string, error) { return "keploy-" + strconv.Itoa(os.Geteuid()), nil }

// runFilesOwner is the uid the private dir at path must belong to: this
// user's. A seam for the test of a dir another user made.
var runFilesOwner = func(string) int { return os.Geteuid() }

// checkPrivateDirOwner returns why the dir fi (Lstat of path) is not this
// user's alone: another user owns it, its group or everyone may write in it,
// or it is a mount point (a FUSE mount another user made at the name reports
// whatever owner and mode its daemon likes, and serves what it likes; the
// kernel, not the daemon, sets the device a mount's files are on).
func checkPrivateDirOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("its owner cannot be read")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if pst, ok := parent.Sys().(*syscall.Stat_t); !ok || pst.Dev != st.Dev {
		return errors.New("it is a mount point, on another filesystem than the temp dir")
	}
	if want := runFilesOwner(path); int64(st.Uid) != int64(want) {
		return fmt.Errorf("it is owned by uid %d, not by this user (uid %d)", st.Uid, want)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("other users can write in it (mode %#o)", perm)
	}
	return nil
}

// setPrivateDirMode gives the private dir at path the mode 0755: only this
// user writes in it, and an app of any user reads the CA file in it. Through a
// handle opened without following a link, so it changes only that dir.
func setPrivateDirMode(path string) error {
	d, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Chmod(0o755)
}

// openRunFileLock opens the run file at path for its lock: never through a
// symlink, and never blocking on something that is not a regular file. Read
// and write, as NFS grants an exclusive lock only to a file open for writing;
// it writes nothing, and on Unix an open takes nothing from an app that reads
// the file. Close-on-exec, as Go opens every file: the app is not handed the
// lock.
func openRunFileLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("%s: %w", path, errNotRegular)
		}
		return nil, err
	}
	return f, nil
}

// isSymlinkRefused reports whether err is openRunFileLock refusing a symlink.
func isSymlinkRefused(err error) bool { return errors.Is(err, syscall.ELOOP) }

// isInUse reports whether a remove failed because the file is open: never on
// Unix.
func isInUse(error) bool { return false }

// tryLockRunFile takes the exclusive lock on f without waiting: false when
// another open of the file, in this process or another, holds it.
func tryLockRunFile(f *os.File) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var lockErr error
	if err := rc.Control(func(fd uintptr) {
		for {
			lockErr = unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(lockErr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return false, err
	}
	if errors.Is(lockErr, unix.EWOULDBLOCK) {
		return false, nil
	}
	return lockErr == nil, lockErr
}
