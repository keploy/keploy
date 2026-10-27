package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// lossyUpstream is a fake upstream resolver on a raw UDP socket, so a row
// decides per packet what the resolver does with it: drop it, answer it, or
// answer an earlier one. It never answers by itself; handle does.
type lossyUpstream struct {
	pc   net.PacketConn
	addr string

	mu      sync.Mutex
	queries []lossyQuery
}

type lossyQuery struct {
	msg  *dns.Msg
	from net.Addr
}

// startLossyUpstream binds 127.0.0.1:0 (or host:0) and hands every query
// to handle with its 1-based sequence number and every query seen so far.
func startLossyUpstream(t *testing.T, host string, handle func(u *lossyUpstream, seq int, q lossyQuery)) *lossyUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &lossyUpstream{pc: pc, addr: pc.LocalAddr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, dns.MaxMsgSize)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			msg := new(dns.Msg)
			if msg.Unpack(buf[:n]) != nil {
				continue
			}
			u.mu.Lock()
			u.queries = append(u.queries, lossyQuery{msg: msg, from: from})
			seq := len(u.queries)
			u.mu.Unlock()
			handle(u, seq, lossyQuery{msg: msg, from: from})
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		<-done
	})
	return u
}

func (u *lossyUpstream) seen() []lossyQuery {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]lossyQuery(nil), u.queries...)
}

func (u *lossyUpstream) query(seq int) lossyQuery {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.queries[seq-1]
}

// answerA replies to q, at to, with one A record.
func (u *lossyUpstream) answerA(t *testing.T, q *dns.Msg, to net.Addr, ip string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetReply(q)
	m.Answer = append(m.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP(ip),
	})
	wire, err := m.Pack()
	if err != nil {
		t.Errorf("pack answer: %v", err)
		return
	}
	if _, err := u.pc.WriteTo(wire, to); err != nil {
		t.Errorf("write answer: %v", err)
	}
}

// proxyWithResolvConf builds a record/test-mode Proxy whose upstream list and
// resolver options come from a resolv.conf with the given options line, then
// points the captured servers at the fake upstream's port (resolv.conf has
// no port syntax).
func proxyWithResolvConf(t *testing.T, options string, upstreamAddr string, servers ...string) *Proxy {
	t.Helper()
	host, port, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}
	if len(servers) == 0 {
		servers = []string{host}
	}
	conf := ""
	for _, s := range servers {
		conf += "nameserver " + s + "\n"
	}
	conf += "options " + options + "\n"
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = orig })

	p := newProxyWithUpstream(t, upstreamAddr, 2*time.Second)
	p.dnsUpstreamServers = nil
	p.captureDNSUpstream()
	if len(p.dnsUpstreamServers) != len(servers) {
		t.Fatalf("captured upstream servers %v, want %v", p.dnsUpstreamServers, servers)
	}
	p.dnsUpstreamPort = port
	return p
}

func answeredA(t *testing.T, resp *dns.Msg, want string) {
	t.Helper()
	if resp == nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("answer = %v, want one A %s", resp, want)
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.ParseIP(want)) {
		t.Fatalf("answer = %v, want A %s", resp.Answer[0], want)
	}
}

// Record mode asks the upstream again when its queries are lost, as many
// times as resolv.conf's attempts say, as the app's own stub resolver would
// have: a lost UDP packet is no reason to answer the app SERVFAIL (and log an
// ERROR) while the upstream still answers. Two of three sends are lost here,
// so the default two attempts would not reach the answer.
func TestRecordModeAsksTheUpstreamAgainWhenItsFirstQueryIsLost(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		if seq <= 2 {
			return // the network lost it
		}
		u.answerA(t, q.msg, q.from, "10.0.0.9")
	})
	p := proxyWithResolvConf(t, "timeout:1 attempts:3", u.addr)

	q := dns.Question{Name: "_couchbase._tcp.couchbase.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resp, err := p.recordDNSMock(context.Background(), q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v (the upstream saw %d queries)", err, len(u.seen()))
	}
	answeredA(t, resp.Msg, "10.0.0.9")
	if got := len(u.seen()); got != 3 {
		t.Errorf("the upstream saw %d queries, want 3 (two lost, one answered)", got)
	}
}

