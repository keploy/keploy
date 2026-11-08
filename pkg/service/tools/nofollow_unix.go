//go:build !windows

package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// oNoFollow makes a create-or-truncate refuse a symlink at the final
// component. The config path is fully resolved before it is opened, so it can
// never legitimately be a link; without this, one planted between the check
// and the write is followed, and the generated config lands wherever it
// points.
const oNoFollow = syscall.O_NOFOLLOW

// openNoFollowIn is os.OpenFile of name, one path element, in the folder dir,
// with O_NOFOLLOW: a symbolic link at name is refused (ELOOP), never followed,
// in one call. os.Root.OpenFile would follow one whose target is in dir.
func openNoFollowIn(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	rc, err := d.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var openErr error
	if err := rc.Control(func(dirfd uintptr) {
		for {
			fd, openErr = unix.Openat(int(dirfd), name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
			if openErr != unix.EINTR {
				return
			}
		}
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: openErr}
	}
	return os.NewFile(uintptr(fd), path), nil
}
