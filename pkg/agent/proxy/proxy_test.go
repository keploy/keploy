package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// newTestServerTLSConfig returns a server TLS config with a fresh self-signed
// certificate for "test.local", offering nextProtos.
func newTestServerTLSConfig(t *testing.T, nextProtos []string) *tls.Config {
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
	return cfg
}

// newTLSTestServer spins up a TLS listener with a self-signed cert. The
// handler hook (if non-nil) receives each accepted, fully-handshaked
// *tls.Conn. Returns the listener and the server's TLS config.
func newTLSTestServer(t *testing.T, nextProtos []string, onAccept func(*tls.Conn)) (net.Listener, *tls.Config) {
	t.Helper()
	cfg := newTestServerTLSConfig(t, nextProtos)

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(rawLn, cfg)

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

// TestNewProxyTLSUpgradeFn_DestSide_DefaultSkipsVerification locks in the
// DEFAULT dest-side posture: when the upgrade fn is invoked with
// isClient=true (keploy dialing the real upstream) and upstream
// verification has NOT been opted into, it MUST handshake without
// verifying the upstream cert, regardless of what InsecureSkipVerify the
// caller-supplied cfg held. The supplied cfg must NOT be mutated — only
// the per-dial clone gets the flag flipped.
//
// This is a regression test for the dest-side passthrough drop reported
// as `dest TLS handshake failed: x509: certificate is valid for
// 127.0.0.1, not 10.224.0.152` against in-cluster K8s services whose
// upstream cert SAN doesn't match the ClusterIP keploy sees.
//
// The override used to be unconditional; it is now driven by
// record.upstreamTls.verify (see the companion
// TestNewProxyTLSUpgradeFn_DestSide_VerifyFlag* tests). This case is the
// one that must never change: false — the zero value, and what every
// existing user gets — has to reproduce the old behaviour exactly,
// including on a strict caller cfg like the one below, which is precisely
// the shape the K8s incident produced.
func TestNewProxyTLSUpgradeFn_DestSide_DefaultSkipsVerification(t *testing.T) {
	// Self-signed cert with CN "test.local"; we'll dial it asking for
	// a different ServerName to force the strict-verify path to fail
	// if the fix is missing.
	ln, _ := newTLSTestServer(t, []string{"h2", "http/1.1"}, nil)
	defer ln.Close()

	// verify=false, rootCAs=nil — the zero-config default.
	upgrade := newProxyTLSUpgradeFn(zap.NewNop(), false, nil)

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer rawConn.Close()

	// Strict cfg: deliberately wrong ServerName, no InsecureSkipVerify.
	// Without the fix, tls.Client(...).Handshake would fail with an
	// x509 hostname error.
	caller := &tls.Config{
		ServerName: "wrong.example.invalid",
		NextProtos: []string{"h2", "http/1.1"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tlsConn, err := upgrade(ctx, rawConn, true, caller)
	if err != nil {
		t.Fatalf("dest-side upgrade should ignore upstream cert validity, got: %v", err)
	}
	defer tlsConn.Close()

	// The caller's cfg must remain pristine — clone-on-dial, never mutate
	// what the parser handed us.
	if caller.InsecureSkipVerify {
		t.Fatalf("caller cfg.InsecureSkipVerify was mutated to true; expected the upgrade fn to clone before flipping the flag")
	}
	if caller.ServerName != "wrong.example.invalid" {
		t.Fatalf("caller cfg.ServerName was mutated to %q; expected the upgrade fn to clone before touching it", caller.ServerName)
	}
	if caller.RootCAs != nil {
		t.Fatalf("caller cfg.RootCAs was mutated; expected the upgrade fn to clone before touching it")
	}
}

// TestClampConsumerStallGrace pins the validation of the operator-supplied
// teardown grace.
//
// The zero cases are the load-bearing ones: zero must survive as zero so
// relay.withDefaults() can substitute DefaultConsumerStallGrace. keploy.yml
// ships an explicit 2s, so this is not the path most users take — but zero
// still arrives here from an operator who writes 0s, a programmatically built
// Config, and any enterprise SetDefaultConfig string that omits the key.
// Clamping it up to the 100ms floor would hand those callers a twentyfold cut
// to the window a briefly-stalled parser gets at teardown, i.e. exactly the
// mock loss the grace exists to prevent.
func TestClampConsumerStallGrace(t *testing.T) {
	t.Parallel()

	logger := zap.NewNop()

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		// Literals, deliberately, not minRecordBufferStallGrace /
		// maxRecordBufferStallGrace: the bounds ARE the safety contract, and a
		// table that reads them back from the constants it is checking moves
		// with any edit to them and pins nothing.
		{"zero defers to the relay default", 0, 0},
		{"negative defers to the relay default", -5 * time.Second, 0},
		{"below the floor clamps up", 50 * time.Millisecond, 100 * time.Millisecond},
		{"just below the floor clamps up", 99 * time.Millisecond, 100 * time.Millisecond},
		{"the floor itself is kept", 100 * time.Millisecond, 100 * time.Millisecond},
		{"an in-range value is untouched", 2 * time.Second, 2 * time.Second},
		{"the ceiling itself is kept", 10 * time.Second, 10 * time.Second},
		{"just above the ceiling clamps down", 10*time.Second + time.Nanosecond, 10 * time.Second},
		{"above the ceiling clamps down", 5 * time.Minute, 10 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clampConsumerStallGrace(logger, tc.in); got != tc.want {
				t.Errorf("clampConsumerStallGrace(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewSnapshotsStallGrace pins that the configured grace actually reaches
// the Proxy.
//
// A correct clamp is worthless if nothing calls it. Without this, deleting the
// clampConsumerStallGrace call in New — or the ConsumerStallGrace field in
// proxy_v2.go's relay.Config literal — turns the whole knob into a no-op with
// every package still green, which is precisely how a config option rots into
// decoration. This covers the config -> Proxy half; the Proxy -> relay.Config
// half is a private field read inside recordViaSupervisor and is left to
// relay's own TestNewWiresTheStallGraceIntoBothTees.
func TestNewSnapshotsStallGrace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero stays zero so the relay default applies", 0, 0},
		{"an in-range value reaches the proxy", 5 * time.Second, 5 * time.Second},
		{"an out-of-range value is clamped on the way in", 10 * time.Minute, 10 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.New()
			cfg.Record.RecordBuffer.ConsumerStallGrace = tc.in
			if got := New(zap.NewNop(), nil, cfg).recordBufferStallGrace; got != tc.want {
				t.Fatalf("recordBufferStallGrace = %v, want %v", got, tc.want)
			}
		})
	}

	// The value a user who never touches the knob actually gets, end to end
	// through the real default config rather than a hand-built one.
	t.Run("the shipped default arrives intact", func(t *testing.T) {
		t.Parallel()
		cfg := config.New()
		if got, want := New(zap.NewNop(), nil, cfg).recordBufferStallGrace, 2*time.Second; got != want {
			t.Fatalf("recordBufferStallGrace = %v, want %v", got, want)
		}
	})
}

// TestMockClearsThePreviousTestSetsConsumption pins the cross-process invariant
// the CLI's per-set latch now depends on.
//
// Under KEPLOY_AGENT_OWNS_CONSUMED the agent filters from its own
// consumedPersistent. The replayer treats a successful MockOutgoing — which
// lands here — as proof that the previous test set's consumption is gone, and
// on that basis stops sending its own map. If Mock ever stops performing the
// reset, the CLI hands set N+1's pool to an agent still holding set N's names:
// mock names are unique only WITHIN a set, so set N's mock-1 (Usage: Deleted)
// drops set N+1's unrelated mock-1, and filterOutDeleted stamps set N's
// IsFiltered/SortOrder onto its mock-2. A wrong test result, silently, with
// every suite green.
//
// TestPersistentConsumed_ClearedAtTestSetBoundary pins ResetForReplaySession in
// isolation; nothing pinned that Mock actually calls it, and it is one
// deletable line.
func TestMockClearsThePreviousTestSetsConsumption(t *testing.T) {
	p := New(zap.NewNop(), nil, &config.Config{})
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	p.setMockManager(mm)

	// The previous test set consumed two mocks. mock-2 is deliberately not
	// Deleted: a clear that only swept Deleted entries would still stamp the
	// old ordering onto the next set's same-named mock.
	for _, st := range []models.MockState{
		{Name: "mock-1", Kind: models.MySQL, Usage: models.Deleted},
		{Name: "mock-2", Kind: models.MySQL, Usage: models.Updated, IsFiltered: true, SortOrder: 7},
	} {
		if err := mm.flagMockAsUsed(st, 0); err != nil {
			t.Fatalf("flagMockAsUsed: %v", err)
		}
	}
	if len(p.GetPersistentConsumed()) != 2 {
		t.Fatalf("precondition: the previous set's consumption was not recorded")
	}

	// The test-set boundary. The error is ignored on purpose: Mock can fail
	// later, at the nsswitch setup, and that failure is AFTER the reset — the
	// property under test is the reset, not the call's overall success.
	_ = p.Mock(context.Background(), models.OutgoingOptions{})

	if got := p.GetPersistentConsumed(); len(got) != 0 {
		t.Fatalf("Mock did not clear the previous test set's consumption: %#v — the replayer treats a "+
			"successful MockOutgoing as proof that it did, and stops sending its own map, so this "+
			"silently mis-filters the next test set", got)
	}
}

// closeCountingConn is a connection that counts its Close calls, so a test can
// see whether the code under test closed it, as soon as that code returns.
type closeCountingConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// CloseWrite forwards a half-close, as the TCP connection it wraps would.
func (c *closeCountingConn) CloseWrite() error { return util.CloseWriteIfPossible(c.Conn) }

// NetConn is the connection beneath, the convention the proxy's own wrappers
// follow, so what looks for the socket under a connection finds it here too.
func (c *closeCountingConn) NetConn() net.Conn { return c.Conn }

// acceptedConn returns both ends of a loopback TCP connection: app is the
// application's end, and proxySide the end the proxy's listener accepted,
// counting its closes.
func acceptedConn(t *testing.T) (app *net.TCPConn, proxySide *closeCountingConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	return c.(*net.TCPConn), &closeCountingConn{Conn: s}
}

// fakeDestInfo answers handleConnection's lookup of a connection's original
// destination the way the connect hooks would.
type fakeDestInfo struct {
	dest   *agent.NetworkAddress
	getErr error
	delErr error
}

func (f fakeDestInfo) Get(context.Context, uint16) (*agent.NetworkAddress, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	d := *f.dest // handleConnection may rewrite the port of what it gets
	return &d, nil
}

func (f fakeDestInfo) Delete(context.Context, uint16) error { return f.delErr }

// predialedTo dials addr and hands it to p as the connection pre-dialled for
// the application end of src, the way a held handshake does, so the first
// dial of addr takes it: a test can then count whether the proxy closed the
// upstream connection it used.
func predialedTo(t *testing.T, p *Proxy, src net.Conn, addr string) *closeCountingConn {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	counted := &closeCountingConn{Conn: c}
	end, ok := clientEnd(src.RemoteAddr())
	if !ok {
		t.Fatalf("no client end for %v", src.RemoteAddr())
	}
	p.predials.put(end, util.NewPredialed(addr, connseq.NewUpstream(counted)))
	return counted
}

// awaitEvent waits for ch to deliver. What it waits for follows from a socket
// event the code under test causes without waiting on anything, however
// loaded the host is, so the wait has no budget of its own: it is bounded only
// so that code that never causes the event fails the test instead of hanging
// the binary. Each wait may take a quarter of the time left before the test
// binary's deadline, so one that never ends still leaves time for the waits
// and the tests after it to report their own results.
func awaitEvent[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var expired <-chan time.Time
	if dl, ok := t.Deadline(); ok {
		timer := time.NewTimer(time.Until(dl) / 4)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case v := <-ch:
		return v
	case <-expired:
		t.Fatalf("%s never happened", what)
		panic("unreachable")
	}
}

// Every path out of handleConnection closes the application's connection.
// Those that returned before the cleanup was registered, or that replaced
// the connection with nil, left the application blocked on its call until
// its own timeout.
func TestHandleConnectionClosesTheAppConnectionOnEveryEarlyReturn(t *testing.T) {
	dest := ipv4Dest(t, "127.0.0.1:8443")
	session := &agent.Session{
		Mode: models.MODE_TEST,
		// Mocking keeps the proxy off the destination; the MySQL probe is
		// off so nothing waits for the application to stay silent.
		OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true},
	}
	// A TLS record holding a ClientHello with no body: the MITM handshake
	// reads it whole and fails on it.
	brokenClientHello := []byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00}

	for _, tc := range []struct {
		name    string
		dest    fakeDestInfo
		session *agent.Session
		send    []byte
		wantErr bool
	}{
		{name: "the destination lookup fails", dest: fakeDestInfo{getErr: errors.New("no redirect record")}},
		{name: "releasing the destination record fails", dest: fakeDestInfo{dest: dest, delErr: errors.New("map delete failed")}, wantErr: true},
		{name: "the client's TLS handshake fails", dest: fakeDestInfo{dest: dest}, session: session, send: brokenClientHello, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, src := acceptedConn(t)
			p := &Proxy{logger: zap.NewNop(), DestInfo: tc.dest, mysqlPorts: newMysqlPortRegistry()}
			if tc.session != nil {
				p.setSession(tc.session)
			}
			if len(tc.send) > 0 {
				if _, err := app.Write(tc.send); err != nil {
					t.Fatal(err)
				}
			}

			err := p.handleConnection(context.Background(), src)
			if (err != nil) != tc.wantErr {
				t.Fatalf("handleConnection returned %v, want an error: %v", err, tc.wantErr)
			}
			if src.closes.Load() == 0 {
				t.Fatal("handleConnection returned without closing the application's connection")
			}
			if _, err := io.ReadAll(app); err != nil {
				t.Fatalf("the application's end did not see its connection end cleanly: %v", err)
			}
		})
	}
}

// A CONNECT tunnel the corporate proxy opened is closed when the application
// ends its connection before sending anything through it. The proxy's
// connection used to become dstConn only after the inner bytes were peeked,
// so returning at that peek left it open and unreferenced.
func TestHandleConnectionClosesTheTunnelWhenTheAppLeavesBeforeUsingIt(t *testing.T) {
	corp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corp.Close() })
	go func() {
		c, err := corp.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, _ = io.Copy(io.Discard, c)
	}()

	app, src := acceptedConn(t)
	p := &Proxy{
		logger:     zap.NewNop(),
		DestInfo:   fakeDestInfo{dest: ipv4Dest(t, corp.Addr().String())},
		mysqlPorts: newMysqlPortRegistry(),
	}
	p.setSession(&agent.Session{
		Mode:            models.MODE_RECORD,
		MC:              make(chan *models.Mock, 1),
		OutgoingOptions: models.OutgoingOptions{DisableMysqlAutoDetect: true},
	})
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	tunnel := predialedTo(t, p, src, corp.Addr().String())

	req := "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"
	if _, err := app.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	resp, err := http.ReadResponse(bufio.NewReader(app), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the tunnel's 200: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d, want 200", resp.StatusCode)
	}
	// The application leaves without sending a byte through the tunnel.
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	if err := awaitEvent(t, done, "handleConnection returning after the application left"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	if tunnel.closes.Load() == 0 {
		t.Fatal("handleConnection returned without closing its connection to the corporate proxy")
	}
	if src.closes.Load() == 0 {
		t.Fatal("handleConnection returned without closing the application's connection")
	}
}

