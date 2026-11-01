package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows for the one entry's order on an instant (Decide): a conflict at a fine
// cut, a pick, a templated slot, an answer, a copy at a fine cut, the call's
// own window, a copy at a coarse cut, the test's begin, and the test's own
// value, in that order. Each row names its outcome exactly, so none passes on
// an entry that refuses every word. Every window here is a plain read with
// a time to live that main's reading and the later clock reading read alike:
// look-back and own-cut offsets are the clock rows'.

// iCall is the call at path (a leaf of type typ), recorded at rAt holding rec
// and seen in record's full round arriving at fAt holding live, in window w.
func iCall(path string, typ Type, rec, live string, rAt, fAt time.Time, w *Window, earlier []Earlier, raw []Raw) Input {
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Noise: path, Type: typ, Value: v}}}
	}
	return Input{
		Drifts: oDrifts(), Drift: []string{path},
		Live: leaf(live), Rec: leaf(rec),
		At: rAt, Arrived: fAt, Earlier: earlier, Raw: raw, Window: w,
		Test: "test-2", Mock: "mock-300",
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	}
}

// iSent is a request the call's own test sent at at (as recorded), its leaf
// path holding rec, and holding live as round F sent it.
func iSent(mock string, at time.Time, path string, typ Type, rec, live string) Earlier {
	return Earlier{Mock: mock, At: at, Own: true, Seen: []Seen{{Side: Request,
		Leaf: Leaf{Path: path, Noise: path, Type: typ, Value: rec}, Live: live}}}
}

// iWin is a window: the recording's span ends at rAt, round F's at fAt, each
// back by span.
func iWin(rAt, fAt time.Time, span time.Duration) *Window {
	return &Window{Rec: Span{From: rAt.Add(-span), To: rAt}, F: Span{From: fAt.Add(-span), To: fAt}}
}

func iMs(t time.Time) string    { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func iMicro(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }
func iEpochMs(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}
func iEpochNs(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// iTimeless is oAnswer's recording with no answer time.
func iTimeless(name string, before time.Duration, word string) Raw {
	return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: oAt.Add(-before),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT next_slot FROM " + name,
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: word}}}, CommandComplete: "SELECT 1"},
		}},
	}}, false)
}

// iClock checks d is a Clock with its check set.
func iClock(t *testing.T, what string, d Decision) {
	t.Helper()
	if d.Outcome != OutcomeClock || d.Clock == nil {
		t.Errorf("%s: %s; want a Clock with its check", what, oDescribe([]Decision{d}))
	}
}

// iValue checks d is a Value bound at mock.
func iValue(t *testing.T, what string, d Decision, mock string) {
	t.Helper()
	if d.Outcome != OutcomeValue || d.From.Mock != mock {
		t.Errorf("%s: %s; want a Value bound at %s", what, oDescribe([]Decision{d}), mock)
	}
}

// A background call's look-back ("dirtyAt <= now-2s", a $numberLong of
// milliseconds) that round F made at another phase of its second than the
// recording did: the call's window holds it 2 s back in both runs, so it is
// a Clock at -2 s, never a word made anew each run.
func TestOutcomeALookBackReadAtAnotherPhaseIsAClock(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 800*time.Millisecond)
	rec, live := iEpochMs(rAt.Add(-2*time.Second-5*time.Millisecond)), iEpochMs(fAt.Add(-2*time.Second-7*time.Millisecond))
	// Mongo's replay compares an offset (Window.ComparesOffsets); one that
	// does not refuses it as one it cannot check.
	in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 20*time.Millisecond).ComparesOffsets(), nil, nil)
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "dirtyAt <= now-2s at another phase", d)
	in.Window = iWin(rAt, fAt, 20*time.Millisecond)
	oRefused(t, "dirtyAt <= now-2s where the replay compares no offset", oWord(t, Decide(in), rec, live), "cannot-check")
	in.Window = iWin(rAt, fAt, 20*time.Millisecond).ComparesOffsets()
	if d.Outcome == OutcomeClock && d.Clock != nil && (d.Clock.Min > -2*time.Second || d.Clock.Max < -2*time.Second) {
		t.Errorf("dirtyAt <= now-2s: the Clock's offset [%v, %v]; want it to hold -2s", d.Clock.Min, d.Clock.Max)
	}
}

