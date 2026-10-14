package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	mysqlmodels "go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// recordingMySQLParser captures which SURFACE the dispatcher handed it.
// The V2 path sets RecordSession.V2 and leaves Ingress/Egress nil; the
// legacy path does the opposite. That difference is the only observable
// the dispatch decision produces, so it is what the test asserts on.
type recordingMySQLParser struct {
	v2Declared bool

	mu       sync.Mutex
	called   bool
	sawV2    bool
	sawSocks bool
}

func (f *recordingMySQLParser) MatchType(context.Context, []byte) bool { return false }
func (f *recordingMySQLParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.sawV2 = s.V2 != nil
	f.sawSocks = s.Ingress != nil || s.Egress != nil
	return nil
}
func (f *recordingMySQLParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}
func (f *recordingMySQLParser) IsV2() bool { return f.v2Declared }

func (f *recordingMySQLParser) snapshot() (called, v2, socks bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called, f.sawV2, f.sawSocks
}

// TestMySQLProbeBranchRoutesByShouldRecordViaSupervisor pins the dispatch
// decision of the MySQL PROBE branch behaviourally.
//
// This is the only test that covers the gate itself. The full-stack test
// calls recordViaSupervisor directly, so it stays green with the gate
// reverted, negated, or short-circuited; and a source-scanning pin is
// defeated by `if false &&` or by inserting a `!`. Here the parser reports
// which surface it was handed, so any of those mutations flips the
// observable and fails.
func TestMySQLProbeBranchRoutesByShouldRecordViaSupervisor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		v2       bool
		relayOff string
		wantV2   bool
		why      string
	}{
		{
			name: "V2 parser goes to the supervisor", v2: true, wantV2: true,
			why: "the probe branch recorded legacy on the default config while the matched-parser branch recorded V2 — one parser, two answers",
		},
		{
			name: "non-V2 parser stays legacy", v2: false, wantV2: false,
			why: "a parser that opts out must not be handed FakeConns it cannot use",
		},
		{
			// The rollback knob was removed. A stale KEPLOY_NEW_RELAY in the
			// environment must not divert the probe branch back to legacy.
			name: "the removed rollback knob no longer forces legacy", v2: true, relayOff: "off", wantV2: true,
			why: "KEPLOY_NEW_RELAY is gone; leaving it set must be inert at this dispatch site too",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.relayOff != "" {
				t.Setenv("KEPLOY_NEW_RELAY", tc.relayOff)
			}

			parser := &recordingMySQLParser{v2Declared: tc.v2}
			p := &Proxy{
				logger:                     zap.NewNop(),
				Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
				recordBufferCap:            8 << 20,
				recordBufferQueueSize:      64,
				recordBufferStallGrace:     5 * time.Second,
				recordBufferHalfCloseGrace: 5 * time.Second,
			}

			srcRaw, destRaw, cleanup := tcpConnPair(t)
			defer cleanup()
			// Mirror handleConnection: the upgrader holds pointers to the
			// caller's own conn variables, never to a callee's copies.
			// handleConnection reads every destination through the
			// connseq.Upstream its dial made (util.DialUpstream).
			srcConn, destConn := srcRaw, net.Conn(connseq.NewUpstream(destRaw))
			upgrader := util.NewConnTLSUpgrader(&srcConn, &destConn, zap.NewNop(), nil)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errGrp, gctx := errgroup.WithContext(ctx)
			mocks := make(chan *models.Mock, 8)

			if err := p.recordMySQLOutgoing(gctx, srcConn, destConn, mocks, errGrp,
				zap.NewNop(), 1, 2, models.OutgoingOptions{
					DstCfg: &models.ConditionalDstCfg{Addr: destConn.RemoteAddr().String()},
				}, upgrader); err != nil {
				t.Fatalf("recordMySQLOutgoing: %v", err)
			}

			called, sawV2, sawSocks := parser.snapshot()
			if !called {
				t.Fatal("the parser was never invoked at all")
			}
			if sawV2 != tc.wantV2 {
				t.Fatalf("parser was handed V2=%v, want V2=%v.\n%s", sawV2, tc.wantV2, tc.why)
			}
			// The two surfaces are mutually exclusive by construction; assert
			// it so a session that carries both never passes silently.
			if tc.wantV2 && sawSocks {
				t.Error("V2 session also carried Ingress/Egress sockets the relay owns")
			}
			if !tc.wantV2 && !sawSocks {
				t.Error("legacy session carried no sockets")
			}
		})
	}
}

