package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// realConnPair is the application's end and keploy's accepted end of one
// loopback connection.
func realConnPair(t *testing.T) (app, accepted net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		got <- c
	}()
	app, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	accepted = <-got
	t.Cleanup(func() { _ = app.Close(); _ = accepted.Close() })
	return app, accepted
}

// realServer is a real destination that answers each connection with what it
// read, upper-cased marker first, once the client half-closes.
func realServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("real:"), b...))
			}()
		}
	}()
	return ln.Addr().String()
}

func realProxy(t *testing.T, entries []string, resolved map[string]string) *Proxy {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse(entries)
	require.NoError(t, err)
	book := &realdeps.Book{}
	for name, a := range resolved {
		book.Resolved(name, []netip.Addr{netip.MustParseAddr(a)})
	}
	p := &Proxy{logger: zap.NewNop()}
	p.real.Store(&realState{list: list, book: book})
	return p
}

type served struct {
	handled bool
	conn    net.Conn
	err     error
}

func serveRealAsync(p *Proxy, accepted net.Conn, dst string) chan served {
	out := make(chan served, 1)
	go func() {
		h, c, err := p.serveReal(context.Background(), accepted, dst, "", 1, nil)
		out <- served{h, c, err}
	}()
	return out
}

// An address entry relays the connection to the real destination at once:
// nothing is read, recorded or mocked, and the destination is reached.
func TestAnAddressEntryRelaysToTheRealDestination(t *testing.T) {
	dst := realServer(t)
	p := realProxy(t, []string{dst}, nil)
	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dst)
	_, err := app.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	got, err := io.ReadAll(app)
	require.NoError(t, err)
	require.Equal(t, "real:hello", string(got))
	s := <-done
	require.True(t, s.handled)
	require.NoError(t, s.err)
	run := p.RealRun()
	require.Equal(t, []string{dst}, run.Reached)
	require.Equal(t, []string{dst}, run.Outside, "no test was open")
}

// A name entry whose address keploy's DNS did not name reads the name from the
// client's first bytes: its own name is relayed, another name is a
// dependency, and the bytes read stay for whoever handles it next.
func TestANameEntryIsDecidedByTheNameTheClientSends(t *testing.T) {
	dst := realServer(t)
	_, portText, _ := net.SplitHostPort(dst)
	p := realProxy(t, []string{"api.test:" + portText}, map[string]string{"api.test": "127.0.0.1", "web.test": "127.0.0.1"})

	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dst)
	req := "GET / HTTP/1.1\r\nHost: api.test\r\n\r\n"
	_, err := app.Write([]byte(req))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	got, err := io.ReadAll(app)
	require.NoError(t, err)
	require.Equal(t, "real:"+req, string(got))
	require.True(t, (<-done).handled)

	app, accepted = realConnPair(t)
	done = serveRealAsync(p, accepted, dst)
	other := "GET / HTTP/1.1\r\nHost: web.test\r\n\r\n"
	_, err = app.Write([]byte(other))
	require.NoError(t, err)
	s := <-done
	require.False(t, s.handled, "another name is a dependency")
	buf := make([]byte, len(other))
	_, err = io.ReadFull(bufio.NewReader(s.conn), buf)
	require.NoError(t, err)
	require.Equal(t, other, string(buf), "the bytes read to name it are still there")
	require.Empty(t, p.RealRun().Incomplete)
}

// A connection whose name keploy cannot read is closed as
// capture_incomplete: never guessed, passed through or mocked.
func TestAConnectionKeployCannotNameIsCaptureIncomplete(t *testing.T) {
	p := realProxy(t, []string{"db.test:3306"}, map[string]string{"db.test": "127.0.0.1"})
	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, "127.0.0.1:3306")
	_, err := app.Write([]byte{0x00, 0x01, 0x02})
	require.NoError(t, err)
	s := <-done
	require.True(t, s.handled)
	run := p.RealRun()
	require.Len(t, run.Incomplete, 1)
	require.Equal(t, models.CaptureIncompleteRealName, run.Incomplete[0].Class)
	require.Contains(t, run.Incomplete[0].Why, "db.test")
	require.Empty(t, run.Reached)

	// And so is one whose client closes it before naming it.
	app, accepted = realConnPair(t)
	done = serveRealAsync(p, accepted, "127.0.0.1:3306")
	require.NoError(t, app.Close())
	require.True(t, (<-done).handled)
	require.Len(t, p.RealRun().Incomplete, 1)
}

