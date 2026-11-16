package relay

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The TCP states the rows wait for (xnu bsd/netinet/tcp_fsm.h).
const (
	stateClosed    uint8 = 0
	stateCloseWait uint8 = 5
	stateLastAck   uint8 = 8
)

// tcpStateOf is the TCP state of the socket fd: the first byte of xnu's
// TCP_INFO (0x200, struct tcp_info's tcpi_state), asked apart from the
// relay's own question; false when it cannot be asked.
func tcpStateOf(fd uintptr) (uint8, bool) {
	var buf [512]byte
	size := uint32(len(buf))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, 0x200,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	return buf[0], errno == 0 && size > 0
}

// ackedOf is how many of the bytes written to the socket fd its peer's stack
// has acknowledged, from xnu's TCP_INFO: the bytes sent (tcpi_txbytes) less
// those sent again (tcpi_txretransmitbytes) and those not acknowledged yet
// (tcpi_txunacked). The relay does not count this way (txbytes is kept only
// while the kernel collects network statistics, as it does by default); a
// test may, on a fresh connection that has sent no FIN. False when it cannot
// be asked.
func ackedOf(fd uintptr) (uint64, bool) {
	var buf [512]byte
	size := uint32(len(buf))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, 0x200,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 || size < 100 {
		return 0, false
	}
	le := binary.LittleEndian
	return le.Uint64(buf[76:]) - le.Uint64(buf[84:]) - le.Uint64(buf[92:]), true
}

// unreadOf is how many bytes the socket fd holds unread (SO_NREAD).
func unreadOf(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NREAD)
}

// queuedOf is how many bytes the socket fd holds that its peer has not
// acknowledged yet, sent or not (SO_NWRITE).
func queuedOf(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE)
}

// windowClosedOf reports whether the peer of the socket fd has closed its
// receive window: every byte sent is acknowledged (tcpi_txunacked 0) and the
// send buffer still holds some (tcpi_snd_sbbytes), so they are not sent;
// false when it cannot be asked.
func windowClosedOf(fd uintptr) bool {
	var buf [512]byte
	size := uint32(len(buf))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, 0x200,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 || size < 100 {
		return false
	}
	le := binary.LittleEndian
	return le.Uint64(buf[92:]) == 0 && le.Uint32(buf[64:]) > 0
}
