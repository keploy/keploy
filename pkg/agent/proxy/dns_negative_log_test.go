package proxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/dnsnegative"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// dnsReplyWriter is the app's end of one DNS exchange: what ServeDNS writes
// back to the app, kept for the test to read.
type dnsReplyWriter struct{ replies []*dns.Msg }

func (w *dnsReplyWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 26789}
}
func (w *dnsReplyWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}
func (w *dnsReplyWriter) WriteMsg(m *dns.Msg) error   { w.replies = append(w.replies, m); return nil }
func (w *dnsReplyWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *dnsReplyWriter) Close() error                { return nil }
func (w *dnsReplyWriter) TsigStatus() error           { return nil }
func (w *dnsReplyWriter) TsigTimersOnly(bool)         {}
func (w *dnsReplyWriter) Hijack()                     {}

// recordingDNSProxy is a Proxy in a recording, its DNS forwarded to the resolver
// at host:port, logging to the returned observer.
func recordingDNSProxy(t *testing.T, host, port string) (*Proxy, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{
		logger:             zap.New(core),
		IP4:                "127.0.0.1",
		IP6:                "::1",
		dnsUpstreamServers: []string{host},
		dnsUpstreamPort:    port,
		dnsForwardTimeout:  2 * time.Second,
		dnsCache:           newDNSCache(),
		recordedDNSMocks:   newRecordedDNSMocksCache(),
		nodataRelayLogged:  newNodataRelayLogCache(),
	}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD, MC: make(chan *models.Mock, 16)})
	// A resolved name binds the process's mock manager to the session's
	// channel (recordDNSMock): unbind it, so no later test sends into it.
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	return p, logs
}

// askDNS sends the app's lookup of name (qtype) to p's DNS handler and returns
// the answer the app got.
func askDNS(t *testing.T, p *Proxy, name string, qtype uint16) *dns.Msg {
	t.Helper()
	r := new(dns.Msg)
	r.SetQuestion(name, qtype)
	w := &dnsReplyWriter{}
	p.ServeDNS(w, r)
	if len(w.replies) != 1 {
		t.Fatalf("the app got %d answers to one lookup of %s %s; want 1", len(w.replies), dns.TypeToString[qtype], name)
	}
	return w.replies[0]
}

// negativeAnswerLines are the lines that name query and say what answer it got.
func negativeAnswerLines(logs *observer.ObservedLogs, level zapcore.Level, query string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.FilterLevelExact(level).All() {
		m := e.ContextMap()
		if m["query"] == query && m["answer"] != nil {
			out = append(out, e)
		}
	}
	return out
}

// splitUpstream starts a resolver that answers the names in resolves with an
// A record and every other name NXDOMAIN, and returns its address's parts.
func splitUpstream(t *testing.T, resolves ...string) (host, port string, stop func()) {
	t.Helper()
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNameError)
		for _, n := range resolves {
			if len(r.Question) > 0 && strings.EqualFold(r.Question[0].Name, n) && r.Question[0].Qtype == dns.TypeA {
				m.SetRcode(r, dns.RcodeSuccess)
				m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 7)}}
			}
		}
		_ = w.WriteMsg(m)
	})
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		stop()
		t.Fatalf("split %q: %v", addr, err)
	}
	return h, p, stop
}

// An app run natively that dials a name only a Docker network resolves (the
// samples-go gin-mongo app's "mongoDb") gets NXDOMAIN from the host's
// resolver, which keploy record passes on unchanged; the app's driver then
// times out 30 s later. keploy said nothing about it, so the agent's log had
// no trace of the lookup at all. It says so once, at INFO, for each
// (name, qtype, answer) however often the app asks again: from keploy's DNS
// cache and after the cache's entry expired and keploy asked the resolver
// again.
func TestRecordSaysOnceThatItPassedOnAnUpstreamNXDOMAIN(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)

	const name = "mongoDb."
	for i := 0; i < 6; i++ {
		if i == 3 {
			p.dnsCache.Purge() // the cache's 30 s are up: keploy asks the resolver again
		}
		for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
			if got := askDNS(t, p, name, qt); got.Rcode != dns.RcodeNameError || len(got.Answer) != 0 {
				t.Fatalf("lookup %d of %s %s: the app got rcode %s with %d answers; want the resolver's NXDOMAIN, unchanged",
					i, dns.TypeToString[qt], name, dns.RcodeToString[got.Rcode], len(got.Answer))
			}
		}
	}

	lines := negativeAnswerLines(logs, zapcore.InfoLevel, name)
	if len(lines) != 2 {
		t.Fatalf("12 lookups of %s (6 A, 6 AAAA), each answered NXDOMAIN, logged %d INFO lines naming it; want 2, one per qtype: %+v", name, len(lines), lines)
	}
	upstream := net.JoinHostPort(host, port)
	for i, qt := range []string{"A", "AAAA"} {
		m := lines[i].ContextMap()
		if m["qtype"] != qt || m["answer"] != "NXDOMAIN" || m["upstream"] != upstream {
			t.Errorf("line %d says qtype %v, answer %v, upstream %v; want %s, NXDOMAIN, %s", i, m["qtype"], m["answer"], m["upstream"], qt, upstream)
		}
		if m["next_step"] != dnsnegative.NextStepDotlessNative {
			t.Errorf("line %d for the dotless name %s gives the next step %q; want the hint that it is likely a container or compose service name", i, name, m["next_step"])
		}
	}

	// The count of the 10 answers not logged one by one is said when the
	// recording stops: at DEBUG, since they repeat the lines above.
	_ = p.SetGracefulShutdown(context.Background())
	got := logs.FilterMessage(dnsnegative.MsgCount).All()
	if len(got) != 1 || got[0].Level != zapcore.DebugLevel || got[0].ContextMap()["repeats"] != uint64(10) || got[0].ContextMap()["debug_lookups"] != uint64(0) {
		t.Fatalf("the stop logged %+v; want one DEBUG count of 10 repeats, no lookup past the limit", got)
	}
}

