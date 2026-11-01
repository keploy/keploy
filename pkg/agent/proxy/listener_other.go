//go:build !linux && !darwin

package proxy

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent"
)

func listenerOwner(net.IP, uint32) (int, bool) { return 0, true }

func listening(net.IP, uint32) bool { return true }

func descends(int, int) bool { return false }

// callerOf is the kernel's number for the process that made a redirected
// connection: no call descends from it here (descends), so none is live.
func callerOf(dest *agent.NetworkAddress) (caller int, unnamed bool) {
	return int(dest.KernelPid), false
}
