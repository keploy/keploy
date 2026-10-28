//go:build darwin

package proxy

import (
	"context"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// errPairedBytesDarwin is why no bytes wait can be answered on darwin yet.
// Linux reads, for each paired connection, how much the application had
// written to its socket (sock_diag) and waits until the proxy's side has
// received it. macOS has no sock_diag, and nothing here reads the
// application's socket yet: a pooled connection's call written just before
// a test's end can still sit in the application's send buffer, or on
// loopback's input queue (XNU's lo_output queues to the DLIL input thread,
// so delivery is not synchronous), when the end reads. The wait cannot
// vouch for those bytes, so it says it cannot tell
// (models.ErrProxyConnsUnsupported): under the proven switch no test is
// judged verified on darwin until it can.
var errPairedBytesDarwin = fmt.Errorf("%w: keploy cannot yet read, on macOS, how much the application wrote to the connections its proxy holds", models.ErrProxyConnsUnsupported)

// WaitPairedBytes cannot tell on darwin (errPairedBytesDarwin).
func (p *Proxy) WaitPairedBytes(context.Context, uint64) error { return errPairedBytesDarwin }

// PairedBytesErr says the bytes cannot be counted on darwin
// (errPairedBytesDarwin).
func (p *Proxy) PairedBytesErr() error { return errPairedBytesDarwin }

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