// tcpConnPair returns a connected pair of real TCP conns. Real sockets,
// not net.Pipe: the relay needs CloseWrite and deadline support.
func tcpConnPair(t *testing.T) (client, server net.Conn, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()

	dialed, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return dialed, got.c, func() {
		_ = dialed.Close()
		_ = got.c.Close()
	}
}

// TestRecordMySQLOutgoing_UnregisteredParser covers the case where the
// probe says IsMySQL but no parser is registered — reachable on a build
// that omits it, because the configured-port and known-port verdicts never
// check registration.
//
// It used to nil-panic; then it returned an error, which unwound to
// handleConnection's deferred close and dropped the user's connection.
// Neither is right: not having a parser is a reason to stop PARSING, not a
// reason to end someone's connection. It now relays.
func TestRecordMySQLOutgoing_UnregisteredParser(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(c, c)
	}()

	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     5 * time.Second,
		recordBufferHalfCloseGrace: 5 * time.Second,
	}

	appSide, proxySide, cleanup := tcpConnPair(t)
	defer cleanup()
	dstConn, err := util.DialUpstream(context.Background(), &net.Dialer{Timeout: 5 * time.Second}, "tcp", upstream.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer func() { _ = dstConn.Close() }()

	src, dst := proxySide, net.Conn(dstConn)
	upgrader := util.NewConnTLSUpgrader(&src, &dst, zap.NewNop(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errGrp, gctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")

	go func() {
		_ = p.recordMySQLOutgoing(ctx, proxySide, dstConn, make(chan *models.Mock, 1), errGrp,
			zap.NewNop(), 1, 2, models.OutgoingOptions{}, upgrader)
	}()

	if _, err := appSide.Write([]byte("ping")); err != nil {
		t.Fatalf("app write: %v", err)
	}
	_ = appSide.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(appSide, buf); err != nil {
		t.Fatalf("an unregistered parser dropped the connection instead of relaying it: %v", err)
	}
}

// upgradingParser drives the legacy TLSUpgrader the way the MySQL
// recorder's STARTTLS path does (recorder/conn.go UpgradeDestTLS).
type upgradingParser struct {
	recordingMySQLParser
	upgradeErr error
}

func (u *upgradingParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	u.mu.Lock()
	u.called = true
	u.sawV2 = s.V2 != nil
	u.mu.Unlock()
	if s.TLSUpgrader == nil {
		u.upgradeErr = fmt.Errorf("legacy session carried no TLSUpgrader")
		return nil
	}
	if _, err := s.TLSUpgrader.UpgradeDestTLS(&tls.Config{InsecureSkipVerify: true}); err != nil { //nolint:gosec // test dial
		u.upgradeErr = err
	}
	return nil
}

// TestRecordMySQLOutgoing_TLSUpgradeRebindsTheCallersConn pins the
// pointer contract that makes deferred close correct after a STARTTLS.
//
// ConnTLSUpgrader exists to write the upgraded *tls.Conn back through
// the &srcConn/&dstConn pointers it was given, so handleConnection's
// deferred close acts on the TLS conn and sends close_notify. Build the
// upgrader inside the dispatch helper instead of at the call site and
// those pointers alias the helper's PARAMETER COPIES: the upgrade still
// succeeds, the parser still works, every other test stays green — and
// the caller closes the raw socket, so the peer sees a reset instead of
// a clean shutdown. That is precisely the regression this test exists to
// catch, because it has no other symptom.
func TestRecordMySQLOutgoing_TLSUpgradeRebindsTheCallersConn(t *testing.T) {
	// A real TLS server for the dest side to hand-shake against.
	cert := selfSignedForDispatch(t)
	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer func() { _ = tlsLn.Close() }()
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()

	parser := &upgradingParser{recordingMySQLParser: recordingMySQLParser{v2Declared: false}}
	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     5 * time.Second,
		recordBufferHalfCloseGrace: 5 * time.Second,
	}

	srcRaw, _, cleanup := tcpConnPair(t)
	defer cleanup()
	dstRaw, err := net.DialTimeout("tcp", tlsLn.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial tls listener: %v", err)
	}
	defer func() { _ = dstRaw.Close() }()

	// These stand in for handleConnection's own variables — the ones its
	// deferred close operates on.
	var srcConn net.Conn = srcRaw
	var dstConn net.Conn = dstRaw
	before := dstConn

	upgrader := util.NewConnTLSUpgrader(&srcConn, &dstConn, p.logger, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errGrp, gctx := errgroup.WithContext(ctx)

	if err := p.recordMySQLOutgoing(gctx, srcConn, dstConn, make(chan *models.Mock, 8), errGrp,
		zap.NewNop(), 1, 2, models.OutgoingOptions{}, upgrader); err != nil {
		t.Fatalf("recordMySQLOutgoing: %v", err)
	}
	if parser.upgradeErr != nil {
		t.Fatalf("the parser's dest TLS upgrade failed: %v", parser.upgradeErr)
	}

	if dstConn == before {
		t.Fatal("after the parser upgraded the destination, the CALLER's dstConn still points " +
			"at the raw socket. handleConnection's deferred close will shut down the TCP " +
			"connection instead of the tls.Conn, so no close_notify is sent and the peer sees " +
			"a reset. The TLSUpgrader must be built where the real conn variables live, not " +
			"over a callee's parameter copies.")
	}
	if _, ok := dstConn.(*tls.Conn); !ok {
		t.Fatalf("caller's dstConn is %T after the upgrade, want *tls.Conn", dstConn)
	}
}

func selfSignedForDispatch(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "keploy-dispatch-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestMySQLProbeBranchHonoursTheKillSwitch pins that the record-mode parsing
// kill switch actually disables parsing on the MySQL probe branch.
//
// It did not. The only gate on util.DefaultKillSwitch lives in
// handleConnection's matched-parser section; the probe branch dispatches and
// returns several hundred lines earlier, so KEPLOY_DISABLE_PARSING / SIGUSR1 /
// the admin endpoint were silently inert for MySQL on its normal route —
// probeMysql's zero-cost fast path is the port check against MysqlPorts (3306
// and 4000 by default), so the probe branch IS the common MySQL path. An
// operator disabled parsing, restarted, and every MySQL connection was still
// handed to the parser.
//
// The observable is the parser being invoked at all: with the switch tripped it
// must never be called, whichever surface it would otherwise have been handed.
func TestMySQLProbeBranchHonoursTheKillSwitch(t *testing.T) {
	for _, v2 := range []bool{true, false} {
		name := "V2 parser"
		if !v2 {
			name = "legacy parser"
		}
		t.Run(name, func(t *testing.T) {
			util.DefaultKillSwitch.Trip()
			defer util.DefaultKillSwitch.Reset()

			parser := &recordingMySQLParser{v2Declared: v2}
			p := &Proxy{
				logger:                     zap.NewNop(),
				Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
				recordBufferCap:            8 << 20,
				recordBufferQueueSize:      64,
				recordBufferStallGrace:     5 * time.Second,
				recordBufferHalfCloseGrace: 5 * time.Second,
			}

			srcRaw, destRaw, cleanup := tcpConnPair(t)
			defer cleanup()
			// handleConnection reads every destination through the
			// connseq.Upstream its dial made (util.DialUpstream).
			srcConn, destConn := srcRaw, net.Conn(connseq.NewUpstream(destRaw))
			upgrader := util.NewConnTLSUpgrader(&srcConn, &destConn, zap.NewNop(), nil)

			// globalPassThrough reads these two off the context and type-asserts
			// them as strings, so the passthrough route panics without them.
			// handleConnection sets both well before it reaches the probe
			// branch, so production is safe; the test has to mirror that.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
			ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
			errGrp, gctx := errgroup.WithContext(ctx)

			// globalPassThrough copies until both sides close; closing them
			// immediately lets it return rather than blocking the test.
			_ = srcRaw.Close()
			_ = destRaw.Close()

			_ = p.recordMySQLOutgoing(gctx, srcConn, destConn, make(chan *models.Mock, 8), errGrp,
				zap.NewNop(), 1, 2, models.OutgoingOptions{
					DstCfg: &models.ConditionalDstCfg{Addr: destConn.RemoteAddr().String()},
				}, upgrader)

			if called, _, _ := parser.snapshot(); called {
				t.Fatal("the parser was invoked with the record-mode kill switch tripped — " +
					"KEPLOY_DISABLE_PARSING / SIGUSR1 is documented as routing to raw " +
					"passthrough, and every next_step message now points operators at it")
			}
		})
	}
}

// TestMySQLProbeKillSwitchRelaysAndHonoursHalfClose is the other half of the
// kill-switch contract: not just "the parser was not invoked", but "the
// connection still works".
//
// TestMySQLProbeBranchHonoursTheKillSwitch closes both sockets before
// dispatching and only inspects the parser, so it cannot tell a working
// passthrough from a dropped connection — replacing the relay with a bare
// `return nil` keeps it green. That gap hid a real defect: the gate first
// routed through globalPassThrough, which never calls CloseWrite and returns
// on io.EOF, so a client doing shutdown(SHUT_WR) — the end-of-request signal
// for every EOF-delimited exchange — had its connection torn down instead of
// receiving the reply. relayDeclinedConn documents exactly this and uses
// util.RelayRawPassthrough; the gate now does too.
//
// This matters here specifically because probeMysql's fast path admits by PORT
// alone (MysqlPorts, 3306/4000 by default, zero content inspection), so any
// protocol on those ports reaches this dispatch site, half-closing ones
// included.
func TestMySQLProbeKillSwitchRelaysAndHonoursHalfClose(t *testing.T) {
	util.DefaultKillSwitch.Trip()
	defer util.DefaultKillSwitch.Reset()

	// An upstream that replies only AFTER it observes end-of-request. If the
	// FIN is never forwarded, it never speaks; if the reply is not relayed
	// back, the client never hears it. Either failure mode shows up as an
	// empty read below.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				break
			}
		}
		_, _ = c.Write([]byte("SERVER-REPLY"))
	}()

	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen app: %v", err)
	}
	defer func() { _ = appLn.Close() }()
	appConn, err := net.Dial("tcp", appLn.Addr().String())
	if err != nil {
		t.Fatalf("dial app side: %v", err)
	}
	defer func() { _ = appConn.Close() }()
	srcConn, err := appLn.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	dstConn, err := util.DialUpstream(context.Background(), nil, "tcp", up.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	errGrp, gctx := errgroup.WithContext(ctx)

	parser := &recordingMySQLParser{v2Declared: true}
	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     5 * time.Second,
		recordBufferHalfCloseGrace: 5 * time.Second,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordMySQLOutgoing(gctx, srcConn, dstConn, make(chan *models.Mock, 8), errGrp,
			zap.NewNop(), 1, 2, models.OutgoingOptions{
				DstCfg: &models.ConditionalDstCfg{Addr: dstConn.RemoteAddr().String()},
			}, nil)
	}()

	if _, err := appConn.Write([]byte("REQUEST")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := appConn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	_ = appConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	n, _ := appConn.Read(buf)
	if got := string(buf[:n]); got != "SERVER-REPLY" {
		t.Fatalf("app half-closed its write side and read back %q, want %q — the kill "+
			"switch must relay the connection, not end it. globalPassThrough returns on "+
			"io.EOF without forwarding the FIN or the reply; use util.RelayRawPassthrough", got, "SERVER-REPLY")
	}
	if called, _, _ := parser.snapshot(); called {
		t.Error("the parser was invoked with the kill switch tripped")
	}
	<-done
}

