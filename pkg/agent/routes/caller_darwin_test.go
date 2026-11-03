//go:build darwin

package routes

import (
	"bufio"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// The kernel names this process as the holder of the client end of its own
// connection, over IPv4 and over IPv6, and only of the client end: the
// server's accepted end, the same two endpoints the other way round, is not
// the socket asked for.
func TestTCPOwnerFindsTheClientEnd(t *testing.T) {
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
			defer ln.Close()
			c, err := net.Dial(network, ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
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
func TestTCPOwnerNamesNoOneForAConnectionNobodyHolds(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer := netip.MustParseAddrPort(ln.Addr().String())
	self := netip.AddrPortFrom(peer.Addr(), peer.Port()+1)
	if pid, err := tcpOwner(self, peer); err == nil || !strings.Contains(err.Error(), "no process holds") {
		t.Fatalf("tcpOwner of a connection nobody holds = %d, %v; want no process", pid, err)
	}
}

// Only TCP sockets are read: a UDP socket this process binds to the same two
// endpoints as a child's TCP connection (UDP ports are their own namespace)
// does not make this process, the child's parent, its holder.
func TestTCPOwnerReadsOnlyTCPSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	child, local := holdConnFrom(t, ln.Addr().String())
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	self := netip.MustParseAddrPort(local)
	peer := netip.MustParseAddrPort(ln.Addr().String())
	decoy, err := net.DialUDP("udp4", net.UDPAddrFromAddrPort(self), net.UDPAddrFromAddrPort(peer))
	if err != nil {
		t.Fatalf("binding the UDP decoy to %s -> %s: %v", self, peer, err)
	}
	defer decoy.Close()
	pid, err := tcpOwner(self, peer)
	if err != nil || pid != child.Process.Pid {
		t.Fatalf("tcpOwner(%s -> %s) = %d, %v; want the child %d", self, peer, pid, err, child.Process.Pid)
	}
}

// A listening socket on the asked local endpoint is not the connection asked
// for: the remote end must match too.
func TestTCPOwnerNeedsTheRemoteEndToMatch(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	self := netip.MustParseAddrPort(ln.Addr().String())
	peer := netip.AddrPortFrom(self.Addr(), 1)
	if pid, err := tcpOwner(self, peer); err == nil || !strings.Contains(err.Error(), "no process holds") {
		t.Fatalf("tcpOwner(%s -> %s), only a listener on %s = %d, %v; want no process", self, peer, self, pid, err)
	}
}

// A child that holds a copy of this process's client socket (as one being
// started holds it between its fork and its exec) does not hide the caller:
// the holder the other descends from, this process, is named.
func TestTCPOwnerNamesTheParentOfAChildHoldingACopy(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := c.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), envHoldUntilStdinCloses+"=1")
	child.ExtraFiles = []*os.File{f}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil { // returns once the child has exec'd, holding the copy
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})
	self := netip.MustParseAddrPort(c.LocalAddr().String())
	peer := netip.MustParseAddrPort(c.RemoteAddr().String())
	if pid, err := tcpOwner(self, peer); err != nil || pid != os.Getpid() {
		t.Fatalf("tcpOwner(%s -> %s), held by this process and its child %d = %d, %v; want this process %d", self, peer, child.Process.Pid, pid, err, os.Getpid())
	}
}

// holdConnFrom starts this test binary again as a child that dials addr and
// holds the connection; it returns the child and the connection's local
// address. The child and its process group die in cleanup.
func holdConnFrom(t *testing.T, addr string) (*exec.Cmd, string) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envHoldConnTo+"="+addr)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the holding child said nothing: %v", err)
	}
	return c, strings.TrimSpace(line)
}
