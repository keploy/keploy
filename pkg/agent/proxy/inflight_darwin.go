//go:build darwin

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// macOS TCP states (netinet/tcp_fsm.h). These differ from Linux's numbering
// (include/net/tcp_states.h) — macOS starts ESTABLISHED at 4, Linux at 1.
const (
	darwinTCPSClosed    = 0
	darwinTCPSEstab     = 4
	darwinTCPSCloseWait = 5
	darwinTCPSFinWait1  = 6
	darwinTCPSClosing   = 7
	darwinTCPSLastAck   = 8
	darwinTCPSFinWait2  = 9
	darwinTCPSTimeWait  = 10
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

// finReceived reports whether the remote end sent a FIN. The macOS TCP states
// that imply a received FIN are CLOSE_WAIT (the common case: the remote
// closed, the local handler still holds the socket), LAST_ACK and CLOSING.
// TCPS_CLOSED with an EOF means the full close completed.
func finReceived(state uint8, eof bool) bool {
	switch state {
	case darwinTCPSCloseWait, darwinTCPSLastAck, darwinTCPSClosing:
		return true
	case darwinTCPSClosed:
		return eof
	}
	return false
}

// clientGone reports whether the remote end has disconnected: it sent a FIN
// (CLOSE_WAIT, LAST_ACK, CLOSING) or the socket is fully closed (TCPS_CLOSED,
// which for a socket the handler still holds means a reset or a completed
// FIN exchange).
func clientGone(state uint8) bool {
	switch state {
	case darwinTCPSCloseWait, darwinTCPSLastAck, darwinTCPSClosing, darwinTCPSClosed:
		return true
	}
	return false
}
