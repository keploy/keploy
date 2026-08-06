package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// newVerifiableTLSTestServer starts a TLS listener on 127.0.0.1 whose
// self-signed certificate is a valid trust anchor for itself and carries
// IP:127.0.0.1 as a SAN. The returned pool contains that certificate, so a
// client dialling with ServerName "127.0.0.1" and RootCAs=pool completes a
// FULLY VERIFIED handshake.
//
// Distinct from newTLSTestServer (proxy_test.go) on purpose: that one is a
// leaf-only cert with a DNS SAN and no usable trust anchor, which is the
// right fixture for "verification is off" but can only ever produce failures
// once verification is on. Proving the opt-in works needs a chain that can
// actually succeed — otherwise a test asserting "verify=true errors" would
// still pass if the flag silently did nothing but break ServerName.
//
// closeAfterHandshake makes the server hang up as soon as the handshake
// completes. hijackAndMITM's callers need that: it ends in a bidirectional
// relay that only unblocks when a side closes, so a server that lingers
// would leave the test waiting on the context timeout.
func newVerifiableTLSTestServer(t *testing.T, closeAfterHandshake bool) (net.Listener, *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "upstream.test"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		DNSNames:              []string{"upstream.test"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
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
			// Errors are the client's business (this fixture is used for
			// deliberate verification failures too); just drop the conn.
			_ = tc.Handshake()
			if closeAfterHandshake {
				_ = tc.Close()
			}
		}
	}()
	t.Cleanup(func() { _ = tlsLn.Close() })
	return tlsLn, pool
}

// TestNewProxyTLSUpgradeFn_DestSide_VerifyFlagOnSucceedsWithRoots is the
// positive half of the opt-in: with record.upstreamTls.verify on and the
// upstream's own CA supplied as the root pool, the dest-side handshake
// completes and the connection is genuinely verified (HandshakeComplete with
// InsecureSkipVerify unset on the dial config).
func TestNewProxyTLSUpgradeFn_DestSide_VerifyFlagOnSucceedsWithRoots(t *testing.T) {
	ln, pool := newVerifiableTLSTestServer(t, false)

	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), true, pool, nil)

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	caller := &tls.Config{ServerName: "127.0.0.1"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := upgrade(ctx, rawConn, true, caller)
	if err != nil {
		t.Fatalf("verified dest-side upgrade should succeed against its own CA, got: %v", err)
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("expected *tls.Conn, got %T", conn)
	}
	st := tlsConn.ConnectionState()
	if !st.HandshakeComplete {
		t.Fatalf("handshake reported incomplete")
	}
	// VerifiedChains is populated ONLY when crypto/tls actually built and
	// checked a chain — it stays empty under InsecureSkipVerify. This is the
	// assertion that distinguishes "verification ran" from "handshake
	// happened to work".
	if len(st.VerifiedChains) == 0 {
		t.Fatalf("VerifiedChains is empty: the handshake succeeded without verifying, so the flag did not reach tls.Config")
	}
}