// The test-mode forward on a mock miss resends within its budget too: a lost
// first packet must not turn into keploy's synthetic answer for a name the
// upstream resolves.
func TestForwardOnMissAsksTheUpstreamAgainWhenItsFirstQueryIsLost(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		if seq == 1 {
			return
		}
		u.answerA(t, q.msg, q.from, "10.0.0.8")
	})
	p := newProxyWithUpstream(t, u.addr, 3*time.Second)

	q := dns.Question{Name: "mysql.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resp, err := p.forwardDNSUpstream(q)
	if err != nil {
		t.Fatalf("forwardDNSUpstream: %v (the upstream saw %d queries)", err, len(u.seen()))
	}
	answeredA(t, resp, "10.0.0.8")
	if got := len(u.seen()); got != 2 {
		t.Errorf("the upstream saw %d queries, want 2", got)
	}
}

// A resend is the same query from the same socket, so the upstream's late
// answer to the first send still counts. The fake answers the first query
// only once the resend has arrived, at the first query's source address, and
// never answers the resend.
func TestALateAnswerToTheFirstQueryStillCounts(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		if seq != 2 {
			return
		}
		first := u.query(1)
		if q.msg.Id != first.msg.Id {
			t.Errorf("the resend has id %d, the first query %d: want the same query", q.msg.Id, first.msg.Id)
		}
		if q.from.String() != first.from.String() {
			t.Errorf("the resend came from %s, the first query from %s: want the same socket", q.from, first.from)
		}
		u.answerA(t, first.msg, first.from, "10.0.0.7")
	})
	p := proxyWithResolvConf(t, "timeout:1 attempts:2", u.addr)

	q := dns.Question{Name: "late.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resp, err := p.recordDNSMock(context.Background(), q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v (the upstream saw %d queries)", err, len(u.seen()))
	}
	answeredA(t, resp.Msg, "10.0.0.7")
}

// A server that refuses (ICMP port unreachable) is passed over at once, not
// after its wait: the next server is asked straight away. The refusing
// server's wait is 30 s, so an answer before that came from moving on at the
// refusal, never from the wait running out.
func TestARefusingUpstreamIsPassedOverAtOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("127.0.0.2 is a loopback address on Linux only")
	}
	u := startLossyUpstream(t, "127.0.0.2", func(u *lossyUpstream, seq int, q lossyQuery) {
		u.answerA(t, q.msg, q.from, "10.0.0.6")
	})
	_, port, _ := net.SplitHostPort(u.addr)
	// 127.0.0.1 at the same port has nothing bound: it refuses.
	p := proxyWithResolvConf(t, "timeout:30 attempts:1", net.JoinHostPort("127.0.0.1", port), "127.0.0.1", "127.0.0.2")

	q := dns.Question{Name: "next.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	start := time.Now()
	resp, err := p.recordDNSMock(context.Background(), q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	answeredA(t, resp.Msg, "10.0.0.6")
	if waited := time.Since(start); waited >= 30*time.Second {
		t.Errorf("the answer took %s: the refusing server's whole wait, not the refusal, moved the exchange on", waited)
	}
}

// A truncated UDP answer is asked again over TCP, on both paths, and the
// forward names the server it asked over TCP as the resolver that answered.
func TestATruncatedAnswerIsAskedAgainOverTCP(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		m := new(dns.Msg)
		m.SetReply(q.msg)
		m.Truncated = true
		wire, _ := m.Pack()
		_, _ = u.pc.WriteTo(wire, q.from)
	})
	_, port, _ := net.SplitHostPort(u.addr)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("the UDP port's TCP twin is taken: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, ip := range []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"} {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP(ip),
			})
		}
		_ = w.WriteMsg(m)
	})
	srv := &dns.Server{Listener: ln, Handler: mux}
	served := make(chan struct{})
	go func() { _ = srv.ActivateAndServe(); close(served) }()
	t.Cleanup(func() { _ = srv.Shutdown(); <-served })

	p := proxyWithResolvConf(t, "timeout:5 attempts:2", u.addr)
	p.dnsForwardTimeout = 5 * time.Second
	q := dns.Question{Name: "big.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	rec, err := p.recordDNSMock(context.Background(), q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	fwd, resolver, err := p.forwardDNSUpstreamVia(q)
	if err != nil {
		t.Fatalf("forwardDNSUpstreamVia: %v", err)
	}
	if resolver != u.addr {
		t.Errorf("the forward says %q answered, want %q, the server asked over TCP", resolver, u.addr)
	}
	for name, resp := range map[string]*dns.Msg{"record": rec.Msg, "forward": fwd} {
		if resp.Truncated || len(resp.Answer) != 3 {
			t.Errorf("%s: TC=%v with %d answers, want the whole TCP answer (3)", name, resp.Truncated, len(resp.Answer))
		}
	}
}

