//go:build darwin

package proxy

import (
	"errors"
	"net"
	"syscall"
)

// listenerDrops is 0 on darwin, and 0 is what a drop at the listener costs
// there. macOS has no SO_MEMINFO to count the drops by, but a drop at its
// listener loses no connection a test could miss: with the accept queue full
// it drops the client's SYN silently, so the client's connect stays in
// progress until the listener takes the connection, or fails it. A connect
// that never completes is abandoned, and the shim never numbers it
// (enterprise's TestAnAbandonedConnectIsNeverNumbered,
// pkg/agent/hooks/darwin/proxyconns_test.go); one that fails is the
// application's own error. Probed on macOS 26.7: 200 concurrent loopback
// connects to a listener of backlog 1, four times; one connected, the rest
// timed out, and none completed at the client without reaching accept. The
// system's "listen queue overflow" count (netstat -s -p tcp) stayed 0
// throughout, so it is no count to read instead. On Linux, a rise of the
// listener's count is charged as a capture loss to the open test.
func listenerDrops(syscall.RawConn) (uint64, error) { return 0, nil }

// netListenDrops is not read on darwin: macOS has no network namespace, and
// keploy does not read the system's count of listen-queue drops. The error
// leaves models.ListenDrops.NetKnown false, so every rise of the listener's
// own count is charged (the agent's chargeDrops).
func netListenDrops(syscall.RawConn) (uint64, error) {
	return 0, errors.New("macOS has no network namespace's listen drops for keploy to read")
}

// noFastOpen is a no-op on darwin.
func noFastOpen(net.Listener) error { return nil }
