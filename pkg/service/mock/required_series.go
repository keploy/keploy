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
// RULE USED lines count. nil when no entry has a series.
func placedSeries(mp *models.Mapping, table map[string][]string) map[string]seriesDue {
	if mp == nil {
		return nil
	}
	var out map[string]seriesDue
	for _, e := range mp.Worker {
		if e.Series == "" {
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
		out[e.Series] = s
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
	for _, id := range owedSeries(req, ranTests(tests), usedNames(consumed)) {
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
		notMade = append(notMade, RequiredNotMade{Series: id, Rule: s.Rule, Trails: s.Trails, Names: s.makers()})
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
// reached its place: the set's start, or the test of that ID ran.
func owedSeries(req map[string]seriesDue, ran, used map[string]bool) []string {
	var owed []string
	for _, id := range slices.Sorted(maps.Keys(req)) {
		s := req[id]
		if !s.dueIn(ran) || s.early || s.madeIn(used) {
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
// timer bound (seriesDue.early), and none of whose recordings consumed
// holds: the run says each (RuleNotDueLine) and does not fail on it. None
// when what the run consumed could not be read (known false): the run's
// end names those series as unread.
func notYetDue(req map[string]seriesDue, tests []TestReceipt, consumed []models.MockState, known bool) []RequiredNotMade {
	if !known {
		return nil
	}
	ran, used := ranTests(tests), usedNames(consumed)
	var out []RequiredNotMade
	for _, id := range slices.Sorted(maps.Keys(req)) {
		s := req[id]
		if !s.early || !s.dueIn(ran) || s.madeIn(used) {
			continue
		}
		out = append(out, RequiredNotMade{Series: id, Rule: s.Rule, Trails: s.Trails, Names: s.makers(), Due: s.due.String()})
	}
	return out
}

// RuleNotDueLine is the run's RULE NOT DUE line for n, a series of a once
// rule whose timer bound the application's life (lifeOf) did not reach. It
// says what the life was read from. The line's fields end it as JSON, as
// keploy's other RULE lines.
func RuleNotDueLine(n RequiredNotMade, life appLife) string {
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
