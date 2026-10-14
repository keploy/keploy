//go:build linux

package proxy

import (
	"fmt"
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
