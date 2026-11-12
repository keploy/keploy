//go:build windows

package routes

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// The client end of a connection this process opened is read as this
// process's, over IPv4 and IPv6, from the system's own tables: their layout
// and the owning pid are what GetExtendedTcpTable fills, not a test's.
func TestTCPOwnerOnWindowsFindsTheClientEnd(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			host := "127.0.0.1"
			if network == "tcp6" {
				host = "[::1]"
			}
			ln, err := net.Listen(network, host+":0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			c, err := net.Dial(network, ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			s, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			self := netip.MustParseAddrPort(c.LocalAddr().String())
			peer := netip.MustParseAddrPort(c.RemoteAddr().String())
			pid, err := tcpOwner(self, peer)
			if err != nil || pid != os.Getpid() {
				t.Fatalf("tcpOwner(%s -> %s) = %d, %v; want this process %d", self, peer, pid, err, os.Getpid())
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

// ownProcessGroup starts c in a process group of its own; killProcessGroup
// ends it (the rows' children start no process of their own).
func ownProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func killProcessGroup(c *exec.Cmd) { _ = c.Process.Kill() }
