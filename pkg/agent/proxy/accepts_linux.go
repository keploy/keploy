//go:build linux

package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// listenerDrops reads the listening socket's sk_drops through
// getsockopt(SO_MEMINFO): the kernel adds one at the listener for every SYN or
// final ACK it drops there (tcp_listendrop), an accept queue full among the
// causes (Linux 4.12 on), and one for every packet a half-open connection
// discards (tcp_check_req answering none: a client's reset of a connect it
// gave up, a retransmitted SYN, a packet out of its window), which loses no
// connection (models.ListenDrops).
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

// netstatPath is where a process reads its network namespace's TCP counters.
var netstatPath = "/proc/self/net/netstat"

// netListenDrops reads TcpExtListenDrops of the network namespace the
// listener is in: the kernel adds one at every tcp_listendrop, with the
// listener's sk_drops (Linux 4.12 on), and at nothing else. The count is the
// process's namespace's (netstatPath), so the listener is checked to be in it
// (SIOCGSKNS, Linux 4.9 on): a listener opened in another namespace, or one
// that cannot be told, has no count here.
func netListenDrops(rc syscall.RawConn) (uint64, error) {
	nsfd, errno := -1, error(nil)
	if err := rc.Control(func(fd uintptr) { nsfd, errno = unix.IoctlRetInt(int(fd), unix.SIOCGSKNS) }); err != nil {
		return 0, fmt.Errorf("reach the listener to tell its network namespace: %v", err)
	}
	if errno != nil {
		return 0, fmt.Errorf("tell the listener's network namespace (SIOCGSKNS): %v", errno)
	}
	defer func() { _ = unix.Close(nsfd) }()
	var its, own unix.Stat_t
	if err := unix.Fstat(nsfd, &its); err != nil {
		return 0, fmt.Errorf("tell the listener's network namespace: %v", err)
	}
	if err := unix.Stat("/proc/self/ns/net", &own); err != nil {
		return 0, fmt.Errorf("tell keploy's own network namespace: %v", err)
	}
	if its.Dev != own.Dev || its.Ino != own.Ino {
		return 0, errors.New("the listener is in another network namespace than keploy's process")
	}
	b, err := os.ReadFile(netstatPath)
	if err != nil {
		return 0, fmt.Errorf("read the network namespace's TCP counters: %v", err)
	}
	return parseListenDrops(b)
}

// parseListenDrops finds TcpExtListenDrops in /proc/net/netstat's text: a
// "TcpExt:" line of names, then one of their values.
func parseListenDrops(b []byte) (uint64, error) {
	var names [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) == 0 || string(f[0]) != "TcpExt:" {
			continue
		}
		if names == nil {
			names = f
			continue
		}
		if len(f) != len(names) {
			return 0, fmt.Errorf("the network namespace's TcpExt counters have %d values for %d names", len(f)-1, len(names)-1)
		}
		for i, n := range names {
			if string(n) == "ListenDrops" {
				v, err := strconv.ParseUint(string(f[i]), 10, 64)
				if err != nil {
					return 0, fmt.Errorf("the network namespace's ListenDrops %q: %v", f[i], err)
				}
				return v, nil
			}
		}
		return 0, errors.New("the network namespace's TCP counters have no ListenDrops")
	}
	return 0, errors.New("the network namespace's TCP counters have no TcpExt values")
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
