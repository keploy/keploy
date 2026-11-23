//go:build windows

package routes

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// loopbackConn opens a connection to a listener of this process over
// network (tcp4 or tcp6) and returns its client end's addresses; both ends
// and the listener close when t ends.
func loopbackConn(t *testing.T, network string) (self, peer netip.AddrPort) {
	t.Helper()
	host := "127.0.0.1"
	if network == "tcp6" {
		host = "[::1]"
	}
	ln, err := net.Listen(network, host+":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := net.Dial(network, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return netip.MustParseAddrPort(c.LocalAddr().String()), netip.MustParseAddrPort(c.RemoteAddr().String())
}

// The client end of a connection this process opened is read as this
// process's, over IPv4 and IPv6, from the system's own tables: their layout
// and the owning pid are what GetExtendedTcpTable fills, not a test's.
func TestTCPOwnerOnWindowsFindsTheClientEnd(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			self, peer := loopbackConn(t, network)
			pid, err := tcpOwner(self, peer)
			if err != nil || pid != os.Getpid() {
				t.Fatalf("tcpOwner(%s -> %s) = %d, %v; want this process %d", self, peer, pid, err, os.Getpid())
			}
		})
	}
}

// A table that does not fit the first buffer is read again into one it
// fits, whole: with a first buffer of one byte, too small for any table,
// the client end of a connection is still found, over IPv4 and IPv6.
func TestTCPOwnerOnWindowsReadsATableLargerThanItsFirstBuffer(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			self, peer := loopbackConn(t, network)
			v4, err := tcpConnTable(windows.AF_INET, 1)
			if err != nil {
				t.Fatalf("the IPv4 table from a 1-byte first buffer: %v", err)
			}
			v6, err := tcpConnTable(windows.AF_INET6, 1)
			if err != nil {
				t.Fatalf("the IPv6 table from a 1-byte first buffer: %v", err)
			}
			if pid, err := tcpTablesOwner(v4, v6, self, peer); err != nil || pid != os.Getpid() {
				t.Fatalf("the owner of %s -> %s from tables read from a 1-byte first buffer = %d, %v; want this process %d", self, peer, pid, err, os.Getpid())
			}
		})
	}
}

// A connection no process holds names no one.
func TestTCPOwnerOnWindowsNamesNoOneForAConnectionNobodyHolds(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	peer := netip.MustParseAddrPort(ln.Addr().String())
	self := netip.AddrPortFrom(peer.Addr(), peer.Port()+1)
	if pid, err := tcpOwner(self, peer); err == nil || !strings.Contains(err.Error(), "no process holds") {
		t.Fatalf("tcpOwner of a connection nobody holds = %d, %v; want no process", pid, err)
	}
}