// closeHook runs onClose once, the first time the client's connection is
// closed, before closing it.
type closeHook struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *closeHook) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

// mysqlReplayMisses serves one MySQL connection through the proxy's replay
// dispatch from a session pool holding session, with app playing the
// application, and returns the misses the test it was made in reads. The test
// reads them the moment the application's connection is closed: the earliest a
// harness that ends on the failed database call can read them. A miss queued
// only after that close is therefore missing from what this returns, whatever
// the scheduler does.
func mysqlReplayMisses(t *testing.T, session []*models.Mock, app func(net.Conn)) []models.UnmatchedCall {
	t.Helper()
	// Under the switch the proxy's error drain runs, and GetMockErrors waits
	// for it on events. Without the switch it would wait for the drain on a
	// two-second clock, so the proxy runs without one there: GetMockErrors
	// then sweeps the error channel itself, and reads whatever was queued by
	// the time the connection is closed, with no clock involved.
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	if provenswitch.On() {
		p = startedDrainProxy(t, 16)
	}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: mysqlparser.New(zap.NewNop())}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	now := time.Now()
	mm.SetMocksWithWindow(nil, session, now.Add(-time.Hour), now.Add(time.Hour))

	ctx := context.Background()
	p.BeginTestErrorCapture() // the test begins
	appEnd, pipeEnd := net.Pipe()
	t.Cleanup(func() { _ = appEnd.Close() })
	var misses []models.UnmatchedCall
	var readErr error
	agentEnd := &closeHook{Conn: pipeEnd, onClose: func() {
		misses, readErr = p.GetMockErrors(ctx) // the test ends: its database call failed
	}}
	connCtx := context.WithValue(context.WithValue(ctx, models.ClientConnectionIDKey, "1"), models.DestConnectionIDKey, "2")
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = p.mockMySQLOutgoing(connCtx, agentEnd, "10.0.0.5:3306", mm, models.OutgoingOptions{})
		_ = agentEnd.Close() // handleConnection's deferred close
	}()
	app(appEnd)
	<-closed // the misses were read: onClose has run
	if readErr != nil {
		t.Fatal(readErr)
	}
	return misses
}

