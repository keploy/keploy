package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// freeDNSPort is a port a TCP and a UDP DNS server can both bind, on every
// address, as they do: one the kernel gives a TCP listener whose number is
// free on UDP too, found by allocation. A free TCP port says nothing of UDP:
// on a busy host another process's UDP socket (a resolver's ephemeral port)
// can hold the same number. The port is released for the server to bind, so
// another process can still take it first; serveDNSOnFreePort retries then.
func freeDNSPort(t *testing.T) int {
	t.Helper()
	for range 1000 {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("could not reserve a free port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		_ = l.Close()
		if err != nil {
			continue // held on UDP: take another
		}
		_ = pc.Close()
		return port
	}
	t.Fatal("no port free on both TCP and UDP in 1000 allocations")
	return 0
}

// serveDNSOnFreePort serves a DNS server for network on a free port
// (freeDNSPort) and returns it once it has settled, with its port and the
// channel its serve call's result arrives on. before runs on each port before
// the server starts. A server whose bind another process beat to the port
// (address already in use) is stopped, and another port tried, by allocation.
func serveDNSOnFreePort(t *testing.T, network string, before func(port int)) (*dnsServer, int, <-chan error) {
	t.Helper()
	for range 100 {
		port := freeDNSPort(t)
		if before != nil {
			before(port)
		}
		s, served := serveDNSServerForTest(t, network, port)
		// Settling is the server's own event: a bind fires NotifyStartedFunc,
		// and a serve call that returns without one settles it too. A server
		// that does neither hangs the test, for go test's -timeout to report.
		<-s.settled
		if !s.bound && errors.Is(s.err, syscall.EADDRINUSE) {
			continue
		}
		return s, port, served
	}
	t.Fatalf("the %s DNS server found its port taken 100 times", network)
	return nil, 0, nil
}

// serveDNSServerForTest serves a new DNS server for network on port, stops it
// when the test ends, and returns it with the channel its serve call's result
// arrives on.
func serveDNSServerForTest(t *testing.T, network string, port int) (*dnsServer, <-chan error) {
	t.Helper()
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(port)}
	s := p.newDNSServer(network)
	served := make(chan error, 1)
	go func() { served <- s.serve() }()
	t.Cleanup(func() { _ = s.stop() })
	return s, served
}

// The DNS servers used to be launched in un-awaited goroutines, so StartProxy
// (and everything gated on it -- notably the agent readiness that releases the
// depends_on'd app container in docker-compose replay) could proceed before the
// DNS socket was bound. A reconstructed app resolving a recorded name at boot
// then raced an unbound socket and died with UnknownHostException. The fix wires
// miekg/dns's NotifyStartedFunc into dnsServer.settled so StartProxy can block
// until DNS is actually listening. These tests assert the core invariant that
// gate now depends on: a server settles as bound only AFTER its socket is bound.

func TestATCPDNSServerSettlesAsBoundOnlyAfterItIsBound(t *testing.T) {
	s, port, _ := serveDNSOnFreePort(t, "tcp", func(port int) {
		// Nothing is listening yet.
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatalf("port %d accepted a connection before the DNS server started", port)
		}
	})
	if !s.bound {
		t.Fatalf("the TCP DNS server did not bind: %v", s.err)
	}

	// It settled as bound, so the socket must now be bound and accepting (TCP is
	// connection-oriented, so a successful dial proves it is genuinely listening).
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("port %d not accepting after the server settled as bound (it settled before the bind): %v", port, err)
	}
	_ = c.Close()
}

// The TCP DNS server listens on every interface, so its socket is plain TCP:
// miekg/dns would bind it with Go's default, MPTCP.
func TestTheTCPDNSServerListensPlainTCP(t *testing.T) {
	s, _, _ := serveDNSOnFreePort(t, "tcp", nil)
	if !s.bound {
		t.Fatalf("the TCP DNS server did not bind: %v", s.err)
	}
	plaintcptest.RequirePlainTCP(t, s.srv.Listener)
}

func TestAUDPDNSServerSettlesAsBoundWhenListening(t *testing.T) {
	// UDP is connectionless, so we assert the readiness contract StartProxy relies
	// on: the server settles as bound rather than erroring or never settling.
	if s, _, _ := serveDNSOnFreePort(t, "udp", nil); !s.bound {
		t.Fatalf("the UDP DNS server did not bind: %v", s.err)
	}
}

