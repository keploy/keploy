//go:build darwin

package proxy

import (
	"context"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// WaitPairedBytes returns nil on darwin: the full byte-level settlement
// tracking (Linux's WaitPairedBytes) relies on sock_diag — a netlink-based
// kernel interface that enumerates TCP sockets by address pair and reads their
// byte counters (tcpi_bytes_received, tcpi_bytes_acked). macOS has no
// sock_diag; the closest alternative (net.inet.tcp.pcblist_n sysctl) uses
// private, ABI-unstable structures gated by code-signing on macOS 15+.
//
// The connection-level settlement (ProxyConnsReporter, in enterprise
// proxyconns.go) and the TCP state visibility (inflight_darwin.go, via
// TCP_CONNECTION_INFO) are both active on darwin. Together they ensure every
// connection is paired and its TCP state is visible. What is missing is the
// per-connection byte-count wait: on Linux, the wait ensures that bytes
// written to a pooled (keep-alive) connection by the app have reached the
// proxy's side before the test's end reads. On localhost this gap is
// effectively zero (loopback delivery is synchronous within the kernel), so
// skipping the byte-level wait has no practical impact for local recordings.
func (p *Proxy) WaitPairedBytes(context.Context, uint64) error { return nil }

// PairedBytesErr returns nil on darwin: byte counting is not broken, it is
// skipped because sock_diag is unavailable. The proxy still tracks connection
// state via TCP_CONNECTION_INFO (inflight_darwin.go).
func (p *Proxy) PairedBytesErr() error { return nil }

// checkPairedBytes on Linux proves, on the canary's connection, that
// sock_diag gives the byte counts the wait reads. On darwin sock_diag does
// not exist, so there is nothing to prove; the canary is skipped too
// (accepts.go).
func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

// proveAppVisible on Linux uses sock_diag to confirm the app's sockets are
// visible from the proxy's network namespace. On darwin the app and proxy
// share one namespace (no containers), so visibility is always true; there is
// nothing to prove.
func (p *Proxy) proveAppVisible(*net.TCPConn) {}

// pairClosing notes a paired connection's close. On darwin, readUnread
// returns the unread byte count via SO_NREAD, so the closing path can detect
// bytes the proxy never read. The capture-loss charging (pairLost) is wired
// through the shared paired.closing path.
func (p *Proxy) pairClosing(_ context.Context, conn net.Conn, _ *trackedConn) {
	_, _, _ = p.paired.closing(conn)
}

// readUnread returns the number of bytes in conn's receive buffer that the
// application wrote but the proxy has not yet read. On macOS, SO_NREAD
// (getsockopt SOL_SOCKET) returns this count — the macOS-specific equivalent
// of Linux's SIOCINQ/FIONREAD. For TCP sockets it returns total unread bytes.
func readUnread(conn *net.TCPConn) (int, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int32
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		len := uint32(4)
		gerr = getsockopt(int(fd), unix.SOL_SOCKET, unix.SO_NREAD,
			unsafe.Pointer(&n), &len)
	}); err != nil {
		return 0, err
	}
	return int(n), gerr
}

// getsockopt is a thin wrapper for the getsockopt syscall.
func getsockopt(fd, level, name int, val unsafe.Pointer, vallen *uint32) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_GETSOCKOPT,
		uintptr(fd),
		uintptr(level),
		uintptr(name),
		uintptr(val),
		uintptr(unsafe.Pointer(vallen)),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// readProxySide reads the proxy's side of a paired connection: how many bytes
// it has received and its TCP state. On macOS this uses TCP_CONNECTION_INFO
// (the same API inflight_darwin.go uses for settlement).
func readProxySide(conn *net.TCPConn) proxySide {
	rc, err := conn.SyscallConn()
	if err != nil {
		return proxySide{err: err}
	}
	var ci *unix.TCPConnectionInfo
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		ci, gerr = unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	}); err != nil {
		return proxySide{err: err}
	}
	if gerr != nil {
		return proxySide{err: gerr}
	}
	return proxySide{received: ci.Rxbytes, state: ci.State}
}
