//go:build !linux

package plaintcptest

import (
	"net"
	"testing"
)

func requirePlainTCP(t testing.TB, ln net.Listener) {
	t.Helper()
	tcpListener(t, ln)
	t.Skip("MPTCP listeners are Linux-only in Go: every TCP listener here is plain TCP")
}
