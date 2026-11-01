package learn

import (
	"strings"
	"testing"
	"time"
)

// Rows for the timer rule's mode (Decide with ModeElsewhere): a call that
// landed in another test's window, or between tests, paired for learning
// only with a recording another scope owns. Every earlier place of the run
// is a candidate, for sources and for conflicts. The pairing keeps what it
// decided only where every changed word is a Clock or noise keploy.yml
// declares; else it learns nothing, never noise, never a Value, never a
// fresh word, and its line names the word that stopped it and the edit.

// The janitor's lease insert: {_id "pat-janitor:<unix/1800>", claimed_at
// now, expires_at now+30m}, BSON dates at a millisecond cut.
const (
	eID      = "body.documents[0]._id"
	eClaimed = "body.documents[0].claimed_at"
	eExpires = "body.documents[0].expires_at"
)

// eLease is the lease insert recorded at rAt and made in round F at fAt,
// its id rid and fid, as a ModeElsewhere input in the window w.
func eLease(rAt, fAt time.Time, rid, fid string, earlier []Earlier) Input {
	shape := func(at time.Time, id string) Shape {
		return Shape{Leaves: map[string]Leaf{
			eID:      oText(eID, id),
			eClaimed: oText(eClaimed, iMs(at.Add(-time.Millisecond))),
			eExpires: oText(eExpires, iMs(at.Add(30*time.Minute-time.Millisecond))),
		}}
	}
	drift := []string{eClaimed, eExpires}
	if rid != fid {
		drift = append(drift, eID)
	}
	return Input{
		Drifts: oDrifts(), Drift: drift,
		Live: shape(fAt, fid), Rec: shape(rAt, rid),
		At: rAt, Arrived: fAt, Earlier: earlier, Window: iWin(rAt, fAt, 50*time.Millisecond).ComparesOffsets(),
		Test: "test-4", Mock: "mock-90", Mode: ModeElsewhere, Landed: Landed{Where: LandedOther, Test: "test-5"},
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	}
}

var (
	eRAt = oAt.Add(400 * time.Millisecond)
	eFAt = oAt.Add(time.Hour + 900*time.Millisecond)
)

// eNothing checks the pairing learned nothing: no decision is kept as a
// Clock, a Value or a fresh word, and some decision's line names each of
// names (the word or place that stopped it, and the edit).
func eNothing(t *testing.T, what string, ds []Decision, names ...string) {
	t.Helper()
	var lines []string
	for _, d := range ds {
		if d.Outcome != OutcomeRefused {
			t.Errorf("FALSE GREEN? %s: kept %s; want nothing learned", what, oDescribe([]Decision{d}))
		}
		lines = append(lines, d.Line)
	}
	all := strings.Join(lines, "\n")
	for _, n := range names {
		if !strings.Contains(all, n) {
			t.Errorf("%s: lines %q; want one naming %q", what, all, n)
		}
	}
}

