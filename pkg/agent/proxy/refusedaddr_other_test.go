//go:build !linux

package proxy

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The ports these tests expect refused or free are held only on Linux
// (refusedaddr_linux_test.go). Elsewhere a socket that only holds a port does
// not refuse a dial the same way: macOS drops a SYN sent to a bound socket
// that is not listening, by XNU's source, instead of refusing it; Windows is
// not checked. So here a port is one a listener left when it closed, and
// another socket can take it meanwhile. CI runs this package on Linux only.

// refusedAddr is a loopback address, 127.0.0.1:P, that nothing listened at
// when it was taken: with network "tcp" for a connect, with "udp" for a
// datagram.
func refusedAddr(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := pc.LocalAddr().String()
		require.NoError(t, pc.Close())
		return addr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// sharedPort is a TCP port that nothing listened at when it was taken.
func sharedPort(t *testing.T) int {
	t.Helper()
	_, port, err := net.SplitHostPort(refusedAddr(t, "tcp"))
	require.NoError(t, err)
	p, err := strconv.Atoi(port)
	require.NoError(t, err)
	return p
}

// listenShared opens a TCP listener at addr, a port sharedPort gave, closed
// when the test ends if the test has not closed it. The test is skipped when
// another socket took the port.
func listenShared(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen at %s, another socket took the port: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}
