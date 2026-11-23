//go:build windows

package http

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The TCP states the relayed rows wait for (mstcpip.h's TCPSTATE).
const (
	stateEstablished uint8 = 4
	stateClosed      uint8 = 0
)

// sioTCPInfo is SIO_TCP_INFO (mstcpip.h: _WSAIORW(IOC_VENDOR, 39)).
const sioTCPInfo = 0xD8000027

// tcpInfoOf is the socket fd's TCP state and the count of the bytes it has
// received, read or not (TCP_INFO_v0's State and BytesIn); false when it
// cannot be asked.
func tcpInfoOf(fd uintptr) (state uint8, received uint64, ok bool) {
	var buf [88]byte
	var version, got uint32
	err := windows.WSAIoctl(windows.Handle(fd), sioTCPInfo,
		(*byte)(unsafe.Pointer(&version)), uint32(unsafe.Sizeof(version)),
		&buf[0], uint32(len(buf)), &got, nil, 0)
	if err != nil || got < 88 {
		return 0, 0, false
	}
	return uint8(binary.LittleEndian.Uint32(buf[0:4])), binary.LittleEndian.Uint64(buf[56:64]), true
}
