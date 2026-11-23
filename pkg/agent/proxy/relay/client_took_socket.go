//go:build linux || darwin || windows

package relay

import (
	"net"
	"syscall"

	"go.uber.org/zap"
)

// canAskClientSockets: on Linux, macOS and Windows a client connection that is
// a TCP socket is asked what its client did (clientSocketOf).
const canAskClientSockets = true

// clientSocketOf is the TCP socket under the client conn c, through the conns
// that wrap one and say so (NetConn: util.Conn, *tls.Conn, the proxy's
// trackedConn and recordConn); nil for a conn that is not a TCP socket, which
// is not asked. log is where macOS and Windows say once that their TCP_INFO
// cannot answer (client_took_darwin.go, client_socket_windows.go).
func clientSocketOf(c net.Conn, log *zap.Logger) clientSocket {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if tc, ok := c.(*net.TCPConn); ok {
			rc, err := tc.SyscallConn()
			if err != nil {
				return nil
			}
			return newTCPClientSocket(rc, log)
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}

// tcpStateSocketOf returns a dedicated clientSocket for querying TCP state on
// c (used only by that side's forwarder in peerWriteState.resetOnEOF, never
// shared with Relay.client so look() has a single caller). It walks NetConn()
// wrappers and also accepts syscall.Conn wrappers such as connseq.Upstream.
func tcpStateSocketOf(c net.Conn) clientSocket {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if sc, ok := c.(syscall.Conn); ok {
			rc, err := sc.SyscallConn()
			if err != nil {
				return nil
			}
			return newTCPClientSocket(rc, nil)
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}
