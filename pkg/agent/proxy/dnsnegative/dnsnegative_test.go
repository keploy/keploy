package dnsnegative

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const upstream = "192.0.2.53:53"

var (
	corp     = []string{".", "corp.example.com"} // "search ." (systemd-resolved's stub) names no domain
	twoCorps = []string{"a.example", "b.example"}
	k8s      = []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
)

// step is one answer the app's resolver got: to name (qtype A unless set),
// rcode, with records when it resolved (none: NODATA).
type step struct {
	name    string
	qtype   uint16
	rcode   int
	records int
}

func nx(name string) step  { return step{name: name, rcode: dns.RcodeNameError} }
func nx6(name string) step { return step{name: name, qtype: dns.TypeAAAA, rcode: dns.RcodeNameError} }
func ok(name string) step  { return step{name: name, rcode: dns.RcodeSuccess, records: 1} }
func ok6(name string) step {
	return step{name: name, qtype: dns.TypeAAAA, rcode: dns.RcodeSuccess, records: 1}
}
func nodata6(name string) step { return step{name: name, qtype: dns.TypeAAAA, rcode: dns.RcodeSuccess} }
func sf(name string) step      { return step{name: name, rcode: dns.RcodeServerFailure} }

func (s step) answer() Answer {
	qt := s.qtype
	if qt == 0 {
		qt = dns.TypeA
	}
	return Answer{Question: dns.Question{Name: s.name, Qtype: qt, Qclass: dns.ClassINET}, Rcode: s.rcode, Records: s.records, Upstream: upstream}
}

// harness is a recording's Log, configured as cfg, logging to logs.
type harness struct {
	t      *testing.T
	l      *Log
	cfg    Config
	logger *zap.Logger
	logs   *observer.ObservedLogs
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	return &harness{t: t, l: &Log{}, cfg: cfg, logger: zap.New(core), logs: logs}
}

// ask notes each step as a question the app asked after the last was
// answered.
func (h *harness) ask(steps ...step) {
	for _, s := range steps {
		a := s.answer()
		h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, "", a.Question), a)
	}
}

// question is the app asking s's question, its answer to come (answer).
func (h *harness) question(s step) Ticket {
	return h.l.Ask(h.logger, h.cfg, "", s.answer().Question)
}

// answer is the answer to the question t asked.
func (h *harness) answer(t Ticket, s step) {
	h.l.Note(h.logger, h.cfg, t, s.answer())
}

func (h *harness) stop() { h.l.Stop(h.logger) }

// lines are the lines at level that say what answer a query got.
func (h *harness) lines(level zapcore.Level) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range h.logs.FilterLevelExact(level).All() {
		if m := e.ContextMap(); m["query"] != nil && m["answer"] != nil {
			out = append(out, e)
		}
	}
	return out
}

// info is "query qtype answer | next step" of each INFO line.
func (h *harness) info() []string {
	var out []string
	for _, e := range h.lines(zapcore.InfoLevel) {
		m := e.ContextMap()
		out = append(out, m["query"].(string)+" "+m["qtype"].(string)+" "+m["answer"].(string)+" | "+m["next_step"].(string))
	}
	return out
}

func (h *harness) debugQueries() []string {
	var out []string
	for _, e := range h.lines(zapcore.DebugLevel) {
		m := e.ContextMap()
		out = append(out, m["query"].(string)+" "+m["qtype"].(string))
	}
	return out
}

func (h *harness) countLines() []observer.LoggedEntry {
	return h.logs.FilterMessage(MsgCount).All()
}

func infoLine(query, qtype, answer, next string) string {
	return query + " " + qtype + " " + answer + " | " + next
}

