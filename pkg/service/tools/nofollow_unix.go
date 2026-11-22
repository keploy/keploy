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

// openat is unix.Openat. A row stands in for it to interrupt an open.
var openat = unix.Openat

// openNoFollowIn is os.OpenFile of name, one path element, in the folder dir,
// with O_NOFOLLOW: a symbolic link at name is refused (ELOOP), never followed,
// in one call. os.Root.OpenFile would follow one whose target is in dir. An
// open a signal interrupts (EINTR, which some file systems return even for a
// restartable call) is made again, as os.OpenFile does.
func openNoFollowIn(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	dirfd := int(d.Fd())
	for {
		fd, err := openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
		if err == nil {
			return os.NewFile(uintptr(fd), path), nil
		}
		if err != unix.EINTR {
			return nil, &fs.PathError{Op: "open", Path: path, Err: err}
		}
	}
}
