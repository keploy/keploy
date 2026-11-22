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
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// upstreamSNIProbe records the ServerName of every ClientHello the fixture
// upstream receives, so a test can assert on what keploy actually put on the
// wire rather than on what it meant to.
type upstreamSNIProbe struct {
	mu   sync.Mutex
	seen []string
}

func (p *upstreamSNIProbe) record(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, name)
}

// last returns the SNI of the most recent ClientHello, waiting briefly for one
// to arrive (the fixture's accept loop runs on its own goroutine).
func (p *upstreamSNIProbe) last(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.seen)
		var v string
		if n > 0 {
			v = p.seen[n-1]
		}
		p.mu.Unlock()
		if n > 0 {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream never received a ClientHello")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newDNSOnlySNIRecordingTLSTestServer starts a TLS listener on 127.0.0.1 whose
// self-signed (CA-of-itself) certificate carries ONLY the DNS SAN
// "upstream.test" — deliberately NO IP SAN — and records every ClientHello's
// SNI.
//
// The missing IP SAN is the whole point. newVerifiableTLSTestServer's cert
// carries IP:127.0.0.1, which makes "verify against the dial address" succeed
// by accident and hides every ServerName-plumbing defect. This is the fixture
// that models the real world: a hostname-addressed upstream, whose certificate
// can only be validated against the name the APPLICATION asked for. It is also
// the shape the CI upstream cert now has (quote/echo.keploy.local, no IP SAN).
//
// closeAfterHandshake makes the server hang up as soon as the handshake
// completes, which hijackAndMITM's bidirectional relay needs to unblock.
func newDNSOnlySNIRecordingTLSTestServer(t *testing.T, closeAfterHandshake bool) (net.Listener, *x509.CertPool, *upstreamSNIProbe) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "upstream.test"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		DNSNames:              []string{"upstream.test"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	probe := &upstreamSNIProbe{}
	cert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: priv}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			probe.record(hello.ServerName)
			return &cert, nil
		},
	})
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
			// Errors are the client's business — this fixture is used for
			// deliberate verification failures too.
			_ = tc.Handshake()
			if closeAfterHandshake {
				_ = tc.Close()
			}
		}
	}()
	t.Cleanup(func() { _ = tlsLn.Close() })
	return tlsLn, pool, probe
}

// TestHijackAndMITM_VerifyUsesApplicationSNI is the regression test for the
// opportunistic-TLS upstream dial hard-coding its ServerName to the
// destination IP literal.
//
// The upstream is reached at 127.0.0.1:<port> — dstAddr on this path is ALWAYS
// an ip:port built from destInfo — but its certificate carries only
// DNS:upstream.test, which is what any hostname-addressed upstream looks like.
// The application sends SNI "upstream.test", and hijackAndMITM has that value
// in hand the moment its client-facing handshake completes.
//
// FAILS BEFORE THE FIX: the site used serverName := hostFromAddr(dstAddr)
// unconditionally, so verification ran against "127.0.0.1" and crypto/tls
// returned `cannot validate certificate for 127.0.0.1 because it doesn't
// contain any IP SANs`. Worse than a dropped mock here — hijackAndMITM defers
// srcConn.Close()/dstConn.Close(), so the application's own socket dies after
// it had already completed its handshake with keploy.
func TestHijackAndMITM_VerifyUsesApplicationSNI(t *testing.T) {
	ln, pool, probe := newDNSOnlySNIRecordingTLSTestServer(t, true)

	err := runHijackAndMITM(t, ln.Addr().String(), "upstream.test", models.OutgoingOptions{
		UpstreamTLSVerify:  true,
		UpstreamTLSRootCAs: pool,
	})
	if err != nil {
		t.Fatalf("verify=true against a DNS-SAN-only upstream must reuse the application's SNI and succeed, got: %v", err)
	}
	if got := probe.last(t); got != "upstream.test" {
		t.Fatalf("upstream saw SNI %q; want %q — the dial used the destination address instead of the SNI the application sent", got, "upstream.test")
	}
}

