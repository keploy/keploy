//go:build linux

package utils

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// WriteShmFile writes b to path in a directory every container of a pod can
// write (the pod's /dev/shm): to path.tmp first, made afresh as a file of
// this process's own with mode 0600 (whatever another container left at that
// name, a link or a FIFO among them, is removed first, never written through
// or into), then renamed into place, which replaces whatever was at path (a
// link included) without writing through it. One temporary name, so a
// process killed between the two leaves one file behind, which the next
// write takes over.
func WriteShmFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := unix.Unlink(tmp); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s: %w", tmp, err)
	}
	fd, err := unix.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}
