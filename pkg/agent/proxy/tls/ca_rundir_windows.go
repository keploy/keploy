//go:build windows

package tls

import (
	"os"
	"path/filepath"
	"syscall"
)

// holdRunLock creates dir's lock file and keeps it open until it is closed: by
// remove, or by the system when the process exits. Go opens a file without
// FILE_SHARE_DELETE and runEnded asks for no sharing at all, so while it is
// open here no other open of it succeeds. Creating the file is opening it, so
// it is never there unheld while its run lives.
func holdRunLock(dir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, runLockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
}

// runEnded reports whether dir's run has ended: its lock file is there and no
// process has it open, so an open that shares it with no one succeeds. When it
// has, the caller holds that handle until release.
func runEnded(dir string) (release func(), ended bool) {
	p, err := syscall.UTF16PtrFromString(filepath.Join(dir, runLockName))
	if err != nil {
		return nil, false
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, false
	}
	return func() { _ = syscall.CloseHandle(h) }, true
}

// ownedByThisUser is true: the temp directory on Windows is the user's own
// (%LOCALAPPDATA%\Temp), so every directory in it is.
func ownedByThisUser(string) bool { return true }
