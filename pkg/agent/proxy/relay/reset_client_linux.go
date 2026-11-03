//go:build linux

package relay

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// resetClientOnClose resets the client's TCP socket, under c through the conns
// that wrap one and say so (NetConn), as the destination's end was: it sets
// SO_LINGER 0 on the socket and closes it through resetCloser, the conn under
// every TLS layer, since a TLS layer's close writes a close_notify alert
// first, which an HTTPS client reads as the clean end of the stream, not a
// reset. The proxy's later close of c then finds the socket closed.
//
// Only once the client's stack has acknowledged every byte written to the
// socket (SIOCOUTQ 0): a reset drops what the socket still holds, so the
// client must have every byte the destination sent first. A client in
// delayed-ACK mode (a request/response connection, as an HTTP client's is)
// acknowledges the last bytes some tens of milliseconds after they came, so it
// looks at the socket again, from firstLookAgain to lastLookAgain apart, until
// they are acknowledged. It stops looking when grace has passed (the relay's
// HalfCloseGrace, its bound on waiting for the client's answer at its end;
// none when it is not positive) or ctx ends (the recording's stop), and when
// the socket can no longer say: then nothing is done, the proxy's close stays
// a FIN, and the client gets every byte. Called once nothing writes to the
// client any more. It reports whether the socket was reset.
func resetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	tc := tcpConnUnder(c)
	if tc == nil {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	bound := time.NewTimer(max(grace, 0))
	defer bound.Stop()
	wait := firstLookAgain
	look := time.NewTimer(wait)
	defer look.Stop()
	for {
		drained, known := false, false
		if rc.Control(func(fd uintptr) {
			var unsent int32
			known = unsentOf(fd, &unsent)
			drained = known && unsent == 0
		}) != nil || !known {
			return false
		}
		if drained {
			set := false
			_ = rc.Control(func(fd uintptr) {
				set = unix.SetsockoptLinger(int(fd), unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}) == nil
			})
			return set && resetCloser(c).Close() == nil
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

// resetCloser is the conn a reset closes the socket under c through: the one
// right under the innermost TLS layer (*tls.Conn) in c's chain of conns that
// wrap one and say so (NetConn), or c itself when it has none. A TLS layer's
// close writes its close_notify alert before the socket's close, which a
// reset must not let the client read first. The conns under the layers are
// keploy's own, and their close runs what must come before the socket's
// close: a connection the proxy pairs (its trackedConn) reads what the socket
// holds unread as it closes (pairClosing), which a close of the socket
// itself, under that conn, would leave reading a closed socket, failing the
// pairing for the rest of the run. That close sends the client nothing before
// the reset, but for the FIN of that read when the client has bytes on their
// way or unread then: the read ends at the first of them (a capture loss) or
// at the client's own end of its side. The layers above it close with the
// proxy's later close of c, and find the socket closed.
func resetCloser(c net.Conn) net.Conn {
	closer := c
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		if _, tlsLayer := c.(*tls.Conn); tlsLayer {
			closer = u.NetConn()
		}
		c = u.NetConn()
	}
	return closer
}

// tcpConnUnder is the TCP socket under c, through the conns that wrap one and
// say so (NetConn), or nil.
func tcpConnUnder(c net.Conn) *net.TCPConn {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if tc, ok := c.(*net.TCPConn); ok {
			return tc
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}
