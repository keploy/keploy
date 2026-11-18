package relay

import (
	"encoding/binary"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The TCP states the rows wait for (mstcpip.h's TCPSTATE).
const (
	stateClosed    uint8 = 0
	stateCloseWait uint8 = 7
	stateLastAck   uint8 = 9
)

// fionread is FIONREAD (winsock2.h: _IOR('f', 127, u_long)).
const fionread = 0x4004667F

// tcpInfoV0 asks the socket fd for its TCP_INFO_v0 (88 bytes), apart from
// the relay's own question; false when it cannot be asked.
func tcpInfoV0(fd uintptr) ([88]byte, bool) {
	var buf [88]byte
	var version, got uint32
	err := windows.WSAIoctl(windows.Handle(fd), sioTCPInfo,
		(*byte)(unsafe.Pointer(&version)), uint32(unsafe.Sizeof(version)),
		&buf[0], uint32(len(buf)), &got, nil, 0)
	return buf, err == nil && got >= 88
}

// tcpStateOf is the TCP state of the socket fd: TCP_INFO_v0's State.
func tcpStateOf(fd uintptr) (uint8, bool) {
	buf, ok := tcpInfoV0(fd)
	return uint8(binary.LittleEndian.Uint32(buf[0:4])), ok
}

// ackedOf is how many of the bytes written to the socket fd its peer's stack
// has acknowledged, from TCP_INFO_v0: the bytes sent (BytesOut) less those
// sent again (BytesRetrans) and those not acknowledged yet (BytesInFlight).
// A test may count this way on a fresh connection that has sent no FIN.
func ackedOf(fd uintptr) (uint64, bool) {
	buf, ok := tcpInfoV0(fd)
	out := binary.LittleEndian.Uint64(buf[48:56])
	retrans := uint64(binary.LittleEndian.Uint32(buf[68:72]))
	inFlight := uint64(binary.LittleEndian.Uint32(buf[28:32]))
	if !ok || out < retrans+inFlight {
		return 0, false
	}
	return out - retrans - inFlight, true
}

// unreadOf is how many bytes the socket fd holds that its reader has not read
// (FIONREAD).
func unreadOf(fd uintptr) (int, error) {
	var n, got uint32
	err := windows.WSAIoctl(windows.Handle(fd), fionread, nil, 0,
		(*byte)(unsafe.Pointer(&n)), uint32(unsafe.Sizeof(n)), &got, nil, 0)
	return int(n), err
}

// setReceiveBuffer sets the receive buffer of the socket fd to n bytes
// (SO_RCVBUF).
func setReceiveBuffer(fd uintptr, n int) error {
	return windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_RCVBUF, n)
}

// receiveBufferSizeOf is the receive buffer of the socket fd as it reads it
// back (SO_RCVBUF).
func receiveBufferSizeOf(fd uintptr) (int, error) {
	return windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_RCVBUF)
}

// receiveBufferGrownOf is the most the socket fd, whose receive buffer reads
// back held bytes, can come to hold unread. Windows' socket holds more unread
// than its receive buffer: measured on Windows 11 over loopback, with the
// window closed, a client whose buffer was set to 4096 before it connected
// held 12288 bytes unread, and one set to 65536 held 131072. So the bound is
// four times held and 64K more.
func receiveBufferGrownOf(_ uintptr, held int) (int, error) { return 4*held + 64<<10, nil }

// windowClosedOf reports whether the peer of the socket fd has closed its
// receive window, from its TCP_INFO_v0: the window the peer last offered
// (SndWnd) is 0, and no more than the one byte of the proxy's window probe is
// in flight (wintcpinfo.go's counted). Windows does not count what its send
// buffer still holds, so a row that waits for this writes more than the
// client's socket can come to hold (receiveBufferBoundOf). False when it
// cannot be asked.
func windowClosedOf(fd uintptr) bool {
	buf, ok := tcpInfoV0(fd)
	return ok && binary.LittleEndian.Uint32(buf[36:40]) == 0 && binary.LittleEndian.Uint32(buf[28:32]) <= 1
}

// ackedThrough is how many of the bytes written to the socket fd its peer has
// acknowledged, from its TCP_INFO_v0 (ackedOf), on a connection that has sent
// no FIN: Windows does not count what its send buffer still holds, so the
// bytes written are not needed.
func ackedThrough(fd uintptr, _ uint64) (uint64, error) {
	acked, ok := ackedOf(fd)
	if !ok {
		return 0, errors.New("the socket's TCP_INFO_v0 cannot be asked, or sent fewer bytes than it resent and has in flight")
	}
	return acked, nil
}

// probeResentOf reports whether the socket fd has sent a byte again
// (TCP_INFO_v0's BytesRetrans); known is false when it cannot be asked.
func probeResentOf(fd uintptr) (resent, known bool) {
	buf, ok := tcpInfoV0(fd)
	return ok && binary.LittleEndian.Uint32(buf[68:72]) > 0, ok
}
