package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Every Redis connection a replay relays to the real Redis is counted as a
// call relayed live, per destination: `keploy mock replay` reads the count
// and claims no isolation for a run whose Redis answered live. Before any
// relay the count says "none", an empty list, never nil.
func TestARelayedRedisConnectionIsCountedLive(t *testing.T) {
	up := newFakeRedis(t, nil)
	p := replayProxy(t, zap.NewNop(), up.addr, oldRedisMock())
	if got := p.RelayedLive(); got == nil || len(got) != 0 {
		t.Fatalf("before any relay the count is %#v, want an empty list", got)
	}
	for i := 0; i < 3; i++ {
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		if got := roundTrip(t, app, bufio.NewReader(app), goRedisHello); got != "+REAL\r\n" {
			t.Fatalf("answered %q, want the real Redis's answer", got)
		}
		_ = app.Close()
		awaitEvent(t, done, "handleConnection returning")
	}
	want := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: up.addr, Connections: 3}}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}
}

// A connection a replay answers from its recording is no relay: a Generic
// call its mock answers leaves the count empty.
func TestAMockedConnectionIsNotCountedLive(t *testing.T) {
	up := newFakeRedis(t, nil)
	const req, resp = "get k\r\n", "VALUE k 0 1\r\nv\r\nEND\r\n"
	p := replayProxy(t, zap.NewNop(), up.addr, &models.Mock{
		Name: "mock-0",
		Kind: models.GENERIC,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: req}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: models.String, Data: resp}}}},
		},
	})
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	_, err := app.Write([]byte(req))
	require.NoError(t, err)
	got := make([]byte, len(resp))
	_, err = io.ReadFull(app, got)
	require.NoError(t, err, "the mock answers")
	require.Equal(t, resp, string(got))
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
	if up.received() != "" {
		t.Fatalf("the live server was reached: %q", up.received())
	}
	if rows := p.RelayedLive(); len(rows) != 0 {
		t.Fatalf("a mocked call was counted relayed: %+v", rows)
	}
}

// A replay's connection in a session with mocking off goes to the live
// service, and is counted so.
func TestAMockingOffReplayConnectionIsCountedLive(t *testing.T) {
	up := newFakeRedis(t, nil)
	p := replayProxy(t, zap.NewNop(), up.addr)
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: false, DisableMysqlAutoDetect: true}})
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	if got := roundTrip(t, app, bufio.NewReader(app), "get k\r\n"); got != "END\r\n" {
		t.Fatalf("answered %q, want the live server's answer", got)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
	want := models.RelayedLives{{Reason: models.RelayedPassThrough, Destination: up.addr, Connections: 1}}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}
}

// A replay's connection the session intercepts opportunistically for TLS is
// relayed to the live service (keploy only takes its keys), not answered
// from a recording, and is counted so.
func TestAnOpportunisticTLSReplayConnectionIsCountedLive(t *testing.T) {
	up := newFakeRedis(t, nil)
	p := replayProxy(t, zap.NewNop(), up.addr)
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true, OpportunisticTLSIntercept: true, DisableMysqlAutoDetect: true}})
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	if got := roundTrip(t, app, bufio.NewReader(app), "get k\r\n"); got != "END\r\n" {
		t.Fatalf("answered %q, want the live server's answer", got)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
	want := models.RelayedLives{{Reason: models.RelayedPassThrough, Destination: up.addr, Connections: 1}}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}
}

// A connection to a mock.real destination is relayed to it live (KEPLOY_PROVEN),
// and counted so.
func TestAMockRealConnectionIsCountedLive(t *testing.T) {
	dst := realServer(t)
	p := realProxy(t, []string{dst}, nil)
	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dst)
	_, err := app.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	_, err = io.ReadAll(app)
	require.NoError(t, err)
	require.True(t, (<-done).handled)
	want := models.RelayedLives{{Reason: models.RelayedReal, Destination: dst, Connections: 1}}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}
}

