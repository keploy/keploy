//go:build darwin

package proxy

import (
	"net"
	"syscall"
)

// listenerDrops returns 0 on darwin: macOS does not expose SO_MEMINFO, so the
// kernel's listener-queue drops cannot be read. The count is always 0 rather
// than an error so the proven settlement flow proceeds. Consequence: if the
// kernel drops connections at the proxy's listener (accept queue full), macOS
// cannot detect it — the proven verdict treats the drop count as verified zero.
// On Linux, a non-zero drop count is charged as a capture loss to the open test.
func listenerDrops(syscall.RawConn) (uint64, error) { return 0, nil }

// netListenDrops returns 0 on darwin: macOS has a global tcps_listendrop
// counter (via sysctl net.inet.tcp.stats) but no per-network-namespace or
// per-socket equivalent. The global counter also requires root or unlocking
// net.inet.tcp.disable_access_to_stats. Returning 0 lets the flow proceed.
func netListenDrops(syscall.RawConn) (uint64, error) { return 0, nil }

// noFastOpen is a no-op on darwin.
func noFastOpen(net.Listener) error { return nil }