// TestNewProxyTLSUpgradeFn_DestSide_VerifyFlagOnRejectsUntrusted is the
// negative half: the same flag must actually reject an upstream that does not
// verify. The fixture from proxy_test.go is a self-signed leaf that is not in
// any pool we pass, so a verifying dial must fail — the exact failure the
// default deliberately suppresses.
func TestNewProxyTLSUpgradeFn_DestSide_VerifyFlagOnRejectsUntrusted(t *testing.T) {
	ln, _ := newTLSTestServer(t, 0, []string{"h2", "http/1.1"}, nil)
	defer ln.Close()

	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), true, nil, nil)

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	caller := &tls.Config{
		ServerName: "wrong.example.invalid",
		NextProtos: []string{"h2", "http/1.1"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := upgrade(ctx, rawConn, true, caller)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("verify=true must not silently skip verification of an untrusted upstream")
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	if !errors.As(err, &unknownAuthority) && !errors.As(err, &hostnameErr) && !errors.As(err, &certInvalid) {
		t.Fatalf("expected an x509 verification error, got: %v", err)
	}
	// Still no mutation of the caller's config, on the verifying path too.
	if caller.InsecureSkipVerify {
		t.Fatalf("caller cfg.InsecureSkipVerify was mutated")
	}
}

// TestNewProxyTLSUpgradeFn_DestSide_VerifyFillsEmptyServerName pins the
// ServerName fallback on the V2 relay path.
//
// buildDestTLSConfigV2 leaves ServerName empty whenever the session has no
// DstCfg address, and crypto/tls rejects an empty ServerName with
// verification on ("either ServerName or InsecureSkipVerify must be
// specified") BEFORE it looks at a certificate. Without the fallback this
// dial cannot even be attempted, so the assertion that it SUCCEEDS — against
// a server whose cert carries IP:127.0.0.1 — is what proves ServerName was
// backfilled from the peer address rather than left empty.
func TestNewProxyTLSUpgradeFn_DestSide_VerifyFillsEmptyServerName(t *testing.T) {
	ln, pool := newVerifiableTLSTestServer(t, false)

	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), true, pool, nil)

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	caller := &tls.Config{} // no ServerName at all

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := upgrade(ctx, rawConn, true, caller)
	if err != nil {
		if strings.Contains(err.Error(), "either ServerName or InsecureSkipVerify") {
			t.Fatalf("empty ServerName was not backfilled — the verify flag is unusable on no-SNI destinations: %v", err)
		}
		t.Fatalf("verified upgrade with empty ServerName failed: %v", err)
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("expected *tls.Conn, got %T", conn)
	}
	if got := tlsConn.ConnectionState().ServerName; got != "" {
		// crypto/tls omits SNI for IP literals, so the negotiated
		// ServerName stays empty on the wire; the fallback is only there to
		// give the verifier a name to check. Assert the wire shape so a
		// future change that starts leaking an SNI is caught.
		t.Fatalf("expected no SNI on the wire for an IP-literal ServerName, got %q", got)
	}
	if len(tlsConn.ConnectionState().VerifiedChains) == 0 {
		t.Fatalf("VerifiedChains is empty: the fallback let the handshake through without verifying")
	}
	if caller.ServerName != "" {
		t.Fatalf("caller cfg.ServerName was mutated to %q; the fallback must only touch the clone", caller.ServerName)
	}
}

// TestNewProxyTLSUpgradeFn_DestSide_VerifyOffLeavesServerNameEmpty is the
// byte-identical-default guarantee for the V2 relay path: with the flag off,
// an empty caller ServerName must stay empty — no backfill, no SNI the
// application never sent.
func TestNewProxyTLSUpgradeFn_DestSide_VerifyOffLeavesServerNameEmpty(t *testing.T) {
	ln, pool := newVerifiableTLSTestServer(t, false)

	// Pool supplied but verify off: neither the pool nor the ServerName
	// fallback may take effect.
	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), false, pool, nil)

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	caller := &tls.Config{}

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
}

// runHijackAndMITM drives the opportunistic-TLS hijack end to end against a
// real upstream: a real TLS client dials keploy's MITM listener, keploy
// terminates that handshake with its own CA-minted cert, then dials the
// upstream itself with the config under test.
//
// clientSNI is the ServerName the simulated APPLICATION puts in its
// ClientHello to keploy. It is what hijackAndMITM must reuse for its own
// upstream dial once verification is on, so it is a parameter rather than a
// constant.
//
// Returns hijackAndMITM's error. nil means BOTH handshakes completed and the
// relay ran to a clean close — i.e. the upstream leg was accepted.
func runHijackAndMITM(t *testing.T, upstreamAddr, clientSNI string, opts models.OutgoingOptions) error {
	t.Helper()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for MITM client side: %v", err)
	}
	defer clientLn.Close()

	clientDone := make(chan error, 1)
	go func() {
		// The app trusts keploy's MITM cert (in production keploy installs
		// its CA into the app's trust store); skipping here keeps the test
		// about the UPSTREAM leg, which is what changed.
		c, derr := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // client-facing leg: keploy's own MITM cert, not the subject of this test
			ServerName:         clientSNI,
		})
		if derr != nil {
			clientDone <- derr
			return
		}
		// Close immediately so the cleartext relay has an EOF to finish on.
		_ = c.Close()
		clientDone <- nil
	}()

	srcConn, err := clientLn.Accept()
	if err != nil {
		t.Fatalf("accept MITM client side: %v", err)
	}
	dstConn, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}

	p := &Proxy{logger: zap.NewNop()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// bufferedClientHello is empty on purpose: hijackAndMITM only prepends it
	// to the src reader, so letting tls.Server read the ClientHello straight
	// off the socket is equivalent and keeps the fixture simple.
	hijackErr := p.hijackAndMITM(ctx, srcConn, dstConn, nil, upstreamAddr, time.Time{}, opts)

	if cerr := <-clientDone; cerr != nil {
		t.Fatalf("client-facing MITM handshake failed (test fixture problem, not the upstream leg): %v", cerr)
	}
	return hijackErr
}

