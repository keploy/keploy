//go:build darwin

package proxy

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent/listeners"
	"golang.org/x/sys/unix"
)

func listenerOwner(_ net.IP, port uint32) (int, bool) {
	pid, ok := listeners.Owner(uint16(port))
	if !ok {
		return 0, true
	}
	if err := unix.Kill(int(pid), 0); err != nil && err != unix.EPERM {
		return 0, false
	}
	return int(pid), true
}

func descends(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || kp == nil {
			return false
		}
		pid = int(kp.Eproc.Ppid)
	}
	return pid == ancestor
}
