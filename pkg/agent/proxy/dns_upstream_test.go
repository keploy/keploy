package proxy

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
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
	resp, err := p.recordDNSMock(q, time.Now(), nil)
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
	resp, err := p.recordDNSMock(q, time.Now(), nil)
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
	resp, err := p.recordDNSMock(q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	answeredA(t, resp.Msg, "10.0.0.6")
	if waited := time.Since(start); waited >= 30*time.Second {
		t.Errorf("the answer took %s: the refusing server's whole wait, not the refusal, moved the exchange on", waited)
	}
}

// A truncated UDP answer is asked again over TCP, on both paths.
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

	rec, err := p.recordDNSMock(q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	fwd, err := p.forwardDNSUpstream(q)
	if err != nil {
		t.Fatalf("forwardDNSUpstream: %v", err)
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
	resp, err := p.recordDNSMock(q, time.Now(), nil)
	if err != nil {
		t.Fatalf("recordDNSMock: %v", err)
	}
	answeredA(t, resp.Msg, "10.0.0.5")
}