// containerAt names one address a container, as the index of the local
// Docker daemon would for a container's published port.
type containerAt struct {
	addr string
	c    containerkey.Container
}

func (r containerAt) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return r.c, dst.String() == r.addr, nil
}

func (containerAt) Bind(context.Context)                               {}
func (containerAt) Watch(context.Context, *zap.Logger)                 {}
func (containerAt) Endpoint() string                                   { return "unix:///fake.sock" }
func (containerAt) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (containerAt) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (containerAt) AwaitReadsOf(context.Context, []string)             {}

// probeSaw is what a parser saw of its connection's container key while the
// proxy served it: through its context, and on a mock of its connection.
type probeSaw struct {
	fromCtx, stamped, connID string
}

// ctxProbe is a parser that takes every connection and reports what it saw.
type ctxProbe struct{ saw chan probeSaw }

func (ctxProbe) MatchType(context.Context, []byte) bool { return true }

func (p ctxProbe) look(ctx context.Context) error {
	connID, _ := ctx.Value(models.ClientConnectionIDKey).(string)
	key, _ := containerkey.FromContext(ctx)
	m := &models.Mock{ConnectionID: connID}
	containerkey.Default.Stamp(m)
	p.saw <- probeSaw{fromCtx: key, stamped: m.Spec.Metadata[containerkey.MetadataKey], connID: connID}
	return nil
}

func (p ctxProbe) RecordOutgoing(ctx context.Context, _ *integrations.RecordSession) error {
	return p.look(ctx)
}

func (p ctxProbe) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	return p.look(ctx)
}

