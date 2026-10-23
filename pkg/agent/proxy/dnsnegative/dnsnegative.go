// Package dnsnegative says, once per lookup in a recording, that a DNS answer
// the app got did not resolve its lookup (NXDOMAIN, SERVFAIL, REFUSED, ...),
// with what the user can do about it.
//
// keploy record's DNS relay passes the upstream resolver's answer on to the
// app unchanged, and the enterprise proxyless capture sees the app's resolver
// answer it; neither records a mock of an answer that did not resolve. An app
// run natively that dials a name only a Docker network resolves (a compose
// service's, such as "mongoDb") got NXDOMAIN, its database driver timed out
// 30 s later, and keploy's output had no trace of the lookup. Both paths note
// every answer the app gets in a Log, which logs the line, so they say the
// same thing the same way.
//
// A Log tells the app's own lookups from the steps of its resolver's
// search-list walk by what it saw before and after them, and resolv.conf's
// ndots. A resolver asks a name whose dots reach ndots as given first (glibc's
// and Go's then try it in each search domain; musl's never does), and a name
// with fewer dots in each search domain first and as given last. A name in a
// search domain is a walk's step when keploy saw the step before it. With
// none seen (the walk's first step, or one whose earlier steps keploy did not
// see: it began before keploy noted, or their events were lost), keploy waits
// for the step after it when the name's dots reach ndots, as the app's own
// name may be asked as given first. With fewer, the app's own name is asked
// last, after an expansion that tells keploy so, and the name is a step, with
// a DEBUG line (so is an absolute name, which is asked as given only). A
// lookup whose outcome the rest of a walk decides is held until keploy sees
// it (another step of the walk, the search found the name, or it failed in
// every search domain), the app asks for it again, twice for one qtype, with
// no other question for it in flight and no step of its search in flight, or
// the recording stops: never on a timer. A walk's next step shows as soon as
// its question comes (Ask), before its answer.
//
// A caller tells the Log of each question as it comes (Ask) and gives its
// Ticket back with the answer (Note), or without one (Unanswered): a Ticket
// never given back leaves its name in flight until the next recording, so
// that the app asking for it again is not told from a sibling, and its held
// lines wait for the stop. An answer must be noted before any question the
// app asked after it got that answer is told: a walk's next step told first
// finds nothing held yet, and the lookup it ends is said, at the stop, as if
// its search went unseen. A caller may note only while it records: a walk
// under way when it starts has its earlier steps unseen, and its steps are
// told as above.
// One Log is one recording at a time, across every scope it keeps apart: Stop
// and Reset end and start them all, and they share its bounds.
package dnsnegative

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// LogMax bounds the lookups a recording gives an INFO line of their own:
	// a client resolving per-request or user-supplied hostnames would
	// otherwise flood the log for as long as the recording runs. The lookups
	// past it get a DEBUG line each, and their count is logged when the
	// recording stops (Stop).
	LogMax = 32
	// TrackMax bounds every set of the app's names a Log keeps: the lookups
	// logged (at INFO, and past LogMax at DEBUG), the routine ones, the
	// names it learned the app asked as given, and the lookups it holds.
	// Past it the answers are only counted.
	TrackMax = 1024
	// seenMax bounds the answers a Log remembers as evidence of a walk's
	// earlier steps (Log.seen). A walk takes milliseconds, so only the last
	// few hundred answers matter; the rest only cost memory.
	seenMax = 4096
)

// The next step a line about a lookup that did not resolve gives, by what the
// resolver answered and, for a name with no dot, where keploy sees the lookup
// from. A name with no dot is most often a container's, a compose service's
// or a Kubernetes Service's, which only that network's DNS resolves.
const (
	NextStepNoSuchName    = "the resolver says no such name exists: check the host name the app is configured with"
	NextStepResolverError = "the resolver answered but could not or would not resolve this name: check that it serves recursive queries from where keploy runs, and that it can resolve this name's zone"
	// NextStepResolverErrorObserved is NextStepResolverError for an answer
	// keploy saw the app get, not one it relayed.
	NextStepResolverErrorObserved = "the resolver answered but could not or would not resolve this name: check that it serves recursive queries from where the app runs, and that it can resolve this name's zone"
	NextStepDotlessNative         = "the name has no dot: it is likely a container or compose service name, which only that Docker network's DNS resolves; run the app on that network, or give it a name or address this host resolves (localhost, for a port a container publishes)"
	NextStepDotlessDocker         = "the name has no dot: it is likely a container or compose service name; check the name, and that keploy's agent runs on the app's Docker network, whose DNS resolves it"
	NextStepDotlessContainer      = "the name has no dot: it is likely a container or compose service name, which Docker's DNS answers only on that service's network; check the name, and that the app's container is on that network"
	NextStepDotlessK8s            = "the name has no dot: no Service of that name answers in the pod's search domains; check the Service's name and namespace"
	// NextStepSearchFailed ends the next step of a name the resolver asked
	// as given before its search domains, once keploy saw it try the name in
	// every one of them, and none resolve it.
	NextStepSearchFailed = "; the resolver then asked for it in each of its search domains, and none of them resolved it"
	// NextStepSearchSkipped ends it when keploy saw the app ask for the name
	// again, and again, with its resolver trying it in no search domain in
	// between: musl's never searches a name whose dots reach ndots.
	NextStepSearchSkipped = "; the app asked for it again without its resolver trying it in any of its search domains in between"
	// NextStepSearchUnseen ends it when keploy did not see a resolver answer
	// for the name in every search domain: the recording stopped first, the
	// search stopped short, or keploy answered a step itself, no resolver
	// having answered. glibc's and Go's resolvers search after the name as
	// given, and may find it there.
	NextStepSearchUnseen = "; keploy did not see a resolver's answer for it in each of its search domains, so it may still resolve in one of them"
	// NextStepWalkUnseen ends it for a name in a search domain the recording
	// stopped holding: a step of a walk of the shorter name, or the app's
	// own, asked as given only (musl), keploy saw nothing before or after it
	// to tell.
	NextStepWalkUnseen = "; it lies in one of the resolver's search domains, and keploy saw no other step of a walk with it before the recording stopped, so it may have been only a name the resolver tried for a shorter one"
	// NextStepSeeDebug is for the lines that count lookups not logged at
	// INFO, and NextStepUntracked adds what --debug does not show.
	NextStepSeeDebug  = "run keploy record with --debug to see each lookup past the limit"
	NextStepUntracked = "; the untracked_answers are to lookups past what keploy tracks, which no level logs"
)