// A REAL destination that refuses fails the connection as it would without
// keploy, and the open test's row counts the failure.
func TestARefusedRealDestinationFailsTheConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dst := ln.Addr().String()
	require.NoError(t, ln.Close())
	p := realProxy(t, []string{dst}, nil)
	p.realTracker.BeginTest()
	app, accepted := realConnPair(t)
	s := <-serveRealAsync(p, accepted, dst)
	require.True(t, s.handled)
	require.NoError(t, s.err)
	_ = accepted.Close()
	_, err = app.Read(make([]byte, 1))
	require.Error(t, err)
	uses, _ := p.realTracker.EndTest()
	require.Equal(t, []models.RealUse{{Dest: dst, Failed: 1}}, uses)
	require.Empty(t, p.RealRun().Reached)
}

// Without the switch, or without a list, nothing is decided here.
func TestServeRealDecidesNothingWithoutTheSwitchOrAList(t *testing.T) {
	dst := realServer(t)
	p := realProxy(t, []string{dst}, nil)
	t.Setenv(provenswitch.Env, "0")
	_, accepted := realConnPair(t)
	h, c, err := p.serveReal(context.Background(), accepted, dst, "", 1, nil)
	require.False(t, h)
	require.NoError(t, err)
	require.Same(t, accepted, c)

	t.Setenv(provenswitch.Env, "1")
	empty := &Proxy{logger: zap.NewNop()}
	h, c, _ = empty.serveReal(context.Background(), accepted, dst, "", 1, nil)
	require.False(t, h)
	require.Same(t, accepted, c)
}

// SetRealDeps refuses a list it cannot read and needs the switch; an empty
// list installs none.
func TestSetRealDeps(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	t.Setenv(provenswitch.Env, "0")
	require.Error(t, p.SetRealDeps(context.Background(), []string{"localhost:1"}))
	t.Setenv(provenswitch.Env, "1")
	require.ErrorIs(t, p.SetRealDeps(context.Background(), []string{"localhost"}), realdeps.ErrBadEntry)
	require.NoError(t, p.SetRealDeps(context.Background(), []string{"localhost:5432", "container:redis"}))
	require.NotNil(t, p.realList())
	require.NoError(t, p.SetRealDeps(context.Background(), nil))
	require.Nil(t, p.realList())
}

// A mock.real name's DNS questions are answered live by the upstream
// resolver every time (never from the cache, never recorded), and the answer
// names its address for the run.
func TestARealNamesDNSIsResolvedLiveAndBooked(t *testing.T) {
	var asked atomic.Int32
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if strings.EqualFold(r.Question[0].Name, "auth.test.") {
			asked.Add(1)
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: "auth.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("10.0.0.7")})
		}
		_ = w.WriteMsg(m)
	})
	defer stop()
	p := newProxyWithUpstream(t, addr, 2*time.Second)
	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse([]string{"auth.test:443"})
	require.NoError(t, err)
	book := &realdeps.Book{}
	p.real.Store(&realState{list: list, book: book})
	recorded := make(chan *models.Mock, 4)
	p.setSession(&agent.Session{Mode: models.MODE_RECORD, MC: recorded})
	before := asked.Load() // the helper's readiness probe asks probe.test only

	srv := serveDNSOn(t, p, "udp")
	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
	for i := 0; i < 2; i++ {
		q := new(dns.Msg)
		q.SetQuestion("Auth.Test.", dns.TypeA)
		resp, _, err := c.Exchange(q, srv)
		require.NoError(t, err)
		require.Len(t, resp.Answer, 1)
		require.Equal(t, "10.0.0.7", resp.Answer[0].(*dns.A).A.String())
	}
	require.Equal(t, int32(2), asked.Load()-before, "each question went to the upstream")
	require.Empty(t, recorded, "a real name's lookup is never recorded")
	d := realdeps.Decider{List: list, Book: book}
	got := d.AtAccept(netip.MustParseAddrPort("10.0.0.7:443"), "")
	require.Equal(t, realdeps.Real, got.Verdict, "keploy's DNS named the address")
}

