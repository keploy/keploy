package proxy

import (
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The resolver's negative answers are answers. A name that does not exist
// (NXDOMAIN) or has no record of the asked type (NODATA: NOERROR with no
// answer) is what the app saw while recording, with the SOA whose TTL tells
// its resolver how long to keep that verdict. A replay is isolated from the
// real network, so an answer the recording does not hold is a mock miss, and
// the app is steered at the proxy instead of getting the answer it got live.
//
// The case that showed it: an app whose /etc/hosts gives its host name an IPv4
// address only asks the resolver for AAAA of that name at each request. Each
// answer is NXDOMAIN, and none was recorded, so every one of them was a miss
// at replay, inside tests too.

// The SOA a recursive resolver returns with a negative answer for a name under
// a TLD that does not exist (.test), its TTL as the resolver counted it down.
const negativeRootSOA = ". 86367 IN SOA a.root-servers.net. nstld.verisign-grs.com. 2026110300 1800 900 604800 86400"

// The SOA of a zone that holds the name but no record of the asked type.
const negativeZoneSOA = "example.com. 3600 IN SOA ns.icann.org. noc.dns.icann.org. 2026110301 7200 3600 1209600 3600"

// negativeUpstream starts a resolver that answers every question with rcode,
// no answer, and ns as its authority section, standing in for the real
// resolver while keploy records.
func negativeUpstream(t *testing.T, rcode int, ns ...string) (host, port string) {
	t.Helper()
	var authority []dns.RR
	for _, s := range ns {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		authority = append(authority, rr)
	}
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		m.RecursionAvailable = true
		// A sentinel question (below) is answered: it orders the mock stream.
		if len(r.Question) == 1 && r.Question[0].Name == sentinelName {
			m.SetRcode(r, dns.RcodeSuccess)
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: sentinelName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("10.0.0.9"),
			}}
		} else {
			m.Ns = authority
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return h, p
}

const sentinelName = "sentinel.keploy.test."

// recordQuestions records qs through keploy's DNS server against the resolver
// at host:port, then records the sentinel, and returns every DNS mock the
// recording emitted before the sentinel's, with the answers the app got.
//
// A DNS mock reaches the session's channel before recordDNSMock returns while
// the channel has room (SyncMockManager.sendFor's direct send), so the drain
// below reads all of them without a wait. The sentinel, a NOERROR answer,
// holds that: if its mock is not there, the stream did not deliver in order
// and the row fails rather than read "no mock" from a stream it did not see.
func recordQuestions(t *testing.T, host, port string, qs ...dns.Question) ([]*models.Mock, []dnsCacheEntry) {
	t.Helper()
	mc := make(chan *models.Mock, 64)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	p := &Proxy{
		logger:             zap.NewNop(),
		dnsUpstreamServers: []string{host},
		dnsUpstreamPort:    port,
		recordedDNSMocks:   newRecordedDNSMocksCache(),
	}
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc}

	var answers []dnsCacheEntry
	for _, q := range qs {
		answers = append(answers, p.resolveUncachedDNSResponse(q, models.MODE_RECORD, true, time.Now(), session))
	}
	p.resolveUncachedDNSResponse(dns.Question{Name: sentinelName, Qtype: dns.TypeA, Qclass: dns.ClassINET},
		models.MODE_RECORD, true, time.Now(), session)

	var mocks []*models.Mock
	for {
		select {
		case m := <-mc:
			if m.Kind == models.DNS && m.Spec.DNSReq != nil && m.Spec.DNSReq.Name == sentinelName {
				return mocks, answers
			}
			mocks = append(mocks, m)
		default:
			t.Fatalf("the sentinel's NOERROR answer was not recorded; %d mock(s) came before the stream ended", len(mocks))
			return nil, nil
		}
	}
}

// replayIsolated answers q as an isolated replay does, from mocks alone: it
// has no resolver to forward a miss to. It returns the answer and the miss
// reports the question raised.
func replayIsolated(t *testing.T, mocks []*models.Mock, q dns.Question) (dnsCacheEntry, []models.MockMismatchReport) {
	t.Helper()
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetFilteredMocks(mocks)
	p := &Proxy{
		logger:      zap.NewNop(),
		errChannel:  make(chan error, 4),
		IP4:         "127.0.0.1",
		IP6:         "::1",
		mockManager: mgr,
	}
	got := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
	return got, collectMockMissReports(t, p)
}

func rrStrings(rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		out = append(out, rr.String())
	}
	return out
}