// A look-back the app cut to its second or its millisecond (now-2s and
// now-90s as Unix seconds, 2 s as Unix milliseconds) is a read of the clock:
// a Clock, or refused as no clock read where the call's window cannot hold
// it, or as one keploy cannot check where the replay compares no offset;
// never a word made anew each run and never a Value (a separate read).
// Which offset is learned is the clock rows'.
func TestOutcomeALookBackIsNeverFreshNorAValue(t *testing.T) {
	const p = "body.filter.lastActive.$lt"
	rAt := oAt.Add(400 * time.Millisecond)
	fAt := oAt.Add(2*time.Hour + 150*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	for _, tc := range []struct {
		what, rec, live string
	}{
		{"now-2s in seconds", secs(rAt.Add(-2 * time.Second)), secs(fAt.Add(-2 * time.Second))},
		{"now-90s in seconds", secs(rAt.Add(-90 * time.Second)), secs(fAt.Add(-90 * time.Second))},
		{"now-2s in milliseconds", iEpochMs(rAt.Add(-2 * time.Second)), iEpochMs(fAt.Add(-2 * time.Second))},
	} {
		for _, w := range []*Window{iWin(rAt, fAt, 30*time.Millisecond), nil} {
			d := oWord(t, Decide(iCall(p, Number, tc.rec, tc.live, rAt, fAt, w, nil, nil)), tc.rec, tc.live)
			switch {
			case d.Outcome == OutcomeClock:
			case d.Outcome == OutcomeRefused && d.Refusal != nil && (d.Refusal.Kind == "no-clock-read" || d.Refusal.Kind == "cannot-check"):
			default:
				t.Errorf("%s, window %v: %s; want a Clock, or refused as no clock read or as one keploy cannot check", tc.what, w != nil, oDescribe([]Decision{d}))
			}
		}
	}
}

// A departure the app picked from a timetable an answer gave, which listed
// round F's departure but not the recorded one: F's moment stands unchanged
// at that answer, which does not show it. A pick, refused naming the place,
// before any window (http learn_test.go:4108 "one unlisted at record", at
// learn level).
func TestOutcomeAPickFromAnAnswerIsRefused(t *testing.T) {
	const p = "body.departure"
	rAt := oAt.Add(200 * time.Millisecond)
	fAt := oAt.Add(time.Hour)
	for _, tc := range []struct {
		what, rec, live string
		table           map[string]string
	}{
		{"a time of day", "2026-10-14 10:01", "2026-10-14 10:02", map[string]string{
			"body.date": "2026-10-14", "body.departures[0]": "10:00", "body.departures[1]": "10:02", "body.departures[2]": "10:03"}},
		{"an instant", "2026-10-14T10:01:00Z", "2026-10-14T10:02:00Z", map[string]string{
			"body.departures[0]": "2026-10-14T10:00:00Z", "body.departures[1]": "2026-10-14T10:02:00Z", "body.departures[2]": "2026-10-14T10:03:00Z"}},
	} {
		e := Earlier{Mock: "mock-0", At: rAt.Add(-100 * time.Millisecond), Own: true}
		for path, v := range tc.table {
			e.Seen = append(e.Seen, Seen{Side: Response, Leaf: oText(path, v), Live: v})
		}
		in := iCall(p, String, tc.rec, tc.live, rAt, fAt, iWin(rAt, fAt, time.Hour), []Earlier{e}, nil)
		d := oWord(t, Decide(in), "01", "02")
		oRefused(t, tc.what+": a departure F picked from mock-0's timetable", d, "pick", "mock-0", "body.departures[1]")
	}
}

// Two reads one database round trip apart that share a millisecond in both
// runs: the earlier send and the call each hold the same millisecond, and
// the call's own window holds it. At a coarse cut that is no copy: a Clock
// each, checked in its own span, never bound to the other.
func TestOutcomeTwoReadsInOneMillisecondAreAClockEach(t *testing.T) {
	const p = "body.updatedAt"
	rAt := oAt.Add(500 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 600*time.Microsecond)
	rec, live := iMs(rAt.Add(-200*time.Microsecond)), iMs(fAt.Add(-300*time.Microsecond))
	sent := []Earlier{iSent("mock-1", rAt.Add(-400*time.Microsecond), "body.createdAt", String, rec, live)}
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), sent, nil)), rec, live)
	iClock(t, "a millisecond both reads share, the window holds it", d)
}

