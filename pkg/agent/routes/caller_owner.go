//go:build linux || darwin

package routes

import (
	"fmt"
	"net/netip"
	"sort"
)

// soleHolder picks the caller among the processes holding the client end of
// a connection (self -> peer). A child the caller is starting (between its
// fork and its exec) holds a copy of the caller's socket for a moment: the
// holder every other one descends from is the caller. A socket processes
// share otherwise (passed, or inherited by a child that kept it) names no
// one caller. parent reads a process's parent from the kernel.
func soleHolder(self, peer netip.AddrPort, holders map[int]bool, parent func(pid int) (int, bool)) (int, error) {
	for pid := range holders {
		all := true
		for other := range holders {
			if other != pid && !descendsFrom(other, pid, parent) {
				all = false
				break
			}
		}
		if all {
			return pid, nil
		}
	}
	pids := make([]int, 0, len(holders))
	for pid := range holders {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return 0, fmt.Errorf("the socket %s -> %s is held by processes %v", self, peer, pids)
}

// descendsFrom reports whether pid is a descendant of ancestor, following
// parent.
func descendsFrom(pid, ancestor int, parent func(pid int) (int, bool)) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		ppid, ok := parent(pid)
		if !ok {
			return false
		}
		if ppid == ancestor {
			return true
		}
		pid = ppid
	}
	return false
}

// sameEndpoint compares two endpoints with IPv4-mapped IPv6 read as IPv4.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}
