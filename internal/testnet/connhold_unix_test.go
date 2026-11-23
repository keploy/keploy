//go:build unix

package testnet

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// What a dial to a held address ends in.
var errConnRefused error = syscall.ECONNREFUSED

// errHeldBind is what a bind at a held port ends in: the address is in use,
// as at any port another socket is at.
var errHeldBind error = syscall.EADDRINUSE

// The address holdUDPByConnection gives stays its own while the test runs:
// no other UDP socket can bind it, and a datagram sent to it is answered port
// unreachable, which a connected sender reads as refused.
func TestAUDPPortHeldByAConnectionCannotBeTakenAndRefusesADatagram(t *testing.T) {
	requireUDPHeld(t, holdUDPByConnection(t))
}

// requireUDPHeld checks that a UDP bind at addr is refused because the hold
// is at the port (errHeldBind), and that a datagram sent to addr is refused.
// A hold that took the datagram would answer nothing; the deadline makes
// that a failure, not a hang.
func requireUDPHeld(t *testing.T, addr string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = pc.Close()
	}
	require.ErrorIs(t, err, errHeldBind, "a UDP socket took the held port %s", addr)
	c, err := net.Dial("udp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = c.Write([]byte("q"))
	require.NoError(t, err)
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, errConnRefused, "a datagram to %s", addr)
}