// Each negative answer the resolver gives is recorded once, with its rcode and
// its authority section as given, and an isolated replay serves it back: the
// same rcode, the same SOA with the same TTL, no answer, and no miss.
func TestANegativeAnswerIsRecordedAndServedAtReplay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rcode int
		ns    []string
		q     dns.Question
	}{
		{
			// The Frappe shape: crm.test is in /etc/hosts as IPv4 only.
			name:  "NXDOMAIN with the root SOA, AAAA of a v4-only host",
			rcode: dns.RcodeNameError,
			ns:    []string{negativeRootSOA},
			q:     dns.Question{Name: "crm.test.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
		},
		{
			name:  "NXDOMAIN with no authority section",
			rcode: dns.RcodeNameError,
			q:     dns.Question{Name: "nosuch.internal.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		},
		{
			name:  "NODATA with the zone's SOA",
			rcode: dns.RcodeSuccess,
			ns:    []string{negativeZoneSOA},
			q:     dns.Question{Name: "example.com.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port := negativeUpstream(t, tc.rcode, tc.ns...)
			// Asked twice, as an app asks at each request: one mock.
			mocks, answers := recordQuestions(t, host, port, tc.q, tc.q)

			for i, a := range answers {
				if a.Msg == nil || a.Rcode != tc.rcode {
					t.Fatalf("record answer %d to the app = %+v, want the resolver's rcode %s", i, a.Msg, dns.RcodeToString[tc.rcode])
				}
			}
			if len(mocks) != 1 {
				t.Fatalf("recorded %d DNS mocks for %s %s, want 1: %+v", len(mocks), dns.TypeToString[tc.q.Qtype], tc.q.Name, mocks)
			}
			m := mocks[0]
			if m.Kind != models.DNS || m.Spec.DNSReq == nil || m.Spec.DNSResp == nil {
				t.Fatalf("recorded mock is not a DNS mock: %+v", m)
			}
			if !dnsRequestMatches(m.Spec.DNSReq, tc.q) {
				t.Fatalf("recorded question %+v, want %+v", *m.Spec.DNSReq, tc.q)
			}
			if got := m.Spec.DNSResp.Rcode; got != tc.rcode {
				t.Fatalf("recorded rcode %s, want %s", dns.RcodeToString[got], dns.RcodeToString[tc.rcode])
			}
			if len(m.Spec.DNSResp.Answers) != 0 {
				t.Fatalf("recorded answers %q for a negative answer", m.Spec.DNSResp.Answers)
			}
			if !equalStrings(m.Spec.DNSResp.Ns, rrStringsOf(t, tc.ns)) {
				t.Fatalf("recorded authority %q, want the resolver's %q", m.Spec.DNSResp.Ns, tc.ns)
			}

			got, misses := replayIsolated(t, mocks, tc.q)
			if len(misses) != 0 {
				t.Fatalf("replay reported %d miss(es) for a recorded answer: %+v", len(misses), misses)
			}
			if got.Msg == nil || got.Rcode != tc.rcode {
				t.Fatalf("replay answered %+v, want the recorded rcode %s", got.Msg, dns.RcodeToString[tc.rcode])
			}
			if len(got.Answer) != 0 {
				t.Fatalf("replay answered %q, want no answer as recorded", rrStrings(got.Answer))
			}
			if !equalStrings(rrStrings(got.Ns), rrStringsOf(t, tc.ns)) {
				t.Fatalf("replay authority %q, want the recorded %q", rrStrings(got.Ns), tc.ns)
			}
		})
	}
}

// A resolver that could not or would not answer says nothing about the name:
// SERVFAIL, REFUSED, NOTIMP and FORMERR are not recorded, so a replay never
// serves a failure that was the resolver's at one moment.
func TestAResolverFailureIsNotRecorded(t *testing.T) {
	for _, rcode := range []int{dns.RcodeServerFailure, dns.RcodeRefused, dns.RcodeNotImplemented, dns.RcodeFormatError} {
		t.Run(dns.RcodeToString[rcode], func(t *testing.T) {
			host, port := negativeUpstream(t, rcode)
			q := dns.Question{Name: "flaky.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			mocks, answers := recordQuestions(t, host, port, q)
			if a := answers[0]; a.Msg == nil || a.Rcode != rcode {
				t.Fatalf("record answer to the app = %+v, want the resolver's %s", a.Msg, dns.RcodeToString[rcode])
			}
			if len(mocks) != 0 {
				t.Fatalf("recorded %d mock(s) for a %s, want none: %+v", len(mocks), dns.RcodeToString[rcode], mocks[0].Spec.DNSResp)
			}
		})
	}
}

// rrStringsOf is ns as keploy writes a record: miekg/dns's presentation form.
func rrStringsOf(t *testing.T, ns []string) []string {
	t.Helper()
	var out []string
	for _, s := range ns {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		out = append(out, rr.String())
	}
	return out
}

// answerA is a resolved answer to q: one A record.
func answerA(q dns.Question, ip string) []string {
	return []string{(&dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP(ip),
	}).String()}
}

// sequenceUpstream answers the n-th question that is not the sentinel with
// steps[n] (the last step from then on), standing in for a resolver whose
// answer for a name changes during the recording.
type answerStep struct {
	rcode   int
	answers []string
	ns      []string
}

func sequenceUpstream(t *testing.T, steps ...answerStep) (host, port string) {
	t.Helper()
	parse := func(ss []string) []dns.RR {
		var out []dns.RR
		for _, s := range ss {
			rr, err := dns.NewRR(s)
			if err != nil {
				t.Errorf("parse %q: %v", s, err)
				continue
			}
			out = append(out, rr)
		}
		return out
	}
	var n atomic.Int32 // the handler runs on the server's goroutines
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		switch {
		case len(r.Question) == 1 && r.Question[0].Name == sentinelName:
			m.SetReply(r)
			m.Answer = parse(answerA(r.Question[0], "10.0.0.9"))
		case len(r.Question) == 1 && r.Question[0].Name == "probe.test.":
			m.SetReply(r) // startFakeUpstream's readiness probe
		default:
			st := steps[min(int(n.Add(1))-1, len(steps)-1)]
			m.SetRcode(r, st.rcode)
			m.Answer = parse(st.answers)
			m.Ns = parse(st.ns)
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return h, p
}

// A name's answer can change while recording: a service not up yet does not
// resolve, then does. Its negative answer and its resolved one are each
// recorded once, and the replay serves the resolved one in either order, as
// it did when keploy recorded resolved answers only. A NODATA answer first
// no longer keeps the later resolved one out of the recording.
func TestANameThatResolvesLaterKeepsItsResolvedAnswer(t *testing.T) {
	q := dns.Question{Name: "db.compose.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resolved := answerStep{rcode: dns.RcodeSuccess, answers: answerA(q, "172.18.0.5")}
	nxdomain := answerStep{rcode: dns.RcodeNameError, ns: []string{negativeRootSOA}}
	nodata := answerStep{rcode: dns.RcodeSuccess, ns: []string{negativeZoneSOA}}
	// A CNAME to a name that does not exist: NXDOMAIN with a record.
	dangling := answerStep{rcode: dns.RcodeNameError, answers: []string{"db.compose. 60 IN CNAME db.gone.compose."}, ns: []string{negativeRootSOA}}
	for _, tc := range []struct {
		name  string
		steps []answerStep
	}{
		{"NXDOMAIN, then resolved", []answerStep{nxdomain, nxdomain, resolved, resolved}},
		{"NODATA, then resolved", []answerStep{nodata, resolved, resolved}},
		{"resolved, then NXDOMAIN", []answerStep{resolved, nxdomain, nxdomain}},
		{"NXDOMAIN through a dangling CNAME, then resolved", []answerStep{dangling, resolved}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port := sequenceUpstream(t, tc.steps...)
			qs := make([]dns.Question, len(tc.steps))
			for i := range qs {
				qs[i] = q
			}
			mocks, _ := recordQuestions(t, host, port, qs...)
			if len(mocks) != 2 {
				t.Fatalf("recorded %d DNS mocks, want 2 (the negative answer and the resolved one): %+v", len(mocks), mocks)
			}
			for _, order := range [][]*models.Mock{mocks, {mocks[1], mocks[0]}} {
				got, misses := replayIsolated(t, order, q)
				if len(misses) != 0 {
					t.Fatalf("replay reported %d miss(es): %+v", len(misses), misses)
				}
				if dnsAnswerRank(got.Msg) != dnsRankResolved || !equalStrings(rrStrings(got.Answer), resolved.answers) {
					t.Fatalf("replay answered %+v, want the resolved answer %q", got.Msg, resolved.answers)
				}
			}
		})
	}
}

// dnsMockOf is a recorded DNS mock of q with the given answer.
func dnsMockOf(t *testing.T, q dns.Question, rcode int, answers []string, ns ...string) *models.Mock {
	t.Helper()
	return &models.Mock{Kind: models.DNS, Spec: models.MockSpec{
		DNSReq:  &models.DNSReq{Name: q.Name, Qtype: q.Qtype, Qclass: q.Qclass},
		DNSResp: &models.DNSResp{Rcode: rcode, Answers: answers, Ns: rrStringsOf(t, ns)},
	}}
}

// With no resolved answer for a question, NODATA is served over NXDOMAIN in
// either order: NODATA says the name exists, which it did once it answered
// so. Of two answers of one kind, the first recorded is served.
func TestReplayServesNODATAOverNXDOMAINInEitherOrder(t *testing.T) {
	q := dns.Question{Name: "gone.example.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}
	nx := dnsMockOf(t, q, dns.RcodeNameError, nil, negativeRootSOA)
	nodata := dnsMockOf(t, q, dns.RcodeSuccess, nil, negativeZoneSOA)
	for _, order := range [][]*models.Mock{{nx, nodata}, {nodata, nx}} {
		got, misses := replayIsolated(t, order, q)
		if len(misses) != 0 || got.Msg == nil || dnsAnswerRank(got.Msg) != dnsRankNODATA ||
			!equalStrings(rrStrings(got.Ns), rrStringsOf(t, []string{negativeZoneSOA})) {
			t.Fatalf("replay answered %+v (misses %+v), want the recorded NODATA with its SOA", got.Msg, misses)
		}
	}

	otherSOA := "example.org. 3600 IN SOA ns.example.org. host.example.org. 1 7200 3600 1209600 3600"
	nx2 := dnsMockOf(t, q, dns.RcodeNameError, nil, otherSOA)
	if got, _ := replayIsolated(t, []*models.Mock{nx, nx2}, q); got.Msg == nil ||
		!equalStrings(rrStrings(got.Ns), rrStringsOf(t, []string{negativeRootSOA})) {
		t.Fatalf("replay answered %+v, want the first recorded NXDOMAIN", got.Msg)
	}

	// A question of another type for the name has no recorded answer.
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetFilteredMocks([]*models.Mock{nx, nodata})
	p := &Proxy{logger: zap.NewNop(), mockManager: mgr}
	other := dns.Question{Name: q.Name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
	if got, ok := p.getMockedDNSResponse(other); ok {
		t.Fatalf("served %+v for %s %s, which holds no recorded answer", got.Msg, dns.TypeToString[other.Qtype], other.Name)
	}
}

// A v4-only service not up yet when the app first asks for it: its A question
// answers NXDOMAIN, then resolves; its AAAA question answers NXDOMAIN, then
// NODATA (docker's resolver and CoreDNS answer NOERROR with no record for a
// name with no IPv6 address). NODATA and NXDOMAIN are each recorded, and the
// replay serves A resolved and AAAA NODATA, what the app got once the service
// was up: a stale AAAA NXDOMAIN fails musl's getaddrinfo for the name even
// though its A question resolves.
func TestAnAAAAThatBecomesNODATAIsServedAsNODATA(t *testing.T) {
	const name = "db.compose."
	qA := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
	qAAAA := dns.Question{Name: name, Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}
	var nA, nAAAA atomic.Int32 // the handler runs on the server's goroutines
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		switch q := r.Question[0]; {
		case q.Name == sentinelName:
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: sentinelName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("10.0.0.9")}}
		case q.Name != name: // startFakeUpstream's readiness probe
		case q.Qtype == dns.TypeA && nA.Add(1) <= 2, q.Qtype == dns.TypeAAAA && nAAAA.Add(1) <= 2:
			m.SetRcode(r, dns.RcodeNameError)
		case q.Qtype == dns.TypeA:
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 600}, A: net.ParseIP("172.18.0.5")}}
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var qs []dns.Question
	for i := 0; i < 4; i++ { // the app retries until the service is up
		qs = append(qs, qA, qAAAA)
	}
	mocks, _ := recordQuestions(t, host, port, qs...)
	if len(mocks) != 4 {
		t.Fatalf("recorded %d DNS mocks, want 4 (A NXDOMAIN and resolved, AAAA NXDOMAIN and NODATA): %+v", len(mocks), mocks)
	}
	gotA, missesA := replayIsolated(t, mocks, qA)
	gotAAAA, missesAAAA := replayIsolated(t, mocks, qAAAA)
	if len(missesA)+len(missesAAAA) != 0 {
		t.Fatalf("replay reported misses: %+v %+v", missesA, missesAAAA)
	}
	if dnsAnswerRank(gotA.Msg) != dnsRankResolved {
		t.Fatalf("replay A = %+v, want the resolved answer", gotA.Msg)
	}
	if dnsAnswerRank(gotAAAA.Msg) != dnsRankNODATA {
		t.Fatalf("replay AAAA = %s with %d answers, want NODATA, what the app got once the name existed",
			dns.RcodeToString[gotAAAA.Rcode], len(gotAAAA.Answer))
	}
}

// A resolved answer among the unfiltered mocks is served over a negative one
// among the filtered mocks, and of two resolved answers, the filtered one.
func TestReplayPrefersAResolvedAnswerAcrossMockTiers(t *testing.T) {
	q := dns.Question{Name: "api.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	negative := dnsMockOf(t, q, dns.RcodeNameError, nil)
	resolved := dnsMockOf(t, q, dns.RcodeSuccess, answerA(q, "10.1.2.3"))
	filteredResolved := dnsMockOf(t, q, dns.RcodeSuccess, answerA(q, "10.9.9.9"))
	for _, tc := range []struct {
		name               string
		filtered, unfilter []*models.Mock
		want               []string
	}{
		{"negative filtered, resolved unfiltered", []*models.Mock{negative}, []*models.Mock{resolved}, resolved.Spec.DNSResp.Answers},
		{"resolved in both tiers", []*models.Mock{filteredResolved}, []*models.Mock{resolved}, filteredResolved.Spec.DNSResp.Answers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mgr.Close)
			mgr.SetFilteredMocks(tc.filtered)
			mgr.SetUnFilteredMocks(tc.unfilter)
			p := &Proxy{logger: zap.NewNop(), mockManager: mgr}
			got, ok := p.getMockedDNSResponse(q)
			if !ok || !equalStrings(rrStrings(got.Answer), tc.want) {
				t.Fatalf("served %+v, want %q", got.Msg, tc.want)
			}
		})
	}
}

