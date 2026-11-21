//go:build darwin

package procsock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TCP is one TCP socket of a process, as its descriptor table shows it.
type TCP struct {
	// FD is the process's descriptor of it.
	FD int32
	// Local and Remote are its ends.
	Local, Remote netip.AddrPort
	// State is its TCP state, in XNU's numbering (TCPS_CLOSED is 0,
	// TCPS_ESTABLISHED 4).
	State uint8
	// SndCC is what its send buffer holds: written by the process and not
	// yet acknowledged by its peer, sent or not. It does not count a FIN.
	SndCC uint32
}

// The proc_info system call and the layouts it fills, from <sys/proc_info.h>
// on arm64 and x86_64 alike (all fields are fixed-width).
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
	offSndCC             = 232 // psi (24) + socket_info.soi_snd (208): sockbuf_info.sbi_cc
	offSoiKind           = 256 // psi (24) + socket_info.soi_kind (232)
	offInsi              = 264 // psi (24) + socket_info.soi_proto (240): struct in_sockinfo
	offInsiFport         = offInsi + 0
	offInsiLport         = offInsi + 4
	offInsiVflag         = offInsi + 24
	offInsiFaddr         = offInsi + 32 // 16 bytes: in6_addr, or IPv4 in the last 4
	offInsiLaddr         = offInsi + 48
	offTCPState          = offInsi + 80 // tcp_sockinfo.tcpsi_state, after its in_sockinfo (80)
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

// SocketFDs lists pid's socket descriptors. Another user's process answers
// EPERM.
func SocketFDs(pid int) ([]int32, error) {
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

// Socket is pid's descriptor fd when it is a TCP socket.
func Socket(pid int, fd int32) (TCP, bool) {
	buf := make([]byte, socketFDInfoSize)
	n, err := procInfo(procInfoCallFDInfo, pid, procPIDFDSocketInfo, uint64(fd), buf)
	if err != nil || n < socketFDInfoSize {
		return TCP{}, false
	}
	s, ok := parseTCP(buf)
	s.FD = fd
	return s, ok
}

// parseTCP reads a struct socket_fdinfo's TCP socket. The ports are in network
// byte order, as the protocol control block keeps them.
func parseTCP(b []byte) (TCP, bool) {
	if len(b) < socketFDInfoSize || int32(binary.LittleEndian.Uint32(b[offSoiKind:])) != sockinfoTCP {
		return TCP{}, false
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
		return TCP{}, false
	}
	return TCP{
		Local:  netip.AddrPortFrom(la, binary.BigEndian.Uint16(b[offInsiLport:])),
		Remote: netip.AddrPortFrom(ra, binary.BigEndian.Uint16(b[offInsiFport:])),
		State:  uint8(binary.LittleEndian.Uint32(b[offTCPState:])),
		SndCC:  binary.LittleEndian.Uint32(b[offSndCC:]),
	}, true
}

// Proc is one process as proc_info's short BSD record shows it: its parent,
// its process group and whether it has exited and waits to be reaped.
type Proc struct {
	PID, PPID, PGID int
	// Zombie is a process that has exited and waits for its parent to
	// reap it (XNU's SZOMB).
	Zombie bool
}

// The processes are listed and read through proc_info too, never through
// the kern.proc sysctls that list them (kern.proc.all, .pgrp, .uid and the
// others ps(1) reads). Those wait, uninterruptibly, for every process they
// list that is in the middle of an exec or a posix_spawn, whoever owns it: a
// spawn held in its file actions, such as an open of a FIFO no one reads,
// holds every such listing on the machine, ps(1)'s included, for as long as
// it is held, and keploy's agent stopped answering with it. proc_info's list
// and short record wait on no other process.
const (
	procInfoCallListPIDs = 1  // PROC_INFO_CALL_LISTPIDS
	procAllPIDs          = 1  // PROC_ALL_PIDS
	procPGRPOnly         = 2  // PROC_PGRP_ONLY
	procPIDTShortBSDInfo = 13 // PROC_PIDT_SHORTBSDINFO: struct proc_bsdshortinfo
	shortBSDInfoSize     = 64 // sizeof(struct proc_bsdshortinfo)
	offShortPPID         = 4  // pbsi_ppid
	offShortPGID         = 8  // pbsi_pgid
	offShortStatus       = 12 // pbsi_status
	sZomb                = 5  // SZOMB, bsd/sys/proc.h
	pidSize              = 4  // a pid_t in the list
	maxListPIDsRetries   = 4
	listPIDsHeadroom     = 64
)

// PIDs lists every process (libproc's proc_listallpids).
func PIDs() ([]int, error) { return listPIDs(procAllPIDs, 0) }

// GroupPIDs lists the processes of process group pgid, a zombie's included
// (libproc's proc_listpids with PROC_PGRP_ONLY).
func GroupPIDs(pgid int) ([]int, error) { return listPIDs(procPGRPOnly, pgid) }

func listPIDs(kind, arg int) ([]int, error) {
	size, err := procInfo(procInfoCallListPIDs, kind, arg, 0, nil)
	if err != nil {
		return nil, err
	}
	for try := 0; try < maxListPIDsRetries; try++ {
		buf := make([]byte, size+listPIDsHeadroom*pidSize)
		n, err := procInfo(procInfoCallListPIDs, kind, arg, 0, buf)
		if err != nil {
			return nil, err
		}
		if n >= len(buf) {
			// The table may have grown past the buffer: list it again.
			size = n * 2
			continue
		}
		var pids []int
		for off := 0; off+pidSize <= n; off += pidSize {
			if pid := int(int32(binary.LittleEndian.Uint32(buf[off:]))); pid > 0 {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	return nil, errors.New("the process table kept growing while it was listed")
}

// Info reads process pid's short BSD record, which any user may read. A
// process that has gone, or is exiting, answers ESRCH.
func Info(pid int) (Proc, error) {
	buf := make([]byte, shortBSDInfoSize)
	n, err := procInfo(procInfoCallPIDInfo, pid, procPIDTShortBSDInfo, 0, buf)
	if err != nil {
		return Proc{}, err
	}
	if n < shortBSDInfoSize {
		return Proc{}, fmt.Errorf("process %d's record is %d bytes, not %d", pid, n, shortBSDInfoSize)
	}
	return Proc{
		PID:    pid,
		PPID:   int(binary.LittleEndian.Uint32(buf[offShortPPID:])),
		PGID:   int(binary.LittleEndian.Uint32(buf[offShortPGID:])),
		Zombie: binary.LittleEndian.Uint32(buf[offShortStatus:]) == sZomb,
	}, nil
}
