//go:build windows

package tls

import (
	"os"

	"golang.org/x/sys/windows"
)

// openNoFollowNonBlocking opens path read-only for readOpenedRegular, opening
// a symlink or other reparse point itself, never its target, so its Stat is
// not regular and it is skipped. A Windows file system holds no FIFO (named
// pipes live under \\.\pipe\). An open there can still wait for an oplock
// break, but only a process that can open the file can hold an oplock on it,
// and %TEMP% is the user's own. Sharing read, write and delete takes nothing
// from another process using the file.
func openNoFollowNonBlocking(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
