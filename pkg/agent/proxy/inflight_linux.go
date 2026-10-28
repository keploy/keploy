//go:build linux

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketReceived is what the socket has received in order (tcpi_bytes_received,
// the application's FIN counted as one) and its TCP state (tcpi_state); ok is
// false when it cannot be asked (a socket already closed). The kernel reads
// the state before it takes the socket's lock, and the count under it, so a
// FIN that comes in between is counted with the state from before it: two
// reads that agree on both had no change between the first's count and the
// second's, which brackets the whole second read, and are taken. Ones that never agree give the latest, whose
// count is never older than its state: it can count one more than the state
// says (busy a moment longer), never less.
func socketReceived(raw syscall.RawConn) (received uint64, state uint8, ok bool) {
	read := func() (uint64, uint8, bool) {
		var ti *unix.TCPInfo
		var err error
		if cerr := raw.Control(func(fd uintptr) { ti, err = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); cerr != nil || err != nil {
			return 0, 0, false
		}
		return ti.Bytes_received, ti.State, true
	}
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

// finCounted reports whether the count socketReceived gives
// (tcpi_bytes_received, which counts the application's FIN as one) holds the
// FIN: whether a socket in TCP state state has received it. CLOSE_WAIT,
// LAST_ACK and CLOSING are reached only by one
// (a socket a handler holds never reports TIME_WAIT: the FIN_WAIT2 or
// CLOSING that would reach it leaves it in TCP_CLOSE). One in TCP_CLOSE (a
// reset, or closed as its FIN_WAIT2, CLOSING or LAST_ACK ended) has received
// one for sure only when a read returned the end of the stream (eof).
func finCounted(state uint8, eof bool) bool {
	switch state {
	case tcpCloseWait, tcpLastAck, tcpClosing:
		return true
	case tcpClose:
		return eof
	}
	return false
}

// clientGone reports whether a socket in TCP state state, which its handler
// has not closed, has received its client's FIN or a reset: CLOSE_WAIT,
// LAST_ACK and CLOSING are reached only by a FIN, and TCP_CLOSE, for a
// socket still open, by a reset, or by a FIN once keploy's own half-close
// was answered (FIN_WAIT2 ended). Either way the client has gone.
func clientGone(state uint8) bool {
	switch state {
	case tcpCloseWait, tcpLastAck, tcpClosing, tcpClose:
		return true
	}
	return false
}
