package proxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// askedUpstream is a fake upstream resolver that keeps what it was asked.
// newAskedUpstream's answers A, AAAA and TXT questions with records no
// loopback or local answer can be mistaken for, and others with no record
// (startFakeUpstream's readiness probe left out); newSilentUpstream's
// answers none.
type askedUpstream struct {
	addr string
	mu   sync.Mutex
	qs   []string
}

func newAskedUpstream(t *testing.T) *askedUpstream {
	t.Helper()
	u := &askedUpstream{}
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			if q.Name == "probe.test." {
				continue
			}
			u.mu.Lock()
			u.qs = append(u.qs, dns.TypeToString[q.Qtype]+" "+q.Name)
			u.mu.Unlock()
			hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: 60}
			switch q.Qtype {
			case dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: net.ParseIP("192.0.2.53")})
			case dns.TypeAAAA:
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: net.ParseIP("2001:db8::53")})
			case dns.TypeTXT:
				m.Answer = append(m.Answer, &dns.TXT{Hdr: hdr, Txt: []string{"from-upstream"}})
			}
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	u.addr = addr
	return u
}

func (u *askedUpstream) asked() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.qs...)
}

// wantAsked checks that u was asked n questions. The asks are over when it
// is called, but u's reader may not have read the last of them yet on a
// loaded machine, so it waits up to 5 s for n before it compares.
func wantAsked(t *testing.T, u *askedUpstream, n int, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(u.asked()) < n && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := u.asked(); len(got) != n {
		t.Errorf("%s was asked %v, want %d question(s)", what, got, n)
	}
}

// newSilentUpstream is an upstream resolver that reads every question and
// answers none, so every exchange with it times out. It keeps what it was
// asked.
func newSilentUpstream(t *testing.T) *askedUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &askedUpstream{addr: pc.LocalAddr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			m := new(dns.Msg)
			if m.Unpack(buf[:n]) != nil || len(m.Question) == 0 {
				continue
			}
			u.mu.Lock()
			u.qs = append(u.qs, dns.TypeToString[m.Question[0].Qtype]+" "+m.Question[0].Name)
			u.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		<-done
	})
	return u
}

// shortRecordTimeout makes a recording's upstream exchange give up after
// 200 ms for the test, so a silent upstream costs that, not 5 s.
func shortRecordTimeout(t *testing.T) {
	t.Helper()
	prev := recordDNSTimeout
	recordDNSTimeout = 200 * time.Millisecond
	t.Cleanup(func() { recordDNSTimeout = prev })
}

// localhostTestProxy is a proxy forwarding to up, with a proxy address that is
// not loopback, so keploy's own steering answer is told apart from the
// loopback one, and its log kept.
func localhostTestProxy(t *testing.T, up *askedUpstream, s *agent.Session) (*Proxy, *observer.ObservedLogs) {
	t.Helper()
	p := newProxyWithUpstream(t, up.addr, 2*time.Second)
	core, logs := observer.New(zapcore.DebugLevel)
	p.logger = zap.New(core)
	p.IP4 = "10.9.9.9"
	p.IP6 = "fd00::99"
	p.EnableIPv6Redirect = true
	p.errChannel = make(chan error, 64)
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	p.mockManager = mgr
	p.setSession(s)
	return p, logs
}

func askKeploy(t *testing.T, server, name string, qtype uint16) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	r, _, err := (&dns.Client{Net: "udp", Timeout: 20 * time.Second}).Exchange(q, server)
	if err != nil {
		t.Fatalf("%s %s: keploy's DNS did not answer: %v", dns.TypeToString[qtype], name, err)
	}
	return r
}

func noErrorLines(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	for _, e := range logs.All() {
		if e.Level >= zapcore.ErrorLevel {
			t.Errorf("keploy logged %s %q %v", e.Level, e.Message, e.ContextMap())
		}
	}
}

