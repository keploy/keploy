//go:build darwin

package http

import "golang.org/x/sys/unix"

// The TCP states the relayed rows wait for (xnu bsd/netinet/tcp_fsm.h).
const (
	stateEstablished uint8 = 4
	stateClosed      uint8 = 0
)

// tcpInfoOf is the socket fd's TCP state and the count of the bytes it has
// received, read or not (TCP_CONNECTION_INFO's tcpi_rxbytes); false when it
// cannot be asked.
func tcpInfoOf(fd uintptr) (state uint8, received uint64, ok bool) {
	info, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	if err != nil {
		return 0, 0, false
	}
	return info.State, info.Rxbytes, true
}
