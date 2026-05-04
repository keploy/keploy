//go:build windows

package proxy

import "net"

// peekUpstreamLive on Windows is a stub that always reports the socket as
// alive; the caller falls back to write-based stale detection (the
// idempotent-replay branch in handleHttp1ZeroCopy still catches the race on
// the next req.Write).
//
// Why a stub and not a real probe:
//
// The unix sibling uses recvfrom(MSG_PEEK|MSG_DONTWAIT). On Windows the
// equivalent flags exist in golang.org/x/sys/windows (WSARecv + MSG_PEEK),
// but Go puts Windows sockets in BLOCKING mode and gets its async behavior
// from overlapped I/O bound to the runtime's IOCP. A synchronous WSARecv
// (lpOverlapped == NULL) on such a socket blocks until data arrives, which
// defeats the purpose of the probe and can stall the goroutine.
//
// The alternatives all conflict with Go's IOCP ownership of the socket:
//
//   - Overlapped WSARecv + CancelIoEx: the completion still posts to Go's
//     IOCP, racing with Go's own reads on the same fd.
//   - ioctlsocket(FIONBIO, 1) + peek + restore: MSDN explicitly warns
//     against FIONBIO on sockets opened with WSA_FLAG_OVERLAPPED.
//   - WSAEventSelect: overrides the socket's notification mode that Go
//     relies on.
//
// Since the keploy ingress forwarder is driven by the eBPF bind redirector
// (Linux-only), Windows is not a real deployment target for this codepath.
// Keep the stub until/unless that changes.
func peekUpstreamLive(_ net.Conn) bool {
	return true
}
