//go:build !linux

package testnet

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Nothing is held here (see the package doc): a port is one a listener left
// when it closed, and another socket can take it meanwhile.

// RefusedAddr is a loopback address, 127.0.0.1:P, that nothing listened at
// when it was taken: with network "tcp" for a connect, with "udp" for a
// datagram.
func RefusedAddr(t *testing.T, network string) string {
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

// SharedPort is a TCP port that nothing listened at when it was taken.
func SharedPort(t *testing.T) int {
	t.Helper()
	_, port, err := net.SplitHostPort(RefusedAddr(t, "tcp"))
	require.NoError(t, err)
	p, err := strconv.Atoi(port)
	require.NoError(t, err)
	return p
}

// ListenShared opens a TCP listener at addr, a port SharedPort gave, closed
// when the test ends if the test has not closed it. The test is skipped when
// another socket took the port.
func ListenShared(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := OpenShared(network, addr)
	if err != nil {
		t.Skipf("cannot listen at %s, another socket took the port: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// OpenShared opens a TCP listener at addr, a port SharedPort gave, and
// returns its error instead of skipping the test; the caller closes it.
func OpenShared(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}

// BoundHere cannot tell here which sockets are bound at port: known is
// false.
func BoundHere(t *testing.T, port int) (n int, known bool) {
	t.Helper()
	return 0, false
}

// GiveAway works on Linux only, where BoundHere can tell whose socket is at
// a port: here it skips the test.
func GiveAway(t *testing.T, ln *net.TCPListener) {
	t.Helper()
	t.Skip("BoundHere cannot tell here whose socket is at a port")
}