// eachSwitchState runs test with the proven switch off and on: a MySQL miss is
// queued before its connection is closed either way.
func eachSwitchState(t *testing.T, test func(t *testing.T)) {
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			test(t)
		})
	}
}

// requireOneMySQLMiss fails the test unless misses is the one MySQL miss of a
// connection to 10.0.0.5:3306 that summary names.
func requireOneMySQLMiss(t *testing.T, misses []models.UnmatchedCall, summary string) {
	t.Helper()
	if len(misses) != 1 {
		t.Fatalf("the test read %d misses when its application's MySQL connection was closed; want its one miss (with none, the test passes although its database call failed): %+v", len(misses), misses)
	}
	if m := misses[0]; m.Protocol != "MySQL" || m.ActualSummary != summary || m.Destination != "10.0.0.5:3306" {
		t.Fatalf("miss %+v; want a MySQL miss of %q at 10.0.0.5:3306", m, summary)
	}
}

// A MySQL connection the test set holds no session mock for is closed
// unanswered, and the test it was made in has that miss by the time the
// application sees its connection fail. Before, the parser returned nil and
// the test passed with no miss.
func TestAMySQLConnectionWithNoMocksIsAMissOfItsTest(t *testing.T) {
	eachSwitchState(t, func(t *testing.T) {
		misses := mysqlReplayMisses(t, nil, func(app net.Conn) {
			if got, err := io.ReadAll(app); len(got) != 0 || err != nil {
				t.Errorf("the application read %q, %v; want its connection closed unanswered", got, err)
			}
		})
		requireOneMySQLMiss(t, misses, "connection phase: new connection")
	})
}

