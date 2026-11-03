//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/utils/plaintcp"
)

// unlistedAddr is ":P", every address at a port P the socket tables list no
// socket at and that no other socket is given for the test's life
// (testnet.UnlistedPort): free at UDP, and at TCP to a listener that sets
// SO_REUSEADDR, as the proxy's and the DNS server's do. A port a bind to port
// 0 gave could be taken by another process once it is let go, or be at
// another process's socket in another table, which the holder the start
// names would then include.
func unlistedAddr(t *testing.T) (string, uint32) {
	t.Helper()
	port := testnet.UnlistedPort(t)
	return fmt.Sprintf(":%d", port), uint32(port)
}

// recordMoves sets p's port mover to one that keeps what it was told and
// returns refuse.
func recordMoves(p *Proxy, told *[][2]uint32, refuse error) {
	p.SetPortMover(func(proxyPort, dnsPort uint32) error {
		*told = append(*told, [2]uint32{proxyPort, dnsPort})
		return refuse
	})
}

// freeAgain fails t unless a TCP and a UDP socket can bind port again: what
// a start that failed bound there was let go.
func freeAgain(t *testing.T, what string, port uint32) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("%s: port %d is still held (TCP): %v", what, port, err)
	}
	_ = ln.Close()
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("%s: port %d is still held (UDP): %v", what, port, err)
	}
	_ = pc.Close()
}

// Two agents started together on one port, as two keploy runs on one host on
// the default ports: the CLI found the port free for both, and the first
// agent's proxy holds it by the time the second binds. The second proxy
// listens on another port, keeps it as its port, says which was taken and by
// what, and tells the port mover where it and its DNS server listen before
// it says it is ready. Where its port may not move (no SetPortMover), its
// start fails naming both; where the mover refuses, the start fails and the
// new port is let go. A port that is free is kept, and no mover is told.
func TestTwoProxiesStartedTogetherOnOnePortBothListen(t *testing.T) {
	addr, taken := unlistedAddr(t)
	first, err := plaintcp.Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	holder := fmt.Sprintf("the proxy's port %d is taken by pid %d (", taken, os.Getpid())

	core, logs := observer.New(zap.WarnLevel)
	var told [][2]uint32
	second, _ := startListening(t, nil, func(p *Proxy) {
		p.Port, p.DNSPort, p.logger = taken, 26789, zap.New(core)
		recordMoves(p, &told, nil)
	})
	port := second.Port
	if port == taken || uint32(second.Listener.Addr().(*net.TCPAddr).Port) != port {
		t.Fatalf("the second proxy listens on %v, keeps port %d; the first holds %d", second.Listener.Addr(), port, taken)
	}
	if len(told) != 1 || told[0] != [2]uint32{port, 26789} {
		t.Fatalf("the mover was told %v, want [[%d 26789]]", told, port)
	}
	if logs.Len() != 1 || !strings.Contains(logs.All()[0].Message, holder) ||
		!strings.Contains(logs.All()[0].Message, fmt.Sprintf("listens on port %d instead", port)) {
		t.Fatalf("want one WARN naming %q and the new port %d, got %v", holder, port, logs.All())
	}

	serve := func(p *Proxy) (ready, start error) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		readyChan := make(chan error, 1)
		start = p.Serve(ctx, readyChan)
		return <-readyChan, start
	}
	cfg := config.New()
	cfg.ProxyPort = taken
	fixed := New(zap.NewNop(), plainDestInfo{}, cfg)
	if ready, start := serve(fixed); !errors.Is(ready, syscall.EADDRINUSE) || !strings.Contains(ready.Error(), holder) || !errors.Is(start, syscall.EADDRINUSE) || fixed.Port != taken {
		t.Fatalf("a port that may not move: want EADDRINUSE naming %q on port %d, got ready %v, start %v, port %d", holder, taken, ready, start, fixed.Port)
	}

	refused := errors.New("refused")
	told = nil
	refusing := New(zap.NewNop(), plainDestInfo{}, cfg)
	recordMoves(refusing, &told, refused)
	if ready, start := serve(refusing); !errors.Is(ready, refused) || !errors.Is(start, refused) || len(told) != 1 ||
		told[0][0] == taken || refusing.Port != taken || !strings.Contains(ready.Error(), holder) {
		t.Fatalf("a mover that refuses: want the start failed naming %q, port kept at %d; got ready %v, start %v, told %v, port %d", holder, taken, ready, start, told, refusing.Port)
	}
	freeAgain(t, "a refused move", told[0][0])

	told = nil
	_, free := unlistedAddr(t)
	kept, _ := startListening(t, nil, func(p *Proxy) { p.Port = free; recordMoves(p, &told, nil) })
	if len(told) != 0 || kept.Port != free || uint32(kept.Listener.Addr().(*net.TCPAddr).Port) != free {
		t.Fatalf("free port %d: the mover was told %v; listens on %v, keeps port %d", free, told, kept.Listener.Addr(), kept.Port)
	}
}

// dnsProxy is a proxy, as the agent makes one, on proxy port port (0: the
// default) and DNS port dnsPort.
func dnsProxy(port, dnsPort uint32) *Proxy {
	cfg := config.New()
	if port != 0 {
		cfg.ProxyPort = port
	}
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.DNSPort = dnsPort
	return p
}

// startDNSOn starts p's DNS servers and returns their stop.
func startDNSOn(t *testing.T, p *Proxy) (func(), error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var g errgroup.Group
	err := p.startDNSServers(ctx, &g)
	return func() { cancel(); _ = g.Wait() }, err
}

