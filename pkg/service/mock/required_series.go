package mock

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// seriesDue is one series a declared rule placed in a set's worker scope
// (models.MockEntry.Series), as the mock run's end requires it.
type seriesDue struct {
	// Names are the series' recordings, in the order worker: lists them.
	Names []string
	// Rule is the rule that placed them, "<id>@<first 8 hex of its digest>".
	Rule string
	// Trails is the test the series trails; empty for a background series.
	Trails string
	// Place is, for a background series, its first recording's place: the
	// test after whose beginning the run reaches it; empty for the set's
	// start.
	Place string
	// write is whether an entry of the series says neither that it reads
	// (models.MockEntry.Read), that its write changed nothing when recorded
	// (models.MockEntry.Unchanged), nor that its rule never requires it
	// (models.MockEntry.Optional): such a series is required.
	write bool
	// served are, of Names, the entries that say they read, that their
	// write changed nothing, or that their rule never requires them: served,
	// never what makes a required series (madeIn). Only a once series holds
	// such entries beside a write: the reads its write follows on its
	// connection (keploy.yml mock.rules chain), whose answers do not make a
	// write the application stopped making.
	served []string
	// due is the bound of the application's timer that makes a once
	// series' call (models.MockEntry.Due): the longest of its entries'; 0
	// for none.
	due time.Duration
	// early is, at the run's end, that the application's run ended before
	// due (markEarly): the series is not due, whatever its place.
	early bool
	// causes are, by recording of Names, the recording whose answer carried
	// the value its call sends (models.MockEntry.CausedBy), for those that
	// name one the set's mappings list (causeOf); causeIn is, by cause, the
	// test whose list holds it, or whose window a worker: cause was placed
	// in ("" for none).
	causes, causeIn map[string]string
}

// uncaused is whether every recording of s that makes it (makers) names a
// cause (seriesDue.causes) and used holds none of them: the run served no
// answer that led the application to make the series' call, so it is not
// due. A series none or only some of whose makers name a cause is never
// uncaused: it stays due as one with none.
func (s seriesDue) uncaused(used map[string]bool) bool {
	ms := s.makers()
	if len(ms) == 0 {
		return false
	}
	for _, n := range ms {
		c, ok := s.causes[n]
		if !ok || used[c] {
			return false
		}
	}
	return true
}

// unserved are the causes of s's makers (seriesDue.causes), each once in
// makers' order, with the test whose window recorded it and that test's
// outcome in the run (outcome).
func (s seriesDue) unserved(outcome map[string]string) []UnservedCause {
	var out []UnservedCause
	for _, n := range s.makers() {
		c := s.causes[n]
		if slices.ContainsFunc(out, func(u UnservedCause) bool { return u.Name == c }) {
			continue
		}
		w := s.causeIn[c]
		out = append(out, UnservedCause{Name: c, Window: w, Outcome: outcome[w]})
	}
	return out
}

// UnservedCause is a recording whose answer led the application to make a
// trailing series' call when it was recorded (models.MockEntry.CausedBy),
// which the run did not serve (RequiredNotMade.Unserved).
type UnservedCause struct {
	// Name is the recording.
	Name string `yaml:"name" json:"name"`
	// Window is the test whose window recorded it (its list holds it, or a
	// rule placed it there); empty when it is no test's.
	Window string `yaml:"window,omitempty" json:"window,omitempty"`
	// Outcome is Window's outcome in the run (passed, failed, skipped...);
	// empty when the run has no line for it.
	Outcome string `yaml:"outcome,omitempty" json:"outcome,omitempty"`
}

