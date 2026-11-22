//go:build darwin

package util

import (
	"net"

	"golang.org/x/sys/unix"
)

// The BSD TCP states (netinet/tcp_fsm.h) TCP_CONNECTION_INFO's tcpi_state
// carries, of those a peer's FIN or a reset leads to.
const (
	tcpsClosed    = 0
	tcpsCloseWait = 5
	tcpsClosing   = 7
	tcpsLastAck   = 8
)

// PeerGone reports whether the TCP socket under conn has received its peer's
// FIN or a reset (its state CLOSE_WAIT, LAST_ACK or CLOSING, reached only by
// a FIN, or CLOSED, which a socket still open reaches by a reset), whatever
// it holds unread before it: the kernel's own state, taken at once. known is
// false where it cannot be asked: no socket under conn (conn, then each
// NetConn, as WatchPeerEnd looks), or one already closed.
func PeerGone(conn net.Conn) (gone, known bool) {
	_, rc, ok := socketOf(conn)
	if !ok {
		return false, false
	}
	var info *unix.TCPConnectionInfo
	var err error
	if cerr := rc.Control(func(fd uintptr) {
		info, err = unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	}); cerr != nil || err != nil {
		return false, false
	}
	switch info.State {
	case tcpsCloseWait, tcpsLastAck, tcpsClosing, tcpsClosed:
		return true, true
	}
	return false, true
}