// Two agents' DNS servers on one port. With SO_REUSEPORT the second bound it
// beside the first, and the kernel then shared each one's queries between
// the two: each run's app could be answered by the other's mocks. The second
// now moves to a port of its own and tells the port mover, or, where it may
// not move, fails; where the mover refuses, it fails and lets the new port go.
func TestTwoDNSServersStartedTogetherOnOnePortDoNotShareIt(t *testing.T) {
	_, want := unlistedAddr(t)
	first := dnsProxy(0, want)
	stop, err := startDNSOn(t, first)
	defer stop()
	if err != nil {
		t.Fatalf("the first DNS servers: %v", err)
	}
	for _, s := range []*dnsServer{first.tcpDNS, first.udpDNS} {
		if on := reusePortOf(t, s); on != 0 {
			t.Fatalf("the %s DNS server's SO_REUSEPORT is %d: a second agent can bind its port beside it", s.name(), on)
		}
	}
	if got := askDNS(t, "udp", want); got != nil {
		t.Fatalf("the first DNS server over UDP: %v", got)
	}
	if got := askDNS(t, "tcp", want); got != nil {
		t.Fatalf("the first DNS server over TCP: %v", got)
	}

	var told [][2]uint32
	second := dnsProxy(16790, want)
	recordMoves(second, &told, nil)
	stop2, err := startDNSOn(t, second)
	defer stop2()
	if err != nil {
		t.Fatalf("the second DNS servers: %v", err)
	}
	if second.DNSPort == want || len(told) != 1 || told[0] != [2]uint32{16790, second.DNSPort} {
		t.Fatalf("the second agent's DNS servers: port %d (the first's %d), the mover told %v", second.DNSPort, want, told)
	}
	if got := askDNS(t, "udp", second.DNSPort); got != nil {
		t.Fatalf("the moved DNS server over UDP: %v", got)
	}

	third := dnsProxy(0, want)
	stop3, err := startDNSOn(t, third)
	defer stop3()
	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), fmt.Sprintf("port %d is taken by pid %d", want, os.Getpid())) || third.DNSPort != want {
		t.Fatalf("a DNS port that may not move: want EADDRINUSE naming this process on port %d, got %v, port %d", want, err, third.DNSPort)
	}

	refused := errors.New("refused")
	told = nil
	fourth := dnsProxy(0, want)
	recordMoves(fourth, &told, refused)
	stop4, err := startDNSOn(t, fourth)
	defer stop4()
	if !errors.Is(err, refused) || len(told) != 1 || told[0][0] != fourth.Port || told[0][1] == want || fourth.DNSPort != want {
		t.Fatalf("a mover that refuses: got %v, told %v, DNS port %d (want kept at %d)", err, told, fourth.DNSPort, want)
	}
	freeAgain(t, "a refused move", told[0][1])
}

// The DNS servers bind UDP and TCP on one port: where only TCP is taken, the
// UDP socket bound there is let go, and both move to a port of their own.
func TestADNSPortTakenOnTCPOnlyLetsItsUDPGo(t *testing.T) {
	addr, taken := unlistedAddr(t)
	ln, err := plaintcp.Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var told [][2]uint32
	p := dnsProxy(0, taken)
	recordMoves(p, &told, nil)
	stop, err := startDNSOn(t, p)
	defer stop()
	if err != nil || p.DNSPort == taken || len(told) != 1 {
		t.Fatalf("got %v, port %d (taken %d), told %v", err, p.DNSPort, taken, told)
	}
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", taken))
	if err != nil {
		t.Fatalf("the UDP socket on the TCP-taken port %d was kept: %v", taken, err)
	}
	_ = pc.Close()
}

// A bind that fails for any reason but a taken port fails the start as it
// is, and moves nothing; where every other port fails to bind, the start
// fails saying so, keeps its port, and tells no mover.
func TestOnlyATakenPortMovesAndOnlyToABoundOne(t *testing.T) {
	var told [][2]uint32
	p := dnsProxy(0, 70000)
	recordMoves(p, &told, nil)
	stop, err := startDNSOn(t, p)
	defer stop()
	if err == nil || errors.Is(err, syscall.EADDRINUSE) || p.DNSPort != 70000 || len(told) != 0 {
		t.Fatalf("an invalid port: got %v, port %d, told %v", err, p.DNSPort, told)
	}

	boom := errors.New("boom")
	q := &Proxy{logger: zap.NewNop(), Port: 16789}
	recordMoves(q, &told, nil)
	err = q.bindOrMove("proxy", &q.Port, func(port uint32) error {
		if port == 16789 {
			return syscall.EADDRINUSE
		}
		return boom
	}, func() { t.Fatal("nothing bound was let go") })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "no other port could be bound") || q.Port != 16789 || len(told) != 0 {
		t.Fatalf("no other port bound: got %v, port %d, told %v", err, q.Port, told)
	}
}

// askDNS sends the DNS server on port a query over network and returns nil
// once it answers.
func askDNS(t *testing.T, network string, port uint32) error {
	t.Helper()
	c := &dns.Client{Net: network, Timeout: 5 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion("keploy.example.", dns.TypeA)
	_, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	return err
}

func reusePortOf(t *testing.T, s *dnsServer) int {
	t.Helper()
	var sc syscall.Conn
	if s.srv.Listener != nil {
		sc = s.srv.Listener.(*net.TCPListener)
	} else {
		sc = s.srv.PacketConn.(*net.UDPConn)
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var on int
	var gerr error
	if err := rc.Control(func(fd uintptr) { on, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return on
}
