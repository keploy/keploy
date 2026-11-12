//go:build darwin

package routes

import (
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// tcpOwner finds the process owning the TCP socket bound to self and
// connected to peer, among the processes whose descriptors the agent may
// read (its own user's; another user's process answers EPERM and is
// skipped, as /proc hides another user's descriptors on Linux).
func tcpOwner(self, peer netip.AddrPort) (int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, fmt.Errorf("listing the processes: %w", err)
	}
	parents := make(map[int]int, len(procs))
	holders := map[int]bool{}
	for i := range procs {
		pid := int(procs[i].Proc.P_pid)
		parents[pid] = int(procs[i].Eproc.Ppid)
		if pid <= 0 {
			continue
		}
		fds, err := procsock.SocketFDs(pid)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			s, ok := procsock.Socket(pid, fd)
			if ok && sameEndpoint(s.Local, self) && sameEndpoint(s.Remote, peer) {
				holders[pid] = true
				break
			}
		}
	}
	if len(holders) == 0 {
		return 0, fmt.Errorf("no process holds the socket %s -> %s", self, peer)
	}
	return soleHolder(self, peer, holders, func(pid int) (int, bool) {
		ppid, ok := parents[pid]
		return ppid, ok
	})
}