// RequiredNotMade is a series a declared rule placed in the worker scope that
// was due in the run and that the run never answered (Receipt.RequiredNotMade).
type RequiredNotMade struct {
	// Series is the series, "<rule id>/<trailed test, or ->/<n>".
	Series string `yaml:"series" json:"series"`
	// Rule is the rule that placed it, "<id>@<first 8 hex of its digest>".
	Rule string `yaml:"rule" json:"rule"`
	// Trails is the test it trails, which fails; empty for a background
	// series, which fails the set.
	Trails string `yaml:"trails,omitempty" json:"trails,omitempty"`
	// Names are the series' recordings that make it required (its writes
	// that changed data: all of them, but for a once series' chain reads),
	// none of which the run used.
	Names []string `yaml:"names" json:"names"`
	// Due is, of a series the run did not require because the application's
	// run ended before its rule's timer bound (Receipt.RequiredNotDue), that
	// bound ("10s"); empty on a series required and not made.
	Due string `yaml:"due,omitempty" json:"due,omitempty"`
	// TrailsFailed is, of a trailing series, whether the test it trails
	// failed: the series not made may follow from that failure, and the run
	// did not wait for it (seriesDue.holdsWait).
	TrailsFailed bool `yaml:"trails_failed,omitempty" json:"trails_failed,omitempty"`
	// Unserved is, of a series the run did not require because it served
	// none of the answers that led the application to make its call
	// (Receipt.RequiredNotDue, seriesDue.uncaused), each such answer; empty
	// otherwise.
	Unserved []UnservedCause `yaml:"unserved,omitempty" json:"unserved,omitempty"`
}

// requiredSeries is, by series, each series a declared rule placed in mp's
// worker scope (models.MockEntry.Series), with its recordings in worker:
// order: keploy requires each once per run when it is due
// (requiredNotMade), whether or not the agent holds its recordings (one it
// cannot serve is never used). A series is one requirement however many
// recordings it has: the application repeats its call a number of times that
// depends on how long the run takes (a sweep that runs every minute), and
// one answered recording makes the series. Only a series of a write that
// changed data is required: one every entry of which says it reads
// (models.MockEntry.Read) is served, not required, as every read is, and
// so is one every entry of which says it reads or that its write changed
// nothing when recorded (models.MockEntry.Unchanged: a MongoDB reply of n
// 0), or that its rule never requires it (models.MockEntry.Optional: a call
// keploy mock record saw once, which a shorter run may end before); all are
// left out. One entry with none of them makes its series required. A series trailing a test that is
// not one of table's, the set's, is never due and is left out. nil when no
// entry has a required series.
func requiredSeries(mp *models.Mapping, table map[string][]string) map[string]seriesDue {
	out := placedSeries(mp, table)
	maps.DeleteFunc(out, func(_ string, s seriesDue) bool { return !s.write })
	if len(out) == 0 {
		return nil
	}
	return out
}

// placedSeries is, by series, every series of mp's worker scope
// (models.MockEntry.Series) that requiredSeries reads, the ones it leaves
// out as served, not required, among them (write false): what the run's
// RULE USED lines count. An entry keploy's judging marked covered
// (models.MockEntry.CoveredBy) is in none: the agent is never handed it
// (leaveCoveredOut), and the entry it names stands for it. nil when no
// entry has a series.
func placedSeries(mp *models.Mapping, table map[string][]string) map[string]seriesDue {
	if mp == nil {
		return nil
	}
	var out map[string]seriesDue
	for _, e := range mp.Worker {
		// A covered entry (models.MockEntry.CoveredBy) is never served: the
		// entry it names answers its calls and stands for it.
		if e.Series == "" || e.CoveredBy != "" {
			continue
		}
		if _, ok := table[e.Trails]; e.Trails != "" && !ok {
			continue
		}
		if out == nil {
			out = map[string]seriesDue{}
		}
		s, seen := out[e.Series]
		if !seen {
			s = seriesDue{Rule: e.Rule, Trails: e.Trails, Place: e.Place}
		}
		s.Names = append(s.Names, e.Name)
		if e.Read || e.Unchanged || e.Optional {
			s.served = append(s.served, e.Name)
		} else {
			s.write = true
		}
		if d := entryDue(e); d > s.due {
			s.due = d
		}
		if e.CausedBy != "" {
			if s.causes == nil {
				s.causes = map[string]string{}
			}
			s.causes[e.Name] = e.CausedBy
		}
		out[e.Series] = s
	}
	return keepCauses(mp, out)
}

// keepCauses is series with each series' causes (seriesDue.causes) kept
// only where they name a recording mp lists that can be served: in a
// test's list, start-up, or worker: uncovered (models.MockEntry.CoveredBy:
// never served), and not the recording itself. A cause the mappings list
// nowhere could never be served, which would leave its series never due:
// it is dropped, so the series is required as one with none. Each kept
// cause's window (seriesDue.causeIn) is the test whose list holds it, or a
// worker: cause's place.
func keepCauses(mp *models.Mapping, series map[string]seriesDue) map[string]seriesDue {
	var window map[string]string
	for id, s := range series {
		if len(s.causes) == 0 {
			continue
		}
		if window == nil {
			window = servableWindows(mp)
		}
		kept, in := map[string]string{}, map[string]string{}
		for n, c := range s.causes {
			w, ok := window[c]
			if !ok || c == n {
				continue
			}
			kept[n], in[c] = c, w
		}
		s.causes, s.causeIn = kept, in
		series[id] = s
	}
	return series
}

