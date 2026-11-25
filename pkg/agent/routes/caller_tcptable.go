package routes

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
)

// The tables Windows's GetExtendedTcpTable fills for
// TCP_TABLE_OWNER_PID_CONNECTIONS (tcpmib.h): a DWORD row count, then that
// many MIB_TCPROW_OWNER_PID rows (AF_INET) or MIB_TCP6ROW_OWNER_PID rows
// (AF_INET6). Each DWORD is in the machine's little-endian order, except
// that an address is its bytes in network order and a port is the low 16
// bits of its DWORD in network order. They are read here, in no build of its
// own, so that their reading is tested on every system.
//
// heldTCPState is the states (MIB_TCP_STATE) in which a process can still
// hold a connected socket: ESTAB, FIN_WAIT1, FIN_WAIT2, CLOSE_WAIT, CLOSING
// and LAST_ACK, as Linux's read takes them. SYN_SENT has sent no request yet,
// TIME_WAIT and DELETE_TCB are no process's, CLOSED is a connection reset or
// never made, and LISTEN and SYN_RCVD are a server's.
var heldTCPState = map[uint32]bool{
	5:  true, // MIB_TCP_STATE_ESTAB
	6:  true, // MIB_TCP_STATE_FIN_WAIT1
	7:  true, // MIB_TCP_STATE_FIN_WAIT2
	8:  true, // MIB_TCP_STATE_CLOSE_WAIT
	9:  true, // MIB_TCP_STATE_CLOSING
	10: true, // MIB_TCP_STATE_LAST_ACK
}

// tcpRowLayout is where a table row keeps each field, in bytes from the
// row's start.
type tcpRowLayout struct {
	size, addrLen                            int
	state, laddr, lport, raddr, rport, owner int
}

var (
	// MIB_TCPROW_OWNER_PID: dwState, dwLocalAddr, dwLocalPort,
	// dwRemoteAddr, dwRemotePort, dwOwningPid.
	tcp4Rows = tcpRowLayout{size: 24, addrLen: 4, state: 0, laddr: 4, lport: 8, raddr: 12, rport: 16, owner: 20}
	// MIB_TCP6ROW_OWNER_PID: ucLocalAddr[16], dwLocalScopeId, dwLocalPort,
	// ucRemoteAddr[16], dwRemoteScopeId, dwRemotePort, dwState, dwOwningPid.
	tcp6Rows = tcpRowLayout{size: 56, addrLen: 16, laddr: 0, lport: 20, raddr: 24, rport: 44, state: 48, owner: 52}
)

// tcpTableOwners is the owning process of each established row of table,
// laid out as rows, whose local end is self and remote end is peer. A row
// owned by process 0 (a connection the system holds, as in TIME_WAIT) names
// no process and is skipped, as Linux skips a socket with no inode.
func tcpTableOwners(table []byte, rows tcpRowLayout, self, peer netip.AddrPort) ([]int, error) {
	if len(table) < 4 {
		return nil, fmt.Errorf("the TCP table is %d bytes, too short for its row count", len(table))
	}
	n := uint64(binary.LittleEndian.Uint32(table))
	if n*uint64(rows.size) > uint64(len(table)-4) {
		return nil, fmt.Errorf("the TCP table says it has %d rows of %d bytes, but it has %d bytes after its count", n, rows.size, len(table)-4)
	}
	var owners []int
	for i := 0; i < int(n); i++ {
		row := table[4+i*rows.size : 4+(i+1)*rows.size]
		owner := binary.LittleEndian.Uint32(row[rows.owner:])
		// A caller's socket is one a process can still hold: established,
		// or closed by one side or both while the caller keeps it (a caller
		// that called CloseWrite after its request). Owner 0 is no process.
		if !heldTCPState[binary.LittleEndian.Uint32(row[rows.state:])] || owner == 0 {
			continue
		}
		la, _ := netip.AddrFromSlice(row[rows.laddr : rows.laddr+rows.addrLen])
		ra, _ := netip.AddrFromSlice(row[rows.raddr : rows.raddr+rows.addrLen])
		local := netip.AddrPortFrom(la, binary.BigEndian.Uint16(row[rows.lport:]))
		remote := netip.AddrPortFrom(ra, binary.BigEndian.Uint16(row[rows.rport:]))
		if sameEndpoint(local, self) && sameEndpoint(remote, peer) {
			owners = append(owners, int(owner))
		}
	}
	return owners, nil
}

// tcpTablesOwner is the one process owning the client end self -> peer in
// the AF_INET table v4 and the AF_INET6 table v6. Windows charges a socket to
// the process that created it, also once a child inherits its handle, so
// rows never name two processes for one connection; if they did, no one
// caller is named. (A child that inherited the caller's socket and sent on it
// is read as the caller: Go creates its sockets uninheritable, and a claim
// still has to come from a process of the run.)
func tcpTablesOwner(v4, v6 []byte, self, peer netip.AddrPort) (int, error) {
	owners := map[int]bool{}
	for _, t := range []struct {
		table []byte
		rows  tcpRowLayout
	}{{v4, tcp4Rows}, {v6, tcp6Rows}} {
		pids, err := tcpTableOwners(t.table, t.rows, self, peer)
		if err != nil {
			return 0, err
		}
		for _, pid := range pids {
			owners[pid] = true
		}
	}
	pids := make([]int, 0, len(owners))
	for pid := range owners {
		pids = append(pids, pid)
	}
	switch len(pids) {
	case 0:
		return 0, fmt.Errorf("no process holds the socket %s -> %s", self, peer)
	case 1:
		return pids[0], nil
	}
	sort.Ints(pids)
	return 0, fmt.Errorf("the socket %s -> %s is held by processes %v", self, peer, pids)
}