// TestHijackAndMITM_UpstreamVerification pins the opportunistic-TLS upstream
// dial — the fourth InsecureSkipVerify site, the one CodeQL never flagged.
//
// This is a site-level test, not a config-shape test: it asserts on the
// outcome of a real handshake against a real server, so it fails if the flag
// stops reaching tls.Config for any reason.
//
// Every case here drives the client with NO SNI, which is what an application
// dialling an IP literal actually sends (RFC 6066 forbids IP literals in SNI),
// so these cases exercise the dial-address fallback. The companion
// TestHijackAndMITM_VerifyUsesApplicationSNI in upstream_tls_sni_test.go
// covers the other branch — an application that DID send a name.
func TestHijackAndMITM_UpstreamVerification(t *testing.T) {
	t.Run("default accepts an untrusted upstream", func(t *testing.T) {
		// The byte-identical guarantee for this site: the zero-value
		// OutgoingOptions (what every existing user has) must still complete
		// against a self-signed upstream that no pool vouches for.
		ln, _ := newVerifiableTLSTestServer(t, true)
		if err := runHijackAndMITM(t, ln.Addr().String(), "", models.OutgoingOptions{}); err != nil {
			t.Fatalf("default must accept an unverifiable upstream, got: %v", err)
		}
	})

	t.Run("verify on rejects an untrusted upstream", func(t *testing.T) {
		ln, _ := newVerifiableTLSTestServer(t, true)
		err := runHijackAndMITM(t, ln.Addr().String(), "", models.OutgoingOptions{
			UpstreamTLSVerify: true, // no roots: the upstream's CA is unknown
		})
		if err == nil {
			t.Fatalf("verify=true must reject an upstream signed by an unknown authority")
		}
		if !strings.Contains(err.Error(), "upstream handshake to") {
			t.Fatalf("expected the upstream-handshake error, got: %v", err)
		}
		var unknownAuthority x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthority) {
			t.Fatalf("expected x509.UnknownAuthorityError, got: %v", err)
		}
	})

	t.Run("verify on accepts an upstream in the configured roots", func(t *testing.T) {
		// The application sent no SNI, so ServerName falls back to
		// hostFromAddr(dstAddr) == "127.0.0.1" and the upstream cert's
		// IP:127.0.0.1 SAN is what validates it — the IP-SAN path.
		ln, pool := newVerifiableTLSTestServer(t, true)
		err := runHijackAndMITM(t, ln.Addr().String(), "", models.OutgoingOptions{
			UpstreamTLSVerify:  true,
			UpstreamTLSRootCAs: pool,
		})
		if err != nil {
			t.Fatalf("verify=true with the upstream's own CA must succeed, got: %v", err)
		}
	})
}

