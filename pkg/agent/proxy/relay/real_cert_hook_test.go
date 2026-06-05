package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestPublishRealCertFromUpgraded_FiresWithExpectedArgs verifies the
// RealCertHook invocation path that the V2 relay's handleUpgradeTLS
// uses (publishRealCertFromUpgraded → r.cfg.RealCertHook). The
// security-sensitive bit is that the cbshim rendezvous matches the
// client's source port and the actual upstream leaf cert DER —
// without this coverage, a refactor that, say, hashed the cert or
// keyed on a different addr field would compile + pass every other
// relay test but silently break SCRAM-PLUS for postgres-over-MITM.
//
// The test stages a real loopback TLS handshake (so the dest side
// has a populated PeerCertificates state), wires that as the
// "upgraded" net.Conn, gives the relay a src with a known source
// port, and asserts the hook fires with the expected arguments.
func TestPublishRealCertFromUpgraded_FiresWithExpectedArgs(t *testing.T) {
	const wantSourcePort = 54321
	serverCert, serverKey, rootPool := mkSelfSigned(t, "postgres-test")

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tls.Certificate{
			Certificate: [][]byte{serverCert.Raw},
			PrivateKey:  serverKey,
			Leaf:        serverCert,
		}},
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Server-side accept goroutine: complete the handshake then
	// idle until the client closes. The server-side conn isn't
	// what we test against — we want the CLIENT-side *tls.Conn,
	// which holds the dst-cert PeerCertificates after handshake.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		if tc, ok := c.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		// Hold the conn open until the client closes.
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
	}()

	// Client-side handshake against our loopback server. clientConn
	// is the *tls.Conn we'll pass to publishRealCertFromUpgraded as
	// the "upgraded" dest conn.
	clientConn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    rootPool,
		ServerName: "postgres-test",
	})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close(); <-serverDone })
	if err := clientConn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}

	// Build a minimal Relay with the hook installed + a src whose
	// RemoteAddr returns our chosen source port. The hook captures
	// arguments under a mutex so the test can assert deterministically.
	type capture struct {
		connID  string
		der     []byte
		sigAlgo x509.SignatureAlgorithm
	}
	var got capture
	var fired atomic.Int32
	var mu sync.Mutex
	r := &Relay{
		cfg: Config{
			RealCertHook: func(connID string, realCertDER []byte, sigAlgo x509.SignatureAlgorithm) {
				mu.Lock()
				defer mu.Unlock()
				got = capture{
					connID:  connID,
					der:     append([]byte(nil), realCertDER...),
					sigAlgo: sigAlgo,
				}
				fired.Add(1)
			},
		},
	}
	src := net.Conn(fakeSrcWithPort{port: wantSourcePort})
	r.src.Store(&src)

	publishRealCertFromUpgraded(r, clientConn, zap.NewNop())

	if got := fired.Load(); got != 1 {
		t.Fatalf("RealCertHook fired %d times, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.connID != strconv.Itoa(wantSourcePort) {
		t.Errorf("connID = %q, want %q", got.connID, strconv.Itoa(wantSourcePort))
	}
	if string(got.der) != string(serverCert.Raw) {
		t.Errorf("realCertDER does not equal the server's leaf DER (got %d bytes, want %d)",
			len(got.der), len(serverCert.Raw))
	}
	if got.sigAlgo != serverCert.SignatureAlgorithm {
		t.Errorf("sigAlgo = %v, want %v", got.sigAlgo, serverCert.SignatureAlgorithm)
	}
}

// TestPublishRealCertFromUpgraded_NoCallWhenHookNil verifies the
// nil-safe early-return when no hook is installed. Pinned coverage
// for the "operator never enables the shim" path — the existing
// UpgradeTLS tests are all hook-less so a regression that
// dereferenced a nil RealCertHook would surface here first.
func TestPublishRealCertFromUpgraded_NoCallWhenHookNil(t *testing.T) {
	r := &Relay{cfg: Config{}} // RealCertHook left nil
	src := net.Conn(fakeSrcWithPort{port: 1234})
	r.src.Store(&src)
	// Even with no TLS conn, the function must early-return cleanly.
	publishRealCertFromUpgraded(r, nil, zap.NewNop())
	publishRealCertFromUpgraded(r, &net.IPConn{}, zap.NewNop()) // non-TLS conn
	// No panic, no hook call — that's the assertion.
}

// fakeSrcWithPort is a minimal net.Conn implementation that satisfies
// the RemoteAddr() *net.TCPAddr type assertion publishRealCertFromUpgraded
// performs. Every other Conn method is a no-op (the function never
// reads/writes the src — only its RemoteAddr is consulted).
type fakeSrcWithPort struct {
	port int
}

func (f fakeSrcWithPort) Read(_ []byte) (int, error)  { return 0, nil }
func (f fakeSrcWithPort) Write(_ []byte) (int, error) { return 0, nil }
func (f fakeSrcWithPort) Close() error                { return nil }
func (f fakeSrcWithPort) LocalAddr() net.Addr         { return &net.TCPAddr{Port: 9999} }
func (f fakeSrcWithPort) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.port}
}
func (f fakeSrcWithPort) SetDeadline(_ time.Time) error      { return nil }
func (f fakeSrcWithPort) SetReadDeadline(_ time.Time) error  { return nil }
func (f fakeSrcWithPort) SetWriteDeadline(_ time.Time) error { return nil }

// mkSelfSigned mints a throwaway self-signed cert + private key for
// the test's loopback TLS server. Returns the parsed *x509.Certificate
// (so the test can compare DER + sig algo), the matching private key,
// and a *x509.CertPool the client side trusts.
func mkSelfSigned(t *testing.T, commonName string) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{commonName},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return cert, key, pool
}
