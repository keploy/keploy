//go:build linux || darwin

package relay

import (
	"context"
	"net"
	"time"
)

// resetClientOnClose resets the client's TCP socket, under c through the conns
// that wrap one and say so (NetConn), as the destination's end was
// (resetOnceTaken), once the socket's send buffer holds nothing (unsentOf 0):
// what it holds was written and not yet acknowledged by the client's stack.
// It reports whether the socket was reset.
func resetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	tc := tcpConnUnder(c)
	if tc == nil {
		return false
	}
	return resetOnceTaken(ctx, c, tc, grace, func(fd uintptr) (all, known bool) {
		var unsent int32
		known = unsentOf(fd, &unsent)
		return known && unsent == 0, known
	})
}
