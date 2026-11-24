//go:build darwin

package proxy

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/listeners"
)

// The first port is held for the test's life (testnet.SharedPort) and its
// listener opens beside the hold: once the listener closes, a server another
// test or process starts at port 0 is not given the port, so the dial that
// finds it closed is refused.
func TestListenerOwnerOnMacUsesReportedListens(t *testing.T) {
	ip := net.ParseIP("127.0.0.1")
	ln := testnet.ListenShared(t, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t))))
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	if pid, listening := listenerOwner(ip, port); pid != 0 || !listening {
		t.Fatalf("a live port nobody reported must be left to the mock path, got %d %v", pid, listening)
	}
	listeners.Note(uint32(os.Getpid()), uint16(port))
	if pid, listening := listenerOwner(ip, port); pid != os.Getpid() || !listening {
		t.Fatalf("owner = %d %v", pid, listening)
	}
	_ = ln.Close()
	if pid, listening := listenerOwner(ip, port); pid != 0 || listening {
		t.Fatalf("a closed port is not listening whatever the table says, got %d %v", pid, listening)
	}
	if _, ok := listeners.Owner(uint16(port)); ok {
		t.Fatal("the stale owner of a closed port must be forgotten")
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln2.Close() }()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	listeners.Note(uint32(cmd.Process.Pid), uint16(ln2.Addr().(*net.TCPAddr).Port))
	if pid, listening := listenerOwner(ip, uint32(ln2.Addr().(*net.TCPAddr).Port)); pid != 0 || !listening {
		t.Fatalf("a live port whose reported owner exited is someone else's, got %d %v", pid, listening)
	}
	if _, ok := listeners.Owner(uint16(ln2.Addr().(*net.TCPAddr).Port)); ok {
		t.Fatal("the owner that exited must be forgotten")
	}
	if !descends(os.Getpid(), os.Getppid()) || descends(os.Getppid(), os.Getpid()) {
		t.Fatal("descends must follow parents only")
	}
}

// A listener the run reported is read from its owner's descriptor table and
// never dialed: a server that takes one client and stops listening, as
// vstest's runner does for the testhost it starts, would take keploy's dial
// for that client. Each family, bound to the loopback address and to the
// unspecified one, is asked as the proxy asks it (listenerOwner, listening),
// and the listener's queue must hold nothing afterwards.
func TestListenerOwnerOnMacNeverDialsAReportedListener(t *testing.T) {
	for _, c := range []struct{ network, bind, dest string }{
		{"tcp4", "127.0.0.1:0", "127.0.0.1"},
		{"tcp4", "0.0.0.0:0", "127.0.0.1"},
		{"tcp6", "[::1]:0", "::1"},
		{"tcp6", "[::]:0", "::1"},
		{"tcp", "[::]:0", "::1"},
		{"tcp", "[::]:0", "127.0.0.1"},
	} {
		t.Run(c.network+" "+c.bind+"->"+c.dest, func(t *testing.T) {
			ln, err := net.Listen(c.network, c.bind)
			if err != nil {
				t.Skipf("no %s listener here: %v", c.network, err)
			}
			defer func() { _ = ln.Close() }()
			port := uint16(ln.Addr().(*net.TCPAddr).Port)
			listeners.Note(uint32(os.Getpid()), port)
			defer listeners.Forget(port)
			ip := net.ParseIP(c.dest)
			if pid, ok := listenerOwner(ip, uint32(port)); pid != os.Getpid() || !ok {
				t.Fatalf("listenerOwner = %d %v, want this process listening", pid, ok)
			}
			if !listening(ip, uint32(port)) {
				t.Fatal("listening = false for a live reported listener")
			}
			_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(300 * time.Millisecond))
			if conn, err := ln.Accept(); err == nil {
				_ = conn.Close()
				t.Fatal("the listener accepted a connection: keploy dialed a listener it can read passively")
			}
		})
	}
}

// A reported listener of the other family than the destination's is not
// taken to answer it: an IPv4-only 0.0.0.0 listener does not take ::1, and an
// IPV6_V6ONLY [::] one does not take 127.0.0.1 (the dial finds both refused).
func TestListenerOwnerOnMacKeepsTheFamilies(t *testing.T) {
	for _, c := range []struct{ network, bind, dest string }{
		{"tcp4", "0.0.0.0:0", "::1"},
		{"tcp6", "[::]:0", "127.0.0.1"},
	} {
		t.Run(c.bind+"->"+c.dest, func(t *testing.T) {
			ln, err := net.Listen(c.network, c.bind)
			if err != nil {
				t.Skipf("no %s listener here: %v", c.network, err)
			}
			defer func() { _ = ln.Close() }()
			port := uint16(ln.Addr().(*net.TCPAddr).Port)
			if ownerListens(os.Getpid(), net.ParseIP(c.dest), port) {
				t.Fatalf("a %s listener was taken to answer %s", c.bind, c.dest)
			}
		})
	}
}

// A call refused in one family does not forget a reported listener of the
// other: a client of localhost tries ::1 and then 127.0.0.1, and the second
// must still find the listener passively, not by a dial it takes.
func TestListenerOwnerOnMacKeepsAListenerTheOtherFamilyMissed(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	listeners.Note(uint32(os.Getpid()), port)
	defer listeners.Forget(port)
	if _, ok := listenerOwner(net.ParseIP("::1"), uint32(port)); ok {
		t.Fatal("::1 is taken to be listening at an IPv4-only listener")
	}
	if pid, ok := listenerOwner(net.ParseIP("127.0.0.1"), uint32(port)); pid != os.Getpid() || !ok {
		t.Fatalf("after ::1 was refused, 127.0.0.1 gives %d %v", pid, ok)
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(300 * time.Millisecond))
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("the listener accepted keploy's dial after the other family's refusal")
	}
}

// A reported listener whose owner is not the process holding the socket
// (one a child inherited and its owner closed) is still found: by the dial,
// as before, since the owner's table does not hold it.
func TestOwnerListensReadsOnlyTheOwnersTable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if !ownerListens(os.Getpid(), net.ParseIP("127.0.0.1"), port) {
		t.Fatal("ownerListens missed this process's own listener")
	}
	if ownerListens(os.Getpid(), net.ParseIP("127.0.0.2"), port) {
		t.Fatal("a listener bound to 127.0.0.1 does not listen at 127.0.0.2")
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if ownerListens(cmd.Process.Pid, net.ParseIP("127.0.0.1"), port) {
		t.Fatal("ownerListens found the listener in a process that does not hold it")
	}
}
