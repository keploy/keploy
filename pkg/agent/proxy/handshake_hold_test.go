package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type fakeHandshakeDest struct {
	dest     *agent.NetworkAddress
	err      error
	checkErr error
	checks   *atomic.Int32 // counts CheckHandshakeLookup calls, when set
}

func (f fakeHandshakeDest) CheckHandshakeLookup(context.Context) error {
	if f.checks != nil {
		f.checks.Add(1)
	}
	return f.checkErr
}
func (fakeHandshakeDest) Get(context.Context, uint16) (*agent.NetworkAddress, error) {
	return nil, errors.New("not established")
}
func (fakeHandshakeDest) Delete(context.Context, uint16) error { return nil }

func (f fakeHandshakeDest) GetForHandshake(context.Context, netip.AddrPort, netip.AddrPort) (*agent.NetworkAddress, error) {
	return f.dest, f.err
}

func ipv4Dest(t *testing.T, addr string) *agent.NetworkAddress {
	t.Helper()
	ap := netip.MustParseAddrPort(addr)
	b := ap.Addr().As4()
	// ToIP4AddressStr reads the address as a host-order uint32 (the eBPF
	// map's layout): a.b.c.d is a<<24 | b<<16 | c<<8 | d.
	return &agent.NetworkAddress{Version: 4, IPv4Addr: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), Port: uint32(ap.Port())}
}

func listenCounting(t *testing.T) (string, *atomic.Int32) {
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

var (
	appEnd   = netip.MustParseAddrPort("127.0.0.1:50000")
	proxyEnd = netip.MustParseAddrPort("127.0.0.1:16789")
)

// TestDecideHandshakeDialsAndKeepsTheConnection: while recording, the held
// handshake is released only once the destination accepted, and the
// connection that dial opened is the one the proxy's handler then gets —
// the destination sees one connection.
func TestDecideHandshakeDialsAndKeepsTheConnection(t *testing.T) {
	addr, n := listenCounting(t)
	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	decide := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, addr)})

	d := decide(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Accept || d.Keep == nil {
		t.Fatalf("decision %+v, want accept keeping the connection", d)
	}
	if p.predials.take(appEnd) != nil {
		t.Fatal("the connection was published before the holder kept the decision")
	}
	d.Keep()
	pre := p.predials.take(appEnd)
	if pre == nil {
		t.Fatal("no pre-dialled connection kept for the application's end")
	}
	c, err := util.DialDestination(context.Background(), nil, "tcp", util.DialTarget{Addr: addr, Predialed: pre})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for deadline := time.Now().Add(time.Second); n.Load() < 1 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a second connection, if one were dialled, would be counted by now
	if got := n.Load(); got != 1 {
		t.Fatalf("destination saw %d connections, want the application's one", got)
	}
}

// TestDecideHandshakeDiscardsWhatItDialledForAnAbandonedHandshake: when the
// holder drops a decision because a newer connection from the same end
// replaced it, the upstream it dialled is closed, not published.
func TestDecideHandshakeDiscardsWhatItDialledForAnAbandonedHandshake(t *testing.T) {
	addr, _ := listenCounting(t)
	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, addr)})(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Accept || d.Discard == nil {
		t.Fatalf("decision %+v, want accept with a discard", d)
	}
	d.Discard()
	if p.predials.take(appEnd) != nil {
		t.Fatal("a discarded decision published its connection")
	}
}

// TestDecideHandshakeAnswersAsTheDestinationDid: a destination that refuses
// makes the application's connect fail the same way, and nothing is kept.
func TestDecideHandshakeAnswersAsTheDestinationDid(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := l.Addr().String()
	_ = l.Close() // nothing listens there now

	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	decide := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})
	if d := decide(context.Background(), appEnd, proxyEnd); d.Outcome != synhold.Refuse || d.Keep != nil {
		t.Fatalf("decision %+v, want refuse with nothing kept", d)
	}
	if p.predials.take(appEnd) != nil {
		t.Fatal("kept a connection for a refused destination")
	}
}

// TestDecideHandshakeLeavesOtherConnectionsAsBefore: a connection the hooks
// have no destination for, and one whose upstream will not be dialled
// (served from mocks while testing), complete at once without a dial.
func TestDecideHandshakeLeavesOtherConnectionsAsBefore(t *testing.T) {
	addr, n := listenCounting(t)

	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	if d := p.decideHandshake(fakeHandshakeDest{err: errors.New("no such socket")})(context.Background(), appEnd, proxyEnd); d.Outcome != synhold.Accept || d.Keep != nil {
		t.Fatalf("untracked connection: decision %+v, want accept with nothing kept", d)
	}

	mocked := &agent.Session{Mode: models.MODE_TEST}
	mocked.Mocking = true
	p.setSession(mocked)
	if d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, addr)})(context.Background(), appEnd, proxyEnd); d.Outcome != synhold.Accept || d.Keep != nil {
		t.Fatalf("mocked connection: decision %+v, want accept with nothing kept", d)
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 0 || p.predials.take(appEnd) != nil {
		t.Fatal("dialled the real destination of a connection that is served from mocks")
	}
}