// The resolver's options come from resolv.conf, capped as glibc caps them.
func TestCaptureDNSUpstreamReadsTheResolverOptions(t *testing.T) {
	for _, tc := range []struct {
		options string
		want    dnsStubPolicy
	}{
		{"timeout:3 attempts:4", dnsStubPolicy{timeout: 3 * time.Second, attempts: 4}},
		{"ndots:5", dnsStubPolicy{timeout: 5 * time.Second, attempts: 2}},
		{"timeout:90 attempts:9", dnsStubPolicy{timeout: 30 * time.Second, attempts: 5}},
		{"timeout:3 rotate attempts:4", dnsStubPolicy{timeout: 3 * time.Second, attempts: 4, rotate: true}},
		{"rotated", dnsStubPolicy{timeout: 5 * time.Second, attempts: 2}},
	} {
		p := proxyWithResolvConf(t, tc.options, "127.0.0.1:53")
		if got := p.dnsStub(); got != tc.want {
			t.Errorf("options %q: policy %+v, want %+v", tc.options, got, tc.want)
		}
	}
	if got := (&Proxy{}).dnsStub(); got != (dnsStubPolicy{timeout: 5 * time.Second, attempts: 2}) {
		t.Errorf("no resolv.conf read: policy %+v, want the defaults", got)
	}
}

// Each round waits twice as long as the one before, shared among the
// servers after the first; the test-mode forward gets the same shape inside
// its budget.
func TestTheResolverScheduleBacksOffAndFitsTheForwardBudget(t *testing.T) {
	pol := dnsStubPolicy{timeout: 5 * time.Second, attempts: 3}
	s := time.Second
	if got, want := pol.waits(1), []time.Duration{5 * s, 10 * s, 20 * s}; !equalWaits(got, want) {
		t.Errorf("one server: %v, want %v", got, want)
	}
	if got, want := pol.waits(2), []time.Duration{5 * s, 5 * s, 5 * s, 5 * s, 10 * s, 10 * s}; !equalWaits(got, want) {
		t.Errorf("two servers: %v, want %v", got, want)
	}
	within := dnsStubPolicy{timeout: 5 * time.Second, attempts: 2}.waitsWithin(1, 3*time.Second)
	if want := []time.Duration{s, 2 * s}; !equalWaits(within, want) {
		t.Errorf("one server within 3 s: %v, want %v", within, want)
	}
}

