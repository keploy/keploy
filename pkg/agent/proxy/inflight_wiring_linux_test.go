//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// wiringParser is a parser the tests reach through handleConnection, as
// production reaches one: while it serves, it reports what the tracker says
// of the connection, and, when hold is set, holds a request first.
type wiringParser struct {
	p    *Proxy
	hold bool
	saw  chan wiringSaw
}

type wiringSaw struct {
	unknown []string
	held    int32
}

func (wiringParser) MatchType(context.Context, []byte) bool { return true }
func (wiringParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (w wiringParser) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	release := func() {}
	if w.hold {
		release = integrations.HoldRequest(ctx)
	}
	var held int32
	w.p.inflight.mu.Lock()
	for tc := range w.p.inflight.conns {
		held += tc.held.Load()
	}
	w.p.inflight.mu.Unlock()
	w.saw <- wiringSaw{unknown: w.p.inflight.Unknown(true), held: held}
	release()
	return nil
}

// serveThrough hands a kept connection to handleConnection with typ's parser
// as the only one, as the accept loop does, and returns what the parser saw.
func serveThrough(t *testing.T, typ integrations.IntegrationType, hold bool) wiringSaw {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:50051")}, cfg)
	w := wiringParser{p: p, hold: hold, saw: make(chan wiringSaw, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{typ: w}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: typ}}
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
	app, src := acceptedConn(t)
	tc := p.inflight.track(src)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), tc) }()
	if _, err := io.WriteString(app, "hello\n"); err != nil {
		t.Fatal(err)
	}
	saw := awaitEvent(t, w.saw, "the parser being handed the connection")
	awaitEvent(t, done, "handleConnection returning")
	return saw
}

// A connection a parser that reads ahead serves, through handleConnection,
// is unknown while the parser does not mark what it holds: handleConnection
// wraps the connection again before the parser has it, and the mark goes to
// the connection the accept kept (review B1: the mark never reached it). One
// a parser that answers before it reads again serves is not.
func TestHandleConnectionMarksTheKeptConnectionOfAReadAheadParser(t *testing.T) {
	if got := serveThrough(t, integrations.GRPC, false).unknown; strings.Join(got, ",") != "grpc" {
		t.Fatalf("a gRPC parser that does not mark its holds served a kept connection: unknown %v, want [grpc]", got)
	}
	if got := serveThrough(t, integrations.HTTP, false).unknown; len(got) != 0 {
		t.Fatalf("an HTTP parser served it: unknown %v, want none", got)
	}
}

// A parser holds its requests on the connection the accept kept: the holder
// handleConnection hands it reaches it.
func TestHandleConnectionHandsTheParserTheKeptConnectionsHolder(t *testing.T) {
	if got := serveThrough(t, integrations.HTTP, true).held; got != 1 {
		t.Fatalf("a parser's hold through handleConnection: held %d on the kept connection, want 1", got)
	}
}

// keptCounter is a listener counter that checks, as it numbers each
// accepted connection, that the proxy already keeps it (review B3).
type keptCounter struct {
	fakeCounter
	mu      sync.Mutex
	kept    func(peer *net.TCPAddr) bool
	counted int
	early   int
}

func (k *keptCounter) MarkProxyListener(rc syscall.RawConn) error {
	return k.fakeCounter.MarkProxyListener(rc)
}

func (k *keptCounter) ProxyConnPair(rc syscall.RawConn) (agent.ProxyConnPairing, error) {
	var peer *net.TCPAddr
	_ = rc.Control(func(fd uintptr) {
		sa, err := unix.Getpeername(int(fd))
		if err != nil {
			return
		}
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			peer = &net.TCPAddr{IP: net.IP(a.Addr[:]), Port: a.Port}
		case *unix.SockaddrInet6:
			peer = &net.TCPAddr{IP: net.IP(a.Addr[:]), Port: a.Port}
		}
	})
	k.mu.Lock()
	k.counted++
	if k.kept != nil && !k.kept(peer) {
		k.early++
	}
	k.mu.Unlock()
	return k.fakeCounter.ProxyConnPair(rc)
}

