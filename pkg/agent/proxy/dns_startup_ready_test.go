package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// freeTCPPort returns a currently-free localhost TCP port for the test to bind.
// (Reserve-:0-then-close is a small TOCTOU window, but the same DNSPort is used
// for both the TCP and UDP listeners here so it doubles as a free UDP port too.)
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
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
	port := freeTCPPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	// Nothing is listening yet.
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("port %d accepted a connection before the DNS server started", port)
	}

	s, served := serveDNSServerForTest(t, "tcp", port)
	select {
	case <-s.settled:
		if !s.bound {
			t.Fatalf("the TCP DNS server did not bind: %v", s.err)
		}
	case err := <-served:
		t.Fatalf("serve returned before the server settled: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the TCP DNS server never settled (NotifyStartedFunc not fired)")
	}

	// It settled as bound, so the socket must now be bound and accepting (TCP is
	// connection-oriented, so a successful dial proves it is genuinely listening).
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("port %d not accepting after the server settled as bound (it settled before the bind): %v", port, err)
	}
	_ = c.Close()
}

func TestAUDPDNSServerSettlesAsBoundWhenListening(t *testing.T) {
	s, served := serveDNSServerForTest(t, "udp", freeTCPPort(t))

	// UDP is connectionless, so we assert the readiness contract StartProxy relies
	// on: the server settles as bound rather than erroring or never settling.
	select {
	case <-s.settled:
		if !s.bound {
			t.Fatalf("the UDP DNS server did not bind: %v", s.err)
		}
	case err := <-served:
		t.Fatalf("serve returned before the server settled: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the UDP DNS server never settled (NotifyStartedFunc not fired)")
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
	port := freeTCPPort(t)
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(port)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // before startDNSServers starts a goroutine
	var g errgroup.Group

	if err := p.startDNSServers(ctx, &g); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("startDNSServers with its context ended: %v", err)
	}
	// The servers' other stopper, racing serveDNS's: it follows the same rule,
	// and whichever comes first shuts each server down.
	stopped := make(chan error, 1)
	go func() { stopped <- p.stopDNSServers() }()
	if err := g.Wait(); err != nil {
		t.Fatalf("stopping the DNS servers as their context ended: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stopDNSServers: %v", err)
	}

	// Both ports are free again: these binds do not set SO_REUSEPORT, so they
	// cannot share a port with a DNS server still bound to it.
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("the TCP DNS server still holds its port: %v", err)
	}
	_ = l.Close()
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("the UDP DNS server still holds its port: %v", err)
	}
	_ = pc.Close()
}