// A lookup answered from keploy's DNS cache is noted as one answered by the
// resolver: in a recording that starts within the 30 s an earlier one cached
// the answer, the first answer the app gets is a cache hit, and its line still
// names the resolver that gave it.
func TestRecordSaysSoForAnAnswerFromKeploysDNSCache(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	askDNS(t, p, "mongoDb.", dns.TypeA) // cached by the last recording

	p.ResetRecordedDNSMocks() // the next recording starts
	stop()                    // the resolver is gone: only the cache can answer now

	if got := askDNS(t, p, "mongoDb.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Fatalf("the cached answer was not served: the app got %s", dns.RcodeToString[got.Rcode])
	}
	lines := negativeAnswerLines(logs, zapcore.InfoLevel, "mongoDb.")
	if len(lines) != 2 {
		t.Fatalf("one lookup in each of two recordings, the second from the cache, logged %d INFO lines; want one in each: %+v", len(lines), lines)
	}
	if up := lines[1].ContextMap()["upstream"]; up != net.JoinHostPort(host, port) {
		t.Fatalf("the cache hit's line names the resolver %v; want the one that gave the cached answer, %s", up, net.JoinHostPort(host, port))
	}
}

// A compose service's name on a host that searches corp.example.com is asked
// as db.corp.example.com. first, which NXDOMAINs, and then as db., which
// resolves: a healthy run, which no INFO line may call a failure.
func TestRecordSaysNothingOfASearchListWalkThatResolves(t *testing.T) {
	host, port, stop := splitUpstream(t, "db.")
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"corp.example.com"}, 1

	if got := askDNS(t, p, "db.corp.example.com.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Fatalf("the candidate got %s; want NXDOMAIN", dns.RcodeToString[got.Rcode])
	}
	if got := askDNS(t, p, "db.", dns.TypeA); got.Rcode != dns.RcodeSuccess || len(got.Answer) != 1 {
		t.Fatalf("the name as given got %s with %d answers; want it resolved", dns.RcodeToString[got.Rcode], len(got.Answer))
	}
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("a search-list walk that resolved logged %+v at INFO", got)
	}
	if got := negativeAnswerLines(logs, zapcore.DebugLevel, "db.corp.example.com."); len(got) != 1 {
		t.Fatalf("the candidate's NXDOMAIN logged %d DEBUG lines; want 1", len(got))
	}
}

