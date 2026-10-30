// Package plaintcptest checks, in tests, that a listener keploy opened is
// plain TCP and not MPTCP (see package plaintcp).
package plaintcptest

import (
	"net"
	"testing"
)

// RequirePlainTCP fails t unless ln's socket is plain TCP (its SO_PROTOCOL is
// IPPROTO_TCP, not IPPROTO_MPTCP). It skips t, saying why, where a listener
// opened with Go's defaults is plain TCP too (no MPTCP in the kernel,
// net.mptcp.enabled=0, GODEBUG multipathtcp off, or not Linux): there the
// check could not tell a listener that turns MPTCP off from one that does not.
func RequirePlainTCP(t testing.TB, ln net.Listener) {
	t.Helper()
	requirePlainTCP(t, ln)
}

// tcpListener is ln's *net.TCPListener, or t fails.
func tcpListener(t testing.TB, ln net.Listener) *net.TCPListener {
	t.Helper()
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("the listener is a %T, not a *net.TCPListener", ln)
	}
	return tl
}
