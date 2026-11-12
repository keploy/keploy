//go:build !linux && !darwin

package proxy

import (
	"fmt"
	"net"
	"syscall"

	"go.keploy.io/server/v3/pkg/models"
)

// listenerDrops cannot be read here: only Linux counts a listener's drops
// where a process can read them.
func listenerDrops(syscall.RawConn) (uint64, error) {
	return 0, fmt.Errorf("%w: this platform cannot read a listener's drops", models.ErrProxyConnsUnsupported)
}

// netListenDrops cannot be read here either.
func netListenDrops(syscall.RawConn) (uint64, error) {
	return 0, fmt.Errorf("this platform cannot read a network namespace's listen drops")
}

// noFastOpen has nothing to do here: the kernel count is Linux's.
func noFastOpen(net.Listener) error { return nil }
