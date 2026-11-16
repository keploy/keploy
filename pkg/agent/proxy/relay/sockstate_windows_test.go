package relay

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The TCP states the rows wait for (mstcpip.h's TCPSTATE).
const (
	stateClosed    uint8 = 0
	stateCloseWait uint8 = 7
	stateLastAck   uint8 = 9
)

// sioTCPInfo is SIO_TCP_INFO (mstcpip.h: _WSAIORW(IOC_VENDOR, 39)), which
// fills a TCP_INFO_v0 for version 0 (Windows 10 1703 and later).
const sioTCPInfo = 0xD8000027

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
