//go:build windows

package relay

import (
	"context"
	"net"
	"time"
)

// resetClientOnClose resets the client's TCP socket as the destination's end
// was (resetOnceTaken), once the client's stack has acknowledged every byte
// written to it. Windows' socket has no count of the bytes its send buffer
// holds, as Linux's SIOCOUTQ and macOS's SO_NWRITE are, so the bytes written
// are read from the count the proxy put on the socket at its accept
// (CountClientWrites, found under c), and what the client acknowledged from
// the socket's TCP_INFO_v0 (winTCPInfo.tookAll). With no count under c,
// nothing is done and the close stays a FIN, as before: the bytes the socket
// sent do not count those its send buffer still holds, which a reset drops.
// It reports whether the socket was reset.
func resetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	return resetClientAsking(ctx, c, grace, getTCPInfo)
}

// resetClientAsking is resetClientOnClose asking the socket's SIO_TCP_INFO
// through ask (getTCPInfo); a row gives a short, failing or fenced one. An
// answer that cannot say (a failed call, one shorter than keploy reads, or
// counts outside tookAll's fence) does nothing: the close stays a FIN.
func resetClientAsking(ctx context.Context, c net.Conn, grace time.Duration, ask func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error)) bool {
	a := countedClientUnder(c)
	if a == nil {
		return false
	}
	// The count began at the bytes the socket had sent once as it was
	// accepted, as the relay's does (beginFrom).
	out0, _ := a.begin.sentOnce()
	var buf [wtcpInfoSize]byte
	return resetOnceTaken(ctx, c, a.tcp, grace, func(fd uintptr) (all, known bool) {
		// A failed call answers no byte, which reads as no answer.
		n, _ := ask(fd, &buf)
		info, ok := readWinTCPInfo(buf[:n])
		if !ok {
			return false, false
		}
		w := a.count.now()
		return info.tookAll(out0 + w.said + w.maybe)
	})
}
