package proxy

import (
	"context"
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

// askedUpstream is a fake upstream resolver that answers every question with
// an address no loopback answer can be mistaken for, and keeps what it was
// asked (startFakeUpstream's readiness probe left out).
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
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	var mu sync.Mutex
	var silentAsked int
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := silent.ReadFrom(buf)
			if err != nil {
				return
			}
			// Only localhost names are counted: the Go resolver grpc-go uses
			// goes on to the host's search list after a negative answer, and
			// those are ordinary names keploy rightly asks upstream.
			m := new(dns.Msg)
			if m.Unpack(buf[:n]) != nil || len(m.Question) == 0 {
				continue
			}
			if name := strings.ToLower(m.Question[0].Name); name == "localhost." || strings.HasSuffix(name, ".localhost.") {
				mu.Lock()
				silentAsked++
				mu.Unlock()
			}
		}
	}()

	up := &askedUpstream{addr: silent.LocalAddr().String()}
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
	mu.Lock()
	n := silentAsked
	mu.Unlock()
	if n != 0 {
		t.Errorf("keploy asked the upstream %d time(s) for a localhost name", n)
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
// never takes the name upstream.
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

// An upstream that does not answer a query a recording forwards is not keploy
// failing: keploy answers the app SERVFAIL, as a recursive resolver answers
// when the servers it asks do not, and a failed answer is never recorded. So
// it is a WARN, once per (name, qtype) a session, then Debug, never an ERROR
// (an ERROR line fails CI lanes that read the agent's log, and enterprise CI
// had to filter this one out by hand).
func TestAnUpstreamThatDoesNotAnswerARecordingIsAWarnOncePerQuestion(t *testing.T) {
	const msg = "DNS resolution failed in record mode"
	p := newProxyWithUpstream(t, testnet.RefusedAddr(t, "udp"), time.Second)
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
}

// The same holds for a mock.real name's live lookup: SERVFAIL to the app, a
// WARN once per (name, qtype), never an ERROR.
func TestARealNameNoUpstreamAnswersIsAWarnOncePerQuestion(t *testing.T) {
	const msg = "keploy could not resolve a mock.real name with the upstream resolver; the app is answered SERVFAIL"
	p := newProxyWithUpstream(t, testnet.RefusedAddr(t, "udp"), 200*time.Millisecond)
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
}

// The agent's proxy dedupes the WARN from its first query: New builds the
// tracker, so a long upstream outage is one WARN per (name, qtype).
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