// Under the proven switch the proxy names a container destination when it
// accepts the connection: the parser's context carries the key, and the
// mocks of that connection (by the connection ID the same context carries)
// are stamped with it while it is open. Once handleConnection returns, the
// keyer holds nothing of it. Without the switch nothing is named. The same in
// a record (Record) and in a replay (Mock).
func TestHandleConnectionNamesAContainerDestination(t *testing.T) {
	for _, tc := range []struct {
		mode models.Mode
		on   bool
	}{{models.MODE_RECORD, true}, {models.MODE_RECORD, false}, {models.MODE_TEST, true}, {models.MODE_TEST, false}} {
		mode, on := tc.mode, tc.on
		t.Run(fmt.Sprintf("%s/proven=%v", mode, on), func(t *testing.T) {
			if on {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			upstream := keepAliveHTTPUpstream(t)
			saved := containerkey.Default
			containerkey.Default = containerkey.NewKeyer(containerAt{addr: upstream,
				c: containerkey.Container{ID: "c1", Image: "localstack/localstack:3.8", Port: 4566, Fingerprint: "0123456789ab"}})
			t.Cleanup(func() { containerkey.Default = saved })

			cfg := config.New()
			cfg.Agent.Mode = mode
			p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
			probe := ctxProbe{saw: make(chan probeSaw, 1)}
			p.Integrations = map[integrations.IntegrationType]integrations.Integrations{"probe": probe}
			p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: "probe"}}
			opts := models.OutgoingOptions{DisableMysqlAutoDetect: true}
			if mode == models.MODE_RECORD {
				if err := p.Record(context.Background(), make(chan *models.Mock, 16), opts); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
			} else {
				// Mock can fail last, at its nsswitch setup (root's), with
				// the session already set up.
				opts.Mocking = true
				_ = p.Mock(context.Background(), opts)
			}

			app, src := acceptedConn(t)
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			if _, err := io.WriteString(app, "GET /queue HTTP/1.1\r\nHost: dependency\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			saw := awaitEvent(t, probe.saw, "the parser being handed the connection")
			awaitEvent(t, done, "handleConnection returning")

			want := ""
			if on {
				want = "container:localstack/localstack:4566@0123456789ab"
			}
			if saw.fromCtx != want || saw.stamped != want {
				t.Fatalf("while open, the parser's context carried %q and its connection's mock was stamped %q, want %q", saw.fromCtx, saw.stamped, want)
			}
			// A mock handed over after the connection closed is still
			// stamped; a new session forgets the connection.
			m := &models.Mock{ConnectionID: saw.connID}
			containerkey.Default.Stamp(m)
			if got := m.Spec.Metadata[containerkey.MetadataKey]; got != want {
				t.Fatalf("after the connection closed the keyer stamped %q, want %q", got, want)
			}
			containerkey.Default.Reset()
			m = &models.Mock{ConnectionID: saw.connID}
			containerkey.Default.Stamp(m)
			if got := m.Spec.Metadata[containerkey.MetadataKey]; got != "" {
				t.Fatalf("after a new session the keyer still stamped %q", got)
			}
		})
	}
}

// sessionIndex is a container index that counts what the proxy asks of it.
type sessionIndex struct {
	at             map[string]containerkey.Container
	binds, watches atomic.Int32
}

func (s *sessionIndex) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	c, ok := s.at[dst.String()]
	return c, ok, nil
}

func (s *sessionIndex) Bind(context.Context)                             { s.binds.Add(1) }
func (s *sessionIndex) Watch(context.Context, *zap.Logger)               { s.watches.Add(1) }
func (*sessionIndex) Endpoint() string                                   { return "unix:///fake.sock" }
func (*sessionIndex) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (*sessionIndex) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (*sessionIndex) AwaitReadsOf(context.Context, []string)             {}

// Only under the proven switch does the agent bind its reading of Docker to
// its life, and a session start (Record, Mock) index the local daemon's
// containers and start their scopes afresh. Without the switch the proxy
// never touches the index: no watch of the daemon (its goroutines, its
// connection, its messages) and the scopes left as they were.
func TestContainerKeysStartOnlyUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", on), func(t *testing.T) {
			if on {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			idx := &sessionIndex{at: map[string]containerkey.Container{
				"172.17.0.2:5432": {ID: "a", Image: "postgres:16", Port: 5432, Fingerprint: "0123456789ab"},
				"172.17.0.3:5432": {ID: "b", Image: "postgres:16", Port: 5432, Fingerprint: "0123456789ab"},
			}}
			saved := containerkey.Default
			containerkey.Default = containerkey.NewKeyer(idx)
			t.Cleanup(func() { containerkey.Default = saved })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			cfg := config.New()
			p := New(zap.NewNop(), fakeDestInfo{}, cfg)
			p.bindContainerKeys(ctx)
			t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
			sessions := map[string]func(){
				"Record": func() {
					_ = p.Record(ctx, make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true})
				},
				// Mock can fail later, at its nsswitch setup: after the
				// container keys' start, which is what is checked here.
				"Mock": func() { _ = p.Mock(ctx, models.OutgoingOptions{}) },
			}
			use := func(connID, dst string) containerkey.State {
				containerkey.Default.Name(nil, connID, dst)
				_, st := containerkey.StateFromContext(containerkey.WithConn(ctx, containerkey.Default, connID))
				containerkey.Default.Forget(connID)
				return st
			}
			for _, name := range []string{"Record", "Mock"} {
				containerkey.Default.Reset()
				a := use("a", "172.17.0.2:5432")
				sessions[name]()
				// b is configured as a is: in a's scope it cannot be told
				// apart from it, in a new session's it can.
				b := use("b", "172.17.0.3:5432")
				want := containerkey.Withheld
				if on {
					want = containerkey.Keyed
				}
				if a != containerkey.Keyed || b != want {
					t.Fatalf("around %s: a was %v and b %v, want b %v", name, a, b, want)
				}
			}
			wantBinds, wantWatches := int32(0), int32(0)
			if on {
				wantBinds, wantWatches = 1, 2
			}
			if got := idx.binds.Load(); got != wantBinds {
				t.Fatalf("the agent bound the index %d times, want %d", got, wantBinds)
			}
			if got := idx.watches.Load(); got != wantWatches {
				t.Fatalf("Record and Mock watched the index %d times, want %d", got, wantWatches)
			}
		})
	}
}

// A client that dialled an IP literal sends no SNI and checks the MITM leaf
// against an IP SAN, as client-go does reaching the Kubernetes API at
// https://10.96.0.1 (dsmon F7). On replay that has to hold whether or not the
// proxy offers h2 to the destination: the h2 hint used to be put on a context
// rebuilt from the connection's, which dropped the destination IP, so a
// destination with PreferH2 or a recorded Http2 mock got a leaf with no IP SAN.
func TestHandleConnectionNamesTheLeafForAnIPLiteralDestinationOnReplay(t *testing.T) {
	const ip = "10.96.0.1"
	for _, tc := range []struct {
		name     string
		preferH2 bool
		mocks    []*models.Mock
		wantALPN string
	}{
		{name: "the destination is http/1.1", wantALPN: "http/1.1"},
		{name: "PreferH2 is set", preferH2: true, wantALPN: "h2"},
		{name: "the destination has a recorded Http2 mock", mocks: []*models.Mock{h2MockForTest("k8s-api", ip+":443")}, wantALPN: "h2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, src := acceptedConn(t)
			p := replayProxyTo(t, ip+":443", models.OutgoingOptions{PreferH2: tc.preferH2}, tc.mocks...)

			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			// An IP literal: crypto/tls sends no SNI for it.
			client := leafCheckingClient(app, ip, ip)
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake with %s: %v", ip, err)
			}
			if got := client.ConnectionState().NegotiatedProtocol; got != tc.wantALPN {
				t.Fatalf("negotiated %q, want %q", got, tc.wantALPN)
			}
			_ = client.Close()
			awaitEvent(t, done, "handleConnection returning after the client left")
		})
	}
}

