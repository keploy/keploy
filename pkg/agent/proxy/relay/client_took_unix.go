//go:build linux || darwin

package relay

import "net"

// canAskClientSockets: on Linux and macOS a client connection that is a TCP
// socket is asked what its client did (clientSocketOf).
const canAskClientSockets = true

// clientSocketOf is the TCP socket under the client conn c, through the conns
// that wrap one and say so (NetConn: util.Conn, *tls.Conn, the proxy's
// trackedConn and recordConn); nil for a conn that is not a TCP socket, which
// is not asked.
func clientSocketOf(c net.Conn) clientSocket {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if tc, ok := c.(*net.TCPConn); ok {
			rc, err := tc.SyscallConn()
			if err != nil {
				return nil
			}
			return newTCPClientSocket(rc)
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}