func equalWaits(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Only a response to this query counts: a reply with another ID (a stale or
// forged answer) and a packet that is not a response are both passed over.
func TestOnlyTheAnswerToThisQueryCounts(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		echo := q.msg.Copy()
		wire, _ := echo.Pack()
		_, _ = u.pc.WriteTo(wire, q.from)

		stale := q.msg.Copy()
		stale.Id++
		u.answerA(t, stale, q.from, "10.6.6.6")

		u.answerA(t, q.msg, q.from, "10.0.0.5")
	})
	p := proxyWithResolvConf(t, "timeout:5 attempts:1", u.addr)

	q := dns.Question{Name: "only.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resp, err := p.recordDNSMock(context.Background(), q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	answeredA(t, resp.Msg, "10.0.0.5")
}

// stepClock is a dnsClock a row steps by hand. Now stays where the row puts
// it; each After the exchange asks for queues in order until the row takes
// it and, if it wants, fires it. So a row reads which wait or budget the
// exchange asked for, and ends a wait by its own event, never by time.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *stepTimer
}

type stepTimer struct {
	d    time.Duration
	fire chan time.Time
}

func newStepClock() *stepClock {
	return &stepClock{now: time.Unix(1_700_000_000, 0), timers: make(chan *stepTimer, 64)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *stepClock) After(d time.Duration) (<-chan time.Time, func()) {
	st := &stepTimer{d: d, fire: make(chan time.Time, 1)}
	c.timers <- st
	return st.fire, func() {}
}

// next is the exchange's next After request, or a failure if the exchange
// ended first.
func (c *stepClock) next(t *testing.T, ended <-chan struct{}) *stepTimer {
	t.Helper()
	select {
	case st := <-c.timers:
		return st
	case <-ended:
		t.Fatalf("the exchange ended before it asked for its next wait")
		return nil
	}
}

type exchangeResult struct {
	msg      *dns.Msg
	resolver string
	err      error
}

// runExchange runs one exchange on clock in the background; ended closes when
// it returns, and its result is then in the returned channel.
func runExchange(ctx context.Context, clock dnsClock, name string, servers []string, port string, waits []time.Duration) (<-chan exchangeResult, <-chan struct{}) {
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	res := make(chan exchangeResult, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		msg, resolver, err := exchangeDNSUpstreamOn(ctx, clock, zap.NewNop(), m, servers, port, waits)
		res <- exchangeResult{msg, resolver, err}
	}()
	return res, ended
}

// The agent's stop ends a record-mode exchange in flight: the DNS servers
// stop at once, not after the exchange's resend schedule. The upstream never
// answers and the first round waits 10 s, so the stop must finish before the
// exchange's resend, an event the upstream sees. On the stop the agent logs
// no "DNS resolution failed" ERROR: the upstream did not fail.
func TestAStopEndsTheRecordExchangeInFlight(t *testing.T) {
	asked, resent := make(chan struct{}), make(chan struct{})
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		switch seq {
		case 1:
			close(asked)
		case 2:
			close(resent)
		}
	})
	p := proxyWithResolvConf(t, "timeout:10 attempts:2", u.addr)
	core, logs := observer.New(zapcore.DebugLevel)
	p.logger = zap.New(core)
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var g *errgroup.Group
	var err error
	for range 5 { // another process can take the free port before the bind
		g = new(errgroup.Group)
		p.DNSPort = uint32(freeDNSPort(t))
		if err = p.startDNSServers(ctx, g); err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	if err != nil {
		t.Fatalf("startDNSServers: %v", err)
	}

	c, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.DNSPort))))
	if err != nil {
		t.Fatalf("dial keploy's DNS server: %v", err)
	}
	defer func() { _ = c.Close() }()
	q := new(dns.Msg)
	q.SetQuestion("_couchbase._tcp.couchbase.", dns.TypeSRV)
	wire, _ := q.Pack()
	if _, err := c.Write(wire); err != nil {
		t.Fatalf("ask keploy's DNS server: %v", err)
	}
	<-asked

	cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- g.Wait() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("the DNS servers stopped with %v", err)
		}
	case <-resent:
		t.Fatalf("the stop waited for the exchange in flight: it resent its query before the DNS servers stopped")
	}
	if n := len(u.seen()); n != 1 {
		t.Errorf("the upstream saw %d queries, want 1: the stop ended the exchange before its resend", n)
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Errorf("the stop logged %d ERROR(s), first %q: want none, the upstream did not fail", n, logs.FilterLevelExact(zapcore.ErrorLevel).All()[0].Message)
	}
}