// On replay the MITM offers h2 only to a destination that was recorded
// speaking HTTP/2. On the transparent path (no CONNECT request) the host is
// known only from the ClientHello's SNI, so the offer has to be decided there:
// decided before the handshake, it went by the port alone, and a recorded
// HTTP/2 destination on :443 pushed every other :443 host onto h2. A client
// that offers both (Go's net/http, every cloud SDK on it) then spoke HTTP/2 to
// a host it was recorded speaking HTTP/1.1 to, and none of its calls matched:
// the HTTP/2 matcher never sees the host's http/1.1 mocks.
//
// Only that changes: every connection whose SNI names no host recorded
// speaking HTTP/1.1 only is offered what the port decided before.
func TestReplayOffersH2OnlyToTheHostTheClientHelloNames(t *testing.T) {
	const ip = "10.0.0.9"
	vault := h2MockForTest("vault", "vault.example.com:443")
	queue := h1MockForTest("queue", "queue.example.com", "/send")
	// A client sending through a forward proxy records an absolute URL and
	// may send no Host header.
	queueByURL := h1MockForTest("queue", "", "https://queue.example.com/send")
	queueByURL.Spec.HTTPReq.Header = map[string]string{}
	// Recorded headers are a plain map: the key is spelt as the recorder
	// wrote it.
	queueLowerKey := h1MockForTest("queue", "", "/send")
	queueLowerKey.Spec.HTTPReq.Header = map[string]string{"host": "queue.example.com"}
	for _, tc := range []struct {
		name     string
		recorded []*models.Mock
		dst      string // ip:port the connect hooks report; ip:443 when ""
		sni      string // "" sends none
		wantALPN string
	}{
		{name: "a host recorded speaking HTTP/1.1 beside one recorded speaking HTTP/2 on the same port", recorded: []*models.Mock{vault, queue}, sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "this host was recorded speaking HTTP/2", recorded: []*models.Mock{vault, queue}, sni: "vault.example.com", wantALPN: "h2"},
		{name: "the recorded host differs only in case", recorded: []*models.Mock{vault, queue}, sni: "VAULT.example.com", wantALPN: "h2"},
		{name: "the host recorded speaking HTTP/1.1 differs only in case", recorded: []*models.Mock{vault, queue}, sni: "QUEUE.example.com", wantALPN: "http/1.1"},
		{name: "the host spoke HTTP/1.1 at this port, its Host naming the port", recorded: []*models.Mock{vault, h1MockForTest("queue", "queue.example.com:443", "/send")}, sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the Host recorded speaking HTTP/1.1 has a trailing dot", recorded: []*models.Mock{vault, h1MockForTest("queue", "queue.example.com.", "/send")}, sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the host spoke HTTP/1.1 through a forward proxy, its URL naming it", recorded: []*models.Mock{vault, queueByURL}, sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the host spoke HTTP/1.1, its Host header key in lower case", recorded: []*models.Mock{vault, queueLowerKey}, sni: "queue.example.com", wantALPN: "http/1.1"},
		// The connection's IP was recorded speaking HTTP/2, at another port.
		{name: "the host spoke HTTP/1.1 and the connection's IP spoke HTTP/2 at another port", recorded: []*models.Mock{vault, h2MockForTest("ip", ip+":8443"), queue}, sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the host spoke plain HTTP", recorded: []*models.Mock{vault, h1MockForTest("queue", "queue.example.com", "http://queue.example.com/send")}, sni: "queue.example.com", wantALPN: "h2"},
		// The requests carry a virtual host (the recorded :authority) behind
		// the balancer the client names: the SNI names no recorded host.
		{name: "the SNI names a host the recording does not", recorded: []*models.Mock{h2MockForTest("api", "api.example.com:443"), queue}, sni: "lb.example.com", wantALPN: "h2"},
		// Go's net/http keeps a fully qualified name's trailing dot in the
		// :authority and crypto/tls drops it from the SNI.
		{name: "the recorded authority has a trailing dot", recorded: []*models.Mock{h2MockForTest("vault", "vault.example.com.:443")}, sni: "vault.example.com", wantALPN: "h2"},
		{name: "the recorded authority has a trailing dot and the host also spoke HTTP/1.1", recorded: []*models.Mock{h2MockForTest("vault", "vault.example.com.:443"), h1MockForTest("vault-h1", "vault.example.com", "/v1")}, sni: "vault.example.com", wantALPN: "h2"},
		// The client dialled the IP with a TLS server-name override.
		{name: "the recorded authority is the connection's IP", recorded: []*models.Mock{h2MockForTest("vault", ip+":443")}, sni: "vault.example.com", wantALPN: "h2"},
		{name: "the recorded authority is the connection's IP and the host also spoke HTTP/1.1", recorded: []*models.Mock{h2MockForTest("vault", ip+":443"), h1MockForTest("vault-h1", "vault.example.com", "/v1")}, sni: "vault.example.com", wantALPN: "h2"},
		{name: "the host spoke HTTP/1.1 at a port its recording does not say", recorded: []*models.Mock{h2MockForTest("vault", "vault.example.com:8443"), queue}, dst: ip + ":8443", sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the host spoke HTTP/1.1 at another port", recorded: []*models.Mock{vault, h1MockForTest("queue", "queue.example.com:8443", "/send")}, sni: "queue.example.com", wantALPN: "h2"},
		{name: "the host spoke HTTP/1.1 at :443 and the connection is to :8443", recorded: []*models.Mock{h2MockForTest("vault", "vault.example.com:8443"), h1MockForTest("queue", "queue.example.com:443", "/send")}, dst: ip + ":8443", sni: "queue.example.com", wantALPN: "h2"},
		// A client that dialled an IP literal sends no SNI: the port decides.
		{name: "no SNI, a destination recorded speaking HTTP/2 on this port", recorded: []*models.Mock{vault, queue}, wantALPN: "h2"},
		{name: "no SNI, the only destination recorded speaking HTTP/2 is on another port", recorded: []*models.Mock{vault}, dst: ip + ":8443", wantALPN: "http/1.1"},
		{name: "no SNI, the connection's IP recorded speaking HTTP/2 at a port the recording does not say", recorded: []*models.Mock{h2MockForTest("ip", ip+":0")}, wantALPN: "http/1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := tc.dst
			if dst == "" {
				dst = ip + ":443"
			}
			leafName := tc.sni
			if leafName == "" {
				leafName = ip
			}
			app, src := acceptedConn(t)
			p := replayProxyTo(t, dst, models.OutgoingOptions{}, tc.recorded...)

			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			client := leafCheckingClient(app, tc.sni, leafName)
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake with %q: %v", tc.sni, err)
			}
			if got := client.ConnectionState().NegotiatedProtocol; got != tc.wantALPN {
				t.Fatalf("negotiated %q, want %q", got, tc.wantALPN)
			}
			_ = client.Close()
			// The row checks the ALPN offer, not how the connection ends.
			_ = awaitEvent(t, done, "handleConnection returning after the client left")
		})
	}
}

// Before a session's mock manager exists, the recording's seeded HTTP/2
// destinations decide: a host they do not name is offered what the port
// decides, as before.
func TestReplayOffersH2ByThePortBeforeTheMockManager(t *testing.T) {
	app, src := acceptedConn(t)
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, "10.0.0.9:443")}, mysqlPorts: newMysqlPortRegistry()}
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true}})
	p.SeedRecordedDestinations([]*models.Mock{h2MockForTest("vault", "vault.example.com:443")})

	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	client := leafCheckingClient(app, "lb.example.com", "lb.example.com")
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if got := client.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("negotiated %q, want h2", got)
	}
	_ = client.Close()
	_ = awaitEvent(t, done, "handleConnection returning after the client left")
}

// From a set's first test on, the mocks the test serves decide the offer, as
// a replay stages them: a test's own calls, per test. A host recorded
// speaking HTTP/2 in another test and HTTP/1.1 in this one is offered
// http/1.1 here, as the port decided before when this test serves no HTTP/2
// mock: its HTTP/1.1 calls are the ones served.
func TestReplayOffersH2ByTheMocksTheTestServes(t *testing.T) {
	const ip = "10.0.0.9"
	t0 := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	inTest := func(mk *models.Mock, at time.Time) *models.Mock {
		mk.Spec.ReqTimestampMock = at
		mk.Spec.ResTimestampMock = at.Add(time.Millisecond)
		mk.TestModeInfo.Lifetime = models.LifetimePerTest
		return mk
	}
	for _, tc := range []struct {
		name     string
		recorded func() (served, other []*models.Mock)
		sni      string
		wantALPN string
	}{
		{name: "a host recorded speaking HTTP/1.1 in this test beside one recorded speaking HTTP/2", sni: "queue.example.com", wantALPN: "http/1.1"},
		{name: "the host recorded speaking HTTP/2 in this test", sni: "vault.example.com", wantALPN: "h2"},
		{name: "a host the recording does not name", sni: "lb.example.com", wantALPN: "h2"},
		{
			name: "a host recorded speaking HTTP/2 in another test and HTTP/1.1 in this one",
			recorded: func() (served, other []*models.Mock) {
				return []*models.Mock{inTest(h1MockForTest("queue", "queue.example.com", "/send"), t0)},
					[]*models.Mock{inTest(h2MockForTest("queue-h2", "queue.example.com:443"), t0.Add(-time.Minute))}
			},
			sni:      "queue.example.com",
			wantALPN: "http/1.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := []*models.Mock{
				inTest(h2MockForTest("vault", "vault.example.com:443"), t0),
				inTest(h1MockForTest("queue", "queue.example.com", "/send"), t0),
			}
			var other []*models.Mock
			if tc.recorded != nil {
				served, other = tc.recorded()
			}
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, ip+":443")}, mysqlPorts: newMysqlPortRegistry()}
			p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true}})
			p.SeedRecordedDestinations(append(append([]*models.Mock{}, served...), other...))
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(served, nil, t0.Add(-time.Second), t0.Add(time.Second))
			p.mockManager = mm
			if !mm.HasFirstTestFired() {
				t.Fatal("the test's window is staged but its first test has not fired")
			}

			app, src := acceptedConn(t)
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			client := leafCheckingClient(app, tc.sni, tc.sni)
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake with %q: %v", tc.sni, err)
			}
			if got := client.ConnectionState().NegotiatedProtocol; got != tc.wantALPN {
				t.Fatalf("negotiated %q, want %q", got, tc.wantALPN)
			}
			_ = client.Close()
			_ = awaitEvent(t, done, "handleConnection returning after the client left")
		})
	}
}