// servableWindows is, by each recording mp lists that a replay can serve,
// the test whose window holds it: the test whose list holds it, a worker:
// entry's place, "" for start-up's or a worker: entry with no place. A
// covered worker: entry (models.MockEntry.CoveredBy) is never served and
// is left out.
func servableWindows(mp *models.Mapping) map[string]string {
	out := map[string]string{}
	for _, e := range mp.Startup {
		out[e.Name] = ""
	}
	for _, e := range mp.Worker {
		if e.CoveredBy == "" {
			out[e.Name] = e.Place
		}
	}
	for _, tc := range mp.TestCases {
		for _, e := range tc.Mocks {
			out[e.Name] = tc.ID
		}
	}
	return out
}

// requiredNotMade is, in series order, each series of req that was due in
// this run and none of whose recordings consumed holds. A trailing series is
// due when the test it trails ran (its line in tests passed or failed); a
// background one when the run reached its place: the set's start, or the
// test of that ID ran. A test the run did not run (skipped, gated, never
// begun) makes nothing due. When what the run consumed could not be read
// (known false), no due series can be said made: unread then names each test
// a due trailing series trails, and unreadSets each due background series.
func requiredNotMade(req map[string]seriesDue, tests []TestReceipt, consumed []models.MockState, known bool) (notMade []RequiredNotMade, unread map[string]bool, unreadSets []string) {
	if len(req) == 0 {
		return nil, nil, nil
	}
	failed := map[string]bool{}
	for _, t := range tests {
		if t.Outcome == models.ScopeOutcomeFailed {
			failed[t.Name] = true
		}
	}
	// What the run consumed unread, whether a series' cause was served is
	// not known: no series is let off as uncaused (owedSeries).
	for _, id := range owedSeries(req, ranTests(tests), usedNames(consumed), known) {
		s := req[id]
		if !known {
			if s.Trails == "" {
				unreadSets = append(unreadSets, id)
				continue
			}
			if unread == nil {
				unread = map[string]bool{}
			}
			unread[s.Trails] = true
			continue
		}
		notMade = append(notMade, RequiredNotMade{Series: id, Rule: s.Rule, Trails: s.Trails, Names: s.makers(), TrailsFailed: failed[s.Trails]})
	}
	return notMade, unread, unreadSets
}

// ranOutcome is whether a test whose outcome is outcome ran: it passed or
// failed. One skipped, gated or never begun did not.
func ranOutcome(outcome string) bool {
	return outcome == models.ScopeOutcomePassed || outcome == models.ScopeOutcomeFailed
}

// usedNames is the set of the recordings consumed holds.
func usedNames(consumed []models.MockState) map[string]bool {
	used := make(map[string]bool, len(consumed))
	for _, c := range consumed {
		used[c.Name] = true
	}
	return used
}

// owedSeries is, in series order, each series of req that is due, by the
// tests that ran (ran), and none of whose recordings used holds. A trailing
// series is due when the test it trails ran; a background one when the run
// reached its place: the set's start, or the test of that ID ran. One whose
// every making recording names a cause the run did not serve (uncaused) is
// not due, but only where used is what the run consumed (known): unread,
// whether a cause was served is not known, and the series stays owed.
func owedSeries(req map[string]seriesDue, ran, used map[string]bool, known bool) []string {
	var owed []string
	for _, id := range slices.Sorted(maps.Keys(req)) {
		s := req[id]
		if !s.dueIn(ran) || s.early || s.madeIn(used) || (known && s.uncaused(used)) {
			continue
		}
		owed = append(owed, id)
	}
	return owed
}

// dueIn is whether s is due in a run whose tests that ran are ran: a
// trailing series when the test it trails ran, a background one when the run
// reached its place: the set's start, or the test of that ID ran.
func (s seriesDue) dueIn(ran map[string]bool) bool {
	if s.Trails == "" {
		return s.Place == "" || ran[s.Place]
	}
	return ran[s.Trails]
}