// keeps reports whether p keeps a connection from peer.
func keeps(p *Proxy) func(peer *net.TCPAddr) bool {
	return func(peer *net.TCPAddr) bool { return keptFrom(p, peer) != nil }
}

// keptFrom is the connection from peer p keeps; nil, none.
func keptFrom(p *Proxy, peer *net.TCPAddr) *trackedConn {
	if peer == nil {
		return nil
	}
	p.inflight.mu.Lock()
	defer p.inflight.mu.Unlock()
	for tc := range p.inflight.conns {
		if ra, ok := tc.RemoteAddr().(*net.TCPAddr); ok && ra.Port == peer.Port && ra.IP.Equal(peer.IP) {
			return tc
		}
	}
	return nil
}

// A connection is kept before it is counted as accepted: a wait that saw the
// count reach it finds it, and its unread request, among the kept ones. Kept
// after, a test's end could see the count reached and nothing pending while
// the request sat unread.
func TestAConnectionIsKeptBeforeItIsCountedAccepted(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	k := &keptCounter{}
	p, _ := startListening(t, k, func(p *Proxy) { k.kept = keeps(p) })
	addr := p.Listener.Addr().String()
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_, _ = c.Read(make([]byte, 1))
	}
	if err := p.WaitAccepted(context.Background(), 4); err != nil {
		t.Fatalf("WaitAccepted(4): %v", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// The first is the proxy's own check of the count (the canary), which no
	// test's end waits on, and which it never keeps.
	if k.early != 1 || k.counted != 4 {
		t.Fatalf("%d of %d connections were counted accepted before the proxy kept them, want only its own check", k.early, k.counted)
	}
}

// With the proven switch off, a proxy started settling reads (--settle-reads,
// cfg.Agent.SettleReads) keeps every connection its accept hands over, from
// the first, as the switch's does: a read of a test's misses can then tell
// when none holds its calls. A proxy that does not settle keeps none, as
// before.
func TestAProxyThatSettlesReadsKeepsEveryConnectionWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	for _, settles := range []bool{true, false} {
		// For a settling proxy the kernel pairs each connection after the
		// canary with one of the application's: an unpaired one would drop
		// its count (a NAT'd connection, dropCounting). Paired, a proxy that
		// does not settle would keep them too (pairbytes.go), so its are not.
		k := &keptCounter{}
		if settles {
			k.dst = ipv4Dest(t, "127.0.0.1:1")
		}
		p, stop := startListening(t, k, func(p *Proxy) { p.settleReads = settles; k.kept = keeps(p) })
		addr := p.Listener.Addr().String()
		for i := 0; i < 3; i++ {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
		}
		if err := p.WaitAccepted(context.Background(), 4); err != nil {
			t.Fatalf("settles %v: WaitAccepted(4): %v", settles, err)
		}
		k.mu.Lock()
		early, counted := k.early, k.counted
		k.mu.Unlock()
		stop()
		// The proxy's own check of the count (the canary) is never kept.
		want := 1
		if !settles {
			want = 4
		}
		if counted != 4 || early != want {
			t.Fatalf("settles %v: %d of %d connections were not kept when counted accepted, want %d", settles, early, counted, want)
		}
	}
}

// A proxy decides at its start, after its canary and before it says it is
// ready, whether the kernel counts its calls: the canary counted and the bytes
// on its paired connections readable. One whose paired bytes cannot be read
// is not counted, and a settling proxy that is not counted keeps no
// connection, as before (it settles no read).
func TestAProxyIsCountedFromItsStartOnlyWithItsPairedBytes(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	k := &keptCounter{}
	p, stop := startListening(t, k, func(p *Proxy) { p.settleReads = true })
	if !p.CountedFromStart() || !p.keepsConnections() || p.PairedBytesErr() != nil {
		t.Fatalf("a counted proxy: CountedFromStart %v, keepsConnections %v, PairedBytesErr %v", p.CountedFromStart(), p.keepsConnections(), p.PairedBytesErr())
	}
	stop()

	k = &keptCounter{}
	p, _ = startListening(t, k, func(p *Proxy) {
		p.settleReads = true
		p.paired.fail(errors.New("no sock_diag here"))
	})
	if p.PairedBytesErr() == nil {
		t.Fatal("PairedBytesErr does not say the paired bytes cannot be read")
	}
	if p.CountedFromStart() || p.keepsConnections() {
		t.Fatalf("a proxy whose paired bytes cannot be read: CountedFromStart %v, keepsConnections %v", p.CountedFromStart(), p.keepsConnections())
	}
}

