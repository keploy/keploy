//go:build darwin

package relay

import "golang.org/x/sys/unix"

// unresetError is the reset xnu kept no error for: the socket fd's TCP_INFO
// says the connection closed with bytes the client's stack never
// acknowledged, after the client's FIN (finRead: the relay read it; or a look
// found it in), and its SO_ERROR holds none. ECONNRESET, once, as Linux's
// socket gives it; 0 otherwise, and when the socket cannot say.
//
// xnu's tcp_input (bsd/netinet/tcp_input.c, read against xnu-12377.121.6)
// takes a reset in each state as Linux's tcp_reset does, with one difference:
// in ESTABLISHED, FIN_WAIT_1, FIN_WAIT_2 and CLOSE_WAIT it sets so_error to
// ECONNRESET and closes the connection; in CLOSING and LAST_ACK, the states
// the proxy's FIN goes into after the client's, it closes it and sets none,
// where Linux keeps ECONNRESET. The relay's FIN goes once the destination's
// end is read, and the destination ends as soon as it reads the client's FIN
// the relay forwarded: on the loopback, that FIN can go before the closed
// client's reset of the last chunk comes back (a Mac run of 100 rounds, a
// chunk then a FIN to a closed client: closed with SO_ERROR 0 and bytes
// unacknowledged in all 100; a chunk with no FIN: ECONNRESET in all 100).
//
// Nothing else closes such a connection with no error and a byte
// unacknowledged: an acknowledgement of the proxy's FIN closes LAST_ACK only
// once every byte before it is acknowledged (tcpi_txunacked 0), CLOSING goes
// to TIME_WAIT the same way, and a timeout or a drop sets its error
// (tcp_drop: ETIMEDOUT or the cause). A reset with no FIN before it is in a
// state that keeps its error, so the FIN is required too: a socket that cannot
// say it came gives none.
func (s *tcpClientSocket) unresetError(fd uintptr, finRead bool) int {
	if s.ended || !s.askInfo(fd, &s.ebuf, &s.einfo) {
		return 0
	}
	if s.einfo.state != tcpsClosed || s.einfo.txunacked == 0 {
		return 0
	}
	s.mu.Lock()
	finIn := s.finIn
	s.mu.Unlock()
	if !finRead && !finIn {
		return 0
	}
	// The kernel clears an error as it gives it: so does this one.
	s.ended = true
	return int(unix.ECONNRESET)
}

// endGiven notes that SO_ERROR gave the connection's error. xnu keeps one on
// a TCP socket only for what ends the connection (tcp_input's reset, or
// tcp_drop's timeout or cause; a soft error such as an ICMP unreachable is
// kept apart, in t_softerror, and never closes an ESTABLISHED connection).
// Once given and cleared, the socket closed on it holds no error and, after
// the client's FIN, bytes unacknowledged: what unresetError reads as a reset
// it kept none for. That is the same end, given already, so unresetError
// gives nothing more: a closed client's reset that came before the proxy's
// FIN went (in CLOSE_WAIT, where xnu keeps ECONNRESET) is handed on once.
func (s *tcpClientSocket) endGiven() { s.ended = true }
