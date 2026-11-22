package util

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fionread is winsock's FIONREAD (_IOR('f', 127, u_long)), which
// golang.org/x/sys/windows does not name.
const fionread = 0x4004667f

// UnreadBytes is what the socket under conn has received that is not read
// yet (FIONREAD): bytes only, a close is not counted. It is 0 when there is no
// socket under conn or the ask fails, so it can only fall short of what has
// arrived.
func UnreadBytes(conn net.Conn) int64 {
	_, rc, ok := socketOf(conn)
	if !ok {
		return 0
	}
	var n, got uint32
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = windows.WSAIoctl(windows.Handle(fd), fionread, nil, 0,
			(*byte)(unsafe.Pointer(&n)), uint32(unsafe.Sizeof(n)), &got, nil, 0)
	}); err != nil || ierr != nil {
		return 0
	}
	return int64(n)
}
