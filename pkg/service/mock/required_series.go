package mock

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

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
		if !s.dueIn(ran) || s.madeIn(used) {
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
