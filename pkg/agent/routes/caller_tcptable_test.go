package routes

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

// tcpRow is one row of a Windows TCP table, as the test lays it out.
type tcpRow struct {
	state       uint32
	local, peer netip.AddrPort
	owner       uint32
}

// winPort is a port as Windows keeps one: the low 16 bits of a DWORD, in
// network order, so its first byte is the port's high byte.
func winPort(b []byte, port uint16) {
	b[0], b[1] = byte(port>>8), byte(port)
}

// v4Table lays rows out as MIB_TCPTABLE_OWNER_PID, spelled out field by field
// from tcpmib.h rather than from the reader's layout.
func v4Table(rows ...tcpRow) []byte {
	b := make([]byte, 4+24*len(rows))
	binary.LittleEndian.PutUint32(b, uint32(len(rows)))
	for i, r := range rows {
		row := b[4+24*i:]
		binary.LittleEndian.PutUint32(row[0:], r.state)
		la, ra := r.local.Addr().As4(), r.peer.Addr().As4()
		copy(row[4:8], la[:])
		winPort(row[8:], r.local.Port())
		copy(row[12:16], ra[:])
		winPort(row[16:], r.peer.Port())
		binary.LittleEndian.PutUint32(row[20:], r.owner)
	}
	return b
}

// v6Table lays rows out as MIB_TCP6TABLE_OWNER_PID, with scope IDs that are
// not zero, so a reader that took a scope for an address or a port fails.
func v6Table(rows ...tcpRow) []byte {
	b := make([]byte, 4+56*len(rows))
	binary.LittleEndian.PutUint32(b, uint32(len(rows)))
	for i, r := range rows {
		row := b[4+56*i:]
		la, ra := r.local.Addr().As16(), r.peer.Addr().As16()
		copy(row[0:16], la[:])
		binary.LittleEndian.PutUint32(row[16:], 0xdddd)
		winPort(row[20:], r.local.Port())
		copy(row[24:40], ra[:])
		binary.LittleEndian.PutUint32(row[40:], 0xeeee)
		winPort(row[44:], r.peer.Port())
		binary.LittleEndian.PutUint32(row[48:], r.state)
		binary.LittleEndian.PutUint32(row[52:], r.owner)
	}
	return b
}

const estab = 5

var (
	winClient = netip.MustParseAddrPort("127.0.0.1:50123")
	winAgent  = netip.MustParseAddrPort("127.0.0.1:16789")
)

// The caller is the owner of the row whose local end is the request's remote
// address and whose remote end is the agent: not the agent's own row of the
// same connection, nor a row that matches one end only.
func TestTheWindowsTCPTableNamesTheClientEndsOwner(t *testing.T) {
	v4 := v4Table(
		tcpRow{estab, winAgent, winClient, 4000}, // the agent's end
		tcpRow{estab, winClient, netip.MustParseAddrPort("127.0.0.1:16790"), 4001},
		tcpRow{estab, netip.MustParseAddrPort("127.0.0.1:50124"), winAgent, 4002},
		tcpRow{estab, winClient, winAgent, 4321},
	)
	pid, err := tcpTablesOwner(v4, v6Table(), winClient, winAgent)
	if err != nil || pid != 4321 {
		t.Fatalf("tcpTablesOwner = %d, %v; want 4321", pid, err)
	}
}

// Ports are read in network order: a port whose two bytes differ is found,
// and one whose bytes are swapped is another port.
func TestTheWindowsTCPTableReadsPortsInNetworkOrder(t *testing.T) {
	self := netip.MustParseAddrPort("127.0.0.1:258") // 0x0102
	peer := netip.MustParseAddrPort("127.0.0.1:772") // 0x0304
	table := v4Table(tcpRow{estab, self, peer, 77})
	if pid, err := tcpTablesOwner(table, v6Table(), self, peer); err != nil || pid != 77 {
		t.Fatalf("tcpTablesOwner = %d, %v; want 77", pid, err)
	}
	swapped := netip.MustParseAddrPort("127.0.0.1:513") // 0x0201
	if pid, err := tcpTablesOwner(table, v6Table(), swapped, peer); err == nil {
		t.Fatalf("the local port read byte-swapped matched: process %d", pid)
	}
	if pid, err := tcpTablesOwner(table, v6Table(), self, netip.MustParseAddrPort("127.0.0.1:1027")); err == nil {
		t.Fatalf("the remote port read byte-swapped matched: process %d", pid)
	}
}

