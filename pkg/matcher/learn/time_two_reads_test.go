package learn

import (
	"strings"
	"testing"
	"time"
)

// Rows of a whole decoder-typed time refused as a conflict: its recorded
// value stood whole at an earlier place, and round F's call did not carry
// what round F gave there. An insert sent createdAt (an epoch in
// nanoseconds) and, later in the test, created_at (a BSON datetime): two
// reads of the clock that met at the millisecond when recorded. Round F
// sent another moment at createdAt, and the call yet another: two reads,
// which the time default checks by form. Where round F repeated the
// recorded value at the place, or the place is no send of the
// application's own, the value had a source the call did not follow: it
// stays refused.

const (
	trPath  = "body.docs.msgs.0.created_at.$date.$numberLong"
	trPlace = "body.docs.msgs.0.createdAt.$numberLong"
)

// trTimes are the call's moments (rAt, fAt), its values (rec, live: the
// call's own reads, a BSON datetime at the millisecond), and the earlier
// place's values (placeRec: the recorded read, 79.666 us into the call's
// recorded millisecond, which a cut at 100 us names, as an api-server e2e
// recording held it; placeLive: round F's read there, 61.4 ms before the
// call's).
func trTimes() (rAt, fAt time.Time, rec, live, placeRec, placeLive string) {
	rAt, fAt = oAt.Add(300*time.Millisecond), oAt.Add(time.Hour+320*time.Millisecond)
	recRead := oAt.Add(-200 * time.Millisecond)
	liveRead := oAt.Add(time.Hour - 141*time.Millisecond)
	return rAt, fAt, iMs(recRead), iMs(liveRead), iEpochNs(recRead.Add(79666 * time.Nanosecond)), iEpochNs(liveRead.Add(-62*time.Millisecond + 595011*time.Nanosecond))
}

// trCall is the call at path, of type typ, with the earlier places ps.
func trCall(path string, typ Type, ps ...Earlier) Input {
	rAt, fAt, rec, live, _, _ := trTimes()
	return iCall(path, typ, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), ps, nil)
}

// trSent is the insert that sent createdAt before the call: as recorded,
// and in round F live (another read, or the recorded value repeated).
func trSent(live string) Earlier {
	_, _, _, _, placeRec, _ := trTimes()
	return iSent("mock-1787", oAt.Add(-150*time.Millisecond), trPlace, Number, placeRec, live)
}

// trGiven is a place of the test's: the test case's request (the insert's
// read, sent by the test), or a value the test gave keploy (gave: the call's
// recorded text, which a given holds by), and in round F another value.
func trGiven(gave bool) Earlier {
	_, _, rec, _, placeRec, placeLive := trTimes()
	v := placeRec
	if gave {
		v = rec
	}
	return Earlier{Mock: "test case post-x", Given: true, Gave: gave, At: oAt.Add(-time.Second),
		Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: Number, Value: v}, Live: placeLive}}}
}

// trAnswer is an answer the application read before the call, holding the
// call's recorded value, and in round F another value.
func trAnswer() Earlier {
	_, _, _, _, placeRec, placeLive := trTimes()
	e := iSent("mock-1786", oAt.Add(-170*time.Millisecond), "body.cursor.firstBatch.0.createdAt.$numberLong", Number, placeRec, placeLive)
	e.Seen[0].Side = Response
	return e
}

func TestAWholeTypedTimeOfTwoReadsThatMetWhenRecordedIsCheckedByForm(t *testing.T) {
	_, _, rec, _, placeRec, placeLive := trTimes()
	form := TimeForm(Leaf{Type: Instant, Value: rec})
	if form == "" {
		t.Fatalf("precondition: %q has no form", rec)
	}

	// The insert round F sent another read at: kept by form under the
	// default, its line naming the clock checks' conflict and the place.
	in := trCall(trPath, Instant, trSent(placeLive))
	tStrictThenDefault(t, "an insert's createdAt round F sent another read at", in, RefusalConflict, true, form)
	in.TimeForm = true
	if ds := Decide(in); len(ds) == 1 && !strings.Contains(ds[0].Line, "mock-1787") {
		t.Errorf("an insert's createdAt round F sent another read at: line %q; want it to name the place", ds[0].Line)
	}

	for _, c := range []struct {
		what string
		in   Input
	}{
		// Round F sent the recorded value again there: the call did not
		// follow a place that held still.
		{"an insert's createdAt round F sent as recorded", trCall(trPath, Instant, trSent(placeRec))},
		// A value the test gave keploy, or the test case's request: the
		// test's own value, which the call did not carry.
		{"a value the test gave keploy", trCall(trPath, Instant, trGiven(true))},
		{"the test case's request", trCall(trPath, Instant, trGiven(false))},
		// An answer: a dependency's value, which the call did not carry.
		{"an answer round F gave another value at", trCall(trPath, Instant, trAnswer())},
		// A moved send beside a place that held still: one source the call
		// did not follow is enough.
		{"a moved insert beside the test's value", trCall(trPath, Instant, trSent(placeLive), trGiven(true))},
		// At a key: an id, which keeps its learned check.
		{"a moved insert, the call's time under _id", trCall("body.docs.msgs.0._id.$date.$numberLong", Instant, trSent(placeLive))},
		// No decoder-typed time: a text in a clock layout, found by its
		// shape alone.
		{"a moved insert, the call's value a text", trCall(trPath, String, trSent(placeLive))},
	} {
		tStrictThenDefault(t, c.what, c.in, RefusalConflict, false, "")
	}
}
