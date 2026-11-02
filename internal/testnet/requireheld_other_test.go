//go:build !darwin

package testnet

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireHeld checks that a listen on addr is refused because the hold is at
// the port (errHeldBind says with what, per platform): no other socket can
// bind the held port.
func requireHeld(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("listened on %s: the port is free for any process to take, so a server there can answer the closed port", addr)
	}
	require.ErrorIs(t, err, errHeldBind, "listen on %s: want the bind refused for the hold", addr)
}
