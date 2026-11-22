//go:build linux

package learn

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"

	"golang.org/x/sys/unix"
)

// heldZoneFile, where a test sets it, is called with each descriptor
// openRegular opens: the O_PATH one before it reads the file's type, and
// the one it then reads the file through.
var heldZoneFile func(fd int)

// openRegular opens the regular file at p, a path in the process's own
// view with every link on it resolved (resolveUnder), under root, for
// reading, and no other. The agent runs as root and p is a path a
// process's files give (an application's TZ, its /etc/localtime; keploy's
// own, under "/", the same way), so keploy first holds the file by an
// O_PATH descriptor, which opens nothing (no device driver's open runs, a
// FIFO gains no reader), reads its type through it, and opens it for
// reading only where it is a regular file, through that descriptor
// (/proc/self/fd), so the file read is the one checked. The path's last
// name is not followed (O_NOFOLLOW), as resolveUnder followed every link
// on p, so a link there is one swapped in since, an error read again at
// the next call; root itself (p "/") is followed, as it is a link
// (/proc/<pid>/root).
func openRegular(root, p string) (*os.File, error) {
	name := under(root, p)
	flags := unix.O_PATH | unix.O_CLOEXEC
	if path.Clean("/"+p) != "/" {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := unix.Open(name, flags, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	held := os.NewFile(uintptr(fd), name)
	defer func() { _ = held.Close() }()
	if heldZoneFile != nil {
		heldZoneFile(fd)
	}
	fi, err := held.Stat()
	if err != nil {
		return nil, err
	}
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("%s became a link while keploy read it", name)
	case !m.IsRegular():
		return nil, lasting(errNotRegular{m})
	}
	rfd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	case err != nil:
		// The descriptor holds the file, so only keploy's own /proc fails
		// here (none mounted, another PID namespace's): never the file's
		// absence, so the error does not wrap it.
		return nil, fmt.Errorf("keploy cannot open %s again through its own /proc/self/fd: %v", name, err)
	}
	if heldZoneFile != nil {
		heldZoneFile(rfd)
	}
	return os.NewFile(uintptr(rfd), name), nil
}