// A server whose bind fails settles as not bound, with the bind's error, and
// stopping it shuts nothing down and does not wait. The one ERROR serve logs
// is true of a failed bind too: it does not say the server stopped serving,
// since it never served, and it carries the bind's error.
func TestADNSServerThatCannotBindSettlesWithItsError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0") // no SO_REUSEPORT: the server cannot share it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	core, logs := observer.New(zap.InfoLevel)
	p := &Proxy{logger: zap.New(core), DNSPort: uint32(held.Addr().(*net.TCPAddr).Port)}
	s := p.newDNSServer("tcp")

	if err := s.serve(); err == nil {
		t.Fatal("serve bound a port another socket holds")
	}
	select {
	case <-s.settled:
	default:
		t.Fatal("a server whose serve call returned without binding is not settled")
	}
	if s.bound || s.err == nil {
		t.Fatalf("settled as bound=%v err=%v; want not bound, with the bind's error", s.bound, s.err)
	}
	errs := logs.FilterLevelExact(zap.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("serve logged %d ERRORs for a failed bind; want 1: %v", len(errs), errs)
	}
	if want := "the TCP DNS server failed to bind or stopped serving"; errs[0].Message != want {
		t.Fatalf("a failed bind logged %q; want %q", errs[0].Message, want)
	}
	if got := errs[0].ContextMap()["error"]; got != s.err.Error() {
		t.Fatalf("a failed bind's ERROR carries error %v; want the bind's own, %q", got, s.err)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("stopping a server that never bound: %v", err)
	}
}

// StartProxy's context can end before its DNS goroutines have run, as it does
// when the agent is stopped while it starts (a race build of the agent,
// stopped so while starved of memory, reported the races below). Each server
// is then stopped once it has bound, not before: miekg/dns refuses to shut down
// a server that has not bound ("server not started"), and the bind that came
// after held the port for the life of the process. The goroutines that stopped
// the servers also read the fields that the goroutines serving them were
// still to set, which the race detector reports.
//
// With the old code, 40 of 40 runs left the UDP port bound after the servers'
// group returned, 16 of them the TCP port too, and under -race every run
// reported both fields' races.
func TestADNSServerCancelledBeforeItBindsIsStoppedOnceItBinds(t *testing.T) {
	for range 100 {
		p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(freeDNSPort(t))}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // before startDNSServers starts a goroutine
		var g errgroup.Group

		if err := p.startDNSServers(ctx, &g); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("startDNSServers with its context ended: %v", err)
		}
		// The servers' other stopper, racing serveDNS's: it follows the same
		// rule, and whichever comes first shuts each server down.
		stopped := make(chan error, 1)
		go func() { stopped <- p.stopDNSServers() }()
		if err := g.Wait(); err != nil {
			t.Fatalf("stopping the DNS servers as their context ended: %v", err)
		}
		if err := <-stopped; err != nil {
			t.Fatalf("stopDNSServers: %v", err)
		}
		// Another process took the port first (freeDNSPort releases it for the
		// servers to bind): a server that could not bind proves nothing here.
		if lostPort(p.tcpDNS) || lostPort(p.udpDNS) {
			continue
		}

		// Each server bound, and is shut down: its own socket is closed. A
		// probe that binds the port number again would also fail on another
		// process's socket. This proves only that the sockets the servers
		// bound are closed, not that no other socket holds the port; for the
		// bug pinned here, a late bind after a refused shutdown, the server's
		// own socket is the one held.
		for _, s := range []*dnsServer{p.tcpDNS, p.udpDNS} {
			if !s.bound {
				t.Fatalf("the %s DNS server did not bind: %v", s.name(), s.err)
			}
		}
		if err := p.tcpDNS.srv.Listener.(*net.TCPListener).SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("the TCP DNS server still holds its socket: %v", err)
		}
		if err := p.udpDNS.srv.PacketConn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("the UDP DNS server still holds its socket: %v", err)
		}
		return
	}
	t.Fatal("the DNS servers found their port taken 100 times")
}

// lostPort reports whether s did not bind because another socket held its
// port (address already in use).
func lostPort(s *dnsServer) bool {
	<-s.settled
	return !s.bound && errors.Is(s.err, syscall.EADDRINUSE)
}
