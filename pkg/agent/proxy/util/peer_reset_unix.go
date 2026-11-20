//go:build linux || darwin

package util

import (
	"context"
	"net"
)

// AwaitPeerReset waits, while nothing else reads conn, on a socket whose peer
// has sent its FIN (a read of it returned io.EOF) until that peer resets it,
// or ctx ends. A peer that half-closed (it sent its request, then ended its
// side) and then closes its socket sends nothing more once it has read all it
// was sent, so a reset, or ctx, is the only end the socket can still show.
// The socket's state is asked again at each event the poller gives it (an
// acknowledgement, the reset), never on a clock. It returns at once where the
// socket shows more than its peer's FIN (a reset, or no state to ask), and
// waits on ctx alone where there is no socket under conn (conn, then each
// NetConn, as WatchPeerEnd looks). The wait ends by the read deadline of the
// conn that gives the socket, which it leaves set once ctx has ended.
func AwaitPeerReset(ctx context.Context, conn net.Conn) {
	sock, rc, ok := socketOf(conn)
	if !ok {
		<-ctx.Done()
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = sock.SetReadDeadline(aLongTimeAgo) })
	defer stop()
	_ = rc.Read(func(fd uintptr) bool { return !finOnly(fd) })
}
