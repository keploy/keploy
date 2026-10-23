package proxy

import (
	"strconv"
	"strings"
	"sync"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const (
	// negativeAnswerLogMax bounds the lookups a recording gives an INFO line
	// of their own for an answer that did not resolve them, which keploy
	// passed on to the app (noteNegativeAnswer): a client resolving
	// per-request or user-supplied hostnames would otherwise flood the log
	// for as long as the recording runs. The lookups past it get a DEBUG line
	// each, and their count is logged when the recording stops
	// (logNegativeAnswers).
	negativeAnswerLogMax = 32
	// negativeAnswerTrackMax bounds every set of the app's names this keeps:
	// the lookups logged (at INFO, and past negativeAnswerLogMax at DEBUG),
	// the routine ones (routineNegativeReason), and the names keploy learns
	// the app asked as given (learnAppAskedName). Past it the answers are
	// only counted.
	negativeAnswerTrackMax = 1024
)

// The next step a line about a lookup that did not resolve gives, by what the
// resolver answered and, for a name with no dot, where keploy runs. A name
// with no dot is most often a container's, a compose service's or a
// Kubernetes Service's, which only that network's DNS resolves.
const (
	nextStepNoSuchName    = "the resolver says no such name exists: check the host name the app is configured with"
	nextStepResolverError = "the resolver answered but could not or would not resolve this name: check that it serves recursive queries from where keploy runs, and that it can resolve this name's zone"
	nextStepDotlessNative = "the name has no dot: it is likely a container or compose service name, which only that Docker network's DNS resolves; run the app on that network, or give it a name or address this host resolves (localhost, for a port a container publishes)"
	nextStepDotlessDocker = "the name has no dot: it is likely a container or compose service name; check the name, and that keploy's agent runs on the app's Docker network, whose DNS resolves it"
	nextStepDotlessK8s    = "the name has no dot: no Service of that name answers in the pod's search domains; check the Service's name and namespace"
	// nextStepSearchMayFind ends the next step for a name a resolver asks as
	// given before it tries its search domains (asksAsGivenFirst): the lookup
	// may yet resolve in one of them.
	nextStepSearchMayFind = "; if the app's resolver went on to find it in one of its search domains, this answer was only a step of that search"
	// nextStepSeeDebug is for the lines that count lookups not logged at INFO,
	// and nextStepUntracked adds what --debug does not show.
	nextStepSeeDebug  = "run keploy record with --debug to see each lookup past the limit"
	nextStepUntracked = "; the untracked_answers are to lookups past what keploy tracks, which no level logs"
)

// negativeAnswers is what a recording has logged of the DNS answers that did
// not resolve the app's lookups and that keploy passed on to the app, and the
// lines it held back. Its zero value is ready; ResetRecordedDNSMocks empties
// it for each recording.
type negativeAnswers struct {
	mu sync.Mutex
	// named holds the app's own lookups: the first negativeAnswerLogMax
	// logged at INFO, the rest at DEBUG.
	named onceSet
	// routine holds the lookups whose answer is routine, logged at DEBUG.
	routine onceSet
	// asked holds the names (lower case) keploy learned the app asked as
	// given, though they lie in a search domain (learnAppAskedName).
	asked map[string]struct{}
	// reportedRepeats, reportedDebug and reportedUntracked are the counts
	// logNegativeAnswers last logged.
	reportedRepeats, reportedDebug, reportedUntracked uint64
}

func (n *negativeAnswers) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.named, n.routine, n.asked = onceSet{}, onceSet{}, nil
	n.reportedRepeats, n.reportedDebug, n.reportedUntracked = 0, 0, 0
}

// markAsked records that the app asked for name as given; a full set learns
// no more.
func (n *negativeAnswers) markAsked(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.asked) >= negativeAnswerTrackMax {
		return
	}
	if n.asked == nil {
		n.asked = make(map[string]struct{})
	}
	n.asked[strings.ToLower(name)] = struct{}{}
}

func (n *negativeAnswers) wasAsked(name string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.asked[strings.ToLower(name)]
	return ok
}

// onceSet holds up to a bound of keys, so that a line is said once per key,
// and counts the lines it holds back: the repeats of a key it holds, and the
// keys that came once it was full (which it never holds, so their repeats
// count there too). Its zero value holds nothing.
type onceSet struct {
	seen    map[string]struct{}
	repeats uint64
	pastMax uint64
}

