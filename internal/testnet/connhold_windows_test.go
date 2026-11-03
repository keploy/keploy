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

// errHeldBind is what a bind at 127.0.0.1 at a held port ends in. The hold
// binds the port exclusively at the wildcard address (holdControl), and
// Windows refuses a bind beside such a socket with WSAEACCES when the bind
// names a specific address and sets no option, as Go's do, not with
// WSAEADDRINUSE (Microsoft's "Using SO_REUSEADDR and SO_EXCLUSIVEADDRUSE",
// Enhanced Socket Security). A bind at the same address as a socket without
// the option still ends in WSAEADDRINUSE (errAddrInUse).
var errHeldBind error = windows.WSAEACCES

// requireUDPHeld checks that a UDP bind at addr is refused because the hold
// is at the port (errHeldBind). Go turns off Windows's report of a port
// unreachable answer on its UDP sockets (SIO_UDP_CONNRESET), so a read there
// never says a datagram was refused, and the refusal is checked on Unix only.
func requireUDPHeld(t *testing.T, addr string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = pc.Close()
	}
	require.ErrorIs(t, err, errHeldBind, "a UDP socket took the held port %s", addr)
}