// A recorded negative answer says what a name was while recording. A replay
// asks its own network's resolver first, for a name that network owns and a
// type it answers itself, and serves that answer when it is the better kind:
// a container that was not up while recording resolves at replay, as it did
// before keploy recorded negative answers. Every other question gets the
// recorded answer and reaches no resolver, test.allowEgress "dns" or not: it
// opens real egress for a question no mock answers, and these have one.
func TestARecordedNegativeAnswerDoesNotShadowTheReplaysNetwork(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		q := r.Question[0]
		mu.Lock()
		asked[dns.TypeToString[q.Qtype]+" "+q.Name]++
		mu.Unlock()
		m := new(dns.Msg)
		m.SetReply(r)
		switch {
		case q.Name == "riso-peer." && q.Qtype == dns.TypeA, q.Name == "db." && q.Qtype == dns.TypeA,
			q.Name == "late." && q.Qtype == dns.TypeA:
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 600}, A: net.ParseIP("172.19.0.2")}}
		case q.Name == "riso-peer." && q.Qtype == dns.TypeAAAA: // NODATA: no IPv6 address
		case q.Name == "flaky.":
			m.SetRcode(r, dns.RcodeServerFailure)
		case q.Name != "probe.test.": // startFakeUpstream's readiness probe
			m.SetRcode(r, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	p := hostResolverProxy(t, addr)
	p.IsDocker = true
	p.dnsClusterDomain = "" // a container network: docker's resolver
	p.dnsInNetworkServers = append([]string(nil), p.dnsUpstreamServers...)
	p.netNames = netnames.New()
	p.netNames.Apply(netnames.List{Networks: []string{"keploy-network"}, Names: []string{"riso-peer", "db", "gone", "flaky", "late"}})

	question := func(name string, qtype uint16) dns.Question {
		return dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET}
	}
	peerA, peerAAAA, peerTXT := question("riso-peer.", dns.TypeA), question("riso-peer.", dns.TypeAAAA), question("riso-peer.", dns.TypeTXT)
	dbA, goneAAAA, flakyA := question("db.", dns.TypeA), question("gone.", dns.TypeAAAA), question("flaky.", dns.TypeA)
	lateA := question("late.", dns.TypeA)
	crmAAAA := question("crm.test.", dns.TypeAAAA)
	p.mockManager.SetFilteredMocks([]*models.Mock{
		dnsMockOf(t, peerA, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, peerAAAA, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, peerTXT, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, dbA, dns.RcodeSuccess, answerA(dbA, "10.0.0.7")),
		dnsMockOf(t, goneAAAA, dns.RcodeSuccess, nil, negativeZoneSOA),
		dnsMockOf(t, flakyA, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, crmAAAA, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, lateA, dns.RcodeSuccess, nil, negativeZoneSOA),
	})

	for _, tc := range []struct {
		name     string
		q        dns.Question
		asked    int
		wantRank int
		answer   string // the served A record's address, when it has one
		live     bool   // the in-network resolver's answer, not the recording's
	}{
		{"an owned name that resolves now", peerA, 1, dnsRankResolved, "172.19.0.2", true},
		{"an owned name that answers NODATA now", peerAAAA, 1, dnsRankNODATA, "", true},
		{"recorded NODATA, an owned name that resolves now", lateA, 1, dnsRankResolved, "172.19.0.2", true},
		{"a type docker's resolver recurses", peerTXT, 0, dnsRankNXDOMAIN, "", false},
		{"a recorded resolved answer", dbA, 0, dnsRankResolved, "10.0.0.7", false},
		{"recorded NODATA over the network's NXDOMAIN", goneAAAA, 1, dnsRankNODATA, "", false},
		{"recorded NXDOMAIN over the network's SERVFAIL", flakyA, 1, dnsRankNXDOMAIN, "", false},
		{"a name the network does not own", crmAAAA, 0, dnsRankNXDOMAIN, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, egress := range []bool{false, true} {
				p.allowEgressDNS = egress
				key := dns.TypeToString[tc.q.Qtype] + " " + tc.q.Name
				mu.Lock()
				before := asked[key]
				mu.Unlock()
				got := p.resolveUncachedDNSResponse(tc.q, models.MODE_TEST, true, time.Now(), nil)
				mu.Lock()
				n := asked[key] - before
				mu.Unlock()
				if n != tc.asked {
					t.Errorf("allowEgress dns %v: %s was put to a resolver %d time(s), want %d", egress, key, n, tc.asked)
				}
				if got.Msg == nil || dnsAnswerRank(got.Msg) != tc.wantRank || !got.FromUpstream {
					t.Fatalf("allowEgress dns %v: served %+v, want kind %d", egress, got.Msg, tc.wantRank)
				}
				if tc.answer != "" {
					if len(got.Answer) != 1 {
						t.Fatalf("allowEgress dns %v: served %v, want the one A record %s", egress, got.Answer, tc.answer)
					}
					if a, ok := got.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP(tc.answer)) {
						t.Errorf("allowEgress dns %v: served %v, want %s", egress, got.Answer, tc.answer)
					}
				}
				recordedSOA := len(got.Ns) == 1 && got.Ns[0].Header().Rrtype == dns.TypeSOA
				if tc.wantRank != dnsRankResolved && recordedSOA == tc.live {
					t.Errorf("allowEgress dns %v: served %+v; want the %s answer", egress, got.Msg, map[bool]string{true: "network's", false: "recorded"}[tc.live])
				}
			}
		})
	}
	if misses := collectMockMissReports(t, p); len(misses) != 0 {
		t.Fatalf("a question a mock answers was reported as a miss: %+v", misses)
	}
}

