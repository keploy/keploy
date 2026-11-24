//go:build darwin

package proxy

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/listeners"
	"go.keploy.io/server/v3/pkg/agent/procsock"
	"golang.org/x/sys/unix"
)

// listenerOwner is the process of the run that listens at ip:port, as the
// shim reported its listen (listeners.Note), and whether anything listens
// there.
//
// A reported listener is looked for in its owner's own descriptor table
// (ownerListens), never connected to. A connect is a client the listener
// accepts: a server that takes one client and stops listening, as vstest's
// runner does for the testhost it starts (dotnet test), accepted keploy's
// probe as its testhost and waited 90 s for a version answer that never
// came, and the testhost's own connection then found the port closed. Only a
// port no process of the run reported, or whose owner no longer holds a
// listening socket at it (one a child inherited and its owner closed), is
// still dialed to tell a closed port from a live one.
func listenerOwner(ip net.IP, port uint32) (int, bool) {
	pid, ok := listeners.Owner(uint16(port))
	if !ok && recorded.has(port) && !recorded.child(port) {
		return 0, true
	}
	if ok && ip != nil && ip.IsLoopback() && ownerListens(int(pid), ip, uint16(port)) {
		return int(pid), true
	}
	if ip != nil && ip.IsLoopback() && refused(ip, port) {
		// Forgotten only when its owner holds no listener at the port on
		// any address: one of the other family (a client of localhost
		// tries ::1, then 127.0.0.1) is still the run's, and forgetting it
		// would have the next call, in its family, dialed.
		if ok && !ownerHoldsPort(int(pid), uint16(port)) {
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

func listening(ip net.IP, port uint32) bool {
	_, ok := listenerOwner(ip, port)
	return ok
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

// ownerListens reports whether process pid holds a TCP socket listening at
// port on ip or on the unspecified address of ip's family, read from its descriptor table
// (procsock), so the listener sees nothing. False when the table cannot be
// read.
func ownerListens(pid int, ip net.IP, port uint16) bool {
	want, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	want = want.Unmap()
	fds, err := procsock.SocketFDs(pid)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		s, ok := procsock.Socket(pid, fd)
		if !ok || s.State != xnuTCPListen || s.Local.Port() != port {
			continue
		}
		// An unspecified address counts for its own family, and a dual-stack
		// [::] (procsock reads it as 0.0.0.0) for both: an IPv4-only 0.0.0.0
		// does not take ::1, nor an IPV6_V6ONLY [::] 127.0.0.1.
		if a := s.Local.Addr().Unmap(); a == want || (a.IsUnspecified() && (a.Is4() == want.Is4() || s.DualStack)) {
			return true
		}
	}
	return false
}

// ownerHoldsPort reports whether process pid holds a TCP socket listening at
// port on any address (procsock). False when its table cannot be read.
func ownerHoldsPort(pid int, port uint16) bool {
	fds, err := procsock.SocketFDs(pid)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if s, ok := procsock.Socket(pid, fd); ok && s.State == xnuTCPListen && s.Local.Port() == port {
			return true
		}
	}
	return false
}

// refused dials ip:port and reports whether the connect was refused. The
// listener, if any, accepts the dial as a client: listenerOwner asks it only
// of a port it cannot read passively.
func refused(ip net.IP, port uint32) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), 200*time.Millisecond)
	if err != nil {
		return errors.Is(err, unix.ECONNREFUSED)
	}
	_ = conn.Close()
	return false
}

// callerOf is the process that made a redirected connection, by the number
// the agent's process table names it by: macOS has no PID namespaces, so the
// kernel's number (KernelPid) is that number. unnamed is never true.
func callerOf(dest *agent.NetworkAddress) (caller int, unnamed bool) {
	return int(dest.KernelPid), false
}
