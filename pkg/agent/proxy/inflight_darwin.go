//go:build darwin

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketReceived reads the TCP state and bytes received from a darwin socket
// via getsockopt(TCP_CONNECTION_INFO). This is the macOS equivalent of Linux's
// getsockopt(TCP_INFO): it gives the settlement watchdog visibility into
// whether a connection is idle, closed, or still receiving data.
//
// Without this, the macOS proxy is blind to TCP state (inflight_other.go
// returns (0, 0, false)), and settlement stalls for the full 600-second
// watchdog timeout on any connection whose Go handler has not returned —
// keep-alive pools, SSE streams, deliberately-hanging test endpoints.
func socketReceived(raw syscall.RawConn) (received uint64, state uint8, ok bool) {
	read := func() (uint64, uint8, bool) {
		var ci *unix.TCPConnectionInfo
		var err error
		if cerr := raw.Control(func(fd uintptr) {
			ci, err = unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
		}); cerr != nil || err != nil {
			return 0, 0, false
		}
		return ci.Rxbytes, ci.State, true
	}
	// Same convergence loop as the Linux implementation: two reads that agree
	// bracket the second read, so the state and count are from one instant.
	received, state, ok = read()
	for i := 0; ok && i < 3; i++ {
		r, s, ok2 := read()
		if !ok2 {
			return received, state, ok
		}
		if r == received && s == state {
			return r, s, true
		}
		received, state = r, s
	}
	return received, state, ok
}

// finCounted is false on darwin: XNU's tcpi_rxbytes counts the payload a
// socket received, never the FIN (100 bytes and a FIN read 100, in
// CLOSE_WAIT), so untaken takes nothing off for the application's FIN, where
// Linux's tcpi_bytes_received counts it as one.
func finCounted(uint8, bool) bool { return false }

// clientGone reports whether the client of a socket its handler still holds
// has gone (xnuClientGone).
func clientGone(state uint8) bool { return xnuClientGone(state) }