// A Redis connection is counted by where its dial went: to an address
// keploy's DNS answered a name with from a recorded mock, which the name no
// longer has, the relay dials the name's address now (liveAddr), and the row
// names the name, at its port; dialled where the application dialled it (the
// name cannot be resolved now: this proxy has no resolver), the row names
// that address.
func TestARelayedRedisConnectionIsNamedByWhereItsDialWent(t *testing.T) {
	up := newFakeRedis(t, nil)
	upAP := netip.MustParseAddrPort(up.addr)
	recorded := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), upAP.Port()).String()
	seed := func(p *Proxy) {
		msg := new(dns.Msg)
		msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "redis.test.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.ParseIP("10.0.0.2").To4()}}
		p.dnsAnswered.answered("redis.test.", dnsCacheEntry{Msg: msg, Mocked: true})
	}
	relay := func(t *testing.T, p *Proxy) models.RelayedLives {
		t.Helper()
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		_ = roundTrip(t, app, bufio.NewReader(app), goRedisHello)
		_ = app.Close()
		awaitEvent(t, done, "handleConnection returning")
		return p.RelayedLive()
	}

	// The name is at the fake Redis now.
	upstream, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Name == "redis.test." && r.Question[0].Qtype == dns.TypeA {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "redis.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: upAP.Addr().AsSlice()}}
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	host, port, err := net.SplitHostPort(upstream)
	require.NoError(t, err)
	p := replayProxy(t, zap.NewNop(), recorded, oldRedisMock())
	p.dnsUpstreamServers, p.dnsUpstreamPort, p.dnsForwardTimeout = []string{host}, port, 2*time.Second
	seed(p)
	want := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: net.JoinHostPort("redis.test", strconv.Itoa(int(upAP.Port()))), Connections: 1}}
	if got := relay(t, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed to where the name is now: %+v, want %+v", got, want)
	}

	// No resolver: dialled at the recorded address, named by it.
	p = replayProxy(t, zap.NewNop(), up.addr, oldRedisMock())
	msg := new(dns.Msg)
	msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "redis.test.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: upAP.Addr().AsSlice()}}
	p.dnsAnswered.answered("redis.test.", dnsCacheEntry{Msg: msg, Mocked: true})
	want = models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: up.addr, Connections: 1}}
	if got := relay(t, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed where the application dialled: %+v, want %+v", got, want)
	}
}

// redisTarget names a replay's Redis dial by where it goes: the name when
// liveTarget dials the name's address now, the TLS server's name it is
// dialled by, nothing (the count keeps the application's address) when the
// dial goes there.
func TestRedisTargetNamesWhereTheDialGoes(t *testing.T) {
	p := liveProxy(t,
		map[string][]string{"moved.test.": {"10.0.0.2"}, "kept.test.": {"10.0.0.5"}, "gone.test.": {"10.0.0.9"}},
		map[string][]string{"moved.test.": {"10.0.0.3"}, "kept.test.": {"10.0.0.5"}})
	for _, name := range []string{"moved.test.", "kept.test.", "gone.test."} {
		if got := lookup(t, p, name, false); len(got) != 1 {
			t.Fatalf("the replay's DNS answered %s with %v, want its one recorded address", name, got)
		}
	}
	for _, c := range []struct{ dst, addr, live, named string }{
		{"10.0.0.2:6379", "10.0.0.2:6379", "10.0.0.3:6379", "moved.test:6379"},
		{"10.0.0.5:6379", "10.0.0.5:6379", "10.0.0.5:6379", ""},
		{"10.0.0.9:6379", "10.0.0.9:6379", "10.0.0.9:6379", ""},
		{"192.0.2.1:6379", "192.0.2.1:6379", "192.0.2.1:6379", ""},
		{"10.0.0.5:6380", "redis.example:6380", "redis.example:6380", "redis.example:6380"},
	} {
		live, named := p.redisTarget(c.dst, c.addr)
		if live != c.live || named != c.named {
			t.Errorf("redisTarget(%q, %q) = %q, %q; want %q, %q", c.dst, c.addr, live, named, c.live, c.named)
		}
	}
}

