//go:build !linux

package testnet

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// RefusedAddr's address, held by a connection here, stays its own while the
// test runs and refuses a dial: a TCP connect, a UDP datagram (requireHeld
// and requireUDPHeld say how, per platform).
func TestARefusedAddressRefusesADialForTheTestsLife(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		addr := RefusedAddr(t, "tcp")
		requireHeld(t, addr)
		requireDialRefused(t, addr)
	})
	t.Run("udp", func(t *testing.T) {
		requireUDPHeld(t, RefusedAddr(t, "udp"))
	})
}

// A shared port is kept for the test's life beside the test's own listeners:
// while none is open there, it is kept from other sockets (requireShared
// says how, per platform) and a dial to it is refused; the test's listener
// (ListenShared) opens at it, takes a dial, closes, and opens there again.
func TestASharedPortIsKeptForTheTestsLifeBesideItsOwnListeners(t *testing.T) {
	port := SharedPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	requireShared(t, port)
	requireDialRefused(t, addr)
	for _, round := range []string{"first", "again"} {
		ln := ListenShared(t, "tcp", addr)
		c, err := net.Dial("tcp", addr)
		require.NoError(t, err, "dial the test's listener, %s", round)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireShared(t, port)
		requireDialRefused(t, addr)
	}
}

// A reserved port is kept from other sockets (requireReserved says how, per
// platform) and takes the code's own listener, as net.Listen opens it, at
// 0.0.0.0 and at 127.0.0.1; the listener takes a dial, and once it closes a
// dial to the port is refused.
func TestAReservedPortTakesTheCodesListener(t *testing.T) {
	p := ReservedPort(t)
	requireReserved(t, p)
	port := strconv.Itoa(p)
	addr := net.JoinHostPort("127.0.0.1", port)
	for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
		ln, err := net.Listen("tcp4", net.JoinHostPort(host, port))
		require.NoError(t, err, "the code's listener at %s", host)
		c, err := net.Dial("tcp4", addr)
		require.NoError(t, err, "a dial to the code's listener at %s", host)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireDialRefused(t, addr)
	}
}
