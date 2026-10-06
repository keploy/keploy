//go:build darwin

package proxy

import (
	"errors"
	"net"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/agent/listeners"
	"golang.org/x/sys/unix"
)

func listenerOwner(ip net.IP, port uint32) (int, bool) {
	pid, ok := listeners.Owner(uint16(port))
	if !ok && recorded.has(port) && !recorded.child(port) {
		return 0, true
	}
	if ip != nil && ip.IsLoopback() && refused(ip, port) {
		if ok {
			listeners.Forget(uint16(port))
		}
		return 0, false
	}
	if !ok {
		return 0, true
	}
	if err := unix.Kill(int(pid), 0); err != nil && err != unix.EPERM {
		listeners.Forget(uint16(port))
		return 0, true
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

func refused(ip net.IP, port uint32) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), 200*time.Millisecond)
	if err != nil {
		return errors.Is(err, unix.ECONNREFUSED)
	}
	_ = conn.Close()
	return false
}
