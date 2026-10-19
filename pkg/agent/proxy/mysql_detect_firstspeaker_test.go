package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// acceptingServer accepts connections, counting them and announcing each on
// accepted, and does with each what serve says.
func acceptingServer(t *testing.T, serve func(c net.Conn)) (addr string, conns *atomic.Int32, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	ch := make(chan struct{}, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			ch <- struct{}{}
			go serve(c)
		}
	}()
	return ln.Addr().String(), &n, ch
}

// silentUntilAsked is a client-speaks-first server: it says nothing until its
// client does, then answers each line with "ok <line>".
func silentUntilAsked(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if _, err := io.WriteString(c, "ok "+line); err != nil {
			return
		}
	}
}

// A pooled connection is opened at startup and first used later: its client
// is silent until then, and so is a client-speaks-first destination. The
// probe must wait for whichever end speaks first and take that as the
// verdict, so the client's first bytes are handed on the moment they arrive.
// It used to give the client 250ms, then wait up to 2s for a greeting with the
// client's bytes unread: a call made in that time was held back and recorded
// late, against the next test.
//
// The client speaks only once the destination has accepted the probe's
// connection, which the old probe did only after its silence window: what
// the probe does next is then decided by events alone.
func TestProbeDecidesByWhicheverEndSpeaksFirst(t *testing.T) {
	addr, conns, accepted := acceptingServer(t, silentUntilAsked)
	port := uint32(mustPort(t, addr))
	p := testProxy(t)
	app, srcConn := clientPair(t)

	type result struct {
		probe *mysqlProbe
		err   error
	}
	done := make(chan result, 1)
	go func() {
		probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, port,
			models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		done <- result{probe, err}
	}()
	awaitEvent(t, accepted, "the probe reaching the destination")
	select {
	case r := <-done:
		t.Fatalf("the probe decided (reason %q) while neither end had said anything", r.probe.Reason)
	default:
	}

	const request = "publish 1\n"
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	r := awaitEvent(t, done, "the probe's verdict once the client spoke")
	if r.err != nil {
		t.Fatalf("probe: %v", r.err)
	}
	if r.probe.IsMySQL || r.probe.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason=%q; the client spoke first, so the verdict is client-spoke-first", r.probe.IsMySQL, r.probe.Reason)
	}
	if !p.mysqlPorts.IsKnownNotMysql(port) {
		t.Error("the client-spoke-first verdict was not cached")
	}
	got := make([]byte, len(request))
	if _, err := io.ReadFull(r.probe.SrcConn, got); err != nil || string(got) != request {
		t.Fatalf("the client's first bytes read back as %q, %v; want %q", got, err, request)
	}
	// And what the client sends next reads too: the probe left no deadline
	// on the connection it handed on.
	const next = "publish 2\n"
	if _, err := io.WriteString(app, next); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len(next))
	if _, err := io.ReadFull(r.probe.SrcConn, got); err != nil || string(got) != next {
		t.Fatalf("the client's next bytes read back as %q, %v; want %q", got, err, next)
	}

	// The upstream the probe reached is the one the connection's dial takes:
	// the destination sees the application's one connection.
	if r.probe.Predialed == nil {
		t.Fatal("the probe dropped the upstream it dialled; the connection's dial would open a second one")
	}
	up, err := util.DialRaw(context.Background(), nil, "tcp", addr, r.probe.Predialed)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if _, err := io.WriteString(up, request); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(up).ReadString('\n')
	if err != nil || answer != "ok "+request {
		t.Fatalf("the destination answered %q, %v; want %q", answer, err, "ok "+request)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("the destination saw %d connections for the application's one", n)
	}
}

