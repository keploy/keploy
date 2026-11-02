//go:build !linux

package testnet

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// RefusedAddr is a loopback address, 127.0.0.1:P, that refuses every dial
// for the test's life: with network "tcp" a connect, with "udp" a datagram.
// P is the local end of a connection that stays open until the test ends
// (holdByConnection, holdUDPByConnection): a socket that is only bound, as
// RefusedAddr holds the port on Linux, does not refuse a connect on macOS.
// On Windows the holding socket binds P exclusively. P is held at 127.0.0.1
// only.
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

// SharedPort is a TCP port nothing listens at, kept for the test's life,
// that the test's own listeners open at: ListenShared and OpenShared open a
// listener at 127.0.0.1 at the port, so a test can close its listener, find
// a dial to the port refused, and listen there again. On macOS the port is
// held as RefusedAddr's is, by a connection (holdByConnection), at 127.0.0.1
// only, and a listener that sets SO_REUSEADDR, as Go's do, binds beside the
// connected socket. On Windows it is ReservedPort's port, claimed below the
// dynamic port range with no socket at it (sharedPort). A bind to port 0 is
// never given the port. A socket that names the port can still take it.
func SharedPort(t *testing.T) int {
	t.Helper()
	return sharedPort(t)
}

// ListenShared opens a TCP listener at addr, a port SharedPort keeps (on
// macOS beside the connection that holds it); it is closed when the test
// ends if the test has not closed it.
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
// test that sets more on it before it listens at a port SharedPort keeps:
// its Control is shareControl, which sets nothing here (see shareControl).
func SharedListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: shareControl}
}

// ReservedPort is a free TCP port for code under test that listens at it by
// number with net.Listen, at any address, kept from every other socket that
// is given a port without naming one for the test's life. On macOS it is
// SharedPort's port: held by a connection at 127.0.0.1, at a port no bind to
// port 0 is given, and Go's listeners there set SO_REUSEADDR, which binds
// beside the connected socket; once the code's listener closes, a dial to the
// port is refused again. On Windows, where no socket can hold the port for
// the code's listener (an exclusive hold refuses the listener's bind, and
// one that is not exclusive keeps no socket off the port), it is a port
// below the dynamic port range, which no bind to port 0 and no connect is
// given, claimed for the test's life so that no other test is given it
// either, and SharedPort's port is the same. A socket that names the port
// can still take it.
func ReservedPort(t *testing.T) int {
	t.Helper()
	return reservedPort(t)
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