// The offer reads the served Http mocks on every handshake whose SNI names a
// host not recorded speaking HTTP/2. A mock recorded calling another host is
// read for its host only: its URL is not parsed, and a Host with no port
// allocates nothing.
func TestReplayReadsOnlyTheHostOfAnHttpMockForAnotherHost(t *testing.T) {
	var mocks []*models.Mock
	for i := 0; i < 400; i++ {
		host := fmt.Sprintf("other-%d.example.com", i)
		if i%2 == 1 {
			host += ":443"
		}
		mocks = append(mocks, h1MockForTest(fmt.Sprintf("other-%d", i), host, "https://"+host+"/send?n=1"))
	}
	p := replayProxyTo(t, "10.0.0.9:443", models.OutgoingOptions{}, mocks...)
	if p.replayTargetHasHTTPMock("queue.example.com", 443) {
		t.Fatal("no mock names queue.example.com, yet it was found")
	}
	allocs := testing.AllocsPerRun(20, func() { p.replayTargetHasHTTPMock("queue.example.com", 443) })
	if allocs >= float64(len(mocks))/4 {
		t.Fatalf("asking about one host among %d Http mocks for other hosts made %.0f allocations, want none per mock", len(mocks), allocs)
	}
}

// An Http mock that recorded no destination (no Host header, a path-only URL)
// names no host. Its empty destination must not match a CONNECT request's
// host ".", which is empty once its trailing dot is ignored.
func TestReplayHttpMockWithNoDestinationNamesNoHost(t *testing.T) {
	mk := h1MockForTest("no-destination", "", "/send")
	mk.Spec.HTTPReq.Header = map[string]string{}
	if httpMockCalled(mk, ".", 443) {
		t.Fatal(`an Http mock with no recorded destination was taken as calling "."`)
	}
}

// A client that made a CONNECT request named its destination there, so the h2
// offer goes by the CONNECT host, not by the name its ClientHello asks for: a
// client can ask for another name (a TLS server-name override) while its
// HTTP/2 requests, which the mocks were recorded from, carry the host it
// connected to.
func TestReplayOffersH2ToTheHostTheConnectRequestNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string // the Http2 mock's :authority; vault.example.com:443 when ""
		connect  string
		sni      string
		wantALPN string
	}{
		{name: "the CONNECT names the host recorded speaking HTTP/2", connect: "vault.example.com", sni: "queue.example.com", wantALPN: "h2"},
		{name: "the CONNECT names another host", connect: "queue.example.com", sni: "vault.example.com", wantALPN: "http/1.1"},
		{name: "the CONNECT names the host with a trailing dot", connect: "vault.example.com.", sni: "vault.example.com", wantALPN: "h2"},
		{name: "the recorded authority names the CONNECT host with a trailing dot", recorded: "vault.example.com.:443", connect: "vault.example.com", sni: "vault.example.com", wantALPN: "h2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, src := acceptedConn(t)
			// The connect hooks report the corporate proxy the application
			// dialled.
			authority := tc.recorded
			if authority == "" {
				authority = "vault.example.com:443"
			}
			p := replayProxyTo(t, "10.96.0.9:3128", models.OutgoingOptions{}, h2MockForTest("vault", authority))
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			if _, err := fmt.Fprintf(app, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", tc.connect, tc.connect); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(app), &http.Request{Method: http.MethodConnect})
			if err != nil {
				t.Fatalf("reading the CONNECT reply: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT answered %s, want 200", resp.Status)
			}
			client := leafCheckingClient(app, tc.sni, tc.sni)
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake after CONNECT %s: %v", tc.connect, err)
			}
			if got := client.ConnectionState().NegotiatedProtocol; got != tc.wantALPN {
				t.Fatalf("negotiated %q, want %q", got, tc.wantALPN)
			}
			_ = client.Close()
			_ = awaitEvent(t, done, "handleConnection returning after the client left")
		})
	}
}

// replayProxyTo is a proxy replaying mocks, with opts, the connections the
// connect hooks say went to dst (an ip:port).
func replayProxyTo(t *testing.T, dst string, opts models.OutgoingOptions, mocks ...*models.Mock) *Proxy {
	t.Helper()
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, dst)}, mysqlPorts: newMysqlPortRegistry()}
	opts.Mocking, opts.DisableMysqlAutoDetect = true, true
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: opts})
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetUnFilteredMocks(mocks)
	p.mockManager = mm
	return p
}

// leafCheckingClient is a TLS client on conn that sends sni ("" for none)
// and takes keploy's leaf only if it is valid for name under this run's MITM
// CA, as an application trusting that CA would.
func leafCheckingClient(conn net.Conn, sni, name string) *tls.Conn {
	return tls.Client(conn, &tls.Config{
		ServerName: sni,
		NextProtos: []string{"h2", "http/1.1"},
		// The run's CA is minted by the handshake itself, so the leaf is
		// verified in VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pTls.ActiveCACertPEM()) {
				return errors.New("no MITM CA for this run")
			}
			leaf := cs.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
				return fmt.Errorf("leaf CN %q, IP SANs %v: %w", leaf.Subject.CommonName, leaf.IPAddresses, err)
			}
			return nil
		},
	})
}

// A client that made a CONNECT request and sends no SNI in the ClientHello
// after it gets a leaf for the host its CONNECT named: it already told the
// proxy where it is going. The name goes with its own connection to the
// handshake (pTls.WithConnectTarget).
func TestHandleConnectionNamesTheLeafForTheConnectTargetWithNoSNI(t *testing.T) {
	const target = "api.example.com"
	app, src := acceptedConn(t)
	// The connect hooks report the corporate proxy the application dialled.
	p := replayProxyTo(t, "10.96.0.9:3128", models.OutgoingOptions{})
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	if _, err := fmt.Fprintf(app, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the CONNECT reply: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %s, want 200", resp.Status)
	}
	client := leafCheckingClient(app, "", target)
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake after CONNECT %s, no SNI: %v", target, err)
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning after the client left")
}

// starttlsParser serves a plaintext STARTTLS on replay: it answers the
// client's "STARTTLS" line and then terminates TLS on the connection with
// keploy's MITM, under the context the proxy hands it, the way a parser's
// client-side upgrade does.
type starttlsParser struct{}

func (starttlsParser) MatchType(_ context.Context, buf []byte) bool {
	return strings.HasPrefix(string(buf), "STARTTLS")
}

func (starttlsParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return errors.New("starttlsParser serves replays only")
}

func (starttlsParser) MockOutgoing(ctx context.Context, src net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	line := make([]byte, len("STARTTLS\r\n"))
	if _, err := io.ReadFull(src, line); err != nil {
		return err
	}
	if _, err := io.WriteString(src, "OK\r\n"); err != nil {
		return err
	}
	c, _, err := pTls.HandleTLSConnection(ctx, zap.NewNop(), src, time.Time{})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, c)
	return nil
}

// A parser that terminates TLS on a connection after its CONNECT (a STARTTLS
// upgrade) names the leaf for the CONNECT target when the client sends no
// SNI, as the proxy's own handshake does: the target is on the context
// handleConnection hands the parser.
func TestAParserUpgradingAConnectionAfterItsConnectNamesTheLeafForTheTarget(t *testing.T) {
	const target = "db.example.com"
	app, src := acceptedConn(t)
	p := starttlsReplayProxyTo(t, "10.96.0.9:3128")
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	if _, err := fmt.Fprintf(app, "CONNECT %s:6000 HTTP/1.1\r\nHost: %s:6000\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(app)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the CONNECT reply: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %s, want 200", resp.Status)
	}
	askForSTARTTLS(t, app, br)
	client := leafCheckingClient(app, "", target)
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake with the parser after CONNECT %s, no SNI: %v", target, err)
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning after the client left")
}

// A parser that terminates TLS on a connection (a STARTTLS upgrade) names the
// leaf for the IP the client dialled when the client sends no SNI, as the
// proxy's own handshake does: a MySQL or Postgres client that dialled an IP
// literal and checks the server's identity verifies the leaf against an IP
// SAN. The IP is on the context handleConnection hands the parser.
func TestAParserUpgradingAConnectionNamesTheLeafForTheIPItsClientDialled(t *testing.T) {
	const ip = "10.96.0.7"
	app, src := acceptedConn(t)
	p := starttlsReplayProxyTo(t, ip+":6000")
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	askForSTARTTLS(t, app, bufio.NewReader(app))
	client := leafCheckingClient(app, "", ip)
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake with the parser for %s, no SNI: %v", ip, err)
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning after the client left")
}