// A name the app asked as given that lies in one of the resolver's search
// domains, and does not exist, gets its INFO line, though its NXDOMAIN looks
// like a candidate's: a redundant expansion of it (the name with a search
// domain appended that it already ends in) shows the app asked for it.
// Kubernetes' ndots:5 asks the candidates first and the name last; a host's
// ndots:1 asks a dotted name first and the candidates after, so its line
// waits for the search, which fails too.
func TestRecordSaysSoForAMissingNameInsideASearchDomain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		search  []string
		ndots   int
		asked   string   // the name the app was configured with
		queries []string // what its resolver asks, in order
		next    string
	}{
		{"Kubernetes, ndots:5: the candidates first", []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}, 5,
			"mongodb.default.svc.cluster.local.", []string{
				"mongodb.default.svc.cluster.local.default.svc.cluster.local.",
				"mongodb.default.svc.cluster.local.svc.cluster.local.",
				"mongodb.default.svc.cluster.local.cluster.local.",
				"mongodb.default.svc.cluster.local.",
			}, dnsnegative.NextStepNoSuchName},
		{"a host, ndots:1: the name first", []string{"corp.example.com"}, 1,
			"Payments.corp.example.com.", []string{
				"Payments.corp.example.com.",
				"Payments.corp.example.com.corp.example.com.",
				"Payments.corp.example.com.corp.example.com.", // again, from the cache: learning it again counts nothing
			}, dnsnegative.NextStepNoSuchName + dnsnegative.NextStepSearchFailed}, // the search failed too, and the line says so
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
			defer stop()
			p, logs := recordingDNSProxy(t, host, port)
			p.dnsSearch, p.dnsNdots = tc.search, tc.ndots
			for _, q := range tc.queries {
				askDNS(t, p, q, dns.TypeA)
			}
			info := negativeAnswerLinesAt(logs, zapcore.InfoLevel)
			if len(info) != 1 || info[0].ContextMap()["query"] != tc.asked {
				t.Fatalf("the resolver's walk %v logged %+v at INFO; want one line, for %s", tc.queries, info, tc.asked)
			}
			if m := info[0].ContextMap(); m["upstream"] != net.JoinHostPort(host, port) || m["next_step"] != tc.next {
				t.Fatalf("the line says %+v; want the resolver %s and the next step %q", m, net.JoinHostPort(host, port), tc.next)
			}
			// The line said in another light is no answer of its own: nothing
			// is counted for it, however often it is learned.
			_ = p.SetGracefulShutdown(context.Background())
			if got := logs.FilterMessage(dnsnegative.MsgCount).All(); len(got) != 0 {
				t.Fatalf("the walk held no line back, yet the stop logged a count: %+v", got)
			}
		})
	}
}

// negativeAnswerLinesAt are the lines at level that say what answer a query got.
func negativeAnswerLinesAt(logs *observer.ObservedLogs, level zapcore.Level) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.FilterLevelExact(level).All() {
		if m := e.ContextMap(); m["query"] != nil && m["answer"] != nil {
			out = append(out, e)
		}
	}
	return out
}

// keploy test does not take this path: a negative answer it passes on (with
// mocking off) is not recording's to explain.
func TestReplayDoesNotLogTheRecordingsNegativeAnswerLine(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.setSession(&agent.Session{Mode: models.MODE_TEST})
	if got := askDNS(t, p, "mongoDb.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Fatalf("with mocking off, replay relays the resolver's answer; got %s", dns.RcodeToString[got.Rcode])
	}
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("replay logged %+v", got)
	}
}

// The rules above stand on resolv.conf's ndots as keploy reads it: tests that
// set dnsNdots by hand keep passing if the read is lost, while every
// external name in every pod would get a false INFO line.
func TestCaptureDNSUpstreamReadsNdots(t *testing.T) {
	for _, tc := range []struct {
		options string
		want    int
	}{
		{"options ndots:5\n", 5},
		{"", 1}, // resolv.conf's default
		{"options ndots:0\n", 0},
	} {
		t.Run(strconv.Quote(tc.options), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, []byte("nameserver 10.96.0.10\nsearch default.svc.cluster.local svc.cluster.local cluster.local\n"+tc.options), 0600); err != nil {
				t.Fatalf("write temp resolv.conf: %v", err)
			}
			orig := resolvConfPath
			resolvConfPath = path
			t.Cleanup(func() { resolvConfPath = orig })
			p := &Proxy{logger: zap.NewNop(), DNSPort: 53}
			p.captureDNSUpstream()
			if p.dnsNdots != tc.want {
				t.Errorf("dnsNdots = %d; want %d", p.dnsNdots, tc.want)
			}
		})
	}
}

// The same name in another case is the same lookup: DNS names are
// case-insensitive, and keploy's DNS cache is not, so "mongodb." is answered
// by the resolver again after "MongoDb.", and noted again.
func TestRecordSaysOnceForANameAskedInTwoCases(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	askDNS(t, p, "MongoDb.", dns.TypeA)
	askDNS(t, p, "mongodb.", dns.TypeA)
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 1 {
		t.Fatalf("MongoDb. then mongodb. logged %d INFO lines; want 1: %+v", len(got), got)
	}
}

// A name glibc and Go ask as given before the search domains (its dots reach
// ndots) that one of them resolves is a healthy run: the line keploy would
// give the name's NXDOMAIN waits for the search, which found it.
func TestRecordSaysNothingOfANameItsSearchFound(t *testing.T) {
	host, port, stop := splitUpstream(t, "svc.prod.corp.example.com.")
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"corp.example.com"}, 1
	askDNS(t, p, "svc.prod.", dns.TypeA)
	askDNS(t, p, "svc.prod.corp.example.com.", dns.TypeA)
	_ = p.SetGracefulShutdown(context.Background())
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("a name its search found logged %+v at INFO", got)
	}
	if got := negativeAnswerLines(logs, zapcore.DebugLevel, "svc.prod."); len(got) != 1 || got[0].ContextMap()["routine"] != dnsnegative.RoutineFound {
		t.Fatalf("the name as given logged %+v at DEBUG; want one line saying the search found it", got)
	}
}