// The control: the janitor's lease with its id unchanged is two Clock words
// (0 and +30 m), kept, each line saying it was learned from round F's call
// for learning only.
func TestOutcomeElsewhereLearnsATimersClockWords(t *testing.T) {
	ds := Decide(eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968290", nil))
	for _, tc := range []struct{ what, rec, live string }{
		{"claimed_at", iMs(eRAt.Add(-time.Millisecond)), iMs(eFAt.Add(-time.Millisecond))},
		{"expires_at", iMs(eRAt.Add(30*time.Minute - time.Millisecond)), iMs(eFAt.Add(30*time.Minute - time.Millisecond))},
	} {
		d := oWord(t, ds, tc.rec, tc.live)
		iClock(t, tc.what, d)
		if !strings.Contains(d.Line, "learned from round F's call in test-5's window, for learning only") {
			t.Errorf("%s: line %q; want it to say it was learned from round F's call in test-5's window, for learning only", tc.what, d.Line)
		}
	}
	// Landed between tests, the line says so; nothing kept says "another window".
	in := eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968290", nil)
	in.Landed = Landed{Where: LandedBetween}
	if d := oWord(t, Decide(in), iMs(eRAt.Add(-time.Millisecond)), iMs(eFAt.Add(-time.Millisecond))); !strings.Contains(d.Line, "round F's call in between tests, for learning only") {
		t.Errorf("claimed_at landed between tests: line %q; want it to name between tests", d.Line)
	}
	// A pairing that learns nothing names where the call landed too.
	in = eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", nil)
	for _, d := range Decide(in) {
		if !strings.Contains(d.Line, "landed in test-5's window") {
			t.Errorf("a pairing that learned nothing: line %q; want it to name test-5's window", d.Line)
		}
	}
}

// A word an earlier place of the run sent (a Value anywhere), a pick, an
// echo, a conflict: nothing is learned.
func TestOutcomeElsewhereLearnsNothingBesideAValueAnEarlierPlaceSent(t *testing.T) {
	sent := []Earlier{{Mock: "mock-12", At: eRAt.Add(-3 * time.Second), Run: true,
		Seen: []Seen{{Side: Request, Leaf: oText("body.lease", "pat-janitor:968290"), Live: "pat-janitor:968291"}}}}
	ds := Decide(eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", sent))
	eNothing(t, "a bucket another test sent", ds, "mock-12", "body.documents[0]._id")
	// An answer of another test gave round F's claimed_at unchanged: a pick.
	pick := []Earlier{{Mock: "mock-13", At: eRAt.Add(-2 * time.Second), Run: true,
		Seen: []Seen{{Side: Response, Leaf: oText("body.claimed_at", iMs(eFAt.Add(-time.Millisecond))), Live: iMs(eFAt.Add(-time.Millisecond))}}}}
	eNothing(t, "a claimed_at another test's answer gave", Decide(eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968290", pick)), "mock-13")
	// The call's recorded answer echoes the bucket that changed: an echo.
	echo := eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", nil)
	echo.Resp = []Leaf{oText("reply.lease", "pat-janitor:968290")}
	eNothing(t, "a bucket its recorded answer echoes", Decide(echo), "reply.lease")
	// Another test's request held the recorded id whole, and round F moved
	// it while the call sent another: a conflict at an earlier place.
	held := []Earlier{{Mock: "mock-15", At: eRAt.Add(-time.Second), Run: true,
		Seen: []Seen{{Side: Request, Leaf: oText("body.lease", "pat-janitor:968290"), Live: "pat-janitor:968292"}}}}
	eNothing(t, "a bucket another test held whole, F moved", Decide(eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", held)), "mock-15")
}

// A janitor-like insert tied with another flow's recording; its word
// "pat" against "session" is made anew each run (no source, chance only):
// nothing learned, and no noise written. The line says what would let the
// rule learn its clock reads.
func TestOutcomeElsewhereLearnsNothingWhereAWordIsFresh(t *testing.T) {
	ds := Decide(eLease(eRAt, eFAt, "session-janitor:968290", "pat-janitor:968290", nil))
	eNothing(t, `"session" against "pat"`, ds, "made anew each run", "keploy.yml")
}

// A timer whose only change that is no clock read is a 6-digit bucket
// (no instant): nothing is learned, and the line prints the noise edit. With
// that word declared noise in keploy.yml, its clock reads learn.
func TestOutcomeElsewhereLearnsAClocksWordsOnlyWhereTheBucketIsDeclared(t *testing.T) {
	in := eLease(eRAt, eFAt, "pat-janitor:968290", "pat-janitor:968291", nil)
	ds := Decide(in)
	eNothing(t, "a 6-digit bucket", ds, "body.documents[0]._id")
	noise := false
	for _, d := range ds {
		noise = noise || oEdits(d.Refusal, "noise-at-word")
	}
	if !noise {
		t.Errorf("a 6-digit bucket: %s; want the noise edit at its word", oDescribe(ds))
	}
	in.Declared = func(field string, word int) bool { return field == eID }
	clocks := 0
	for _, d := range Decide(in) {
		switch d.Outcome {
		case OutcomeClock:
			clocks++
		case OutcomeValue, OutcomeFresh:
			t.Errorf("the bucket declared: %s; want no Value and no fresh word", oDescribe([]Decision{d}))
		}
	}
	if clocks != 2 {
		t.Errorf("the bucket declared: %d Clock words; want 2 (claimed_at, expires_at)", clocks)
	}
}

// A clock word another test's earlier request held whole at a fine
// cut, which round F moved: a conflict at an earlier place of the run,
// nothing learned (conflicts read every earlier place too).
func TestOutcomeElsewhereLearnsNothingBesideAFineConflictAnywhere(t *testing.T) {
	const p = "body.filter.since"
	rec, live := iMicro(eRAt.Add(-3*time.Millisecond)), iMicro(eFAt.Add(-4*time.Millisecond))
	moved := iMicro(eFAt.Add(-2 * time.Second))
	held := []Earlier{{Mock: "mock-14", At: eRAt.Add(-time.Second), Run: true,
		Seen: []Seen{{Side: Request, Leaf: oText("body.at", rec), Live: moved}}}}
	in := iCall(p, String, rec, live, eRAt, eFAt, iWin(eRAt, eFAt, 50*time.Millisecond), held, nil)
	in.Mode = ModeElsewhere
	eNothing(t, "a microsecond another test held whole, F moved", Decide(in), "mock-14")
	// The control: with no such place the window holds it, a Clock, kept.
	in.Earlier = nil
	iClock(t, "the same read, no earlier place", oWord(t, Decide(in), rec, live))
}

// A word the timer rule keeps nothing of, refused for that alone, carries a
// kind of its own: an adapter tells it from a word that could not be read.
func TestOutcomeElsewhereAWordItDoesNotKeepIsRefusedAsLandedElsewhere(t *testing.T) {
	ds := Decide(eLease(eRAt, eFAt, "session-janitor:968290", "pat-janitor:968290", nil))
	if len(ds) == 0 {
		t.Fatal("no decision")
	}
	for _, d := range ds {
		if d.Refusal == nil || d.Refusal.Kind != RefusalLandedElsewhere {
			t.Errorf("%s; want refused as %s", oDescribe([]Decision{d}), RefusalLandedElsewhere)
			continue
		}
		if r, ok := ParseRefusal(d.Line); !ok || r.Kind != RefusalLandedElsewhere {
			t.Errorf("%q read back as %+v, %v", d.Line, r, ok)
		}
	}
}