// starttlsReplayProxyTo is replayProxyTo with starttlsParser its only parser.
func starttlsReplayProxyTo(t *testing.T, dst string) *Proxy {
	t.Helper()
	p := replayProxyTo(t, dst, models.OutgoingOptions{})
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{"starttls": starttlsParser{}}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: "starttls"}}
	return p
}

// askForSTARTTLS sends starttlsParser its STARTTLS line on w and reads its OK
// from r, after which the parser terminates TLS on the connection.
func askForSTARTTLS(t *testing.T, w io.Writer, r *bufio.Reader) {
	t.Helper()
	if _, err := io.WriteString(w, "STARTTLS\r\n"); err != nil {
		t.Fatal(err)
	}
	ok := make([]byte, len("OK\r\n"))
	if _, err := io.ReadFull(r, ok); err != nil || string(ok) != "OK\r\n" {
		t.Fatalf("the parser answered %q (%v), want OK", ok, err)
	}
}

// bootStageRecord is one stage of the agent's start the proxy began
// (beginBootWork): the goroutine that began it, and the one that ended it.
type bootStageRecord struct {
	name           string
	beganOn, endOn uint64
	ended          bool
}

// recordBootWork stands in for utils.BeginBootWork for the rest of the test,
// and returns what has been begun so far.
func recordBootWork(t *testing.T) (begun func() []bootStageRecord) {
	t.Helper()
	var mu sync.Mutex
	var recs []*bootStageRecord
	was := beginBootWork
	beginBootWork = func(name string) func() {
		r := &bootStageRecord{name: name, beganOn: goroutineID()}
		mu.Lock()
		recs = append(recs, r)
		mu.Unlock()
		return func() {
			mu.Lock()
			defer mu.Unlock()
			if !r.ended {
				r.ended, r.endOn = true, goroutineID()
			}
		}
	}
	t.Cleanup(func() { beginBootWork = was })
	return func() []bootStageRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]bootStageRecord, len(recs))
		for i, r := range recs {
			out[i] = *r
		}
		return out
	}
}

// goroutineID is the calling goroutine's id, from its stack's header.
func goroutineID() uint64 {
	var b [64]byte
	f := strings.Fields(strings.TrimPrefix(string(b[:runtime.Stack(b[:], false)]), "goroutine "))
	id, _ := strconv.ParseUint(f[0], 10, 64)
	return id
}

// The proxy listener's start -- its bind, the kernel count's proof, the
// handshake hold -- is a stage of the agent's start of its own, begun and
// ended on the goroutine that does it, and over before readyChan says how it
// went: StartProxy's stage waits for it on readyChan, and that wait alone
// shows no progress, so a listener's start that took long on a loaded host
// was counted as beats without progress, and the agent's start given up on.
// So is a start that cannot listen.
func TestTheProxyListenersStartIsAStageOfTheAgentsStart(t *testing.T) {
	t.Run("it listens", func(t *testing.T) {
		begun := recordBootWork(t)
		// The shutdown closes the shared mock manager's channel: a new one,
		// then none, for the next test.
		mgr := syncMock.Get()
		t.Cleanup(func() {
			mgr.SetOutputChannel(make(chan *models.Mock))
			mgr.SetOutputChannel(nil)
		})
		p := &Proxy{logger: zap.NewNop()}
		ctx, cancel := context.WithCancel(context.Background())
		readyChan := make(chan error, 1)
		startErr := make(chan error, 1)
		go func() { startErr <- p.start(ctx, readyChan) }()
		select {
		case err := <-readyChan:
			if err != nil {
				cancel()
				t.Fatalf("start() reported not ready: %v", err)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			cancel()
			t.Fatal("start() never said it was ready")
		}
		assertOneOwnStage(t, begun(), "the proxy listener's start")
		cancel()
		select {
		case <-startErr:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("start() did not return after its context was cancelled")
		}
	})
	t.Run("it cannot listen", func(t *testing.T) {
		held, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		begun := recordBootWork(t)
		p := &Proxy{logger: zap.NewNop(), Port: uint32(held.Addr().(*net.TCPAddr).Port)}
		readyChan := make(chan error, 1)
		go func() { _ = p.start(context.Background(), readyChan) }()
		select {
		case err := <-readyChan:
			if err == nil {
				t.Fatal("start() said it was ready on a port another socket holds")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("start() never said how its start went")
		}
		assertOneOwnStage(t, begun(), "the proxy listener's start")
	})
}

// assertOneOwnStage fails the test unless recs is one stage, name, begun and
// ended on one goroutine other than the test's.
func assertOneOwnStage(t *testing.T, recs []bootStageRecord, name string) {
	t.Helper()
	if len(recs) != 1 || recs[0].name != name {
		t.Fatalf("stages begun %+v, want %q alone", recs, name)
	}
	r := recs[0]
	if !r.ended {
		t.Fatalf("%q had not ended when its start was over", name)
	}
	if r.beganOn != r.endOn || r.beganOn == goroutineID() {
		t.Fatalf("%q was begun on goroutine %d and ended on %d (the test is %d): want both on the goroutine doing the work", name, r.beganOn, r.endOn, goroutineID())
	}
}

// The app clock the agent hands the proxy is the one its learn step reads a
// call's window on.
func TestSetAppClockIsTheLearnStepsClock(t *testing.T) {
	p := &Proxy{learnTracker: learn.NewTracker()}
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	prev := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec}}
	paired := &models.Mock{Name: "mock-4", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: rec.Add(4 * time.Millisecond)}}
	served := time.Date(2026, 10, 20, 15, 50, 37, 0, time.UTC)
	p.learnTracker.ServedMockAt(prev, served)
	if w := p.learnTracker.CallWindow(paired, []*models.Mock{prev}, served.Add(time.Millisecond)); w != nil {
		t.Fatalf("a window before the app's clock was set: %+v", w)
	}
	p.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(21 * time.Second), true })
	w := p.learnTracker.CallWindow(paired, []*models.Mock{prev}, served.Add(time.Millisecond))
	if w == nil || !w.F.From.Equal(served.Add(21*time.Second)) || !w.F.To.Equal(served.Add(21*time.Second+time.Millisecond)) {
		t.Fatalf("window %+v; want F read on the app clock the agent handed the proxy", w)
	}
	// A move of the clock after the serve: the span starts where the run
	// the agent handed the proxy began.
	moved := served.Add(500 * time.Microsecond)
	p.SetAppClock(func(real time.Time) (time.Time, bool) {
		if real.Before(moved) {
			return real.Add(21 * time.Second), true
		}
		return real.Add(time.Hour), true
	})
	if s, ok := p.learnTracker.LiveSpan(served.Add(time.Millisecond)); ok {
		t.Fatalf("span %+v across the move with no run handed", s)
	}
	p.SetAppClockRun(func(time.Time) (time.Time, bool) { return moved, true })
	if s, ok := p.learnTracker.LiveSpan(served.Add(time.Millisecond)); !ok || !s.From.Equal(moved.Add(time.Hour)) {
		t.Fatalf("span %+v,%v; want it from the run the agent handed the proxy", s, ok)
	}
}

// h2KeepAliveParser is a real HTTP/2 framing parser that relays an open
// persistent HTTP/2 connection between client and upstream and emits one
// models.HTTP2 mock per completed stream exchange into syncMock.FromContextOrGlobal(ctx).
type h2KeepAliveParser struct{}

func (h2KeepAliveParser) MatchType(_ context.Context, buf []byte) bool {
	return bytes.HasPrefix(buf, []byte(http2.ClientPreface))
}

func (h2KeepAliveParser) RecordOutgoing(ctx context.Context, sess *integrations.RecordSession) error {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(sess.Ingress, preface); err != nil {
		return err
	}
	if _, err := sess.Egress.Write(preface); err != nil {
		return err
	}
	inFramer := http2.NewFramer(sess.Ingress, sess.Ingress)
	outFramer := http2.NewFramer(sess.Egress, sess.Egress)
	for {
		fr, err := inFramer.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		reqTs := time.Now()
		hf, ok := fr.(*http2.HeadersFrame)
		if !ok {
			continue
		}
		if err := outFramer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      hf.StreamID,
			BlockFragment: hf.HeaderBlockFragment(),
			EndStream:     hf.StreamEnded(),
			EndHeaders:    hf.HeadersEnded(),
		}); err != nil {
			return err
		}
		respFr, err := outFramer.ReadFrame()
		if err != nil {
			return err
		}
		resTs := time.Now()
		rhf, ok := respFr.(*http2.HeadersFrame)
		if !ok {
			return fmt.Errorf("unexpected h2 upstream frame %T", respFr)
		}
		if err := inFramer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      rhf.StreamID,
			BlockFragment: rhf.HeaderBlockFragment(),
			EndStream:     rhf.StreamEnded(),
			EndHeaders:    rhf.HeadersEnded(),
		}); err != nil {
			return err
		}
		mk := &models.Mock{
			Kind:         models.HTTP2,
			ConnectionID: sess.ClientConnID,
			Spec: models.MockSpec{
				ReqTimestampMock: reqTs,
				ResTimestampMock: resTs,
				Metadata:         map[string]string{"stream_id": strconv.FormatUint(uint64(hf.StreamID), 10)},
			},
		}
		syncMock.FromContextOrGlobal(ctx).AddMock(mk)
	}
}