// A refusal (ICMP port unreachable) is one message: the server's socket reads
// on, so a later answer from that server still reaches the exchange. The
// reader's exit is an event, so a reader that stops at the refusal fails the
// row instead of leaving it waiting.
func TestAServersSocketReadsOnAfterARefusal(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close() // nothing listens: the first send is refused

	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	q := new(dns.Msg)
	q.SetQuestion("again.example.", dns.TypeA)
	wire, _ := q.Pack()

	replies := make(chan dnsUpstreamReply)
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup
	wg.Add(1)
	go readDNSUpstreamReplies(c, 0, q.Id, replies, done, &wg)
	exited := make(chan struct{})
	go func() { wg.Wait(); close(exited) }()

	if _, err := c.Write(wire); err != nil {
		t.Fatalf("first send: %v", err)
	}
	select {
	case rep := <-replies:
		if !errors.Is(rep.err, syscall.ECONNREFUSED) {
			t.Fatalf("first reply %+v, want the refusal", rep)
		}
	case <-exited:
		t.Fatalf("the reader stopped before it handed over the refusal")
	}

	// The server is up now, at the same address, and answers.
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Skipf("the refused address was taken in between: %v", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, dns.MaxMsgSize)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}
		r := new(dns.Msg)
		r.SetReply(m)
		out, _ := r.Pack()
		_, _ = pc.WriteTo(out, from)
	}()
	if _, err := c.Write(wire); err != nil {
		t.Fatalf("second send: %v", err)
	}
	select {
	case rep := <-replies:
		if rep.err != nil || rep.msg == nil || rep.msg.Id != q.Id {
			t.Fatalf("second reply %+v, want the server's answer", rep)
		}
	case <-exited:
		t.Fatalf("the reader stopped at the refusal: the server's later answer was never read")
	}
}

// A late answer from an earlier server counts while a later server is
// awaited. Server 0 answers only once server 1 has been asked (an event), and
// server 1 never answers; the wait for server 1 never ends unless the row
// ends it, so only taking server 0's answer ends the exchange. The exchange
// names server 0 as the resolver that answered, not server 1, the one it
// asked last. The backstop only bounds how long a broken exchange takes to
// fail.
func TestALateAnswerFromAnEarlierServerCounts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("127.0.0.2 is a loopback address on Linux only")
	}
	asked1 := make(chan struct{})
	var once sync.Once
	s1 := startLossyUpstream(t, "127.0.0.2", func(u *lossyUpstream, seq int, q lossyQuery) {
		once.Do(func() { close(asked1) })
	})
	_, port, _ := net.SplitHostPort(s1.addr)
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("the port's 127.0.0.1 twin is taken: %v", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, dns.MaxMsgSize)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}
		<-asked1
		r := new(dns.Msg)
		r.SetReply(m)
		r.Answer = append(r.Answer, &dns.A{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("10.0.2.1")})
		out, _ := r.Pack()
		_, _ = pc.WriteTo(out, from)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backstop := time.AfterFunc(time.Minute, cancel)
	defer backstop.Stop()
	clock := newStepClock()
	res, ended := runExchange(ctx, clock, "early.example.", []string{"127.0.0.1", "127.0.0.2"}, port, []time.Duration{time.Second, time.Second})
	clock.next(t, ended).fire <- time.Time{} // server 0's wait ends: server 1 is asked
	got := <-res
	if got.err != nil {
		t.Fatalf("exchange: %v: server 0's answer, arriving while server 1 was awaited, was not taken", got.err)
	}
	answeredA(t, got.msg, "10.0.2.1")
	if want := net.JoinHostPort("127.0.0.1", port); got.resolver != want {
		t.Errorf("the exchange says %q answered, want %q: server 0's answer is named by the server asked last", got.resolver, want)
	}
}