// RFC 6761 6.3: a localhost name (localhost. and every name under it) is
// answered by the resolver itself, the loopback address for an address query
// and a negative answer for any other type, and is never asked of another
// server. keploy's DNS stands in for the app's resolver, so it answers them
// so, the same in a recording and in a replay, isolated or not: it never asks
// an upstream (where a timeout was an ERROR line that failed CI's gRPC fuzzer
// on grpc-go's _grpc_config.localhost TXT probe), never answers with its own
// steering address, and never reports one as a missing mock.
func TestLocalhostNamesAreAnsweredByKeployAndNeverAskedUpstream(t *testing.T) {
	sessions := map[string]*agent.Session{
		"record":                  {Mode: models.MODE_RECORD},
		"replay":                  {Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}},
		"replay with DNS egress":  {Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}},
		"replay with mocking off": {Mode: models.MODE_TEST},
	}
	for mode, s := range sessions {
		t.Run(mode, func(t *testing.T) {
			up := newAskedUpstream(t)
			p, logs := localhostTestProxy(t, up, s)
			p.allowEgressDNS = mode == "replay with DNS egress"
			// The answer cache is not asked for a localhost name either: an
			// answer it held for one (none is put there) is not the app's.
			stale := new(dns.Msg)
			stale.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "localhost.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.99")}}
			p.dnsCache.Add(generateCacheKey("localhost.", dns.TypeA), dnsCacheEntry{Msg: stale, FromUpstream: true})
			server := serveDNSOn(t, p, "udp")

			for _, c := range []struct {
				name  string
				qtype uint16
				want  string // the one answer's address; "" for an empty answer
			}{
				{"_grpc_config.localhost.", dns.TypeTXT, ""},
				{"localhost.", dns.TypeA, "127.0.0.1"},
				{"localhost.", dns.TypeAAAA, "::1"},
				{"Svc.LocalHost.", dns.TypeA, "127.0.0.1"},
				{"a.b.localhost.", dns.TypeAAAA, "::1"},
				{"svc.localhost.", dns.TypeMX, ""},
				{"_grpclb._tcp.localhost.", dns.TypeSRV, ""},
				{"localhost.", dns.TypeTXT, ""},
			} {
				r := askKeploy(t, server, c.name, c.qtype)
				what := dns.TypeToString[c.qtype] + " " + c.name
				if r.Rcode != dns.RcodeSuccess {
					t.Errorf("%s: rcode %s, want NOERROR", what, dns.RcodeToString[r.Rcode])
				}
				var got []string
				for _, rr := range r.Answer {
					switch v := rr.(type) {
					case *dns.A:
						got = append(got, v.A.String())
					case *dns.AAAA:
						got = append(got, v.AAAA.String())
					default:
						got = append(got, rr.String())
					}
					if !strings.EqualFold(rr.Header().Name, c.name) {
						t.Errorf("%s: answer names %q", what, rr.Header().Name)
					}
				}
				switch {
				case c.want == "" && len(got) != 0:
					t.Errorf("%s: answered %v, want no record (NODATA)", what, got)
				case c.want != "" && (len(got) != 1 || got[0] != c.want):
					t.Errorf("%s: answered %v, want [%s]", what, got, c.want)
				}
			}
			if a := up.asked(); len(a) != 0 {
				t.Errorf("keploy asked its upstream for localhost names: %v", a)
			}
			assertNotReported(t, p, "a localhost name is answered by keploy, never a missing mock")
			noErrorLines(t, logs)
		})
	}
}

// Only localhost. and names under it are localhost names: a name that merely
// ends in the letters, or holds localhost as an inner label, is an ordinary
// name and is resolved as before (here: asked upstream in a recording).
func TestNamesThatOnlyContainLocalhostAreAskedUpstream(t *testing.T) {
	up := newAskedUpstream(t)
	p, _ := localhostTestProxy(t, up, &agent.Session{Mode: models.MODE_RECORD})
	server := serveDNSOn(t, p, "udp")
	names := []string{"notlocalhost.", "localhost.example.com.", "localhostx."}
	for _, n := range names {
		r := askKeploy(t, server, n, dns.TypeA)
		if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "192.0.2.53" {
			t.Errorf("A %s: answered %v, want upstream's 192.0.2.53", n, r.Answer)
		}
	}
	if a := up.asked(); len(a) != len(names) {
		t.Errorf("upstream was asked %v, want each of %v once", a, names)
	}
}

