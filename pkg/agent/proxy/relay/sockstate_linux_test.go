package relay

import "golang.org/x/sys/unix"

// The TCP states the rows wait for (include/net/tcp_states.h).
const (
	stateClosed    = unix.BPF_TCP_CLOSE
	stateCloseWait = unix.BPF_TCP_CLOSE_WAIT
	stateLastAck   = unix.BPF_TCP_LAST_ACK
)

// tcpStateOf is the TCP state of the socket fd (TCP_INFO tcpi_state); false
// when it cannot be asked.
func tcpStateOf(fd uintptr) (uint8, bool) {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, false
	}
	return info.State, true
}

// ackedOf is how many of the bytes written to the socket fd its peer's stack
// has acknowledged (TCP_INFO tcpi_bytes_acked); false when it cannot be asked.
func ackedOf(fd uintptr) (uint64, bool) {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, false
	}
	return info.Bytes_acked, true
}

// unreadOf is how many bytes the socket fd holds unread (SIOCINQ).
func unreadOf(fd uintptr) (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCINQ) }

// queuedOf is how many bytes the socket fd holds that its peer has not
// acknowledged yet, sent or not (SIOCOUTQ).
func queuedOf(fd uintptr) (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }

// windowClosedOf reports whether the peer of the socket fd has closed its
// receive window: every byte sent is acknowledged (tcpi_unacked 0) and some
// are not sent (tcpi_notsent_bytes); false when it cannot be asked.
func windowClosedOf(fd uintptr) bool {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	return err == nil && info.Unacked == 0 && info.Notsent_bytes > 0
}