// An upstream that cannot be asked answers a real name SERVFAIL, never
// keploy's own address.
func TestARealNameKeployCannotResolveIsServfail(t *testing.T) {
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, _ := net.SplitHostPort(ln.LocalAddr().String())
	require.NoError(t, ln.Close())
	p := newProxyWithUpstream(t, "127.0.0.1:"+port, 200*time.Millisecond)
	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse([]string{"auth.test:443"})
	require.NoError(t, err)
	p.real.Store(&realState{list: list, book: &realdeps.Book{}})
	p.setSession(&agent.Session{Mode: models.MODE_TEST, Mocking: true})
	resp := p.realDNS(dns.Question{Name: "auth.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	require.Equal(t, dns.RcodeServerFailure, resp.Rcode)
	require.Empty(t, resp.Answer)
}

func TestBookDNSAnswerNamesV4AndV6Addresses(t *testing.T) {
	book := &realdeps.Book{}
	list, err := realdeps.Parse([]string{"auth.test:443"})
	require.NoError(t, err)
	st := &realState{list: list, book: book}
	st.bookDNSAnswer(dns.Question{Name: "auth.test."}, []dns.RR{
		&dns.CNAME{Hdr: dns.RR_Header{Name: "auth.test.", Rrtype: dns.TypeCNAME}, Target: "lb.cloud."},
		&dns.A{Hdr: dns.RR_Header{Name: "lb.cloud.", Rrtype: dns.TypeA}, A: net.ParseIP("10.0.0.8")},
		&dns.AAAA{Hdr: dns.RR_Header{Name: "lb.cloud.", Rrtype: dns.TypeAAAA}, AAAA: net.ParseIP("fd00::8")},
	})
	d := realdeps.Decider{List: list, Book: book}
	for _, a := range []string{"10.0.0.8", "fd00::8"} {
		got := d.AtAccept(netip.AddrPortFrom(netip.MustParseAddr(a), 443), "")
		require.Equal(t, realdeps.Real, got.Verdict, a)
		require.Equal(t, "auth.test:"+strconv.Itoa(443), got.Label, "the question's name, never the CNAME target")
	}
}

// A name keploy's DNS gave the address decides at accept: the connection is
// relayed, and a client of a line protocol that waits for the server after
// its first line is relayed then, never held for bytes it will not send.
func TestADNSNamedRealConnectionOfALineProtocolIsRelayed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("220 mail ready\r\n"))
		line, _ := bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte("250 got " + line))
	}()
	dst := ln.Addr().String()
	_, portText, _ := net.SplitHostPort(dst)
	p := realProxy(t, []string{"mail.example.com:" + portText}, nil)
	p.real.Load().book.Answered("mail.example.com.", []netip.Addr{netip.MustParseAddr("127.0.0.1")})

	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dst)
	r := bufio.NewReader(app)
	greeting, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "220 mail ready\r\n", greeting)
	_, err = app.Write([]byte("EHLO me\r\n"))
	require.NoError(t, err)
	reply, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "250 got EHLO me\r\n", reply)
	require.NoError(t, app.Close())
	require.True(t, (<-done).handled)
	require.Empty(t, p.RealRun().Incomplete)
}

// A question the app's resolver expanded with a search domain asks for the
// listed name: it is answered live, and booked under the listed name, so the
// address has the one name the app asked for.
func TestASearchExpansionOfARealNameIsThatName(t *testing.T) {
	var asked atomic.Int32
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if strings.EqualFold(r.Question[0].Name, "postgres.default.svc.cluster.local.") {
			asked.Add(1)
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("10.0.0.9")})
		}
		_ = w.WriteMsg(m)
	})
	defer stop()
	p := newProxyWithUpstream(t, addr, 2*time.Second)
	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse([]string{"postgres:5432"})
	require.NoError(t, err)
	book := &realdeps.Book{}
	p.real.Store(&realState{list: list, book: book, search: []string{"default.svc.cluster.local", "svc.cluster.local"}})
	p.setSession(&agent.Session{Mode: models.MODE_RECORD, MC: make(chan *models.Mock, 1)})
	srv := serveDNSOn(t, p, "udp")
	q := new(dns.Msg)
	q.SetQuestion("postgres.default.svc.cluster.local.", dns.TypeA)
	resp, _, err := (&dns.Client{Net: "udp", Timeout: 5 * time.Second}).Exchange(q, srv)
	require.NoError(t, err)
	require.Len(t, resp.Answer, 1)
	require.Equal(t, int32(1), asked.Load(), "answered live")
	got := realdeps.Decider{List: list, Book: book}.AtAccept(netip.MustParseAddrPort("10.0.0.9:5432"), "")
	require.Equal(t, realdeps.Real, got.Verdict)
	require.Equal(t, "postgres:5432", got.Label)
	st := p.realList()
	_, ok := st.listedName("postgres.other.example.com.")
	require.False(t, ok, "only the resolver's own search domains")
}

