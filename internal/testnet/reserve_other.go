//go:build !linux && !windows

package testnet

import "testing"

// reservedPort is ReservedPort here: SharedPort's port, held by a
// connection at a port no bind to port 0 is given; Go's listeners set
// SO_REUSEADDR, which binds beside the connected socket.
func reservedPort(t *testing.T) int {
	t.Helper()
	return SharedPort(t)
}