func (h2KeepAliveParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return errors.New("h2KeepAliveParser records only")
}

func (h2KeepAliveParser) MockNoise() mocknoise.Adapter { return nil }

// pgPoolKeepAliveParser relays a persistent Postgres wire-protocol simple-query
// connection ('Q' query -> 'C' command-complete + 'Z' ready-for-query) and
// emits one models.Postgres mock per query into syncMock.FromContextOrGlobal(ctx).
type pgPoolKeepAliveParser struct{}

func (pgPoolKeepAliveParser) MatchType(_ context.Context, buf []byte) bool {
	return len(buf) >= 5 && buf[0] == 'Q'
}

func readPGMessage(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgLen := int(binary.BigEndian.Uint32(hdr[1:5]))
	if msgLen < 4 || msgLen > 64*1024 {
		return 0, nil, fmt.Errorf("invalid pg msg length %d", msgLen)
	}
	payload := make([]byte, msgLen-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	full := append(hdr[:], payload...)
	return hdr[0], full, nil
}

func (pgPoolKeepAliveParser) RecordOutgoing(ctx context.Context, sess *integrations.RecordSession) error {
	for {
		msgType, qFrame, err := readPGMessage(sess.Ingress)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msgType != 'Q' {
			continue
		}
		reqTs := time.Now()
		if _, err := sess.Egress.Write(qFrame); err != nil {
			return err
		}
		// Relay upstream 'C' (CommandComplete) and 'Z' (ReadyForQuery).
		for {
			rt, respFrame, err := readPGMessage(sess.Egress)
			if err != nil {
				return err
			}
			if _, err := sess.Ingress.Write(respFrame); err != nil {
				return err
			}
			if rt == 'Z' {
				break
			}
		}
		resTs := time.Now()
		queryText := strings.TrimSuffix(string(qFrame[5:]), "\x00")
		mk := &models.Mock{
			Kind:         models.Postgres,
			ConnectionID: sess.ClientConnID,
			Spec: models.MockSpec{
				ReqTimestampMock: reqTs,
				ResTimestampMock: resTs,
				Metadata:         map[string]string{"query": queryText},
			},
		}
		syncMock.FromContextOrGlobal(ctx).AddMock(mk)
	}
}

func (pgPoolKeepAliveParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return errors.New("pgPoolKeepAliveParser records only")
}

func (pgPoolKeepAliveParser) MockNoise() mocknoise.Adapter { return nil }

func startH2KeepAliveUpstream(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen h2 upstream: %v", err)
	}
	var accepts atomic.Int32
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				preface := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(conn, preface); err != nil {
					return
				}
				fr := http2.NewFramer(conn, conn)
				var hbuf bytes.Buffer
				enc := hpack.NewEncoder(&hbuf)
				for {
					f, err := fr.ReadFrame()
					if err != nil {
						return
					}
					hf, ok := f.(*http2.HeadersFrame)
					if !ok {
						continue
					}
					hbuf.Reset()
					_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
					if err := fr.WriteHeaders(http2.HeadersFrameParam{
						StreamID:      hf.StreamID,
						BlockFragment: append([]byte(nil), hbuf.Bytes()...),
						EndStream:     true,
						EndHeaders:    true,
					}); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), &accepts
}

func startPGPoolKeepAliveUpstream(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen pg upstream: %v", err)
	}
	var accepts atomic.Int32
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				for {
					mt, _, err := readPGMessage(conn)
					if err != nil || mt != 'Q' {
						return
					}
					tag := "SELECT 1\x00"
					var cc [5]byte
					cc[0] = 'C'
					binary.BigEndian.PutUint32(cc[1:5], uint32(4+len(tag)))
					if _, err := conn.Write(append(cc[:], []byte(tag)...)); err != nil {
						return
					}
					rfq := []byte{'Z', 0, 0, 0, 5, 'I'}
					if _, err := conn.Write(rfq); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), &accepts
}

func sendH2Request(t *testing.T, fr *http2.Framer, streamID uint32, path string) {
	t.Helper()
	var hbuf bytes.Buffer
	enc := hpack.NewEncoder(&hbuf)
	_ = enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
	_ = enc.WriteField(hpack.HeaderField{Name: ":path", Value: path})
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: hbuf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		t.Fatalf("write h2 stream %d: %v", streamID, err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read h2 response stream %d: %v", streamID, err)
	}
	if _, ok := f.(*http2.HeadersFrame); !ok {
		t.Fatalf("stream %d response frame = %T, want *http2.HeadersFrame", streamID, f)
	}
}

func sendPGQuery(t *testing.T, conn net.Conn, sql string) {
	t.Helper()
	payload := sql + "\x00"
	var hdr [5]byte
	hdr[0] = 'Q'
	binary.BigEndian.PutUint32(hdr[1:5], uint32(4+len(payload)))
	if _, err := conn.Write(append(hdr[:], []byte(payload)...)); err != nil {
		t.Fatalf("write pg query %q: %v", sql, err)
	}
	for {
		mt, _, err := readPGMessage(conn)
		if err != nil {
			t.Fatalf("read pg response for %q: %v", sql, err)
		}
		if mt == 'Z' {
			return
		}
	}
}

// TestProxyRecordRebindsOutputChannelForKeptAliveHTTP2AndDBConns drives two
// real kept-alive connections through Proxy.handleConnection — one HTTP/2
// framed stream and one Postgres wire-protocol DB-pool connection — keeping
// both sockets open across Session 1 -> Session 2 (abandoned m1) ->
// CloseOutChan() -> Session 3 (m3). Every mock emitted on those persistent
// connections must land on the active session's output channel without opening
// a new connection or issuing a DNS lookup.
func TestProxyRecordRebindsOutputChannelForKeptAliveHTTP2AndDBConns(t *testing.T) {
	mgr := syncMock.Get()
	t.Cleanup(func() { mgr.SetOutputChannel(nil) })

	h2Addr, h2Accepts := startH2KeepAliveUpstream(t)
	pgAddr, pgAccepts := startPGPoolKeepAliveUpstream(t)

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	dest := &switchableDestInfo{dest: ipv4Dest(t, h2Addr)}
	p := New(zap.NewNop(), dest, cfg)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
		integrations.HTTP2:       h2KeepAliveParser{},
		integrations.POSTGRES_V2: pgPoolKeepAliveParser{},
	}
	p.integrationsPriority = []ParserPriority{
		{Priority: 1, ParserType: integrations.HTTP2},
		{Priority: 2, ParserType: integrations.POSTGRES_V2},
	}

	ctx := context.Background()
	opts := models.OutgoingOptions{DisableMysqlAutoDetect: true}

	// Session 1: open persistent HTTP/2 and Postgres connections once.
	m1 := make(chan *models.Mock, 16)
	if err := p.Record(ctx, m1, opts); err != nil {
		t.Fatalf("session 1 Proxy.Record: %v", err)
	}

	h2Client, h2Src := acceptedConn(t)
	h2Done := make(chan error, 1)
	go func() { h2Done <- p.handleConnection(ctx, h2Src) }()
	if _, err := io.WriteString(h2Client, http2.ClientPreface); err != nil {
		t.Fatalf("write h2 preface: %v", err)
	}
	h2Framer := http2.NewFramer(h2Client, h2Client)
	sendH2Request(t, h2Framer, 1, "/s1")

	dest.set(ipv4Dest(t, pgAddr))
	pgClient, pgSrc := acceptedConn(t)
	pgDone := make(chan error, 1)
	go func() { pgDone <- p.handleConnection(ctx, pgSrc) }()
	sendPGQuery(t, pgClient, "SELECT 1 /* s1 */")

	t.Cleanup(func() {
		_ = h2Client.Close()
		_ = pgClient.Close()
		select {
		case <-h2Done:
		case <-time.After(2 * time.Second):
		}
		select {
		case <-pgDone:
		case <-time.After(2 * time.Second):
		}
	})

	awaitKinds := func(ch <-chan *models.Mock, wantH2, wantPG int, label string) {
		t.Helper()
		gotH2, gotPG := 0, 0
		for range wantH2 + wantPG {
			select {
			case got := <-ch:
				switch got.Kind {
				case models.HTTP2:
					gotH2++
				case models.Postgres:
					gotPG++
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: timed out waiting for mocks (got h2=%d pg=%d, want h2=%d pg=%d)", label, gotH2, gotPG, wantH2, wantPG)
			}
		}
		if gotH2 != wantH2 || gotPG != wantPG {
			t.Fatalf("%s: got h2=%d pg=%d, want h2=%d pg=%d", label, gotH2, gotPG, wantH2, wantPG)
		}
	}

	awaitKinds(m1, 1, 1, "session 1")

	// Session 2 (abandoned m1, simulating abrupt control-plane crash/restart):
	// reuse the SAME already-open HTTP/2 and Postgres sockets with zero new
	// accepts. Both pre-request startup mocks and windowed mocks must reach m2.
	m2 := make(chan *models.Mock, 16)
	if err := p.Record(ctx, m2, opts); err != nil {
		t.Fatalf("session 2 Proxy.Record: %v", err)
	}
	sendH2Request(t, h2Framer, 3, "/s2-startup")
	sendPGQuery(t, pgClient, "SELECT 2 /* s2-startup */")
	awaitKinds(m2, 1, 1, "session 2 pre-request")

	// Windowed (post-FirstRequestSeen) traffic on the same kept-alive sockets.
	mgr.SetFirstRequestSignaled()
	wStart := time.Now().Add(-50 * time.Millisecond)
	sendH2Request(t, h2Framer, 5, "/s2-windowed")
	sendPGQuery(t, pgClient, "SELECT 3 /* s2-windowed */")
	mgr.ResolveRange(wStart, time.Now().Add(50*time.Millisecond), "test-resumed-1", true, true)
	awaitKinds(m2, 1, 1, "session 2 windowed")

	if len(m1) != 0 {
		t.Fatalf("abandoned channel m1 received %d leaked mock(s)", len(m1))
	}

	// Session 3 (explicit CloseOutChan between sessions): Proxy.Record must
	// reopen and bind m3 so kept-alive HTTP/2 and DB-pool mocks flow to m3.
	mgr.CloseOutChan()
	m3 := make(chan *models.Mock, 16)
	if err := p.Record(ctx, m3, opts); err != nil {
		t.Fatalf("session 3 Proxy.Record: %v", err)
	}
	sendH2Request(t, h2Framer, 7, "/s3-after-close")
	sendPGQuery(t, pgClient, "SELECT 4 /* s3-after-close */")
	awaitKinds(m3, 1, 1, "session 3 after CloseOutChan")

	if got := h2Accepts.Load(); got != 1 {
		t.Fatalf("h2 upstream accepted %d connections, want 1 (kept-alive)", got)
	}
	if got := pgAccepts.Load(); got != 1 {
		t.Fatalf("pg upstream accepted %d connections, want 1 (kept-alive)", got)
	}
}

