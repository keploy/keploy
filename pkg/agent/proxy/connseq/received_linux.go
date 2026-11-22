//go:build linux

package connseq

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// socketCount asks a TCP socket for its count of the bytes it has received
// (TCP_INFO tcpi_bytes_received, Linux 4.1 on): every byte the destination
// sent that reached the proxy, read or not. It is made once per connection,
// with its question, so asking allocates nothing; the Upstream's lock
// serialises the asking.
type socketCount struct {
	rc  syscall.RawConn
	ask func(fd uintptr)
	// n is the last count the socket gave.
	n uint64
}

// socketCounterOf is the count of the TCP socket under conn; nil for a conn
// that is not a TCP socket. A TCP conn is always numbered by its socket's
// count: SyscallConn fails only for a TCPConn with no socket under it, which
// no dial returns, and such a conn is refused rather than numbered by what
// was read, as relay.New refuses a destination with no Upstream.
func socketCounterOf(conn net.Conn) socketCounter {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		panic("connseq: a TCP connection with no socket under it: " + err.Error())
	}
	s := &socketCount{rc: rc}
	s.ask = func(fd uintptr) {
		var info unix.TCPInfo
		size := uint32(unix.SizeofTCPInfo)
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, unix.TCP_INFO,
			uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&size)), 0)
		if errno == 0 {
			s.n = info.Bytes_received
		}
	}
	return s
}

// received is the socket's count. Once the socket can no longer be asked (the
// proxy closed it), it is the last count the socket gave, never a count from
// elsewhere: the client chunk being numbered then can no longer reach the
// destination, so nothing the destination sends can answer it.
func (s *socketCount) received() uint64 {
	_ = s.rc.Control(s.ask)
	return s.n
}
