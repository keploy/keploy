//go:build windows

package relay

import (
	"context"
	"net"
	"time"
)

// resetClientOnClose resets the client's TCP socket under c, as Linux's and
// macOS's does (reset_client_unix.go): SO_LINGER 0, then the close of the conn
// under every TLS layer (resetCloser), which Windows sends as a reset. Only
// once the client's stack has acknowledged every byte written to the socket,
// since a reset drops what the socket still holds.
//
// Windows' socket has no count of the bytes its send buffer holds, so what was
// written is read from the count the proxy put on the socket at its accept
// (CountClientWrites), and what the client acknowledged from the socket's
// TCP_INFO_v0 (client_socket_windows.go): the bytes sent once, less those in
// flight. A connection that is closed already (the client reset it, or both
// FINs are in) holds nothing the client could still take, and is closed at
// once, as Linux's, whose send queue a reset empties. It looks again from
// firstLookAgain to lastLookAgain apart, as Linux's does, until grace has
// passed or ctx ends. With no count under c, a write that went around the
// count, or a TCP_INFO_v0 that cannot say (the call fails or answers short, or
// a fast retransmit or a timeout ran), nothing is done: the proxy's close
// stays a FIN, and the client gets every byte. It reports whether the socket
// was reset.
func resetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	tc, a := tcpConnUnder(c), countedClientUnder(c)
	if tc == nil || a == nil {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	s := newTCPClientSocket(rc, nil)
	s.beginFrom(a)
	return resetOnceAcked(ctx, s, grace, func() bool {
		return tc.SetLinger(0) == nil && resetCloser(c).Close() == nil
	})
}

// resetOnceAcked is resetClientOnClose's wait on the client's socket s: it
// resets the socket (reset) once s says the connection is closed or the
// client acknowledged every byte written, and reports reset's answer. It
// reports false, and resets nothing, at once when s cannot say (a write went
// around the count, or a look is unknown), and once grace has passed or ctx
// ends.
func resetOnceAcked(ctx context.Context, s *tcpClientSocket, grace time.Duration, reset func() bool) bool {
	bound := time.NewTimer(max(grace, 0))
	defer bound.Stop()
	wait := firstLookAgain
	look := time.NewTimer(wait)
	defer look.Stop()
	for {
		// written before look: every byte counted written that the look
		// finds acknowledged was acknowledged by then.
		n, ok := s.written()
		l := s.look()
		if !ok || l.said == clientUnknown {
			return false
		}
		if l.said == clientClosed || l.acked >= n {
			return reset()
		}
		look.Reset(wait)
		select {
		case <-look.C:
			wait = min(2*wait, lastLookAgain)
		case <-bound.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}