// What CI's gRPC fuzzer hit, end to end: a grpc-go client of localhost,
// recorded, with an upstream resolver that never answers. grpc-go's dns
// resolver asks TXT _grpc_config.localhost before it hands its first
// addresses over; keploy answers it itself, so the upstream is never asked,
// nothing is logged as an error, and the client's call goes through.
func TestARecordedGRPCClientOfLocalhostAsksNoUpstream(t *testing.T) {
	// The Go resolver goes on to the host's search names (after a negative
	// answer, or first when ndots is above 1), and keploy rightly forwards
	// those to the silent upstream: a short timeout keeps that from using up
	// grpc-go's resolving time on a host with a long search list.
	shortRecordTimeout(t)
	up := newSilentUpstream(t)
	p, logs := localhostTestProxy(t, up, &agent.Session{Mode: models.MODE_RECORD})
	var txtMu sync.Mutex
	var txtAnswers []*dns.Msg
	answered := make(chan struct{}, 1)
	keploy := &dns.Server{Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		rec := &recordingWriter{ResponseWriter: w}
		p.ServeDNS(rec, r)
		if len(r.Question) == 1 && r.Question[0].Qtype == dns.TypeTXT && strings.EqualFold(r.Question[0].Name, "_grpc_config.localhost.") {
			txtMu.Lock()
			txtAnswers = append(txtAnswers, rec.msg)
			txtMu.Unlock()
			select {
			case answered <- struct{}{}:
			default:
			}
		}
	})}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	keploy.PacketConn = pc
	started := make(chan struct{})
	keploy.NotifyStartedFunc = func() { close(started) }
	go func() { _ = keploy.ActivateAndServe() }()
	t.Cleanup(func() { _ = keploy.Shutdown() })
	<-started

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	_, port, _ := net.SplitHostPort(lis.Addr().String())

	// dns://<resolver>/<host>: grpc-go's dns resolver, asking keploy's DNS.
	conn, err := grpc.NewClient("dns://"+pc.LocalAddr().String()+"/localhost:"+port,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("the gRPC call to localhost did not go through: %v", err)
	}
	select {
	case <-answered:
	case <-ctx.Done():
		t.Fatal("grpc-go never asked keploy for TXT _grpc_config.localhost")
	}
	txtMu.Lock()
	first := txtAnswers[0]
	txtMu.Unlock()
	if first == nil || first.Rcode != dns.RcodeSuccess || len(first.Answer) != 0 {
		t.Errorf("TXT _grpc_config.localhost answered %v, want NOERROR with no record", first)
	}
	// Only localhost names count: the Go resolver grpc-go uses goes on to the
	// host's search list after a negative answer, and those are ordinary
	// names keploy rightly asks upstream.
	for _, q := range up.asked() {
		if name := strings.ToLower(q[strings.IndexByte(q, ' ')+1:]); name == "localhost." || strings.HasSuffix(name, ".localhost.") {
			t.Errorf("keploy asked the upstream %s, a localhost name", q)
		}
	}
	noErrorLines(t, logs)
}

// recordingWriter keeps the message keploy's handler wrote.
type recordingWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (w *recordingWriter) WriteMsg(m *dns.Msg) error {
	w.msg = m.Copy()
	return w.ResponseWriter.WriteMsg(m)
}

