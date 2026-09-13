package util

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// requireIPv6 reports whether this environment can exercise the IPv6 arm.
//
// A container without ::1 on lo would otherwise SKIP both fallback cases and
// still report the package ok — a suite that tests nothing while looking
// healthy. KEPLOY_TEST_IPV6_REQUIRED=1 turns that into a hard failure, matching
// the KEPLOY_TEST_MYSQL_REQUIRED gate this repo already uses in go-test.yaml
// for exactly this reason.
func requireIPv6(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("KEPLOY_TEST_IPV6_REQUIRED") == "1" {
		t.Fatalf("KEPLOY_TEST_IPV6_REQUIRED=1 but %s", reason)
	}
	t.Skip(reason)
}

func listenLoopback(t *testing.T, addr string) (ln net.Listener, otherFamily string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		requireIPv6(t, "cannot listen on "+addr+": "+err.Error())
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", ln.Addr(), err)
	}
	if strings.HasPrefix(addr, "[") {
		return ln, net.JoinHostPort("127.0.0.1", port)
	}
	return ln, net.JoinHostPort("::1", port)
}

// requireRefused asserts the precondition every fallback test depends on: the
// family the application asked for is REFUSED. Without it a test could pass on
// a host serving both families, proving nothing.
func requireRefused(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		_ = c.Close()
		// Not an IPv6 problem: something else holds the mirrored ephemeral
		// port, so skip plainly rather than sending someone hunting IPv6.
		t.Skipf("%s is already served here, so the fallback path is not exercised", addr)
	}
	if !neterr.IsConnRefused(err) {
		requireIPv6(t, "dial to "+addr+" failed with "+err.Error()+", not ECONNREFUSED")
	}
}

// The defect: interception makes the application's connect to the wrong family
// SUCCEED, so it never performs the fallback that would have saved it. If the
// proxy then dials that family literally and gives up, the application gets EOF
// — indistinguishable from a server that accepted and hung up — and drivers
// report a dead connection instead of retrying.
func TestDialDestination_FallsBackToTheOtherLoopbackFamily(t *testing.T) {
	for _, tc := range []struct{ name, listen string }{
		{"upstream is IPv4-only, app asked for IPv6", "127.0.0.1:0"},
		{"upstream is IPv6-only, app asked for IPv4", "[::1]:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, requested := listenLoopback(t, tc.listen)
			requireRefused(t, requested)

			loopbackFallbackReported.Store(false)
			core, logs := observer.New(zap.InfoLevel)
			conn, err := DialDestination(context.Background(), zap.New(core), "tcp", DialTarget{Addr: requested})
			if err != nil {
				t.Fatalf("DialDestination(%s) = %v; the application would see EOF instead of the "+
					"connection refused it needs to fall back, and cannot recover", requested, err)
			}
			_ = conn.Close()

			if n := len(logs.FilterMessageSnippet("one loopback address family").All()); n != 1 {
				t.Errorf("fallback reported %d times, want 1 — a silent fallback hides a real "+
					"topology problem from the operator", n)
			}
		})
	}
}

// The notice is once per PROCESS, not once per connection: it describes the
// node's topology, so on the setup where it fires every connection would fire
// it. This is the entire reason the flag is a package-level atomic.
func TestDialDestination_ReportsTheFallbackOncePerProcess(t *testing.T) {
	_, requested := listenLoopback(t, "127.0.0.1:0")
	requireRefused(t, requested)

	loopbackFallbackReported.Store(false)
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	for i := 0; i < 3; i++ {
		conn, err := DialDestination(context.Background(), logger, "tcp", DialTarget{Addr: requested})
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = conn.Close()
	}
	if n := len(logs.FilterMessageSnippet("one loopback address family").All()); n != 1 {
		t.Fatalf("three fallbacks logged %d lines, want 1 — per-connection noise on exactly the "+
			"node where every connection takes this path", n)
	}
}

// Only a REFUSAL means nothing is listening on the requested family. A timeout
// is a different condition — a real but slow upstream — and redirecting it to
// the other family would silently move the application to another server.
// Without this the change is a general-purpose retry, not a family fallback.
func TestDialDestination_OnlyFallsBackOnConnectionRefused(t *testing.T) {
	// Fully mocked dial: no listener and no IPv6 needed, so this must never be
	// one of the tests a v6-less container silently skips.
	const requested = "127.0.0.1:1"
	var attempts []string
	var mu sync.Mutex
	_, err := DialDestinationWith(context.Background(), zap.NewNop(), DialTarget{Addr: requested},
		func(_ context.Context, addr string) (net.Conn, error) {
			mu.Lock()
			attempts = append(attempts, addr)
			mu.Unlock()
			return nil, syscall.ETIMEDOUT
		})
	if err == nil {
		t.Fatal("a timing-out dial succeeded")
	}
	if len(attempts) != 1 {
		t.Fatalf("dial attempted %v; a non-refusal must not be retried on the other family", attempts)
	}
}