func TestDialsUpstream(t *testing.T) {
	session := func(mode models.Mode, mocking bool) *agent.Session {
		s := &agent.Session{Mode: mode}
		s.Mocking = mocking
		return s
	}
	for _, tc := range []struct {
		name                string
		s                   *agent.Session
		passthrough, oppTLS bool
		want                bool
	}{
		{"record", session(models.MODE_RECORD, false), false, false, true},
		{"test with mocks", session(models.MODE_TEST, true), false, false, false},
		{"test without mocks", session(models.MODE_TEST, false), false, false, true},
		{"test, global passthrough", session(models.MODE_TEST, true), true, false, true},
		{"test, opportunistic TLS", session(models.MODE_TEST, true), false, true, true},
	} {
		p := &Proxy{GlobalPassthrough: tc.passthrough}
		tc.s.OpportunisticTLSIntercept = tc.oppTLS
		if got := p.dialsUpstream(tc.s); got != tc.want {
			t.Errorf("%s: dialsUpstream = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPredialsAreReapedAndReplaced: an upstream whose application gave up
// before completing the handshake is closed, as is one a new handshake from
// the same end replaces.
func TestPredialsAreReapedAndReplaced(t *testing.T) {
	addr, _ := listenCounting(t)
	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	closed := func(c net.Conn) bool {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_, err := c.Write([]byte("x"))
		return err != nil
	}

	var t1 predials
	old, fresh := dial(), dial()
	t1.put(appEnd, util.NewPredialed(addr, old))
	t1.put(appEnd, util.NewPredialed(addr, fresh))
	if !closed(old) {
		t.Fatal("a replaced pre-dial was left open")
	}
	if closed(fresh) {
		t.Fatal("the replacing pre-dial was closed")
	}

	t1.reap(time.Now())
	if t1.take(appEnd) == nil {
		t.Fatal("a fresh pre-dial was reaped")
	}
	superseded := dial()
	t1.put(appEnd, util.NewPredialed(addr, superseded))
	t1.drop(appEnd)
	if !closed(superseded) || t1.take(appEnd) != nil {
		t.Fatal("a pre-dial a new handshake from the same end superseded was kept")
	}
	stale := dial()
	t1.put(appEnd, util.NewPredialed(addr, stale))
	t1.reap(time.Now().Add(predialTTL + time.Second))
	if !closed(stale) || t1.take(appEnd) != nil {
		t.Fatal("a pre-dial past its TTL was not reaped")
	}

	last := dial()
	t1.put(appEnd, util.NewPredialed(addr, last))
	t1.closeAll()
	if !closed(last) {
		t.Fatal("closeAll left a pre-dial open")
	}
}

func TestClientEndIsTheSYNsSource(t *testing.T) {
	mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 50000}
	if got, ok := clientEnd(mapped); !ok || got != appEnd {
		t.Fatalf("clientEnd(%v) = %v, %v; want %v", mapped, got, ok, appEnd)
	}
	if _, ok := clientEnd(&net.UnixAddr{Name: "x"}); ok {
		t.Fatal("clientEnd accepted a non-TCP address")
	}
}

func TestDestAddrMatchesWhatTheHandlerDials(t *testing.T) {
	if got := destAddr(ipv4Dest(t, "10.1.2.3:5432")); got != "10.1.2.3:5432" {
		t.Fatalf("ipv4: %q", got)
	}
	v6 := &agent.NetworkAddress{Version: 6, IPv6Addr: [4]uint32{0x20010db8, 0, 0, 1}, Port: 443}
	if got, want := destAddr(v6), "["+util.ToIPv6AddressStr(v6.IPv6Addr)+"]:443"; got != want {
		t.Fatalf("ipv6: %q, want %q", got, want)
	}
}

type plainDestInfo struct{}

func (plainDestInfo) Get(context.Context, uint16) (*agent.NetworkAddress, error) {
	return nil, errors.New("none")
}
func (plainDestInfo) Delete(context.Context, uint16) error { return nil }

// TestHandshakesAreNotHeldWhereTheyCannotBe: without a way to name a
// connection during its handshake, without sock_diag, or without NFQUEUE,
// the proxy accepts handshakes at once as it always did, and says why once.
func TestHandshakesAreNotHeldWhereTheyCannotBe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dest   agent.DestInfo
		port   uint32
		reason string // "" when nothing is logged at Info
	}{
		{"hooks cannot name a handshake", plainDestInfo{}, 16789, "the connection hooks in use cannot name a connection's destination during its handshake"},
		{"sock_diag does not answer", fakeHandshakeDest{checkErr: errors.New("no tcp_diag")}, 16789, "sock_diag cannot name a connecting socket"},
		{"the queue cannot be set up", fakeHandshakeDest{}, 0, "could not start holding handshakes"},
		// Turned off: nothing is even checked, and nothing is logged above Debug.
		{"turned off", fakeHandshakeDest{checkErr: errors.New("must not be checked")}, 16789, "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			p := &Proxy{logger: zap.New(core), DestInfo: tc.dest, Port: tc.port, DisableHandshakeHold: tc.reason == "off"}
			if tc.reason == "off" {
				tc.reason = ""
			}
			if stop := p.startHandshakeHold(context.Background(), true); stop != nil {
				stop()
				stop() // idempotent
			}
			infos := logs.FilterMessageSnippet("could not hold").All()
			if tc.reason == "" {
				if len(infos) != 0 {
					t.Fatalf("logged %v; a DestInfo that cannot name handshakes is not a failure", infos)
				}
				return
			}
			if len(infos) != 1 || infos[0].ContextMap()["reason"] != tc.reason {
				t.Fatalf("logged %v, want one Info with reason %q", infos, tc.reason)
			}
		})
	}
}

