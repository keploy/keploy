// Package plaintcp opens TCP listeners that are plain TCP whatever Go's
// default. Since Go 1.24, net.Listen, net.ListenTCP and a zero
// net.ListenConfig open an MPTCP listener on Linux wherever the kernel has
// MPTCP (GODEBUG multipathtcp's default). keploy's agent listens for the
// application, on its port and on every interface, during record and test: an
// MPTCP listener there puts a plain-TCP application's port behind the
// kernel's MPTCP code, which the application never chose (a remote
// use-after-free in it, CVE-2026-98432, needs exactly that listener).
//
// Every listener keploy opens on behalf of an application, or that the
// network can reach, is opened here. SetMultipathTCP(false) wins over GODEBUG
// and makes a plain TCP socket on every kernel and platform.
package plaintcp

import (
	"context"
	"net"
	"syscall"
)

// Config is a ListenConfig whose listeners are plain TCP, never MPTCP, with
// control (nil for none) run on each socket before it is bound, as
// net.ListenConfig.Control is.
func Config(control func(network, address string, c syscall.RawConn) error) net.ListenConfig {
	lc := net.ListenConfig{Control: control}
	lc.SetMultipathTCP(false)
	return lc
}

// Listen is net.Listen for a TCP network ("tcp", "tcp4" or "tcp6"), but its
// listener is plain TCP, never MPTCP.
func Listen(ctx context.Context, network, address string) (net.Listener, error) {
	lc := Config(nil)
	return lc.Listen(ctx, network, address)
}
