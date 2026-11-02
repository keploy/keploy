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

// A shared port is held for the test's life beside the test's own listeners:
// while none is open there, no other socket can bind it (requireHeld) and a
// dial to it is refused; the test's listener (ListenShared) opens at it,
// takes a dial, closes, and opens there again.
func TestASharedPortIsHeldForTheTestsLifeBesideItsOwnListeners(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(SharedPort(t)))
	requireHeld(t, addr)
	requireDialRefused(t, addr)
	for _, round := range []string{"first", "again"} {
		ln := ListenShared(t, "tcp", addr)
		c, err := net.Dial("tcp", addr)
		require.NoError(t, err, "dial the test's listener, %s", round)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireHeld(t, addr)
		requireDialRefused(t, addr)
	}
}