// Why a lookup's answer is routine: the routine field of its DEBUG line.
const (
	RoutineCandidate = "a search-list candidate: a name in one of the resolver's search domains, which the resolver tries besides the name as given"
	RoutineFound     = "the name as given, which the resolver then found in one of its search domains"
	RoutineReverse   = "a reverse lookup (PTR), which is never a step of the app's own lookup"
	RoutineGrpcProbe = "gRPC's service-config probe, which grpc-go makes on every target and whose failure it ignores"
)

// The lines' messages. Relayed is keploy record's DNS relay, which passed the
// answer on; observed is a capture of the app's own exchange with its
// resolver.
const (
	MsgRelayed         = "the upstream DNS resolver did not resolve the app's lookup, and keploy passed its answer on to the app unchanged"
	MsgRelayedRoutine  = "the upstream DNS resolver did not resolve a routine lookup, and keploy passed its answer on to the app unchanged"
	MsgObserved        = "the DNS resolver the app asked did not resolve its lookup"
	MsgObservedRoutine = "the DNS resolver the app asked did not resolve a routine lookup"
	MsgLimit           = "keploy has logged as many unresolved DNS lookups at INFO as a recording does; it logs the rest at DEBUG, and their count when the recording stops"
	MsgCount           = "unresolved DNS answers the app got without an INFO line of their own: repeats of the lookups logged, lookups past the limit logged at DEBUG, and answers to lookups past what keploy tracks, only counted"
)

// Where says where keploy sees the app's lookups from: what a name with no
// dot most likely is, and what the user can do about it, depend on it. A
// Kubernetes pod is told by its search list (Config.Search).
type Where uint8

const (
	// Host: the app resolves through this host's resolver configuration.
	Host Where = iota
	// AgentContainer: keploy's agent relays the app's lookups from its own
	// Docker container.
	AgentContainer
	// AppContainer: the app runs in a Docker container and asks Docker's
	// DNS itself, which keploy sees.
	AppContainer
)

// Config is how the app resolves the lookups noted with it: resolv.conf's
// search list and ndots as keploy read them, where keploy sees the lookups
// from, and what the lines say of keploy's part.
type Config struct {
	Search []string
	Ndots  int
	Where  Where
	// Relayed says keploy passed the answers on to the app (its DNS relay);
	// otherwise it only saw them (a capture).
	Relayed bool
	// Fields are added to every line about these lookups (the pod, on a
	// node's agent).
	Fields []zap.Field
}

// Answer is the answer the app got to one question.
type Answer struct {
	Question dns.Question
	Rcode    int
	// Records is how many records its answer section holds.
	Records int
	// Upstream is the resolver that gave it (host:port), "" when unknown.
	Upstream string
	// Synthetic says keploy made the answer itself, no resolver having
	// answered: a step of the app's lookup, but no resolver's word on the
	// name, so it gets no line (where keploy makes it says why).
	Synthetic bool
	// Scope keeps apps that share a Log apart (the pod, on a node's agent):
	// a lookup is once per scope, and a walk's steps are one scope's. Noted
	// with a Ticket from Ask, the answer is in the scope its question was
	// asked in, whatever this says; this is for the zero Ticket.
	Scope string
}

// Ticket is a question the app asked, from Log.Ask: Note is given it with
// the question's answer, or Unanswered when none came. The zero Ticket is a
// question the Log was not told of: its answer is noted, but never taken for
// the app asking again.
type Ticket struct {
	// seq is the question's place among those the Log was told of.
	seq uint64
	// quiet says no other question for the name was in flight when it was
	// asked: it is the start of a lookup of its own, not a sibling (the
	// AAAA beside an A) or another lookup at the same moment.
	quiet bool
	// inflight is the question's key among those in flight (its scope and
	// its name, lower case); "" when it is not counted. gen is the
	// recording it was counted in.
	inflight string
	gen      uint64
	// scope is the scope it was asked in: its answer's, whatever the
	// Answer says.
	scope string
}

// Log is what a recording has said of the answers that did not resolve the
// app's lookups, and the lines it holds back. Its zero value is ready; Reset
// starts each recording afresh.
type Log struct {
	mu sync.Mutex
	// seq counts the questions the Log was told of (Ask).
	seq uint64
	// inflight counts, by scope and name (lower case), the questions asked
	// and not yet answered: a question asked while another for its name is
	// in flight is no lookup of its own. gen is the recording's: Reset starts
	// a new one, so that a Ticket a caller never gave back leaves its name in
	// flight no longer than its recording.
	inflight map[string]int
	gen      uint64
	// stopped says the recording stopped (Stop): what comes after holds
	// nothing, as no stop is left to say it.
	stopped bool
	// named holds the app's own lookups: the first LogMax logged at INFO,
	// the rest at DEBUG.
	named onceSet
	// routine holds the lookups whose answer is routine, logged at DEBUG.
	routine onceSet
	// learned holds the names (scope and lower case) keploy learned the app
	// asked as given, though they lie in a search domain (learn).
	learned map[string]struct{}
	// held holds the lookups whose line waits on the rest of a walk, by
	// name (scope and lower case) and qtype; nheld counts them.
	held  map[string]map[uint16]*heldLookup
	nheld int
	// seen remembers the last answer to each (name, qtype), by key(): what
	// tells a walk's steps from the app's own lookups.
	seen *lru.Cache[string, seenAnswer]
	// stoppedAt is the place the recording's Stop took among the questions
	// (it counts as one in seq): Reset forgets the answers remembered before
	// it, which no walk under way at the next start can be a step after.
	stoppedAt uint64
	// reportedRepeats, reportedDebug and reportedUntracked are the counts
	// Stop last logged.
	reportedRepeats, reportedDebug, reportedUntracked uint64
}

// seenAnswer is an answer remembered, with the recording (gen) and the
// place among the questions (seq) it came at.
type seenAnswer struct {
	Answer
	gen, seq uint64
}

// heldKind is why a lookup is held.
type heldKind uint8

const (
	// asGiven: a name the resolver asked as given before its search
	// domains. Its line waits for the search: found, failed in every search
	// domain, or not seen.
	asGiven heldKind = iota
	// unproven: a name X.S in a search domain S whose dots reach ndots, with
	// no step of a walk of X seen before it. It is a step of that walk when
	// the walk goes on (X in another search domain, or X itself): the first
	// step, which needs none before it, or one whose earlier steps keploy did
	// not see. Or it is the app's own name, asked as given: glibc and Go then
	// search it (asGiven), musl does not.
	unproven
)

