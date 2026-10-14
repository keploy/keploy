//go:build !linux

package proxy

import (
	"fmt"
	"syscall"

	"go.keploy.io/server/v3/pkg/models"
)

// listenerDrops cannot be read here: only Linux counts a listener's drops
// where a process can read them.
func listenerDrops(syscall.RawConn) (uint64, error) {
	return 0, fmt.Errorf("%w: this platform cannot read a listener's drops", models.ErrProxyConnsUnsupported)
}