// The same with the connection dialled while the application's handshake was
// held: the probe looks at it and hands it back, client bytes and all.
func TestProbeDecidesByWhicheverEndSpeaksFirstOnTheHeldConnection(t *testing.T) {
	addr, conns, accepted := acceptingServer(t, silentUntilAsked)
	upstream, err := util.DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, accepted, "the held connection being accepted")
	pre := util.NewPredialed(addr, upstream)
	defer pre.CloseIfUnused()
	p := testProxy(t)
	app, srcConn := clientPair(t)
	pre.WatchUntilTaken(func() {
		if util.ReceivedNothing(srcConn) {
			_ = srcConn.Close()
		}
	})

	const request = "publish 1\n"
	port := uint32(mustPort(t, addr))
	done := make(chan *mysqlProbe, 1)
	go func() {
		probe, err := p.probeMysql(context.Background(), srcConn, addr, pre, port,
			models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		if err != nil {
			t.Errorf("probe: %v", err)
		}
		done <- probe
	}()
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	probe := awaitEvent(t, done, "the probe's verdict once the client spoke")
	if probe.IsMySQL || probe.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason=%q, want client-spoke-first", probe.IsMySQL, probe.Reason)
	}
	if probe.Predialed != nil {
		t.Fatal("the held connection must go back to its own Predialed, not to a new one")
	}
	up, err := util.DialRaw(context.Background(), nil, "tcp", addr, pre)
	if err != nil {
		t.Fatal(err)
	}
	if !pre.Is(up) {
		t.Fatal("the dial after the probe did not take the held connection back")
	}
	if _, err := io.WriteString(up, request); err != nil {
		t.Fatal(err)
	}
	if answer, err := bufio.NewReader(up).ReadString('\n'); err != nil || answer != "ok "+request {
		t.Fatalf("the destination answered %q, %v", answer, err)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("the destination saw %d connections for the application's one", n)
	}
}

// A MySQL server greets the moment it accepts, before its client says a
// word: that alone decides, however long the client then stays silent.
func TestProbeDetectsTheGreetingWithoutWaitingOnTheClient(t *testing.T) {
	greeting := mysqlGreeting()
	addr, _, _ := acceptingServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write(greeting)
		_, _ = io.Copy(io.Discard, c)
	})
	p := testProxy(t)
	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, uint32(mustPort(t, addr)),
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL || probe.Reason != "detected-handshake" {
		t.Fatalf("IsMySQL=%v reason=%q, want true/detected-handshake", probe.IsMySQL, probe.Reason)
	}
	if probe.Predialed != nil {
		t.Fatal("a positive verdict keeps its upstream as DstConn; it must not also hand it on")
	}
	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(probe.DstConn, got); err != nil || string(got) != string(greeting) {
		t.Fatalf("the greeting replayed as %x, %v", got, err)
	}
}

// relayParser takes every connection and, in a record, relays one
// line-protocol exchange between the application and its destination,
// telling the test what it relayed.
type relayParser struct{ relayed chan string }

func (relayParser) MatchType(context.Context, []byte) bool { return true }

func (r relayParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	req, err := bufio.NewReader(s.Ingress).ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := io.WriteString(s.Egress, req); err != nil {
		return err
	}
	resp, err := bufio.NewReader(s.Egress).ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := io.WriteString(s.Ingress, resp); err != nil {
		return err
	}
	r.relayed <- req
	return nil
}

func (relayParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}

// Through handleConnection, with no held handshake: a pooled connection first
// used after its destination was reached is relayed over the connection the
// probe dialled, so the destination sees one connection and gets the call when
// the application makes it. The old probe dropped its connection after its
// greeting wait and generic dispatch dialled a second one.
func TestHandleConnectionRelaysAPooledConnectionOverTheProbesUpstream(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	addr, conns, accepted := acceptingServer(t, silentUntilAsked)

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, addr)}, cfg)
	parser := relayParser{relayed: make(chan string, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
		integrations.MYSQL: mysql.New(zap.NewNop()), // what the probe matches a greeting with
		"relay":            parser,
	}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: "relay"}}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })

	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	awaitEvent(t, accepted, "the probe reaching the destination")

	const request = "publish 1\n"
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, parser.relayed, "the call being relayed"); got != request {
		t.Fatalf("relayed %q, want %q", got, request)
	}
	answer, err := bufio.NewReader(app).ReadString('\n')
	if err != nil || answer != "ok "+request {
		t.Fatalf("the application read %q, %v; want %q", answer, err, "ok "+request)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("the destination saw %d connections for the application's one", n)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
}