// A MySQL handshake no recorded one matches is a miss of the test it was made
// in, by the time the application sees its connection fail. Before, the
// parser's error did not wrap models.ErrNoMockMatched, so the proxy dropped
// it from the test's misses and the test passed.
func TestAMySQLHandshakeNoMockMatchesIsAMissOfItsTest(t *testing.T) {
	const caps = uint32(mysqlmodels.CLIENT_PROTOCOL_41 | mysqlmodels.CLIENT_PLUGIN_AUTH | mysqlmodels.CLIENT_SECURE_CONNECTION)
	hr41 := func(user string) *mysqlmodels.HandshakeResponse41Packet {
		return &mysqlmodels.HandshakeResponse41Packet{CapabilityFlags: caps, MaxPacketSize: 1 << 24, CharacterSet: 0x21,
			Username: user, AuthResponse: make([]byte, 20), AuthPluginName: string(mysqlmodels.Native)}
	}
	packet := func(seq uint8, typ string, msg interface{}) mysqlmodels.PacketBundle {
		return mysqlmodels.PacketBundle{Header: &mysqlmodels.PacketInfo{Header: &mysqlmodels.Header{SequenceID: seq}, Type: typ}, Message: msg}
	}
	handshake := &models.Mock{
		Kind: models.MySQL,
		Name: "mock-0",
		Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "config", "connID": "0"},
			MySQLRequests: []mysqlmodels.Request{{PacketBundle: packet(1, mysqlmodels.HandshakeResponse41, hr41("app"))}},
			MySQLResponses: []mysqlmodels.Response{
				{PacketBundle: packet(0, mysqlmodels.AuthStatusToString(mysqlmodels.HandshakeV10), &mysqlmodels.HandshakeV10Packet{
					ProtocolVersion: 0x0a, ServerVersion: "8.0.36", ConnectionID: 7, AuthPluginData: make([]byte, 20),
					CapabilityFlags: caps, CharacterSet: 0x21, StatusFlags: 2, AuthPluginName: string(mysqlmodels.Native)})},
				{PacketBundle: packet(2, mysqlmodels.StatusToString(mysqlmodels.OK), &mysqlmodels.OKPacket{Header: mysqlmodels.OK, StatusFlags: 2})},
			},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
	}
	eachSwitchState(t, func(t *testing.T) {
		misses := mysqlReplayMisses(t, []*models.Mock{handshake}, func(app net.Conn) {
			if _, err := readOneMySQLPacket(app); err != nil {
				t.Errorf("reading the greeting: %v", err)
				return
			}
			payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr41("intruder"))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := app.Write(append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 1}, payload...)); err != nil {
				t.Errorf("sending the HandshakeResponse41: %v", err)
				return
			}
			if got, err := io.ReadAll(app); len(got) != 0 || err != nil {
				t.Errorf("the application read %q, %v after its handshake; want its connection closed unanswered", got, err)
			}
		})
		requireOneMySQLMiss(t, misses, "connection phase: HandshakeResponse41")
	})
}