// A run whose mock.real lists a localhost name still has it answered by
// keploy: the list is asked after the localhost check, so its live lookup
// never takes the name upstream. keploy's answer is still booked as the
// listed name's, so a connection to 127.0.0.1 on the listed port is that
// name's, made REAL at its handshake.
func TestALocalhostNameOnTheMockRealListIsAnsweredByKeploy(t *testing.T) {
	up := newAskedUpstream(t)
	p, logs := localhostTestProxy(t, up, &agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
	t.Setenv(provenswitch.Env, "1")
	list, err := realdeps.Parse([]string{"svc.localhost:443", "auth.test:443"})
	if err != nil {
		t.Fatal(err)
	}
	p.real.Store(&realState{list: list, book: &realdeps.Book{}})
	server := serveDNSOn(t, p, "udp")
	if r := askKeploy(t, server, "svc.localhost.", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "127.0.0.1" {
		t.Errorf("A svc.localhost.: answered %v, want [127.0.0.1]", r.Answer)
	}
	if label, ok := p.realAtHandshake(ipv4Dest(t, "127.0.0.1:443")); !ok || label != "svc.localhost:443" {
		t.Errorf("a connection to 127.0.0.1:443 is REAL %v as %q, want REAL as svc.localhost:443: keploy's answer was not booked as the listed name's", ok, label)
	}
	// The list is still asked for its other names: auth.test is looked up live.
	if r := askKeploy(t, server, "auth.test.", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "192.0.2.53" {
		t.Errorf("A auth.test.: answered %v, want upstream's 192.0.2.53", r.Answer)
	}
	if a := up.asked(); len(a) != 1 || a[0] != "A auth.test." {
		t.Errorf("upstream was asked %v, want only [A auth.test.]", a)
	}
	noErrorLines(t, logs)
}

// unansweredLines is what logs holds at each level for msg about name.
func unansweredLines(logs *observer.ObservedLogs, msg, name, qtype string) map[zapcore.Level]int {
	got := map[zapcore.Level]int{}
	for _, e := range logs.FilterMessage(msg).All() {
		f := e.ContextMap()
		if f["query"] == name && f["qtype"] == qtype && f["error"] != nil {
			got[e.Level]++
		}
	}
	return got
}

func wantUnanswered(t *testing.T, logs *observer.ObservedLogs, msg, name, qtype string, warn, debug int) {
	t.Helper()
	got := unansweredLines(logs, msg, name, qtype)
	if got[zapcore.WarnLevel] != warn || got[zapcore.DebugLevel] != debug || len(got) > 2 || (len(got) == 2 && (warn == 0 || debug == 0)) {
		t.Errorf("%s %s: logged %v, want %d WARN and %d Debug and nothing else", qtype, name, got, warn, debug)
	}
}

// An upstream of the app's that does not answer a query a recording forwards,
// by refusing it or by staying silent until the exchange times out (CI's
// case), is not keploy failing: keploy answers the app SERVFAIL, as a
// recursive resolver answers when the servers it asks do not, and a failed
// answer is never recorded. So it is a WARN once per (name, qtype) every 30
// minutes, and again in a new session, Debug in between, never an ERROR (an
// ERROR line fails CI lanes that read the agent's log, and enterprise CI had
// to filter this one out by hand).
func TestAnUpstreamThatDoesNotAnswerARecordingIsAWarnOncePerQuestion(t *testing.T) {
	const msg = "DNS resolution failed in record mode"
	shortRecordTimeout(t)
	silent := newSilentUpstream(t)
	for kind, addr := range map[string]string{
		"a refused upstream": testnet.RefusedAddr(t, "udp"),
		"a silent upstream":  silent.addr,
	} {
		t.Run(kind, func(t *testing.T) {
			p := newProxyWithUpstream(t, addr, time.Second)
			core, logs := observer.New(zapcore.DebugLevel)
			p.logger = zap.New(core)
			p.unansweredForwardLogged = newNodataRelayLogCache()
			ask := func(name string, qtype uint16) {
				t.Helper()
				resp := p.resolveUncachedDNSResponse(dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET}, models.MODE_RECORD, true, time.Now(), nil)
				if resp.Msg == nil || resp.Rcode != dns.RcodeServerFailure || len(resp.Answer) != 0 {
					t.Fatalf("%s %s: answered %v, want SERVFAIL", dns.TypeToString[qtype], name, resp.Msg)
				}
			}
			ask("svc.example.", dns.TypeA)
			ask("svc.example.", dns.TypeA)
			ask("svc.example.", dns.TypeAAAA)
			wantUnanswered(t, logs, msg, "svc.example.", "A", 1, 1)
			wantUnanswered(t, logs, msg, "svc.example.", "AAAA", 1, 0)
			noErrorLines(t, logs)

			// A new session says it again.
			p.ResetRecordedDNSMocks()
			ask("svc.example.", dns.TypeA)
			wantUnanswered(t, logs, msg, "svc.example.", "A", 2, 1)
		})
	}
	// The silent upstream was asked each time, so each failure was its timeout.
	wantAsked(t, silent, 4, "the silent upstream")
}

// A recording with no upstream resolver of the app's captured at startup
// asks public resolvers keploy picked (recordDNSFallback), which the app's own
// resolver would not have asked. When none of them answers, the SERVFAIL is
// keploy's own, not the one the app's resolver gives, so every such failure
// stays an ERROR.
func TestARecordingWithNoUpstreamOfTheAppsIsStillAnError(t *testing.T) {
	const msg = "DNS resolution failed in record mode"
	shortRecordTimeout(t)
	silent := newSilentUpstream(t)
	prev := recordDNSFallback
	recordDNSFallback = []string{testnet.RefusedAddr(t, "udp"), silent.addr}
	t.Cleanup(func() { recordDNSFallback = prev })

	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{logger: zap.New(core), unansweredForwardLogged: newNodataRelayLogCache()}
	q := dns.Question{Name: "svc.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	for i := 0; i < 2; i++ {
		resp := p.resolveUncachedDNSResponse(q, models.MODE_RECORD, true, time.Now(), nil)
		if resp.Msg == nil || resp.Rcode != dns.RcodeServerFailure || len(resp.Answer) != 0 {
			t.Fatalf("answered %v, want SERVFAIL", resp.Msg)
		}
	}
	all, errs := logs.FilterMessage(msg).Len(), logs.FilterMessage(msg).FilterLevelExact(zapcore.ErrorLevel).All()
	if all != 2 || len(errs) != 2 {
		t.Errorf("logged the failure %d times, %d at ERROR; want 2, both at ERROR", all, len(errs))
	}
	for _, e := range errs {
		if f := e.ContextMap(); f["query"] != "svc.example." || f["qtype"] != "A" || f["error"] == nil {
			t.Errorf("the ERROR logs %v, want the question (query svc.example., qtype A) and the error", f)
		}
	}
	wantAsked(t, silent, 2, "the fallback resolver")
}

// The same holds for a mock.real name's live lookup, with an upstream that
// refuses it or stays silent until the forward times out: SERVFAIL to the
// app, a WARN once per (name, qtype) every 30 minutes, never an ERROR.
func TestARealNameNoUpstreamAnswersIsAWarnOncePerQuestion(t *testing.T) {
	const msg = "keploy could not resolve a mock.real name with the upstream resolver; the app is answered SERVFAIL"
	silent := newSilentUpstream(t)
	for name, addr := range map[string]string{
		"a refused upstream": testnet.RefusedAddr(t, "udp"),
		"a silent upstream":  silent.addr,
	} {
		t.Run(name, func(t *testing.T) {
			p := newProxyWithUpstream(t, addr, 200*time.Millisecond)
			core, logs := observer.New(zapcore.DebugLevel)
			p.logger = zap.New(core)
			p.unansweredForwardLogged = newNodataRelayLogCache()
			q := dns.Question{Name: "auth.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			for i := 0; i < 2; i++ {
				if resp := p.realDNS(q); resp.Rcode != dns.RcodeServerFailure {
					t.Fatalf("answered %v, want SERVFAIL", resp.Msg)
				}
			}
			wantUnanswered(t, logs, msg, "auth.test.", "A", 1, 1)
			noErrorLines(t, logs)
		})
	}
	// The silent upstream was asked each time, so each failure was its timeout.
	wantAsked(t, silent, 2, "the silent upstream")
}

// A mock.real name keploy asked no upstream for is keploy's failure, not an
// upstream's: for a type keploy does not forward (HTTPS here), or with no
// upstream to ask, the app is answered SERVFAIL without the live answer it was
// promised, so every such lookup stays an ERROR and no upstream is asked.
func TestARealNameKeployAsksNoUpstreamForIsStillAnError(t *testing.T) {
	const msg = "keploy could not resolve a mock.real name with the upstream resolver; the app is answered SERVFAIL"
	up := newAskedUpstream(t)
	for _, tc := range []struct {
		name  string
		p     *Proxy
		qtype uint16
	}{
		{"a type keploy does not forward", newProxyWithUpstream(t, up.addr, time.Second), dns.TypeHTTPS},
		{"no upstream to ask", &Proxy{}, dns.TypeA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			tc.p.logger = zap.New(core)
			tc.p.unansweredForwardLogged = newNodataRelayLogCache()
			q := dns.Question{Name: "auth.test.", Qtype: tc.qtype, Qclass: dns.ClassINET}
			for i := 0; i < 2; i++ {
				if resp := tc.p.realDNS(q); resp.Rcode != dns.RcodeServerFailure {
					t.Fatalf("answered %v, want SERVFAIL", resp.Msg)
				}
			}
			all, errs := logs.FilterMessage(msg).Len(), logs.FilterMessage(msg).FilterLevelExact(zapcore.ErrorLevel).Len()
			if all != 2 || errs != 2 {
				t.Errorf("logged the failure %d times, %d at ERROR; want 2, both at ERROR", all, errs)
			}
		})
	}
	if got := up.asked(); len(got) != 0 {
		t.Errorf("asked the upstream %v, want nothing", got)
	}
}

// The forwarder's error says whether it asked: errNoUpstreamAnswered only
// when it asked the upstream and none answered (refused, timed out, or the
// forward's deadline spent before the next server), not when it asked none.
func TestForwardDNSMarksOnlyAnUnansweredAsk(t *testing.T) {
	q := dns.Question{Name: "auth.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	refused := newProxyWithUpstream(t, testnet.RefusedAddr(t, "udp"), 200*time.Millisecond)
	if _, err := refused.forwardDNSUpstream(q); !errors.Is(err, errNoUpstreamAnswered) {
		t.Errorf("a refused upstream: err %v, want errNoUpstreamAnswered", err)
	}

	// A silent upstream: the error is the exchange's own timeout, marked.
	silent := newSilentUpstream(t)
	_, err := newProxyWithUpstream(t, silent.addr, 200*time.Millisecond).forwardDNSUpstream(q)
	var ne net.Error
	if !errors.Is(err, errNoUpstreamAnswered) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("a silent upstream: err %v, want errNoUpstreamAnswered over a timeout", err)
	}
	wantAsked(t, silent, 1, "the silent upstream")

	// Two silent upstreams: the first one's timeout spends the forward's
	// deadline, so the second is never asked. The error is the forward's
	// own deadline, not an exchange's timeout, and it is marked too.
	silent2 := newSilentUpstream(t)
	two := newProxyWithUpstream(t, silent2.addr, 200*time.Millisecond)
	two.dnsUpstreamServers = []string{"127.0.0.1", "127.0.0.1"}
	if _, err := two.forwardDNSUpstream(q); !errors.Is(err, errNoUpstreamAnswered) || errors.As(err, &ne) {
		t.Errorf("two silent upstreams: err %v, want errNoUpstreamAnswered over the forward's deadline, not an exchange's error", err)
	}
	wantAsked(t, silent2, 1, "two silent upstreams (the first only)")

	up := newAskedUpstream(t)
	httpsQ := q
	httpsQ.Qtype = dns.TypeHTTPS
	for name, ask := range map[string]func() error{
		"a type keploy does not forward": func() error {
			_, err := newProxyWithUpstream(t, up.addr, time.Second).forwardDNSUpstream(httpsQ)
			return err
		},
		"no upstream to ask": func() error { _, err := (&Proxy{}).forwardDNSUpstream(q); return err },
	} {
		if err := ask(); err == nil || errors.Is(err, errNoUpstreamAnswered) {
			t.Errorf("%s: err %v, want an error that is not errNoUpstreamAnswered", name, err)
		}
	}
}

// The agent's proxy dedupes the WARN from its first query: New builds the
// tracker, so a long upstream outage is one WARN per (name, qtype) every 30
// minutes.
func TestTheAgentsProxyDedupesTheUnansweredForwardWarn(t *testing.T) {
	p := New(zap.NewNop(), nil, config.New())
	key := generateCacheKey("svc.example.", dns.TypeA)
	if !firstNotice(p.unansweredForwardLogged, key) || firstNotice(p.unansweredForwardLogged, key) {
		t.Error("New's proxy does not dedupe the unanswered forward WARN")
	}
}

// A localhost name is localhost. or a name under it, in any case, with or
// without the root's dot; nothing else is.
func TestIsLocalhostName(t *testing.T) {
	for name, want := range map[string]bool{
		"localhost.": true, "localhost": true, "LOCALHOST.": true,
		"svc.localhost": true, "_grpc_config.localhost.": true, "a.b.LocalHost.": true,
		"notlocalhost.": false, "localhostx.": false, "localhost.example.com.": false,
		"localhost.localdomain.": false, ".": false, "": false,
	} {
		if got := isLocalhostName(name); got != want {
			t.Errorf("isLocalhostName(%q) = %v, want %v", name, got, want)
		}
	}
}
