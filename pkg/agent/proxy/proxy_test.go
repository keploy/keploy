package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
