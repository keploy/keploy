package util

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// UnreadBytes is what the socket under conn has received that is not read
// yet (SIOCINQ): bytes only, a close is not counted. It is 0 when there is no
// socket under conn or the ask fails, so it can only fall short of what has
// arrived.
func UnreadBytes(conn net.Conn) int64 {
	_, rc, ok := socketOf(conn)
	if !ok {
		return 0
	}
	var n int
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		n, ierr = unix.IoctlGetInt(int(fd), unix.SIOCINQ)
	}); err != nil || ierr != nil || n < 0 {
		return 0
	}
	return int64(n)
}

// OwnSocket is the socket conn is itself (a *net.TCPConn, not a wrapper whose
// reads may do more than its socket's), for ReadNow and Received.
func OwnSocket(conn net.Conn) (syscall.RawConn, bool) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rc, err := sc.SyscallConn()
	return rc, err == nil
}

// Received is what the TCP socket rc has received from its peer since it
// opened, in sequence (TCP_INFO's bytes_received): its bytes, read or not, and
// the peer's close (its FIN) as one more once it has come, as the sequence
// space counts it. One count, taken by the kernel at once: it holds every
// byte it counts, and is false where TCP_INFO cannot be had (not a TCP socket).
func Received(rc syscall.RawConn) (int64, bool) {
	var info *unix.TCPInfo
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		info, ierr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	}); err != nil || ierr != nil || info == nil {
		return 0, false
	}
	return int64(info.Bytes_received), true
}

// ReadNow reads the socket fd (non-blocking, as Go's are) without waiting:
// wait reports that it holds nothing to read yet. n is 0 and err nil at the
// end of the stream.
func ReadNow(fd uintptr, p []byte) (n int, err error, wait bool) {
	for {
		n, err = unix.Read(int(fd), p)
		switch err {
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			return 0, nil, true
		}
		if n < 0 {
			n = 0
		}
		return n, err, false
	}
}
