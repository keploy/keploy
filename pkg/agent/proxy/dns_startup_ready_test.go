package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/plaintcp"
	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// freeDNSPort is a port a TCP and a UDP DNS server can both bind, on every
// address, as they do: one the kernel gives a TCP listener whose number is
// free on UDP too, found by allocation. A free TCP port says nothing of UDP:
// on a busy host another process's UDP socket (a resolver's ephemeral port)
// can hold the same number. The port is released for the servers to bind, so
// another process can still take it first: startDNSForTest's servers move
// then.
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

// startDNSForTest starts a proxy's DNS servers as StartProxy does, on a free
// port (freeDNSPort; one another process took first moves, the port mover
// accepting every move), and stops them when the test ends.
func startDNSForTest(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(freeDNSPort(t))}
	p.SetPortMover(func(uint32, uint32) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	var g errgroup.Group
	t.Cleanup(func() { cancel(); _ = g.Wait() })
	if err := p.startDNSServers(ctx, &g); err != nil {
		t.Fatalf("startDNSServers: %v", err)
	}
	return p
}

// The DNS servers used to be launched in un-awaited goroutines, so StartProxy
// (and everything gated on it -- notably the agent readiness that releases the
// depends_on'd app container in docker-compose replay) could proceed before the
// DNS socket was bound. A reconstructed app resolving a recorded name at boot
// then raced an unbound socket and died with UnknownHostException. The
// sockets are now bound before startDNSServers starts a server on them, and it
// returns once both serve (settled, by miekg/dns's NotifyStartedFunc).
func TestStartedDNSServersServeOnTheirBoundPort(t *testing.T) {
	p := startDNSForTest(t)
	for _, s := range []*dnsServer{p.tcpDNS, p.udpDNS} {
		select {
		case <-s.settled:
		default:
			t.Fatalf("startDNSServers returned before the %s DNS server settled", s.name())
		}
		if !s.serving {
			t.Fatalf("the %s DNS server does not serve: %v", s.name(), s.err)
		}
	}
	// TCP is connection-oriented, so a successful dial proves the port is
	// genuinely listening.
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.DNSPort))), 2*time.Second)
	if err != nil {
		t.Fatalf("port %d not accepting once startDNSServers returned: %v", p.DNSPort, err)
	}
	_ = c.Close()
}

// The TCP DNS server listens on every interface, so its socket is plain TCP:
// miekg/dns would bind it with Go's default, MPTCP.
func TestTheTCPDNSServerListensPlainTCP(t *testing.T) {
	plaintcptest.RequirePlainTCP(t, startDNSForTest(t).tcpDNS.srv.Listener)
}

// A server whose serve call fails before it serves (here: handed no socket)
// settles as not serving, with its error, and stopping it shuts nothing down
// and does not wait. serve logs it as one ERROR, with the error.
func TestADNSServerThatCannotServeSettlesWithItsError(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	p := &Proxy{logger: zap.New(core), DNSPort: 53}
	s := p.newDNSServer("tcp")

	if err := s.serve(); err == nil {
		t.Fatal("serve served with no socket")
	}
	select {
	case <-s.settled:
	default:
		t.Fatal("a server whose serve call returned without serving is not settled")
	}
	if s.serving || s.err == nil {
		t.Fatalf("settled as serving=%v err=%v; want not serving, with serve's error", s.serving, s.err)
	}
	errs := logs.FilterLevelExact(zap.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("serve logged %d ERRORs for a serve that failed; want 1: %v", len(errs), errs)
	}
	if want := "the TCP DNS server failed to serve or stopped serving"; errs[0].Message != want {
		t.Fatalf("a serve that failed logged %q; want %q", errs[0].Message, want)
	}
	if got := errs[0].ContextMap()["error"]; got != s.err.Error() {
		t.Fatalf("a failed serve's ERROR carries error %v; want serve's own, %q", got, s.err)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("stopping a server that never served: %v", err)
	}
}

