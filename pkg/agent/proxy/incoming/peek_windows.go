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
// Note this is a stub for the PEEK only, not a statement that the forwarder is
// unused on Windows. An earlier version of this comment said Windows "is not a
// real deployment target for this codepath", which does not follow: the
// editions that intercept natively on Windows move the application's listener
// and forward through exactly this code. Only the interception backend lives
// elsewhere; the proxy is here.
//
// What is absent on Windows is the PRE-EMPTIVE check, not stale detection as
// such: the read-path classification in handleHttp1ZeroCopy still runs, and
// job 97044495434 is it failing — "Failed to read upstream response and
// request is not safely replayable ... wsarecv" on a GET with content_length 0,
// i.e. canReplay was true and only isStaleConnError returning false sent it to
// writeBadGateway. So with no peek to catch the dead connection first, that
// classification is the whole of Windows' defence, and it is only as good as
// the errno matching behind it (pkg/neterr).
//
// Keep the stub until a probe exists that does not fight Go's IOCP ownership
// of the socket.
func peekUpstreamLive(_ net.Conn) bool {
	return true
}