// mysqlTestHR41 is the HandshakeResponse41 a client of user sends, asking for
// plugin.
func mysqlTestHR41(user string, plugin mysqlmodels.AuthPluginName) *mysqlmodels.HandshakeResponse41Packet {
	const caps = uint32(mysqlmodels.CLIENT_PROTOCOL_41 | mysqlmodels.CLIENT_PLUGIN_AUTH | mysqlmodels.CLIENT_SECURE_CONNECTION)
	return &mysqlmodels.HandshakeResponse41Packet{CapabilityFlags: caps, MaxPacketSize: 1 << 24, CharacterSet: 0x21,
		Username: user, AuthResponse: make([]byte, 20), AuthPluginName: string(plugin)}
}

// mysqlTestPacket is one recorded packet.
func mysqlTestPacket(seq uint8, typ string, msg interface{}) mysqlmodels.PacketBundle {
	return mysqlmodels.PacketBundle{Header: &mysqlmodels.PacketInfo{Header: &mysqlmodels.Header{SequenceID: seq}, Type: typ}, Message: msg}
}

// mysqlTestVerdict is the server's final answer on a login, at seq: its OK, or
// its refusal.
func mysqlTestVerdict(seq uint8, refused bool) mysqlmodels.Response {
	if refused {
		return mysqlmodels.Response{PacketBundle: mysqlTestPacket(seq, mysqlmodels.StatusToString(mysqlmodels.ERR),
			&mysqlmodels.ERRPacket{Header: mysqlmodels.ERR, ErrorCode: 1045, SQLStateMarker: "#", SQLState: "28000", ErrorMessage: "Access denied"})}
	}
	return mysqlmodels.Response{PacketBundle: mysqlTestPacket(seq, mysqlmodels.StatusToString(mysqlmodels.OK), &mysqlmodels.OKPacket{Header: mysqlmodels.OK, StatusFlags: 2})}
}