// In a replay, a connect to a mock.real address is held and dialled for real,
// so it answers as the real destination does: refused when it refuses, which
// the open test's REAL row counts, and kept for the relay when it accepts. A
// connect to anything else is accepted at once, as before.
func TestAReplaysRealConnectAnswersAsTheRealDestination(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := ln.Addr().String()
	require.NoError(t, ln.Close())
	up, _ := listenCounting(t)

	p := realProxy(t, []string{refused, up}, nil)
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
	p.realTracker.BeginTest()

	d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})(context.Background(), appEnd, proxyEnd)
	require.Equal(t, synhold.Refuse, d.Outcome)
	uses, _ := p.realTracker.EndTest()
	require.Equal(t, []models.RealUse{{Dest: refused, Failed: 1}}, uses)

	d = p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, up)})(context.Background(), appEnd, proxyEnd)
	require.Equal(t, synhold.Accept, d.Outcome)
	require.NotNil(t, d.Keep, "the real destination's connection is kept for the relay")
	d.Discard()

	other, _ := listenCounting(t)
	d = p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, other)})(context.Background(), appEnd, proxyEnd)
	require.Equal(t, synhold.Accept, d.Outcome)
	require.Nil(t, d.Keep, "a dependency's connect in a replay dials nothing")

	// Without the switch the list is not read: nothing is dialled.
	t.Setenv(provenswitch.Env, "0")
	d = p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})(context.Background(), appEnd, proxyEnd)
	require.Equal(t, synhold.Accept, d.Outcome)
	require.Nil(t, d.Keep)
}

// An agent started with global passthrough would make every call live, so
// it refuses a run's mock.real, an empty one too: the run fails instead of
// passing calls through that mock.real does not list (U18).
func TestAnAgentWithGlobalPassthroughRefusesMockReal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), GlobalPassthrough: true}
	require.ErrorContains(t, p.SetRealDeps(context.Background(), nil), "global passthrough")
	require.ErrorContains(t, p.SetRealDeps(context.Background(), []string{"localhost:5432"}), "global passthrough")
	require.Nil(t, p.realList())
}

// unreadableDocker is a Docker index that cannot be read.
type unreadableDocker struct{}

func (unreadableDocker) Lookup(netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{}, false, errors.New("dial unix /var/run/docker.sock: connect: permission denied")
}
func (unreadableDocker) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (unreadableDocker) Pending(netip.AddrPort) ([]string, bool)            { return nil, false }
func (unreadableDocker) AwaitReadsOf(context.Context, []string)             {}
func (unreadableDocker) Bind(context.Context)                               {}
func (unreadableDocker) Watch(context.Context, *zap.Logger)                 {}
func (unreadableDocker) Endpoint() string                                   { return "unix:///fake.sock" }

// With Docker unreadable, a run whose mock.real lists container: entries says
// once that they match nothing; the handshake's lookups name nothing, so the
// session's own warning that Docker cannot be read is still the accept's to
// give.
func TestContainerEntriesDockerCannotReadAreSaidOnce(t *testing.T) {
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(unreadableDocker{})
	t.Cleanup(func() { containerkey.Default = saved })
	core, logs := observer.New(zap.DebugLevel)
	p := realProxy(t, []string{"container:redis"}, nil)
	p.logger = zap.New(core)
	for range 3 {
		_, ok := p.realAtHandshake(ipv4Dest(t, "127.0.0.1:6379"))
		require.False(t, ok)
	}
	require.Equal(t, 1, logs.FilterMessageSnippet("container: entries").Len())
	_, _, ok := containerkey.Default.Name(p.logger, "1", "127.0.0.1:6379")
	require.False(t, ok)
	require.Equal(t, 1, logs.FilterMessageSnippet("names container destinations by their address").Len(),
		"a handshake's lookup spent the session's warning")
}