// In a cluster, a service that did not exist while recording (an NXDOMAIN
// recorded under the cluster domain) resolves at replay through cluster DNS,
// as it did before keploy recorded negative answers. An external name keeps
// its recorded NXDOMAIN and reaches no resolver.
func TestAClusterServiceNotUpWhileRecordingResolvesAtReplay(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		q := r.Question[0]
		mu.Lock()
		asked[q.Name]++
		mu.Unlock()
		m := new(dns.Msg)
		m.SetReply(r)
		switch q.Name {
		case "payments.shop.svc.cluster.local.":
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5}, A: net.ParseIP("10.96.0.42")}}
		case "probe.test.": // startFakeUpstream's readiness probe
		default:
			m.SetRcode(r, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	p := hostResolverProxy(t, addr)
	p.dnsClusterDomain = "cluster.local"
	p.dnsInNetworkServers = append([]string(nil), p.dnsUpstreamServers...)
	svc := dns.Question{Name: "payments.shop.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	ext := dns.Question{Name: "crm.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	p.mockManager.SetFilteredMocks([]*models.Mock{
		dnsMockOf(t, svc, dns.RcodeNameError, nil, negativeRootSOA),
		dnsMockOf(t, ext, dns.RcodeNameError, nil, negativeRootSOA),
	})
	got := p.resolveUncachedDNSResponse(svc, models.MODE_TEST, true, time.Now(), nil)
	if got.Msg == nil || got.Rcode != dns.RcodeSuccess || len(got.Answer) != 1 {
		t.Fatalf("cluster service: served %+v, want cluster DNS's A 10.96.0.42", got.Msg)
	}
	got = p.resolveUncachedDNSResponse(ext, models.MODE_TEST, true, time.Now(), nil)
	mu.Lock()
	n := asked["crm.test."]
	mu.Unlock()
	if got.Msg == nil || got.Rcode != dns.RcodeNameError || n != 0 {
		t.Fatalf("external name: served %+v, asked %d; want the recorded NXDOMAIN and no resolver asked", got.Msg, n)
	}
}

// prefixUpstream answers a name of two labels by its prefix: "ok" resolves
// for A and is NODATA for another type, "nd" is NODATA, and every other name,
// a search-list expansion included, is NXDOMAIN.
func prefixUpstream(t *testing.T) (host, port string) {
	t.Helper()
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := r.Question[0]
		switch {
		case dns.CountLabel(q.Name) != 2:
			m.SetRcode(r, dns.RcodeNameError)
		case strings.HasPrefix(q.Name, "ok"):
			if q.Qtype == dns.TypeA {
				m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("10.0.0.1")}}
			}
		case strings.HasPrefix(q.Name, "nd"), q.Name == "probe.test.":
		default:
			m.SetRcode(r, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// recordEach records the answer to each question in turn, as a recording whose
// dnsCache has let each go (its 30 s TTL), and counts the mocks recorded of
// each kind (dnsMockRank).
func recordEach(t *testing.T, qs []dns.Question) [3]int {
	t.Helper()
	host, port := prefixUpstream(t)
	mc := make(chan *models.Mock, 2*len(qs))
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	p := &Proxy{logger: zap.NewNop(), dnsUpstreamServers: []string{host}, dnsUpstreamPort: port, recordedDNSMocks: newRecordedDNSMocksCache()}
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc}
	for _, q := range qs {
		p.resolveUncachedDNSResponse(q, models.MODE_RECORD, true, time.Now(), session)
	}
	var kinds [3]int
	for {
		select {
		case m := <-mc:
			if m.Kind == models.DNS && m.Spec.DNSReq != nil {
				kinds[dnsMockRank(m.Spec.DNSResp)]++
			}
		default:
			return kinds
		}
	}
}

// The names an app asks that do not exist never evict a resolved answer from
// the dedupe tracker: 600 names that resolve, 600 that do not and 600 with no
// record of the type, then the 600 that resolve asked again, record each
// answer once.
func TestNegativeAnswersNeverEvictAResolvedAnswerFromTheDedupe(t *testing.T) {
	var qs []dns.Question
	for _, prefix := range []string{"ok", "nx", "nd", "ok"} {
		for i := 0; i < 600; i++ {
			qs = append(qs, dns.Question{Name: fmt.Sprintf("%s-%d.example.", prefix, i), Qtype: dns.TypeA, Qclass: dns.ClassINET})
		}
	}
	got := recordEach(t, qs)
	if got != [3]int{dnsRankNXDOMAIN: 600, dnsRankNODATA: 600, dnsRankResolved: 600} {
		t.Fatalf("recorded NXDOMAIN %d, NODATA %d, resolved %d; want 600 of each",
			got[dnsRankNXDOMAIN], got[dnsRankNODATA], got[dnsRankResolved])
	}
}

// Under k8s ndots:5 a pod asks an external name under each of its four search
// domains before the name itself, for A and AAAA: 8 NXDOMAINs, a resolved A
// and an AAAA with no record (NODATA). The dedupe holds all ten answers of 450
// such names, so a recording that asks each of them twice records each answer
// once.
func TestTheDedupeHoldsTheSearchListAnswersOf450K8sNames(t *testing.T) {
	search := []string{"shop.svc.cluster.local.", "svc.cluster.local.", "cluster.local.", "internal.example."}
	var once []dns.Question
	for i := 0; i < 450; i++ {
		name := fmt.Sprintf("ok-%d.example.", i)
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			for _, domain := range search {
				once = append(once, dns.Question{Name: name + domain, Qtype: qtype, Qclass: dns.ClassINET})
			}
			once = append(once, dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET})
		}
	}
	got := recordEach(t, append(once, once...))
	if got != [3]int{dnsRankNXDOMAIN: 8 * 450, dnsRankNODATA: 450, dnsRankResolved: 450} {
		t.Fatalf("recorded NXDOMAIN %d, NODATA %d, resolved %d; want %d, 450 and 450 (each answer once)",
			got[dnsRankNXDOMAIN], got[dnsRankNODATA], got[dnsRankResolved], 8*450)
	}
}

