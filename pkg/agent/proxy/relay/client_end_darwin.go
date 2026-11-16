//go:build darwin

package relay

import "golang.org/x/sys/unix"

// unresetError is the reset xnu does not keep as the socket's error: a reset
// that comes once the proxy's FIN is sent, in LAST_ACK or CLOSING, closes the
// connection and sets no error (tcp_input), where Linux keeps ECONNRESET
// (tcp_reset, in every state but CLOSE). The relay's FIN can go before the
// closed client's reset of the last chunk comes back: the destination's
// forwarder forwards the destination's FIN as soon as it reads it, and on
// macOS the loopback hands the client's stack the chunk and the FIN apart.
//
// Such a connection is closed with bytes the client's stack never
// acknowledged (tcpi_txunacked: the chunk the reset answered, and the FIN):
// a connection closed by the client's acknowledgement of the proxy's FIN has
// every byte acknowledged, and one a timeout ended keeps its error
// (ETIMEDOUT). So a closed connection with bytes outstanding gives
// ECONNRESET, once, as the socket's error would. 0 when it holds none, the
// socket cannot say, or it was given.
func (s *tcpClientSocket) unresetError(fd uintptr) int {
	if s.resetGiven || !s.askInfo(fd, &s.ebuf, &s.einfo) {
		return 0
	}
	if s.einfo.state != tcpsClosed || s.einfo.txunacked == 0 {
		return 0
	}
	s.resetGiven = true
	return int(unix.ECONNRESET)
}