// A fabricated address is a capture-layer stand-in that models.ConditionalDstCfg
// says must not be dialed at all. Inferring a counterpart for it turns the
// documented SAFE outcome (refused, capture aborts loudly) into the documented
// DANGEROUS one (silently record an unrelated local server as the dependency).
func TestDialDestination_NeverInfersACounterpartForAFabricatedAddress(t *testing.T) {
	const requested = "127.0.0.1:1" // mocked dial; no listener, no IPv6
	var attempts []string
	_, err := DialDestinationWith(context.Background(), zap.NewNop(),
		DialTarget{Addr: requested, Fabricated: true},
		func(_ context.Context, addr string) (net.Conn, error) {
			attempts = append(attempts, addr)
			return nil, syscall.ECONNREFUSED
		})
	if err == nil {
		t.Fatal("dial to a fabricated address succeeded")
	}
	if len(attempts) != 1 {
		t.Fatalf("a fabricated stand-in was retried on the other family (%v) — that is how an "+
			"unrelated local server gets recorded as the dependency", attempts)
	}
}

// The fallback must stay a loopback rule. 127.0.0.2 is IsLoopback but a
// DISTINCT host: dialing it does not reach a 127.0.0.1 listener, so treating
// the two as interchangeable would connect an application to another server —
// the shape you get from several database instances on 127.0.0.1/.2/.3.
func TestLoopbackCounterpart_OnlyTheExactLoopbackAddresses(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
	}{
		{"127.0.0.1:3306", "[::1]:3306"},
		{"[::1]:3306", "127.0.0.1:3306"},
		{"[::ffff:127.0.0.1]:3306", "[::1]:3306"}, // 4-in-6 reaches an IPv4 listener
		{"127.0.0.2:3306", ""},                    // loopback, but a different host
		{"127.0.0.53:3306", ""},                   // systemd-resolved
		{"192.0.2.1:3306", ""},
		{"0.0.0.0:3306", ""},
		{"[::]:3306", ""},
		{"localhost:3306", ""}, // a name: the stdlib does its own fallback
		{"[fe80::1%lo]:3306", ""},
		{"not-an-address", ""},
	} {
		got, ok := loopbackCounterpart(tc.addr)
		if tc.want == "" {
			if ok {
				t.Errorf("loopbackCounterpart(%q) = %q, want no counterpart — inferring one would "+
					"dial a host the application never asked for", tc.addr, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("loopbackCounterpart(%q) = (%q,%v), want (%q,true)", tc.addr, got, ok, tc.want)
		}
	}
}

// When both families fail, the operator must be given the address their
// application actually used, not this proxy's inference about it.
func TestDialDestination_ReportsTheRequestedAddressWhenBothFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()

	requested := net.JoinHostPort("127.0.0.1", port)
	_, err = DialDestination(context.Background(), zap.NewNop(), "tcp", DialTarget{Addr: requested})
	if err == nil {
		t.Fatal("dial succeeded with nothing listening")
	}
	if !strings.Contains(err.Error(), requested) {
		t.Fatalf("error names %q, want the requested address %q", err, requested)
	}
}

// The counterpart dial is bounded. The first dial was refused in one round
// trip, but a host that DROPs the other family would otherwise stall the
// proxy's hot path for a full TCP connect timeout, once per connection.
func TestDialDestination_BoundsTheCounterpartDial(t *testing.T) {
	const requested = "127.0.0.1:1" // mocked dial; no listener, no IPv6
	start := time.Now()
	_, err := DialDestinationWith(context.Background(), zap.NewNop(), DialTarget{Addr: requested},
		func(ctx context.Context, addr string) (net.Conn, error) {
			if addr == requested {
				return nil, syscall.ECONNREFUSED
			}
			<-ctx.Done() // a black-holed counterpart
			return nil, ctx.Err()
		})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected failure")
	}
	if elapsed > loopbackFallbackTimeout+2*time.Second {
		t.Fatalf("counterpart dial took %v, unbounded beyond the %v budget", elapsed, loopbackFallbackTimeout)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("err = %v, want the original refusal", err)
	}
}

