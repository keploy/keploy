package util

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
)

// selfSigned is a throwaway server certificate for 127.0.0.1.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// countingListener counts the connections a destination receives.
func countingListener(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return l.Addr().String(), &n
}

func waitCount(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := n.Load(); got != want {
		t.Fatalf("destination saw %d connections, want %d", got, want)
	}
}

// TestDialRawUsesThePredialedConnectionOnce: the connection opened while the
// handshake was held is the connection's first dial of that address — the
// destination sees one connection, as it would without keploy — and a later
// dial (a redial after an abandoned attempt) opens a new one.
func TestDialRawUsesThePredialedConnectionOnce(t *testing.T) {
	addr, n := countingListener(t)
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	ctx := context.Background()

	c1, err := DialDestination(ctx, nil, "tcp", DialTarget{Addr: addr, Predialed: p})
	if err != nil {
		t.Fatal(err)
	}
	if c1 != pre {
		t.Fatal("the first dial opened a new connection instead of using the pre-dialled one")
	}
	waitCount(t, n, 1)

	c2, err := DialRaw(ctx, nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2 == pre {
		t.Fatal("the pre-dialled connection was handed out twice")
	}
	waitCount(t, n, 2)

	p.CloseIfUnused() // taken: must not close the connection the dial owns
	if _, err := c1.Write([]byte("x")); err != nil {
		t.Fatalf("CloseIfUnused closed a connection a dial had taken: %v", err)
	}
}

// TestDialRawLeavesOtherAddressesAlone: a dial of some other address (a
// CONNECT tunnel's target, a redirect) is not given the pre-dialled
// connection, which then stays the first dial of its own address's.
func TestDialRawLeavesOtherAddressesAlone(t *testing.T) {
	addr, _ := countingListener(t)
	other, n := countingListener(t)
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	ctx := context.Background()

	c, err := DialRaw(ctx, nil, "tcp", other, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c == pre {
		t.Fatal("a dial of another address got the pre-dialled connection")
	}
	waitCount(t, n, 1)
	if p.take("udp", addr) != nil {
		t.Fatal("a UDP dial was given the TCP connection")
	}
	if got, _ := DialRaw(ctx, nil, "tcp", addr, p); got != pre {
		t.Fatal("the pre-dialled connection was not kept for its own address")
	}
}

// TestCloseIfUnusedClosesAnUntakenConnection: a connection whose path never
// dialled (or that ended first) must not leave its upstream open.
func TestCloseIfUnusedClosesAnUntakenConnection(t *testing.T) {
	addr, _ := countingListener(t)
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	p.CloseIfUnused()
	if _, err := pre.Write([]byte("x")); err == nil {
		t.Fatal("the untaken connection is still open")
	}
	if got := p.take("tcp", addr); got != nil {
		t.Fatal("a closed pre-dialled connection was handed to a dial")
	}
	var nilP *Predialed
	nilP.CloseIfUnused() // no pre-dial: nothing to do, no panic
	if none := (*Predialed)(nil); none.take("tcp", addr) != nil {
		t.Fatal("a nil pre-dial produced a connection")
	}
}

// TestDialTLSHandshakesOverThePredialedConnection: a TLS destination's
// handshake runs on the pre-dialled connection, with the ServerName
// tls.Dialer would have inferred.
func TestDialTLSHandshakesOverThePredialedConnection(t *testing.T) {
	cert := selfSigned(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	sni := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			tc := c.(*tls.Conn)
			if err := tc.Handshake(); err == nil {
				sni <- tc.ConnectionState().ServerName
			}
			_ = c.Close()
		}
	}()
	addr := l.Addr().String()
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, NewPredialed(addr, pre)) //nolint:gosec // test server
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.NetConn() != pre {
		t.Fatal("the TLS handshake ran on a new connection")
	}
	if got := <-sni; got != "" {
		// An IP-literal address: crypto/tls sends no SNI for an IP, exactly as
		// tls.Dialer would not.
		t.Fatalf("SNI %q for an IP-literal address", got)
	}
	if accepted.Load() != 1 {
		t.Fatalf("destination saw %d connections, want 1", accepted.Load())
	}
}

