//go:build linux

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketUnread is how many bytes the socket holds unread (SIOCINQ); 0 when
// it cannot be asked (a socket already closed).
func socketUnread(raw syscall.RawConn) int {
	n := 0
	_ = raw.Control(func(fd uintptr) {
		if v, err := unix.IoctlGetInt(int(fd), unix.SIOCINQ); err == nil {
			n = v
		}
	})
	return n
}
