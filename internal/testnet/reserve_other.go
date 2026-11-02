//go:build !linux && !windows

package testnet

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// sharedPort is SharedPort here: the port of a hold by a connection
// (holdByConnection), at a port no bind to port 0 is given. Go's listeners
// set SO_REUSEADDR, which binds beside the connected socket.
func sharedPort(t *testing.T) int {
	t.Helper()
	_, p, err := net.SplitHostPort(holdByConnection(t))
	require.NoError(t, err)
	port, err := strconv.Atoi(p)
	require.NoError(t, err)
	return port
}

// reservedPort is ReservedPort here: SharedPort's port.
func reservedPort(t *testing.T) int {
	t.Helper()
	return sharedPort(t)
}
