//go:build linux

package proxy

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// listenerDrops reads the listening socket's sk_drops through
// getsockopt(SO_MEMINFO): the kernel adds one at the listener for every SYN or
// final ACK it drops there (tcp_listendrop), an accept queue full among the
// causes (Linux 4.12 on).
func listenerDrops(rc syscall.RawConn) (uint64, error) {
	var mem [unix.SK_MEMINFO_VARS]uint32
	size := uint32(unsafe.Sizeof(mem))
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_SOCKET, unix.SO_MEMINFO,
			uintptr(unsafe.Pointer(&mem[0])), uintptr(unsafe.Pointer(&size)), 0)
	}); err != nil {
		return 0, fmt.Errorf("%w: reach the proxy's listener: %v", models.ErrProxyConnsUnsupported, err)
	}
	if errno != 0 {
		return 0, fmt.Errorf("%w: read the proxy's listener's drops (SO_MEMINFO): %v", models.ErrProxyConnsUnsupported, errno)
	}
	if size <= unix.SK_MEMINFO_DROPS*4 {
		return 0, fmt.Errorf("%w: the kernel's SO_MEMINFO has no drops (%d bytes)", models.ErrProxyConnsUnsupported, size)
	}
	return uint64(mem[unix.SK_MEMINFO_DROPS]), nil
}

// noFastOpen turns TCP Fast Open off on a listening socket: a Fast Open queue
// of 0 refuses every Fast Open SYN, so each connection completes its
// handshake first (set after listen(), which, under the sysctl's
// "without the socket option" bit, would give it one).
func noFastOpen(ln net.Listener) error {
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return fmt.Errorf("reach the listener to turn TCP Fast Open off: %w", err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_FASTOPEN, 0) }); err != nil {
		return fmt.Errorf("reach the listener to turn TCP Fast Open off: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("turn TCP Fast Open off on the listener: %w", serr)
	}
	return nil
}