// commandClock reads the time the test command's run is measured by
// (commandRun). A test replaces it.
var commandClock = time.Now

// commandRun is when the test command keploy started began, and when what
// it started was down (Run returned): zero where keploy did not start the
// command itself (a compose run).
type commandRun struct{ began, ended time.Time }

// appLife is how long the application ran in a replay, as keploy reads it
// (lifeOf): a once series whose rule's timer bound it did not reach is not
// due (markEarly).
type appLife struct {
	// timed is that keploy read the life; false, every series stays due as
	// with no bound.
	timed bool
	// lived is the life read, to the exit of what the test command started:
	// from the application's first call keploy served or missed in the run
	// (lifeFirstCall), 0 when it made none (lifeNoCall), or from the test
	// command's start (lifeCommand), which no application it started began
	// before.
	lived time.Duration
	// from is what lived is read from: lifeFirstCall, lifeNoCall or
	// lifeCommand.
	from string
}

const (
	// lifeFirstCall reads the application's life from the earliest call of
	// the run keploy served or missed: the application ran by then.
	lifeFirstCall = "first-call"
	// lifeNoCall is a run in which keploy served and missed no call: the
	// application never ran past a bound.
	lifeNoCall = "no-call"
	// lifeCommand reads the application's life from the test command's
	// start, the longest it can be, where the run's first call is not
	// known: a call keploy saw carries no time or one outside the command's
	// run, or what the run served or missed could not be read.
	lifeCommand = "command"
)

// lifeOf is the application's life in run (appLife), read from what the
// agent served (consumed, each stamped with when it first answered) and
// missed (misses, each with when it missed): the application ran from the
// first of those calls, which keploy sees, to the exit of what the test
// command started. A run keploy did not time (run zero) is not read. Where
// the first call is not known (lifeCommand) the life is the command's whole
// run, so a series is let off only when even that is shorter than its bound.
func lifeOf(run commandRun, consumed []models.MockState, consumedKnown bool, misses []models.UnmatchedCall, missesKnown bool) appLife {
	// Replay sets the run's end with its start.
	if run.began.IsZero() {
		return appLife{}
	}
	whole := appLife{timed: true, lived: run.ended.Sub(run.began), from: lifeCommand}
	if !consumedKnown || !missesKnown {
		return whole
	}
	first, called := run.ended, false
	for _, at := range callTimes(consumed, misses) {
		// A call keploy saw outside the command's run, or one that carries
		// no time (which reads as long before it), leaves when the
		// application began unknown.
		if at.Before(run.began) || at.After(run.ended) {
			return whole
		}
		called = true
		if at.Before(first) {
			first = at
		}
	}
	if !called {
		return appLife{timed: true, from: lifeNoCall}
	}
	return appLife{timed: true, lived: run.ended.Sub(first), from: lifeFirstCall}
}

// callTimes are the times of the run's calls keploy saw: when each consumed
// recording first answered (models.MockState.Timestamp, Unix nanoseconds;
// 0, no time, reads as 1970) and when each miss missed
// (models.UnmatchedCall.At; no time is the zero time).
func callTimes(consumed []models.MockState, misses []models.UnmatchedCall) []time.Time {
	out := make([]time.Time, 0, len(consumed)+len(misses))
	for _, c := range consumed {
		out = append(out, time.Unix(0, c.Timestamp))
	}
	for _, m := range misses {
		out = append(out, m.At)
	}
	return out
}

// entryDue is e's timer bound (models.MockEntry.Due) as written: 0 when it
// has none or it does not parse, and possibly not positive. placedSeries
// keeps a series' longest positive one, so a bound that is not positive
// bounds nothing.
func entryDue(e models.MockEntry) time.Duration {
	d, _ := time.ParseDuration(e.Due)
	return d
}

// markEarly is series with each series whose timer bound (seriesDue.due)
// the application's life (lifeOf) did not reach marked early: it lived
// shorter than the bound. A life keploy did not read (a compose run) marks
// none: every series stays due as it would be with no bound. series itself
// is left as it is.
func markEarly(series map[string]seriesDue, life appLife) map[string]seriesDue {
	if !life.timed {
		return series
	}
	out := make(map[string]seriesDue, len(series))
	for id, s := range series {
		// A series with no bound (due 0) is never early: no life read is
		// negative.
		s.early = life.lived < s.due
		out[id] = s
	}
	return out
}