// A server whose serve call fails after the bind, before it serves, fails
// the DNS servers' start with its own error: startDNSServers never returns
// as if a server served that does not. Here the UDP server's socket is closed
// as the server logs that it starts, on its serving goroutine, just before
// miekg/dns sets the socket's options to serve on it.
func TestADNSServerThatCannotServeFailsTheDNSServersStart(t *testing.T) {
	core, _ := observer.New(zap.InfoLevel)
	var p *Proxy
	closeUDP := zap.Hooks(func(e zapcore.Entry) error {
		if strings.HasPrefix(e.Message, "starting UDP DNS server") {
			_ = p.udpDNS.srv.PacketConn.Close()
		}
		return nil
	})
	p = &Proxy{logger: zap.New(core, closeUDP), DNSPort: uint32(freeDNSPort(t))}
	p.SetPortMover(func(uint32, uint32) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	var g errgroup.Group
	t.Cleanup(func() { cancel(); _ = g.Wait() })

	err := p.startDNSServers(ctx, &g)
	if err == nil {
		t.Fatal("startDNSServers returned nil while the UDP DNS server does not serve")
	}
	if p.udpDNS.serving || !errors.Is(p.udpDNS.err, net.ErrClosed) {
		t.Fatalf("the UDP server settled as serving=%v err=%v; want not serving, failed on its closed socket", p.udpDNS.serving, p.udpDNS.err)
	}
	if !errors.Is(err, p.udpDNS.err) {
		t.Fatalf("startDNSServers returned %v; want the UDP server's own error, %v", err, p.udpDNS.err)
	}
}

// A serve call that returns without serving lets go of the socket it was
// handed: nothing else would, and the port would be held for the life of the
// process. miekg/dns refuses to start a server twice, so a server that serves
// already has the second serve call return without serving.
func TestADNSServerThatReturnsWithoutServingLetsItsSocketGo(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			s := (&Proxy{logger: zap.NewNop()}).newDNSServer(network)
			// closed reports whether the server's socket is closed.
			var closed func() bool
			if network == "tcp" {
				ln, err := plaintcp.Listen(context.Background(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				s.srv.Listener = ln
				closed = func() bool { return errors.Is(ln.(*net.TCPListener).SetDeadline(time.Now()), net.ErrClosed) }
			} else {
				pc, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pc.Close() })
				s.srv.PacketConn = pc
				closed = func() bool { return errors.Is(pc.SetReadDeadline(time.Now()), net.ErrClosed) }
			}
			first := make(chan error, 1)
			go func() { first <- s.srv.ActivateAndServe() }()
			<-s.settled // it serves: miekg/dns has started the server

			if err := s.serve(); err == nil {
				t.Fatal("a second serve call served")
			}
			if !closed() {
				t.Fatal("a serve call that returned without serving kept its socket")
			}
			<-first // its socket closed under it
		})
	}
}

// StartProxy's context can end before its DNS goroutines have run, as it does
// when the agent is stopped while it starts (a race build of the agent,
// stopped so while starved of memory, reported the races below). Each server
// is then stopped once it serves, not before: miekg/dns refuses to shut down
// a server that has not started ("server not started"), and the serve call
// that came after held the port for the life of the process. The goroutines
// that stopped the servers also read the fields that the goroutines serving
// them were still to set, which the race detector reports.
//
// With the code that bound each socket as its server started, 40 of 40 runs
// left the UDP port bound after the servers' group returned, 16 of them the
// TCP port too, and under -race every run reported both fields' races.
func TestADNSServerCancelledBeforeItServesIsStoppedOnceItServes(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(freeDNSPort(t))}
	p.SetPortMover(func(uint32, uint32) error { return nil }) // a port taken first moves
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

	// Each server served, and is shut down: its own socket is closed. A probe
	// that binds the port number again would also fail on another process's
	// socket. This proves only that the sockets the servers served on are
	// closed, not that no other socket holds the port; for the bug pinned
	// here, a late serve after a refused shutdown, the server's own socket is
	// the one held.
	for _, s := range []*dnsServer{p.tcpDNS, p.udpDNS} {
		<-s.settled
		if !s.serving {
			t.Fatalf("the %s DNS server did not serve: %v", s.name(), s.err)
		}
	}
	if err := p.tcpDNS.srv.Listener.(*net.TCPListener).SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the TCP DNS server still holds its socket: %v", err)
	}
	if err := p.udpDNS.srv.PacketConn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the UDP DNS server still holds its socket: %v", err)
	}
}
