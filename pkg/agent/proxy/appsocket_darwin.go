//go:build darwin

package proxy

import (
	"fmt"
	"net/netip"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// An application socket macOS's TCP table does not list to the agent
// (xnuHidden) is read from the descriptor table of the process that holds it
// (procsock), which a process of the same user may read: its TCP state and
// what its send buffer holds, written and not yet acknowledged. That read
// gives no sequence numbers, so what the application had written is known
// only as at most what the proxy's side had received by the read after it
// plus what that send buffer held (the bytes the proxy's side received but
// had not yet acknowledged are in both), and exactly once a read finds the
// send buffer empty: everything written by then was acknowledged, so the
// proxy's side received it (heldTarget, heldSettled, noteHeldAtClose).

// sockOwner is a process and its descriptor of the application's end of a
// paired connection.
type sockOwner struct {
	pid int
	fd  int32
}

// appHolder is, on darwin, the process found holding the application's end of
// a paired connection the table does not list (pairedConn.appSocket): nil
// until a read of the processes found it, and a sockOwner with pid 0 once one
// found none, closed by every process of keploy's user.
type appHolder struct{ holder atomic.Pointer[sockOwner] }

// heldSockets reads the socket of the application's end of a paired
// connection from the processes' descriptor tables, once for every connection
// a call asks about: the table of every TCP socket of every process the agent
// may read (another user's answers EPERM and is skipped), built at the first
// connection that needs it.
type heldSockets struct {
	by  map[[2]netip.AddrPort]heldSocket
	err error
}

type heldSocket struct {
	owner sockOwner
	sock  procsock.TCP
}

func (h *heldSockets) find(local, remote netip.AddrPort) (heldSocket, bool, error) {
	if h.by == nil && h.err == nil {
		h.by, h.err = heldSocketsRead()
	}
	if h.err != nil {
		return heldSocket{}, false, h.err
	}
	s, ok := h.by[[2]netip.AddrPort{unmapped(local), unmapped(remote)}]
	return s, ok, nil
}

// unmapped is a with an IPv4-mapped IPv6 address as its IPv4 one (sameEnd).
func unmapped(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}

// heldSocketsRead is readHeldSockets; rows hide sockets from it.
var heldSocketsRead = readHeldSockets

// heldSocketRead is procsock.Socket, the read of a holder found before
// (appSocket); rows change what it shows.
var heldSocketRead = procsock.Socket

func readHeldSockets() (map[[2]netip.AddrPort]heldSocket, error) {
	pids, err := procsock.PIDs()
	if err != nil {
		return nil, fmt.Errorf("list the processes whose descriptor tables hold the application's sockets: %v", err)
	}
	by := map[[2]netip.AddrPort]heldSocket{}
	for _, pid := range pids {
		fds, err := procsock.SocketFDs(pid)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if s, ok := procsock.Socket(pid, fd); ok {
				k := [2]netip.AddrPort{unmapped(s.Local), unmapped(s.Remote)}
				if _, seen := by[k]; !seen {
					by[k] = heldSocket{owner: sockOwner{pid: pid, fd: fd}, sock: s}
				}
			}
		}
	}
	return by, nil
}

// appSocket reads pc's application socket from the descriptor table of the
// process holding it: from the holder found before, while it still holds it,
// else from h's read of every process (a copy a child inherited is found
// there once its parent's is closed). found is false once no process the
// agent may read holds it, which no later read changes. That proves nothing
// on its way: a socket every holder closed lives on in the kernel until its
// bytes and its FIN are acknowledged, so it is gone only once the proxy's
// side has taken that FIN or a reset (noteWroteAtClose, pairEnded).
func (pc *pairedConn) appSocket(h *heldSockets) (procsock.TCP, bool, error) {
	if o := pc.holder.Load(); o != nil {
		if o.pid == 0 {
			return procsock.TCP{}, false, nil
		}
		if s, ok := heldSocketRead(o.pid, o.fd); ok && sameEnd(s.Local, pc.app) && sameEnd(s.Remote, pc.proxy) {
			return s, true, nil
		}
		// Closed by its holder since: read every process now, not the
		// table h may have read before.
		h = &heldSockets{}
	}
	s, ok, err := h.find(pc.app, pc.proxy)
	if err != nil {
		return procsock.TCP{}, false, err
	}
	if !ok {
		pc.holder.Store(&sockOwner{})
		return procsock.TCP{}, false, nil
	}
	pc.holder.Store(&s.owner)
	return s.sock, true, nil
}

// heldFINPending: the holder closed its socket's sending side and its FIN,
// after everything it wrote, is not yet acknowledged (xnuPCB.finPending).
func heldFINPending(s procsock.TCP) bool {
	return s.State == xnuTCPFinWait1 || s.State == xnuTCPClosing || s.State == xnuTCPLastAck
}
