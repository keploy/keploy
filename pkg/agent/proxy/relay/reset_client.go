package relay

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// ResetClientOnClose resets the application's TCP socket under c, once it has
// acknowledged every byte written to it, as resetClientOnClose does for the
// relay: replay enacts a recorded reset with it. It reports whether the
// socket was reset; when it was not (the bytes not acknowledged within grace,
// ctx ended, or a platform where the socket cannot say what it holds), the
// caller's close stays a FIN.
func ResetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	return resetClientOnClose(ctx, c, grace)
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