// TestHijackAndMITM_DefaultKeepsDialAddressServerName is the default-off
// fidelity guarantee for the same site, and the reason the SNI preference is
// gated on the flag rather than applied unconditionally.
//
// With verification off, ServerName's only remaining effect is the SNI
// extension on the wire. The application here talks to keploy with SNI
// "upstream.test", but on the transparent path it would have talked to the
// real server at an IP literal and sent NO SNI at all. keploy must not invent
// one on its behalf: the site keeps hostFromAddr(dstAddr) — an IP literal, for
// which crypto/tls omits SNI entirely — exactly as it always has.
//
// FAILS if the captured-SNI preference is ever applied outside the verifying
// path: the upstream would then observe "upstream.test" where it observes
// nothing today.
func TestHijackAndMITM_DefaultKeepsDialAddressServerName(t *testing.T) {
	ln, _, probe := newDNSOnlySNIRecordingTLSTestServer(t, true)

	// Zero-value OutgoingOptions — what every existing user has.
	if err := runHijackAndMITM(t, ln.Addr().String(), "upstream.test", models.OutgoingOptions{}); err != nil {
		t.Fatalf("default must accept an unverifiable upstream, got: %v", err)
	}
	if got := probe.last(t); got != "" {
		t.Fatalf("upstream saw SNI %q with verification OFF; want none — the default path must stay byte-identical and never send an SNI the application did not send to the real server", got)
	}
}

// upgradeAfterClientHandshake is newProxyTLSUpgradeFn's fn for one connection
// once it has served that connection's client-side handshake, in which the
// application sent sni ("" for none) under ctx: the order the relay runs the
// two in with verification on (relay.Config.ClientTLSFirst).
func upgradeAfterClientHandshake(t *testing.T, ctx context.Context, verify bool, pool *x509.CertPool, sni string) relay.TLSUpgradeFn {
	t.Helper()
	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), verify, pool)
	app, keploySide := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = keploySide.Close() })
	appErr := make(chan error, 1)
	go func() {
		appErr <- tls.Client(app, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake() //nolint:gosec // the application under test trusts keploy's CA
	}()
	if _, err := upgrade(ctx, keploySide, false, &tls.Config{}); err != nil {
		t.Fatalf("client-side upgrade: %v", err)
	}
	if err := <-appErr; err != nil {
		t.Fatalf("the application's handshake with keploy: %v", err)
	}
	return upgrade
}

// TestNewProxyTLSUpgradeFn_DestSide_VerifyPrefersCapturedSNI is the regression
// test for the relay-path ServerName only being backfilled when EMPTY.
//
// The caller cfg here is byte-for-byte what mysql/recorder.buildDestTLSConfigV2
// produces on the live V2 path: ServerName is the host of Opts.DstCfg.Addr,
// which proxy.handleConnection always sets to the `ip:port` eBPF reported. It
// is non-empty and WRONG for every hostname DSN.
//
// FAILS BEFORE THE FIX: the fallback was `if dialCfg.ServerName == ""`, so a
// non-empty-but-wrong "127.0.0.1" was left in place and the handshake failed
// with `doesn't contain any IP SANs`. On this path that trips the supervisor's
// FallthroughToPassthrough — the app keeps working and the mock vanishes with
// only a Debug log, which is precisely the silent loss the feature exists to
// avoid.
func TestNewProxyTLSUpgradeFn_DestSide_VerifyPrefersCapturedSNI(t *testing.T) {
	ln, pool, probe := newDNSOnlySNIRecordingTLSTestServer(t, false)
	upgrade := upgradeAfterClientHandshake(t, context.Background(), true, pool, "upstream.test")

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	// Exactly what buildDestTLSConfigV2 builds for DstCfg.Addr "127.0.0.1:3306".
	caller := &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := upgrade(ctx, rawConn, true, caller)
	if err != nil {
		if strings.Contains(err.Error(), "IP SANs") {
			t.Fatalf("the parser-supplied IP-literal ServerName was not overridden by the application's SNI: %v", err)
		}
		t.Fatalf("verified dest-side upgrade failed: %v", err)
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("expected *tls.Conn, got %T", conn)
	}
	if len(tlsConn.ConnectionState().VerifiedChains) == 0 {
		t.Fatalf("VerifiedChains is empty: the handshake succeeded without verifying")
	}
	if got := probe.last(t); got != "upstream.test" {
		t.Fatalf("upstream saw SNI %q; want %q", got, "upstream.test")
	}
	if caller.ServerName != "127.0.0.1" {
		t.Fatalf("caller cfg.ServerName was mutated to %q; the override must only touch the clone", caller.ServerName)
	}
}