// A DNS mock that holds no response is served as NOERROR with no answer, so it
// ranks as NODATA: over an NXDOMAIN recorded before it, under a resolved
// answer recorded after it.
func TestADNSMockWithNoResponseRanksAsNODATA(t *testing.T) {
	q := dns.Question{Name: "bare.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	bare := &models.Mock{Kind: models.DNS, Spec: models.MockSpec{DNSReq: &models.DNSReq{Name: q.Name, Qtype: q.Qtype, Qclass: q.Qclass}}}
	for _, tc := range []struct {
		name  string
		mocks []*models.Mock
		want  int
	}{
		{"after an NXDOMAIN", []*models.Mock{dnsMockOf(t, q, dns.RcodeNameError, nil, negativeRootSOA), bare}, dnsRankNODATA},
		{"before a resolved answer", []*models.Mock{bare, dnsMockOf(t, q, dns.RcodeSuccess, answerA(q, "10.0.0.9"))}, dnsRankResolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, misses := replayIsolated(t, tc.mocks, q)
			if len(misses) != 0 || got.Msg == nil || dnsAnswerRank(got.Msg) != tc.want {
				t.Fatalf("replay answered %+v (misses %+v), want kind %d", got.Msg, misses, tc.want)
			}
		})
	}
}

// A recorded record that does not decode, as a hand edit or a corrupt mock
// can leave it, is left out of the answer served, and so is one that holds no
// record (an empty line or a comment): it was served as a nil record, which
// crashed keploy's agent when the replay copied the answer into its DNS cache
// (TestServeDNS_AnswersAMockWhoseRecordHoldsNoneAsNODATA). A mock ranks by
// the answer records it is served with, so one whose answer records all fail
// to decode ranks as the NODATA it is served as: a valid resolved answer
// recorded after it is served, and an NXDOMAIN recorded before it ranks under
// it.
func TestADNSMockWhoseAnswersDoNotDecodeRanksAsTheNODATAItServes(t *testing.T) {
	q := dns.Question{Name: "edited.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	valid := dnsMockOf(t, q, dns.RcodeSuccess, answerA(q, "10.0.0.7"))
	nx := dnsMockOf(t, q, dns.RcodeNameError, nil, negativeRootSOA)
	for _, bad := range []struct {
		name    string
		answers []string
	}{
		{"an address that does not parse", []string{"edited.example. 30 IN A not-an-address"}},
		{"an empty record", []string{""}},
		{"a comment", []string{"; edited out"}},
		{"a comment and a bad record", []string{"; edited out", "edited.example. 30 IN A 10.0.0"}},
	} {
		edited := dnsMockOf(t, q, dns.RcodeSuccess, bad.answers)
		t.Run(bad.name+", before a valid resolved answer", func(t *testing.T) {
			got, misses := replayIsolated(t, []*models.Mock{edited, valid}, q)
			if len(misses) != 0 || got.Msg == nil || got.Rcode != dns.RcodeSuccess || len(got.Answer) != 1 ||
				got.Answer[0] == nil || got.Answer[0].String() != valid.Spec.DNSResp.Answers[0] {
				t.Fatalf("replay answered %+v (misses %+v), want the valid answer %q", got.Msg, misses, valid.Spec.DNSResp.Answers)
			}
		})
		for _, tc := range []struct {
			name  string
			mocks []*models.Mock
		}{
			{"alone", []*models.Mock{edited}},
			{"after an NXDOMAIN", []*models.Mock{nx, edited}},
		} {
			t.Run(bad.name+", "+tc.name, func(t *testing.T) {
				got, misses := replayIsolated(t, tc.mocks, q)
				if len(misses) != 0 || got.Msg == nil || got.Rcode != dns.RcodeSuccess || len(got.Answer) != 0 {
					t.Fatalf("replay answered %+v (misses %+v), want NOERROR with no answer", got.Msg, misses)
				}
				reply := got.Copy()
				reply.Question = []dns.Question{q}
				if _, err := reply.Pack(); err != nil {
					t.Fatalf("the answer served does not pack, so the app gets no reply: %v", err)
				}
			})
		}
	}
	// One answer record that decodes is enough, before or after the one that
	// does not: the mock ranks as resolved, is served first of its kind, and
	// with that record only.
	good := answerA(q, "10.0.0.8")[0]
	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"one bad and one valid record", []string{"; edited out", good}},
		{"one valid and one bad record", []string{good, "edited.example. 30 IN A 10.0.0"}},
	} {
		t.Run(tc.name+", before another valid answer", func(t *testing.T) {
			mixed := dnsMockOf(t, q, dns.RcodeSuccess, tc.answers)
			got, misses := replayIsolated(t, []*models.Mock{nx, mixed, valid}, q)
			if len(misses) != 0 || got.Msg == nil || got.Rcode != dns.RcodeSuccess || len(got.Answer) != 1 ||
				got.Answer[0] == nil || got.Answer[0].String() != good {
				t.Fatalf("replay answered %+v (misses %+v), want the mixed mock's valid record %q", got.Msg, misses, good)
			}
		})
	}
}

