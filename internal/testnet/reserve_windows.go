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
// it is returned. Go's listeners set no option on Windows, so no socket can
// hold the port beside the code's listener; instead no bind to port 0 and no
// connect is given a port below the range, and the claim keeps the port from
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

// listenFree listens at port at every address and closes the listener: nil
// when no other socket is at the port and it is not excluded from use.
func listenFree(port int) error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	return ln.Close()
}