// A Redis connection is counted as soon as it is matched, before its dial
// returns: a replay whose test command exits while its app's Redis dial is
// still pending (a Redis that drops the connection attempt) is never
// claimed isolated. In record mode, where it was dialled before dispatch, it
// is counted once too.
func TestARedisConnectionIsCountedBeforeItsDialReturns(t *testing.T) {
	const dst = "10.0.0.7:6379"
	p := freshSaid(New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, dst)}, config.New()))
	entered, release := make(chan struct{}), make(chan struct{})
	var none net.Conn
	done := make(chan error, 1)
	want := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: dst, Connections: 1}}
	// What target() does first (liveTarget's DNS forward, up to
	// dnsForwardTimeout) is no window either.
	atTarget := make(chan models.RelayedLives, 1)
	go func() {
		done <- p.serveRedisSkip(context.Background(), p.logger, dst, nil, &none, func() (string, string) {
			atTarget <- p.RelayedLive()
			return dst, ""
		}, func(string) (net.Conn, error) {
			close(entered)
			<-release
			return nil, errors.New("connection timed out")
		})
	}()
	if got := awaitEvent(t, atTarget, "the dial target being decided"); !reflect.DeepEqual(got, want) {
		t.Fatalf("while the dial target is decided the count is %+v, want %+v", got, want)
	}
	awaitEvent(t, entered, "the dial starting")
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("while the dial is pending the count is %+v, want %+v", got, want)
	}
	close(release)
	awaitEvent(t, done, "serveRedisSkip returning")
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the dial failed the count is %+v, want %+v", got, want)
	}

	// Record mode: the destination was dialled before dispatch.
	p = freshSaid(New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, dst)}, config.New()))
	app, peer := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = peer.Close() })
	up, upPeer := net.Pipe()
	t.Cleanup(func() { _ = up.Close(); _ = upPeer.Close() })
	_ = app.Close()
	_ = upPeer.Close()
	pre := up
	if err := p.serveRedisSkip(context.Background(), p.logger, dst, peer, &pre, func() (string, string) {
		t.Fatal("a record's connection dialled before dispatch was given a target")
		return "", ""
	}, func(string) (net.Conn, error) {
		t.Fatal("a record's connection dialled before dispatch was dialled again")
		return nil, nil
	}); err != nil {
		t.Fatalf("serveRedisSkip: %v", err)
	}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a record's connection was counted %+v, want %+v", got, want)
	}
}

