//go:build linux

package http

import "golang.org/x/sys/unix"

// The TCP states the relayed rows wait for (include/net/tcp_states.h).
const (
	stateEstablished = unix.BPF_TCP_ESTABLISHED
	stateClosed      = unix.BPF_TCP_CLOSE
)

// tcpInfoOf is the socket fd's TCP state and the count of the bytes it has
// received, read or not (TCP_INFO's tcpi_bytes_received); false when it
// cannot be asked.
func tcpInfoOf(fd uintptr) (state uint8, received uint64, ok bool) {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, 0, false
	}
	return info.State, info.Bytes_received, true
}