// In a replay, a connection the handshake dialled for REAL that the accept
// finds is a dependency (keploy's DNS named its address again in between) is
// served from mocks: the dial is closed, never left for the real server to
// close the app's end. A record dials its dependencies, and keeps it.
func TestARealPredialOfADependencyIsClosedInAReplay(t *testing.T) {
	up, _ := listenCounting(t)
	listed := realServer(t)
	for _, tc := range []struct {
		mode   models.Mode
		closed bool
	}{{models.MODE_TEST, true}, {models.MODE_RECORD, false}} {
		p := realProxy(t, []string{listed}, nil)
		p.setSession(&agent.Session{Mode: tc.mode, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
		c, err := util.DialUpstream(context.Background(), nil, "tcp", up)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		_, accepted := realConnPair(t)
		h, _, err := p.serveReal(context.Background(), accepted, up, "", 1, util.NewPredialed(up, c))
		require.False(t, h)
		require.NoError(t, err)
		_, werr := c.Write([]byte("x"))
		require.Equal(t, tc.closed, werr != nil, "mode %s", tc.mode)
	}
}

// Accepted before its dial (no handshake hold on the host), a connection to a
// localhost entry whose server listens on one loopback family reaches it
// through the other, as the app's own fallback would have: the entry lists
// both. An address entry has no such fallback (TestARealDialReachesOnlyTheListedAddress).
func TestALocalhostEntryReachesItsOtherLoopbackFamily(t *testing.T) {
	if l6, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		t.Skipf("this host has no IPv6 loopback: %v", err)
	} else {
		_ = l6.Close()
	}
	dst := realServer(t)
	_, port, _ := net.SplitHostPort(dst)
	v6 := net.JoinHostPort("::1", port)
	p := realProxy(t, []string{"localhost:" + port}, nil)
	p.realTracker.BeginTest()
	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, v6)
	_, err := app.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	got, err := io.ReadAll(app)
	require.NoError(t, err)
	require.Equal(t, "real:hello", string(got))
	s := <-done
	require.True(t, s.handled)
	require.NoError(t, s.err)
	uses, _ := p.realTracker.EndTest()
	require.Len(t, uses, 1)
	require.Equal(t, "localhost:"+port, uses[0].Dest)
	require.Equal(t, 1, uses[0].Conns)
	require.Zero(t, uses[0].Failed)
}

// A REAL destination is dialled exactly: a listed [::1]:P that refuses is not
// reached through 127.0.0.1:P, which mock.real does not list, and the dial
// counts as failed, never as reached.
func TestARealDialReachesOnlyTheListedAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("v4-only server"))
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	v6 := net.JoinHostPort("::1", port)
	p := realProxy(t, []string{v6}, nil)
	p.realTracker.BeginTest()
	app, accepted := realConnPair(t)
	s := <-serveRealAsync(p, accepted, v6)
	require.True(t, s.handled)
	_ = accepted.Close()
	got, _ := io.ReadAll(app)
	require.Empty(t, got, "the app never reached the address mock.real does not list")
	uses, _ := p.realTracker.EndTest()
	require.Equal(t, []models.RealUse{{Dest: v6, Failed: 1}}, uses)
	require.Empty(t, p.RealRun().Reached)
}

// placingContainer is a Docker index placing c at addr: it holds c there only
// once placed closes, and until then says it is placing it (Pending). Each
// wait for its placing is told on awaiting as it begins.
type placingContainer struct {
	addr     string
	c        containerkey.Container
	placed   chan struct{}
	awaiting chan struct{}
}

func (f placingContainer) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	select {
	case <-f.placed:
		if dst.String() == f.addr {
			return f.c, true, nil
		}
	default:
	}
	return containerkey.Container{}, false, nil
}
func (f placingContainer) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (f placingContainer) Pending(netip.AddrPort) ([]string, bool) {
	select {
	case <-f.placed:
		return nil, true
	default:
		return []string{f.c.ID}, true
	}
}
func (f placingContainer) AwaitReadsOf(ctx context.Context, _ []string) {
	f.awaiting <- struct{}{}
	select {
	case <-f.placed:
	case <-ctx.Done():
	}
}
func (placingContainer) Bind(context.Context)               {}
func (placingContainer) Watch(context.Context, *zap.Logger) {}
func (placingContainer) Endpoint() string                   { return "unix:///fake.sock" }

