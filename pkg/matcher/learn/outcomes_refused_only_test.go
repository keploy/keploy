package learn

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A caller that reads only whether Decide refuses a word (Input.RefusedOnly:
// a recording a call is tied with, tried in turn) gets the same verdict as
// one that reads every word: refused where Decide refuses any word, and
// where it refuses none, the very same decisions.

// roPair decides the input mk builds twice, as a caller that reads every
// word and as one that reads only whether a word is refused, each on an
// input of its own (a call's caches are its own), and checks the verdicts
// agree: both refuse a word, or neither does and both decided the same.
func roPair(t *testing.T, what string, mk func() Input) (full, only []Decision) {
	t.Helper()
	full = Decide(mk())
	in := mk()
	in.RefusedOnly = true
	only = Decide(in)
	switch {
	case refusesAny(full) != refusesAny(only):
		t.Errorf("FALSE GREEN? %s: reading every word refused %v (%s); reading only whether a word is refused refused %v (%s)",
			what, refusesAny(full), oDescribe(full), refusesAny(only), oDescribe(only))
	case !refusesAny(full) && !reflect.DeepEqual(full, only):
		t.Errorf("%s: no word refused, yet the decisions differ: every word %s; only whether refused %s", what, oDescribe(full), oDescribe(only))
	}
	return full, only
}

// roRefused is the leaf bind.$8 of newWordsCall(1), 82 to 50 at a leaf F
// gave nothing new at: refused (TestOutcomeCostALeafIsReadForNewWordsOnceACall).
func roRefused() Input {
	return costF("took 82 ms", "took 50 ms", []Earlier{eLeaf(eRecAt.Add(-50*time.Millisecond), 6, "82 7", "7")}, eStartup(), nil)
}

// roWithLeaf is in with one more drifted leaf at path, recorded rec and
// sent live in round F, decided after in's own (first: before them).
func roWithLeaf(in Input, path, rec, live string, first bool) Input {
	r, l := Shape{Leaves: maps.Clone(in.Rec.Leaves)}, Shape{Leaves: maps.Clone(in.Live.Leaves)}
	r.Leaves[path], l.Leaves[path] = oText(path, rec), oText(path, live)
	in.Rec, in.Live = r, l
	if first {
		in.Drift = append([]string{path}, in.Drift...)
	} else {
		in.Drift = append(slices.Clone(in.Drift), path)
	}
	return in
}

const (
	roRecID, roLiveID   = "6720a1b2c3d4e5f6a7b8c9d0", "6721f0e1d2c3b4a596877869"
	roRecID2, roLiveID2 = "6720a1b2c3d4e5f6a7b8c9e1", "6721f0e1d2c3b4a59687787a"
)

// A leaf refused first ends the decision of a call read only for whether
// it is refused: the leaf after it, an id made anew, is not decided, and the
// refused leaf is decided as it is with every word read. Refused last, or
// at no leaf, every leaf is decided, the same as with every word read.
func TestOutcomeCostARefusedOnlyCallStopsAtItsFirstRefusedLeaf(t *testing.T) {
	refusedFirst := func() Input { return roWithLeaf(roRefused(), "bind.$9", roRecID, roLiveID, false) }
	full, only := roPair(t, "a refused leaf, then an id made anew", refusedFirst)
	costRefused(t, "82 to 50, the first leaf", oWord(t, full, "82", "50"), costUnknown, "mock-8")
	costFresh(t, "the id after the refused leaf, every word read", oWord(t, full, roRecID, roLiveID))
	if len(only) != 1 || len(full) != 2 {
		t.Errorf("a refused leaf, then an id: read only whether refused, %d decisions (%s); every word, %d; want the refused leaf's 1, then a stop", len(only), oDescribe(only), len(full))
	} else if !reflect.DeepEqual(only[0], full[0]) {
		t.Errorf("the refused leaf read only whether refused: %s; every word read: %s", oDescribe(only), oDescribe(full[:1]))
	}
	refusedLast := func() Input { return roWithLeaf(roRefused(), "bind.$9", roRecID, roLiveID, true) }
	if full, only := roPair(t, "an id made anew, then a refused leaf", refusedLast); !reflect.DeepEqual(full, only) {
		t.Errorf("an id, then the refused leaf: read only whether refused %s; every word read %s; want the same, as the refusal comes last", oDescribe(only), oDescribe(full))
	}
	none := func() Input {
		return roWithLeaf(costF("slot "+roRecID2, "slot "+roLiveID2, nil, eStartup(), nil), "bind.$9", roRecID, roLiveID, false)
	}
	full, _ = roPair(t, "two ids made anew", none)
	costFresh(t, "the first id of two", oWord(t, full, roRecID2, roLiveID2))
	costFresh(t, "the second id of two", oWord(t, full, roRecID, roLiveID))
}

// A word refused only by a step after the leaves is refused the same with
// only the verdict read: one recorded id that round F sent as two values at
// two leaves (oneValue), each leaf alone made anew.
func TestOutcomeCostARefusedOnlyCallKeepsTheStepsAfterTheLeaves(t *testing.T) {
	two := func() Input {
		return roWithLeaf(costF(roRecID, roLiveID, nil, eStartup(), nil), "bind.$9", roRecID, roLiveID2, false)
	}
	full, only := roPair(t, "one recorded id sent as two values", two)
	d := oWord(t, only, roRecID, roLiveID2)
	if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Check != "one-value-two-sources" {
		t.Errorf("FALSE GREEN? one recorded id sent as two values, read only whether refused: %s; want refused as two sources (every word read: %s)", oDescribe([]Decision{d}), oDescribe(full))
	}
}

// An exchange at the call's own instant (TestOutcomeAnExchangeAtTheCallsOwnInstantIsReadBothWays)
// is read both ways with only the verdict read too: a word only the reading
// as a recording made after the call refuses is refused, and each other
// case decides as with every word read.
func TestOutcomeCostARefusedOnlyCallReadsTheOwnInstantBothWays(t *testing.T) {
	const path = "bind.$8"
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt, eRecAt.Add(time.Millisecond)
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			return v, nil
		}
	}
	for _, c := range []struct {
		name    string
		tell    func(tr *Tracker)
		refused bool
	}{
		{"told nothing", func(*Tracker) {}, true},
		{"told as recorded", func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, false},
		{"told a view with no word of the value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(chanceSrcRec)) }, false},
		{"told a view that holds F's value", func(tr *Tracker) { tr.ServedMockLive(m, "a", view("6c7d")) }, true},
		{"told a view keploy cannot read", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, true},
	} {
		mk := func() Input {
			tr := NewTracker()
			tr.Begin()
			tr.ServedMock(m)
			tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true),
				Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}})
			c.tell(tr)
			pl := tr.Placed()
			return Input{Drifts: clockedDrifts(), Drift: []string{path},
				Live: Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}},
				Rec:  Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}},
				At:   eRecAt, Arrived: eRecAt, Earlier: InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order), Raw: eStartup(),
				Test: "test-2", Mock: "mock-300", Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		}
		_, only := roPair(t, "an exchange at the call's own instant, "+c.name, mk)
		if d := oWord(t, only, "82", "6c7d"); c.refused != (d.Outcome == OutcomeRefused) {
			t.Errorf("an exchange at the call's own instant, %s, read only whether refused: %s; want refused %v", c.name, oDescribe([]Decision{d}), c.refused)
		}
		// With roRefused's leaf decided first, the first reading refuses
		// the call: one read only for its verdict reads it once, where
		// one read for every word reads it both ways.
		refusedFirst := func() Input {
			in := roWithLeaf(mk(), "bind.$9", "took 82 ms", "took 50 ms", true)
			in.Earlier = append(slices.Clone(in.Earlier), eLeaf(eRecAt.Add(-50*time.Millisecond), 6, "82 7", "7"))
			return in
		}
		n := wordReadings.Load()
		full := Decide(refusedFirst())
		every := wordReadings.Load() - n
		in := refusedFirst()
		in.RefusedOnly = true
		n = wordReadings.Load()
		only = Decide(in)
		once := wordReadings.Load() - n
		if !refusesAny(full) || !refusesAny(only) || every != 2 || once != 1 {
			t.Errorf("a refused leaf beside an exchange at the call's own instant, %s: every word read refused %v in %d readings; only whether refused refused %v in %d; want both refused, in 2 readings and 1",
				c.name, refusesAny(full), every, refusesAny(only), once)
		}
	}
}