// add reports whether key's line is to be said: key is new, and the set,
// bounded by bound, has room for it. count says whether a line held back
// counts: it does for an answer the app got, not for one said again in
// another light (learnAppAskedName).
func (s *onceSet) add(key string, bound int, count bool) bool {
	if _, ok := s.seen[key]; ok {
		if count {
			s.repeats++
		}
		return false
	}
	if len(s.seen) >= bound {
		if count {
			s.pastMax++
		}
		return false
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	s.seen[key] = struct{}{}
	return true
}

// noteNegativeAnswer logs, while recording, that keploy passed on to the app,
// unchanged, an answer from the upstream resolver that did not resolve the
// app's question (NXDOMAIN, SERVFAIL, REFUSED, ...): once a recording for
// each (name, qtype, answer), whether the answer came from the resolver just
// now or from keploy's DNS cache, at INFO with a next step (at DEBUG past the
// first negativeAnswerLogMax lookups). A routine one (routineNegativeReason)
// gets a DEBUG line instead. The lines held back are counted
// (logNegativeAnswers).
//
// The relay used to be silent. An app run natively that dials a name only a
// Docker network resolves (a compose service's, such as "mongoDb") got
// NXDOMAIN from the host's resolver through keploy, its database driver timed
// out 30 s later, and nothing in keploy's output said keploy had seen the
// lookup at all. Recording keeps no mock of such an answer (recordDNSMock),
// so the log is its only trace.
//
// The line names the query, its type, the answer and the resolver's address:
// keploy's own words, the app's DNS name and the host's resolver
// configuration, and no other customer content.
func (p *Proxy) noteNegativeAnswer(question dns.Question, resp dnsCacheEntry) {
	if resp.Msg == nil || !resp.FromUpstream || resp.Rcode == dns.RcodeSuccess {
		return
	}
	if resp.Rcode == dns.RcodeNameError {
		p.learnAppAskedName(question)
	}
	p.sayNegativeAnswer(question, resp, false)
}

// sayNegativeAnswer is noteNegativeAnswer's line. learned says the line is one
// learnAppAskedName says again in another light: it counts nothing held back
// (onceSet.add's count), and its search is known to have failed too.
//
// A lookup keeps the class its first answer gave it, routine or the app's,
// for the recording: its repeats come from keploy's DNS cache, and the
// evidence that made a candidate routine (madeByResolver) may have expired
// from it between two steps of a later walk, which would turn a healthy
// run's candidate into an INFO line. Only learning that the app asked the
// name as given (learnAppAskedName) moves a lookup from routine to the app's.
func (p *Proxy) sayNegativeAnswer(question dns.Question, resp dnsCacheEntry, learned bool) {
	routine := p.routineNegativeReason(question, resp.Rcode)
	key := generateCacheKey(strings.ToLower(question.Name), question.Qtype) + "-" + rcodeString(resp.Rcode)

	n := &p.negativeAnswers
	n.mu.Lock()
	_, wasNamed := n.named.seen[key]
	_, wasRoutine := n.routine.seen[key]
	_, asked := n.asked[strings.ToLower(question.Name)]
	isRoutine := routine != ""
	switch {
	case wasNamed:
		isRoutine = false
	case wasRoutine && !asked:
		isRoutine = true
	}
	var say bool
	if isRoutine {
		say = n.routine.add(key, negativeAnswerTrackMax, !learned)
	} else {
		say = n.named.add(key, negativeAnswerTrackMax, !learned)
	}
	held := len(n.named.seen)
	n.mu.Unlock()
	if !say {
		return
	}
	level := zap.InfoLevel
	if !isRoutine && held > negativeAnswerLogMax {
		level = zap.DebugLevel
		if held == negativeAnswerLogMax+1 {
			p.logger.Info("keploy has logged as many unresolved DNS lookups at INFO as a recording does; it logs the rest at DEBUG, and their count when the recording stops",
				zap.Int("limit", negativeAnswerLogMax), zap.String("next_step", nextStepSeeDebug))
		}
	}
	fields := []zap.Field{
		zap.String("query", question.Name),
		zap.String("qtype", dnsTypeString(question.Qtype)),
		zap.String("answer", rcodeString(resp.Rcode)),
	}
	if resp.upstream != "" {
		fields = append(fields, zap.String("upstream", resp.upstream))
	}
	if isRoutine {
		p.logger.Debug("the upstream DNS resolver did not resolve a routine lookup, and keploy passed its answer on to the app unchanged",
			append(fields, zap.String("routine", routine))...)
		return
	}
	p.logger.Log(level, "the upstream DNS resolver did not resolve the app's lookup, and keploy passed its answer on to the app unchanged",
		append(fields, zap.String("next_step", p.negativeAnswerNextStep(question.Name, resp.Rcode, learned)))...)
}

// learnAppAskedName learns, from a redundant search-list expansion N.S that
// got NXDOMAIN, that the app asked for N itself: a resolver appends each
// search domain to the name it was given, and N already ends in S, so N is
// that name, not a candidate, though it lies in a search domain (Kubernetes'
// mongodb.default.svc.cluster.local, or payments.corp.example.com on a host
// that searches corp.example.com). Its negative answers are then the
// lookup's outcome, logged at INFO.
//
// The order depends on the resolver's ndots. With N's dots below it, the
// candidates come first, and N's answer, after this one, finds N learned.
// With N's dots at or above it, N is asked first, and its answer, taken then
// for a candidate's, came before this one: the resolver only goes on to N.S
// once N failed, so that answer is still in keploy's DNS cache, and is logged
// now, without counting it again.
func (p *Proxy) learnAppAskedName(question dns.Question) {
	base, ok := p.redundantExpansionBase(question.Name)
	if !ok {
		return
	}
	p.negativeAnswers.markAsked(base)
	if p.dnsCache == nil {
		return
	}
	if prior, ok := p.dnsCache.Peek(generateCacheKey(base, question.Qtype)); ok && prior.Msg != nil && prior.FromUpstream && prior.Rcode != dns.RcodeSuccess {
		p.sayNegativeAnswer(dns.Question{Name: base, Qtype: question.Qtype, Qclass: question.Qclass}, prior, true)
	}
}

// logNegativeAnswers logs, as a recording stops, how many unresolved DNS
// answers keploy passed on to the app without an INFO line of their own:
// repeats of a lookup it logged; the lookups past negativeAnswerLogMax, each
// logged at DEBUG; and the answers to lookups past negativeAnswerTrackMax,
// only counted. At INFO, with how to see them, when lookups the user has not
// seen at INFO came since the count was last logged; at DEBUG when only
// repeats or answers it does not track did. It says nothing when nothing
// moved.
func (p *Proxy) logNegativeAnswers() {
	n := &p.negativeAnswers
	n.mu.Lock()
	logged := min(len(n.named.seen), negativeAnswerLogMax)
	repeats := n.named.repeats
	debug := uint64(len(n.named.seen) - logged)
	untracked := n.named.pastMax
	debugMoved := debug > n.reportedDebug
	moved := debugMoved || repeats > n.reportedRepeats || untracked > n.reportedUntracked
	n.reportedRepeats, n.reportedDebug, n.reportedUntracked = repeats, debug, untracked
	n.mu.Unlock()
	if !moved {
		return
	}
	fields := []zap.Field{
		zap.Int("lookups_logged", logged),
		zap.Uint64("repeats", repeats),
		zap.Uint64("debug_lookups", debug),
		zap.Uint64("untracked_answers", untracked),
		zap.Int("limit", negativeAnswerLogMax),
	}
	level := zap.DebugLevel
	if debugMoved {
		level = zap.InfoLevel
		next := nextStepSeeDebug
		if untracked > 0 {
			next += nextStepUntracked
		}
		fields = append(fields, zap.String("next_step", next))
	}
	p.logger.Log(level, "unresolved DNS answers keploy passed on to the app without an INFO line of their own: repeats of the lookups logged, lookups past the limit logged at DEBUG, and answers to lookups past what keploy tracks, only counted", fields...)
}

// routineNegativeReason says why an answer with rcode to q is routine, and ""
// when it is not: when it is the app's lookup failing. Only an NXDOMAIN is
// routine, but for the lookups that are never a step of the app's own (a
// reverse lookup, gRPC's probe). A SERVFAIL or a REFUSED says the resolver
// failed, and it ends the lookup in some resolvers (musl's getaddrinfo stops
// at one for a search-list candidate), so it is the lookup's outcome.
//
//   - A search-list candidate (isSearchListCandidate): the name a resolver
//     makes of the one it was given and one of its search domains. After a
//     candidate's NXDOMAIN a resolver goes on to the next, and to the name as
//     given, so the candidates' NXDOMAINs are its walk, not the lookup's
//     outcome. On a host whose resolv.conf searches corp.example.com, a
//     compose service's name ("db") is asked as db.corp.example.com. first,
//     which NXDOMAINs, and then as db., which Docker's DNS resolves: an INFO
//     line about the first would read as a failure in a run that has none. A
//     name the app asked as given that looks like a candidate is no
//     candidate once keploy has learned so (learnAppAskedName). A name in
//     the first search domain whose remainder has fewer dots than ndots can
//     be either, and keploy cannot learn which when the resolver tries no
//     candidate after it (musl, which never searches a name whose dots reach
//     ndots): such a lookup is logged at DEBUG only, as payments.corp.example.com
//     is on a host with ndots:1 that searches corp.example.com, or
//     mongo-0.mongo.default.svc.cluster.local from a pod in default.
//   - A reverse lookup (in-addr.arpa, ip6.arpa), whatever the answer: a
//     private address has no PTR record, resolvers may SERVFAIL private
//     reverse zones, and Java's getHostName, Python's getfqdn and SMTP
//     clients ask for one in healthy runs; it is never a step of the app's
//     own lookup.
//   - gRPC's service-config probe (_grpc_config.<target>, TXT), whatever the
//     answer: its absence is grpc-go's normal answer ("no service config, use
//     the defaults"), and grpc-go ignores the probe's errors.
func (p *Proxy) routineNegativeReason(q dns.Question, rcode int) string {
	if isReverseLookup(q.Name) {
		return "a reverse lookup (PTR), which is never a step of the app's own lookup"
	}
	if isGrpcServiceConfigProbe(q) {
		return "gRPC's service-config probe, which grpc-go makes on every target and whose failure it ignores"
	}
	if rcode != dns.RcodeNameError {
		return ""
	}
	if p.isSearchListCandidate(q) && !p.negativeAnswers.wasAsked(q.Name) {
		return "a search-list candidate: a name in one of the resolver's search domains, which the resolver tries besides the name as given"
	}
	return ""
}

// negativeAnswerNextStep is what the user can do about a lookup of name that
// got rcode. A name with no dot gets the hint that it is likely a container's,
// a compose service's or a Kubernetes Service's whatever the answer: a
// resolver may as well SERVFAIL or REFUSE a single-label name as NXDOMAIN it.
// An NXDOMAIN for a name the resolver asks as given before its search domains
// adds that the search may yet find it, unless learned says the search failed
// too.
func (p *Proxy) negativeAnswerNextStep(name string, rcode int, learned bool) string {
	var next string
	switch {
	case isDotlessName(name) && p.searchesKubernetes():
		next = nextStepDotlessK8s
	case isDotlessName(name) && p.IsDocker:
		next = nextStepDotlessDocker
	case isDotlessName(name):
		next = nextStepDotlessNative
	case rcode != dns.RcodeNameError:
		return nextStepResolverError
	default:
		next = nextStepNoSuchName
	}
	if rcode == dns.RcodeNameError && !learned && p.asksAsGivenFirst(name) {
		next += nextStepSearchMayFind
	}
	return next
}

// asksAsGivenFirst reports whether a resolver configured as keploy read
// resolv.conf asks name as given before it tries its search domains, so that
// one of them may yet resolve it: there are search domains, and name's dots
// reach ndots.
func (p *Proxy) asksAsGivenFirst(name string) bool {
	if !p.hasSearchDomains() {
		return false
	}
	return nameDots(name) >= p.dnsNdots
}

// nameDots is how many dots name has, its trailing one aside.
func nameDots(name string) int {
	return strings.Count(strings.TrimSuffix(name, "."), ".")
}

// hasSearchDomains reports whether resolv.conf's search list names a domain
// ("search ." names none).
func (p *Proxy) hasSearchDomains() bool {
	for _, s := range p.dnsSearch {
		if strings.Trim(s, ".") != "" {
			return true
		}
	}
	return false
}

// isSearchListCandidate reports whether q's name is one a resolver made of
// the name it was given, X, and one of the search domains keploy read from
// resolv.conf (p.dnsSearch), S: it is X.S, and what keploy saw before it
// says a resolver walking its list would ask it (madeByResolver). Otherwise
// it is the name the app was given, asked by a resolver that did not search
// it: musl never searches a name whose dots reach ndots (under Docker's
// ndots:0, any name), so Kubernetes' mongo-0.mongo.prod.svc.cluster.local,
// asked from a pod in another namespace, is the app's own.
func (p *Proxy) isSearchListCandidate(q dns.Question) bool {
	fqdn := dns.Fqdn(q.Name)
	lower := strings.ToLower(fqdn)
	if len(lower) != len(fqdn) {
		return false
	}
	var before []string // the search domains a resolver tries before s
	for _, s := range p.dnsSearch {
		s = strings.Trim(s, ".")
		if s == "" {
			continue
		}
		if strings.HasSuffix(lower, "."+strings.ToLower(s)+".") && p.madeByResolver(fqdn[:len(fqdn)-len(s)-1], before, q.Qtype) {
			return true
		}
		before = append(before, s)
	}
	return false
}

// madeByResolver reports whether keploy saw what a resolver given the name
// given asks before it asks given.S, S coming after the search domains
// before, so that given.S is a step of its walk. A resolver asks given first
// when its dots reach ndots, and goes on to the search domains only once it
// did not resolve; else it asks given in each search domain in turn, from
// the first, going on while they do not resolve. Either way keploy's DNS
// cache still holds the answer that sent it on, for the walk takes
// milliseconds: the first search domain's candidate alone needs none.
func (p *Proxy) madeByResolver(given string, before []string, qtype uint16) bool {
	if nameDots(given) >= p.dnsNdots {
		return p.cachedUnresolved(given, qtype)
	}
	if len(before) == 0 {
		return true
	}
	for _, b := range before {
		if p.cachedUnresolved(given+b+".", qtype) {
			return true
		}
	}
	return false
}

// cachedUnresolved reports whether keploy's DNS cache holds an answer from
// the upstream resolver to name's lookup of qtype that did not resolve it: a
// negative answer, or NODATA, past which a resolver walks on too.
func (p *Proxy) cachedUnresolved(name string, qtype uint16) bool {
	if p.dnsCache == nil {
		return false
	}
	// Peek, not Get: a diagnostic read must not change what the cache
	// evicts next.
	e, ok := p.dnsCache.Peek(generateCacheKey(name, qtype))
	return ok && e.Msg != nil && e.FromUpstream && (e.Rcode != dns.RcodeSuccess || len(e.Answer) == 0)
}

// searchesKubernetes reports whether the resolver's search list is a
// Kubernetes pod's: it has the cluster's Service domain, svc.<cluster domain>,
// and the pod's namespace's in it, <namespace>.svc.<cluster domain>. Either
// alone is an ordinary domain's name (svc.example.com).
func (p *Proxy) searchesKubernetes() bool {
	for _, s := range p.dnsSearch {
		s = strings.Trim(strings.ToLower(s), ".")
		if !strings.HasPrefix(s, "svc.") || len(s) == len("svc.") {
			continue
		}
		for _, ns := range p.dnsSearch {
			ns = strings.Trim(strings.ToLower(ns), ".")
			if len(ns) > len(s)+1 && strings.HasSuffix(ns, "."+s) {
				return true
			}
		}
	}
	return false
}

// isReverseLookup reports whether name is a reverse-lookup name (PTR's).
func isReverseLookup(name string) bool {
	q := strings.ToLower(dns.Fqdn(name))
	return strings.HasSuffix(q, ".in-addr.arpa.") || strings.HasSuffix(q, ".ip6.arpa.")
}

// isDotlessName reports whether name is a single label: no dot but its
// trailing one.
func isDotlessName(name string) bool {
	n := strings.TrimSuffix(name, ".")
	return n != "" && !strings.Contains(n, ".")
}

// rcodeString is rcode's name (NXDOMAIN, SERVFAIL, ...), or its number when it
// has none.
func rcodeString(rcode int) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}
	return strconv.Itoa(rcode)
}