// A replay's DNS lookup that no recorded answer matches, which keploy's DNS
// forwards to the live resolver and answers the application with, reached a
// live dependency: it is counted, per resolver and query, each lookup the
// answer is handed out for, from the cache too, and so is one the resolver
// answers with no record of the type (NODATA). A lookup a recorded answer
// matches is not, nor one keploy answers itself (the synthesized steer of a
// name the resolver says does not exist), nor a record's lookups. With
// mocking off, the resolver's answer is handed out whatever it is, and
// counted. A name mock.real lists is resolved live by mock.real.
func TestADNSLookupTheLiveResolverAnsweredIsCountedLive(t *testing.T) {
	p := liveProxy(t,
		map[string][]string{"kept.test.": {"10.0.0.5"}},
		map[string][]string{"kept.test.": {"10.0.0.5"}, "new.test.": {"10.0.0.8"}, "listed.test.": {"10.0.0.9"}, "empty.test.": {}})
	resolver := net.JoinHostPort(p.dnsUpstreamServers[0], p.dnsUpstreamPort)

	if got := lookup(t, p, "kept.test.", false); !reflect.DeepEqual(got, []string{"10.0.0.5"}) {
		t.Fatalf("kept.test answered %v, want its recorded answer", got)
	}
	if got := lookup(t, p, "nx.test.", false); !reflect.DeepEqual(got, []string{"127.0.0.1"}) {
		t.Fatalf("nx.test answered %v, want keploy's own steer", got)
	}
	if rows := p.RelayedLive(); len(rows) != 0 {
		t.Fatalf("lookups answered from the recording or by keploy were counted relayed: %+v", rows)
	}

	for i := 0; i < 2; i++ {
		if got := lookup(t, p, "new.test.", false); !reflect.DeepEqual(got, []string{"10.0.0.8"}) {
			t.Fatalf("new.test answered %v, want the live resolver's answer", got)
		}
	}
	want := models.RelayedLives{{Reason: models.RelayedDNS, Destination: resolver, Query: "new.test A", Connections: 2}}
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}

	// NODATA: the name exists and has no record of the type.
	if got := lookup(t, p, "empty.test.", false); len(got) != 0 {
		t.Fatalf("empty.test answered %v, want the live resolver's empty answer", got)
	}
	want = append(models.RelayedLives{{Reason: models.RelayedDNS, Destination: resolver, Query: "empty.test A", Connections: 1}}, want...).Sorted()
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}

	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: false}})
	if got := lookup(t, p, "gone.test.", false); len(got) != 0 {
		t.Fatalf("gone.test answered %v with mocking off, want the live resolver's NXDOMAIN", got)
	}
	want = append(models.RelayedLives{{Reason: models.RelayedDNS, Destination: resolver, Query: "gone.test A", Connections: 1}}, want...).Sorted()
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}

	p.setSession(&agent.Session{Mode: models.MODE_RECORD, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
	lookup(t, p, "recorded.test.", false)
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a record's lookup was counted relayed: %+v, want %+v", got, want)
	}

	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse([]string{"listed.test:443"})
	require.NoError(t, err)
	p.real.Store(&realState{list: list, book: &realdeps.Book{}})
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
	if got := lookup(t, p, "listed.test.", false); !reflect.DeepEqual(got, []string{"10.0.0.9"}) {
		t.Fatalf("listed.test answered %v, want the live resolver's answer", got)
	}
	want = append(want, models.RelayedLive{Reason: models.RelayedReal, Destination: resolver, Query: "listed.test A", Connections: 1}).Sorted()
	if got := p.RelayedLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed %+v, want %+v", got, want)
	}
}

// An agent counts a name of its own on at most maxRelayedNames DNS rows: a
// lookup of a name no row holds yet, past them, is counted on its reason and
// resolver's row of other names, so an agent asked for ever-new names keeps a
// bounded count. A name a row already holds is still counted on its own row.
func TestTheDNSRowsAnAgentKeepsAreBounded(t *testing.T) {
	var r relayedLive
	const resolver = "127.0.0.11:53"
	for i := 0; i < maxRelayedNames+5; i++ {
		r.noteLookup(models.RelayedDNS, resolver, fmt.Sprintf("n%d.test A", i))
	}
	r.noteLookup(models.RelayedDNS, resolver, "n0.test A")
	rows := r.rows()
	if len(rows) != maxRelayedNames+1 {
		t.Fatalf("the agent keeps %d rows, want %d: %d named and one of other names", len(rows), maxRelayedNames+1, maxRelayedNames)
	}
	byQuery := map[string]int{}
	for _, row := range rows {
		byQuery[row.Query] = row.Connections
	}
	if got := byQuery[models.RelayedOtherNames]; got != 5 {
		t.Fatalf("the row of other names counts %d lookups, want the 5 past the bound", got)
	}
	if got := byQuery["n0.test A"]; got != 2 {
		t.Fatalf("a name a row already holds was counted %d times on it, want 2", got)
	}
	if _, kept := byQuery[fmt.Sprintf("n%d.test A", maxRelayedNames)]; kept {
		t.Fatal("a name past the bound has a row of its own")
	}
}
