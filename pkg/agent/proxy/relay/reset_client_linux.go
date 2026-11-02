//go:build linux

package relay

import (
	"net"

	"golang.org/x/sys/unix"
)

// resetClientOnClose makes the proxy's close of the client's TCP socket, under
// c through the conns that wrap one and say so (NetConn), a reset (SO_LINGER
// 0), as the destination's end was. Only when the client's stack has
// acknowledged every byte written to the socket (SIOCOUTQ 0): a reset drops
// what the socket still holds, so with bytes outstanding the close stays a FIN
// and the client gets every byte the destination sent. Called once nothing
// writes to the client any more. It reports whether the close will reset.
func resetClientOnClose(c net.Conn) bool {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if tc, ok := c.(*net.TCPConn); ok {
			rc, err := tc.SyscallConn()
			if err != nil {
				return false
			}
			set := false
			_ = rc.Control(func(fd uintptr) {
				var unsent int32
				if !unsentOf(fd, &unsent) || unsent != 0 {
					return
				}
				set = unix.SetsockoptLinger(int(fd), unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}) == nil
			})
			return set
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return false
		}
		c = u.NetConn()
	}
	return false
}