// A container: entry reaches a container Docker started a moment before the
// connection, which the index was still placing at the accept: the accept
// waits for that placing (never for its configuration), and the connection
// is relayed to the real destination, never recorded or mocked under the
// container's key.
func TestAContainerEntryWaitsForItsContainersPlacing(t *testing.T) {
	dst := realServer(t)
	p := realProxy(t, []string{"container:redis"}, nil)
	f := placingContainer{addr: dst, c: containerkey.Container{ID: "r", Image: "redis:7", Port: 6379},
		placed: make(chan struct{}), awaiting: make(chan struct{}, 1)}
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(f)
	t.Cleanup(func() { containerkey.Default = saved })
	if _, _, ok := containerkey.Default.Name(zap.NewNop(), "1", dst); ok {
		t.Fatal("the container was placed at the accept")
	}
	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dst)
	select {
	case <-f.awaiting: // the accept waits for the placing
	case s := <-done:
		t.Fatalf("the accept decided (handled %v) without waiting for the container's placing", s.handled)
	}
	close(f.placed)
	_, err := app.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	got, err := io.ReadAll(app)
	require.NoError(t, err)
	require.Equal(t, "real:hello", string(got))
	s := <-done
	require.True(t, s.handled, "the connection to the listed container was not relayed")
	require.NoError(t, s.err)
}

// A later push of the same run keeps what the run saw when the proxy holds
// the list already; another list, or none, is installed afresh.
func TestKeepRealDepsKeepsTheRunsBookkeeping(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	p := &Proxy{logger: zap.NewNop()}
	require.NoError(t, p.SetRealDeps(ctx, []string{"localhost:5432"}))
	p.realTracker.Dialed(1, "localhost:5432", true)
	require.NoError(t, p.KeepRealDeps(ctx, []string{"localhost:5432"}))
	require.Equal(t, []string{"localhost:5432"}, p.RealRun().Reached, "the same list keeps what the run saw")

	p.realTracker.Dialed(1, "localhost:5432", true)
	require.NoError(t, p.KeepRealDeps(ctx, []string{"localhost:6379"}))
	require.Empty(t, p.RealRun().Reached, "another list starts afresh")
	require.Equal(t, []string{"localhost:6379"}, p.realList().list.Labels())

	fresh := &Proxy{logger: zap.NewNop()}
	require.NoError(t, fresh.KeepRealDeps(ctx, []string{"localhost:5432"}))
	require.NotNil(t, fresh.realList(), "a replacement agent is handed the list")
	require.NoError(t, fresh.KeepRealDeps(ctx, nil))
	require.Nil(t, fresh.realList(), "an empty list clears a list it held")

	gp := &Proxy{logger: zap.NewNop(), GlobalPassthrough: true}
	require.Error(t, gp.KeepRealDeps(ctx, nil), "global passthrough is refused, kept or not")
	t.Setenv(provenswitch.Env, "0")
	require.Error(t, fresh.KeepRealDeps(ctx, nil))
}

// In a record, a connect to a container: entry's container the index was
// still placing at the handshake is dialled with no label. Refused, the app's
// connect is answered at once, never held for the placing; the placing is
// waited for off that path, and the open test's REAL row counts the failure
// once it ends. One whose placing does not end is capture_incomplete: never a
// silent miss.
func TestARefusedDialToAContainerStillBeingPlacedCountsAgainstTheTest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := ln.Addr().String()
	require.NoError(t, ln.Close())
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	for _, placed := range []bool{true, false} {
		p := realProxy(t, []string{"container:redis"}, nil)
		p.setSession(&agent.Session{Mode: models.MODE_RECORD})
		f := placingContainer{addr: refused, c: containerkey.Container{ID: "r", Image: "redis:7", Port: 6379},
			placed: make(chan struct{}), awaiting: make(chan struct{}, 1)}
		k := containerkey.NewKeyer(f)
		session, end := context.WithCancel(context.Background())
		k.Bind(session)
		k.Watch(session, zap.NewNop())
		containerkey.Default = k
		resolved := make(chan struct{}, 1)
		p.realDialResolved = func() { resolved <- struct{}{} }
		p.realTracker.BeginTest()

		// Answered at once: the decision returns before the placing ends.
		d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})(context.Background(), appEnd, proxyEnd)
		require.Equal(t, synhold.Refuse, d.Outcome, "placed=%v", placed)
		<-f.awaiting // the placing is waited for, off the decision's path
		if placed {
			close(f.placed)
		} else {
			end()
		}
		<-resolved
		end()
		uses, inc := p.realTracker.EndTest()
		if placed {
			require.Equal(t, []models.RealUse{{Dest: "container:redis", Failed: 1}}, uses)
			require.Empty(t, inc)
			continue
		}
		require.Empty(t, uses)
		require.Len(t, inc, 1)
		require.Equal(t, refused, inc[0].Destination)
		require.Contains(t, inc[0].Why, "could not place the containers there")
	}
}

