package proxy

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
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

// The DNS servers used to be launched in un-awaited goroutines, so StartProxy
// (and everything gated on it -- notably the agent readiness that releases the
// depends_on'd app container in docker-compose replay) could proceed before the
// DNS socket was bound. A reconstructed app resolving a recorded name at boot
// then raced an unbound socket and died with UnknownHostException. The fix wires
// miekg/dns's NotifyStartedFunc through start{TCP,UDP}DNSServer so StartProxy can
// block until DNS is actually listening. These tests assert the core invariant
// that gate now depends on: onListening fires only AFTER the socket is bound.

func TestStartTCPDNSServer_SignalsOnlyAfterBound(t *testing.T) {
	port := freeTCPPort(t)
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(port)}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	t.Cleanup(func() {
		if p.TCPDNSServer != nil {
			_ = p.TCPDNSServer.Shutdown()
		}
	})

	// Nothing is listening yet.
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("port %d accepted a connection before the DNS server started", port)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- p.startTCPDNSServer(ctx, func() { close(ready) }) }()

	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("startTCPDNSServer returned before signalling listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startTCPDNSServer never signalled listening (NotifyStartedFunc not fired)")
	}

	// The signal fired, so the socket must now be bound and accepting (TCP is
	// connection-oriented, so a successful dial proves it is genuinely listening).
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("port %d not accepting after onListening fired (signal fired before bind): %v", port, err)
	}
	_ = c.Close()
}

func TestStartUDPDNSServer_SignalsWhenListening(t *testing.T) {
	port := freeTCPPort(t)
	p := &Proxy{logger: zap.NewNop(), DNSPort: uint32(port)}
	t.Cleanup(func() {
		if p.UDPDNSServer != nil {
			_ = p.UDPDNSServer.Shutdown()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- p.startUDPDNSServer(ctx, func() { close(ready) }) }()

	// UDP is connectionless, so we assert the readiness contract StartProxy relies
	// on: NotifyStartedFunc fires (bound) rather than the server erroring or never
	// signalling.
	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("startUDPDNSServer returned before signalling listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startUDPDNSServer never signalled listening (NotifyStartedFunc not fired)")
	}
}