// packingDNSWriter is the dns.ResponseWriter of a TCP client. It keeps the
// reply ServeDNS writes, and whether that reply packs, as miekg's server
// packs it to write it.
type packingDNSWriter struct {
	msg *dns.Msg
	err error
}

func (w *packingDNSWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}

func (w *packingDNSWriter) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5555}
}

func (w *packingDNSWriter) WriteMsg(m *dns.Msg) error {
	_, err := m.Pack()
	w.msg, w.err = m, err
	return err
}

func (w *packingDNSWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *packingDNSWriter) Close() error                { return nil }
func (w *packingDNSWriter) TsigStatus() error           { return nil }
func (w *packingDNSWriter) TsigTimersOnly(bool)         {}
func (w *packingDNSWriter) Hijack()                     {}

// A mock answer line that holds no record (an empty line or a comment) is
// served through ServeDNS, with the replay's dnsCache in place as in keploy's
// agent. dns.NewRR decodes such a line to a nil record. When that nil record
// was served, the copy of the answer made for the cache dereferenced it.
// ServeDNS then panicked on miekg's request goroutine, which does not
// recover, so keploy's agent crashed. The reply is now NOERROR with no answer:
// the NODATA the mock ranks as.
func TestServeDNS_AnswersAMockWhoseRecordHoldsNoneAsNODATA(t *testing.T) {
	q := dns.Question{Name: "edited.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"an empty record", []string{""}},
		{"a comment", []string{"; edited out"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mgr.Close)
			mgr.SetFilteredMocks([]*models.Mock{dnsMockOf(t, q, dns.RcodeSuccess, tc.answers)})
			p := &Proxy{
				logger:      zap.NewNop(),
				errChannel:  make(chan error, 4),
				IP4:         "127.0.0.1",
				IP6:         "::1",
				mockManager: mgr,
				dnsCache:    newDNSCache(),
			}
			p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
			req := new(dns.Msg)
			req.SetQuestion(q.Name, q.Qtype)
			w := &packingDNSWriter{}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ServeDNS panicked, which crashes keploy's agent (miekg's server does not recover): %v\n%s", r, debug.Stack())
					}
				}()
				p.ServeDNS(w, req)
			}()
			if w.msg == nil || w.err != nil {
				t.Fatalf("ServeDNS wrote %v (pack error: %v), want a reply that packs", w.msg, w.err)
			}
			if w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 0 {
				t.Fatalf("ServeDNS replied %v, want NOERROR with no answer", w.msg)
			}
		})
	}
}
