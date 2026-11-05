//go:build !windows

package tls

import (
	"os"
	"path/filepath"
	"syscall"
)

// holdRunLock creates dir's lock file and holds an exclusive flock on it until
// the file is closed: by remove, or by the kernel when the process exits. The
// lock is taken on a file under another name and the file then renamed into
// place, so the lock file is never seen unlocked while its run lives.
func holdRunLock(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, "."+runLockName+"-*")
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, runLockName))
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// runEnded reports whether dir's run has ended: its lock file is there and the
// flock on it is free. When it has, the caller holds the lock until release.
func runEnded(dir string) (release func(), ended bool) {
	f, err := os.Open(filepath.Join(dir, runLockName))
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() { _ = f.Close() }, true
}

// ownedByThisUser reports whether this process's user owns path. The temp
// directory is shared: a directory another user made under this user's name
// is not this user's to remove.
func ownedByThisUser(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
