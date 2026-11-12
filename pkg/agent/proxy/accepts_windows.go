//go:build windows

package proxy

import (
	"errors"
	"net"
	"syscall"
)

// listenerDrops is 0 on Windows, and 0 is what a drop at the listener costs
// there, as on macOS (accepts_darwin.go). Windows has no count of a
// listener's drops a process can read, but a drop at its listener loses no
// connection a test could miss: with the backlog full, the stack does not
// complete the client's connect (Winsock's listen documents that the client
// is refused, WSAECONNREFUSED), so it fails, or completes once the listener
// has room and lands in the backlog the proxy accepts from. A connect that
// fails is the application's own error, and keploy's shim never numbers it
// (enterprise's connpair: an attempt that never completes is never
// numbered). No connection completes at the client without reaching accept:
// TestOnWindowsAFullBacklogLosesNoConnectionTheClientCompleted
// (accepts_backlog_windows_test.go) floods a listener of backlog 1 that no
// one accepts on and checks that every connect the client saw complete is
// accepted. On Linux, a rise of the listener's count is charged as a capture
// loss to the open test.
func listenerDrops(syscall.RawConn) (uint64, error) { return 0, nil }

// netListenDrops is not read on Windows: it has no network namespace, and
// keploy does not read the system's TCP statistics. The error leaves
// models.ListenDrops.NetKnown false, so every rise of the listener's own
// count is charged (the agent's chargeDrops).
func netListenDrops(syscall.RawConn) (uint64, error) {
	return 0, errors.New("windows has no network namespace's listen drops for keploy to read")
}

// noFastOpen is a no-op on Windows: a listener takes TCP Fast Open only when
// it sets TCP_FASTOPEN itself, which the proxy's does not.
func noFastOpen(net.Listener) error { return nil }
