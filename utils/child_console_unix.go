//go:build !windows

package utils

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// drainNow writes to dst what src holds when it is called, and no more: a
// process the child left running may write on without end. src is in the
// poller (Drain set a deadline on it), so its descriptor does not block: a
// read that finds nothing says so.
func drainNow(src *os.File, buf []byte, dst io.Writer) {
	rc, err := src.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		left, err := unix.IoctlGetInt(int(fd), fionread)
		if err != nil {
			return
		}
		for left > 0 {
			n, err := syscall.Read(int(fd), buf[:min(left, len(buf))])
			if n > 0 {
				_, _ = dst.Write(buf[:n])
				left -= n
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil || n <= 0 {
				return
			}
		}
	})
}