// A millisecond read carried to a later send that its own window does not
// hold (a read 40 ms before the call, the window 10 ms) is bound to its first
// send at the coarse copy step, before any begin or fresh word. So it
// is where another flow's call came between the read and the send and pushed
// the read out of the window: it binds to the earlier send in the same cut,
// never fresh, and the replay goes red when the two cross the cut:
// it fails closed.
func TestOutcomeACarriedMillisecondReadBindsToItsFirstSend(t *testing.T) {
	const p = "body.since"
	rAt := oAt.Add(700 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 300*time.Millisecond)
	rec, live := iMs(rAt.Add(-45*time.Millisecond)), iMs(fAt.Add(-46*time.Millisecond))
	sent := []Earlier{iSent("mock-1", rAt.Add(-44*time.Millisecond), "body.createdAt", String, rec, live)}
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 10*time.Millisecond), sent, nil)), rec, live)
	iValue(t, "a millisecond read carried 40 ms past its window", d, "mock-1")
	// Another flow's call 5 ms before the call starts the window there: the
	// read the app made before it binds at the coarse copy step all the same.
	w := &Window{Rec: Span{From: rAt.Add(-5 * time.Millisecond), To: rAt}, F: Span{From: fAt.Add(-5 * time.Millisecond), To: fAt}}
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, sent, nil)), rec, live)
	iValue(t, "a re-read pushed out of its window by another flow's call", d, "mock-1")
}

// A recorded millisecond that stood whole at an earlier send which round F
// moved, while the call sent its own: at a coarse cut two reads may share
// one millisecond, so that place is a coincidence, never a conflict; the
// call's window holds the read, a Clock (two reads that share a millisecond are no conflict).
func TestOutcomeAMillisecondCutIsNoConflict(t *testing.T) {
	const p = "body.updatedAt"
	rAt := oAt.Add(500 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 600*time.Microsecond)
	rec, live := iMs(rAt.Add(-200*time.Microsecond)), iMs(fAt.Add(-300*time.Microsecond))
	moved := iMs(fAt.Add(-9 * time.Millisecond))
	sent := []Earlier{iSent("mock-1", rAt.Add(-400*time.Microsecond), "body.createdAt", String, rec, moved)}
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), sent, nil)), rec, live)
	iClock(t, "a millisecond F moved at an earlier send", d)
}

// A microsecond read the test sent earlier and the call carries in both runs
// is one read copied: bound to its first send, before the call's window is
// read, even where the window holds it too (at a cut finer than a millisecond).
func TestOutcomeAMicrosecondCopyBindsBeforeTheWindow(t *testing.T) {
	const p = "body.since"
	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	rec, live := iMicro(rAt.Add(-600*time.Microsecond)), iMicro(fAt.Add(-610*time.Microsecond))
	sent := []Earlier{iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.createdAt", String, rec, live)}
	for _, w := range []*Window{iWin(rAt, fAt, 800*time.Microsecond), nil} {
		d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, sent, nil)), rec, live)
		iValue(t, "a microsecond copy, window "+strconv.FormatBool(w != nil), d, "mock-1")
	}
}

// api-server's harness names an app "[E2E] app-crud-<UnixNano>" (19 digits,
// a fine cut) after the test began, and sends it at a create and then at a
// read. The read binds to the create's send (step 4), whether a worker poll
// arrived between the clock read and the create (the create's window then
// does not hold it) or not: load never decides between a checked Value and
// an unchecked Clock.
func TestOutcomeANanosecondNameCarriedToALaterSendBindsToItsFirstSend(t *testing.T) {
	const p = "body.name"
	rAt, fAt := oAt.Add(40*time.Millisecond), oAt.Add(time.Hour+45*time.Millisecond)
	made, fmade := oAt.Add(-30*time.Millisecond), oAt.Add(time.Hour-31*time.Millisecond)
	rec, live := "[E2E] app-crud-"+iEpochNs(made), "[E2E] app-crud-"+iEpochNs(fmade)
	create := []Earlier{iSent("mock-41", oAt.Add(-10*time.Millisecond), "body.name", String, rec, live)}
	for _, w := range []*Window{iWin(rAt, fAt, 5*time.Millisecond), iWin(rAt, fAt, 100*time.Millisecond)} {
		d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, create, nil)), iEpochNs(made), iEpochNs(fmade))
		iValue(t, "a nanosecond name carried, window from "+w.Rec.From.Format("05.000"), d, "mock-41")
	}
}

