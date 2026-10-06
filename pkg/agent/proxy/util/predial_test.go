package util

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
	"sync/atomic"
	"testing"
	"time"
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
	pre, err := net.Dial("tcp", addr)
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
	pre, err := net.Dial("tcp", addr)
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
	pre, err := net.Dial("tcp", addr)
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
	pre, err := net.Dial("tcp", addr)
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

	pre, err := net.Dial("tcp", ipAddr)
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
		_, _ = DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{}, NewPredialed(addr, client))
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
	pre, err := net.Dial("tcp", addr)
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
	waitCount(t, &accepted, 2)
}