// TestNewProxyTLSUpgradeFn_DestSide_DefaultIgnoresCapturedSNI is the
// default-off fidelity guarantee for the relay path: with verification off the
// parser's ServerName is what reaches the wire, captured SNI or not.
//
// FAILS if the captured-SNI preference is applied outside the verifying path:
// the upstream would observe "upstream.test" instead of nothing (crypto/tls
// omits SNI for the IP-literal ServerName the parser supplied).
func TestNewProxyTLSUpgradeFn_DestSide_DefaultIgnoresCapturedSNI(t *testing.T) {
	ln, pool, probe := newDNSOnlySNIRecordingTLSTestServer(t, false)
	// Pool supplied but verify off: neither the pool nor the SNI override may
	// take effect.
	upgrade := upgradeAfterClientHandshake(t, context.Background(), false, pool, "upstream.test")

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	caller := &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := upgrade(ctx, rawConn, true, caller)
	if err != nil {
		t.Fatalf("default (verify off) upgrade must succeed: %v", err)
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("expected *tls.Conn, got %T", conn)
	}
	if len(tlsConn.ConnectionState().VerifiedChains) != 0 {
		t.Fatalf("verification ran with the flag off — the default is no longer byte-identical")
	}
	if got := probe.last(t); got != "" {
		t.Fatalf("upstream saw SNI %q with verification OFF; want none — the default must send exactly what the parser's cfg asked for", got)
	}
}

// TestNewProxyTLSUpgradeFn_DestSide_WithoutSNITheConnectTargetNamesTheDial:
// the name the verified dial prefers is the one this connection's application
// asked for, from its own handshake and context. A connection whose
// application sent no SNI and made no CONNECT dials with the parser's name; one
// that made a CONNECT request for "upstream.test" dials with the target.
func TestNewProxyTLSUpgradeFn_DestSide_WithoutSNITheConnectTargetNamesTheDial(t *testing.T) {
	ln, pool, probe := newDNSOnlySNIRecordingTLSTestServer(t, false)
	dial := func(ctx context.Context, upgrade relay.TLSUpgradeFn) error {
		t.Helper()
		rawConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial upstream: %v", err)
		}
		defer func() { _ = rawConn.Close() }()
		conn, err := upgrade(ctx, rawConn, true, &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		if err == nil {
			_ = conn.Close()
		}
		return err
	}

	noName := upgradeAfterClientHandshake(t, context.Background(), true, pool, "")
	err := dial(context.Background(), noName)
	if got := probe.last(t); got != "" {
		t.Fatalf("a connection whose application asked for no name dialled with SNI %q; want the parser's IP literal, which puts none on the wire", got)
	}
	if err == nil || !strings.Contains(err.Error(), "IP SANs") {
		t.Fatalf("a connection whose application asked for no name verified the upstream against a name it never asked for (err %v)", err)
	}

	ctx := pTls.WithConnectTarget(context.Background(), "upstream.test")
	connected := upgradeAfterClientHandshake(t, ctx, true, pool, "")
	if err := dial(ctx, connected); err != nil {
		t.Fatalf("a connection that made a CONNECT request for upstream.test, with no SNI: verified dial failed: %v", err)
	}
	if got := probe.last(t); got != "upstream.test" {
		t.Fatalf("a connection that made a CONNECT request for upstream.test dialled with SNI %q", got)
	}
}