// A carried millisecond read (the call's own window does not hold it) that
// two earlier sends of the test hold in both runs, one of them its own read
// of the clock (learned a Clock there): two separate reads, so the word is
// refused naming both, never bound to either.
func TestOutcomeAReReadBesideACarriedReadIsTwoSources(t *testing.T) {
	const p = "body.since"
	rAt, fAt := oAt.Add(300*time.Millisecond), oAt.Add(time.Hour+320*time.Millisecond)
	rec, live := iMs(oAt.Add(-200*time.Millisecond)), iMs(oAt.Add(time.Hour-205*time.Millisecond))
	carried := iSent("mock-4", oAt.Add(-150*time.Millisecond), "body.createdAt", String, rec, live)
	reread := iSent("mock-7", oAt.Add(-120*time.Millisecond), "body.updatedAt", String, rec, live)
	reread.Seen[0].Clock = true
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), []Earlier{carried, reread}, nil)), rec, live)
	oRefused(t, "a carried read beside a re-read of the same millisecond", d, "two-sources", "mock-4", "mock-7")
}

// A 19-digit UnixNano (a fine cut) that stood whole at the test's earlier
// send, which round F moved to another value while the call sent a third:
// the value did not follow its place. A conflict, refused before any bind
// (client_bind_test.go:75-81 at learn level, where main binds to mock-1: a nanosecond cut is fine).
func TestOutcomeAFineCutThatStoppedFollowingIsAConflict(t *testing.T) {
	const p = "body.cmd.query.app.name"
	rAt := oAt
	fAt := oAt.Add(61 * time.Second)
	rec := "[E2E] app-crud-" + iEpochNs(rAt.Add(-300*time.Millisecond))
	moved := "[E2E] app-crud-" + iEpochNs(fAt.Add(-400*time.Millisecond))
	live := "[E2E] app-crud-" + iEpochNs(fAt.Add(-200*time.Millisecond))
	sent := []Earlier{iSent("mock-1", rAt.Add(-250*time.Millisecond), "body.document.name", String, rec, moved)}
	in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 900*time.Millisecond), sent, nil)
	in.Drifts = oClientDrifts()
	d := oWord(t, Decide(in), iEpochNs(rAt.Add(-300*time.Millisecond)), iEpochNs(fAt.Add(-200*time.Millisecond)))
	oRefused(t, "a UnixNano F moved at mock-1", d, "conflict", "mock-1")
}

// An instant an answer of the call's own test gave before the call, whose
// text round F's adapter did not state: an answer's instant is the
// dependency's value, bound there before any window (step 3). The same
// answer with no answer time is a place whose state keploy cannot tell: a
// pick, refused naming it (step 1 (iii)).
func TestOutcomeAnAnswersInstantBindsWhereFsViewIsNotStated(t *testing.T) {
	const p = "bind.$2"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 300*time.Millisecond)
	rec, live := iMs(rAt.Add(-50*time.Millisecond)), iMs(fAt.Add(-50*time.Millisecond))
	ans := oAnswer("mock-7", 100*time.Millisecond, rec)
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 60*time.Millisecond), nil, []Raw{ans.ServedAs(1)})), rec, live)
	iValue(t, "an answer's instant, F's view not stated", d, "mock-7")
	timeless := iTimeless("mock-7", 100*time.Millisecond, rec)
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 60*time.Millisecond), nil, []Raw{timeless.ServedAs(1)})), rec, live)
	oRefused(t, "an answer's instant with no answer time", d, "pick", "mock-7")
}