// The exchange names the server whose answer it returns: here server 1, the
// second asked, because server 0 never answers. Server 0's wait ends when the
// row ends it (an event), so server 1 is asked only then.
func TestTheExchangeNamesTheServerThatAnswered(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("127.0.0.2 is a loopback address on Linux only")
	}
	s1 := startLossyUpstream(t, "127.0.0.2", func(u *lossyUpstream, seq int, q lossyQuery) {
		u.answerA(t, q.msg, q.from, "10.0.4.2")
	})
	_, port, _ := net.SplitHostPort(s1.addr)
	silent, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("the port's 127.0.0.1 twin is taken: %v", err)
	}
	defer func() { _ = silent.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backstop := time.AfterFunc(time.Minute, cancel)
	defer backstop.Stop()
	clock := newStepClock()
	res, ended := runExchange(ctx, clock, "second.example.", []string{"127.0.0.1", "127.0.0.2"}, port, []time.Duration{time.Second, time.Second})
	clock.next(t, ended).fire <- time.Time{} // server 0's wait ends: server 1 is asked
	got := <-res
	if got.err != nil {
		t.Fatalf("exchange: %v", got.err)
	}
	answeredA(t, got.msg, "10.0.4.2")
	if want := net.JoinHostPort("127.0.0.2", port); got.resolver != want {
		t.Errorf("the exchange says %q answered, want %q, the server whose answer it returned", got.resolver, want)
	}
}

// The schedule is absolute: a wait that ended late (the agent was slow to
// wake) comes out of the next one, so the exchange still ends when its
// schedule does. The upstream never answers.
func TestALateWakeUpComesOutOfTheNextWait(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(*lossyUpstream, int, lossyQuery) {})
	host, port, _ := net.SplitHostPort(u.addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newStepClock()
	res, ended := runExchange(ctx, clock, "slow.example.", []string{host}, port, []time.Duration{time.Second, 2 * time.Second})

	first := clock.next(t, ended)
	if first.d != time.Second {
		t.Fatalf("first wait %s, want 1s", first.d)
	}
	clock.advance(1300 * time.Millisecond) // woke 300 ms late
	first.fire <- time.Time{}
	second := clock.next(t, ended)
	if want := 1700 * time.Millisecond; second.d != want {
		t.Errorf("second wait %s, want %s: the late wake-up must come out of it, so the exchange ends 3 s after its start", second.d, want)
	}
	second.fire <- time.Time{}
	if got := <-res; got.err == nil {
		t.Fatalf("exchange answered %v, want no answer", got.msg)
	}
}