// TestAliasLetsTheServerNameTakeTheConnection: the TLS path dials the server
// name the application sent; the connection opened to the address the
// application connected to must serve that dial, with that name as SNI,
// rather than a second connection the name resolves to.
func TestAliasLetsTheServerNameTakeTheConnection(t *testing.T) {
	cert := selfSigned(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	sni := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			tc := c.(*tls.Conn)
			if err := tc.Handshake(); err == nil {
				sni <- tc.ConnectionState().ServerName
			}
			_ = c.Close()
		}
	}()
	ipAddr := l.Addr().String()
	_, port, _ := net.SplitHostPort(ipAddr)
	byName := net.JoinHostPort("localhost", port)

	pre, err := DialUpstream(context.Background(), nil, "tcp", ipAddr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(ipAddr, pre)
	p.Alias(byName)
	conn, err := DialTLS(context.Background(), nil, "tcp", byName, &tls.Config{InsecureSkipVerify: true}, p) //nolint:gosec // test server
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.NetConn() != pre {
		t.Fatal("the dial by name opened a new connection")
	}
	if got := <-sni; got != "localhost" {
		t.Fatalf("SNI %q, want localhost", got)
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("destination saw %d connections, want 1", n)
	}
	var nilP *Predialed
	nilP.Alias(byName) // no pre-dial: nothing to alias, no panic
}

// TestDialTLSInfersServerNameAsTLSDialerDoes: with no ServerName the host
// part of the address is used, as written.
func TestDialTLSInfersServerNameAsTLSDialerDoes(t *testing.T) {
	for addr, want := range map[string]string{
		"db.example.com:5432": "db.example.com",
		"[::1]:443":           "[::1]",
		"localhost":           "localhost",
	} {
		got := make(chan string, 1)
		client, server := net.Pipe()
		go func() {
			srv := tls.Server(server, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
				got <- h.ServerName
				return nil, net.ErrClosed
			}})
			_ = srv.Handshake()
			_ = server.Close()
		}()
		_, _ = DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{}, NewPredialed(addr, connseq.NewUpstream(client)))
		sn := <-got
		// crypto/tls drops an IP literal from the SNI it sends.
		if want == "[::1]" {
			want = ""
		}
		if sn != want {
			t.Errorf("addr %s: SNI %q, want %q", addr, sn, want)
		}
	}
}

// TestDialTLSHandshakeIsBoundByTheDialersTimeout: a destination that accepts
// and then never answers the handshake cannot hold the dial past the
// dialer's timeout, as with tls.Dialer.
func TestDialTLSHandshakeIsBoundByTheDialersTimeout(t *testing.T) {
	addr, _ := countingListener(t) // accepts, never speaks TLS
	start := time.Now()
	_, err := DialTLS(context.Background(), &net.Dialer{Timeout: 300 * time.Millisecond}, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, nil) //nolint:gosec // test server
	if err == nil {
		t.Fatal("handshake with a silent destination succeeded")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("handshake ran %v past a 300ms dialer timeout", took)
	}
}

// TestDialTLSRedialsWhenTheIdlePredialWasJustClosed: a destination that
// closes the pre-dialled connection as the handshake begins (it sat idle
// since the application's connect) costs a fresh dial, not the request.
func TestDialTLSRedialsWhenTheIdlePredialWasJustClosed(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				// Read the ClientHello, then close: the idle timeout firing
				// just as the handshake starts.
				_, _ = c.Read(make([]byte, 512))
				_ = c.Close()
				continue
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				time.Sleep(time.Second)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, NewPredialed(addr, pre)) //nolint:gosec // test server
	if err != nil {
		t.Fatalf("DialTLS: %v; it must redial when the pre-dialled connection was closed under it", err)
	}
	defer conn.Close()
	if conn.NetConn() == pre {
		t.Fatal("the handshake ran on the closed connection")
	}
	// The redial is a dial of the destination like any other: the relay
	// refuses a destination with no Upstream (relay.New), so the
	// application's connection would die instead of being relayed.
	if connseq.Of(conn) == nil {
		t.Fatal("the redialled connection has no connseq.Upstream under its TLS")
	}
	waitCount(t, &accepted, 2)
}