// A replay connection that arrives before its test set's mocks are stored
// waits for them, or for its client to speak, whichever comes first: a client
// that speaks while the mocks are still on their way is handed on at once.
// It used to get 250ms, then wait for the mocks (up to 20s) with its bytes
// unread.
func TestReplayClientSpeakingBeforeTheMocksArriveIsHandedOnAtOnce(t *testing.T) {
	p := testProxy(t) // no mocks stored, ever, in this test
	app, srcConn := clientPair(t)

	done := make(chan *mysqlProbe, 1)
	go func() {
		probe, err := p.probeMysql(context.Background(), srcConn, "10.0.0.7:5672", nil, 5672,
			models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
		if err != nil {
			t.Errorf("probe: %v", err)
		}
		done <- probe
	}()
	// Past the 250ms the old probe gave a silent client, so it was waiting on the mocks
	// by now; the probe under test decides on the bytes whenever they come.
	time.Sleep(750 * time.Millisecond)
	const hello = "AMQP\x00\x00\x09\x01"
	if _, err := io.WriteString(app, hello); err != nil {
		t.Fatal(err)
	}
	probe := awaitEvent(t, done, "the probe's verdict once the client spoke")
	if probe.IsMySQL || probe.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason=%q, want client-spoke-first", probe.IsMySQL, probe.Reason)
	}
	got := make([]byte, len(hello))
	if _, err := io.ReadFull(probe.SrcConn, got); err != nil || string(got) != hello {
		t.Fatalf("the client's bytes read back as %q, %v", got, err)
	}
}

// A silent replay connection waiting for its mocks is released when the run
// ends, with no verdict.
func TestReplayWaitForTheMocksEndsWithTheRun(t *testing.T) {
	p := testProxy(t)
	_, srcConn := clientPair(t)
	base, cancel := context.WithCancel(context.Background())
	ctx := &doneAsked{Context: base, asked: make(chan struct{})}
	done := make(chan *mysqlProbe, 1)
	go func() {
		probe, _ := p.probeMysql(ctx, srcConn, "10.0.0.7:5672", nil, 5672,
			models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
		done <- probe
	}()
	awaitEvent(t, ctx.asked, "the probe waiting on the run")
	cancel()
	probe := awaitEvent(t, done, "the probe returning once the run ended")
	if probe.IsMySQL || probe.Reason != "cancelled" {
		t.Fatalf("IsMySQL=%v reason=%q, want false/cancelled", probe.IsMySQL, probe.Reason)
	}
}

// greetingDecided stops reading exactly when MySQL.MatchType's answer can no
// longer change: never before (a split greeting would be misread as not
// MySQL), and not after (a destination that has said all it will say before
// its client answers would never be decided).
func TestGreetingDecidedOnlyWhenMatchTypeIsFinal(t *testing.T) {
	m := mysql.New(zap.NewNop())
	ctx := context.Background()
	g := mysqlGreeting()
	greeting := g[:4+(int(g[0])|int(g[1])<<8|int(g[2])<<16)] // its first packet
	for n := 0; n < len(greeting); n++ {
		if greetingDecided(ctx, greeting[:n], m) {
			t.Fatalf("decided after %d of the greeting's %d bytes", n, len(greeting))
		}
	}
	if !greetingDecided(ctx, greeting, m) || !m.MatchType(ctx, greeting) {
		t.Fatal("the whole greeting must be decided, and match")
	}

	banner := []byte("220 smtp.example.com ESMTP\r\n")
	if greetingDecided(ctx, banner[:3], m) {
		t.Fatal("decided on 3 bytes of a banner")
	}
	if !greetingDecided(ctx, banner[:4], m) {
		t.Fatal("a text banner declares a packet no handshake has; four bytes decide it")
	}

	// A complete first packet decides, whatever follows; MatchType answers
	// the same with or without what follows.
	short := []byte{0x05, 0x00, 0x00, 0x00, 0x0a, 'x', 0x00, 0x01, 0x02}
	if !greetingDecided(ctx, short, m) {
		t.Fatal("a complete first packet must decide")
	}
	for _, tail := range [][]byte{nil, make([]byte, 64), greeting} {
		buf := append(append([]byte(nil), greeting...), tail...)
		if !m.MatchType(ctx, buf) {
			t.Fatalf("MatchType's answer on a complete greeting changed with %d bytes after it", len(tail))
		}
		buf = append(append([]byte(nil), short...), tail...)
		if m.MatchType(ctx, buf) {
			t.Fatalf("MatchType's answer on a complete non-handshake packet changed with %d bytes after it", len(tail))
		}
	}
}

// maxHandshakePacket is MatchType's own limit: a valid greeting padded to it
// matches, one byte more does not.
func TestMaxHandshakePacketIsMatchTypes(t *testing.T) {
	m := mysql.New(zap.NewNop())
	for _, tc := range []struct {
		body int
		want bool
	}{{maxHandshakePacket, true}, {maxHandshakePacket + 1, false}} {
		g := mysqlGreeting()
		body := append([]byte(nil), g[4:]...)
		body = append(body, make([]byte, tc.body-len(body))...)
		pkt := append([]byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), 0}, body...)
		if got := m.MatchType(context.Background(), pkt); got != tc.want {
			t.Fatalf("MatchType on a %d-byte greeting packet = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func mustPort(t *testing.T, addr string) int {
	t.Helper()
	tcp, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil || !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("not a loopback address: %q (%v)", addr, err)
	}
	return tcp.Port
}

// doneAsked announces the first time its Done is asked for: the replay probe
// asks only as it starts to wait.
type doneAsked struct {
	context.Context
	asked chan struct{}
	once  sync.Once
}

func (c *doneAsked) Done() <-chan struct{} {
	c.once.Do(func() { close(c.asked) })
	return c.Context.Done()
}

// halfClosingServer closes its side of each connection at once and then
// waits for the peer's close, announcing it on closed.
func halfClosingServer(t *testing.T) (addr string, closed <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 4)
	addr, _, _ = acceptingServer(t, func(c net.Conn) {
		defer c.Close()
		_ = c.(*net.TCPConn).CloseWrite()
		_, _ = io.Copy(io.Discard, c)
		ch <- struct{}{}
	})
	return addr, ch
}

