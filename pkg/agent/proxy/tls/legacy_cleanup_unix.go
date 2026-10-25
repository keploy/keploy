//go:build !windows

package tls

import (
	"os"
	"syscall"
)

// openNoFollowNonBlocking opens path read-only for readOpenedRegular: never
// through a final symlink (O_NOFOLLOW fails with ELOOP), and never blocking
// in open(2) on a FIFO or device with no partner (O_NONBLOCK; a read of a
// regular file ignores it).
func openNoFollowNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