// A failed connect while Docker cannot be read, or where nothing is being
// placed, is a dependency's: it charges nothing, and it is answered at once.
func TestARefusedDialWithNothingBeingPlacedChargesNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := ln.Addr().String()
	require.NoError(t, ln.Close())
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	for name, index := range map[string]*containerkey.Keyer{
		"unreadable":      containerkey.NewKeyer(unreadableDocker{}),
		"nothing placing": containerkey.NewKeyer(placingContainer{addr: "x", placed: closedChan(), awaiting: make(chan struct{}, 1)}),
	} {
		containerkey.Default = index
		p := realProxy(t, []string{"container:redis"}, nil)
		p.setSession(&agent.Session{Mode: models.MODE_RECORD})
		p.realDialResolved = func() { t.Errorf("%s: a failed connect was waited on", name) }
		p.realTracker.BeginTest()
		d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})(context.Background(), appEnd, proxyEnd)
		require.Equal(t, synhold.Refuse, d.Outcome, name)
		uses, inc := p.realTracker.EndTest()
		require.Empty(t, uses, name)
		require.Empty(t, inc, name)
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// placedOnPending is an index whose placing ends the moment its placings are
// read: between the index being asked what it places and the destination
// being looked up. Its wait never ends on its own.
type placedOnPending struct{ placingContainer }

func (f placedOnPending) Pending(dst netip.AddrPort) ([]string, bool) {
	ids, could := f.placingContainer.Pending(dst)
	select {
	case <-f.placed:
	default:
		close(f.placed)
	}
	return ids, could
}

func (f placedOnPending) AwaitReadsOf(ctx context.Context, _ []string) {
	f.awaiting <- struct{}{}
	<-ctx.Done()
}

// A container whose placing ends just as a failed connect to it is decided
// is found placed, and the failure counts at once: the placings are read
// before the destination is looked up, so the end of one is never missed.
func TestARefusedDialToAContainerPlacedMeanwhileCountsAtOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := ln.Addr().String()
	require.NoError(t, ln.Close())
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	p := realProxy(t, []string{"container:redis"}, nil)
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	f := placedOnPending{placingContainer{addr: refused, c: containerkey.Container{ID: "r", Image: "redis:7", Port: 6379},
		placed: make(chan struct{}), awaiting: make(chan struct{}, 1)}}
	k := containerkey.NewKeyer(f)
	session, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	k.Bind(session)
	k.Watch(session, zap.NewNop())
	containerkey.Default = k
	p.realTracker.BeginTest()
	d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, refused)})(context.Background(), appEnd, proxyEnd)
	require.Equal(t, synhold.Refuse, d.Outcome)
	uses, inc := p.realTracker.EndTest()
	require.Equal(t, []models.RealUse{{Dest: "container:redis", Failed: 1}}, uses, "the failure was not counted when it was decided")
	require.Empty(t, inc)
}

// A refused connect to a localhost entry's IPv4 loopback written the
// dual-stack way ([127.0.0.1]:p) tries the other loopback family, as one
// written 127.0.0.1:p does.
func TestOtherLoopbackReadsADualStackDestination(t *testing.T) {
	dec := realdeps.Decision{Verdict: realdeps.Real, Label: "localhost:5432"}
	p := realProxy(t, []string{"localhost:5432"}, nil)
	for _, dst := range []string{"127.0.0.1:5432", "[127.0.0.1]:5432"} {
		other, ok := p.otherLoopback(dec, dst, syscall.ECONNREFUSED)
		require.True(t, ok, dst)
		require.Equal(t, "[::1]:5432", other, dst)
	}
}

