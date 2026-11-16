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

// receiveBufferGrownOf is the most the receive buffer of the socket fd, which
// holds held bytes now and was asked for smallWindow before it connected, can
// come to hold. xnu grows even a buffer the application sized
// (tcp_sbrcv_grow_rwin, bsd/netinet/tcp_subr.c:3655, xnu-12377.121.6): by
// rcvbufinc, 16 of its segments (t_maxseg << 4, TCP_MAXSEG), each time the
// room left in it is less than that while it holds no more than its ideal
// size (sb_idealsize: the size asked for, which SO_RCVBUF sets in
// sosetoptlock, bsd/kern/uipc_socket.c:4895) and one such step. It is that
// small only once tcp_sbrcv_trim (bsd/netinet/tcp_input.c:1178), as the
// application reads, took it back toward what it holds; the trim never takes
// it below the ideal size, and nothing else grows a buffer sized so:
// tcp_sbrcv_grow needs SB_AUTOSIZE, which SO_RCVBUF clears, and tcp_mss
// (tcp_input.c:7304) sizes it only as it connects. So from now on it never
// holds more than held, or the size asked for and two steps: on macOS 26 it
// reads back 326640 as it connects and was seen at 522544 after a read of
// 64K (2048 and two steps of 16 * 16332 are 524672).
func receiveBufferGrownOf(fd uintptr, held int) (int, error) {
	mss, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
	if err != nil {
		return 0, err
	}
	return max(held, smallWindow+2*(mss<<4)), nil
}
