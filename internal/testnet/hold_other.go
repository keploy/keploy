//go:build !linux

package testnet

import (
	"context"
	"net"
	"strconv"
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

// SharedPort is a TCP port nothing listens at, held for the test's life as
// RefusedAddr's is, by a connection (holdByConnection), at 127.0.0.1 only.
// ListenShared and OpenShared open a listener at 127.0.0.1 at the port beside
// the hold, so a test can close its listener, find a dial to the port
// refused, and listen there again. On macOS a listener that sets
// SO_REUSEADDR, as Go's do, binds beside a connected socket; on Windows,
// where Go's listeners set no option, SharedListenConfig sets SO_REUSEADDR.
// A bind to port 0 is never given the port. A socket that names the port and
// sets SO_REUSEADDR can still take it.
func SharedPort(t *testing.T) int {
	t.Helper()
	_, p, err := net.SplitHostPort(holdByConnection(t))
	require.NoError(t, err)
	port, err := strconv.Atoi(p)
	require.NoError(t, err)
	return port
}

// ListenShared opens a TCP listener at addr, a port SharedPort holds, beside
// the hold; it is closed when the test ends if the test has not closed it.
func ListenShared(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := OpenShared(network, addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// OpenShared is ListenShared that returns its error instead of failing the
// test, for a goroutine the test starts; the caller closes the listener.
func OpenShared(network, addr string) (net.Listener, error) {
	lc := SharedListenConfig()
	return lc.Listen(context.Background(), network, addr)
}

// SharedListenConfig is the net.ListenConfig OpenShared listens with, for a
// test that sets more on it before it listens at a port SharedPort holds:
// its Control is shareControl, which lets the listener bind beside the hold.
func SharedListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: shareControl}
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