type switchableDestInfo struct {
	mu   sync.Mutex
	dest *agent.NetworkAddress
}

func (s *switchableDestInfo) set(d *agent.NetworkAddress) {
	s.mu.Lock()
	s.dest = d
	s.mu.Unlock()
}

func (s *switchableDestInfo) Get(_ context.Context, _ uint16) (*agent.NetworkAddress, error) {
	s.mu.Lock()
	d := s.dest
	s.mu.Unlock()
	return d, nil
}

func (s *switchableDestInfo) Delete(_ context.Context, _ uint16) error { return nil }

// TestStaleSessionDNSDoesNotRollBackOutputChannel verifies that once
// Proxy.Record installs a new session (m2), a delayed DNS mock callback
// carrying the previous session (staleS1 -> m1) cannot roll syncMock.Get()
// back to the abandoned/closed channel m1.
func TestStaleSessionDNSDoesNotRollBackOutputChannel(t *testing.T) {
	mgr := syncMock.Get()
	t.Cleanup(func() { mgr.SetOutputChannel(nil) })

	// Spin up a local UDP DNS responder answering A queries for stale.local.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp dns: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var req dns.Msg
			if err := req.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := new(dns.Msg)
			resp.SetReply(&req)
			for _, q := range req.Question {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
					A:   net.ParseIP("10.0.0.42"),
				})
			}
			raw, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(raw, addr)
		}
	}()
	host, port, err := net.SplitHostPort(pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("split dns addr: %v", err)
	}

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), nil, cfg)
	p.dnsUpstreamServers = []string{host}
	p.dnsUpstreamPort = port
	p.recordedDNSMocks = newRecordedDNSMocksCache()

	m1 := make(chan *models.Mock, 8)
	if err := p.Record(context.Background(), m1, models.OutgoingOptions{}); err != nil {
		t.Fatalf("session 1 Proxy.Record: %v", err)
	}
	staleS1 := p.getSession()

	// Session 2 replaces session 1.
	m2 := make(chan *models.Mock, 8)
	if err := p.Record(context.Background(), m2, models.OutgoingOptions{}); err != nil {
		t.Fatalf("session 2 Proxy.Record: %v", err)
	}

	// Late DNS goroutine from session 1 finishes and invokes recordDNSMock
	// with staleS1: it must NOT roll syncMock.Get() back to m1.
	_, err = p.recordDNSMock(dns.Question{Name: "stale.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}, time.Now(), staleS1)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}

	// A subsequent mock on syncMock.Get() must still arrive on m2, never m1.
	probe := &models.Mock{
		Kind:         models.HTTP2,
		ConnectionID: "after-stale-dns",
		Spec:         models.MockSpec{ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
	}
	if drop := mgr.AddMock(probe); drop != syncMock.MockTaken {
		t.Fatalf("AddMock after stale DNS = %q, want %q", drop, syncMock.MockTaken)
	}
	foundProbe := false
	deadline := time.After(time.Second)
	for !foundProbe {
		select {
		case got := <-m2:
			if got == probe {
				foundProbe = true
			}
		case <-deadline:
			t.Fatalf("probe mock did not arrive on m2 after stale DNS (m1 len=%d)", len(m1))
		}
	}
	if len(m1) != 0 {
		t.Fatalf("stale session channel m1 received %d mock(s); syncMock rolled back", len(m1))
	}
}

// TestProxyRecordAtomicSessionBindPreventsStaleGoroutineRebind exercises the
// transition from session 1 to session 2 while stale goroutines from session 1
// hammer bindSessionOutput concurrently with Proxy.Record. Because Proxy.Record
// publishes p.session and binds the new output channel atomically inside one
// sessionMu.Lock critical section (setSession), a stale goroutine can never
// rebind m1 after m2 has been bound.
func TestProxyRecordAtomicSessionBindPreventsStaleGoroutineRebind(t *testing.T) {
	mgr := syncMock.Get()
	t.Cleanup(func() { mgr.SetOutputChannel(nil) })

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), nil, cfg)

	for iter := range 25 {
		m1 := make(chan *models.Mock, 8)
		if err := p.Record(context.Background(), m1, models.OutgoingOptions{}); err != nil {
			t.Fatalf("iter %d session 1 Record: %v", iter, err)
		}
		staleS1 := p.getSession()

		m2 := make(chan *models.Mock, 8)
		stopStale := make(chan struct{})
		var staleWG sync.WaitGroup
		for range 4 {
			staleWG.Add(1)
			go func() {
				defer staleWG.Done()
				for {
					select {
					case <-stopStale:
						return
					default:
						p.bindSessionOutput(mgr, staleS1)
					}
				}
			}()
		}

		// Record(m2) runs while stale goroutines hammer bindSessionOutput(staleS1).
		if err := p.Record(context.Background(), m2, models.OutgoingOptions{}); err != nil {
			close(stopStale)
			staleWG.Wait()
			t.Fatalf("iter %d session 2 Record: %v", iter, err)
		}

		// Even while stale goroutines are still hammering bindSessionOutput(staleS1),
		// every mock emitted after Record(m2) returns must land on m2, never m1.
		probe := &models.Mock{
			Kind:         models.HTTP2,
			ConnectionID: fmt.Sprintf("atomic-bind-%d", iter),
			Spec:         models.MockSpec{ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		}
		if drop := mgr.AddMock(probe); drop != syncMock.MockTaken {
			close(stopStale)
			staleWG.Wait()
			t.Fatalf("iter %d AddMock = %q, want %q", iter, drop, syncMock.MockTaken)
		}
		close(stopStale)
		staleWG.Wait()

		select {
		case got := <-m2:
			if got != probe {
				t.Fatalf("iter %d m2 received unexpected mock %v", iter, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("iter %d probe did not arrive on m2 (m1 len=%d)", iter, len(m1))
		}
		if len(m1) != 0 {
			t.Fatalf("iter %d stale m1 received %d mock(s); atomic bind broken", iter, len(m1))
		}
	}
}