// A destination that ends the probe's own connection without a word, while
// the client is silent, is not MySQL. The probe closes that connection — the
// destination sees it closed — and leaves the application's connection open:
// it is not the application's own connection, so generic dispatch dials
// afresh once the client speaks.
func TestProbeClosesItsUpstreamTheDestinationEnded(t *testing.T) {
	noCollector(t)
	addr, closed := halfClosingServer(t)
	p := testProxy(t)
	app, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, uint32(mustPort(t, addr)),
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL || probe.Reason != "upstream-read-error" {
		t.Fatalf("IsMySQL=%v reason=%q, want false/upstream-read-error", probe.IsMySQL, probe.Reason)
	}
	if probe.Predialed != nil {
		t.Fatal("an upstream the destination ended must not be handed on")
	}
	awaitEvent(t, closed, "the destination seeing the probe's connection closed")

	const request = "publish 1\n"
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(request))
	if _, err := io.ReadFull(probe.SrcConn, got); err != nil || string(got) != request {
		t.Fatalf("the application's connection read %q, %v; it must stay open", got, err)
	}
}

// Through handleConnection, with no held handshake: a client that says a few
// bytes and leaves before generic dispatch dials closes the upstream the probe
// handed on — the destination sees the close — instead of leaving it open.
func TestHandleConnectionClosesTheProbesUpstreamNothingTook(t *testing.T) {
	noCollector(t)
	t.Setenv(provenswitch.Env, "0")
	closed := make(chan struct{}, 1)
	addr, conns, accepted := acceptingServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
		closed <- struct{}{}
	})

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, addr)}, cfg)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
		integrations.MYSQL: mysql.New(zap.NewNop()),
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })

	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	awaitEvent(t, accepted, "the probe reaching the destination")
	// Fewer bytes than generic dispatch peeks for, then gone.
	if _, err := app.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
	awaitEvent(t, closed, "the destination seeing the probe's upstream closed")
	if n := conns.Load(); n != 1 {
		t.Fatalf("the destination saw %d connections", n)
	}
}