func equal(a, b []string) bool {
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

// The once-per-lookup key is the name lower-cased: DNS names are
// case-insensitive, and a resolver may ask the same name in another case (an
// app configured with "mongoDb", a client that randomises the case).
func TestOneLinePerLookupWhateverTheCaseOfItsName(t *testing.T) {
	h := newHarness(t, Config{Relayed: true})
	h.ask(nx("MongoDb."), nx("mongodb."), nx("MONGODB."))
	if got := h.info(); len(got) != 1 || !strings.HasPrefix(got[0], "MongoDb. A NXDOMAIN") {
		t.Fatalf("one name asked in three cases logged %v at INFO; want one line, for the first", got)
	}
	h.stop()
	if c := h.countLines(); len(c) != 1 || c[0].ContextMap()["repeats"] != uint64(2) {
		t.Fatalf("the stop counted %+v; want the two other cases as repeats", c)
	}
}

// A lookup logged at INFO stays the app's: when a later walk's candidate
// evidence makes the same name look like a step of it, its repeats are still
// the app's lookup's, not routine ones with a DEBUG line of their own.
func TestALookupLoggedAtINFOStaysTheApps(t *testing.T) {
	h := newHarness(t, Config{Search: twoCorps, Ndots: 1, Relayed: true})
	h.ask(
		nx("db.b.example."), // no db.a.example. before it: the app's own, asked as given
		nx("db.b.example."), // again, with no search between: held on
		nx("db.b.example."), // and again: INFO
		nx("db.a.example."), // a walk of db, whose first candidate this is
		nx("db.b.example."), // again: now it looks like that walk's second step
	)
	if got := h.info(); len(got) != 1 || !strings.HasPrefix(got[0], "db.b.example. A NXDOMAIN") {
		t.Fatalf("logged %v at INFO; want one line, for db.b.example.", got)
	}
	for _, q := range h.debugQueries() {
		if q == "db.b.example. A" {
			t.Fatalf("the lookup logged at INFO got a DEBUG line too: %v", h.debugQueries())
		}
	}
	if got := h.l.named.repeats; got != 3 {
		t.Fatalf("its repeats counted %d times with the app's lookups; want 3", got)
	}
}

// A lookup the app's resolver walks: which answers get an INFO line, with what
// next step, and when. A resolver asks a name whose dots reach ndots as given
// first; glibc's and Go's then try it in each search domain, musl's never
// does. A name with fewer dots is tried in each search domain first and as
// given last. keploy says only what it saw: that the search failed too, when
// it saw it fail in every search domain; that it did not see it, when the app
// asked for the name again first, or the recording stopped.
func TestWhatAWalkTellsTheUser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		steps  []step
		stop   bool     // the recording stops after the steps
		info   []string // the INFO lines, in order
		debugs int      // how many DEBUG lines (-1: not checked)
	}{
		{"a host, ndots:1: a dotted name found in the search domain: no INFO (glibc, Go)",
			Config{Search: corp, Ndots: 1}, []step{nx("svc.prod."), ok("svc.prod.corp.example.com.")}, true,
			nil, 1},
		{"the same, A then AAAA (glibc's single-request), the AAAA NODATA first: no INFO",
			Config{Search: corp, Ndots: 1}, []step{nx("svc.prod."), nx6("svc.prod."), nodata6("svc.prod.corp.example.com."), ok("svc.prod.corp.example.com.")}, true,
			nil, 2},
		{"a host, ndots:1: a dotted name that fails in the search domain too: INFO, plainly",
			Config{Search: corp, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid.corp.example.com.")}, false,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}, 1},
		{"a host, ndots:1, two search domains: plain only once both failed",
			Config{Search: twoCorps, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid.a.example."), nx("nope.example.invalid.b.example.")}, false,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}, 2},
		{"a host, ndots:1: musl asks the dotted name as given only, and once again: held, the line comes at the stop",
			Config{Search: corp, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid.")}, true,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}, 0},
		{"a host, ndots:1: musl asks the dotted name as given only, and again, twice: INFO, no search between",
			Config{Search: corp, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid."), nx("nope.example.invalid.")}, false,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchSkipped)}, 0},
		{"the same, asked once, until the recording stops",
			Config{Search: corp, Ndots: 1}, []step{nx("nope.example.invalid.")}, true,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}, 0},
		{"the same, the search begun, the app asking again: the search decides, unseen at the stop",
			Config{Search: twoCorps, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid.a.example."), nx("nope.example.invalid.")}, false,
			nil, 1},
		{"the same, the recording stopping: the search unseen",
			Config{Search: twoCorps, Ndots: 1}, []step{nx("nope.example.invalid."), nx("nope.example.invalid.a.example."), nx("nope.example.invalid.")}, true,
			[]string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}, 1},
		{"a host, ndots:1: a compose name tried in the search domain, then as given, resolved: no INFO",
			Config{Search: corp, Ndots: 1}, []step{nx("db.corp.example.com."), ok("db.")}, true,
			nil, 1},
		{"a host, ndots:1: a dotless name failing in the search domain and as given: INFO, the Docker hint, plainly",
			Config{Search: corp, Ndots: 1}, []step{nx("mongoDb.corp.example.com."), nx("mongoDb.")}, false,
			[]string{infoLine("mongoDb.", "A", "NXDOMAIN", NextStepDotlessNative)}, 1},
		{"a host, ndots:1: a missing name in the search domain, glibc (as given, then searched): INFO, plainly",
			Config{Search: corp, Ndots: 1}, []step{nx("payments.corp.example.com."), nx("payments.corp.example.com.corp.example.com.")}, false,
			[]string{infoLine("payments.corp.example.com.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}, 1},
		{"a host, ndots:1: a missing name in the first search domain, musl (as given only), asked again, twice: INFO",
			Config{Search: corp, Ndots: 1}, []step{nx("payments.corp.example.com."), nx("payments.corp.example.com."), nx("payments.corp.example.com.")}, false,
			[]string{infoLine("payments.corp.example.com.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchSkipped)}, 0},
		{"the same, musl, asked once, until the recording stops",
			Config{Search: corp, Ndots: 1}, []step{nx("payments.corp.example.com.")}, true,
			[]string{infoLine("payments.corp.example.com.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepWalkUnseen)}, 0},
		{"Docker, ndots:0: a dotless host name found in the search domain: no INFO (glibc, Go)",
			Config{Search: corp, Ndots: 0, Where: AgentContainer}, []step{nx("ldap."), ok("ldap.corp.example.com.")}, true,
			nil, 1},
		{"Docker, ndots:0: a dotless name musl asks as given only, again, twice: INFO, the agent's network, no search between",
			Config{Search: corp, Ndots: 0, Where: AgentContainer}, []step{nx("mongo."), nx("mongo."), nx("mongo.")}, false,
			[]string{infoLine("mongo.", "A", "NXDOMAIN", NextStepDotlessDocker+NextStepSearchSkipped)}, 0},
		{"Docker, ndots:0, glibc: a dotless name failing as given and in the search domain: INFO, plainly",
			Config{Search: corp, Ndots: 0, Where: AppContainer}, []step{nx("mongo."), nx("mongo.corp.example.com.")}, false,
			[]string{infoLine("mongo.", "A", "NXDOMAIN", NextStepDotlessContainer+NextStepSearchFailed)}, 1},
		{"a pod, ndots:5: an external name found as given after the candidates: no INFO",
			Config{Search: k8s, Ndots: 5}, []step{nx("api.example.com.default.svc.cluster.local."), nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com.")}, true,
			nil, 3},
		{"a pod, ndots:5: an external name failing everywhere: INFO for the name as given, plainly",
			Config{Search: k8s, Ndots: 5}, []step{nx("api.example.invalid.default.svc.cluster.local."), nx("api.example.invalid.svc.cluster.local."), nx("api.example.invalid.cluster.local."), nx("api.example.invalid.")}, false,
			[]string{infoLine("api.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName)}, 3},
		{"a pod, ndots:5: a missing Service, its own namespace's name, asked last: INFO, plainly",
			Config{Search: k8s, Ndots: 5}, []step{nx("mongodb.default.svc.cluster.local.default.svc.cluster.local."), nx("mongodb.default.svc.cluster.local.svc.cluster.local."), nx("mongodb.default.svc.cluster.local.cluster.local."), nx("mongodb.default.svc.cluster.local.")}, false,
			[]string{infoLine("mongodb.default.svc.cluster.local.", "A", "NXDOMAIN", NextStepNoSuchName)}, 3},
		{"a pod, ndots:5: a missing dotless Service: INFO, no such Service",
			Config{Search: k8s, Ndots: 5}, []step{nx("mongo.default.svc.cluster.local."), nx("mongo.svc.cluster.local."), nx("mongo.cluster.local."), nx("mongo.")}, false,
			[]string{infoLine("mongo.", "A", "NXDOMAIN", NextStepDotlessK8s)}, 3},
		{"a pod, ndots:5: a StatefulSet member's name in its own namespace, found as the first candidate: no INFO",
			Config{Search: k8s, Ndots: 5}, []step{ok("mongo-0.mongo.default.svc.cluster.local.")}, true,
			nil, 0},
		{"a pod, ndots:5, glibc: a missing StatefulSet member in another namespace, as given then searched: INFO, plainly",
			Config{Search: k8s, Ndots: 5}, []step{nx("mongo-1.mongo.prod.svc.cluster.local."), nx("mongo-1.mongo.prod.svc.cluster.local.default.svc.cluster.local."), nx("mongo-1.mongo.prod.svc.cluster.local.svc.cluster.local."), nx("mongo-1.mongo.prod.svc.cluster.local.cluster.local.")}, false,
			[]string{infoLine("mongo-1.mongo.prod.svc.cluster.local.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}, 3},
		{"a pod, ndots:5, musl: a missing StatefulSet member in its own namespace (the first search domain), asked again, twice: INFO",
			Config{Search: k8s, Ndots: 5}, []step{nx("mongo-0.mongo.default.svc.cluster.local."), nx("mongo-0.mongo.default.svc.cluster.local."), nx("mongo-0.mongo.default.svc.cluster.local.")}, false,
			[]string{infoLine("mongo-0.mongo.default.svc.cluster.local.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchSkipped)}, 0},
		{"a pod, ndots:5, glibc: the same member, as given then searched: INFO, plainly",
			Config{Search: k8s, Ndots: 5}, []step{nx("mongo-0.mongo.default.svc.cluster.local."), nx("mongo-0.mongo.default.svc.cluster.local.default.svc.cluster.local."), nx("mongo-0.mongo.default.svc.cluster.local.svc.cluster.local."), nx("mongo-0.mongo.default.svc.cluster.local.cluster.local.")}, false,
			[]string{infoLine("mongo-0.mongo.default.svc.cluster.local.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}, 3},
		{"a candidate that SERVFAILs is a failure of its own, and the search goes on",
			Config{Search: twoCorps, Ndots: 1, Relayed: true}, []step{nx("svc.prod."), sf("svc.prod.a.example."), ok("svc.prod.b.example.")}, true,
			[]string{infoLine("svc.prod.a.example.", "A", "SERVFAIL", NextStepResolverError)}, 1},
		{"the same, seen by a capture: the app's resolver failed",
			Config{Search: twoCorps, Ndots: 1}, []step{nx("svc.prod."), sf("svc.prod.a.example."), ok("svc.prod.b.example.")}, true,
			[]string{infoLine("svc.prod.a.example.", "A", "SERVFAIL", NextStepResolverErrorObserved)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg)
			h.ask(tc.steps...)
			if tc.stop {
				h.stop()
			}
			if got := h.info(); !equal(got, tc.info) {
				t.Fatalf("the walk %v logged at INFO\n  %q\nwant\n  %q", tc.steps, got, tc.info)
			}
			if got := h.debugQueries(); tc.debugs >= 0 && len(got) != tc.debugs {
				t.Fatalf("the walk logged %d DEBUG lines %v; want %d", len(got), got, tc.debugs)
			}
		})
	}
}

// The app's lookups at the same moment (a pool opening its connections, each
// a lookup of its own) are asked before the first answer comes: they are no
// sign that the resolver will not search, and the search that follows
// decides.
func TestConcurrentLookupsOfANameWaitForItsSearch(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	first, second := h.question(nx("svc.prod.")), h.question(nx("svc.prod.")) // both asked before either is answered
	h.answer(first, nx("svc.prod."))
	h.answer(second, nx("svc.prod."))
	h.ask(ok("svc.prod.corp.example.com."))
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("two concurrent lookups of a name the search found logged %v at INFO", got)
	}
	if got := h.l.routine.repeats; got != 1 {
		t.Fatalf("the second answer counted %d routine repeats; want 1", got)
	}
}

// Which answers a recording gives an INFO line, and with what next step; which
// a DEBUG line only; and which none, when the app gets the same answer again
// with nothing else between. Only an NXDOMAIN is ever routine: a search-list
// candidate's is the resolver walking its list, a reverse lookup's is what a
// private address has, and gRPC's service-config probe NXDOMAINs in every
// healthy gRPC app. A SERVFAIL ends musl's lookup at the candidate, so it is
// the lookup's outcome wherever it comes, but for a reverse lookup's, which
// is never a step of the app's own. A name with no dot gets the container-name
// hint whatever the answer, by where keploy sees it from.
func TestWhatAnAnswerTellsTheUser(t *testing.T) {
	nxa := func(name string) Answer { return nx(name).answer() }
	answer := func(name string, qtype uint16, rcode int) Answer {
		return step{name: name, qtype: qtype, rcode: rcode}.answer()
	}
	skipped := NextStepSearchSkipped
	for _, tc := range []struct {
		name  string
		cfg   Config
		a     Answer
		level zapcore.Level // the line's level; zapcore.InvalidLevel: no line
		next  string
	}{
		{"a dotless name, NXDOMAIN, native: INFO, the Docker-network hint", Config{Search: corp, Ndots: 1}, nxa("mongoDb."), zapcore.InfoLevel, NextStepDotlessNative},
		{"a dotless name, SERVFAIL, native: INFO, the Docker-network hint", Config{}, answer("mongoDb.", dns.TypeA, dns.RcodeServerFailure), zapcore.InfoLevel, NextStepDotlessNative},
		{"a dotless name, REFUSED, native: INFO, the Docker-network hint", Config{Search: corp, Ndots: 1}, answer("redis.", dns.TypeA, dns.RcodeRefused), zapcore.InfoLevel, NextStepDotlessNative},
		{"a dotless name, NXDOMAIN, agent in Docker: INFO, the agent's network", Config{Where: AgentContainer}, nxa("mongo."), zapcore.InfoLevel, NextStepDotlessDocker},
		{"a dotless name, NXDOMAIN, app in Docker: INFO, the app's network", Config{Where: AppContainer}, nxa("mongo."), zapcore.InfoLevel, NextStepDotlessContainer},
		{"a dotless name, NXDOMAIN, in a pod: INFO, no such Service", Config{Search: k8s, Ndots: 5}, nxa("mongo."), zapcore.InfoLevel, NextStepDotlessK8s},
		{"a dotless name, SERVFAIL, in a pod: INFO, no such Service", Config{Search: k8s, Ndots: 5}, answer("mongo.", dns.TypeA, dns.RcodeServerFailure), zapcore.InfoLevel, NextStepDotlessK8s},
		{"a dotless name, a search domain named svc.: no pod", Config{Search: []string{"svc.example.com"}, Ndots: 1}, nxa("mongo."), zapcore.InfoLevel, NextStepDotlessNative},
		{"a dotted name, NXDOMAIN, no search domain: INFO, check the name", Config{}, nxa("no-such-host.example.invalid."), zapcore.InfoLevel, NextStepNoSuchName},
		{"a dotted name, NXDOMAIN, only search .: INFO, check the name", Config{Search: []string{"."}}, nxa("no-such-host.example.invalid."), zapcore.InfoLevel, NextStepNoSuchName},
		{"a dotted name asked before the search domains (ndots:1), again, twice: INFO, no search between", Config{Search: corp, Ndots: 1}, nxa("db.prod."), zapcore.InfoLevel, NextStepNoSuchName + skipped},
		{"a dotless name asked before the search domains (Docker's ndots:0), again, twice: INFO, the hint, no search between", Config{Search: corp, Where: AgentContainer}, nxa("ldap."), zapcore.InfoLevel, NextStepDotlessDocker + skipped},
		{"a name in a search domain no resolver made (Docker's ndots:0, musl), again: INFO", Config{Search: corp, Where: AgentContainer}, nxa("payments.corp.example.com."), zapcore.InfoLevel, NextStepNoSuchName + skipped},
		{"a dotted name asked after the search domains (ndots:5): INFO, check the name", Config{Search: k8s, Ndots: 5}, nxa("api.example.invalid."), zapcore.InfoLevel, NextStepNoSuchName},
		{"a dotted name, SERVFAIL: INFO, the resolver failed", Config{Search: corp, Ndots: 1}, answer("flaky.example.com.", dns.TypeAAAA, dns.RcodeServerFailure), zapcore.InfoLevel, NextStepResolverErrorObserved},
		{"a search-list candidate, NXDOMAIN: DEBUG", Config{Search: twoCorps, Ndots: 3}, nxa("db.A.Example."), zapcore.DebugLevel, ""},
		{"a name in the first search domain whose dots reach ndots, again: INFO (musl asked it as given)", Config{Search: corp, Ndots: 1}, nxa("db.Corp.Example.com."), zapcore.InfoLevel, NextStepNoSuchName + skipped},
		{"a redundant expansion, NXDOMAIN: DEBUG, whatever came before", Config{Search: corp, Ndots: 1}, nxa("pay.corp.example.com.corp.example.com."), zapcore.DebugLevel, ""},
		{"a search-list candidate, SERVFAIL: INFO", Config{Search: corp, Ndots: 1}, answer("db.corp.example.com.", dns.TypeA, dns.RcodeServerFailure), zapcore.InfoLevel, NextStepResolverErrorObserved},
		{"a search-list candidate, REFUSED, relayed: INFO", Config{Search: corp, Ndots: 1, Relayed: true}, answer("db.corp.example.com.", dns.TypeA, dns.RcodeRefused), zapcore.InfoLevel, NextStepResolverError},
		{"a reverse lookup, NXDOMAIN: DEBUG", Config{Search: corp, Ndots: 1}, answer("5.0.17.172.in-addr.arpa.", dns.TypePTR, dns.RcodeNameError), zapcore.DebugLevel, ""},
		{"an IPv6 reverse lookup, NXDOMAIN: DEBUG", Config{Search: corp, Ndots: 1}, answer("1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa.", dns.TypePTR, dns.RcodeNameError), zapcore.DebugLevel, ""},
		{"a reverse lookup, SERVFAIL: DEBUG", Config{Search: corp, Ndots: 1}, answer("5.0.17.172.in-addr.arpa.", dns.TypePTR, dns.RcodeServerFailure), zapcore.DebugLevel, ""},
		{"gRPC's service-config probe, SERVFAIL: DEBUG", Config{Search: corp, Ndots: 1}, answer("_grpc_config.upstream.", dns.TypeTXT, dns.RcodeServerFailure), zapcore.DebugLevel, ""},
		{"gRPC's service-config probe, NXDOMAIN: DEBUG", Config{Search: corp, Ndots: 1}, answer("_grpc_config.upstream.", dns.TypeTXT, dns.RcodeNameError), zapcore.DebugLevel, ""},
		{"the search domain itself is no candidate, again: INFO", Config{Search: corp, Ndots: 1}, answer("corp.example.com.", dns.TypeMX, dns.RcodeNameError), zapcore.InfoLevel, NextStepNoSuchName + skipped},
		{"a resolved name: none", Config{Search: corp, Ndots: 1}, Answer{Question: dns.Question{Name: "example.org.", Qtype: dns.TypeA}, Records: 1, Upstream: upstream}, zapcore.InvalidLevel, ""},
		{"NODATA (NOERROR, no records): none", Config{Search: corp, Ndots: 1}, answer("example.org.", dns.TypeAAAA, dns.RcodeSuccess), zapcore.InvalidLevel, ""},
		{"keploy's own SERVFAIL when no resolver answered: none (logged as an error where it is made)", Config{Search: corp, Ndots: 1}, Answer{Question: dns.Question{Name: "mongoDb.", Qtype: dns.TypeA}, Rcode: dns.RcodeServerFailure, Synthetic: true}, zapcore.InvalidLevel, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg)
			for i := 0; i < 3; i++ {
				h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, "", tc.a.Question), tc.a)
			}
			all := h.logs.FilterField(zap.String("query", tc.a.Question.Name)).All()
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
			if m["qtype"] != dns.TypeToString[tc.a.Question.Qtype] || m["answer"] != dns.RcodeToString[tc.a.Rcode] || m["upstream"] != upstream {
				t.Errorf("the line says qtype %v, answer %v, upstream %v; want %s, %s, %s", m["qtype"], m["answer"], m["upstream"], dns.TypeToString[tc.a.Question.Qtype], dns.RcodeToString[tc.a.Rcode], upstream)
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

// The same name and type with a different answer is a lookup of its own: a
// name that NXDOMAINs and then SERVFAILs tells the user two things.
func TestEachAnswerToALookupIsLoggedOnce(t *testing.T) {
	h := newHarness(t, Config{})
	h.ask(nx("payments.internal."), sf("payments.internal."), nx("payments.internal."), sf("payments.internal."))
	got := h.info()
	if len(got) != 2 || !strings.Contains(got[0], " NXDOMAIN ") || !strings.Contains(got[1], " SERVFAIL ") {
		t.Fatalf("NXDOMAIN, SERVFAIL, NXDOMAIN, SERVFAIL logged %v; want one line for each answer", got)
	}
}

// A name kept routine (its walk looked like another's) that a redundant
// expansion shows the app asked as given is said from what keploy saw of it,
// without counting it again: plainly when it is asked as given last (its
// dots below ndots), with its search failed when keploy saw it fail in every
// search domain; held, while its search is under way, until it ends.
func TestANameKeptRoutineIsLearnedTheApps(t *testing.T) {
	t.Run("asked as given last", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		h.ask(nx("mongodb.default.svc.cluster.local."), nx("mongodb.svc.cluster.local."), nx("mongodb.cluster.local."), nx("mongodb.")) // a walk of mongodb
		h.ask(nx("mongodb.default.svc.cluster.local.default.svc.cluster.local."))                                                       // the app's own walk of mongodb.default.svc.cluster.local
		want := []string{
			infoLine("mongodb.", "A", "NXDOMAIN", NextStepDotlessK8s),
			infoLine("mongodb.default.svc.cluster.local.", "A", "NXDOMAIN", NextStepNoSuchName),
		}
		if got := h.info(); !equal(got, want) {
			t.Fatalf("logged %q at INFO; want %q", got, want)
		}
	})
	t.Run("its search seen to fail", func(t *testing.T) {
		h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
		h.ask(nx("pay.a.example."), nx("pay.b.example."))                                           // a walk of pay: pay.b.example. routine
		h.ask(nx("pay.b.example."), nx("pay.b.example.a.example."), nx("pay.b.example.b.example.")) // the app's own, searched
		want := []string{infoLine("pay.b.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}
		if got := h.info(); !equal(got, want) {
			t.Fatalf("logged %q at INFO as its search's last step came; want %q", got, want)
		}
		h.ask(nx("pay.b.example.b.example.")) // again: nothing more is said or counted
		if got := h.info(); !equal(got, want) {
			t.Fatalf("logged %q at INFO; want %q", got, want)
		}
		h.stop()
		if c := h.countLines(); len(c) != 0 {
			t.Fatalf("the stop counted %+v; want nothing held back of the app's lookup (its answers were counted as routine)", c)
		}
	})
	for _, tc := range []struct {
		name string
		last step
		info []string
	}{
		{"its search under way, then failing", nx("pay.a.example.b.example."),
			[]string{infoLine("pay.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}},
		{"its search under way, then finding it", ok("pay.a.example.b.example."), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
			h.ask(nx("pay.a.example."), nx("pay.b.example."))           // a walk of pay: pay.a.example. its candidate
			h.ask(nx("pay.a.example."), nx("pay.a.example.a.example.")) // the app's own, searched: the redundant step first
			if got := h.info(); len(got) != 0 {
				t.Fatalf("logged %q at INFO while the search was under way", got)
			}
			h.ask(tc.last)
			h.stop()
			if got := h.info(); !equal(got, tc.info) {
				t.Fatalf("logged %q at INFO; want %q", got, tc.info)
			}
		})
	}
}

// A lookup held while its search ran, which then failed, counts its held
// answers as repeats of its line.
func TestAFailedSearchCountsTheRepeatsHeld(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("nope.example.invalid."))
	search := h.question(nx("nope.example.invalid.corp.example.com."))
	h.ask(nx("nope.example.invalid.")) // another lookup while the search runs: held back
	h.answer(search, nx("nope.example.invalid.corp.example.com."))
	h.stop()
	c := h.countLines()
	if len(c) != 1 || c[0].ContextMap()["repeats"] != uint64(1) {
		t.Fatalf("the stop counted %+v; want the answer held back while the search ran as a repeat", c)
	}
}

// After the recording's stop nothing is held, for no stop is left to say it:
// what comes then (the app's lookups as it shuts down) is said at once.
func TestAfterTheStopNothingIsHeld(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.stop()
	h.ask(nx("nope.example.invalid."))
	if got := h.info(); len(got) != 1 || got[0] != infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen) {
		t.Fatalf("a lookup after the stop logged %q at INFO; want it said at once", got)
	}
	h.l.Reset()
	h.ask(nx("nope.example.invalid."))
	if h.l.nheld != 1 {
		t.Fatalf("the next recording holds %d lookups; want it holding again", h.l.nheld)
	}
}

// A Ticket a caller never gave back leaves its name in flight no longer than
// its recording.
func TestATicketNeverGivenBackLastsOnlyItsRecording(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.question(nx("nope.example.invalid.")) // never answered, never given back
	h.l.Reset()
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid."), nx("nope.example.invalid."))
	if got := h.info(); len(got) != 1 || !strings.HasSuffix(got[0], NextStepSearchSkipped) {
		t.Fatalf("logged %q at INFO; want the next recording's retries said", got)
	}
}

// The lookups logged are the app's names, so they are bounded: a client
// resolving a fresh name per request must flood neither the log nor the
// agent's memory for the life of the recording. Past the INFO bound one line
// says the rest go to DEBUG, they do, and the count is logged when the
// recording stops: at INFO with a next step when lookups the user has not
// seen came since the last count, at DEBUG when only repeats did, and not at
// all when nothing moved. Past the tracking bound, answers are only counted.
func TestTheLinesAreBoundedAndTheRestCounted(t *testing.T) {
	h := newHarness(t, Config{})
	const past = 50
	name := func(i int) step { return nx("req-" + strconv.Itoa(i) + ".tenant.example.") }
	for i := 0; i < LogMax+past; i++ {
		h.ask(name(i))
	}
	const repeats = 3
	for i := 0; i < repeats; i++ {
		h.ask(name(0))
	}
	if got := len(h.lines(zapcore.InfoLevel)); got != LogMax {
		t.Fatalf("%d distinct lookups answered NXDOMAIN logged %d INFO lines; want the bound, %d", LogMax+past, got, LogMax)
	}
	if got := len(h.lines(zapcore.DebugLevel)); got != past {
		t.Fatalf("the %d lookups past the INFO bound logged %d DEBUG lines; want one each", past, got)
	}
	if got := h.logs.FilterMessage(MsgLimit).All(); len(got) != 1 || got[0].Level != zapcore.InfoLevel ||
		got[0].ContextMap()["limit"] != int64(LogMax) || got[0].ContextMap()["next_step"] != NextStepSeeDebug {
		t.Fatalf("the bound reached logged %+v; want one INFO line saying the rest go to DEBUG, with the limit and how to see them", got)
	}
	if len(h.countLines()) != 0 {
		t.Fatalf("the count was logged before the recording stopped: %+v", h.countLines())
	}
	h.stop()
	h.stop()
	got := h.countLines()
	if len(got) != 1 || got[0].Level != zapcore.InfoLevel || got[0].ContextMap()["next_step"] != NextStepSeeDebug {
		t.Fatalf("two stops logged %+v; want one INFO count, with how to see the lookups", got)
	}
	if m := got[0].ContextMap(); m["repeats"] != uint64(repeats) || m["debug_lookups"] != uint64(past) || m["untracked_answers"] != uint64(0) || m["lookups_logged"] != int64(LogMax) {
		t.Fatalf("the count says %+v; want repeats %d, debug_lookups %d, untracked_answers 0, lookups_logged %d", m, repeats, past, LogMax)
	}
	h.ask(name(1)) // only a repeat moves
	h.stop()
	if got := h.countLines(); len(got) != 2 || got[1].Level != zapcore.DebugLevel || got[1].ContextMap()["repeats"] != uint64(repeats+1) {
		t.Fatalf("a count only repeats moved is logged again at DEBUG: want a second line, DEBUG, repeats %d; got %+v", repeats+1, got)
	}
	h.ask(name(LogMax + past)) // a lookup past the limit moves
	h.stop()
	if got := h.countLines(); len(got) != 3 || got[2].Level != zapcore.InfoLevel || got[2].ContextMap()["debug_lookups"] != uint64(past+1) {
		t.Fatalf("a new lookup past the limit brings the count back to INFO: want a third line, INFO, debug_lookups %d; got %+v", past+1, got)
	}

	// Past the tracking bound nothing more is held, and the answers are counted.
	for i := 0; i < TrackMax+past; i++ {
		h.ask(name(i))
	}
	if got := len(h.l.named.seen); got != TrackMax {
		t.Fatalf("the set of lookups logged holds %d after %d distinct names; want its bound, %d", got, TrackMax+past, TrackMax)
	}
	if got := len(h.lines(zapcore.DebugLevel)); got != TrackMax-LogMax {
		t.Fatalf("%d DEBUG lines for lookups past the INFO bound; want one for each the set holds, %d", got, TrackMax-LogMax)
	}
	h.stop()
	if m := h.countLines()[3].ContextMap(); m["debug_lookups"] != uint64(TrackMax-LogMax) || m["untracked_answers"] != uint64(past) {
		t.Fatalf("the count says %+v; want debug_lookups %d and untracked_answers %d", m, TrackMax-LogMax, past)
	}
	for i := 0; i < repeats; i++ { // answers it does not track move the count at DEBUG
		h.ask(name(TrackMax))
	}
	h.stop()
	if got := h.countLines(); len(got) != 5 || got[4].Level != zapcore.DebugLevel || got[4].ContextMap()["untracked_answers"] != uint64(past+repeats) {
		t.Fatalf("answers to an untracked lookup are counted at DEBUG: want a fifth line, DEBUG, untracked_answers %d; got %+v", past+repeats, got)
	}
}

// The routine lookups' DEBUG lines, the names learned as asked, and the
// lookups held are bounded the same way: past its bound a lookup is said at
// once, as it would be without waiting.
func TestTheOtherSetsAreBounded(t *testing.T) {
	const past = 50
	h := newHarness(t, Config{Search: []string{"svc.cluster.local"}, Ndots: 5})
	for i := 0; i < TrackMax+past; i++ {
		n := "req-" + strconv.Itoa(i) + ".svc.cluster.local."
		h.ask(nx(n), nx(n+"svc.cluster.local."))
	}
	if got := len(h.l.routine.seen); got != TrackMax || h.l.routine.pastMax == 0 {
		t.Fatalf("the set of routine lookups holds %d, and counted %d answers past it; want it full at its bound, %d, and the rest counted", got, h.l.routine.pastMax, TrackMax)
	}
	if got := len(h.l.learned); got != TrackMax {
		t.Fatalf("the names learned as asked hold %d after %d; want them full at their bound, %d", got, TrackMax+past, TrackMax)
	}

	h = newHarness(t, Config{Search: corp, Ndots: 1})
	for i := 0; i < TrackMax+past; i++ {
		h.ask(nx("req-" + strconv.Itoa(i) + ".tenant.example."))
	}
	if h.l.nheld != TrackMax {
		t.Fatalf("%d lookups asked as given before a search are held; want the bound, %d", h.l.nheld, TrackMax)
	}
	if got := len(h.lines(zapcore.InfoLevel)) + len(h.lines(zapcore.DebugLevel)); got != past {
		t.Fatalf("the %d lookups past the bound of those held logged %d lines; want one each, at once", past, got)
	}
	for _, e := range h.lines(zapcore.InfoLevel) {
		if !strings.HasSuffix(e.ContextMap()["next_step"].(string), NextStepSearchUnseen) {
			t.Fatalf("a lookup said at once past the bound gives the next step %q; want it to say keploy did not see its search", e.ContextMap()["next_step"])
		}
	}
	h.stop()
	if got := len(h.lines(zapcore.InfoLevel)) + len(h.lines(zapcore.DebugLevel)); got != TrackMax {
		t.Fatalf("the stop logged the lookups held: %d lines in all; want the bound of the lookups logged, %d", got, TrackMax)
	}
	if got := h.l.named.pastMax; got != past {
		t.Fatalf("%d of the lookups held found no room to be logged, and counted %d; want %d", past, got, past)
	}
	if h.l.nheld != 0 || len(h.l.held) != 0 {
		t.Fatalf("the stop left %d lookups held", h.l.nheld)
	}
}

// Each recording logs its own: a lookup the last recording logged is logged
// again in the next, and neither the last one's count nor the names it
// learned carry over. A lookup still held as the next recording starts (its
// walk under way) carries over, for its walk to decide; so does what keploy
// remembers of the resolver's walks.
func TestEachRecordingLogsItsOwn(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("mongoDb."), nx("mongoDb."), nx("pay.corp.example.com.corp.example.com."), nx("held.example."))

	h.l.Reset()
	h.ask(nx("held.example.corp.example.com.")) // its walk goes on in the next recording
	want := []string{
		infoLine("mongoDb.", "A", "NXDOMAIN", NextStepDotlessNative),
		infoLine("held.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed),
	}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}
	if got := h.debugQueries(); len(got) != 2 || got[1] != "held.example.corp.example.com. A" {
		t.Fatalf("logged %v at DEBUG; want the candidate known for one, by the answer remembered across the start", got)
	}

	h.ask(nx("mongoDb."))
	if got := len(h.logs.FilterField(zap.String("query", "mongoDb.")).FilterLevelExact(zapcore.InfoLevel).All()); got != 2 {
		t.Fatalf("the same lookup in two recordings logged %d lines; want one in each", got)
	}
	if h.l.wasLearned("", "pay.corp.example.com.") {
		t.Fatal("a name the last recording learned the app asked as given carried over")
	}
	h.stop()
	if got := h.countLines(); len(got) != 0 {
		t.Fatalf("the second recording held nothing back, yet logged a count: %+v (the first recording's repeat carried over)", got)
	}
}

// A walk under way as a recording starts is decided by its next step, with
// what keploy saw of its earlier ones.
func TestAWalkAcrossARecordingsStartIsDecidedByItsSteps(t *testing.T) {
	h := newHarness(t, Config{Search: k8s, Ndots: 5})
	h.ask(nx("api.example.com.default.svc.cluster.local.")) // held: maybe the app's own
	h.l.Reset()                                             // the next recording starts
	h.ask(nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com."))
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("a walk across the start that resolved logged %q at INFO", got)
	}
}

// Concurrent notes: the INFO bound and the one line that says it was reached
// hold however they interleave.
func TestTheBoundHoldsUnderConcurrentLookups(t *testing.T) {
	h := newHarness(t, Config{})
	const lookups = 2 * LogMax
	var wg sync.WaitGroup
	for i := 0; i < lookups; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := nx("req-" + strconv.Itoa(i) + ".tenant.example.")
			h.answer(h.question(s), s)
			h.answer(h.question(s), s)
		}()
	}
	wg.Wait()
	if got := len(h.lines(zapcore.InfoLevel)); got != LogMax {
		t.Fatalf("%d concurrent lookups logged %d INFO lines; want %d", lookups, got, LogMax)
	}
	if got := len(h.lines(zapcore.DebugLevel)); got != lookups-LogMax {
		t.Fatalf("%d concurrent lookups logged %d DEBUG lines; want %d", lookups, got, lookups-LogMax)
	}
	if got := h.logs.FilterMessage(MsgLimit).Len(); got != 1 {
		t.Fatalf("the bound reached logged %d lines saying so; want 1", got)
	}
	if got := h.l.named.repeats; got != lookups {
		t.Fatalf("counted %d repeats; want %d", got, lookups)
	}
}

// Concurrent walks: lookups held while others are noted, released by their
// search's steps, keep the counts whole.
func TestHeldLookupsUnderConcurrentWalks(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	const walks = 64
	var wg sync.WaitGroup
	for i := 0; i < walks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := "svc-" + strconv.Itoa(i) + ".prod."
			h.answer(h.question(nx(n)), nx(n))
			next := nx(n + "corp.example.com.")
			if i%2 == 0 {
				next = ok(n + "corp.example.com.")
			}
			h.answer(h.question(next), next)
		}()
	}
	wg.Wait()
	h.stop()
	if got := len(h.lines(zapcore.InfoLevel)); got != walks/2 {
		t.Fatalf("%d walks, half failing, logged %d INFO lines; want %d", walks, got, walks/2)
	}
	for _, e := range h.lines(zapcore.InfoLevel) {
		if !strings.HasSuffix(e.ContextMap()["next_step"].(string), NextStepSearchFailed) {
			t.Fatalf("a walk seen to fail logged %q", e.ContextMap()["next_step"])
		}
	}
	if h.l.nheld != 0 {
		t.Fatalf("%d lookups still held after every walk ended", h.l.nheld)
	}
}

// A candidate keeps its class for the recording. Its repeats come from a
// cache, and the earlier candidate's answer, the evidence that made it
// routine, may have gone from what keploy remembers between two steps of a
// later walk: that must not turn a healthy run's candidate into an INFO line.
func TestACandidateStaysRoutineWhenItsEvidenceIsForgotten(t *testing.T) {
	h := newHarness(t, Config{Search: k8s, Ndots: 5})
	walk := []step{
		nx("api.example.com.default.svc.cluster.local."),
		nx("api.example.com.svc.cluster.local."),
		nx("api.example.com.cluster.local."),
		ok("api.example.com."),
	}
	h.ask(walk...)
	// The next request's walk, once the first candidate's answer is
	// forgotten: the second's evidence is gone.
	h.l.seen.Remove(key("", "api.example.com.default.svc.cluster.local.", dns.TypeA))
	h.ask(walk[1])
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("a healthy pod's walk logged %v at INFO", got)
	}
	if got := h.l.routine.repeats; got != 1 {
		t.Fatalf("the candidate's answer again counted %d routine repeats; want 1", got)
	}
}

// Apps that share a Log (pods on a node) are kept apart: a lookup is logged
// once in each, and one app's walk does not end another's held lookup.
func TestScopesAreKeptApart(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	in := func(scope string, s step) {
		a := s.answer()
		a.Scope = scope
		h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, scope, a.Question), a)
	}
	in("pod-a", nx("mongoDb."))
	in("pod-b", nx("mongoDb."))
	in("pod-a", nx("svc.prod."))
	in("pod-b", ok("svc.prod.corp.example.com.")) // another pod's search
	if got := h.l.nheld; got != 1 {
		t.Fatalf("pod-b's search ended pod-a's held lookup: %d held; want 1", got)
	}
	h.stop()
	if got := h.info(); len(got) != 3 {
		t.Fatalf("logged %v at INFO; want mongoDb. in each pod, and pod-a's svc.prod.", got)
	}
}

// A capture that sees the app's own exchange with its resolver says so, not
// that keploy passed the answer on, and its lines carry its fields.
func TestAnObservedAnswerSaysWhatKeploySaw(t *testing.T) {
	h := newHarness(t, Config{Fields: []zap.Field{zap.String("pod", "default/orders-0")}})
	h.ask(nx("mongoDb."), nx("_grpc_config.orders.").withType(dns.TypeTXT))
	info := h.logs.FilterLevelExact(zapcore.InfoLevel).All()
	if len(info) != 1 || info[0].Message != MsgObserved || info[0].ContextMap()["pod"] != "default/orders-0" {
		t.Fatalf("logged %+v at INFO; want one line saying the app's resolver did not resolve it, with the pod", info)
	}
	debug := h.lines(zapcore.DebugLevel)
	if len(debug) != 1 || debug[0].Message != MsgObservedRoutine {
		t.Fatalf("logged %+v at DEBUG; want the routine line in the capture's words", debug)
	}
}

func (s step) withType(qtype uint16) step {
	s.qtype = qtype
	return s
}

// keploy's own answer, made when no resolver answered, gets no line, but it is
// a step of the walk the app's resolver makes: the search went on from there,
// though no resolver said the name is missing in that search domain.
func TestASyntheticAnswerIsAStepOfTheWalk(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("nope.example.invalid."))
	syn := sf("nope.example.invalid.corp.example.com.").answer()
	syn.Synthetic = true
	h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, "", syn.Question), syn)
	got := h.info()
	if len(got) != 1 || got[0] != infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen) {
		t.Fatalf("logged %v at INFO; want the name's line, its search not seen answered by a resolver everywhere", got)
	}
}

func TestRedundantExpansionBase(t *testing.T) {
	for _, tc := range []struct {
		search []string
		name   string
		base   string
		ok     bool
	}{
		{k8s, "Mongo.default.svc.cluster.local.default.svc.cluster.local.", "Mongo.default.svc.cluster.local.", true},
		{k8s, "mongo.default.svc.cluster.local.svc.cluster.local", "mongo.default.svc.cluster.local.", true},
		{k8s, "mongo.default.svc.cluster.local.", "", false},
		{[]string{"corp.example.com."}, "pay.corp.example.com.corp.example.com.", "pay.corp.example.com.", true},
		{nil, "pay.corp.example.com.corp.example.com.", "", false},
		{[]string{"."}, "x.corp.", "", false},
	} {
		base, ok := RedundantExpansionBase(tc.search, tc.name)
		if base != tc.base || ok != tc.ok {
			t.Errorf("RedundantExpansionBase(%v, %q) = %q, %v; want %q, %v", tc.search, tc.name, base, ok, tc.base, tc.ok)
		}
	}
}

// Another lookup of a name that starts while the first's walk is still in
// flight is no sign that the resolver will not search it: a sibling question
// (the AAAA beside the A) still unanswered, or the walk's next step already
// asked, makes the question no lookup of its own, so the walk decides.
func TestALookupStartedMidWalkIsNoRetry(t *testing.T) {
	t.Run("a sibling of the held answer still in flight", func(t *testing.T) {
		h := newHarness(t, Config{Search: corp, Ndots: 1})
		a, aaaa := h.question(nx("svc.prod.")), h.question(nx6("svc.prod."))
		h.answer(a, nx("svc.prod."))            // held; its AAAA still in flight
		h.ask(nx("svc.prod."), nx("svc.prod.")) // two other lookups' A, from a cache
		h.answer(aaaa, nx6("svc.prod."))
		h.ask(ok("svc.prod.corp.example.com."), ok6("svc.prod.corp.example.com."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("a lookup started beside an unanswered sibling logged %v at INFO; want none, the search found it", got)
		}
	})
	t.Run("the walk's next step already asked", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		h.ask(nx("api.example.com.default.svc.cluster.local."))      // held: maybe the app's own
		next := h.question(nx("api.example.com.svc.cluster.local.")) // the walk goes on
		h.ask(nx("api.example.com.default.svc.cluster.local."))      // another lookup's first step
		h.answer(next, nx("api.example.com.svc.cluster.local."))
		h.ask(nx("api.example.com.cluster.local."), ok("api.example.com."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("a lookup started while the walk's next step was in flight logged %v at INFO", got)
		}
	})
	t.Run("the walk's next step asked, the held name re-asked before and after", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		first := "api.example.com.default.svc.cluster.local."
		h.ask(nx(first), nx(first))                                  // held, then re-armed by another lookup's first step
		next := h.question(nx("api.example.com.svc.cluster.local.")) // the walk goes on: its question ends the hold
		h.ask(nx(first))                                             // a third lookup's first step: no retry
		h.answer(next, nx("api.example.com.svc.cluster.local."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("a first step asked again after the walk's next question logged %v at INFO", got)
		}
	})
	t.Run("the search of a name asked as given begun", func(t *testing.T) {
		h := newHarness(t, Config{Search: corp, Ndots: 1})
		h.ask(nx("svc.prod."))
		search := h.question(ok("svc.prod.corp.example.com."))
		h.ask(nx("svc.prod."), nx("svc.prod.")) // other lookups, while the search runs
		h.answer(search, ok("svc.prod.corp.example.com."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("a lookup started while the search ran logged %v at INFO", got)
		}
	})
}

// musl asks A and AAAA together: the A's question starts the retry, the
// AAAA's is its sibling, and the second retry ends what keploy held of the
// name, both qtypes.
func TestARetryEndsEveryQtypeHeld(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	a, aaaa := h.question(nx("payments.corp.example.com.")), h.question(nx6("payments.corp.example.com."))
	h.answer(a, nx("payments.corp.example.com."))
	h.answer(aaaa, nx6("payments.corp.example.com."))
	for retry := 1; retry <= 2; retry++ { // the app retries, twice
		a, aaaa = h.question(nx("payments.corp.example.com.")), h.question(nx6("payments.corp.example.com."))
		h.answer(a, nx("payments.corp.example.com."))
		if got, want := len(h.info()), 2*(retry-1); got != want {
			t.Fatalf("retry %d's first answer logged %d INFO lines; want %d", retry, got, want)
		}
		h.answer(aaaa, nx6("payments.corp.example.com."))
	}
	if got := h.l.named.repeats; got != 4 {
		t.Fatalf("the retries' answers counted %d repeats; want 4", got)
	}
}

// A question no answer came to is given back, so that its name is not taken
// to be in flight for good: the next lookup of it is one of its own.
func TestAnUnansweredQuestionLeavesNothingInFlight(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("nope.example.invalid."))
	h.l.Unanswered(h.question(nx("nope.example.invalid.")))
	if len(h.l.inflight) != 0 {
		t.Fatalf("an unanswered question left %v in flight", h.l.inflight)
	}
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid."))
	if got := h.info(); len(got) != 1 || !strings.HasSuffix(got[0], NextStepSearchSkipped) {
		t.Fatalf("logged %v at INFO; want the lookup asked again said, no search between", got)
	}
	var zero Ticket
	h.l.Note(h.logger, h.cfg, zero, nx("other.example.invalid.").answer()) // a question the Log was not told of
	if len(h.l.inflight) != 0 {
		t.Fatalf("the zero ticket changed what is in flight: %v", h.l.inflight)
	}
}

// What is in flight is bounded: past the bound a question is asked untracked,
// and never taken for a lookup of its own.
func TestInFlightIsBounded(t *testing.T) {
	h := newHarness(t, Config{})
	for i := 0; i < inflightMax+10; i++ {
		tk := h.question(nx("req-" + strconv.Itoa(i) + ".tenant.example."))
		if i >= inflightMax && (tk.inflight != "" || tk.quiet) {
			t.Fatalf("question %d past the bound was tracked: %+v", i, tk)
		}
	}
	if got := len(h.l.inflight); got != inflightMax {
		t.Fatalf("%d names in flight; want the bound, %d", got, inflightMax)
	}
}

// A and AAAA asked together (glibc's and Go's default, musl's): one lookup,
// both held until its search decides.
func TestAAndAAAAAskedTogetherWaitForTheSearch(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	a, aaaa := h.question(nx("svc.prod.")), h.question(nx6("svc.prod."))
	h.answer(a, nx("svc.prod."))
	h.answer(aaaa, nx6("svc.prod."))
	a, aaaa = h.question(ok("svc.prod.corp.example.com.")), h.question(nodata6("svc.prod.corp.example.com."))
	h.answer(aaaa, nodata6("svc.prod.corp.example.com."))
	h.answer(a, ok("svc.prod.corp.example.com."))
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("a lookup the search found logged %v at INFO", got)
	}
	if got := h.debugQueries(); len(got) != 2 {
		t.Fatalf("logged %v at DEBUG; want each qtype's answer as given, found in the search domain", got)
	}
}

// A question for the name asked before the held one, and answered after it,
// was in flight beside it: no retry, so one retry after it still only re-arms
// the hold.
func TestAnEarlierQuestionAnsweredLateIsNoRetry(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	early := h.question(nx("svc.prod."))
	held := h.question(nx("svc.prod."))
	h.answer(held, nx("svc.prod."))
	h.answer(early, nx("svc.prod."))
	h.ask(nx("svc.prod."))                  // another lookup, between two steps
	h.ask(ok("svc.prod.corp.example.com.")) // the first lookup's search
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("an answer that came late logged %v at INFO; want none, the search found the name", got)
	}
}

// Another walk's step does not end a lookup held because the resolver
// searches it: a name in the first search domain the app asked as given,
// whose search is under way when the app also looks up the shorter name.
func TestAnotherWalksStepDoesNotEndASearchUnderWay(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("payments.corp.example.com."))                                 // held: maybe the app's own
	search := h.question(nx("payments.corp.example.com.corp.example.com.")) // glibc searches it
	h.ask(nx("payments."))                                                  // the app also looks up payments
	h.answer(search, nx("payments.corp.example.com.corp.example.com."))
	got := h.info()
	want := infoLine("payments.corp.example.com.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)
	if len(got) != 2 || got[0] != infoLine("payments.", "A", "NXDOMAIN", NextStepDotlessNative) || got[1] != want {
		t.Fatalf("logged %q at INFO; want payments. and then %q", got, want)
	}
}

// The answers counted while a lookup was held are counted with it when it is
// said: the stop's count has them.
func TestAHeldLookupsRepeatsAreCounted(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	first, second := h.question(nx("nope.example.invalid.")), h.question(nx("nope.example.invalid."))
	h.answer(first, nx("nope.example.invalid."))
	h.answer(second, nx("nope.example.invalid.")) // held: a concurrent duplicate
	h.stop()
	c := h.countLines()
	if len(c) != 1 || c[0].ContextMap()["repeats"] != uint64(1) {
		t.Fatalf("the stop counted %+v; want the concurrent duplicate as a repeat", c)
	}
}

// Another answer to a held lookup's question, not a retry, is a lookup of its
// own: a SERVFAIL beside the held NXDOMAIN is said.
func TestAnotherAnswerToAHeldLookupIsSaid(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	first, second := h.question(nx("svc.prod.")), h.question(sf("svc.prod."))
	h.answer(first, nx("svc.prod."))
	h.answer(second, sf("svc.prod."))
	if got := h.info(); len(got) != 1 || !strings.HasPrefix(got[0], "svc.prod. A SERVFAIL") {
		t.Fatalf("logged %v at INFO; want the SERVFAIL said", got)
	}
}

// A walk's step whose question the Log was not told of (the zero Ticket: the
// capture began noting between the question and its answer) still ends what
// it shows the end of.
func TestAnAnswerAloneIsAStepOfTheWalk(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		held step // asked, and held: maybe the app's own
		next step // the walk's next step, its question untold
	}{
		{"the name in another search domain", Config{Search: k8s, Ndots: 5}, nx("api.example.com.default.svc.cluster.local."), nx("api.example.com.svc.cluster.local.")},
		{"the name as given", Config{Search: corp, Ndots: 1}, nx("db.corp.example.com."), ok("db.")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg)
			h.ask(tc.held)
			var untold Ticket
			h.l.Note(h.logger, h.cfg, untold, tc.next.answer())
			h.stop()
			if got := h.info(); len(got) != 0 {
				t.Fatalf("a walk that resolved logged %v at INFO", got)
			}
		})
	}
}

// A walk's first step held while another lookup's walk of the same name is
// at its last step (two lookups of db at once): that step's answer, whose
// question came before the hold, ends it.
func TestAHoldMadeWhileTheWalksLastStepIsInFlight(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	last := h.question(ok("db."))     // one lookup's last step, in flight
	h.ask(nx("db.corp.example.com.")) // the other's first step: held
	h.answer(last, ok("db."))         // the first lookup's answer
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("two lookups of a name that resolved logged %v at INFO", got)
	}
}

// A candidate answered NODATA (its name exists, without this qtype) is a step
// a resolver walks on from, as is keploy's own answer when no resolver
// answered: the next candidate is the walk's.
func TestAResolverWalksOnFromNODATAAndFromKeploysOwnAnswer(t *testing.T) {
	for _, first := range []step{
		{name: "db.a.example.", rcode: dns.RcodeSuccess}, // NODATA
		sf("db.a.example."), // keploy's own SERVFAIL
	} {
		h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
		a := first.answer()
		a.Synthetic = first.rcode == dns.RcodeServerFailure
		h.l.Note(h.logger, h.cfg, h.question(first), a)
		h.ask(nx("db.b.example.")) // the recording stops before db. comes
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("after %+v the walk logged %v at INFO; want its next candidate routine", first, got)
		}
	}
}

// The search found the name: what was held of it is a step, every qtype,
// though only one qtype's candidate came.
func TestTheSearchFindingANameEndsEveryQtypeHeld(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	a, aaaa := h.question(nx("svc.prod.")), h.question(nx6("svc.prod."))
	h.answer(a, nx("svc.prod."))
	h.answer(aaaa, nx6("svc.prod."))
	h.ask(ok("svc.prod.corp.example.com."))
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("a name its search found logged %v at INFO", got)
	}
}

// A lookup held because the resolver searches it, in the first search domain
// and with the shorter name's walk going on beside it, is not that walk's
// candidate: only its search decides.
func TestASearchedNameIsNoOtherWalksCandidate(t *testing.T) {
	h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
	h.ask(nx("pay.a.example."))           // held: maybe the app's own
	h.ask(nx("pay.a.example.a.example.")) // glibc searches it
	h.ask(nx("pay."))                     // the app also looks up pay
	h.ask(nx("pay.a.example.b.example.")) // the search's last step
	got := h.info()
	want := []string{
		infoLine("pay.", "A", "NXDOMAIN", NextStepDotlessNative),
		infoLine("pay.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed),
	}
	if !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}
}

// A resolver asks no name longer than a DNS name can be: a search domain
// that would make one is no step its search waits for, and a name no search
// domain can take is said at once.
func TestASearchDomainTooLongForTheNameIsNoStep(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 40) + ".example." // 232 with its dot
	longDomain := strings.Repeat("e", 30) + ".example"                                                                                            // 38: too long beside it
	h := newHarness(t, Config{Search: []string{"a.example", longDomain}, Ndots: 1})
	h.ask(nx(long), nx(long+"a.example."))
	if got := h.info(); len(got) != 1 || got[0] != infoLine(long, "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed) {
		t.Fatalf("logged %q at INFO; want the long name said once its one askable search domain failed", got)
	}
	h = newHarness(t, Config{Search: []string{longDomain}, Ndots: 1})
	h.ask(nx(long))
	if got := h.info(); len(got) != 1 || got[0] != infoLine(long, "A", "NXDOMAIN", NextStepNoSuchName) {
		t.Fatalf("logged %q at INFO; want the long name said at once, with no search to wait for", got)
	}
}

// Another lookup of a name that starts between two steps of the first one's
// walk, with nothing of the name in flight, looks like the app asking again:
// once is not enough to say so, and the first walk's next step decides.
func TestAnOverlappingLookupBetweenTwoStepsIsNoRetry(t *testing.T) {
	t.Run("a pod, an external name: the first candidate asked again", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		h.ask(nx("api.example.com.default.svc.cluster.local."))                                                       // lookup 1's first step: held
		h.ask(nx("api.example.com.default.svc.cluster.local."))                                                       // lookup 2's first step, from a cache
		h.ask(nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com.")) // lookup 1 goes on
		h.ask(nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com.")) // and lookup 2
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("two overlapping walks that resolved logged %v at INFO", got)
		}
	})
	t.Run("a host, a name asked as given first", func(t *testing.T) {
		h := newHarness(t, Config{Search: corp, Ndots: 1})
		h.ask(nx("svc.prod."), nx("svc.prod."), ok("svc.prod.corp.example.com."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("two overlapping lookups the search resolved logged %v at INFO", got)
		}
	})
	t.Run("a host, the name as given asked last, still in flight", func(t *testing.T) {
		h := newHarness(t, Config{Search: corp, Ndots: 1})
		h.ask(nx("db.corp.example.com."))                             // held: maybe the app's own
		last := h.question(ok("db."))                                 // the walk's last step, in flight
		h.ask(nx("db.corp.example.com."), nx("db.corp.example.com.")) // other lookups' first steps
		h.answer(last, ok("db."))
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("lookups started while the walk's last step was in flight logged %v at INFO", got)
		}
	})
}

// A routine line says why: a candidate is the resolver's walk, a name as
// given that its search found is that search's first step.
func TestARoutineLineSaysWhyItIsRoutine(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("db.corp.example.com."), ok("db."), nx("svc.prod."), ok("svc.prod.corp.example.com."))
	want := map[string]string{"db.corp.example.com.": RoutineCandidate, "svc.prod.": RoutineFound}
	got := map[string]string{}
	for _, e := range h.lines(zapcore.DebugLevel) {
		got[e.ContextMap()["query"].(string)] = e.ContextMap()["routine"].(string)
	}
	if len(got) != 2 || got["db.corp.example.com."] != want["db.corp.example.com."] || got["svc.prod."] != want["svc.prod."] {
		t.Fatalf("the routine lines say %q; want %q", got, want)
	}
}

// A candidate up to 254 characters with its trailing dot is asked (Go's
// rule), one longer is not.
func TestTheLongestCandidateAResolverAsks(t *testing.T) {
	const domain = "corp.example.com" // 16
	for _, tc := range []struct {
		nameLen int // with its trailing dot
		asked   bool
	}{{237, true}, {238, false}} {
		// a.<filler>. of nameLen characters, with a dot to reach ndots:1
		name := "a." + strings.Repeat("b", tc.nameLen-3) + "."
		if got := !tooLong(name, domain); got != tc.asked {
			t.Fatalf("a %d-character name in %s (candidate %d with its dot): asked %v; want %v", len(name), domain, len(name)+len(domain)+1, got, tc.asked)
		}
	}
}

// A candidate answered NODATA for one qtype ends nothing while the other
// qtype may still resolve there; NODATA for both, in every search domain, is
// the search failing.
func TestNODATAAtACandidateWaitsForTheOtherQtype(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	a, aaaa := h.question(nx("v4only.prod.")), h.question(nx6("v4only.prod."))
	h.answer(a, nx("v4only.prod."))
	h.answer(aaaa, nx6("v4only.prod."))
	a, aaaa = h.question(ok("v4only.prod.corp.example.com.")), h.question(nodata6("v4only.prod.corp.example.com."))
	h.answer(aaaa, nodata6("v4only.prod.corp.example.com."))
	if got := h.info(); len(got) != 0 {
		t.Fatalf("an AAAA NODATA logged %v at INFO before the A came", got)
	}
	h.answer(a, ok("v4only.prod.corp.example.com."))

	nodataA := step{name: "noaddr.prod.corp.example.com.", rcode: dns.RcodeSuccess}
	a, aaaa = h.question(nx("noaddr.prod.")), h.question(nx6("noaddr.prod."))
	h.answer(a, nx("noaddr.prod."))
	h.answer(aaaa, nx6("noaddr.prod."))
	a, aaaa = h.question(nodataA), h.question(nodata6("noaddr.prod.corp.example.com."))
	h.answer(a, nodataA)
	h.answer(aaaa, nodata6("noaddr.prod.corp.example.com."))
	want := []string{
		infoLine("noaddr.prod.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed),
		infoLine("noaddr.prod.", "AAAA", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed),
	}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want only noaddr.prod.'s lookups, their search failed", got)
	}
}

// Apps that share a Log keep their questions in flight apart too: one pod's
// lookup of a name in flight is no sibling of another pod's.
func TestInFlightIsPerScope(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	ask := func(scope string, s step) Ticket { return h.l.Ask(h.logger, h.cfg, scope, s.answer().Question) }
	note := func(scope string, tk Ticket, s step) {
		a := s.answer()
		a.Scope = scope
		h.l.Note(h.logger, h.cfg, tk, a)
	}
	note("pod-b", ask("pod-b", nx("nope.example.invalid.")), nx("nope.example.invalid.")) // held
	ask("pod-a", nx("nope.example.invalid."))                                             // pod-a's, in flight
	for i := 0; i < 2; i++ {                                                              // pod-b asks again, twice
		note("pod-b", ask("pod-b", nx("nope.example.invalid.")), nx("nope.example.invalid."))
	}
	if got := h.info(); len(got) != 1 {
		t.Fatalf("pod-b's retries logged %v at INFO; want its lookup said (pod-a's question is no sibling of it)", got)
	}
}

// An overlapping lookup asks A and AAAA: answered from a cache, its A is
// noted before its AAAA comes, so both look quiet. That is one re-ask of each
// qtype, not two of the name.
func TestAnOverlappingLookupsAAndAAAAAreOneReAsk(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	a, aaaa := h.question(nx("svc.prod.")), h.question(nx6("svc.prod."))
	h.answer(a, nx("svc.prod."))
	h.answer(aaaa, nx6("svc.prod."))
	h.ask(nx("svc.prod."), nx6("svc.prod."))                                       // another lookup, from the cache, one qtype after the other
	h.ask(ok("svc.prod.corp.example.com."), nodata6("svc.prod.corp.example.com.")) // the first lookup's search
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("an overlapping lookup answered from a cache logged %v at INFO", got)
	}
}

// A search that stops short of the search domains keploy read (glibc ends it
// at a step answered REFUSED; the app's own search list may be shorter) is
// not waited on for good: the app asking for the name again, twice, with no
// step of its search in flight, ends it, as a search keploy did not see
// answered everywhere. A step in flight holds it.
func TestASearchThatStopsShortEndsOnTheAppsRetries(t *testing.T) {
	h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid.a.example.")) // the app's list ends at a.example
	step := h.question(nx("nope.example.invalid.a.example."))                 // a retry's step, in flight
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid."))           // asked while it is: no retry
	h.answer(step, nx("nope.example.invalid.a.example."))
	if got := h.info(); len(got) != 0 {
		t.Fatalf("lookups asked while a step of the search was in flight logged %v at INFO", got)
	}
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid.a.example."), nx("nope.example.invalid."))
	want := []string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}
}

// A step of the search asked and never answered is still a step: the app's
// retries then end the lookup as one whose search keploy did not see
// answered everywhere, not as one never searched. So too a learned name held
// while its search runs, and one whose search keploy answered a step of
// itself.
func TestARetryAfterAnUnansweredStepSaysTheSearchWasUnseen(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	h.ask(nx("nope.example.invalid."))
	h.l.Unanswered(h.question(nx("nope.example.invalid.corp.example.com."))) // asked, no answer came
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid."))
	want := []string{infoLine("nope.example.invalid.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}

	h = newHarness(t, Config{Search: []string{"a.example", "b.example", "c.example"}, Ndots: 1})
	h.ask(nx("pay.a.example."), nx("pay.b.example."))           // a walk of pay: pay.a.example. its candidate
	h.ask(nx("pay.a.example."), nx("pay.a.example.a.example.")) // the app's own, searched: held
	h.ask(nx("pay.a.example."), nx("pay.a.example."))           // the search stops short; the app asks again, twice
	want = []string{infoLine("pay.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("a learned name logged %q at INFO; want %q", got, want)
	}

	h = newHarness(t, Config{Search: twoCorps, Ndots: 1})
	h.ask(nx("pay.a.example."), nx("pay.b.example.")) // a walk of pay: pay.b.example. routine
	h.ask(nx("pay.b.example."))
	syn := sf("pay.b.example.a.example.").answer()
	syn.Synthetic = true
	h.l.Note(h.logger, h.cfg, h.question(sf("pay.b.example.a.example.")), syn) // keploy answered this step itself
	h.ask(nx("pay.b.example.b.example."))
	want = []string{infoLine("pay.b.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("a learned name whose search keploy answered a step of logged %q at INFO; want %q", got, want)
	}
}

// Two overlapping lookups, each between a different pair of the first one's
// steps, are no retry: each new step of the walk clears the re-ask before it.
func TestOverlappingLookupsInDifferentGapsAreNoRetry(t *testing.T) {
	h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
	h.ask(nx("svc.prod."))           // lookup 1, as given: held
	h.ask(nx("svc.prod."))           // lookup 2, before lookup 1's first step
	h.ask(nx("svc.prod.a.example.")) // lookup 1's first step
	h.ask(nx("svc.prod."))           // lookup 3, before its second
	h.ask(ok("svc.prod.b.example.")) // lookup 1 finds it
	h.stop()
	if got := h.info(); len(got) != 0 {
		t.Fatalf("overlapping lookups of a name its search found logged %q at INFO", got)
	}
}

// A held lookup said when the set of lookups logged is full is counted as
// untracked, its answers held back with it.
func TestAHeldLookupWithNoRoomIsCountedUntracked(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	first, second := h.question(nx("held.example.invalid.")), h.question(nx("held.example.invalid."))
	h.answer(first, nx("held.example.invalid."))
	h.answer(second, nx("held.example.invalid.")) // a concurrent duplicate, held back
	for i := 0; i < TrackMax; i++ {               // the set fills
		h.ask(nx("req-" + strconv.Itoa(i) + "."))
	}
	h.stop()
	c := h.countLines()
	if len(c) != 1 || c[0].ContextMap()["untracked_answers"] != uint64(2) || c[0].ContextMap()["repeats"] != uint64(0) {
		t.Fatalf("the stop counted %+v; want the held answer and its duplicate untracked", c)
	}
}

// A Ticket from the last recording, given back in this one, counts nothing
// out of this one's flight.
func TestATicketFromTheLastRecordingCountsNothingOut(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	old := h.question(nx("nope.example.invalid."))
	h.l.Reset()
	inFlight := h.question(nx("nope.example.invalid."))                            // this recording's, still in flight
	h.answer(h.question(nx("nope.example.invalid.")), nx("nope.example.invalid.")) // held
	h.answer(old, nx("nope.example.invalid."))
	h.ask(nx("nope.example.invalid."), nx("nope.example.invalid."))
	if got := h.info(); len(got) != 0 {
		t.Fatalf("logged %q at INFO while a question of the name was in flight", got)
	}
	h.answer(inFlight, nx("nope.example.invalid."))
}

// The answer to a question is in the scope it was asked in, whatever the
// Answer says: within the in-flight bound and past it.
func TestAnAnswerIsInItsQuestionsScope(t *testing.T) {
	for _, past := range []bool{false, true} {
		h := newHarness(t, Config{Search: corp, Ndots: 1})
		if past {
			for i := 0; i < inflightMax; i++ { // questions never answered fill the bound
				h.l.Ask(h.logger, h.cfg, "pod-z", nx("req-"+strconv.Itoa(i)+".").answer().Question)
			}
		}
		q := nx("svc.prod.").answer()
		h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, "pod-a", q.Question), q) // its Scope left empty
		found := ok("svc.prod.corp.example.com.").answer()
		found.Scope = "pod-a" // this one says so
		h.l.Note(h.logger, h.cfg, h.l.Ask(h.logger, h.cfg, "pod-a", found.Question), found)
		h.stop()
		if got := h.info(); len(got) != 0 {
			t.Fatalf("past the bound %v: logged %q at INFO; want pod-a's lookup found by pod-a's search", past, got)
		}
	}
}

// With no room left to hold, a learned name whose search is under way is
// said at once, as one whose search keploy did not see answered everywhere.
func TestALearnedNameWithNoRoomToHoldIsSaidAtOnce(t *testing.T) {
	h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
	h.ask(nx("pay.a.example."), nx("pay.b.example.")) // a walk of pay: pay.a.example. its candidate
	for i := 0; i < TrackMax; i++ {                   // the held set fills
		h.ask(nx("req-" + strconv.Itoa(i) + ".tenant.example."))
	}
	h.ask(nx("pay.a.example."), nx("pay.a.example.a.example."))
	got := h.info()
	want := infoLine("pay.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)
	found := false
	for _, g := range got {
		found = found || g == want
	}
	if !found {
		t.Fatalf("logged %d INFO lines, none %q", len(got), want)
	}
}

// A learned name held while its search runs remembers a step keploy answered
// itself before it was learned: the search failing everywhere is then no
// resolver's word, and the line says keploy did not see it answered.
func TestALearnedHoldRemembersKeploysOwnStep(t *testing.T) {
	h := newHarness(t, Config{Search: []string{"a.example", "b.example", "c.example"}, Ndots: 1})
	h.ask(nx("pay.a.example."), nx("pay.b.example.")) // a walk of pay: pay.a.example. its candidate
	h.ask(nx("pay.a.example."))
	syn := sf("pay.a.example.b.example.").answer()
	syn.Synthetic = true
	h.l.Note(h.logger, h.cfg, h.question(sf("pay.a.example.b.example.")), syn) // keploy answered this step itself
	h.ask(nx("pay.a.example.a.example."))                                      // the redundant step: learned, held
	h.ask(nx("pay.a.example.c.example."))                                      // the last step
	want := []string{infoLine("pay.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchUnseen)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}
}

// What the last recording remembered is no evidence of this one's: a name
// that failed then and resolves now is not said, and the evidence of the last
// recording's walks does not make this one's lookup a candidate.
func TestTheLastRecordingsAnswersAreNotThisOnes(t *testing.T) {
	t.Run("a failure remembered from the last recording", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		walk := func(last step) {
			h.ask(nx("mongo.prod.svc.cluster.local.default.svc.cluster.local."), nx("mongo.prod.svc.cluster.local.svc.cluster.local."),
				nx("mongo.prod.svc.cluster.local.cluster.local."), last)
		}
		for _, stopped := range []bool{true, false} { // with the last recording's stop, and without it
			walk(nx("mongo.prod.svc.cluster.local.")) // the Service has no endpoints yet
			if stopped {
				h.stop()
			}
			before := len(h.info())
			h.l.Reset()
			walk(ok("mongo.prod.svc.cluster.local.")) // the next recording: it resolves
			h.stop()
			if got := h.info()[before:]; len(got) != 0 {
				t.Fatalf("stopped %v: the next recording, whose walk resolved, logged %q at INFO", stopped, got)
			}
			h.l.Reset()
		}
	})
	t.Run("a step remembered from the last recording", func(t *testing.T) {
		h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
		h.ask(nx("pay.b.example.a.example."))             // a step of a search of pay.b.example., remembered
		h.l.Reset()                                       // no stop between
		h.ask(nx("pay.a.example."), nx("pay.b.example.")) // a walk of pay: pay.b.example. routine
		h.ask(nx("pay.b.example.b.example."))             // the app's own, searched: learned, its first step unseen here
		if got := h.info(); len(got) != 0 {
			t.Fatalf("logged %q at INFO, its search said failed from the last recording's step", got)
		}
		h.ask(nx("pay.b.example.a.example.")) // that step, in this recording
		want := []string{infoLine("pay.b.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}
		if got := h.info(); !equal(got, want) {
			t.Fatalf("logged %q at INFO; want %q", got, want)
		}
	})
	t.Run("a walk's evidence remembered from the last recording", func(t *testing.T) {
		h := newHarness(t, Config{Search: twoCorps, Ndots: 1})
		h.ask(nx("pay.a.example."), nx("pay.b.example."), nx("pay.")) // a walk of pay, failing
		h.stop()
		h.l.Reset()
		before := len(h.info())
		h.ask(nx("pay.b.example."), nx("pay.b.example."), nx("pay.b.example.")) // musl: the app's own, asked again, twice
		want := []string{infoLine("pay.b.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchSkipped)}
		if got := h.info()[before:]; !equal(got, want) {
			t.Fatalf("the next recording logged %q at INFO; want %q", got, want)
		}
	})
}

// A held lookup carried into the next recording does not bring the last
// one's answers to its count.
func TestACarriedOverHoldBringsNoCount(t *testing.T) {
	h := newHarness(t, Config{Search: corp, Ndots: 1})
	first, second := h.question(nx("nope.example.invalid.")), h.question(nx("nope.example.invalid."))
	h.answer(first, nx("nope.example.invalid."))
	h.answer(second, nx("nope.example.invalid.")) // held back, in the last recording
	h.l.Reset()
	h.ask(nx("nope.example.invalid.corp.example.com.")) // the walk ends in this one
	h.stop()
	if c := h.countLines(); len(c) != 0 {
		t.Fatalf("the stop counted %+v; want nothing (the held-back answer was the last recording's)", c)
	}
}

// A learned name whose only search domains a resolver does not ask (too long
// beside it) or saw fail is said at once: its search is over.
func TestALearnedNameWhoseSearchIsOverIsSaidAtOnce(t *testing.T) {
	longDomain := strings.Repeat("e", 30) + ".example"                                                                                 // 38 characters
	x := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 26) + "." // 210
	g := x + "a.example."                                                                                                              // 220 with its dot: g.a.example fits, g in the long domain does not
	h := newHarness(t, Config{Search: []string{"a.example", longDomain}, Ndots: 4})
	h.ask(nx(g), nx(x+longDomain+".")) // a walk of x: g its first candidate
	h.ask(nx(g), nx(g+"a.example."))   // the app's own, searched in the one domain that fits
	want := []string{infoLine(g, "A", "NXDOMAIN", NextStepNoSuchName+NextStepSearchFailed)}
	if got := h.info(); !equal(got, want) {
		t.Fatalf("logged %q at INFO; want %q", got, want)
	}
}

// A walk keploy joins partway (it began before the recording noted it, or the
// events of its earlier steps were lost) shows a candidate with no step
// before it: it is no lookup of the app's, and the rest of the walk says so.
// Nor is a candidate the app's own because a walk of it went on unseen once:
// the next walk's steps tell.
func TestAWalkJoinedPartwayIsNoLookupOfTheApps(t *testing.T) {
	walkOfDB := []step{nx("db.a.example."), nx("db.b.example."), ok("db.")}
	walkOfSvc := []step{nx("svc.prod."), nx("svc.prod.a.example."), ok("svc.prod.b.example.")}
	for _, tc := range []struct {
		name  string
		cfg   Config
		steps [][]step // the walks, the first joined partway
		info  []string
	}{
		{"a pod, ndots:5: an external name's walk joined at its second candidate, then resolved: no INFO",
			Config{Search: k8s, Ndots: 5}, [][]step{{nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com.")}},
			nil},
		{"a pod, ndots:5: a Service's walk joined at its second candidate, then resolved: no INFO",
			Config{Search: k8s, Ndots: 5}, [][]step{{nx("mongo.svc.cluster.local."), nx("mongo.cluster.local."), ok("mongo.")}},
			nil},
		{"the same, failing as given too: INFO for the name as given only",
			Config{Search: k8s, Ndots: 5}, [][]step{{nx("mongo.svc.cluster.local."), nx("mongo.cluster.local."), nx("mongo.")}},
			[]string{infoLine("mongo.", "A", "NXDOMAIN", NextStepDotlessK8s)}},
		{"a host, two search domains: a compose name's walk joined at its second candidate, then two whole walks: no INFO",
			Config{Search: twoCorps, Ndots: 1}, [][]step{{nx("db.b.example."), ok("db.")}, walkOfDB, walkOfDB},
			nil},
		{"a host, two search domains: a dotted name's search joined at its first domain, then two whole searches: no INFO",
			Config{Search: twoCorps, Ndots: 1}, [][]step{{nx("svc.prod.a.example."), ok("svc.prod.b.example.")}, walkOfSvc, walkOfSvc},
			nil},
		{"a host, two search domains: a walk stopping after its second candidate: no INFO, both are its steps",
			Config{Search: twoCorps, Ndots: 1}, [][]step{{nx("db.a.example."), nx("db.b.example.")}},
			nil},
		{"a walk seen at its second candidate, then the next walk's first, the rest unseen: the first walk's step was a candidate",
			Config{Search: twoCorps, Ndots: 1}, [][]step{{nx("db.b.example.")}, {nx("db.a.example.")}},
			[]string{infoLine("db.a.example.", "A", "NXDOMAIN", NextStepNoSuchName+NextStepWalkUnseen)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg)
			for _, w := range tc.steps {
				h.ask(w...)
			}
			h.stop()
			if got := h.info(); !equal(got, tc.info) {
				t.Fatalf("the walks %v logged at INFO\n  %q\nwant\n  %q", tc.steps, got, tc.info)
			}
		})
	}
}

// A walk under way across a recording's stop and the next one's start: the
// next recording sees it partway, and its steps say it resolved.
func TestAWalkAcrossAStopAndAStartIsNoLookupOfTheNextRecordings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before step
		after  []step
	}{
		{"an external name (its candidates' dots reach ndots)", nx("api.example.com.default.svc.cluster.local."),
			[]step{nx("api.example.com.svc.cluster.local."), nx("api.example.com.cluster.local."), ok("api.example.com.")}},
		{"a Service (its later candidates' dots below ndots)", nx("mongo.default.svc.cluster.local."),
			[]step{nx("mongo.svc.cluster.local."), nx("mongo.cluster.local."), ok("mongo.")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{Search: k8s, Ndots: 5})
			h.ask(tc.before)
			h.stop()
			h.l.Reset()
			before := len(h.info())
			h.ask(tc.after...)
			h.stop()
			if got := h.info()[before:]; len(got) != 0 {
				t.Fatalf("the next recording, which saw the walk end resolved, logged %q at INFO", got)
			}
		})
	}
}

// The stop is a place among the questions: an answer noted after it, before
// the next question, is evidence for the next recording's walks; those
// noted before it are not. That holds for a caller that notes with the zero
// Ticket only, which never asks.
func TestTheStopIsAPlaceAmongTheQuestions(t *testing.T) {
	seen := func(l *Log, name string) bool {
		_, ok := l.seen.Peek(key("", name, dns.TypeA))
		return ok
	}
	t.Run("an answer after the stop is kept, one before it forgotten", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		h.ask(nx("redis.default.svc.cluster.local."))
		late := h.question(nx("mongo.default.svc.cluster.local."))
		h.stop()
		h.answer(late, nx("mongo.default.svc.cluster.local.")) // after the stop, before the next question
		h.l.Reset()
		if seen(h.l, "redis.default.svc.cluster.local.") || !seen(h.l, "mongo.default.svc.cluster.local.") {
			t.Fatalf("after the reset the Log remembers redis %v, mongo %v; want only mongo, answered after the stop",
				seen(h.l, "redis.default.svc.cluster.local."), seen(h.l, "mongo.default.svc.cluster.local."))
		}
	})
	t.Run("the zero Ticket", func(t *testing.T) {
		h := newHarness(t, Config{Search: k8s, Ndots: 5})
		h.l.Note(h.logger, h.cfg, Ticket{}, nx("redis.default.svc.cluster.local.").answer())
		h.stop()
		h.l.Reset()
		if seen(h.l, "redis.default.svc.cluster.local.") {
			t.Fatal("after the stop and the reset the Log remembers an answer noted before the stop")
		}
	})
}