type heldLookup struct {
	kind heldKind
	a    Answer
	key  string // the once-set key
	// noted is the Log's seq when the answer was noted: a question asked
	// after it may be the app asking again.
	noted uint64
	// searching says the resolver has asked the name in a search domain;
	// synthetic, that one of the answers its search got was keploy's own.
	searching, synthetic bool
	// rearmed says the app asked for the name, this qtype, once again
	// already, with nothing of it in flight and no new step of its search
	// since: once more is a retry. Once is not: another lookup of the name
	// that starts between two steps of the first one's walk looks the same.
	// A step that tries a search domain not tried before clears it: the walk
	// goes on.
	rearmed bool
	// tried says, per search domain, that the name was asked in it and
	// did not resolve there.
	tried   []bool
	repeats uint64
	r       *resolver
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

// add reports whether key's line is to be said: key is new, and the set has
// room for it. count says whether a line held back counts: it does for an
// answer the app got (one released from being held among them), not for one
// said again in another light (learn).
func (s *onceSet) add(key string, count bool) bool {
	if _, ok := s.seen[key]; ok {
		if count {
			s.repeats++
		}
		return false
	}
	if len(s.seen) >= TrackMax {
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

// heldBack counts n answers to key's lookup held back: repeats when the set
// holds key, answers to a lookup it does not track when it was full.
func (s *onceSet) heldBack(key string, n uint64) {
	if s.has(key) {
		s.repeats += n
	} else {
		s.pastMax += n
	}
}

func (s *onceSet) has(key string) bool {
	_, ok := s.seen[key]
	return ok
}

// resolver is a Config read for use: its search domains lower case, without
// their dots at either end, without "." and repeats.
type resolver struct {
	search  []string
	ndots   int
	where   Where
	relayed bool
	fields  []zap.Field
	k8s     bool
}

func newResolver(c Config) *resolver {
	r := &resolver{ndots: c.Ndots, where: c.Where, relayed: c.Relayed, fields: c.Fields}
	for _, s := range c.Search {
		s = strings.ToLower(strings.Trim(s, "."))
		if s == "" || containsString(r.search, s) {
			continue
		}
		r.search = append(r.search, s)
	}
	r.k8s = searchesKubernetes(r.search)
	return r
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// inflightMax bounds the names whose questions are counted in flight; a
// question past it is asked untracked (the zero Ticket's name).
const inflightMax = TrackMax

// Ask notes, while recording, that the app asked q, as resolver
// configuration cfg makes it, in scope, and returns its Ticket, which Note or
// Unanswered must be given back (its answer is noted in that scope). A
// question is a step of a walk as soon as it comes: x.S2 (S2 another search
// domain), or x itself, shows that x.S1 held as maybe the app's own name was
// the walk's candidate; n.S shows that the resolver searches n, held as asked
// as given, so that its search, not the app asking again, decides its line.
func (l *Log) Ask(logger *zap.Logger, cfg Config, scope string, q dns.Question) Ticket {
	name := strings.ToLower(dns.Fqdn(q.Name))
	k := heldKey(scope, name)
	var out lines
	l.mu.Lock()
	l.seq++
	t := Ticket{seq: l.seq, gen: l.gen, scope: scope}
	switch n := l.inflight[k]; {
	case n > 0:
		l.inflight[k] = n + 1
		t.inflight = k
	case len(l.inflight) < inflightMax:
		if l.inflight == nil {
			l.inflight = make(map[string]int)
		}
		l.inflight[k] = 1
		t.inflight, t.quiet = k, true
	}
	if logger != nil && name != "." && l.nheld > 0 {
		l.question(logger, newResolver(cfg), scope, name, &out)
	}
	l.mu.Unlock()
	out.log()
	return t
}

// Unanswered gives back the Ticket of a question no answer came to. A Ticket
// is given back once: to Note or to Unanswered.
func (l *Log) Unanswered(t Ticket) {
	l.mu.Lock()
	l.answered(t)
	l.mu.Unlock()
}

// answered counts t's question out of flight.
func (l *Log) answered(t Ticket) {
	if t.inflight == "" || t.gen != l.gen {
		return
	}
	switch n := l.inflight[t.inflight]; {
	case n > 1:
		l.inflight[t.inflight] = n - 1
	case n == 1:
		delete(l.inflight, t.inflight)
	}
}

// question ends or advances the held lookups whose walk the question for
// name (lower case) is a step of.
func (l *Log) question(logger *zap.Logger, r *resolver, scope, name string, out *lines) {
	for j, s := range r.search {
		suffix := "." + s + "."
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		given := name[:len(name)-len(suffix)] + "."
		for _, h := range l.held[heldKey(scope, given)] {
			// The resolver searches the name: the app asked it as given.
			h.kind = asGiven
			h.searching = true
		}
		l.walkStep(logger, r, scope, given, j, out)
	}
	l.walkStep(logger, r, scope, name, -1, out)
}

// line is one line a Log says, made under its lock and logged after.
type line struct {
	logger *zap.Logger
	level  zapcore.Level
	msg    string
	fields []zap.Field
}

type lines []line

func (ls lines) log() {
	for _, ln := range ls {
		ln.logger.Log(ln.level, ln.msg, ln.fields...)
	}
}

// Note notes, while recording, the answer the app got to the question t, as
// resolver configuration cfg makes it, in the scope t was asked in (the
// Answer's Scope, for the zero Ticket): logs, once a recording for each
// (scope, name, qtype, answer), an answer that did not resolve the lookup, at
// INFO with a next step (at DEBUG past the first LogMax lookups), or, when it
// is routine, at DEBUG; holds one whose line waits on the rest of a walk; and
// counts the lines it holds back (Stop). An answer that resolved a lookup
// gets no line, but may end a walk a held lookup waits on.
func (l *Log) Note(logger *zap.Logger, cfg Config, t Ticket, a Answer) {
	if t.seq != 0 {
		a.Scope = t.scope // asked in that scope: one source for both
	}
	var out lines
	l.mu.Lock()
	l.answered(t)
	if logger != nil {
		l.note(logger, newResolver(cfg), t, a, &out)
	}
	l.mu.Unlock()
	out.log()
}

func (l *Log) note(logger *zap.Logger, r *resolver, t Ticket, a Answer, out *lines) {
	name := strings.ToLower(dns.Fqdn(a.Question.Name))
	if name == "." {
		return
	}
	// Found: a resolver stops its walk at a name with records of a qtype it
	// asked; NODATA (the name, but no record of this qtype) it walks on from,
	// unless the other qtype it asked has records there.
	found := a.Rcode == dns.RcodeSuccess && a.Records > 0
	// A step of other names' walks, whatever this name's own state.
	l.advance(logger, r, a.Scope, name, a.Question.Qtype, found, a.Synthetic, out)
	// Remembered whoever made it: keploy's own answer, when no resolver
	// answered, is what the app's resolver walked on from too.
	if l.seen == nil {
		l.seen, _ = lru.New[string, seenAnswer](seenMax)
	}
	l.seen.Add(key(a.Scope, name, a.Question.Qtype), seenAnswer{Answer: a, gen: l.gen, seq: l.seq})
	if byType := l.held[heldKey(a.Scope, name)]; byType != nil {
		h := byType[a.Question.Qtype]
		retry := t.quiet && h != nil && t.seq > latestNoted(byType) && !l.stepInFlight(r, a.Scope, name)
		switch {
		case retry && !h.rearmed:
			// The app asked for the name again, with nothing else of it in
			// flight, nor a step of its search. Another lookup of the name,
			// started between two steps of the first one's walk, looks the
			// same: hold on, until this qtype is asked once more. (Per
			// qtype: answered from a cache, the A of a lookup is noted
			// before its AAAA comes, which then looks quiet too.)
			h.rearmed = true
			if a.Rcode == h.a.Rcode {
				h.repeats++
				return
			}
		case retry:
			// Again: keploy saw the lookup end with the answers it holds,
			// every qtype's (the AAAA asked beside the retry's A is no
			// lookup of its own). Another qtype asked after the held one
			// (glibc's single-request) is no retry. A search begun and not
			// seen to its end (glibc stops at a REFUSED step; the app's
			// search list may be shorter than the one keploy read) is
			// unseen; none begun, skipped.
			how := releaseSearchSkipped
			if anySearching(byType) {
				how = releaseSearchUnseen
			}
			for _, h := range sortedHeld(byType) {
				l.release(logger, h, how, out)
			}
		case h != nil && a.Rcode == h.a.Rcode:
			// A sibling of the held answer's question, another lookup at
			// the same moment, or the name again while a step of its search
			// is in flight.
			h.repeats++
			return
		}
	}
	if a.Synthetic || a.Rcode == dns.RcodeSuccess {
		return
	}
	if a.Rcode == dns.RcodeNameError {
		l.learn(logger, r, a, name, out)
	}
	l.negative(logger, r, a, name, out)
}

// key is the key of the answer to (name, qtype) in scope: name lower case.
func key(scope, name string, qtype uint16) string {
	return scope + "\x00" + name + "\x00" + strconv.Itoa(int(qtype))
}

// onceKey is a lookup's once-set key: (scope, name, qtype, answer).
func onceKey(scope, name string, qtype uint16, rcode int) string {
	return key(scope, name, qtype) + "\x00" + strconv.Itoa(rcode)
}

func heldKey(scope, name string) string {
	return scope + "\x00" + name
}

// advance ends the held lookups whose walk this answer, to name (lower case),
// is a step of. name.S, S a search domain, is a step of the search of name
// held as asked as given: the name was found if it has records there, or the
// search failed once every search domain has been tried, for every qtype
// held (a NODATA for the AAAA may come before the A that resolves). x.S2
// (another search domain) or x itself is another step of the walk of x whose
// step x.S1 is held as maybe the app's own name: so x.S1 was a candidate.
func (l *Log) advance(logger *zap.Logger, r *resolver, scope, name string, qtype uint16, found, synthetic bool, out *lines) {
	if l.nheld == 0 {
		return
	}
	for j, s := range r.search {
		suffix := "." + s + "."
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		given := name[:len(name)-len(suffix)] + "."
		if byType := l.held[heldKey(scope, given)]; byType != nil {
			if found {
				// The search found the name: what was held of it was the
				// first step of that search, whatever its qtype.
				for _, h := range sortedHeld(byType) {
					l.release(logger, h, releaseFound, out)
				}
			} else if h := byType[qtype]; h != nil && j < len(h.tried) {
				// The resolver searched after the name: the app asked it as
				// given.
				h.kind = asGiven
				h.searching = true
				h.synthetic = h.synthetic || synthetic
				if !h.tried[j] {
					// The walk went on to a search domain not tried
					// before: a re-ask before it was no retry.
					h.tried[j] = true
					for _, held := range byType {
						held.rearmed = false
					}
				}
				if allTried(byType) {
					// Every step failed; but one keploy answered itself, no
					// resolver having answered, is no resolver's word.
					how := releaseSearchFailed
					for _, h := range byType {
						if h.synthetic {
							how = releaseSearchUnseen
						}
					}
					for _, h := range sortedHeld(byType) {
						l.release(logger, h, how, out)
					}
				}
			}
		}
		l.walkStep(logger, r, scope, given, j, out)
	}
	l.walkStep(logger, r, scope, name, -1, out)
}

// walkStep ends the lookups of x in a search domain (x.S) held as unproven,
// now that another step of a walk of x came: x in the j-th search domain, or
// x itself (j < 0). They were steps of that walk, whatever their qtype: in an
// ordered walk, a step before the one that came, or one of a walk of x that
// went on after keploy last saw it.
func (l *Log) walkStep(logger *zap.Logger, r *resolver, scope, x string, j int, out *lines) {
	if l.nheld == 0 {
		return
	}
	for i, s := range r.search {
		if i == j {
			continue
		}
		byType := l.held[heldKey(scope, x[:len(x)-1]+"."+s+".")]
		for _, h := range sortedHeld(byType) {
			if h.kind == unproven {
				l.release(logger, h, releaseCandidate, out)
			}
		}
	}
}

// stepInFlight reports whether a question for name (lower case) in one of
// the search domains is in flight: a step of its search, whose answer
// decides.
func (l *Log) stepInFlight(r *resolver, scope, name string) bool {
	for _, s := range r.search {
		if l.inflight[heldKey(scope, name[:len(name)-1]+"."+s+".")] > 0 {
			return true
		}
	}
	return false
}

// anySearching reports whether the resolver searches the name byType holds.
func anySearching(byType map[uint16]*heldLookup) bool {
	for _, h := range byType {
		if h.searching {
			return true
		}
	}
	return false
}

// latestNoted is when the last of byType's answers was noted.
func latestNoted(byType map[uint16]*heldLookup) uint64 {
	var n uint64
	for _, h := range byType {
		n = max(n, h.noted)
	}
	return n
}

// allTried reports whether the search of the name byType holds was seen to
// fail in every search domain, for every qtype held.
func allTried(byType map[uint16]*heldLookup) bool {
	for _, h := range byType {
		for _, t := range h.tried {
			if !t {
				return false
			}
		}
	}
	return true
}

// sortedHeld is byType's lookups in the order they were noted.
func sortedHeld(byType map[uint16]*heldLookup) []*heldLookup {
	hs := make([]*heldLookup, 0, len(byType))
	for _, h := range byType {
		hs = append(hs, h)
	}
	sortHeld(hs)
	return hs
}

// sortHeld puts hs in the order their answers were noted, those noted at the
// same count by qtype, so that what is said of them is said in one order.
func sortHeld(hs []*heldLookup) {
	sort.Slice(hs, func(i, j int) bool {
		if hs[i].noted != hs[j].noted {
			return hs[i].noted < hs[j].noted
		}
		if hs[i].a.Scope != hs[j].a.Scope {
			return hs[i].a.Scope < hs[j].a.Scope
		}
		if hs[i].a.Question.Name != hs[j].a.Question.Name {
			return hs[i].a.Question.Name < hs[j].a.Question.Name
		}
		return hs[i].a.Question.Qtype < hs[j].a.Question.Qtype
	})
}

// learn learns, from a redundant search-list expansion N.S that got
// NXDOMAIN, that the app asked for N itself: a resolver appends each search
// domain to the name it was given, and N already ends in S, so N is that
// name, not a candidate, though it lies in a search domain (Kubernetes'
// mongodb.default.svc.cluster.local, or payments.corp.example.com on a host
// that searches corp.example.com). Its negative answers are then the
// lookup's outcome, logged at INFO.
//
// With N's dots below ndots the expansions come before N, and N's answer
// finds N learned. With them at or above it, N came first and is held
// (advance ends it). An answer to N kept routine before (its walk looked
// like another's) is said now, from what keploy saw of it, without counting
// it again: plainly when N is asked as given last, or its search was seen to
// fail in every search domain; else it is held until its search ends.
func (l *Log) learn(logger *zap.Logger, r *resolver, a Answer, name string, out *lines) {
	base, ok := RedundantExpansionBase(r.search, a.Question.Name)
	if !ok {
		return
	}
	lower := strings.ToLower(base)
	if len(l.learned) < TrackMax {
		if l.learned == nil {
			l.learned = make(map[string]struct{})
		}
		l.learned[heldKey(a.Scope, lower)] = struct{}{}
	}
	qtype := a.Question.Qtype
	if l.held[heldKey(a.Scope, lower)][qtype] != nil || l.seen == nil {
		return
	}
	// Only this recording's: an earlier one's answer is not this one's
	// lookup failing.
	remembered, ok := l.seen.Peek(key(a.Scope, lower, qtype))
	if !ok || remembered.gen != l.gen || remembered.Rcode != dns.RcodeNameError {
		return
	}
	prior := remembered.Answer
	k := onceKey(a.Scope, lower, qtype, prior.Rcode)
	if l.named.has(k) {
		return
	}
	prior.Question.Name = base
	tried, synthetic := l.searchTried(r, a.Scope, lower, qtype)
	switch {
	case !r.asksAsGivenFirst(lower):
		// Asked as given last, after its candidates: the lookup ends with it.
		l.sayNamed(logger, r, prior, k, "", false, out)
	case tried && synthetic:
		// Every step failed, but one keploy answered itself.
		l.sayNamed(logger, r, prior, k, NextStepSearchUnseen, false, out)
	case tried:
		l.sayNamed(logger, r, prior, k, NextStepSearchFailed, false, out)
	default:
		// Asked as given first, and its search is under way: hold it until
		// the search ends, as one keploy saw asked as given.
		h := l.hold(r, prior, lower, k, asGiven)
		if h == nil {
			l.sayNamed(logger, r, prior, k, NextStepSearchUnseen, false, out)
			return
		}
		h.searching = true
		for j, s := range r.search {
			if seen, ok := l.seenStep(a.Scope, lower, s, qtype); ok {
				h.tried[j] = true
				h.synthetic = h.synthetic || seen.Synthetic
			}
		}
	}
}

// seenStep is this recording's remembered answer to name (lower case) in
// search domain s, when it did not resolve it.
func (l *Log) seenStep(scope, name, s string, qtype uint16) (seenAnswer, bool) {
	a, ok := l.seen.Peek(key(scope, name[:len(name)-1]+"."+s+".", qtype))
	if !ok || a.gen != l.gen || (a.Rcode == dns.RcodeSuccess && a.Records > 0) {
		return seenAnswer{}, false
	}
	return a, true
}

// searchTried reports whether keploy saw name (lower case) asked, in this
// recording, in every search domain a resolver asks it in, unresolved;
// synthetic, that a step's answer was keploy's own.
func (l *Log) searchTried(r *resolver, scope, name string, qtype uint16) (tried, synthetic bool) {
	for _, s := range r.search {
		if tooLong(name, s) {
			continue
		}
		a, ok := l.seenStep(scope, name, s, qtype)
		if !ok {
			return false, false
		}
		synthetic = synthetic || a.Synthetic
	}
	return true, synthetic
}

func (l *Log) wasLearned(scope, name string) bool {
	_, ok := l.learned[heldKey(scope, name)]
	return ok
}

// negative logs the answer a, which did not resolve the lookup of name (lower
// case): once per lookup, by the class its first answer gave it.
//
// A lookup keeps that class, routine or the app's, for the recording: its
// repeats come from a resolver's cache, or keploy's, and the answer that made
// a candidate routine may have gone from what keploy remembers between two
// steps of a later walk, which would turn a healthy run's candidate into an
// INFO line. Only learning that the app asked the name as given (learn) moves
// a lookup from routine to the app's.
func (l *Log) negative(logger *zap.Logger, r *resolver, a Answer, name string, out *lines) {
	q := a.Question
	k := onceKey(a.Scope, name, q.Qtype, a.Rcode)
	learned := l.wasLearned(a.Scope, name)
	switch {
	case l.named.has(k):
		l.sayNamed(logger, r, a, k, "", true, out)
		return
	case l.routine.has(k) && !learned:
		l.sayRoutine(logger, r, a, k, "", true, out)
		return
	}
	reason, waits := l.routineReason(r, a.Scope, q, name, a.Rcode, learned)
	switch {
	case reason != "" && !waits:
		l.sayRoutine(logger, r, a, k, reason, true, out)
	case reason != "":
		if l.hold(r, a, name, k, unproven) == nil {
			l.sayRoutine(logger, r, a, k, reason, true, out)
		}
	case a.Rcode == dns.RcodeNameError && r.asksAsGivenFirst(name):
		if l.hold(r, a, name, k, asGiven) == nil {
			l.sayNamed(logger, r, a, k, NextStepSearchUnseen, true, out)
		}
	default:
		l.sayNamed(logger, r, a, k, "", true, out)
	}
}

// hold holds the lookup of name (lower case) answered a, whose line waits on
// the rest of a walk, and returns it; nil when it cannot: no room is left, or
// the recording has stopped (no stop is left to say what it holds).
func (l *Log) hold(r *resolver, a Answer, name, k string, kind heldKind) *heldLookup {
	hk := heldKey(a.Scope, name)
	if h := l.held[hk][a.Question.Qtype]; h != nil {
		return h
	}
	if l.nheld >= TrackMax || l.stopped {
		return nil
	}
	h := &heldLookup{kind: kind, a: a, key: k, noted: l.seq, tried: make([]bool, len(r.search)), r: r}
	for j, s := range r.search {
		// A resolver does not ask a name longer than a DNS name can be
		// (Go skips it).
		if tooLong(name, s) {
			h.tried[j] = true
		}
	}
	if l.held == nil {
		l.held = make(map[string]map[uint16]*heldLookup)
	}
	if l.held[hk] == nil {
		l.held[hk] = make(map[uint16]*heldLookup)
	}
	l.held[hk][a.Question.Qtype] = h
	l.nheld++
	return h
}

// How a held lookup ends.
type release uint8

const (
	// releaseFound: the search found the name; the held answer was a step.
	releaseFound release = iota
	// releaseCandidate: another step of a walk of a shorter name came (in
	// another search domain, or the name itself); the held answer was that
	// walk's candidate.
	releaseCandidate
	// releaseSearchFailed: the search failed in every search domain.
	releaseSearchFailed
	// releaseSearchSkipped: the app asked again, and again, with no step of
	// a search between.
	releaseSearchSkipped
	// releaseSearchUnseen: keploy saw no end of the search before the
	// recording stopped (or no room was left to hold it).
	releaseSearchUnseen
	// releaseWalkUnseen: a name in a search domain, neither seen to be a
	// walk's step nor searched, when the recording stopped.
	releaseWalkUnseen
)

func (l *Log) release(logger *zap.Logger, h *heldLookup, how release, out *lines) {
	name := strings.ToLower(dns.Fqdn(h.a.Question.Name))
	hk := heldKey(h.a.Scope, name)
	if byType := l.held[hk]; byType != nil && byType[h.a.Question.Qtype] == h {
		delete(byType, h.a.Question.Qtype)
		if len(byType) == 0 {
			delete(l.held, hk)
		}
		l.nheld--
	}
	switch how {
	case releaseFound:
		l.sayRoutine(logger, h.r, h.a, h.key, RoutineFound, true, out)
		l.routine.heldBack(h.key, h.repeats)
	case releaseCandidate:
		l.sayRoutine(logger, h.r, h.a, h.key, RoutineCandidate, true, out)
		l.routine.heldBack(h.key, h.repeats)
	case releaseSearchFailed, releaseSearchSkipped, releaseSearchUnseen, releaseWalkUnseen:
		suffix := map[release]string{releaseSearchFailed: NextStepSearchFailed, releaseSearchSkipped: NextStepSearchSkipped, releaseSearchUnseen: NextStepSearchUnseen, releaseWalkUnseen: NextStepWalkUnseen}[how]
		l.sayNamed(logger, h.r, h.a, h.key, suffix, true, out)
		l.named.heldBack(h.key, h.repeats)
	}
}

// sayNamed says the line of the app's lookup a, key k, with the next step
// suffix ends: at INFO for the first LogMax lookups, at DEBUG past them (with
// one line saying so). count says whether an answer it does not say counts.
func (l *Log) sayNamed(logger *zap.Logger, r *resolver, a Answer, k, suffix string, count bool, out *lines) {
	if !l.named.add(k, count) {
		return
	}
	level := zapcore.InfoLevel
	if held := len(l.named.seen); held > LogMax {
		level = zapcore.DebugLevel
		if held == LogMax+1 {
			*out = append(*out, line{logger: logger, level: zapcore.InfoLevel, msg: MsgLimit,
				fields: []zap.Field{zap.Int("limit", LogMax), zap.String("next_step", NextStepSeeDebug)}})
		}
	}
	msg := MsgObserved
	if r.relayed {
		msg = MsgRelayed
	}
	fields := append(answerFields(r, a), zap.String("next_step", r.nextStep(a.Question.Name, a.Rcode)+suffix))
	*out = append(*out, line{logger: logger, level: level, msg: msg, fields: fields})
}

// sayRoutine says the DEBUG line of the routine lookup a, key k, routine
// saying why (none, for a repeat).
func (l *Log) sayRoutine(logger *zap.Logger, r *resolver, a Answer, k, routine string, count bool, out *lines) {
	if !l.routine.add(k, count) {
		return
	}
	msg := MsgObservedRoutine
	if r.relayed {
		msg = MsgRelayedRoutine
	}
	fields := append(answerFields(r, a), zap.String("routine", routine))
	*out = append(*out, line{logger: logger, level: zapcore.DebugLevel, msg: msg, fields: fields})
}

// answerFields are the fields every line about a carries: the query as the
// app asked it, its type, the answer and the resolver, and cfg's own. They
// are the app's DNS name, the host's resolver configuration and keploy's own
// words, and no other customer content.
func answerFields(r *resolver, a Answer) []zap.Field {
	fields := make([]zap.Field, 0, 5+len(r.fields))
	fields = append(fields, r.fields...)
	fields = append(fields,
		zap.String("query", a.Question.Name),
		zap.String("qtype", typeString(a.Question.Qtype)),
		zap.String("answer", RcodeString(a.Rcode)),
	)
	if a.Upstream != "" {
		fields = append(fields, zap.String("upstream", a.Upstream))
	}
	return fields
}

// Stop logs, as a recording stops, the lookups it still holds (keploy saw no
// end of their search), and how many unresolved answers the app got without
// an INFO line of their own: repeats of a lookup logged; the lookups past
// LogMax, each logged at DEBUG; and the answers to lookups past TrackMax, only
// counted. The count is at INFO, with how to see them, when lookups the user
// has not seen at INFO came since it was last logged; at DEBUG when only
// repeats or answers it does not track did; and not at all when nothing
// moved.
func (l *Log) Stop(logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	var out lines
	l.mu.Lock()
	l.releaseAll(logger, &out)
	// The stop takes a place of its own among the questions, so that an
	// answer noted after it, before the next question, is placed after it.
	l.seq++
	l.stopped, l.stoppedAt = true, l.seq
	logged := min(len(l.named.seen), LogMax)
	repeats := l.named.repeats
	debug := uint64(len(l.named.seen) - logged)
	untracked := l.named.pastMax
	debugMoved := debug > l.reportedDebug
	moved := debugMoved || repeats > l.reportedRepeats || untracked > l.reportedUntracked
	l.reportedRepeats, l.reportedDebug, l.reportedUntracked = repeats, debug, untracked
	l.mu.Unlock()
	out.log()
	if !moved {
		return
	}
	fields := []zap.Field{
		zap.Int("lookups_logged", logged),
		zap.Uint64("repeats", repeats),
		zap.Uint64("debug_lookups", debug),
		zap.Uint64("untracked_answers", untracked),
		zap.Int("limit", LogMax),
	}
	level := zapcore.DebugLevel
	if debugMoved {
		level = zapcore.InfoLevel
		next := NextStepSeeDebug
		if untracked > 0 {
			next += NextStepUntracked
		}
		fields = append(fields, zap.String("next_step", next))
	}
	logger.Log(level, MsgCount, fields...)
}

// releaseAll ends every held lookup, in the order they were noted: keploy saw
// no end of their search.
func (l *Log) releaseAll(logger *zap.Logger, out *lines) {
	if l.nheld == 0 {
		return
	}
	var hs []*heldLookup
	for _, byType := range l.held {
		for _, h := range byType {
			hs = append(hs, h)
		}
	}
	sortHeld(hs)
	for _, h := range hs {
		how := releaseSearchUnseen
		if h.kind == unproven {
			how = releaseWalkUnseen
		}
		l.release(logger, h, how, out)
	}
}

// Reset starts a new recording: nothing the last recording logged, learned,
// counted or took to be in flight carries over. The lookups still held (a
// walk under way as the recording starts, when no stop came since the last
// one began) carry over, for their walk to decide in this one, and so do the
// answers keploy remembers as evidence of the resolver's walks: those noted
// since the last recording stopped, when it did (a walk under way now began
// after that), else all of them. What is remembered of the last recording is
// no evidence that this one's lookup failed (learn), but it may make a step
// of this one's walks look like a candidate (a DEBUG line, never a false
// INFO one). A step of a walk under way whose earlier steps keploy did not
// see is told as candidate says: held for the walk's later steps when its
// dots reach ndots, else a candidate.
func (l *Log) Reset() {
	l.mu.Lock()
	l.named, l.routine, l.learned = onceSet{}, onceSet{}, nil
	l.reportedRepeats, l.reportedDebug, l.reportedUntracked = 0, 0, 0
	for _, byType := range l.held {
		for _, h := range byType {
			h.repeats = 0 // the last recording's answers, not this one's
		}
	}
	if l.stopped && l.seen != nil {
		for _, k := range l.seen.Keys() {
			if a, ok := l.seen.Peek(k); ok && a.seq < l.stoppedAt {
				l.seen.Remove(k)
			}
		}
	}
	l.gen++
	l.inflight, l.stopped, l.stoppedAt = nil, false, 0
	l.mu.Unlock()
}

// routineReason says why an answer with rcode to q, name lower case, is
// routine, and "" when it is not: when it is the app's lookup failing.
// waits says the reason is only that name lies in a search domain, with no
// step of a walk seen before it, and its dots reach ndots: it is held to tell
// which (unproven).
//
// Only an NXDOMAIN is routine, but for the lookups that are never a step of
// the app's own (a reverse lookup, gRPC's probe). A SERVFAIL or a REFUSED
// says the resolver failed, and it ends the lookup in some resolvers (musl's
// getaddrinfo stops at one for a search-list candidate), so it is the
// lookup's outcome.
//
//   - A search-list candidate (candidate): the name a resolver makes of the
//     one it was given and one of its search domains. After a candidate's
//     NXDOMAIN a resolver goes on to the next, and to the name as given, so
//     the candidates' NXDOMAINs are its walk, not the lookup's outcome. On a
//     host whose resolv.conf searches corp.example.com, a compose service's
//     name ("db") is asked as db.corp.example.com. first, which NXDOMAINs,
//     and then as db., which Docker's DNS resolves: an INFO line about the
//     first would read as a failure in a run that has none. A name the app
//     asked as given that looks like a candidate is no candidate once
//     keploy has learned so (learn).
//   - A reverse lookup (in-addr.arpa, ip6.arpa), whatever the answer: a
//     private address has no PTR record, resolvers may SERVFAIL private
//     reverse zones, and Java's getHostName, Python's getfqdn and SMTP
//     clients ask for one in healthy runs; it is never a step of the app's
//     own lookup.
//   - gRPC's service-config probe (_grpc_config.<target>, TXT), whatever the
//     answer: its absence is grpc-go's normal answer ("no service config, use
//     the defaults"), and grpc-go ignores the probe's errors.
func (l *Log) routineReason(r *resolver, scope string, q dns.Question, name string, rcode int, learned bool) (reason string, waits bool) {
	if isReverseLookup(name) {
		return RoutineReverse, false
	}
	if IsGrpcServiceConfigProbe(q) {
		return RoutineGrpcProbe, false
	}
	if rcode != dns.RcodeNameError || learned {
		return "", false
	}
	if _, redundant := RedundantExpansionBase(r.search, name); redundant {
		// A search domain appended to a name that already ends in it: a
		// resolver's expansion, never a name an app is configured with.
		return RoutineCandidate, false
	}
	cand, unseen := l.candidate(r, scope, q.Qtype, name)
	if !cand {
		return "", false
	}
	// With its dots below ndots, the app's own name in a search domain is
	// asked last, after its expansions in the search domains, one of which
	// repeats the domain it lies in and taught keploy that the app asked it
	// (learned): unlearned, it is a candidate, though its walk's earlier
	// steps went unseen. (An absolute name, written with its trailing dot, is
	// asked as given only: keploy cannot tell it from such a candidate, and
	// gives it a DEBUG line only.) With them at or above ndots it may be the
	// app's own, asked as given first: held until the walk tells which.
	return RoutineCandidate, unseen && nameDots(name) >= r.ndots
}

// candidate reports whether name (lower case) is one a resolver may make of
// the name it was given, X, and one of the search domains, S: it is X.S. A
// resolver asks X first when its dots reach ndots, and goes on to the search
// domains only once it did not resolve; else it asks X in each search domain
// in turn, from the first, going on while they do not resolve. Either way the
// answer that sent it on came just before it, but for X in the first search
// domain with X's dots below ndots, the first name the resolver asks. unseen
// says keploy saw no such answer: name is a walk's first step, or one whose
// earlier steps keploy did not see (the walk began before keploy noted, or
// their events were lost), or the name the app was given, asked as given by
// a resolver that will search it (glibc's, Go's) or not (musl never searches
// a name whose dots reach ndots, under Docker's ndots:0 any name: Kubernetes'
// mongo-0.mongo.prod.svc.cluster.local, asked from a pod in another
// namespace, is the app's own). Only the rest of the walk tells which.
func (l *Log) candidate(r *resolver, scope string, qtype uint16, name string) (cand, unseen bool) {
	for i, s := range r.search {
		suffix := "." + s + "."
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		cand = true
		given := name[:len(name)-len(suffix)] + "."
		if nameDots(given) >= r.ndots && l.unresolvedSeen(scope, given, qtype) {
			return true, false
		}
		for _, b := range r.search[:i] {
			if l.unresolvedSeen(scope, given[:len(given)-1]+"."+b+".", qtype) {
				return true, false
			}
		}
	}
	return cand, cand
}

// unresolvedSeen reports whether the last answer keploy saw to name (lower
// case) of qtype, in scope, did not resolve it: a negative answer, or
// NODATA, past which a resolver walks on too.
func (l *Log) unresolvedSeen(scope, name string, qtype uint16) bool {
	if l.seen == nil {
		return false
	}
	// Peek, not Get: a diagnostic read must not change what is forgotten
	// next.
	a, ok := l.seen.Peek(key(scope, name, qtype))
	return ok && (a.Rcode != dns.RcodeSuccess || a.Records == 0)
}

// asksAsGivenFirst reports whether a resolver configured as r asks name as
// given before it tries its search domains, so that one of them may yet
// resolve it: there are search domains, name's dots reach ndots, and the
// name in one of them is no longer than a DNS name can be.
func (r *resolver) asksAsGivenFirst(name string) bool {
	if nameDots(name) < r.ndots {
		return false
	}
	for _, s := range r.search {
		if !tooLong(name, s) {
			return true
		}
	}
	return false
}

// tooLong reports whether name (with its trailing dot) in search domain s
// (without its dots at either end) is longer than a resolver asks: Go skips
// a candidate longer than 254 with its trailing dot.
func tooLong(name, s string) bool {
	return len(name)+len(s)+1 > 254
}

// nextStep is what the user can do about a lookup of name that got rcode. A
// name with no dot gets the hint that it is likely a container's, a compose
// service's or a Kubernetes Service's whatever the answer: a resolver may as
// well SERVFAIL or REFUSE a single-label name as NXDOMAIN it.
func (r *resolver) nextStep(name string, rcode int) string {
	dotless := isDotlessName(name)
	switch {
	case dotless && r.k8s:
		return NextStepDotlessK8s
	case dotless && r.where == AgentContainer:
		return NextStepDotlessDocker
	case dotless && r.where == AppContainer:
		return NextStepDotlessContainer
	case dotless:
		return NextStepDotlessNative
	case rcode != dns.RcodeNameError && r.relayed:
		return NextStepResolverError
	case rcode != dns.RcodeNameError:
		return NextStepResolverErrorObserved
	default:
		return NextStepNoSuchName
	}
}

// nameDots is how many dots name has, its trailing one aside.
func nameDots(name string) int {
	return strings.Count(strings.TrimSuffix(name, "."), ".")
}

// searchesKubernetes reports whether search is a Kubernetes pod's: it has
// the cluster's Service domain, svc.<cluster domain>, and the pod's
// namespace's in it, <namespace>.svc.<cluster domain>. Either alone is an
// ordinary domain's name (svc.example.com).
func searchesKubernetes(search []string) bool {
	for _, s := range search {
		if !strings.HasPrefix(s, "svc.") || len(s) == len("svc.") {
			continue
		}
		for _, ns := range search {
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

// IsGrpcServiceConfigProbe reports whether q is grpc-go's service-config
// lookup: a TXT query for _grpc_config.<target>.
//
// grpc-go's default (dns) resolver issues one per target. The record has no
// bearing on the RPCs keploy captures, and its absence is the normal answer.
func IsGrpcServiceConfigProbe(q dns.Question) bool {
	return q.Qtype == dns.TypeTXT && strings.HasPrefix(strings.ToLower(q.Name), "_grpc_config.")
}

// RedundantExpansionBase is the name a redundant search-list expansion was
// expanded from, as the app gave it to its resolver: name without the search
// domain it appended, in name's own case. A redundant expansion is the shape
// a resolver produces while walking its search list past an already-qualified
// name (ndots:5 is the Kubernetes default): a search domain appended to a
// name that already ends in it. ok is false when name is no redundant
// expansion of search's domains. A name carrying only ONE search suffix is
// not redundant: it is a real name.
func RedundantExpansionBase(search []string, name string) (base string, ok bool) {
	if len(search) == 0 || name == "" {
		return "", false
	}
	fqdn := dns.Fqdn(name)
	q := strings.ToLower(fqdn)
	for _, s := range search {
		s = strings.ToLower(strings.TrimSuffix(dns.Fqdn(s), "."))
		if s == "" {
			continue
		}
		suffix := "." + s + "."
		if !strings.HasSuffix(q, suffix) {
			continue
		}
		// Strip the trailing ".s", keeping the final dot, and ask whether
		// what remains still ends in the same suffix.
		if !strings.HasSuffix(q[:len(q)-len(s)-1], suffix) {
			continue
		}
		// Lower-casing keeps a DNS name's length (its presentation form is
		// ASCII), so the cut falls at the same place in fqdn; a name it
		// would not keep is given lower-cased.
		if len(fqdn) != len(q) {
			fqdn = q
		}
		return fqdn[:len(fqdn)-len(s)-1], true
	}
	return "", false
}

// RcodeString is rcode's name (NXDOMAIN, SERVFAIL, ...), or its number when
// it has none.
func RcodeString(rcode int) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}
	return strconv.Itoa(rcode)
}

// typeString is qtype's name (A, AAAA, ...), or its number when it has none.
func typeString(qtype uint16) string {
	if s, ok := dns.TypeToString[qtype]; ok {
		return s
	}
	return strconv.Itoa(int(qtype))
}
