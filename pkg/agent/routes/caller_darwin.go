//go:build darwin

package routes

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// callerPID is the process that owns the client end of the request's TCP
// connection: the socket whose local address is the request's remote address
// and whose remote address is the server's own. It is read from the kernel's
// per-process socket tables (libproc's proc_info), never from what the caller
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
		fds, err := socketFDs(pid)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			l, p, ok := tcpEnds(pid, fd)
			if ok && sameEndpoint(l, self) && sameEndpoint(p, peer) {
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

// The proc_info system call (what libproc's proc_pidinfo and proc_pidfdinfo
// wrap) and the layouts it fills, from <sys/proc_info.h> on arm64 and x86_64
// alike (all fields are fixed-width).
const (
	sysProcInfo          = 336 // SYS_proc_info
	procInfoCallPIDInfo  = 2   // PROC_INFO_CALL_PIDINFO
	procInfoCallFDInfo   = 3   // PROC_INFO_CALL_PIDFDINFO
	procPIDListFDs       = 1   // PROC_PIDLISTFDS: []struct proc_fdinfo
	procPIDFDSocketInfo  = 3   // PROC_PIDFDSOCKETINFO: struct socket_fdinfo
	proxFDTypeSocket     = 2   // PROX_FDTYPE_SOCKET
	sockinfoTCP          = 2   // SOCKINFO_TCP
	iniIPv4              = 1   // INI_IPV4
	iniIPv6              = 2   // INI_IPV6
	procFDInfoSize       = 8   // sizeof(struct proc_fdinfo): int32 fd, uint32 type
	socketFDInfoSize     = 792 // sizeof(struct socket_fdinfo)
	offSoiKind           = 256 // psi (24) + socket_info.soi_kind (232)
	offInsi              = 264 // psi (24) + socket_info.soi_proto (240): struct in_sockinfo
	offInsiFport         = offInsi + 0
	offInsiLport         = offInsi + 4
	offInsiVflag         = offInsi + 24
	offInsiFaddr         = offInsi + 32 // 16 bytes: in6_addr, or IPv4 in the last 4
	offInsiLaddr         = offInsi + 48
	maxListFDsRetries    = 4
	listFDsHeadroomSlots = 32
)

func procInfo(call, pid, flavor int, arg uint64, buf []byte) (int, error) {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	n, _, errno := unix.Syscall6(sysProcInfo, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// socketFDs lists pid's socket descriptors.
func socketFDs(pid int) ([]int32, error) {
	size, err := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, nil)
	if err != nil {
		return nil, err
	}
	for try := 0; try < maxListFDsRetries; try++ {
		buf := make([]byte, size+listFDsHeadroomSlots*procFDInfoSize)
		n, err := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, buf)
		if err != nil {
			return nil, err
		}
		if n >= len(buf) {
			// The table may have grown past the buffer: list it again.
			size = n * 2
			continue
		}
		var fds []int32
		for off := 0; off+procFDInfoSize <= n; off += procFDInfoSize {
			if binary.LittleEndian.Uint32(buf[off+4:]) == proxFDTypeSocket {
				fds = append(fds, int32(binary.LittleEndian.Uint32(buf[off:])))
			}
		}
		return fds, nil
	}
	return nil, fmt.Errorf("process %d's descriptor table kept growing while it was listed", pid)
}

// tcpEnds is the local and remote endpoint of pid's descriptor fd when it is
// a TCP socket.
func tcpEnds(pid int, fd int32) (local, remote netip.AddrPort, ok bool) {
	buf := make([]byte, socketFDInfoSize)
	n, err := procInfo(procInfoCallFDInfo, pid, procPIDFDSocketInfo, uint64(fd), buf)
	if err != nil || n < socketFDInfoSize {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	return parseTCPSocketInfo(buf)
}

// parseTCPSocketInfo reads a struct socket_fdinfo's TCP endpoints. The ports
// are in network byte order, as the protocol control block keeps them.
func parseTCPSocketInfo(b []byte) (local, remote netip.AddrPort, ok bool) {
	if len(b) < socketFDInfoSize || int32(binary.LittleEndian.Uint32(b[offSoiKind:])) != sockinfoTCP {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	addr := func(off int) (netip.Addr, bool) {
		switch v := b[offInsiVflag]; {
		case v&iniIPv4 != 0:
			return netip.AddrFrom4([4]byte(b[off+12 : off+16])), true
		case v&iniIPv6 != 0:
			return netip.AddrFrom16([16]byte(b[off : off+16])), true
		}
		return netip.Addr{}, false
	}
	la, okL := addr(offInsiLaddr)
	ra, okR := addr(offInsiFaddr)
	if !okL || !okR {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	lport := binary.BigEndian.Uint16(b[offInsiLport:])
	rport := binary.BigEndian.Uint16(b[offInsiFport:])
	return netip.AddrPortFrom(la, lport), netip.AddrPortFrom(ra, rport), true
}
