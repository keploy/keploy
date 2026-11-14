//go:build windows

package proxy

import (
	"errors"
	"net"
	"syscall"
)

// listenerDrops is 0 on Windows: Windows has no count of a listener's drops
// a process can read, and a drop there is charged from the application's
// side of the connection instead. With the backlog full, the stack refuses
// most connects (WSAECONNREFUSED, the application's own error, which keploy's
// shim never numbers), but it can also drop one the client saw complete and
// reset it (TestOnWindowsAFullBacklogResetsAConnectionItDropsAfterTheClientCompletedIt,
// accepts_backlog_windows_test.go: on Windows 11, 7 connects completed at the
// client and 5 reached accept). Such a connection is in the hooks' count of
// the application's connections (agent.ProxyConnsReporter: the shim numbers a
// connect once it completes), and a test settles only once each of those is
// paired with the proxy's side of it or lost (AppConnsSettled): one the
// listener dropped is never paired, so the test's settle waits until the
// hooks find it gone and charge it as a capture loss, or gives up and leaves
// the test not judged. Hooks with no such count leave the counts unknown
// (models.ErrProxyConnsUnsupported). On Linux, a rise of the listener's count
// is also charged as a capture loss to the open test.
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
