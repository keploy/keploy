//go:build !linux && !windows

package testnet

import (
	"net"
	"strconv"
	"testing"
)

// requireReserved checks that a socket holds port at 127.0.0.1 and that no
// bind to port 0 can be given it (requireHeld).
func requireReserved(t *testing.T, port int) {
	t.Helper()
	requireHeld(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// requireShared is requireReserved here, where ReservedPort's port is
// SharedPort's.
func requireShared(t *testing.T, port int) {
	t.Helper()
	requireReserved(t, port)
}
