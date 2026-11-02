//go:build windows

package testnet

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// What a dial to a held address ends in.
var errConnRefused error = windows.WSAECONNREFUSED

// requireUDPHeld checks that a UDP bind at addr fails because the address is
// in use. Go turns off Windows's report of a port unreachable answer on its
// UDP sockets (SIO_UDP_CONNRESET), so a read there never says a datagram was
// refused, and the refusal is checked on Unix only.
func requireUDPHeld(t *testing.T, addr string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = pc.Close()
	}
	require.ErrorIs(t, err, errAddrInUse, "a UDP socket took the held port %s", addr)
}