// Only an established connection's row names a caller, and a row owned by
// process 0 names none.
func TestTheWindowsTCPTableSkipsRowsThatAreNoCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  tcpRow
	}{
		{"closing", tcpRow{8, winClient, winAgent, 4321}}, // MIB_TCP_STATE_CLOSE_WAIT
		{"owned by no process", tcpRow{estab, winClient, winAgent, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := tcpTablesOwner(v4Table(tc.row), v6Table(), winClient, winAgent)
			if err == nil || !strings.Contains(err.Error(), "no process holds") {
				t.Fatalf("tcpTablesOwner = %d, %v; want no process", pid, err)
			}
		})
	}
}

// The AF_INET6 table is read at its own layout: an IPv6 connection, and an
// IPv4 one a dual-stack socket keeps as IPv4-mapped, are both found there.
func TestTheWindowsTCP6TableNamesTheClientEndsOwner(t *testing.T) {
	c6 := netip.MustParseAddrPort("[::1]:50200")
	a6 := netip.MustParseAddrPort("[::1]:16789")
	mapped := netip.AddrPortFrom(netip.AddrFrom16(winClient.Addr().As16()), winClient.Port())
	v6 := v6Table(
		tcpRow{estab, a6, c6, 4000},
		tcpRow{estab, c6, a6, 600},
		tcpRow{estab, mapped, netip.AddrPortFrom(netip.AddrFrom16(winAgent.Addr().As16()), winAgent.Port()), 700},
	)
	if pid, err := tcpTablesOwner(v4Table(), v6, c6, a6); err != nil || pid != 600 {
		t.Fatalf("IPv6: tcpTablesOwner = %d, %v; want 600", pid, err)
	}
	if pid, err := tcpTablesOwner(v4Table(), v6, winClient, winAgent); err != nil || pid != 700 {
		t.Fatalf("IPv4-mapped: tcpTablesOwner = %d, %v; want 700", pid, err)
	}
}

// Rows of one connection that name two processes name no one caller, and
// the error names both.
func TestTheWindowsTCPTablesNameNoOneForTwoOwners(t *testing.T) {
	v4 := v4Table(tcpRow{estab, winClient, winAgent, 900})
	mapped := netip.AddrPortFrom(netip.AddrFrom16(winClient.Addr().As16()), winClient.Port())
	v6 := v6Table(tcpRow{estab, mapped, netip.AddrPortFrom(netip.AddrFrom16(winAgent.Addr().As16()), winAgent.Port()), 800})
	pid, err := tcpTablesOwner(v4, v6, winClient, winAgent)
	if err == nil || !strings.Contains(err.Error(), "[800 900]") {
		t.Fatalf("tcpTablesOwner = %d, %v; want an error naming 800 and 900", pid, err)
	}
	// The same owner in both tables is one caller.
	v6 = v6Table(tcpRow{estab, mapped, netip.AddrPortFrom(netip.AddrFrom16(winAgent.Addr().As16()), winAgent.Port()), 900})
	if pid, err := tcpTablesOwner(v4, v6, winClient, winAgent); err != nil || pid != 900 {
		t.Fatalf("tcpTablesOwner = %d, %v; want 900", pid, err)
	}
}

// A table shorter than its count says is refused, not read past its end, and
// one too short for a count is refused too.
func TestTheWindowsTCPTableRefusesATruncatedTable(t *testing.T) {
	full := v4Table(tcpRow{estab, winClient, winAgent, 4321})
	if _, err := tcpTablesOwner(full[:len(full)-1], v6Table(), winClient, winAgent); err == nil || !strings.Contains(err.Error(), "says it has 1 rows") {
		t.Fatalf("a truncated AF_INET table: %v", err)
	}
	v6 := v6Table(tcpRow{estab, winClient, winAgent, 4321})
	if _, err := tcpTablesOwner(v4Table(), v6[:len(v6)-1], winClient, winAgent); err == nil || !strings.Contains(err.Error(), "of 56 bytes") {
		t.Fatalf("a truncated AF_INET6 table: %v", err)
	}
	if _, err := tcpTablesOwner(full[:3], v6Table(), winClient, winAgent); err == nil || !strings.Contains(err.Error(), "too short for its row count") {
		t.Fatalf("a table with no count: %v", err)
	}
	// A table exactly as long as its rows is read whole.
	if pid, err := tcpTablesOwner(full, v6Table(), winClient, winAgent); err != nil || pid != 4321 {
		t.Fatalf("the whole table: %d, %v", pid, err)
	}
}
