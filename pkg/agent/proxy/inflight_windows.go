//go:build windows

package proxy

import "syscall"

// socketReceived is what the socket has received of the application's data
// (SIO_TCP_INFO's BytesIn, which a standard user may read of a socket it
// holds, tcpInfo) and its TCP state (a TCPSTATE); ok is false when it cannot
// be asked (a socket already closed). One ioctl reads both. A settle cuts a
// connection at this count (cutNow), so bytes that arrived and that no read
// of the handler's has taken are waited for, as on Linux and macOS.
func socketReceived(raw syscall.RawConn) (received uint64, state uint8, ok bool) {
	received, state, err := tcpInfo(raw)
	return received, state, err == nil
}

// finCounted is false on Windows: BytesIn counts the application's data
// alone, never its FIN.
func finCounted(uint8, bool) bool { return false }

// clientGone reports whether a socket in TCP state state, which its handler
// has not closed, has received its client's FIN or a reset: CLOSE_WAIT,
// LAST_ACK, CLOSING and TIME_WAIT are reached only after the client's FIN,
// and CLOSED, for a socket still open, by a reset, or once both FINs were
// taken (Windows takes TIME_WAIT and LAST_ACK's end off a socket its holder
// still has: readProxySide). Either way the client has gone.
func clientGone(state uint8) bool {
	switch state {
	case winTCPCloseWait, winTCPLastAck, winTCPClosing, winTCPTimeWait, winTCPClosed:
		return true
	}
	return false
}

// sockBase is nothing on Windows: BytesIn counts from the connection's first
// byte, so there is nothing to read as it is kept.
type sockBase struct{}

// read reads nothing on Windows (sockBase).
func (*sockBase) read(syscall.RawConn) {}

// socketCounts is socketReceived of c's socket.
func (c *trackedConn) socketCounts(connView) (received uint64, state uint8, ok bool) {
	return socketReceived(c.raw)
}