// An answer that holds only the date of a time the call sends (a timetable's
// day, its times of day neither run's): the date reads as the recorded time
// cut to a day, but the time of day the app wrote beside it is none of the
// answer's, and it is all that changed. Bound to the answer, a replay would
// check the day alone, and any time that day would pass: the answer is no
// source. With no window that holds it the time is refused; where the call's
// window holds it, it is the app's own clock read (instants_test.go:1064).
func TestOutcomeAServedDateIsNoSourceOfTheTimeOfDayBesideIt(t *testing.T) {
	const p = "body.departure"
	rAt := oAt.Add(2 * time.Hour)
	fAt := rAt.Add(time.Hour)
	rec, live := iMs(rAt.Add(-50*time.Millisecond)), iMs(fAt.Add(-50*time.Millisecond))
	day := oAnswer("mock-7", 100*time.Millisecond, oAt.Format("2006-01-02"))
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, nil, nil, []Raw{day.ServedAs(1)})), rec, live)
	if d.Outcome == OutcomeValue {
		t.Errorf("FALSE GREEN: a time bound to the date beside it: %s", oDescribe([]Decision{d}))
	}
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 60*time.Millisecond), nil, []Raw{day.ServedAs(1)})), rec, live)
	iClock(t, "a time the call's window holds, beside a served date", d)
	// The same answer holding the time itself still binds it.
	whole := oAnswer("mock-7", 100*time.Millisecond, rec)
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, nil, nil, []Raw{whole.ServedAs(1)})), rec, live)
	iValue(t, "an answer holding the time itself", d, "mock-7")
}

// A request sent before the call that held the recorded minute and, in round
// F, grew to hold that minute and F's ("since R" to "since R until F"): it
// did not move with the value, so the value is no copy of it. Where it moved
// ("since R" to "since F") it is (instants_test.go:1507).
func TestOutcomeASendThatHoldsBothRunsMomentsIsNoCopy(t *testing.T) {
	const p = "body.at"
	rAt := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	rec, live := "2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"
	sent := func(f string) []Earlier {
		return []Earlier{iSent("mock-1", rAt.Add(-time.Second), "body.range", String, "since 1792667640000", f)}
	}
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, rAt, nil, sent("since 1792667700000"), nil)), rec, live)
	iValue(t, "a send that moved with the value", d, "mock-1")
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, rAt, nil, sent("since 1792667640000 until 1792667700000"), nil)), rec, live)
	if d.Outcome == OutcomeValue {
		t.Errorf("a send that holds both runs' moments in F bound the value: %s", oDescribe([]Decision{d}))
	}
}

// A number of an instant's length that does not parse as one (a 10-digit
// number past 2100) is matched by its exact text: the test's earlier send of
// the same text in each run binds it; one that held the recorded text whole
// and that round F moved is a conflict.
func TestOutcomeANumberThatDoesNotParseIsMatchedByItsText(t *testing.T) {
	const p = "body.ref"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour)
	const rec, live, moved = "9999999999", "9999999998", "9999999997"
	sent := []Earlier{iSent("mock-1", rAt.Add(-10*time.Millisecond), "body.seq", Number, rec, live)}
	d := oWord(t, Decide(iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), sent, nil)), rec, live)
	iValue(t, "an unparsed 10-digit number sent earlier", d, "mock-1")
	sent = []Earlier{iSent("mock-1", rAt.Add(-10*time.Millisecond), "body.seq", Number, rec, moved)}
	d = oWord(t, Decide(iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), sent, nil)), rec, live)
	oRefused(t, "an unparsed 10-digit number F moved at mock-1", d, "conflict", "mock-1")
}

// A client test's own UnixNano that no earlier place holds in either run,
// with no begin: the test's own value, a fresh Value at the call that first
// sends it (client_bind_test.go:23-74, step 8). A later send of it binds to
// that first send at the fine copy step.
func TestOutcomeAClientTestsOwnValueIsFreshAtItsFirstSend(t *testing.T) {
	const p = "body.cmd.query.app.name"
	rAt := oAt
	fAt := oAt.Add(61 * time.Second)
	recN, liveN := iEpochNs(rAt.Add(-300*time.Millisecond)), iEpochNs(fAt.Add(-200*time.Millisecond))
	rec, live := "[E2E] app-crud-"+recN, "[E2E] app-crud-"+liveN
	in := iCall(p, String, rec, live, rAt, fAt, nil, nil, nil)
	in.Drifts = oClientDrifts()
	if d := oWord(t, Decide(in), recN, liveN); d.Outcome != OutcomeFresh {
		t.Errorf("the client test's own value, first sent: %s; want fresh", oDescribe([]Decision{d}))
	}
	in.Earlier = []Earlier{iSent("mock-1", rAt.Add(-250*time.Millisecond), "body.document.name", String, rec, live)}
	iValue(t, "the client test's own value, sent again", oWord(t, Decide(in), recN, liveN), "mock-1")
}

