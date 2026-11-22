package util

import (
	"net"

	"golang.org/x/sys/unix"
)

// UnreadBytes is what the socket under conn has received that is not read
// yet (SO_NREAD, macOS's SIOCINQ): bytes only, a close is not counted. It is
// 0 when there is no socket under conn or the ask fails, so it can only fall
// short of what has arrived.
func UnreadBytes(conn net.Conn) int64 {
	_, rc, ok := socketOf(conn)
	if !ok {
		return 0
	}
	var n int
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		n, ierr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NREAD)
	}); err != nil || ierr != nil || n < 0 {
		return 0
	}
	return int64(n)
}