// readErrConn is a net.Conn whose Write succeeds and whose Read returns a
// fixed error, used to drive DialTLS's redial predicate deterministically.
type readErrConn struct {
	err    error
	closed atomic.Bool
}

func (c *readErrConn) Read([]byte) (int, error)         { return 0, c.err }
func (c *readErrConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *readErrConn) Close() error                     { c.closed.Store(true); return nil }
func (c *readErrConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
func (c *readErrConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }
func (c *readErrConn) SetDeadline(time.Time) error      { return nil }
func (c *readErrConn) SetReadDeadline(time.Time) error  { return nil }
func (c *readErrConn) SetWriteDeadline(time.Time) error { return nil }

// TestClosedBeforeAnswering pins which handshake errors trigger a fresh dial
// of a pre-dialled connection and which must be returned to the caller.
func TestClosedBeforeAnswering(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"wrapped eof", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, true},
		// On Windows these match only Go's synthetic APPLICATION_ERROR
		// constants; real Winsock codes are pinned by the "real peer reset"
		// subtest below and by predial_windows_test.go.
		{"ECONNRESET", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"EPIPE", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, true},

		// Exclusions: timeouts, refusals, and TLS alerts are real handshake
		// outcomes and must not silently redial.
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"context canceled", context.Canceled, false},
		{"ECONNREFUSED", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, false},
		{"tls alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, false},
		{"plain error", errors.New("handshake failed"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := closedBeforeAnswering(tc.err); got != tc.want {
				t.Fatalf("closedBeforeAnswering(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	t.Run("real peer reset", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		resetSent := make(chan struct{})
		go func() {
			defer close(resetSent)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Read(make([]byte, 64))
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
		}()

		c, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		<-resetSent

		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		var readErr error
		buf := make([]byte, 64)
		for {
			if _, readErr = c.Read(buf); readErr != nil {
				break
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, os.ErrDeadlineExceeded) {
			t.Fatalf("harness did not produce a peer reset: %v", readErr)
		}
		if !closedBeforeAnswering(readErr) {
			var errno syscall.Errno
			errors.As(readErr, &errno)
			t.Fatalf("closedBeforeAnswering(%v) = false for a real peer reset (errno=%d)", readErr, uintptr(errno))
		}
	})
}

// TestDialTLSRedialsWhenTheIdlePredialWasReset drives an explicit peer RST
// (SetLinger(0)) on the pre-dialled connection and verifies DialTLS redials
// and completes the TLS handshake over a fresh connseq.Upstream on every OS.
func TestDialTLSRedialsWhenTheIdlePredialWasReset(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Read(make([]byte, 64))
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = c.Close()
				continue
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				_, _ = io.Copy(io.Discard, tc)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, NewPredialed(addr, pre)) //nolint:gosec // test server
	if err != nil {
		t.Fatalf("DialTLS: %v; must redial after peer reset on idle pre-dial", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if conn.NetConn() == pre {
		t.Fatal("the handshake ran on the reset connection")
	}
	if connseq.Of(conn) == nil {
		t.Fatal("the redialled connection has no connseq.Upstream under its TLS")
	}
	waitCount(t, &accepted, 2)
}

// TestDialTLSRedialsWhenTheIdlePredialWasAlreadyClosed verifies DialTLS
// redials when the destination cleanly closed the pre-dialled connection
// before the handshake began.
func TestDialTLSRedialsWhenTheIdlePredialWasAlreadyClosed(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	firstClosed := make(chan struct{})
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				_ = c.Close()
				close(firstClosed)
				continue
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				_, _ = io.Copy(io.Discard, tc)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	<-firstClosed
	conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, NewPredialed(addr, pre)) //nolint:gosec // test server
	if err != nil {
		t.Fatalf("DialTLS: %v; must redial when idle pre-dial was already closed", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if conn.NetConn() == pre {
		t.Fatal("the handshake ran on the closed connection")
	}
	if connseq.Of(conn) == nil {
		t.Fatal("the redialled connection has no connseq.Upstream under its TLS")
	}
	waitCount(t, &accepted, 2)
}

// TestDialTLSDoesNotRedialOnHandshakeAlertOrTimeout verifies that DialTLS
// returns real handshake failures (timeouts and TLS alerts) without opening a
// second connection.
func TestDialTLSDoesNotRedialOnHandshakeAlertOrTimeout(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		var accepted atomic.Int32
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				accepted.Add(1)
				_ = c.Close()
			}
		}()
		addr := l.Addr().String()
		fake := &readErrConn{err: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}}
		_, err = DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, Adopt(addr, fake)) //nolint:gosec // test server
		if err == nil {
			t.Fatal("DialTLS succeeded on a timed-out pre-dialled connection")
		}
		if !fake.closed.Load() {
			t.Fatal("DialTLS leaked the failed pre-dialled connection without closing it")
		}
		if got := accepted.Load(); got != 0 {
			t.Fatalf("destination saw %d redial connections on a timeout, want 0", got)
		}
	})

	t.Run("tls alert", func(t *testing.T) {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		var accepted atomic.Int32
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				accepted.Add(1)
				go func() {
					srv := tls.Server(c, &tls.Config{
						GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
							return nil, errors.New("rejecting client")
						},
					})
					_ = srv.Handshake()
					_ = c.Close()
				}()
			}
		}()
		addr := l.Addr().String()
		pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, err = DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, NewPredialed(addr, pre)) //nolint:gosec // test server
		if err == nil {
			t.Fatal("DialTLS succeeded when server rejected the TLS handshake")
		}
		if got := accepted.Load(); got != 1 {
			t.Fatalf("destination saw %d connections after a TLS alert, want 1 (no redial)", got)
		}
	})
}