// A client test's instant that no earlier place holds and its answer does
// not echo is the test's own value only where it was made before the test
// began: one past the begin in either run (an audit time an hour past its
// call, as api-server's client test writes one) is a read of the clock no
// window of keploy's holds, or a value the test gave, and is refused,
// naming the run; one before each run's begin is fresh. A call whose runs
// know no begin keeps the test's own value fresh, wherever it lies
// (client_bind_test.go:23-74).
func TestOutcomeAClientTestsValueMadeAfterItsBeginIsNotItsOwn(t *testing.T) {
	const p = "body.auditedAt"
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	w := iWin(rAt, fAt, 50*time.Millisecond)
	w.Rec.Began, w.F.Began = rAt.Add(-5*time.Second), fAt.Add(-5*time.Second)
	decide := func(w *Window, rec, live string) Decision {
		in := iCall(p, String, rec, live, rAt, fAt, w, nil, nil)
		in.Drifts = oClientDrifts()
		return oWord(t, Decide(in), rec, live)
	}
	for _, c := range []struct {
		what, rec, live, run string
	}{
		{"an hour past its call in both runs", iMs(rAt.Add(time.Hour)), iMs(fAt.Add(time.Hour + 7*time.Second)), "the recorded one lies past its test's begin"},
		{"before the begin when recorded, an hour past its call in round F", iMs(rAt.Add(-time.Minute)), iMs(fAt.Add(time.Hour)), "round F's lies past its test's begin"},
		{"4 s after its test began when recorded, an hour past its call in round F", iMs(rAt.Add(-4 * time.Second)), iMs(fAt.Add(time.Hour)), "the recorded one lies past its test's begin"},
	} {
		d := decide(w, c.rec, c.live)
		oRefused(t, "a client test's time "+c.what, d, "no-clock-read")
		if want := "nor the test's own value, made before it began: " + c.run; d.Refusal != nil && !strings.Contains(d.Refusal.Why, want) {
			t.Errorf("a client test's time %s: %q; want it to say %q", c.what, d.Refusal.Why, want)
		}
	}
	if d := decide(w, iMs(rAt.Add(-2*time.Minute)), iMs(fAt.Add(-3*time.Minute))); d.Outcome != OutcomeFresh {
		t.Errorf("a client test's time made before its begin in both runs: %s; want fresh", oDescribe([]Decision{d}))
	}
	if d := decide(iWin(rAt, fAt, 50*time.Millisecond), iMs(rAt.Add(-time.Minute)), iMs(fAt.Add(time.Hour))); d.Outcome != OutcomeFresh {
		t.Errorf("a client test's time an hour past its call in round F, no begin known: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// A client test's read of the clock made earlier in the test than its call's
// own window, after the test began in both runs (2 s before the call in the
// recording, 3 s in round F: no one offset), that no earlier place holds: a
// Clock checked from the test's begin (models.SinceTestBegin). The same
// read in a keploy test, whose calls are not read from a begin, is refused:
// no clock read, never fresh.
func TestOutcomeAClientTestsReadSinceItsBeginIsAClock(t *testing.T) {
	const p = "body.cmd.query.dueBefore"
	rAt, fAt := oAt, oAt.Add(61*time.Second+337*time.Millisecond)
	rec, live := iMs(rAt.Add(-2130*time.Millisecond)), iMs(fAt.Add(-3410*time.Millisecond))
	w := iWin(rAt, fAt, 50*time.Millisecond)
	w.Rec.Began, w.F.Began = rAt.Add(-5*time.Second), fAt.Add(-5*time.Second)
	in := iCall(p, String, rec, live, rAt, fAt, w, nil, nil)
	in.Drifts = oClientDrifts()
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "a client test's read 2 s and 3 s before its call, after its begin", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("a client test's read since its begin: checked since %v; want since the test began", d.Clock.Since)
	}
	// Began after the read in round F: the window from the begin does not
	// hold it.
	late := *w
	late.F.Began = fAt.Add(-3 * time.Second)
	in.Window = &late
	if d := oWord(t, Decide(in), rec, live); d.Outcome == OutcomeClock || d.Outcome == OutcomeValue {
		t.Errorf("a client test's read before round F's begin: %s; want no Clock and no Value", oDescribe([]Decision{d}))
	}
	in.Drifts, in.Window = oDrifts(), w
	oRefused(t, "a keploy test's read 2 s and 3 s before its call", oWord(t, Decide(in), rec, live), "no-clock-read")
}

// A read plus a time to live, cut to the minute in Europe/Berlin, in the
// hour its clocks go back (2026-10-25, 03:00 CEST to 02:00 CET): an
// application that resolves a wall time shown twice to its first showing
// writes it an hour early there, so the offset both runs show can stand an
// hour from the one it adds. Refused, naming the turn, as main refuses it,
// whichever reading found the offset; the same read a day later is a Clock.
func TestOutcomeAnOffsetNearAWallTimeShownTwiceIsRefused(t *testing.T) {
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	const p = "body.expiresAt"
	ms := time.Millisecond
	val := func(read time.Time) time.Time { return firstShowingIn(read, time.Minute, ber).Add(2 * time.Hour) }
	decide := func(call time.Time) Decision {
		t.Helper()
		dc := oDrifts()
		dc.zone = ber
		fcall := call.Add(FullRoundLead)
		w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
		leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, ms)}} }
		ds := Decide(Input{Drifts: dc, Drift: []string{p}, Live: leaf(val(fcall.Add(-30 * ms))), Rec: leaf(val(call.Add(-30 * ms))),
			At: call, Arrived: fcall, Window: w, Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}})
		if len(ds) != 1 {
			t.Fatalf("at %s: %d decisions (%s); want one", call, len(ds), oDescribe(ds))
		}
		return ds[0]
	}
	turn := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	for _, call := range []time.Time{turn.Add(10*time.Minute + 7300*ms), turn.Add(40*time.Minute + 7300*ms)} {
		d := decide(call)
		oRefused(t, "a time to live read in the hour shown twice", d, "no-clock-read")
		if !strings.Contains(d.Line, "keploy refuses it rather than learn it") {
			t.Errorf("at %s: %q; want the line to name the turn", call, d.Line)
		}
	}
	if d := decide(time.Date(2026, 10, 26, 10, 0, 7, 300e6, time.UTC)); d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Min > 2*time.Hour || d.Clock.Max < 2*time.Hour {
		t.Errorf("the same read a day later: %s; want a Clock, plus two hours", oDescribe([]Decision{d}))
	}
}