// mysqlTestHandshake is a recorded handshake of user "app" with
// 10.0.0.5:3306 asking for plugin, recorded at at: the greeting and the
// client's HandshakeResponse41, then the server's answers and the client's
// further packets.
func mysqlTestHandshake(name string, at time.Time, plugin mysqlmodels.AuthPluginName, answers []mysqlmodels.Response, requests ...mysqlmodels.Request) *models.Mock {
	return &models.Mock{
		Kind: models.MySQL,
		Name: name,
		Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "config", "connID": name, "destAddr": "10.0.0.5:3306"},
			MySQLRequests: append([]mysqlmodels.Request{{PacketBundle: mysqlTestPacket(1, mysqlmodels.HandshakeResponse41, mysqlTestHR41("app", plugin))}}, requests...),
			MySQLResponses: append([]mysqlmodels.Response{
				{PacketBundle: mysqlTestPacket(0, mysqlmodels.AuthStatusToString(mysqlmodels.HandshakeV10), &mysqlmodels.HandshakeV10Packet{
					ProtocolVersion: 0x0a, ServerVersion: "8.0.36", ConnectionID: 7, AuthPluginData: make([]byte, 20),
					CapabilityFlags: mysqlTestHR41("app", plugin).CapabilityFlags, CharacterSet: 0x21, StatusFlags: 2, AuthPluginName: string(plugin)})},
			}, answers...),
			ReqTimestampMock: at,
			ResTimestampMock: at,
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
	}
}

// mysqlTestNative is a recorded mysql_native_password login, refused or not.
func mysqlTestNative(name string, at time.Time, refused bool) *models.Mock {
	return mysqlTestHandshake(name, at, mysqlmodels.Native, []mysqlmodels.Response{mysqlTestVerdict(2, refused)})
}

// mysqlTestFullAuth is a recorded caching_sha2_password full authentication
// without TLS, refused (a wrong password, on MySQL 8) or not.
func mysqlTestFullAuth(name string, at time.Time, refused bool) *models.Mock {
	return mysqlTestHandshake(name, at, mysqlmodels.CachingSha2,
		[]mysqlmodels.Response{
			{PacketBundle: mysqlTestPacket(2, mysqlmodels.AuthStatusToString(mysqlmodels.AuthMoreData),
				&mysqlmodels.AuthMoreDataPacket{StatusTag: mysqlmodels.AuthMoreData, Data: mysqlmodels.CachingSha2PasswordToString(mysqlmodels.PerformFullAuthentication)})},
			{PacketBundle: mysqlTestPacket(4, mysqlmodels.AuthStatusToString(mysqlmodels.AuthMoreData), &mysqlmodels.AuthMoreDataPacket{StatusTag: mysqlmodels.AuthMoreData, Data: "PUBLIC KEY"})},
			mysqlTestVerdict(6, refused),
		},
		mysqlmodels.Request{PacketBundle: mysqlmodels.PacketBundle{
			Header:  &mysqlmodels.PacketInfo{Header: &mysqlmodels.Header{PayloadLength: 1, SequenceID: 3}, Type: mysqlmodels.CachingSha2PasswordToString(mysqlmodels.RequestPublicKey)},
			Message: "request_public_key",
		}},
		mysqlmodels.Request{PacketBundle: mysqlTestPacket(5, mysqlmodels.EncryptedPassword, "encrypted")},
	)
}

// writeMySQLPacket writes payload to c as one packet with sequence id seq.
func writeMySQLPacket(c net.Conn, seq uint8, payload []byte) error {
	_, err := c.Write(append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}, payload...))
	return err
}