// A nil logger must not panic: the fallback path is rare, and a panic that only
// fires there is the worst kind to leave.
func TestDialDestination_NilLoggerDoesNotPanic(t *testing.T) {
	_, requested := listenLoopback(t, "127.0.0.1:0")
	requireRefused(t, requested)
	loopbackFallbackReported.Store(false)
	conn, err := DialDestination(context.Background(), nil, "tcp", DialTarget{Addr: requested})
	if err != nil {
		t.Fatalf("nil logger: %v", err)
	}
	_ = conn.Close()
}

// The TLS arm is half of this change: a dependency reached over TLS breaks the
// same way, and leaving it out would make the fix hold only for plaintext.
// Nothing pinned it before — a mutant swapping the dialer's config survived the
// whole suite.
func TestDialDestinationTLS_FallsBackAndRepointsServerName(t *testing.T) {
	cert, host := selfSignedFor(t, "127.0.0.1")

	ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	requested := net.JoinHostPort("::1", port) // IPv6: nothing listens there
	requireRefused(t, requested)

	// ServerName pinned to the REQUESTED host, exactly as resolveUpstreamServerName
	// does when upstream verification is on.
	cfg := &tls.Config{RootCAs: poolFor(t, cert), ServerName: "::1"}

	loopbackFallbackReported.Store(false)
	conn, err := DialDestinationTLS(context.Background(), zap.NewNop(), "tcp",
		DialTarget{Addr: requested}, cfg)
	if err != nil {
		t.Fatalf("TLS fallback failed: %v — carrying ServerName %q onto the counterpart verifies "+
			"the certificate against an address the connection never reached, so a verifying "+
			"setup could never complete the fallback", err, cfg.ServerName)
	}
	_ = conn.Close()

	if cfg.ServerName != "::1" {
		t.Errorf("the caller's tls.Config was mutated (ServerName now %q); it must be cloned", cfg.ServerName)
	}
	_ = host
}

func TestTLSConfigForAddr_OnlyRewritesThePinnedServerName(t *testing.T) {
	for _, tc := range []struct{ name, sn, requested, dialed, want string }{
		{"pinned to the requested host is re-pointed", "::1", "::1", "127.0.0.1", "127.0.0.1"},
		{"empty is left empty for the stdlib to infer", "", "::1", "127.0.0.1", ""},
		{"a real hostname the caller meant is kept", "db.internal", "::1", "127.0.0.1", "db.internal"},
		{"same address is untouched", "::1", "::1", "::1", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &tls.Config{ServerName: tc.sn}
			got := tlsConfigForAddr(in, tc.requested, net.JoinHostPort(tc.dialed, "3306"))
			if got.ServerName != tc.want {
				t.Errorf("ServerName = %q, want %q", got.ServerName, tc.want)
			}
			if in.ServerName != tc.sn {
				t.Errorf("caller's config mutated: %q", in.ServerName)
			}
		})
	}
}

func selfSignedFor(t *testing.T, ip string) (tls.Certificate, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: ip},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP(ip)},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, ip
}

func poolFor(t *testing.T, cert tls.Certificate) *x509.CertPool {
	t.Helper()
	c, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := x509.NewCertPool()
	p.AddCert(c)
	return p
}

// When the counterpart ANSWERS and then fails for some other reason — a
// certificate that does not cover it, a reset after accept — that reason must
// survive. Reporting only the original refusal tells the operator "nothing is
// listening" when something was, which is the same misdirection this change
// exists to remove, one level down.
func TestDialDestination_CarriesTheCounterpartFailure(t *testing.T) {
	const requested = "127.0.0.1:1" // mocked dial; no listener, no IPv6
	counterpartErr := errors.New("tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, not ::1")

	_, err := DialDestinationWith(context.Background(), zap.NewNop(), DialTarget{Addr: requested},
		func(_ context.Context, addr string) (net.Conn, error) {
			if addr == requested {
				// The shape net.Dial actually returns, so the assertions below
				// hold for the error a caller really sees, not a bare errno.
				return nil, &net.OpError{
					Op: "dial", Net: "tcp",
					Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
					Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
				}
			}
			return nil, counterpartErr
		})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "certificate is valid for") {
		t.Fatalf("err = %q; the counterpart answered and failed verification, but its reason was "+
			"swallowed and the operator is told nothing is listening", err)
	}
	if !strings.Contains(err.Error(), requested) {
		t.Errorf("err = %q, want it to lead with the requested address", err)
	}
	// Callers classify on this; wrapping must not break it.
	if !neterr.IsConnRefused(err) {
		t.Errorf("err = %q no longer classifies as connection-refused", err)
	}
}