// notYetDue is, in series order, each series of req the run reached (dueIn)
// that it would require but for the application's run ending before its
// timer bound (seriesDue.early), or but for the run serving none of the
// answers that led the application to make its call (seriesDue.uncaused),
// and none of whose recordings consumed holds: the run says each
// (RuleNotDueLine) and does not fail on it. None
// when what the run consumed could not be read (known false): the run's
// end names those series as unread.
func notYetDue(req map[string]seriesDue, tests []TestReceipt, consumed []models.MockState, known bool) []RequiredNotMade {
	if !known {
		return nil
	}
	ran, used := ranTests(tests), usedNames(consumed)
	var outcome map[string]string
	var out []RequiredNotMade
	for _, id := range slices.Sorted(maps.Keys(req)) {
		s := req[id]
		if !s.dueIn(ran) || s.madeIn(used) {
			continue
		}
		switch {
		case s.early:
			out = append(out, RequiredNotMade{Series: id, Rule: s.Rule, Trails: s.Trails, Names: s.makers(), Due: s.due.String()})
		case s.uncaused(used):
			if outcome == nil {
				outcome = map[string]string{}
				for _, t := range tests {
					outcome[t.Name] = t.Outcome
				}
			}
			out = append(out, RequiredNotMade{Series: id, Rule: s.Rule, Trails: s.Trails, Names: s.makers(), Unserved: s.unserved(outcome)})
		}
	}
	return out
}

// uncausedWhy says, for a RULE NOT DUE line, why the run did not serve u:
// what its window's test did in the run.
func uncausedWhy(u UnservedCause) string {
	switch {
	case u.Window == "":
		return u.Name + ", recorded outside every test's window, which the run never asked for"
	case u.Outcome == models.ScopeOutcomeFailed:
		return u.Name + ", recorded in " + u.Window + "'s window: " + u.Window + " failed in this run, so it may have ended before the application asked for it"
	case u.Outcome == models.ScopeOutcomePassed:
		return u.Name + ", recorded in " + u.Window + "'s window: " + u.Window + " passed in this run without the application asking for it"
	case u.Outcome == "":
		return u.Name + ", recorded in " + u.Window + "'s window: " + u.Window + " did not run in this run"
	default:
		return u.Name + ", recorded in " + u.Window + "'s window: " + u.Window + " was " + u.Outcome + " in this run"
	}
}

// ruleNotCausedLine is the run's RULE NOT DUE line for n, a series none of
// whose causes the run served (RequiredNotMade.Unserved): each cause, the
// window that recorded it and why the run did not serve it. The line's
// fields end it as JSON, as keploy's other RULE lines.
func ruleNotCausedLine(n RequiredNotMade) string {
	id, _, _ := strings.Cut(n.Rule, "@")
	num := n.Series[strings.LastIndex(n.Series, "/")+1:]
	causes, whys := make([]string, 0, len(n.Unserved)), make([]string, 0, len(n.Unserved))
	for _, u := range n.Unserved {
		causes, whys = append(causes, u.Name), append(whys, uncausedWhy(u))
	}
	fields := map[string]any{"event": "not-due", "id": id, "series": n.Series, "unserved": causes}
	what := "its background series " + num
	if n.Trails != "" {
		fields["trails"] = n.Trails
		what = n.Trails + "'s trailing series " + num
	}
	b, err := json.Marshal(fields)
	if err != nil {
		b = []byte(`{"event":"unwritable"}`)
	}
	return fmt.Sprintf("RULE NOT DUE %s: %s not due: the application made the call of %s (%s) when it was recorded after an answer that carried the value it sends, and the run served no such answer (%s); keploy requires the series only in a run that served one of them, so no test fails on it. rule=%s",
		n.Rule, id, what, strings.Join(n.Names, ", "), strings.Join(whys, "; "), b)
}