// Mode Elsewhere decides every word whatever the caller reads: a refused
// word keploy.yml declares noise, decided first, stops nothing, and the
// timer's two clock words are kept
// (TestOutcomeElsewhereLearnsAClocksWordsOnlyWhereTheBucketIsDeclared, with
// the bucket declared, and roRefused's leaf decided first and declared).
func TestOutcomeCostARefusedOnlyCallElsewhereDecidesEveryWord(t *testing.T) {
	mk := func() Input {
		in := eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", []Earlier{eLeaf(eRAt.Add(-50*time.Millisecond), 6, "82 7", "7")})
		in = roWithLeaf(in, "bind.$8", "took 82 ms", "took 50 ms", true)
		in.Declared = func(field string, word int) bool { return field == eID || field == "bind.$8" }
		return in
	}
	full, only := roPair(t, "a declared refused leaf decided first, elsewhere", mk)
	clocks := 0
	for _, d := range only {
		if d.Outcome == OutcomeClock {
			clocks++
		}
	}
	if clocks != 2 || !reflect.DeepEqual(full, only) {
		t.Errorf("a declared refused leaf decided first, read only whether refused: %d Clock words (%s); want 2, as every word read (%s)", clocks, oDescribe(only), oDescribe(full))
	}
	in := mk()
	in.Mode = ModeOwn
	if ds, _ := decideWords(in, true); len(ds) == 0 || ds[0].Outcome != OutcomeRefused {
		t.Errorf("the declared leaf, decided first with every earlier place in scope: %s; want it refused, so a stop at it would drop the clock words", oDescribe(ds))
	}
}
