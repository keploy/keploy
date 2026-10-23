package proxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
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
		if m["next_step"] != nextStepDotlessNative {
			t.Errorf("line %d for the dotless name %s gives the next step %q; want the hint that it is likely a container or compose service name", i, name, m["next_step"])
		}
	}

	// The count of the 10 answers not logged one by one is said when the
	// recording stops: at DEBUG, since they repeat the lines above.
	_ = p.SetGracefulShutdown(context.Background())
	got := logs.FilterMessageSnippet("without an INFO line of their own").All()
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

// negativeEntry is an answer from the upstream resolver at upstream, with
// rcode and no records.
func negativeEntry(rcode int, upstream string) dnsCacheEntry {
	return dnsCacheEntry{Msg: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: rcode}}, FromUpstream: true, upstream: upstream}
}

// Which unresolved answers a recording gives an INFO line, and with what next
// step; which a DEBUG line only; and which none. Only an NXDOMAIN is ever
// routine: a search-list candidate's is the resolver walking its list (a
// compose name "db" is asked as db.corp.example.com. first on a host that
// searches corp.example.com, then as db., which resolves), a reverse lookup's
// is what a private address has, and gRPC's service-config probe NXDOMAINs in
// every healthy gRPC app; an INFO line for any of them would read as a
// failure in a run that has none. A SERVFAIL ends musl's lookup at the
// candidate, so it is the lookup's outcome wherever it comes, but for a
// reverse lookup's, which is never a step of the app's own. A name with no
// dot gets the container-name hint whatever the answer, by where keploy runs.
// A name a resolver asks before its search domains (its dots reach ndots) says
// the search may yet find it. Under Docker's ndots:0 musl never searches, so a
// name inside a search domain is the app's own.
func TestRecordLogsANegativeAnswerByWhatItTellsTheUser(t *testing.T) {
	const upstream = "192.0.2.53:53"
	corp := []string{".", "corp.example.com"} // "search ." (systemd-resolved's stub) names no domain
	k8s := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	nx := negativeEntry(dns.RcodeNameError, upstream)
	for _, tc := range []struct {
		name   string
		search []string
		ndots  int
		docker bool
		q      dns.Question
		resp   dnsCacheEntry
		level  zapcore.Level // the line's level; zapcore.InvalidLevel: no line
		next   string
	}{
		{"a dotless name, NXDOMAIN, native: INFO, the Docker-network hint", corp, 1, false, dns.Question{Name: "mongoDb.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepDotlessNative},
		{"a dotless name, SERVFAIL, native: INFO, the Docker-network hint", nil, 0, false, dns.Question{Name: "mongoDb.", Qtype: dns.TypeA}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.InfoLevel, nextStepDotlessNative},
		{"a dotless name, REFUSED, native: INFO, the Docker-network hint", corp, 1, false, dns.Question{Name: "redis.", Qtype: dns.TypeA}, negativeEntry(dns.RcodeRefused, upstream), zapcore.InfoLevel, nextStepDotlessNative},
		{"a dotless name, NXDOMAIN, agent in Docker: INFO, the agent's network", nil, 0, true, dns.Question{Name: "mongo.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepDotlessDocker},
		{"a dotless name, NXDOMAIN, in a pod: INFO, no such Service", k8s, 5, false, dns.Question{Name: "mongo.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepDotlessK8s},
		{"a dotless name, SERVFAIL, in a pod: INFO, no such Service", k8s, 5, false, dns.Question{Name: "mongo.", Qtype: dns.TypeA}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.InfoLevel, nextStepDotlessK8s},
		{"a dotless name, a search domain named svc.: no pod", []string{"svc.example.com"}, 1, false, dns.Question{Name: "mongo.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepDotlessNative},
		{"a dotted name, NXDOMAIN, no search domain: INFO, check the name", nil, 0, false, dns.Question{Name: "no-such-host.example.invalid.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepNoSuchName},
		{"a dotted name, NXDOMAIN, only search .: INFO, check the name", []string{"."}, 0, false, dns.Question{Name: "no-such-host.example.invalid.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepNoSuchName},
		{"a dotted name asked before the search domains (ndots:1): INFO, the search may find it", corp, 1, false, dns.Question{Name: "db.prod.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepNoSuchName + nextStepSearchMayFind},
		{"a dotless name asked before the search domains (Docker's ndots:0): INFO, the hint, the search may find it", corp, 0, true, dns.Question{Name: "ldap.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepDotlessDocker + nextStepSearchMayFind},
		{"a name in a search domain no resolver made (Docker's ndots:0, musl): INFO", corp, 0, true, dns.Question{Name: "payments.corp.example.com.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepNoSuchName + nextStepSearchMayFind},
		{"a dotted name asked after the search domains (ndots:5): INFO, check the name", k8s, 5, false, dns.Question{Name: "api.example.invalid.", Qtype: dns.TypeA}, nx, zapcore.InfoLevel, nextStepNoSuchName},
		{"a dotted name, SERVFAIL: INFO, the resolver failed", corp, 1, false, dns.Question{Name: "flaky.example.com.", Qtype: dns.TypeAAAA}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.InfoLevel, nextStepResolverError},
		{"a search-list candidate, NXDOMAIN: DEBUG", corp, 1, false, dns.Question{Name: "db.Corp.Example.com.", Qtype: dns.TypeA}, nx, zapcore.DebugLevel, ""},
		{"a search-list candidate, SERVFAIL: INFO", corp, 1, false, dns.Question{Name: "db.corp.example.com.", Qtype: dns.TypeA}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.InfoLevel, nextStepResolverError},
		{"a search-list candidate, REFUSED: INFO", corp, 1, false, dns.Question{Name: "db.corp.example.com.", Qtype: dns.TypeA}, negativeEntry(dns.RcodeRefused, upstream), zapcore.InfoLevel, nextStepResolverError},
		{"a reverse lookup, NXDOMAIN: DEBUG", corp, 1, false, dns.Question{Name: "5.0.17.172.in-addr.arpa.", Qtype: dns.TypePTR}, nx, zapcore.DebugLevel, ""},
		{"an IPv6 reverse lookup, NXDOMAIN: DEBUG", corp, 1, false, dns.Question{Name: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa.", Qtype: dns.TypePTR}, nx, zapcore.DebugLevel, ""},
		{"a reverse lookup, SERVFAIL: DEBUG", corp, 1, false, dns.Question{Name: "5.0.17.172.in-addr.arpa.", Qtype: dns.TypePTR}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.DebugLevel, ""},
		{"gRPC's service-config probe, SERVFAIL: DEBUG", corp, 1, false, dns.Question{Name: "_grpc_config.upstream.", Qtype: dns.TypeTXT}, negativeEntry(dns.RcodeServerFailure, upstream), zapcore.DebugLevel, ""},
		{"gRPC's service-config probe, NXDOMAIN: DEBUG", corp, 1, false, dns.Question{Name: "_grpc_config.upstream.", Qtype: dns.TypeTXT}, nx, zapcore.DebugLevel, ""},
		{"the search domain itself is no candidate: INFO", corp, 1, false, dns.Question{Name: "corp.example.com.", Qtype: dns.TypeMX}, nx, zapcore.InfoLevel, nextStepNoSuchName + nextStepSearchMayFind},
		{"a resolved name: none", corp, 1, false, dns.Question{Name: "example.org.", Qtype: dns.TypeA}, dnsCacheEntry{Msg: &dns.Msg{Answer: []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)}}}, FromUpstream: true, upstream: upstream}, zapcore.InvalidLevel, ""},
		{"NODATA (NOERROR, no records): none", corp, 1, false, dns.Question{Name: "example.org.", Qtype: dns.TypeAAAA}, negativeEntry(dns.RcodeSuccess, upstream), zapcore.InvalidLevel, ""},
		{"keploy's own SERVFAIL when no resolver answered: none (logged as an error where it is made)", corp, 1, false, dns.Question{Name: "mongoDb.", Qtype: dns.TypeA}, dnsCacheEntry{Msg: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}, zapcore.InvalidLevel, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			p := &Proxy{logger: zap.New(core), dnsSearch: tc.search, dnsNdots: tc.ndots, IsDocker: tc.docker}
			for i := 0; i < 3; i++ {
				p.noteNegativeAnswer(tc.q, tc.resp)
			}
			all := logs.FilterField(zap.String("query", tc.q.Name)).All()
			if tc.level == zapcore.InvalidLevel {
				if len(all) != 0 {
					t.Fatalf("logged %+v; want no line", all)
				}
				return
			}
			if len(all) != 1 || all[0].Level != tc.level {
				t.Fatalf("three answers logged %+v; want one %s line", all, tc.level)
			}
			m := all[0].ContextMap()
			if m["qtype"] != dns.TypeToString[tc.q.Qtype] || m["answer"] != dns.RcodeToString[tc.resp.Rcode] || m["upstream"] != upstream {
				t.Errorf("the line says qtype %v, answer %v, upstream %v; want %s, %s, %s", m["qtype"], m["answer"], m["upstream"], dns.TypeToString[tc.q.Qtype], dns.RcodeToString[tc.resp.Rcode], upstream)
			}
			if next, _ := m["next_step"].(string); next != tc.next {
				t.Errorf("the line's next step is %q; want %q", next, tc.next)
			}
			if r, _ := m["routine"].(string); (r != "") != (tc.level == zapcore.DebugLevel) {
				t.Errorf("the line's routine reason is %q at %s; a DEBUG line says why it is routine, an INFO line does not", r, tc.level)
			}
		})
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
// ndots:1 asks a dotted name first and the candidates after, so its answer is
// taken from keploy's DNS cache when the expansion shows what it was.
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
			}, nextStepNoSuchName},
		{"a host, ndots:1: the name first", []string{"corp.example.com"}, 1,
			"Payments.corp.example.com.", []string{
				"Payments.corp.example.com.",
				"Payments.corp.example.com.corp.example.com.",
				"Payments.corp.example.com.corp.example.com.", // again, from the cache: learning it again counts nothing
			}, nextStepNoSuchName}, // the search failed too: no "may find it"
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
			if got := logs.FilterMessageSnippet("without an INFO line of their own").All(); len(got) != 0 {
				t.Fatalf("the walk held no line back, yet the stop logged a count: %+v", got)
			}
		})
	}
}

// The same name and type with a different answer is a lookup of its own: a
// name that NXDOMAINs and then SERVFAILs tells the user two things.
func TestRecordLogsEachAnswerToALookupOnce(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	p := &Proxy{logger: zap.New(core)}
	q := dns.Question{Name: "payments.internal.", Qtype: dns.TypeA}
	for _, rc := range []int{dns.RcodeNameError, dns.RcodeServerFailure, dns.RcodeNameError, dns.RcodeServerFailure} {
		p.noteNegativeAnswer(q, negativeEntry(rc, "192.0.2.53:53"))
	}
	var answers []string
	for _, e := range logs.FilterField(zap.String("query", q.Name)).All() {
		answers = append(answers, e.ContextMap()["answer"].(string))
	}
	if len(answers) != 2 || answers[0] != "NXDOMAIN" || answers[1] != "SERVFAIL" {
		t.Fatalf("NXDOMAIN, SERVFAIL, NXDOMAIN, SERVFAIL logged the answers %v; want [NXDOMAIN SERVFAIL]", answers)
	}
}

// The lookups logged are the app's names, so they are bounded: a client
// resolving a fresh name per request must flood neither the log nor the
// agent's memory for the life of the recording. Past the INFO bound one line
// says the rest go to DEBUG, they do, and the count is logged when the
// recording stops: at INFO with a next step when lookups the user has not
// seen came since the last count, at DEBUG when only repeats did, and not at
// all when nothing moved. Past the tracking bound, answers are only counted.
func TestRecordBoundsTheNegativeAnswersItLogsAndCountsTheRest(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{logger: zap.New(core)}
	const past = 50
	entry := negativeEntry(dns.RcodeNameError, "192.0.2.53:53")
	name := func(i int) dns.Question {
		return dns.Question{Name: "req-" + strconv.Itoa(i) + ".tenant.example.", Qtype: dns.TypeA}
	}
	for i := 0; i < negativeAnswerLogMax+past; i++ {
		p.noteNegativeAnswer(name(i), entry)
	}
	const repeats = 3
	for i := 0; i < repeats; i++ {
		p.noteNegativeAnswer(name(0), entry)
	}

	if got := len(negativeAnswerLinesAt(logs, zapcore.InfoLevel)); got != negativeAnswerLogMax {
		t.Fatalf("%d distinct lookups answered NXDOMAIN logged %d INFO lines; want the bound, %d", negativeAnswerLogMax+past, got, negativeAnswerLogMax)
	}
	if got := len(negativeAnswerLinesAt(logs, zapcore.DebugLevel)); got != past {
		t.Fatalf("the %d lookups past the INFO bound logged %d DEBUG lines; want one each", past, got)
	}
	if got := logs.FilterMessageSnippet("it logs the rest at DEBUG").All(); len(got) != 1 || got[0].Level != zapcore.InfoLevel ||
		got[0].ContextMap()["limit"] != int64(negativeAnswerLogMax) || got[0].ContextMap()["next_step"] != nextStepSeeDebug {
		t.Fatalf("the bound reached logged %+v; want one INFO line saying the rest go to DEBUG, with the limit and how to see them", got)
	}

	count := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("without an INFO line of their own").All()
	}
	if len(count()) != 0 {
		t.Fatalf("the count was logged before the recording stopped: %+v", count())
	}
	_ = p.SetGracefulShutdown(context.Background()) // the recording stops
	_ = p.SetGracefulShutdown(context.Background())
	got := count()
	if len(got) != 1 || got[0].Level != zapcore.InfoLevel || got[0].ContextMap()["next_step"] != nextStepSeeDebug {
		t.Fatalf("two stops logged %+v; want one INFO count, with how to see the lookups", got)
	}
	if m := got[0].ContextMap(); m["repeats"] != uint64(repeats) || m["debug_lookups"] != uint64(past) || m["untracked_answers"] != uint64(0) || m["lookups_logged"] != int64(negativeAnswerLogMax) {
		t.Fatalf("the count says %+v; want repeats %d, debug_lookups %d, untracked_answers 0, lookups_logged %d", m, repeats, past, negativeAnswerLogMax)
	}
	p.noteNegativeAnswer(name(1), entry) // only a repeat moves
	_ = p.SetGracefulShutdown(context.Background())
	if got := count(); len(got) != 2 || got[1].Level != zapcore.DebugLevel || got[1].ContextMap()["repeats"] != uint64(repeats+1) {
		t.Fatalf("a count only repeats moved is logged again at DEBUG: want a second line, DEBUG, repeats %d; got %+v", repeats+1, got)
	}
	p.noteNegativeAnswer(name(negativeAnswerLogMax+past), entry) // a lookup past the limit moves
	_ = p.SetGracefulShutdown(context.Background())
	if got := count(); len(got) != 3 || got[2].Level != zapcore.InfoLevel || got[2].ContextMap()["debug_lookups"] != uint64(past+1) {
		t.Fatalf("a new lookup past the limit brings the count back to INFO: want a third line, INFO, debug_lookups %d; got %+v", past+1, got)
	}

	// Past the tracking bound nothing more is held, and the answers are counted.
	for i := 0; i < negativeAnswerTrackMax+past; i++ {
		p.noteNegativeAnswer(name(i), entry)
	}
	if got := len(p.negativeAnswers.named.seen); got != negativeAnswerTrackMax {
		t.Fatalf("the set of lookups logged holds %d after %d distinct names; want its bound, %d", got, negativeAnswerTrackMax+past, negativeAnswerTrackMax)
	}
	if got := len(negativeAnswerLinesAt(logs, zapcore.DebugLevel)); got != negativeAnswerTrackMax-negativeAnswerLogMax {
		t.Fatalf("%d DEBUG lines for lookups past the INFO bound; want one for each the set holds, %d", got, negativeAnswerTrackMax-negativeAnswerLogMax)
	}
	_ = p.SetGracefulShutdown(context.Background())
	if m := count()[3].ContextMap(); m["debug_lookups"] != uint64(negativeAnswerTrackMax-negativeAnswerLogMax) || m["untracked_answers"] != uint64(past) {
		t.Fatalf("the count says %+v; want debug_lookups %d and untracked_answers %d", m, negativeAnswerTrackMax-negativeAnswerLogMax, past)
	}
	for i := 0; i < repeats; i++ { // answers it does not track move the count at DEBUG
		p.noteNegativeAnswer(name(negativeAnswerTrackMax), entry)
	}
	_ = p.SetGracefulShutdown(context.Background())
	if got := count(); len(got) != 5 || got[4].Level != zapcore.DebugLevel || got[4].ContextMap()["untracked_answers"] != uint64(past+repeats) {
		t.Fatalf("answers to an untracked lookup are counted at DEBUG: want a fifth line, DEBUG, untracked_answers %d; got %+v", past+repeats, got)
	}

	// The routine lookups' DEBUG lines, and the names learned as asked, are
	// bounded the same way.
	core, logs = observer.New(zapcore.DebugLevel)
	p = &Proxy{logger: zap.New(core), dnsSearch: []string{"svc.cluster.local"}, dnsNdots: 5}
	for i := 0; i < negativeAnswerTrackMax+past; i++ {
		p.noteNegativeAnswer(dns.Question{Name: "req-" + strconv.Itoa(i) + ".svc.cluster.local.", Qtype: dns.TypeA}, entry)
		p.noteNegativeAnswer(dns.Question{Name: "req-" + strconv.Itoa(i) + ".svc.cluster.local.svc.cluster.local.", Qtype: dns.TypeA}, entry)
	}
	if got := len(p.negativeAnswers.routine.seen); got != negativeAnswerTrackMax || p.negativeAnswers.routine.pastMax == 0 {
		t.Fatalf("the set of routine lookups logged holds %d, and counted %d answers past it; want it full at its bound, %d, and the rest counted", got, p.negativeAnswers.routine.pastMax, negativeAnswerTrackMax)
	}
	if got := len(p.negativeAnswers.asked); got != negativeAnswerTrackMax {
		t.Fatalf("the names learned as asked hold %d after %d; want them full at their bound, %d", got, negativeAnswerTrackMax+past, negativeAnswerTrackMax)
	}
	if got := len(negativeAnswerLinesAt(logs, zapcore.DebugLevel)) + len(negativeAnswerLinesAt(logs, zapcore.InfoLevel)); got > 2*negativeAnswerTrackMax {
		t.Fatalf("%d lines for %d lookups; the two sets bound them to %d", got, 2*(negativeAnswerTrackMax+past), 2*negativeAnswerTrackMax)
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

// Each recording logs its own: a lookup the last recording logged is logged
// again in the next (Record calls ResetRecordedDNSMocks), and neither the last
// one's count nor the names it learned carry over.
func TestEachRecordingLogsItsOwnNegativeAnswers(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{logger: zap.New(core), dnsSearch: []string{"corp.example.com"}}
	q := dns.Question{Name: "mongoDb.", Qtype: dns.TypeA}
	entry := negativeEntry(dns.RcodeNameError, "192.0.2.53:53")
	p.noteNegativeAnswer(q, entry)
	p.noteNegativeAnswer(q, entry)
	p.noteNegativeAnswer(dns.Question{Name: "pay.corp.example.com.corp.example.com.", Qtype: dns.TypeA}, entry)

	p.ResetRecordedDNSMocks()

	p.noteNegativeAnswer(q, entry)
	if got := negativeAnswerLines(logs, zapcore.InfoLevel, q.Name); len(got) != 2 {
		t.Fatalf("the same lookup in two recordings logged %d lines; want one in each", len(got))
	}
	if p.negativeAnswers.wasAsked("pay.corp.example.com.") {
		t.Fatal("a name the last recording learned the app asked as given carried over")
	}
	_ = p.SetGracefulShutdown(context.Background())
	if got := logs.FilterMessageSnippet("without an INFO line of their own").All(); len(got) != 0 {
		t.Fatalf("the second recording held nothing back, yet logged a count: %+v (the first recording's repeat carried over)", got)
	}
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

// Under Docker's ndots:0 a resolver asks every name as given first. glibc then
// tries the search domains, whose candidates come after the name's own
// NXDOMAIN, which keploy's cache still holds: those are routine. musl tries
// none, so a name inside a search domain the app asks with no failed name
// before it is the app's own, and gets its INFO line.
func TestRecordUnderDockersNdots0TellsCandidatesFromTheAppsNames(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots, p.IsDocker = []string{"corp.example.com"}, 0, true

	askDNS(t, p, "nope.", dns.TypeA)                      // glibc: the name as given
	askDNS(t, p, "nope.corp.example.com.", dns.TypeA)     // then its candidate
	askDNS(t, p, "payments.corp.example.com.", dns.TypeA) // musl: the app's own, alone

	var info []string
	for _, e := range negativeAnswerLinesAt(logs, zapcore.InfoLevel) {
		info = append(info, e.ContextMap()["query"].(string))
	}
	if len(info) != 2 || info[0] != "nope." || info[1] != "payments.corp.example.com." {
		t.Fatalf("logged INFO lines for %v; want [nope. payments.corp.example.com.]", info)
	}
	if got := negativeAnswerLines(logs, zapcore.DebugLevel, "nope.corp.example.com."); len(got) != 1 {
		t.Fatalf("the candidate of a name that failed just before logged %d DEBUG lines; want 1", len(got))
	}
}

// The ServeDNS goroutines note answers concurrently: the INFO bound and the
// one line that says it was reached hold however they interleave.
func TestRecordBoundsItsLinesUnderConcurrentLookups(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p := &Proxy{logger: zap.New(core)}
	entry := negativeEntry(dns.RcodeNameError, "192.0.2.53:53")
	const lookups = 2 * negativeAnswerLogMax
	var wg sync.WaitGroup
	for i := 0; i < lookups; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := dns.Question{Name: "req-" + strconv.Itoa(i) + ".tenant.example.", Qtype: dns.TypeA}
			p.noteNegativeAnswer(q, entry)
			p.noteNegativeAnswer(q, entry)
		}()
	}
	wg.Wait()
	if got := len(negativeAnswerLinesAt(logs, zapcore.InfoLevel)); got != negativeAnswerLogMax {
		t.Fatalf("%d concurrent lookups logged %d INFO lines; want %d", lookups, got, negativeAnswerLogMax)
	}
	if got := len(negativeAnswerLinesAt(logs, zapcore.DebugLevel)); got != lookups-negativeAnswerLogMax {
		t.Fatalf("%d concurrent lookups logged %d DEBUG lines; want %d", lookups, got, lookups-negativeAnswerLogMax)
	}
	if got := logs.FilterMessageSnippet("it logs the rest at DEBUG").Len(); got != 1 {
		t.Fatalf("the bound reached logged %d lines saying so; want 1", got)
	}
	if got := p.negativeAnswers.named.repeats; got != lookups {
		t.Fatalf("counted %d repeats; want %d", got, lookups)
	}
}

// In a pod (ndots:5) a resolver walks the search domains before the name as
// given: each later candidate follows the earlier ones' NXDOMAINs, still in
// keploy's cache, and is routine; the name as given comes last and is the
// lookup's outcome. musl asks a name whose dots reach ndots as given only, so
// a StatefulSet member's name in another namespace, which lies in the search
// domain svc.cluster.local, is the app's own: no earlier candidate came.
func TestRecordInAPodTellsTheResolversWalkFromTheAppsName(t *testing.T) {
	k8s := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	for _, tc := range []struct {
		name    string
		queries []string
		info    string // the one query logged at INFO
	}{
		{"glibc, an external name: the candidates, then the name", []string{
			"api.example.invalid.default.svc.cluster.local.",
			"api.example.invalid.svc.cluster.local.",
			"api.example.invalid.cluster.local.",
			"api.example.invalid.",
		}, "api.example.invalid."},
		{"musl, a StatefulSet member in another namespace: the name alone", []string{
			"mongo-0.mongo.prod.svc.cluster.local.",
		}, "mongo-0.mongo.prod.svc.cluster.local."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
			defer stop()
			p, logs := recordingDNSProxy(t, host, port)
			p.dnsSearch, p.dnsNdots = k8s, 5
			for _, q := range tc.queries {
				askDNS(t, p, q, dns.TypeA)
			}
			info := negativeAnswerLinesAt(logs, zapcore.InfoLevel)
			if len(info) != 1 || info[0].ContextMap()["query"] != tc.info {
				t.Fatalf("the queries %v logged %+v at INFO; want one line, for %s", tc.queries, info, tc.info)
			}
			if got := len(negativeAnswerLinesAt(logs, zapcore.DebugLevel)); got != len(tc.queries)-1 {
				t.Fatalf("%d DEBUG lines; want one for each candidate, %d", got, len(tc.queries)-1)
			}
		})
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

// A candidate keeps its class for the recording. Its repeats come from
// keploy's DNS cache, and the earlier candidate's answer, the evidence that
// made it routine, may expire from the cache between two steps of a later
// walk: that must not turn a healthy run's candidate into an INFO line.
func TestRecordKeepsACandidateRoutineWhenItsEvidenceExpires(t *testing.T) {
	host, port, stop := splitUpstream(t, "api.example.com.")
	defer stop()
	p, logs := recordingDNSProxy(t, host, port)
	p.dnsSearch, p.dnsNdots = []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}, 5
	walk := []string{
		"api.example.com.default.svc.cluster.local.",
		"api.example.com.svc.cluster.local.",
		"api.example.com.cluster.local.",
		"api.example.com.",
	}
	for _, q := range walk {
		askDNS(t, p, q, dns.TypeA)
	}
	// The next request's walk, at the moment the first candidate's cached
	// answer expires: the second is still cached, its evidence is gone.
	p.dnsCache.Remove(generateCacheKey(walk[0], dns.TypeA))
	askDNS(t, p, walk[1], dns.TypeA)

	if got := negativeAnswerLinesAt(logs, zapcore.InfoLevel); len(got) != 0 {
		t.Fatalf("a healthy pod's walk logged %+v at INFO", got)
	}
	if got := p.negativeAnswers.routine.repeats; got != 1 {
		t.Fatalf("the candidate's answer from the cache counted %d routine repeats; want 1", got)
	}
}
