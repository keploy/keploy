package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// newTLSTestServer spins up a TLS listener with a self-signed cert. The
// handler hook (if non-nil) receives each accepted, fully-handshaked
// *tls.Conn. Returns the listener and a teardown that closes it.
func newTLSTestServer(t *testing.T, handshakeDelay time.Duration, nextProtos []string, onAccept func(*tls.Conn)) (net.Listener, *tls.Config) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		DNSNames:     []string{"test.local"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   nextProtos,
	}

	// Wrap with a delaying listener so we can simulate slow upstream
	// handshake behavior (the delay fires after TCP accept, before TLS
	// handshake — i.e. between the ClientHello arriving and the server
	// starting to reply).
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(&delayingListener{Listener: rawLn, delay: handshakeDelay}, cfg)

	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			tc, ok := c.(*tls.Conn)
			if !ok {
				_ = c.Close()
				continue
			}
			// Drive the handshake so NegotiatedProtocol is populated.
			if err := tc.Handshake(); err != nil {
				_ = tc.Close()
				continue
			}
			if onAccept != nil {
				onAccept(tc)
			}
			// Leave close to the client side / teardown.
		}
	}()

	return tlsLn, cfg
}

type delayingListener struct {
	net.Listener
	delay time.Duration
}

func (d *delayingListener) Accept() (net.Conn, error) {
	c, err := d.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	return c, nil
}

func TestStartSpeculativeUpstreamTLS_Join_Success(t *testing.T) {
	ln, _ := newTLSTestServer(t, 0, []string{"h2", "http/1.1"}, nil)
	defer ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
		NextProtos:         []string{"h2", "http/1.1"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := startSpeculativeUpstreamTLS(ctx, ln.Addr().String(), cfg)
	conn, err := s.join(ctx)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if conn == nil {
		t.Fatalf("expected non-nil conn")
	}
	defer conn.Close()

	negotiated := conn.ConnectionState().NegotiatedProtocol
	if negotiated != "h2" && negotiated != "http/1.1" {
		t.Fatalf("unexpected negotiated protocol: %q", negotiated)
	}
}

func TestStartSpeculativeUpstreamTLS_Abandon_Cleanup(t *testing.T) {
	ln, _ := newTLSTestServer(t, 0, []string{"http/1.1"}, nil)
	defer ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
		NextProtos:         []string{"http/1.1"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := startSpeculativeUpstreamTLS(ctx, ln.Addr().String(), cfg)
	s.abandon()

	// Give the background drainer a moment to run if the dial had
	// already succeeded. The race detector (-race) will flag any
	// mishandling of the buffered channel.
	time.Sleep(50 * time.Millisecond)

	// abandon() must be idempotent.
	s.abandon()
}

func TestStartSpeculativeUpstreamTLS_DialFailure(t *testing.T) {
	// Point at a closed port — dial fails fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s := startSpeculativeUpstreamTLS(ctx, addr, cfg)
	conn, err := s.join(ctx)
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("expected dial error, got nil")
	}
}

func TestStartSpeculativeUpstreamTLS_ContextCancelDuringDial(t *testing.T) {
	// Slow handshake so cancel lands mid-dial.
	ln, _ := newTLSTestServer(t, 500*time.Millisecond, []string{"http/1.1"}, nil)
	defer ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
	}

	parent, cancel := context.WithCancel(context.Background())
	s := startSpeculativeUpstreamTLS(parent, ln.Addr().String(), cfg)

	// Cancel before the handshake completes.
	time.Sleep(50 * time.Millisecond)
	cancel()

	joinCtx, joinCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer joinCancel()
	conn, err := s.join(joinCtx)
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("expected error from cancelled dial")
	}
}

// TestSpeculativeParallelism asserts that running the speculative upstream
// dial in parallel with a simulated MITM client-facing handshake takes
// ~max(client, upstream) rather than the sum. This is the core
// optimization — the test must fail if someone accidentally reverts the
// parallelization.
func TestSpeculativeParallelism(t *testing.T) {
	const upstreamDelay = 100 * time.Millisecond
	const clientDelay = 100 * time.Millisecond

	ln, _ := newTLSTestServer(t, upstreamDelay, []string{"h2", "http/1.1"}, nil)
	defer ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
		NextProtos:         []string{"h2", "http/1.1"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	s := startSpeculativeUpstreamTLS(ctx, ln.Addr().String(), cfg)
	// Simulate the MITM client-facing handshake.
	time.Sleep(clientDelay)
	conn, err := s.join(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer conn.Close()

	// Parallel: elapsed ~= max(clientDelay, upstreamDelay) = 100ms.
	// Serial would have been ~200ms. Tolerate ±40ms of jitter on top
	// of the nominal 100ms upper bound (race detector adds overhead).
	if elapsed > 160*time.Millisecond {
		t.Fatalf("handshake took %v — serial regression? expected ~%v", elapsed, clientDelay)
	}
	// Lower bound sanity: it can't be faster than the upstream delay
	// because the server waits that long before even starting the
	// handshake.
	if elapsed < upstreamDelay-20*time.Millisecond {
		t.Fatalf("handshake took %v — improbably fast, mock misconfigured?", elapsed)
	}
}

func TestNextProtosSubset(t *testing.T) {
	cases := []struct {
		name     string
		want     []string
		offered  []string
		expected bool
	}{
		{"empty want", nil, []string{"h2"}, true},
		{"single match", []string{"h2"}, []string{"h2", "http/1.1"}, true},
		{"missing element", []string{"h2", "http/1.1"}, []string{"h2"}, false},
		{"exact match", []string{"h2", "http/1.1"}, []string{"h2", "http/1.1"}, true},
		{"want subset of offered", []string{"http/1.1"}, []string{"h2", "http/1.1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextProtosSubset(tc.want, tc.offered); got != tc.expected {
				t.Fatalf("nextProtosSubset(%v, %v) = %v; want %v", tc.want, tc.offered, got, tc.expected)
			}
		})
	}
}