// P251, a pin: a connection to a mock.real name's address that keploy's DNS
// answered for several names (so it did not name it exactly once) waits for
// its client's first bytes to name it. A server-first destination's client
// sends none: it waits to be greeted. In a replay keploy has dialled nothing,
// so no greeting comes; in a record with the handshake hold the destination
// was dialled at the handshake, and its greeting waits unread in that
// connection. Either way the application gets nothing, and the wait has no
// clock and does not hang the run. The test it was open in ends with the
// connection charged as capture_incomplete (class e), naming the candidates
// and that no name came before the test ended; the run's end closes it,
// nothing of it relayed, recorded or mocked, and the run is not charged
// again. The application's own call, waiting on its greeting, is the
// replay's to end (its bound, or the answer stall rule, train/fix/proven-
// stream-hangs).
func TestAServerFirstRealDestinationKeployCannotNameIsCaptureIncompleteAtItsTestsEnd(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "replay: nothing dialled"
		if held {
			name = "record: dialled at the handshake"
		}
		t.Run(name, func(t *testing.T) {
			var accepted atomic.Int32
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = ln.Close() })
			acceptLoop := make(chan struct{})
			go func() {
				defer close(acceptLoop)
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					accepted.Add(1)
					go func() {
						_, _ = c.Write([]byte("220 mail.test ESMTP\r\n"))
						_, _ = io.Copy(io.Discard, c)
						_ = c.Close()
					}()
				}
			}()
			dst := ln.Addr().String()
			_, portText, _ := net.SplitHostPort(dst)
			p := realProxy(t, []string{"mail.test:" + portText}, nil)
			// keploy's DNS answered both names with the address: it names
			// neither exactly once.
			book := p.real.Load().book
			for _, n := range []string{"mail.test", "other.test"} {
				book.Answered(n, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
			}
			var pre *util.Predialed
			if held {
				up, err := util.DialUpstream(context.Background(), nil, "tcp", dst)
				require.NoError(t, err)
				pre = util.NewPredialed(dst, up)
				t.Cleanup(pre.CloseIfUnused)
			}
			p.realTracker.BeginTest()

			app, acceptedConn := realConnPair(t)
			reading := &readSignal{Conn: acceptedConn, started: make(chan struct{})}
			ctx, endRun := context.WithCancel(context.Background())
			done := make(chan served, 1)
			go func() {
				h, c, err := p.serveReal(ctx, reading, dst, "", 1, pre)
				done <- served{h, c, err}
			}()
			// serveReal reads the client for its name once the connection
			// is pending in the tracker.
			awaitEvent(t, reading.started, "serveReal reading the client for its name")

			// The test ends with the client silent: the connection is
			// charged to it, still waiting.
			_, incomplete := p.realTracker.EndTest()
			select {
			case s := <-done:
				t.Fatalf("serveReal ended (%+v) with the client silent and the run going", s)
			default:
			}
			require.Len(t, incomplete, 1)
			require.Equal(t, models.CaptureIncompleteRealName, incomplete[0].Class)
			require.Equal(t, dst, incomplete[0].Destination)
			require.Contains(t, incomplete[0].Why, "mail.test")
			require.Contains(t, incomplete[0].Why, "its client had sent no name when the test ended")

			endRun()
			s := awaitEvent(t, done, "serveReal ending with the run")
			require.True(t, s.handled, "the connection was never handed on, recorded or mocked")
			// Everything keploy wrote the application arrives before its
			// close: it wrote nothing.
			require.NoError(t, app.SetReadDeadline(time.Now().Add(10*time.Second)))
			got, err := io.ReadAll(app)
			require.NoError(t, err)
			require.Empty(t, got, "the application was sent bytes")
			want := int32(0)
			if held {
				want = 1 // the handshake's own dial, never relayed
			}
			// Every accept is counted once the accept loop has ended. The
			// handshake's connection goes first, as handleConnection's
			// deferred close lets it go at the run's end.
			if pre != nil {
				pre.CloseIfUnused()
			}
			_ = ln.Close()
			awaitEvent(t, acceptLoop, "the destination's accept loop ending")
			require.Equal(t, want, accepted.Load())
			run := p.RealRun()
			require.Empty(t, run.Incomplete, "its test was charged; the run is not charged again")
			require.Empty(t, run.Reached)
		})
	}
}

// readSignal is a connection that says when it is first read.
type readSignal struct {
	net.Conn
	once    sync.Once
	started chan struct{}
}

func (c *readSignal) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Read(p)
}
