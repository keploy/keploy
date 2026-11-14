//go:build windows

package routes

import (
	"errors"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const (
	tcpTableOwnerPIDConnections = 4 // TCP_TABLE_OWNER_PID_CONNECTIONS
	tcpTableFirstSize           = 64 << 10
	tcpTableHeadroom            = 64 * 56 // 64 more MIB_TCP6ROW_OWNER_PID rows
	maxTCPTableTries            = 4
)

// tcpOwner finds the process owning the TCP socket bound to self and
// connected to peer, from the system's TCP connection tables with each
// connection's owning process (GetExtendedTcpTable, as netstat -ano reads
// them). A standard user reads every process's rows: no elevation is needed.
func tcpOwner(self, peer netip.AddrPort) (int, error) {
	v4, err := tcpConnTable(windows.AF_INET, tcpTableFirstSize)
	if err != nil {
		return 0, err
	}
	v6, err := tcpConnTable(windows.AF_INET6, tcpTableFirstSize)
	if err != nil {
		return 0, err
	}
	return tcpTablesOwner(v4, v6, self, peer)
}

// tcpConnTable is family af's TCP_TABLE_OWNER_PID_CONNECTIONS table, read
// first into a buffer of first bytes (at least 1). It is read again into a
// larger buffer while the table, with the connections opened since the last
// read, does not fit.
func tcpConnTable(af, first uint32) ([]byte, error) {
	if err := procGetExtendedTCPTable.Find(); err != nil {
		return nil, fmt.Errorf("the system's TCP tables cannot be read: %w", err)
	}
	size := first
	for try := 0; try < maxTCPTableTries; try++ {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPIDConnections, 0)
		switch errno := windows.Errno(r); {
		case errno == 0:
			return buf, nil
		case errors.Is(errno, windows.ERROR_INSUFFICIENT_BUFFER):
			size += tcpTableHeadroom
		default:
			return nil, fmt.Errorf("reading the system's TCP table of address family %d: %w", af, errno)
		}
	}
	return nil, fmt.Errorf("the system's TCP table of address family %d kept growing while it was read", af)
}
