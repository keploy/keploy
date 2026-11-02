//go:build !linux

package testnet

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// RefusedAddr is a loopback address, 127.0.0.1:P, that refuses every dial
// for the test's life: with network "tcp" a connect, with "udp" a datagram.
// P is the local end of a connection that stays open until the test ends
// (holdByConnection, holdUDPByConnection): a socket that is only bound, as
// RefusedAddr holds the port on Linux, does not refuse a connect on macOS.
// P is held at 127.0.0.1 only.
func RefusedAddr(t *testing.T, network string) string {
	t.Helper()
	switch network {
	case "tcp":
		return holdByConnection(t)
	case "udp":
		return holdUDPByConnection(t)
	}
	t.Fatalf("RefusedAddr: network %q is neither tcp nor udp", network)
	return ""
}

// SharedPort is a TCP port that nothing listened at when it was taken.
// Nothing holds it here (see the package doc): it is one a listener left
// when it closed, and another socket can take it meanwhile.
func SharedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
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