// TestResolveUpstreamServerName covers the ServerName decision for the main
// upstream dial in handleConnection.
//
// The verify=false rows are the load-bearing ones: every one of them must
// reproduce exactly what the pre-upstreamTls code produced (captured SNI, else
// the non-IP CONNECT authority, else empty). The verify=true rows are the new
// behaviour that makes the opt-in usable at all.
func TestResolveUpstreamServerName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		capturedSNI       string
		connectTargetHost string
		dstAddr           string
		verify            bool
		want              string
	}{
		// --- verify OFF: must match the old behaviour exactly ---
		{"captured SNI wins", "api.example.com", "", "10.0.0.5:443", false, "api.example.com"},
		{"captured SNI wins over CONNECT authority", "api.example.com", "other.example.com", "10.0.0.5:443", false, "api.example.com"},
		{"no SNI falls back to CONNECT hostname", "", "api.example.com", "10.0.0.5:443", false, "api.example.com"},
		{"no SNI, IP-literal CONNECT target stays empty", "", "10.0.0.5", "10.0.0.5:443", false, ""},
		{"no SNI, no CONNECT stays empty", "", "", "10.0.0.5:443", false, ""},
		{"no SNI, empty CONNECT host stays empty", "", "", "10.0.0.5:443", false, ""},

		// --- verify ON: the fallback that makes the flag usable ---
		{"captured SNI still wins", "api.example.com", "", "10.0.0.5:443", true, "api.example.com"},
		{"CONNECT hostname still wins over the dial address", "", "api.example.com", "10.0.0.5:443", true, "api.example.com"},
		// The transparent IP-literal path — the dominant DB shape, and the
		// one that hard-errors in crypto/tls when ServerName is empty.
		{"no SNI falls back to the dialled IPv4", "", "", "10.0.0.5:443", true, "10.0.0.5"},
		{"no SNI falls back to the dialled IPv6", "", "", "[::1]:5432", true, "::1"},
		// CONNECT to an IP literal: the CONNECT branch declines it, so the
		// dial-address fallback is what rescues verification here.
		{"IP-literal CONNECT target falls through to the dial address", "", "10.0.0.5", "10.0.0.5:443", true, "10.0.0.5"},
		{"address without a port is used verbatim", "", "", "10.0.0.5", true, "10.0.0.5"},
		// A host-less authority (`CONNECT :443`) — handleConnectTunnel now
		// rejects it at the source, but if one ever reaches here it must NOT
		// come back empty: crypto/tls would refuse the config before looking
		// at a certificate, which reads as "keploy is broken" rather than
		// "that target is wrong". The address verbatim fails loudly instead,
		// with an x509 hostname error naming it.
		{"host-less authority is used verbatim rather than emptied", "", "", ":443", true, ":443"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveUpstreamServerName(tc.capturedSNI, tc.connectTargetHost, tc.dstAddr, tc.verify)
			if got != tc.want {
				t.Fatalf("resolveUpstreamServerName(%q, %q, %q, %v) = %q; want %q",
					tc.capturedSNI, tc.connectTargetHost, tc.dstAddr, tc.verify, got, tc.want)
			}
		})
	}
}

// TestResolveUpstreamServerName_NeverEmptyWhenVerifying is the invariant the
// whole plumbing fix exists to hold: with verification on and a dial address
// in hand, this function must never give crypto/tls an empty ServerName,
// because crypto/tls rejects that config outright before examining any
// certificate.
//
// Stated as a property over the input space rather than as more rows in the
// table above, so an input shape nobody thought to add a row for still trips
// it.
//
// The one input that can still yield "" is an empty dstAddr, and it is
// excluded deliberately, not overlooked: dstAddr is empty only when
// destInfo.Version was neither 4 nor 6, in which case the subsequent
// net.Dial("tcp", "") fails regardless and there is no host anywhere in scope
// to derive a name from. That connection is already lost; the flag does not
// make it more so.
func TestResolveUpstreamServerName_NeverEmptyWhenVerifying(t *testing.T) {
	t.Parallel()

	snis := []string{"", "api.example.com"}
	connectHosts := []string{"", "10.0.0.5", "api.example.com"}
	dstAddrs := []string{"10.0.0.5:443", "[::1]:5432", "10.0.0.5", "db.internal:3306", ":443"}

	for _, sni := range snis {
		for _, ch := range connectHosts {
			for _, addr := range dstAddrs {
				if got := resolveUpstreamServerName(sni, ch, addr, true); got == "" {
					t.Fatalf("resolveUpstreamServerName(%q, %q, %q, true) returned an empty ServerName; crypto/tls would reject the config with %q",
						sni, ch, addr, "either ServerName or InsecureSkipVerify must be specified")
				}
			}
		}
	}
}

// TestHostFromConn pins the nil-safety of the last-resort ServerName source.
// Both callers feed the result straight into tls.Config.ServerName, so a
// panic here would take down a recording session on a connection that had
// already closed underneath us.
func TestHostFromConn(t *testing.T) {
	t.Parallel()

	if got := hostFromConn(nil); got != "" {
		t.Fatalf("hostFromConn(nil) = %q; want empty", got)
	}
	if got := hostFromConn(nilAddrConn{}); got != "" {
		t.Fatalf("hostFromConn(nil RemoteAddr) = %q; want empty", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got := hostFromConn(c); got != "127.0.0.1" {
		t.Fatalf("hostFromConn(loopback conn) = %q; want 127.0.0.1", got)
	}
}

// nilAddrConn is a net.Conn whose RemoteAddr is nil — the shape a closed or
// half-constructed connection can present.
type nilAddrConn struct{ net.Conn }

func (nilAddrConn) RemoteAddr() net.Addr { return nil }
