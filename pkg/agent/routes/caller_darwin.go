//go:build darwin

package routes

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// callerPID is the process that owns the client end of the request's TCP
// connection: the socket whose local address is the request's remote address
// and whose remote address is the server's own. It is read from the kernel's
// per-process socket tables (procsock), never from what the caller
// says, so a caller that does not carry keploy's shim, such as a hardened
// binary, is identified like any other.
func callerPID(r *http.Request) (int, error) {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return 0, fmt.Errorf("the connection's remote address %q: %w", r.RemoteAddr, err)
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return 0, errors.New("the connection's local address is unknown")
	}
	local, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return 0, fmt.Errorf("the connection's local address %q: %w", la.String(), err)
	}
	return tcpOwner(remote, local)
}

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