// musl asks a name whose dots reach ndots as given only, so a missing name in
// the first search domain, which a walk of a shorter name would also ask
// first, gets no search after it: when the app asks for it again, twice,
// with no step of a walk between, keploy says so, at INFO, from its DNS
// cache. (Once is not enough: another lookup started between two steps of
// a walk looks the same.)
func TestRecordSaysSoWhenTheAppAsksAgainWithNoSearchBetween(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"corp.example.com"}, 1
	askDNS(t, p, "payments.corp.example.com.", dns.TypeA)
	askDNS(t, p, "payments.corp.example.com.", dns.TypeA) // the app retries
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("a lookup a walk may yet explain logged %+v at INFO", got)
	}
	askDNS(t, p, "payments.corp.example.com.", dns.TypeA) // and again
	got := negativeAnswerLines(logs, zapcore.InfoLevel, "payments.corp.example.com.")
	if len(got) != 1 || got[0].ContextMap()["next_step"] != dnsnegative.NextStepNoSuchName+dnsnegative.NextStepSearchSkipped {
		t.Fatalf("the app asking again logged %+v at INFO; want one line, saying its resolver searched it nowhere in between", got)
	}
}

// A lookup still held when the recording stops is logged then, before the
// count.
func TestRecordLogsWhatItHoldsWhenTheRecordingStops(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"corp.example.com"}, 1
	askDNS(t, p, "nope.example.invalid.", dns.TypeA)
	_ = p.SetGracefulShutdown(context.Background())
	got := negativeAnswerLines(logs, zapcore.InfoLevel, "nope.example.invalid.")
	if len(got) != 1 || got[0].ContextMap()["next_step"] != dnsnegative.NextStepNoSuchName+dnsnegative.NextStepSearchUnseen {
		t.Fatalf("the stop logged %+v at INFO; want the held lookup, saying keploy did not see its search", got)
	}
}

// A capture that sees the app's lookups without the relay (the enterprise
// proxyless DNS capture) notes them in the recording's own log: what it holds
// is logged when the recording stops, and the next recording starts afresh.
func TestRecordSharesItsNegativeAnswerLogWithACapture(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{logger: zap.New(core)}
	l := p.DNSNegativeLog()
	cfg := dnsnegative.Config{Search: []string{"corp.example.com"}, Ndots: 1}
	note := func(name string) {
		q := dns.Question{Name: name, Qtype: dns.TypeA}
		t := l.Ask(zap.New(core), cfg, "", q)
		l.Note(zap.New(core), cfg, t, dnsnegative.Answer{Question: q, Rcode: dns.RcodeNameError, Upstream: "127.0.0.53:53"})
	}
	note("nope.example.invalid.")
	_ = p.SetGracefulShutdown(context.Background())
	if got := negativeAnswerLines(logs, zapcore.InfoLevel, "nope.example.invalid."); len(got) != 1 || got[0].Message != dnsnegative.MsgObserved {
		t.Fatalf("the stop logged %+v; want the capture's held lookup, in its words", got)
	}
	p.ResetRecordedDNSMocks()
	for i := 0; i < 3; i++ { // the app asks again, twice
		note("nope.example.invalid.")
	}
	if got := negativeAnswerLines(logs, zapcore.InfoLevel, "nope.example.invalid."); len(got) != 2 {
		t.Fatalf("the next recording logged the lookup %d times in all; want once in each", len(got))
	}
}

// Another lookup of a name that starts between two steps of the first one's
// walk, its A and then its AAAA answered from keploy's DNS cache, is no
// retry: the first walk's search decides, and finds the name.
func TestRecordTakesNoOverlappingLookupForARetry(t *testing.T) {
	host, port, stop := splitUpstream(t, "svc.prod.corp.example.com.")
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"corp.example.com"}, 1
	askDNS(t, p, "svc.prod.", dns.TypeA)    // the first lookup, asked as given
	askDNS(t, p, "svc.prod.", dns.TypeAAAA) //
	askDNS(t, p, "svc.prod.", dns.TypeA)    // another lookup, from the cache
	askDNS(t, p, "svc.prod.", dns.TypeAAAA) //
	askDNS(t, p, "svc.prod.corp.example.com.", dns.TypeA)
	askDNS(t, p, "svc.prod.corp.example.com.", dns.TypeAAAA)
	_ = p.SetGracefulShutdown(context.Background())
	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("a name its search found logged %+v at INFO", got)
	}
}