// The proxy reads its agent's --settle-reads at its start.
func TestAProxyTakesSettleReadsFromItsAgentsStart(t *testing.T) {
	cfg := config.New()
	cfg.Agent.SettleReads = true
	if p := New(zap.NewNop(), plainDestInfo{}, cfg); !p.settleReads {
		t.Fatal("cfg.Agent.SettleReads set, yet the proxy does not settle reads")
	}
	if p := New(zap.NewNop(), plainDestInfo{}, config.New()); p.settleReads {
		t.Fatal("cfg.Agent.SettleReads unset, yet the proxy settles reads")
	}
}

// The generic parser ends when the application closes its connection once
// answered, with the switch off as on: its handler is done, and the
// connection held nothing. Before, it polled the closed connection for ever,
// never done, and a test's end waited on it for ever while the polling
// counted as progress (review B2).
func TestTheGenericParserEndsWhenTheApplicationCloses(t *testing.T) {
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", on)
			tr := &inflight{}
			app, proxy := acceptedPair(t, tr)
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "1")
			ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
			ctx, cancel := context.WithCancel(integrations.WithRequestHolder(ctx, proxy))
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- generic.New(zap.NewNop()).MockOutgoing(ctx, proxy, nil, genericStore{}, models.OutgoingOptions{})
			}()
			if _, err := io.WriteString(app, "PING"); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 4)
			if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
				t.Fatalf("answer %q, %v", got, err)
			}
			_ = app.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the parser ended with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the generic parser polled a connection the application closed and never ended")
			}
			if h := proxy.held.Load(); h != 0 {
				t.Fatalf("held %d after the connection ended", h)
			}
		})
	}
}

// The progress of a connection that holds work is its own: bytes read, a
// request held or released, its close. A read that returns nothing (a
// deadline waited out, the end of a closed connection) moves none, so a
// polling parser's connection cannot keep a stuck wait looking alive.
func TestAPendingConnectionsProgressIsItsOwn(t *testing.T) {
	tr := &inflight{}
	_, idle := acceptedPair(t, tr)
	app, busy := acceptedPair(t, tr)
	// Neither handler reads: both hold work, in a replay.
	got := tr.PendingProgress(true)
	if len(got) != 2 {
		t.Fatalf("pending %v, want both", got)
	}
	before := got[idle.id]
	if _, err := io.WriteString(app, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := busy.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := idle.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, _ = idle.Read(make([]byte, 1))
	now := tr.PendingProgress(true)
	if now[idle.id] != before {
		t.Fatalf("the idle connection's progress moved %d -> %d on a read that returned nothing", before, now[idle.id])
	}
	if now[busy.id] == got[busy.id] {
		t.Fatal("a read that returned bytes moved no progress for its connection")
	}
}

// An accepted connection tells the learn step its requests under the proven
// switch (learn.Flight), from the instant it is kept; one kept with the
// switch off (an agent that settles its reads) tells none. Each is read as it
// is counted accepted, when the proxy already keeps it: read after, its
// handler may have ended (its destination refuses) and let it go.
func TestAnAcceptedConnectionTellsTheLearnStepItsRequestsUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv("KEPLOY_PROVEN", "1")
		} else {
			t.Setenv("KEPLOY_PROVEN", "")
		}
		k := &keptCounter{dst: ipv4Dest(t, "127.0.0.1:1")}
		// told and kept are counted under k.mu, where k.kept runs.
		var told, kept int
		p, stop := startListening(t, k, func(p *Proxy) {
			p.settleReads = true
			k.kept = func(peer *net.TCPAddr) bool {
				tc := keptFrom(p, peer)
				if tc == nil {
					return false
				}
				kept++
				if tc.flights != nil && tc.flights == p.learnTracker {
					told++
				}
				return true
			}
		})
		c, err := net.Dial("tcp", p.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := p.WaitAccepted(context.Background(), 2); err != nil {
			t.Fatalf("switch %v: WaitAccepted(2): %v", on, err)
		}
		k.mu.Lock()
		gotTold, gotKept := told, kept
		k.mu.Unlock()
		_ = c.Close()
		stop()
		want := 0
		if on {
			want = gotKept
		}
		if gotKept != 1 || gotTold != want {
			t.Fatalf("switch %v: %d of %d kept connections tell the learn step their requests, want %d of 1", on, gotTold, gotKept, want)
		}
	}
}