// The TCP retry of a truncated answer may take what is left of the whole
// exchange, not only the wait of the send it answers; when that budget runs
// out the exchange moves on to its next send. The UDP answer is truncated and
// the TCP server accepts but never answers.
func TestTheTCPRetryHasWhatIsLeftOfTheExchange(t *testing.T) {
	u := startLossyUpstream(t, "127.0.0.1", func(u *lossyUpstream, seq int, q lossyQuery) {
		m := new(dns.Msg)
		m.SetReply(q.msg)
		m.Truncated = true
		wire, _ := m.Pack()
		_, _ = u.pc.WriteTo(wire, q.from)
	})
	host, port, _ := net.SplitHostPort(u.addr)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Skipf("the UDP port's TCP twin is taken: %v", err)
	}
	var held []net.Conn // accepted, held open, never answered
	var heldMu sync.Mutex
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c)
			heldMu.Unlock()
		}
	}()
	defer func() {
		_ = ln.Close()
		<-accepting
		for _, c := range held {
			_ = c.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Only an exchange whose TCP retry never ends meets the backstop: it
	// then fails below instead of hanging.
	backstop := time.AfterFunc(time.Minute, cancel)
	defer backstop.Stop()
	clock := newStepClock()
	res, ended := runExchange(ctx, clock, "big.example.", []string{host}, port, []time.Duration{time.Second, 2 * time.Second})

	if send0 := clock.next(t, ended); send0.d != time.Second {
		t.Fatalf("first wait %s, want 1s", send0.d)
	}
	tcp := clock.next(t, ended)
	if want := 3 * time.Second; tcp.d != want {
		t.Fatalf("the TCP retry's budget is %s, want %s: what is left of the exchange, not the first send's wait", tcp.d, want)
	}
	tcp.fire <- time.Time{}
	if send1 := clock.next(t, ended); send1.d != 3*time.Second {
		t.Errorf("after the TCP budget ran out the next send waits %s, want 3s (until the schedule's end)", send1.d)
	}
	cancel()
	if got := <-res; !errors.Is(got.err, context.Canceled) {
		t.Errorf("exchange ended with %v, want the context's end", got.err)
	}
}

// resolv.conf's "options rotate" starts each query at the next server in
// turn, as glibc does, on the record path and the test-mode forward alike;
// without it every query starts at the first server.
// Both servers answer at once, each with its own address. The forward names
// the server whose answer it returns as the resolver that answered.
func TestRotateStartsEachQueryAtTheNextServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("127.0.0.2 is a loopback address on Linux only")
	}
	second := startLossyUpstream(t, "127.0.0.2", func(u *lossyUpstream, seq int, q lossyQuery) {
		u.answerA(t, q.msg, q.from, "10.0.3.2")
	})
	_, port, _ := net.SplitHostPort(second.addr)
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("the port's 127.0.0.1 twin is taken: %v", err)
	}
	first := &lossyUpstream{pc: pc}
	go func() {
		buf := make([]byte, dns.MaxMsgSize)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			m := new(dns.Msg)
			if m.Unpack(buf[:n]) == nil {
				first.answerA(t, m, from, "10.0.3.1")
			}
		}
	}()
	t.Cleanup(func() { _ = pc.Close() })

	for _, tc := range []struct {
		options string
		want    []string
	}{
		{"timeout:5 attempts:1 rotate", []string{"10.0.3.1", "10.0.3.2", "10.0.3.1", "10.0.3.2"}},
		{"timeout:5 attempts:1", []string{"10.0.3.1", "10.0.3.1", "10.0.3.1", "10.0.3.1"}},
	} {
		p := proxyWithResolvConf(t, tc.options, net.JoinHostPort("127.0.0.1", port), "127.0.0.1", "127.0.0.2")
		for k, want := range tc.want {
			q := dns.Question{Name: fmt.Sprintf("q%d.rotate.example.", k), Qtype: dns.TypeA, Qclass: dns.ClassINET}
			// The first three are record mode's, the last the test-mode
			// forward's: both paths take their turn from one count.
			var resp *dns.Msg
			var resolver string
			var err error
			if k < 3 {
				var entry dnsCacheEntry
				entry, err = p.recordDNSMock(context.Background(), q, time.Now(), nil)
				resp = entry.Msg
			} else {
				resp, resolver, err = p.forwardDNSUpstreamVia(q)
			}
			if err != nil {
				t.Fatalf("options %q, query %d: %v", tc.options, k, err)
			}
			a, ok := resp.Answer[0].(*dns.A)
			if !ok || a.A.String() != want {
				t.Errorf("options %q, query %d answered by %v, want %s", tc.options, k, resp.Answer[0], want)
			}
			if k >= 3 {
				wantResolver := net.JoinHostPort(map[string]string{"10.0.3.1": "127.0.0.1", "10.0.3.2": "127.0.0.2"}[want], port)
				if resolver != wantResolver {
					t.Errorf("options %q, query %d: the forward says %q answered, want %q", tc.options, k, resolver, wantResolver)
				}
			}
		}
	}
}