// A login the recording refused and then, retried, let in, is let in on every
// connection, through the real mock manager: at boot, where the manager does
// not move a reused handshake, inside a test window, where it does, and for
// connections opened at once. Which recorded verdict a login gets depends on
// nothing a connection changes in the pool, with mysql_native_password and
// with caching_sha2_password, whose refusal comes at the end of a full
// authentication. No miss is queued for any of them.
func TestAMySQLLoginRefusedThenRetriedIsLetInOnEveryConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		name      string
		plugin    mysqlmodels.AuthPluginName
		handshake func(name string, at time.Time, refused bool) *models.Mock
	}{
		{"mysql_native_password", mysqlmodels.Native, mysqlTestNative},
		{"caching_sha2_password full authentication", mysqlmodels.CachingSha2, mysqlTestFullAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 64)}
			p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: mysqlparser.New(zap.NewNop())}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			rec := time.Now().Add(-time.Hour)
			session := func() []*models.Mock {
				return []*models.Mock{tc.handshake("mock-refused", rec, true), tc.handshake("mock-ok", rec.Add(time.Second), false)}
			}

			// login opens one connection and returns the first byte of the
			// server's final answer on the login.
			login := func() (byte, error) {
				appEnd, agentEnd := net.Pipe()
				connCtx := context.WithValue(context.WithValue(context.Background(), models.ClientConnectionIDKey, "1"), models.DestConnectionIDKey, "2")
				served := make(chan struct{})
				go func() {
					defer close(served)
					_ = p.mockMySQLOutgoing(connCtx, agentEnd, "10.0.0.5:3306", mm, models.OutgoingOptions{})
					_ = agentEnd.Close()
				}()
				defer func() { _ = appEnd.Close(); <-served }()
				if _, err := readOneMySQLPacket(appEnd); err != nil {
					return 0, err
				}
				payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), mysqlTestHR41("app", tc.plugin))
				if err != nil {
					return 0, err
				}
				if err := writeMySQLPacket(appEnd, 1, payload); err != nil {
					return 0, err
				}
				answer, err := readOneMySQLPacket(appEnd)
				if err != nil {
					return 0, err
				}
				if tc.plugin == mysqlmodels.CachingSha2 && answer[4] == mysqlmodels.AuthMoreData {
					// A full authentication: ask for the public key, and send
					// the password encrypted with it.
					if err := writeMySQLPacket(appEnd, 3, []byte{byte(mysqlmodels.RequestPublicKey)}); err != nil {
						return 0, err
					}
					if _, err := readOneMySQLPacket(appEnd); err != nil {
						return 0, err
					}
					if err := writeMySQLPacket(appEnd, 5, make([]byte, 32)); err != nil {
						return 0, err
					}
					if answer, err = readOneMySQLPacket(appEnd); err != nil {
						return 0, err
					}
				}
				return answer[4], nil
			}
			loginOK := func(phase string, i int) {
				t.Helper()
				got, err := login()
				if err != nil || got != mysqlmodels.OK {
					t.Errorf("%s, connection %d: verdict %#x, %v; want the recorded OK", phase, i, got, err)
				}
			}

			p.BeginTestErrorCapture()
			mm.SetMocksWithWindow(nil, session(), models.BaseTime, time.Now().Add(time.Hour)) // boot
			for i := 1; i <= 3; i++ {
				loginOK("at boot", i)
			}
			mm.SetMocksWithWindow(nil, session(), time.Now().Add(-time.Minute), time.Now().Add(time.Hour)) // a test window
			for i := 1; i <= 3; i++ {
				loginOK("in a test window", i)
			}
			var wg sync.WaitGroup
			for i := 1; i <= 4; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					loginOK("at once", i)
				}(i)
			}
			wg.Wait()
			misses, err := p.GetMockErrors(context.Background())
			if err != nil || len(misses) != 0 {
				t.Fatalf("misses %+v, %v; want none: every login was answered from the recording", misses, err)
			}
		})
	}
}