// unansweringParser serves a connection's request through handleConnection:
// it reads the request through what the proxy hands it (the TLS layer it
// serves over) and has the learn step number a serve of m in it. Without
// answer it returns at once, as a parser that matched a recording it cannot
// deliver does; with it, it writes m's answer and reads on until the
// application closes or resets the connection.
type unansweringParser struct {
	tr     *learn.Tracker
	m      *models.Mock
	answer bool
}

func (unansweringParser) MatchType(context.Context, []byte) bool { return true }
func (unansweringParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (u unansweringParser) MockOutgoing(_ context.Context, src net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	if _, err := src.Read(make([]byte, 64)); err != nil {
		return err
	}
	u.tr.ServedMock(u.m)
	if !u.answer {
		return nil
	}
	if _, err := src.Write([]byte("A")); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, src)
	return nil
}

// The proxy's close of the TLS layer it serves a kept connection over sends
// the layer's close_notify, a write that is no answer: a request its parser
// returned without answering is ended untold before it (learn.Flight.
// EndUntold), so its serve is in an order keploy cannot tell (0,0), never
// one the application read. One the parser answered keeps F's order (1,1),
// whether the application then closes or resets the connection, so that
// the close_notify's write fails. The request is the flight the client's
// Finished lifted: a Go TLS 1.3 server that asks for no client certificate
// writes nothing after it, so it is aloft when the request is read.
func TestTheProxysCloseOfItsTLSLayerEndsARequestItGaveNoAnswerUntold(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	for _, tc := range []struct {
		name   string
		answer bool
		// reset: the application resets the connection once it read the
		// answer (SO_LINGER 0), rather than closing it.
		reset bool
		told  bool
	}{
		{"unanswered", false, false, false},
		{"answered, then closed", true, false, true},
		{"answered, then reset", true, true, true},
	} {
		tr := learn.NewTracker()
		a := &models.Mock{Name: "mock-a"}
		cfg := config.New()
		cfg.Agent.Mode = models.MODE_TEST
		p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:8443")}, cfg)
		p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: unansweringParser{tr: tr, m: a, answer: tc.answer}}
		p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
		_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
		app, src := acceptedConn(t)
		tcn := p.inflight.trackTold(src, tr)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), tcn) }()
		cli := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: tls.VersionTLS13}) //nolint:gosec // the client of a handshake the test only makes
		if err := cli.Handshake(); err != nil {
			t.Fatalf("%s: the client's handshake: %v", tc.name, err)
		}
		if _, err := cli.Write([]byte("GET / HTTP/1.1\r\nHost: dep.example\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if tc.answer {
			if _, err := io.ReadFull(cli, make([]byte, 1)); err != nil {
				t.Fatalf("%s: reading the answer: %v", tc.name, err)
			}
			if tc.reset {
				_ = app.SetLinger(0)
			}
			_ = app.Close()
		} else {
			_, _ = io.Copy(io.Discard, cli) // the proxy's close
		}
		awaitEvent(t, done, "handleConnection returning")
		_ = app.Close()
		fa, la := tr.Placed().Order.Of(a)
		if tc.told && (fa != 1 || la != 1) {
			t.Fatalf("%s: A at %d,%d; want 1,1: the application read its answer", tc.name, fa, la)
		}
		if !tc.told && (fa != 0 || la != 0) {
			t.Fatalf("%s: A at %d,%d; want 0,0: the close_notify landed a request the application was given no answer to", tc.name, fa, la)
		}
	}
}
