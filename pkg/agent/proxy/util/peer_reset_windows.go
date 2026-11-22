//go:build windows

package util

import (
	"context"
	"net"
	"time"
)

// peerResetLookEvery is how often AwaitPeerReset asks a Windows socket's
// state: a reset there marks no event a wait can take.
const peerResetLookEvery = 50 * time.Millisecond

// AwaitPeerReset waits, while nothing else reads conn, on a socket whose peer
// has sent its FIN (a read of it returned io.EOF) until that peer resets it,
// or ctx ends. A peer that half-closed (it sent its request, then ended its
// side) and then closes its socket sends nothing more once it has read all it
// was sent, so a reset, or ctx, is the only end the socket can still show.
// Windows gives a socket whose peer's FIN is in no event at its reset (a read
// of it completes at once, with the FIN, each time), so its state is asked
// every peerResetLookEvery instead (SIO_TCP_INFO). It returns at once where
// the socket shows more than its peer's FIN (a reset, or no state to ask),
// and waits on ctx alone where there is no socket under conn (conn, then each
// NetConn).
func AwaitPeerReset(ctx context.Context, conn net.Conn) {
	_, rc, ok := socketOf(conn)
	if !ok {
		<-ctx.Done()
		return
	}
	look := time.NewTicker(peerResetLookEvery)
	defer look.Stop()
	for {
		fin := false
		if err := rc.Control(func(fd uintptr) { fin = finOnly(fd) }); err != nil || !fin {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-look.C:
		}
	}
}