// TestDialTLSDoesNotRedialWhenNotPredialed pins the wasPre gate: a fresh
// (non-predialled) DialTLS whose handshake is reset by the destination must
// return the error without opening a second connection.
func TestDialTLSDoesNotRedialWhenNotPredialed(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Read(make([]byte, 64))
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = c.Close()
				continue
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				_, _ = io.Copy(io.Discard, tc)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()
	_, err = DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, nil) //nolint:gosec // test server
	if err == nil {
		t.Fatal("DialTLS succeeded after a non-predialled handshake was reset")
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("destination saw %d connections for a non-predialled reset, want 1 (no redial)", got)
	}
}

// TestDialTLSClosesResetPredialBeforeRedialling verifies on every OS that
// DialTLS closes the dead pre-dialled connection before opening the fresh
// upstream connection on redial.
func TestDialTLSClosesResetPredialBeforeRedialling(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fake := &readErrConn{err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	var accepted atomic.Int32
	var closedBeforeAccept atomic.Bool
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				closedBeforeAccept.Store(fake.closed.Load())
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				_, _ = io.Copy(io.Discard, tc)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()
	conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, Adopt(addr, fake)) //nolint:gosec // test server
	if err != nil {
		t.Fatalf("DialTLS: %v; must redial after ECONNRESET on pre-dialled conn", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if !fake.closed.Load() {
		t.Fatal("DialTLS leaked the reset pre-dialled connection without closing it")
	}
	if !closedBeforeAccept.Load() {
		t.Fatal("DialTLS opened the redial connection before closing the reset pre-dialled connection")
	}
	if connseq.Of(conn) == nil {
		t.Fatal("the redialled connection has no connseq.Upstream under its TLS")
	}
	waitCount(t, &accepted, 1)
}
