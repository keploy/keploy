//go:build windows

package testnet

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// reservedPort is ReservedPort on Windows: a port below the dynamic port
// range, IPv4's and IPv6's, that netsh reports, claimed in os.TempDir for
// the test's life (claimPort) and free for a listener at every address when
// it is returned. No socket can hold the port for the code's listener: an
// exclusive hold refuses the listener's bind, and one that is not exclusive
// keeps no socket off the port. Instead no bind to port 0 and no connect is
// given a port below the range, and the claim keeps the port from
// every other test that claims one. A socket that names the port can still
// take it.
func reservedPort(t *testing.T) int {
	t.Helper()
	var outs []string
	for _, family := range []string{"ipv4", "ipv6"} {
		out, err := exec.Command("netsh", "interface", family, "show", "dynamicportrange", "protocol=tcp").Output()
		require.NoError(t, err, "read the %s dynamic port range", family)
		outs = append(outs, string(out))
	}
	start, err := dynamicPortStart(outs...)
	require.NoError(t, err)
	port, err := claimPort(t, os.TempDir(), 1024, start, listenFree)
	require.NoError(t, err)
	return port
}

// sharedPort is SharedPort on Windows: ReservedPort's port, claimed below
// the dynamic port range, with no socket at it. No hold here keeps other
// sockets off a port and lets the test's listeners bind it: no listener
// binds beside an exclusive hold, and a listener that sets no option, as
// Go's do, binds 127.0.0.1 at a port a socket that is not exclusive holds
// at the wildcard address, as any other socket can.
func sharedPort(t *testing.T) int {
	t.Helper()
	return reservedPort(t)
}

// listenFree listens at port at every address and closes the listener: nil
// when no other socket is at the port and it is not excluded from use.
func listenFree(port int) error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	return ln.Close()
}
