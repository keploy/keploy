//go:build !windows

package proxy

import (
	"net"
	"syscall"
)

// peekUpstreamLive returns false if the upstream socket has been closed by
// the peer (FIN/RST already delivered to our kernel) or has unexpected data
// queued. Implemented as one non-blocking recvfrom(MSG_PEEK|MSG_DONTWAIT) —
// same liveness probe nginx uses in ngx_http_upstream_keepalive_close_handler.
//
// Build tag is //go:build !windows because MSG_PEEK + MSG_DONTWAIT and the
// EAGAIN/EWOULDBLOCK/ECONNRESET error codes are POSIX, so this single
// implementation is correct on every Unix-like target keploy ships to
// (linux/amd64, linux/arm64, darwin/arm64, *bsd). A separate Windows stub
// in peek_windows.go returns true unconditionally so non-Unix builds
// compile cleanly.
//
// This catches the "stale pool entry" race where the backend's short
// keep-alive (gunicorn 2s) fires during an idle gap, our kernel receives
// the FIN, but no goroutine has read the upstream socket since the last
// response so we never noticed. Without this probe, the next request's
// bytes get written into a half-dead pipe and vanish — surfacing
// downstream as the customer's 503 + outlier ejection.
func peekUpstreamLive(conn net.Conn) bool {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return true
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return true
	}
	alive := true
	rcErr := sc.Read(func(fd uintptr) bool {
		var buf [1]byte
		n, _, perr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		switch {
		case perr == syscall.EAGAIN || perr == syscall.EWOULDBLOCK:
			alive = true // empty queue, socket healthy
		case perr != nil:
			alive = false // ECONNRESET, EPIPE, etc.
		case n == 0:
			alive = false // FIN already in our buffer
		default:
			alive = false // unexpected data on idle socket — evict
		}
		return true // tell SyscallConn we're done; don't block
	})
	if rcErr != nil {
		return true // can't probe — let the actual write decide
	}
	return alive
}