// A call that carries no app clock (no window: the adapter read none) is
// refused naming that cause, never in the words of a window that does not
// hold the time; a window that does not hold it keeps those words.
func TestOutcomeACallWithNoAppClockSaysSo(t *testing.T) {
	const p = "body.at"
	rAt, fAt := oAt, oAt.Add(FullRoundLead)
	// R and F apart by no constant offset from the call: no read plus an
	// offset either.
	rec, live := "2026-10-31T08:50:00Z", "2026-10-31T08:13:00Z"
	for _, tc := range []struct {
		what, want, not string
		w               *Window
	}{
		{"no window", "the call carries no app clock", "window does not hold", nil},
		{"a window that does not hold it", "a time its call's window does not hold in both runs", "no app clock", iWin(rAt, fAt, 20*time.Millisecond)},
	} {
		d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, tc.w, nil, nil)), rec, live)
		oRefused(t, tc.what, d, "no-clock-read", tc.want)
		if d.Refusal != nil && strings.Contains(d.Refusal.Why, tc.not) {
			t.Errorf("%s: refused %q; want no %q", tc.what, d.Refusal.Why, tc.not)
		}
	}
}

// An instant written percent-encoded ("10%3A00%3A00.123456Z", a link's
// query in a body) is read as the instant it opens to, not as the plain
// words "3A00" and "3A01" beside it: read in its call's window in both runs
// it is a Clock, named through the step that opens it. Sent before the call
// at the test's earlier place, also encoded, where round F moved it while
// the call sent a third value, it stopped following: a conflict at a fine
// cut, refused.
func TestOutcomeAPercentEncodedInstantIsReadAsTheInstantItOpensTo(t *testing.T) {
	const p = "body.next"
	pct := func(t time.Time) string { return strings.ReplaceAll(iMicro(t), ":", "%3A") }
	link := func(t time.Time) string { return "/v1/events?since=" + pct(t) }
	rAt := oAt
	fAt := oAt.Add(61 * time.Second)
	rRead, fRead := rAt.Add(-300*time.Millisecond), fAt.Add(-200*time.Millisecond)
	named := func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
	in := iCall(p, String, link(rRead), link(fRead), rAt, fAt, iWin(rAt, fAt, 900*time.Millisecond), nil, nil)
	in.PlaceOf = named
	ds := Decide(in)
	d := oWord(t, ds, iMicro(rRead), iMicro(fRead))
	iClock(t, "a percent-encoded microsecond read in its window", d)
	if !strings.Contains(d.At.String(), "|pct") {
		t.Errorf("a percent-encoded instant: At %q; want it named through the pct step", d.At)
	}
	sent := []Earlier{iSent("mock-1", rAt.Add(-250*time.Millisecond), "body.cursor", String, pct(rRead), pct(fAt.Add(-400*time.Millisecond)))}
	in = iCall(p, String, link(rRead), link(fRead), rAt, fAt, iWin(rAt, fAt, 900*time.Millisecond), sent, nil)
	in.PlaceOf = named
	ds = Decide(in)
	for _, d := range ds {
		if d.Outcome != OutcomeRefused {
			t.Errorf("FALSE GREEN? a percent-encoded microsecond mock-1 sent encoded and F moved: %s; want every change refused", oDescribe(ds))
		}
	}
	d = oWord(t, ds, iMicro(rRead), iMicro(fRead))
	oRefused(t, "a percent-encoded microsecond mock-1 sent encoded and F moved", d, "conflict", "mock-1")
}

