//go:build windows

package util

import (
	"encoding/binary"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows TCP states (mstcpip.h's TCPSTATE) TCP_INFO_v0's State carries,
// of those a peer's FIN or a reset leads to. Windows keeps no error on a
// socket for a reset: a socket still open that its peer reset reads CLOSED.
const (
	wtcpClosed    = 0
	wtcpCloseWait = 7
	wtcpClosing   = 8
	wtcpLastAck   = 9
)

// sioTCPInfo is SIO_TCP_INFO (mstcpip.h: _WSAIORW(IOC_VENDOR, 39)), and
// tcpInfoV0Size sizeof(TCP_INFO_v0), the answer asked for (Windows 10 1703
// and later).
const (
	sioTCPInfo    = 0xD8000027
	tcpInfoV0Size = 88
)

// PeerGone reports whether the TCP socket under conn has received its peer's
// FIN or a reset (its state CLOSE_WAIT, LAST_ACK or CLOSING, reached only by
// a FIN, or CLOSED, which a socket still open reaches by a reset), whatever
// it holds unread before it: the kernel's own state, taken at once
// (SIO_TCP_INFO). known is false where it cannot be asked: no socket under
// conn (conn, then each NetConn), one already closed, or a Windows that has
// no SIO_TCP_INFO.
func PeerGone(conn net.Conn) (gone, known bool) {
	return peerGone(conn, tcpState)
}

// peerGone is PeerGone through stateOf, which a row gives a socket that
// cannot be asked.
func peerGone(conn net.Conn, stateOf func(fd uintptr) (uint32, bool)) (gone, known bool) {
	_, rc, ok := socketOf(conn)
	if !ok {
		return false, false
	}
	var state uint32
	var asked bool
	if cerr := rc.Control(func(fd uintptr) { state, asked = stateOf(fd) }); cerr != nil || !asked {
		return false, false
	}
	switch state {
	case wtcpCloseWait, wtcpLastAck, wtcpClosing, wtcpClosed:
		return true, true
	}
	return false, true
}

// finOnly reports whether the TCP socket fd has received its peer's FIN and
// nothing after it: CLOSE_WAIT, which a socket reaches by its peer's FIN
// while its own side is open. A reset moves it on (CLOSED), as a failure to
// ask it reads.
func finOnly(fd uintptr) bool {
	state, ok := tcpState(fd)
	return ok && state == wtcpCloseWait
}

// tcpState is the TCP socket fd's state (TCP_INFO_v0's State); false when it
// cannot be asked.
func tcpState(fd uintptr) (uint32, bool) {
	return askTCPState(windows.WSAIoctl, fd)
}

// wsaIoctl is windows.WSAIoctl's signature.
type wsaIoctl func(s windows.Handle, iocc uint32, inbuf *byte, cbif uint32, outbuf *byte, cbob uint32,
	cbbr *uint32, overlapped *windows.Overlapped, completionRoutine uintptr) error

// askTCPState is tcpState through ioctl, which a row gives an answer of any
// length or a failure: false when the call fails or answers fewer bytes than
// State's four.
func askTCPState(ioctl wsaIoctl, fd uintptr) (uint32, bool) {
	var buf [tcpInfoV0Size]byte
	var version, got uint32 // version 0: TCP_INFO_v0
	err := ioctl(windows.Handle(fd), sioTCPInfo,
		(*byte)(unsafe.Pointer(&version)), uint32(unsafe.Sizeof(version)),
		&buf[0], uint32(len(buf)), &got, nil, 0)
	if err != nil || got < 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(buf[0:4]), true
}
