//go:build windows

package proxy

import (
	"net"

	"golang.org/x/sys/windows"
)

// peekUpstreamLive returns false if the upstream socket has been closed by
// the peer (FIN/RST already delivered to our kernel) or has unexpected data
// queued. The unix sibling uses recvfrom(MSG_PEEK|MSG_DONTWAIT); on Windows
// the equivalent is WSARecv with MSG_PEEK on a socket Go has already put in
// non-blocking (IOCP) mode, so MSG_DONTWAIT has no analogue and isn't needed
// — recv on an empty queue returns WSAEWOULDBLOCK immediately.
//
// See peek_unix.go for why this probe exists (stale upstream pool entries
// after the backend's short keep-alive fires during an idle gap).
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
		wsabuf := windows.WSABuf{Len: 1, Buf: &buf[0]}
		var n uint32
		flags := uint32(windows.MSG_PEEK)
		// Synchronous WSARecv (overlapped=nil) on the IOCP-managed socket.
		// On a non-blocking socket with no queued data this returns
		// WSAEWOULDBLOCK without blocking the runtime.
		perr := windows.WSARecv(windows.Handle(fd), &wsabuf, 1, &n, &flags, nil, nil)
		switch {
		case perr == windows.WSAEWOULDBLOCK:
			alive = true // empty queue, socket healthy
		case perr != nil:
			alive = false // WSAECONNRESET, WSAECONNABORTED, etc.
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