// A read of now minus a debounce, in Unix seconds, two seconds after an
// earlier send of the same second (a reconciler's claim, dirtyAt <= now-2s,
// beside the mark that set dirtyAt = now): the call's own read, decided
// before a coarse copy, in both modes. Bound to the mark, a replay would go
// red whenever the two reads fall in different seconds. The offset is the
// clock reading's; a stub reading stands in for it (main's reading refuses
// a look-back cut to the second as too wide to check; the later clock
// reading learns it). Where the reading finds no offset, the copy still
// binds.
func TestOutcomeAReadMinusADebounceIsTheCallsClockBeforeACoarseCopy(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt := oAt.Add(400 * time.Millisecond)
	fAt := oAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec, live := secs(rAt.Add(-2*time.Second)), secs(fAt.Add(-2*time.Second))
	mark := iSent("mock-299", rAt.Add(-2*time.Second+100*time.Millisecond), "body.updates.0.u.$set.dirtyAt", Number, rec, live)
	in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond).ComparesOffsets(), []Earlier{mark}, nil)
	in.Landed = Landed{Where: LandedBetween}
	defer func(f func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool)) {
		clockInWindow = f
	}(clockInWindow)
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{Min: -2010 * time.Millisecond, Max: -1990 * time.Millisecond, Res: time.Millisecond}, "", true
	}
	for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
		in.Mode = mode
		d := oWord(t, Decide(in), rec, live)
		iClock(t, "now minus two seconds beside a mark of the same second", d)
		if d.Clock != nil && (d.Clock.Min > -2*time.Second || d.Clock.Max < -2*time.Second) {
			t.Errorf("mode %v: the Clock's offset is %v..%v; want the reading's, around -2s", mode, d.Clock.Min, d.Clock.Max)
		}
	}
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{}, "", false
	}
	in.Mode = ModeOwn
	iValue(t, "the same read where the clock reading finds no offset", oWord(t, Decide(in), rec, live), "mock-299")
}
