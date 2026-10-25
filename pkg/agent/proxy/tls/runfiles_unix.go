//go:build unix

package tls

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// runFileLocking: flock(2) locks a whole file per open file description and
// the kernel drops them when the process ends, however it ends.
const runFileLocking = true

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
