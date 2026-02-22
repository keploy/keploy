//go:build linux

package orchestrator

import (
	"net"
	"syscall"
)

// TCP_QUICKACK is the Linux socket option to disable delayed ACKs.
const tcpQuickACK = 12

// SetTCPQuickACK disables delayed ACKs to reduce read-side latency.
// Delayed ACKs can add up to 40ms on Linux when the proxy reads from
// one connection and writes to another (the piggyback ACK path never fires).
// NOTE: Linux resets TCP_QUICKACK after every ACK, so this must be called
// repeatedly — ideally after every Write() that precedes a Read().
// Handles TLS-wrapped connections by unwrapping to the underlying *net.TCPConn.
func SetTCPQuickACK(conn net.Conn) {
	tc := unwrapTCPConn(conn)
	if tc == nil {
		return
	}
	rawConn, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = rawConn.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpQuickACK, 1)
	})
}
