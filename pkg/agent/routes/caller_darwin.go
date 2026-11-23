//go:build darwin

package routes

import (
	"fmt"
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// tcpOwner finds the process owning the TCP socket bound to self and
// connected to peer, among the processes whose descriptors the agent may
// read (its own user's; another user's process answers EPERM and is
// skipped, as /proc hides another user's descriptors on Linux).
func tcpOwner(self, peer netip.AddrPort) (int, error) {
	pids, err := procsock.PIDs()
	if err != nil {
		return 0, fmt.Errorf("listing the processes: %w", err)
	}
	holders := map[int]bool{}
	for _, pid := range pids {
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
		p, err := procsock.Info(pid)
		return p.PPID, err == nil
	})
}
