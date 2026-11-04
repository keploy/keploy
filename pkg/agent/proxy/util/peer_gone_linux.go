//go:build linux

package util

import (
	"net"

	"golang.org/x/sys/unix"
)

// PeerGone reports whether the TCP socket under conn has received its peer's
// FIN or a reset (its state CLOSE_WAIT, LAST_ACK or CLOSING, reached only by
// a FIN, or CLOSE, which a socket still open reaches by a reset), whatever it
// holds unread before it: the kernel's own state, taken at once. known is
// false where it cannot be asked: no socket under conn (conn, then each
// NetConn, as WatchPeerEnd looks), or one already closed.
func PeerGone(conn net.Conn) (gone, known bool) {
	_, rc, ok := socketOf(conn)
	if !ok {
		return false, false
	}
	var info *unix.TCPInfo
	var err error
	if cerr := rc.Control(func(fd uintptr) { info, err = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); cerr != nil || err != nil {
		return false, false
	}
	switch info.State {
	case unix.BPF_TCP_CLOSE_WAIT, unix.BPF_TCP_LAST_ACK, unix.BPF_TCP_CLOSING, unix.BPF_TCP_CLOSE:
		return true, true
	}
	return false, true
}

// finOnly reports whether the TCP socket fd has received its peer's FIN and
// nothing after it: CLOSE_WAIT, which a socket reaches by its peer's FIN
// while its own side is open. A reset moves it on (CLOSE), as a failure to
// ask it reads.
func finOnly(fd uintptr) bool {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	return err == nil && info.State == unix.BPF_TCP_CLOSE_WAIT
}