// RuleNotDueLine is the run's RULE NOT DUE line for n, a series of a once
// rule whose timer bound the application's life (lifeOf) did not reach. It
// says what the life was read from. The line's fields end it as JSON, as
// keploy's other RULE lines.
func RuleNotDueLine(n RequiredNotMade, life appLife) string {
	if len(n.Unserved) > 0 {
		return ruleNotCausedLine(n)
	}
	id, _, _ := strings.Cut(n.Rule, "@")
	num := n.Series[strings.LastIndex(n.Series, "/")+1:]
	ran := life.lived.Round(100 * time.Millisecond)
	fields := map[string]string{"event": "not-due", "id": id, "series": n.Series, "due": n.Due, "lived": ran.String(), "from": life.from}
	b, err := json.Marshal(fields)
	if err != nil {
		b = []byte(`{"event":"unwritable"}`)
	}
	var lived string
	switch life.from {
	case lifeFirstCall:
		lived = fmt.Sprintf("the app exited %s after its first call keploy saw in the run, before its", ran)
	case lifeNoCall:
		lived = "the app made no call keploy saw in the run, so it never ran to its"
	default:
		lived = fmt.Sprintf("the app exited at most %s after it started (the test command's whole run), before its", ran)
	}
	return fmt.Sprintf("RULE NOT DUE %s: %s not due: %s %s timer (due: %s), and made no call of its background series %s (%s); keploy requires the series only in a run the app outlives that bound, so the set does not fail on it. rule=%s",
		n.Rule, id, lived, n.Due, n.Due, num, strings.Join(n.Names, ", "), b)
}

// madeIn is whether used holds a recording of s that makes it (makers):
// one such answered call makes the series. A series of one request has the
// same marks on every entry, so any of its recordings makes it; a read of a
// once series' chain does not.
func (s seriesDue) madeIn(used map[string]bool) bool {
	return slices.ContainsFunc(s.makers(), func(n string) bool { return used[n] })
}

// makers are s's recordings but those it serves only (seriesDue.served):
// the writes that changed data when recorded.
func (s seriesDue) makers() []string {
	return slices.DeleteFunc(slices.Clone(s.Names), func(n string) bool { return slices.Contains(s.served, n) })
}

// notMadeByName is, by recording, the test each trailing series of notMade
// trails: failUnmadeTrails fails that test naming every recording of its
// series as a call it did not make. nil when no series of notMade trails one.
func notMadeByName(notMade []RequiredNotMade) map[string]string {
	var out map[string]string
	for _, n := range notMade {
		if n.Trails == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		for _, name := range n.Names {
			out[name] = n.Trails
		}
	}
	return out
}

// RuleNotMadeLine is the run's RULE NOT MADE line for n: what the rule
// required, why the run fails, and the line's fields as JSON at its end
// (" rule={...}", keys sorted), as keploy's other RULE lines end.
func RuleNotMadeLine(n RequiredNotMade) string {
	id, _, _ := strings.Cut(n.Rule, "@")
	num := n.Series[strings.LastIndex(n.Series, "/")+1:]
	fields := map[string]string{"event": "not-made", "id": id, "series": n.Series}
	var what string
	if n.Trails != "" {
		fields["trails"] = n.Trails
		what = fmt.Sprintf("%s ran and its trailing series %s (%s) answered no call by the run's end; the app no longer makes it, or makes another request; %s fails and CI would be red",
			n.Trails, num, strings.Join(n.Names, ", "), n.Trails)
		if n.TrailsFailed {
			what = fmt.Sprintf("%s failed, and its trailing series %s (%s) answered no call by the run's end, which may follow from that failure: keploy waits for a trailing series only after a test that passed; fix %s's failure first, then this series if a run where %s passes does not make it",
				n.Trails, num, strings.Join(n.Names, ", "), n.Trails, n.Trails)
		}
	} else {
		what = fmt.Sprintf("the run reached its background series %s (%s), which answered no call by the run's end; the app no longer makes it, or makes another request; the set fails and CI would be red",
			num, strings.Join(n.Names, ", "))
	}
	label := n.Rule
	if label == "" {
		// keploy record moved the series as trailing a test on its own (no
		// declared rule): a write it trails is required as a rule's is.
		label, fields["by"] = "(keploy record's trail move)", "record"
		delete(fields, "id")
	}
	b, err := json.Marshal(fields)
	if err != nil {
		b = []byte(`{"event":"unwritable"}`)
	}
	return fmt.Sprintf("RULE NOT MADE %s: %s. rule=%s", label, what, b)
}