// TestHoldStartsWithTheFirstSessionThatDials: a replay served from mocks
// never dials, so it adds no rules and loads no modules; the first session
// whose connections dial their destination starts the hold, once, and a
// closed listener never restarts it.
func TestHoldStartsWithTheFirstSessionThatDials(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	// The lookup check fails, so each attempt to start is one check, and
	// nothing is installed.
	var checks atomic.Int32
	p := &Proxy{logger: zap.New(core), DestInfo: fakeHandshakeDest{checkErr: errors.New("no tcp_diag"), checks: &checks}, Port: 16789}
	infos := func() int { return len(logs.FilterMessageSnippet("could not hold").All()) }
	attempts := func() int { return int(checks.Load()) }

	mocked := &agent.Session{Mode: models.MODE_TEST}
	mocked.Mocking = true
	p.setSession(mocked)
	p.ensureHandshakeHold(mocked) // before the listener opens: nothing
	p.armHandshakeHold(context.Background())
	p.ensureHandshakeHold(mocked)
	if n := attempts(); n != 0 {
		t.Fatalf("a replay served from mocks tried to hold handshakes (%d)", n)
	}

	rec := &agent.Session{Mode: models.MODE_RECORD}
	p.ensureHandshakeHold(rec)
	p.setSession(rec)
	p.ensureHandshakeHold(rec)
	if n := attempts(); n != 1 {
		t.Fatalf("one recording session tried to start the hold %d times, want once", n)
	}
	// A later session tries again, quietly: the reason was logged once.
	rec2 := &agent.Session{Mode: models.MODE_RECORD}
	p.ensureHandshakeHold(rec2)
	if n := attempts(); n != 2 {
		t.Fatalf("a later session made %d attempts in all, want 2", n)
	}
	if n := infos(); n != 1 {
		t.Fatalf("the reason was logged at Info %d times, want once", n)
	}

	p.stopHandshakeHold()
	p.ensureHandshakeHold(&agent.Session{Mode: models.MODE_RECORD})
	if n := attempts(); n != 2 {
		t.Fatal("the hold was started after the listener closed")
	}
}

func TestDecideHandshakeRefusesALoopbackPortNothingOwnsInMockMode(t *testing.T) {
	recorded.reset()
	t.Cleanup(recorded.reset)
	addr, _ := listenCounting(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := l.Addr().String()
	_ = l.Close()

	mocked := &agent.Session{Mode: models.MODE_TEST}
	mocked.Mocking = true
	p := &Proxy{logger: zap.NewNop(), mockMode: true, appPID: 1}
	p.setSession(mocked)
	decide := func(dest string) synhold.Outcome {
		return p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, dest)})(context.Background(), appEnd, proxyEnd).Outcome
	}
	if got := decide(free); got != synhold.Refuse {
		t.Fatalf("a loopback port nothing listens on: %v, want refuse", got)
	}
	if got := decide(addr); got != synhold.Accept {
		t.Fatalf("a loopback port something listens on: %v, want accept", got)
	}
	if got := decide("10.1.2.3:5432"); got != synhold.Accept {
		t.Fatalf("a dependency that is not local: %v, want accept", got)
	}
	_, port, _ := net.SplitHostPort(free)
	recorded.add([]*models.Mock{{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:" + port}}}})
	if got := decide(free); got != synhold.Accept {
		t.Fatalf("a recorded destination: %v, want accept", got)
	}
	p.mockMode = false
	recorded.reset()
	if got := decide(free); got != synhold.Accept {
		t.Fatalf("outside mock mode: %v, want accept", got)
	}
}

func TestHoldStartsForANativeMockReplay(t *testing.T) {
	var checks atomic.Int32
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeHandshakeDest{checkErr: errors.New("no tcp_diag"), checks: &checks}, Port: 16789, mockMode: true, appPID: 1}
	mocked := &agent.Session{Mode: models.MODE_TEST}
	mocked.Mocking = true
	p.armHandshakeHold(context.Background())
	p.ensureHandshakeHold(mocked)
	if n := checks.Load(); n != 1 {
		t.Fatalf("a native mock replay tried to hold handshakes %d times, want once", n)
	}
	p.IsDocker = true
	p.ensureHandshakeHold(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: mocked.OutgoingOptions})
	if n := checks.Load(); n != 1 {
		t.Fatal("a docker mock replay tried to hold handshakes")
	}
}