// noCollector turns the garbage collector off for the test: it closes an
// unreferenced connection by itself (the runtime collects at least every two
// minutes), which would hide a connection the code under test left open.
func noCollector(t *testing.T) {
	t.Helper()
	old := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(old) })
}

// A client that leaves before speaking needs no upstream: a dial still in
// flight for it is abandoned, not waited for.
func TestProbeAbandonsTheDialOfAClientThatLeft(t *testing.T) {
	abandoned := make(chan struct{})
	orig := dialProbeUpstream
	dialProbeUpstream = func(ctx context.Context, _ *zap.Logger, _ string) (net.Conn, error) {
		<-ctx.Done() // a destination that never answers
		close(abandoned)
		return nil, ctx.Err()
	}
	t.Cleanup(func() { dialProbeUpstream = orig })
	p := testProxy(t)
	app, srcConn := clientPair(t)
	_ = app.Close()
	done := make(chan *mysqlProbe, 1)
	go func() {
		probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:7", nil, 7,
			models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		if err != nil {
			t.Errorf("probe: %v", err)
		}
		done <- probe
	}()
	awaitEvent(t, abandoned, "the probe abandoning its dial")
	if probe := awaitEvent(t, done, "the probe returning"); probe.Reason != "client-read-error" || probe.Predialed != nil {
		t.Fatalf("reason %q, handed on %v", probe.Reason, probe.Predialed)
	}
}

type panicOnRead struct{ net.Conn }

func (*panicOnRead) Read([]byte) (int, error) { panic("read panicked") }

// A read of the probe's that panics loses what it had read, so the
// connection is ended rather than handed on with bytes missing; an upstream
// it had reached is closed.
func TestProbeEndsTheConnectionWhenAReadPanics(t *testing.T) {
	addr, _, _ := acceptingServer(t, silentUntilAsked)
	port := uint32(mustPort(t, addr))
	t.Run("the client's, record", func(t *testing.T) {
		p := testProxy(t)
		_, srcConn := clientPair(t)
		probe, err := p.probeMysql(context.Background(), &panicOnRead{srcConn}, addr, nil, port,
			models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		if !errors.Is(err, errLookIncomplete) || probe.Reason != "probe-failed" || probe.Predialed != nil {
			t.Fatalf("reason %q, err %v, handed on %v", probe.Reason, err, probe.Predialed)
		}
	})
	t.Run("the destination's, record", func(t *testing.T) {
		noCollector(t)
		closed := make(chan struct{}, 1)
		dst, _, _ := acceptingServer(t, func(c net.Conn) { _, _ = io.Copy(io.Discard, c); closed <- struct{}{} })
		up, err := net.Dial("tcp4", dst)
		if err != nil {
			t.Fatal(err)
		}
		p := testProxy(t)
		_, srcConn := clientPair(t)
		probe, err := p.probeMysql(context.Background(), srcConn, dst, util.Adopt(dst, &panicOnRead{up}),
			uint32(mustPort(t, dst)), models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		if !errors.Is(err, errLookIncomplete) || probe.Reason != "probe-failed" {
			t.Fatalf("reason %q, err %v", probe.Reason, err)
		}
		awaitEvent(t, closed, "the destination seeing the upstream closed")
	})
	t.Run("the client's, replay", func(t *testing.T) {
		p := testProxy(t)
		_, srcConn := clientPair(t)
		probe, err := p.probeMysql(context.Background(), &panicOnRead{srcConn}, "10.0.0.7:5672", nil, 5672,
			models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
		if !errors.Is(err, errLookIncomplete) || probe.Reason != "probe-failed" {
			t.Fatalf("reason %q, err %v", probe.Reason, err)
		}
	})
}
