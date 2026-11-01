package learn

import (
	"fmt"
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
// recording did: the call's window holds it 2 s back in both runs, but the
// offset reads in seconds at a millisecond's resolution, so it is refused
// as main refuses it (a read made outside the call's window, seconds before
// or after it, shows such an offset too, and a replay holds nothing of it:
// a Clock there could never pass), whether or not the replay compares
// offsets, with either clock reading registered. Never a word made anew
// each run, never a Value.
func TestOutcomeALookBackReadAtAnotherPhaseInMillisecondsIsRefused(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 800*time.Millisecond)
	rec, live := iEpochMs(rAt.Add(-2*time.Second-5*time.Millisecond)), iEpochMs(fAt.Add(-2*time.Second-7*time.Millisecond))
	for _, w := range []*Window{iWin(rAt, fAt, 20*time.Millisecond).ComparesOffsets(), iWin(rAt, fAt, 20*time.Millisecond)} {
		d := oWord(t, Decide(iCall(p, Number, rec, live, rAt, fAt, w, nil, nil)), rec, live)
		oRefused(t, "dirtyAt <= now-2s in milliseconds at another phase", d, "no-clock-read")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "the offset reads in seconds") {
			t.Errorf("dirtyAt <= now-2s in milliseconds: %q; want main's reason, that the offset reads in seconds", d.Refusal.Why)
		}
	}
}

// The same look-back written as Unix seconds ("dirtyAt <= now-2s" cut to
// the second, read at another phase of its second in each run, the runs
// no whole minute apart) is a Clock at -2 s where the registered clock
// reading judges its offset's unit (it writes the offset's line: a replay
// checks it at the cut). Main's reading leaves that to its caller, whose
// rule refuses every offset that reads in a second, as main does. Never a
// word made anew each run, never a Value.
func TestOutcomeALookBackReadAtAnotherPhaseInSecondsIsAClock(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 800*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec, live := secs(rAt.Add(-2*time.Second)), secs(fAt.Add(-2*time.Second))
	w := iWin(rAt, fAt, 20*time.Millisecond).ComparesOffsets()
	in := iCall(p, Number, rec, live, rAt, fAt, w, nil, nil)
	d := oWord(t, Decide(in), rec, live)
	at := func(t time.Time) typedInstant {
		return typedInstant{at: time.Unix(t.Unix(), 0).UTC(), res: time.Second, names: namesMoment}
	}
	if off, _, ok := clockInWindow(p, at(rAt.Add(-2*time.Second)), at(fAt.Add(-2*time.Second)), w, nil); ok && off.Why == "" {
		oRefused(t, "dirtyAt <= now-2s in seconds, main's reading", d, "no-clock-read")
		return
	}
	iClock(t, "dirtyAt <= now-2s in seconds at another phase", d)
	if d.Clock != nil && (d.Clock.Min > -2*time.Second || d.Clock.Max < -2*time.Second) {
		t.Errorf("dirtyAt <= now-2s in seconds: the Clock's offset [%v, %v]; want it to hold -2s", d.Clock.Min, d.Clock.Max)
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

// Two earlier sends that hold a client test's second, each its own call's
// read of the clock, beside a value its own window does not hold: two reads
// of one coarse cut say nothing of this one. It goes on to the read since
// the test began, whose check goes red where the value was no read of the
// clock, never green. Where that window does not hold it either, the two
// reads stay the refusal, naming both. In a keploy test, whose calls are not
// read from a begin, too. Here at a hundredth of a second: a millisecond is
// no coarse cut, and two sends of the same millisecond are two sources, as a
// carried read beside a re-read is.
func TestOutcomeACoarseReadBesideTwoClockReadsIsReadSinceItsBegin(t *testing.T) {
	const p = "body.app.createdAt"
	rAt, fAt := oAt.Add(300*time.Millisecond), oAt.Add(time.Hour+320*time.Millisecond)
	read, fRead := oAt.Add(-200*time.Millisecond), oAt.Add(time.Hour-205*time.Millisecond)
	centi := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.00Z") }
	for _, cut := range []struct {
		what string
		form func(time.Time) string
		read bool // a read since the test began; else two sources
	}{{"hundredth of a second", centi, true}, {"millisecond", iMs, false}} {
		rec, live := cut.form(read), cut.form(fRead)
		clockRead := func(mock, path string, at time.Time) Earlier {
			e := iSent(mock, at, path, String, rec, live)
			e.Seen[0].Clock = true
			return e
		}
		lease, audit := clockRead("mock-196", "body.lease_until", oAt.Add(-150*time.Millisecond)), clockRead("mock-305", "body.auditedAt", oAt.Add(-120*time.Millisecond))
		w := iWin(rAt, fAt, 50*time.Millisecond)
		w.Rec.Began, w.F.Began = oAt.Add(-5*time.Second), fAt.Add(-5*time.Second)
		in := iCall(p, String, rec, live, rAt, fAt, w, []Earlier{lease, audit}, nil)
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), rec, live)
		if !cut.read {
			oRefused(t, "a client test's millisecond beside two clock reads of that millisecond", d, "two-sources", "mock-196", "mock-305")
		} else {
			iClock(t, "a client test's hundredth of a second beside two clock reads of it", d)
			if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
				t.Errorf("checked since %v; want since the test began", d.Clock.Since)
			}
		}
		late := *w
		late.F.Began = fAt
		in.Window = &late
		oRefused(t, "a "+cut.what+": round F's begin came after the read", oWord(t, Decide(in), rec, live), "two-sources", "mock-196", "mock-305")
		in.Drifts, in.Window = oDrifts(), w
		oRefused(t, "a "+cut.what+": a keploy test", oWord(t, Decide(in), rec, live), "two-sources", "mock-196", "mock-305")
	}
}

// A read of the second beside an answer that holds a finer instant of that
// second inside a longer text ("[E2E] x-<UnixNano>"): a variable holds one
// text, so the answer is no source keploy can bind, and two reads of one
// coarse cut say nothing of each other. Where the call's own window holds
// the read it is a Clock, as main reads it, and so is a client test's read
// since its begin (an app's updatedAt beside the UnixNano in the app's name
// an answer read back). Where neither holds it, it is refused, naming the
// answer, and a value the answer holds by its text stays a Value.
func TestOutcomeASecondReadBesideAFinerInstantInAnAnswerIsAClock(t *testing.T) {
	const p = "body.created_at"
	rAt, fAt := oAt.Truncate(time.Second).Add(700*time.Millisecond), oAt.Truncate(time.Second).Add(102*time.Second+700*time.Millisecond)
	before := oAt.Sub(rAt.Add(-300 * time.Millisecond))
	name := func(x time.Time) string { return "[E2E] x-" + iEpochNs(x.Add(-300*time.Millisecond)) }
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	ans := func(r, f string) []Raw {
		return []Raw{oAnswer("mock-1624", before, r).ServedAs(1).ServedLive(oAnswer("mock-1624", before, f))}
	}
	call := func(w *Window, raw []Raw) Input { return iCall(p, String, sec(rAt), sec(fAt), rAt, fAt, w, nil, raw) }
	iClock(t, "the second its own window holds", oWord(t, Decide(call(iWin(rAt, fAt, 100*time.Millisecond), ans(name(rAt), name(fAt)))), sec(rAt), sec(fAt)))
	far := iWin(rAt.Add(5*time.Second), fAt.Add(5*time.Second), 100*time.Millisecond)
	d := oWord(t, Decide(call(far, ans(name(rAt), name(fAt)))), sec(rAt), sec(fAt))
	oRefused(t, "a second its window does not hold", d, RefusalNoClockRead, "mock-1624", "another form")
	far.Rec.Began, far.F.Began = rAt.Add(-time.Second), fAt.Add(-time.Second)
	in := call(far, ans(name(rAt), name(fAt)))
	in.Drifts = oClientDrifts()
	d = oWord(t, Decide(in), sec(rAt), sec(fAt))
	iClock(t, "a client test's second read since its begin", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("%+v; want a read since the test began", *d.Clock)
	}
	d = oWord(t, Decide(call(iWin(rAt, fAt, 100*time.Millisecond), ans("x-"+sec(rAt), "x-"+sec(fAt)))), sec(rAt), sec(fAt))
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1624" {
		t.Errorf("a second the answer holds by its text: %s; want the Value bound to the answer", oDescribe([]Decision{d}))
	}
}

// A UnixNano the application sent inside a name, that a later answer reads
// back: the send comes first, so the value is bound there, as a word's is
// bound at its earliest place, and not at the answer's word. An answer before
// any send stays the source, as does one with no send.
func TestOutcomeAFineValueAnAnswerReadsBackIsBoundAtItsSend(t *testing.T) {
	const p = "body.delta.name"
	rAt, fAt := oAt, oAt.Add(111*time.Second)
	rec, live := "[E2E] x-"+iEpochNs(rAt.Add(-3*time.Second)), "[E2E] x-"+iEpochNs(fAt.Add(-3*time.Second))
	r, f := iEpochNs(rAt.Add(-3*time.Second)), iEpochNs(fAt.Add(-3*time.Second))
	send := func(at time.Time) []Earlier {
		return []Earlier{iSent("mock-1760", at, "body.app.name", String, rec, live)}
	}
	ans := []Raw{oAnswer("mock-1768", time.Second, rec).ServedAs(1).ServedLive(oAnswer("mock-1768", time.Second, live))}
	call := func(e []Earlier) Input {
		return iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), e, ans)
	}
	d := oWord(t, Decide(call(send(rAt.Add(-2*time.Second)))), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1760" {
		t.Errorf("a send before the answer: %s; want the Value bound at the send", oDescribe([]Decision{d}))
	}
	d = oWord(t, Decide(call(send(rAt.Add(-500*time.Millisecond)))), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1768" {
		t.Errorf("an answer before the send: %s; want the Value bound at the answer", oDescribe([]Decision{d}))
	}
	d = oWord(t, Decide(call(nil)), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1768" {
		t.Errorf("no send: %s; want the Value bound at the answer", oDescribe([]Decision{d}))
	}
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

// Round F's moment stands in an answer of the call's test or the start-up,
// recorded before the call, whose view in round F keploy cannot tell, and the
// recorded moment does not stand there. F may have given it there, so the
// value may be a copy of that answer, never surely a read of the clock:
// unknown, as the word rule reads the same place. An unstated answer that
// holds neither moment says nothing of an instant, and the window still reads
// it as a clock read; one that holds the recorded moment is its source.
func TestOutcomeFsMomentInAnAnswerOfNoStatedViewIsUnknown(t *testing.T) {
	const p = "body.sentAt"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 300*time.Millisecond)
	rec, live := iMs(rAt.Add(-50*time.Millisecond)), iMs(fAt.Add(-50*time.Millisecond))
	ans := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
	call := func(x Raw) Decision {
		return oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 60*time.Millisecond), nil, []Raw{x})), rec, live)
	}
	for _, c := range []struct {
		what string
		x    Raw
	}{
		{"served in the test, no view stated", ans(live).ServedAs(1)},
		{"served at the start-up, no view stated", ans(live).ServedAs(1).ServedBeforeTheTest()},
		{"served changed, one view holds F's moment", ans("ok").ServedAs(1).ServedChanged(ans("ok"), ans(live))},
	} {
		oRefused(t, c.what, call(c.x), RefusalUnknown, "mock-7")
	}
	iClock(t, "an unstated answer that holds neither moment", call(ans("ok").ServedAs(1)))
	iClock(t, "served changed, no view holds either moment", call(ans("ok").ServedAs(1).ServedChanged(ans("ok"), ans("no"))))
	iValue(t, "an unstated answer that holds the recorded moment", call(ans(rec).ServedAs(1)), "mock-7")
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
// ("since R" to "since F") it is, by its text. The same minute sent as epoch
// milliseconds, which main binds (instants_test.go:1507), is no copy: a
// variable holds one text, and the refusal names the send.
func TestOutcomeASendThatHoldsBothRunsMomentsIsNoCopy(t *testing.T) {
	const p = "body.at"
	rAt := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	rec, live := "2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"
	sent := func(r, f string) []Earlier {
		return []Earlier{iSent("mock-1", rAt.Add(-time.Second), "body.range", String, r, f)}
	}
	d := oWord(t, Decide(iCall(p, String, rec, live, rAt, rAt, nil, sent("since "+rec, "since "+live), nil)), rec, live)
	iValue(t, "a send that moved with the value", d, "mock-1")
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, rAt, nil, sent("since "+rec, "since "+rec+" until "+live), nil)), rec, live)
	if d.Outcome == OutcomeValue {
		t.Errorf("a send that holds both runs' moments in F bound the value: %s", oDescribe([]Decision{d}))
	}
	d = oWord(t, Decide(iCall(p, String, rec, live, rAt, rAt, nil, sent("since 1792667640000", "since 1792667700000"), nil)), rec, live)
	oRefused(t, "the same minute sent as epoch milliseconds", d, RefusalNoClockRead, "mock-1", "another form")
}

// A copied text or number is a source by its text at each step that binds
// one: an answer (3), a fine copy (4), a coarse copy (6). One rule across
// protocols: a variable holds one text, so a place that holds only the same
// moment in another form is no place to bind it, and the refusal names it;
// where the call's own window holds the read, it is that read. A typed
// instant is held by its moment.
func TestOutcomeACopiedTextOrNumberIsASourceByItsText(t *testing.T) {
	const p = "body.at"
	rAt := oAt.Add(2 * time.Hour)
	fAt := rAt.Add(time.Hour)
	rT, fT := rAt.Add(-3*time.Second).Truncate(time.Microsecond), fAt.Add(-3*time.Second).Truncate(time.Microsecond)
	sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	sent := func(r, f string) []Earlier {
		return []Earlier{iSent("mock-1", rAt.Add(-5*time.Second), "body.since", String, r, f)}
	}
	for _, c := range []struct {
		what, rec, live string
		typ             Type
		w               *Window
		earlier         []Earlier
		raw             []Raw
		value           bool
	}{
		{"an answer with its text", iMs(rT), iMs(fT), String, nil, nil, []Raw{oAnswer("mock-7", 4*time.Second, iMs(rT)).ServedAs(1)}, true},
		{"an answer with the moment as epoch milliseconds", iMs(rT), iMs(fT), String, nil, nil, []Raw{oAnswer("mock-7", 4*time.Second, iEpochMs(rT)).ServedAs(1)}, false},
		{"a fine send with its text", iMicro(rT), iMicro(fT), String, nil, sent("at "+iMicro(rT), "at "+iMicro(fT)), nil, true},
		{"a fine send as epoch nanoseconds", iMicro(rT), iMicro(fT), String, nil, sent("at "+iEpochNs(rT), "at "+iEpochNs(fT)), nil, false},
		{"a number with its text", sec(rT), sec(fT), Number, nil, sent("since "+sec(rT), "since "+sec(fT)), nil, true},
		{"a number sent as a date and time", sec(rT), sec(fT), Number, nil, sent("since "+rT.UTC().Format(time.RFC3339), "since "+fT.UTC().Format(time.RFC3339)), nil, false},
	} {
		d := oWord(t, Decide(iCall(p, c.typ, c.rec, c.live, rAt, fAt, c.w, c.earlier, c.raw)), c.rec, c.live)
		mock := "mock-1"
		if c.raw != nil {
			mock = "mock-7"
		}
		if c.value {
			iValue(t, c.what, d, mock)
			continue
		}
		oRefused(t, c.what, d, RefusalNoClockRead, mock, "another form")
	}
	// Where the call's own window holds the read, a place in another form
	// leaves it that read.
	at := []Raw{oAnswer("mock-7", 4*time.Second, iEpochMs(rT)).ServedAs(1)}
	d := oWord(t, Decide(iCall(p, String, iMs(rT), iMs(fT), rAt, fAt, iWin(rAt, fAt, 4*time.Second), nil, at)), iMs(rT), iMs(fT))
	iClock(t, "an answer in another form, the window holding the read", d)
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

// An epoch instant one run sent where the other run sent a number that is
// no time (an audit stamp round F replaced with 8357, or the other way
// round) is no instant in one run: keploy cannot check it, as a typed
// instant leaf of one run only is refused, and it is never bound fresh. The
// same holds inside a longer text. Two numbers neither of which reads as an
// instant keep their exact-text reading (the row above).
func TestOutcomeAnInstantRoundFReplacedWithANumberThatIsNoTimeIsRefused(t *testing.T) {
	const p = "body.auditedAt"
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	w := iWin(rAt, fAt, 50*time.Millisecond)
	w.Rec.Began, w.F.Began = rAt.Add(-5*time.Second), fAt.Add(-5*time.Second)
	recS := strconv.FormatInt(rAt.Add(-4*time.Millisecond).Unix(), 10)
	liveS := strconv.FormatInt(fAt.Add(-4*time.Millisecond).Unix(), 10)
	for _, c := range []struct {
		what, rec, live string
		typ             Type
		client          bool
	}{
		{"the recorded instant, a number in round F, in a client test", recS, "8357", Number, true},
		{"the recorded instant, a number in round F", recS, "8357", Number, false},
		{"a number when recorded, an instant in round F", "8357", liveS, Number, true},
		{"the recorded instant, a number in round F, inside a text", "evt-" + recS, "evt-8357", String, true},
	} {
		in := iCall(p, c.typ, c.rec, c.live, rAt, fAt, w, nil, nil)
		if c.client {
			in.Drifts = oClientDrifts()
		}
		ds := Decide(in)
		if len(ds) != 1 {
			t.Errorf("%s: %s; want one refusal", c.what, oDescribe(ds))
			continue
		}
		oRefused(t, c.what, ds[0], RefusalCannotCheck)
		if r := ds[0].Refusal; r != nil && !strings.Contains(r.Why, "one run's value here is no instant") {
			t.Errorf("%s: refused %q; want it to say one run's value here is no instant", c.what, r.Why)
		}
	}
}

// A time no window of the call's holds, whose recorded value an earlier
// recording held where round F never served it before the call (its call
// missed), is refused as no read of the clock, and the refusal names that
// recording and why it is no source, as main's did ("its source recording
// was never served in this round before this call"): a person reads where
// the value came from.
func TestOutcomeANoClockReadRefusalNamesTheRecordingThatHeldTheValue(t *testing.T) {
	const p = "body.since"
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	rec, live := iMs(rAt.Add(-time.Second)), iMs(fAt.Add(-time.Second))
	unserved := RawOf(&models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		ReqTimestampMock: rAt.Add(-time.Second),
		ResTimestampMock: rAt.Add(-time.Second + time.Millisecond),
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://api/ticks", Body: `{"tick":"` + rec + `"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 201},
	}}, false)
	in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), nil, []Raw{unserved})
	d := oWord(t, Decide(in), rec, live)
	oRefused(t, "a time whose recorded value an unserved recording held", d, "no-clock-read", "mock-1")
	if r := d.Refusal; r != nil && !strings.Contains(r.Why, "the recorded value stood at mock-1, which round F did not give or send before this call") {
		t.Errorf("refused %q; want it to name mock-1, where the recorded value stood, and why it is no source", r.Why)
	}
}

// A wall clock of no zone the application picked from a timetable an
// answer gave before the call is that answer's value, whether the timetable
// writes UTC or the application's offset: the wall clock is read in the
// application's zone too, as main's echo reads it (typed_test.go:926), so
// it is bound to the timetable, never the test's own value or a time no
// answer gave. With no window, in a client test and a keploy test.
func TestOutcomeAWallClockOfNoZonePickedFromATimetableInUTCIsItsValue(t *testing.T) {
	const p = "args.0"
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 22, 5, 44, 20, 0, time.UTC)
	recPick, livePick := time.Date(2026, 10, 22, 5, 44, 0, 0, time.UTC), time.Date(2026, 10, 22, 5, 45, 0, 0, time.UTC)
	for tname, slots := range map[string][2]string{
		"in UTC":             {"2026-10-22T05:44:00Z", "2026-10-22T05:45:00Z"},
		"at the app's +5:30": {"2026-10-22T11:14:00+05:30", "2026-10-22T11:15:00+05:30"},
	} {
		tt := getAnswer("mock-2", "http://api/departures", `{"slots":[{"at":"`+slots[0]+`"},{"at":"`+slots[1]+`"}]}`, call.Add(-time.Second))
		for _, client := range []bool{false, true} {
			in := iCall(p, Instant, "", "", call, call.Add(FullRoundLead), nil, nil, []Raw{RawOf(tt, true)})
			in.Rec.Leaves[p], in.Live.Leaves[p] = WallClockLeaf(p, p, recPick.In(ist), time.Minute), WallClockLeaf(p, p, livePick.In(ist), time.Minute)
			in.Drifts.zone, in.Drifts.clientTests = ist, client
			iValue(t, fmt.Sprintf("a wall clock in IST picked from a timetable %s, client test %v", tname, client), oWord(t, Decide(in), "2026-10-22T11:14", "2026-10-22T11:15"), "mock-2")
		}
	}
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

// A client test's call that carries no app clock (no window) still knows
// the instant its test began in the recording (Input.RecBegan): a time no
// earlier place holds that the recording made at or after that begin is
// no value the test made before it began, and is refused, naming the run
// (integrations' Mongo and Postgres client rows with no app clock); one
// made before it is fresh, and with no recorded begin it stays fresh.
func TestOutcomeAClientTestsValueMadeAfterItsRecordedBeginWithNoClockIsRefused(t *testing.T) {
	const p = "body.createdAt"
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	decide := func(began time.Time, rec, live string) Decision {
		in := iCall(p, String, rec, live, rAt, fAt, nil, nil, nil)
		in.Drifts, in.RecBegan = oClientDrifts(), began
		return oWord(t, Decide(in), rec, live)
	}
	began := rAt.Add(-5 * time.Second)
	for _, c := range []struct{ what, rec, live string }{
		{"1 s before its call", iMs(rAt.Add(-time.Second)), iMs(fAt.Add(-2 * time.Second))},
		{"at its begin", iMs(began), iMs(fAt.Add(-5 * time.Second))},
		{"an hour past its call", iMs(rAt.Add(time.Hour)), iMs(fAt.Add(time.Hour))},
	} {
		d := decide(began, c.rec, c.live)
		oRefused(t, "a client test's time made "+c.what+" with no app clock", d, "no-clock-read")
		if want := "nor the test's own value, made before it began: the recorded one lies past its test's begin"; d.Refusal != nil && !strings.Contains(d.Refusal.Why, want) {
			t.Errorf("a client test's time made %s with no app clock: %q; want it to say %q", c.what, d.Refusal.Why, want)
		}
	}
	if d := decide(began, iMs(rAt.Add(-time.Minute)), iMs(fAt.Add(time.Hour))); d.Outcome != OutcomeFresh {
		t.Errorf("a client test's time made before its recorded begin with no app clock: %s; want fresh", oDescribe([]Decision{d}))
	}
	if d := decide(time.Time{}, iMs(rAt.Add(-time.Second)), iMs(fAt.Add(-2*time.Second))); d.Outcome != OutcomeFresh {
		t.Errorf("a client test's time with no app clock and no recorded begin: %s; want fresh", oDescribe([]Decision{d}))
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

// A client test's read in Unix seconds made just after its test began, a
// second before its call's window in both runs (api-server's audit: the app
// reads its clock, makes other calls, then sends the audit): the window
// plus an offset of about a second holds it, an offset main refuses as one
// that reads in a second. That refusal is of the window's reading alone:
// the read is still checked from the test's begin (step 7), as main learned
// it, and is a Clock since the test began.
func TestOutcomeAClientTestsReadARefusedOffsetBeforeItsWindowIsAClockSinceItsBegin(t *testing.T) {
	const p = "body.auditedAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	r0 := oAt.Add(6300 * time.Millisecond)
	rAt := r0.Add(900 * time.Millisecond)
	f0 := oAt.Add(61*time.Second + 14800*time.Millisecond)
	fAt := f0.Add(1400 * time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = r0, f0
	for _, read := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond} {
		rec, live := secs(r0.Add(time.Millisecond)), secs(f0.Add(read))
		in := iCall(p, Number, rec, live, rAt, fAt, w, nil, nil)
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), rec, live)
		iClock(t, "a client test's read a second before its call's window", d)
		if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
			t.Errorf("read %v after round F's begin: %+v; want a read since the test began", read, *d.Clock)
		}
	}
}

// api-server's outbox carries an app's createdAt in seconds, read when the app
// was made. The harness's name for that app holds a UnixNano of the same
// second, sent before the call in both runs. A variable holds one text, so
// that name is no place to bind the seconds at: no adapter can use the copy.
// The value goes on to the read since the test began, and its check goes red
// where it was no read. A place that holds the same text still binds, and it
// binds ahead of an earlier place in another form. With no begin to read
// from, it is refused, naming the name.
func TestOutcomeASecondInsideANanosecondNameIsNoCopyToBind(t *testing.T) {
	const p = "body.docs.msgs.0.app.createdAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	nanos := func(t time.Time) string { return "app-crud-" + strconv.FormatInt(t.UnixNano(), 10) }
	r0 := oAt.Add(6300 * time.Millisecond)
	rAt := r0.Add(900 * time.Millisecond)
	f0 := oAt.Add(61*time.Second + 14800*time.Millisecond)
	fAt := f0.Add(1400 * time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = r0, f0
	rec, live := secs(r0.Add(100*time.Millisecond)), secs(f0.Add(200*time.Millisecond))
	name := iSent("mock-502", rAt.Add(-300*time.Millisecond), "body.cmd.query.app.name", String, nanos(r0.Add(90*time.Millisecond)), nanos(f0.Add(190*time.Millisecond)))
	in := iCall(p, Number, rec, live, rAt, fAt, w, []Earlier{name}, nil)
	in.Drifts = oClientDrifts()
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "a second that stands only inside a nanosecond's text", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("%+v; want a read since the test began", *d.Clock)
	}
	sent := iSent("mock-503", rAt.Add(-200*time.Millisecond), "body.docs.0.createdAt", Number, rec, live)
	in = iCall(p, Number, rec, live, rAt, fAt, w, []Earlier{name, sent}, nil)
	in.Drifts = oClientDrifts()
	iValue(t, "the same second sent whole at a later place", oWord(t, Decide(in), rec, live), "mock-503")
	noBegin := iWin(rAt, fAt, 100*time.Millisecond)
	in = iCall(p, Number, rec, live, rAt, fAt, noBegin, []Earlier{name}, nil)
	in.Drifts = oClientDrifts()
	oRefused(t, "no begin to read from", oWord(t, Decide(in), rec, live), RefusalNoClockRead, "mock-502", "another form")
}

// A client test's token expiry, a read the application made earlier in its
// test plus a time to live of 30 days cut to the second (expires_at =
// cut_s(now) + 30d), 3.2 s before its call in the recording and 5.9 s in
// round F: no offset its call's own window shows holds in both runs, but
// the window from each run's test begin does, so it is a Clock checked from
// the test's begin. A time to live that changed between the runs (30 days,
// then 29) holds in neither window: refused, never fresh and never a Value.
// In a keploy test, whose calls are not read from a begin, the expiry is
// refused.
func TestOutcomeAClientTestsTimeToLiveOnAnEarlyReadIsAClockSinceItsBegin(t *testing.T) {
	const p = "body.expires_at"
	rAt, fAt := oAt.Add(155*time.Millisecond), oAt.Add(61*time.Second+147*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	w := iWin(rAt, fAt, 61*time.Millisecond).ComparesOffsets()
	w.Rec.Began, w.F.Began = rAt.Add(-12*time.Second), fAt.Add(-12*time.Second)
	for _, tc := range []struct {
		what      string
		rec, live string
		clock     bool
	}{
		{"an expiry 30 days on", secs(rAt.Add(-3200 * time.Millisecond).Add(30 * 24 * time.Hour)), secs(fAt.Add(-5900 * time.Millisecond).Add(30 * 24 * time.Hour)), true},
		{"time to live changed to 29 days", secs(rAt.Add(-3200 * time.Millisecond).Add(30 * 24 * time.Hour)), secs(fAt.Add(-5900 * time.Millisecond).Add(29 * 24 * time.Hour)), false},
	} {
		in := iCall(p, Number, tc.rec, tc.live, rAt, fAt, w, nil, nil)
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), tc.rec, tc.live)
		if !tc.clock {
			oRefused(t, "a client test's "+tc.what+" read before its call's window", d, "no-clock-read")
			continue
		}
		iClock(t, "a client test's "+tc.what+" read before its call's window", d)
		if d.Clock != nil && (d.Clock.Since != models.SinceTestBegin || d.Clock.Min <= 0) {
			t.Errorf("a client test's %s: %+v; want a time to live checked since the test began", tc.what, *d.Clock)
		}
		in.Drifts = oDrifts()
		oRefused(t, "a keploy test's "+tc.what+" read before its call's window", oWord(t, Decide(in), tc.rec, tc.live), "no-clock-read")
	}
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

// A clock read its protocol types keeps that layout, plain or with an
// offset, so a replay that writes it in another form (a text) is checked
// and fails, never passes as any form.
func TestOutcomeATypedClockReadKeepsItsTypedLayout(t *testing.T) {
	const p = "body.dirtyAt"
	ms := time.Millisecond
	call := time.Date(2026, 10, 26, 10, 0, 7, 300e6, time.UTC)
	fcall := call.Add(FullRoundLead)
	w := (&Window{Rec: Span{From: call.Add(-100 * ms), To: call}, F: Span{From: fcall.Add(-100 * ms), To: fcall}}).ComparesOffsets()
	for name, off := range map[string]time.Duration{"plain": 0, "plus two hours": 2 * time.Hour} {
		leaf := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at.Add(off), ms)}} }
		ds := Decide(Input{Drifts: oDrifts(), Drift: []string{p}, Live: leaf(fcall.Add(-30 * ms)), Rec: leaf(call.Add(-30 * ms)),
			At: call, Arrived: fcall, Window: w, Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].Clock.Layout != models.LayoutTyped {
			t.Errorf("a typed clock read, %s: %s; want a Clock in layout %q", name, oDescribe(ds), models.LayoutTyped)
		}
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

// A read of the clock that is one word of a longer text ("evt-<seconds>")
// is a Clock carrying where it stands in the recorded text: its bytes, and
// the length and hash of the text on each side (models.ClockOffset.Word), so
// a replay reads that word alone as the clock and compares the rest, for
// every adapter alike. A read that is its text whole carries none. Where
// the text around it changed too, the sides a replay compares are not the
// same in both runs: refused as one keploy cannot check.
func TestOutcomeAClockReadInsideATextCarriesItsWord(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	rRead, fRead := rAt.Add(-300*time.Millisecond), fAt.Add(-200*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	decide := func(rec, live string) Decision {
		in := iCall("body.ref", String, rec, live, rAt, fAt, iWin(rAt, fAt, 900*time.Millisecond), nil, nil)
		return oWord(t, Decide(in), secs(rRead), secs(fRead))
	}
	rec := "evt-" + secs(rRead)
	d := decide(rec, "evt-"+secs(fRead))
	iClock(t, "a read of the clock after evt-", d)
	if want := models.ClockWordOf(rec, len("evt-"), len(rec)); d.Clock != nil && d.Clock.Word != want {
		t.Errorf("a read of the clock after evt-: word %+v; want %+v", d.Clock.Word, want)
	}
	d = decide(secs(rRead), secs(fRead))
	iClock(t, "a read of the clock, the text whole", d)
	if d.Clock != nil && !d.Clock.Word.IsZero() {
		t.Errorf("a read of the clock, the text whole: word %+v; want none", d.Clock.Word)
	}
	d = decide("evt-"+secs(rRead)+"-7", "evt-"+secs(fRead)+"-8")
	oRefused(t, "a read of the clock in evt-<seconds>-<n>, n changed too", d, "cannot-check")
	if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "the text around it changed too") {
		t.Errorf("a read of the clock whose text changed around it: %q; want it to say so", d.Refusal.Why)
	}
}

// An instant written percent-encoded ("10%3A00%3A00.123456Z", a link's
// query in a body) is read as the instant it opens to, not as the plain
// words "3A00" and "3A01" beside it: read in its call's window in both runs
// it is a read of the clock, named through the step that opens it, which no
// replay checks in an opened text yet: refused as one keploy cannot check,
// naming why. Sent before the call
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
	oRefused(t, "a percent-encoded microsecond read in its window", d, "cannot-check")
	if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "a part of the text keploy opened") {
		t.Errorf("a percent-encoded read in its window: %q; want it to name the opened text", d.Refusal.Why)
	}
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
// clock reading's; a stub reading that judged its unit (it writes the
// offset's line) stands in for it (main's reading leaves the unit to its
// caller, whose rule refuses an offset that reads in a second; the later
// clock reading learns it). Where the reading finds no offset, the copy
// still binds.
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
		return models.ClockOffset{Min: -2010 * time.Millisecond, Max: -1990 * time.Millisecond, Res: time.Millisecond, Why: "the stub reading's line"}, "", true
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

// A window load widened holds the same read as a plain one and as a read
// two seconds back. The reading is asked first, so a debounce is learned with
// its offset: learned plain, the application fails nearly every replay and a
// dropped look-back passes. Where the reading finds nothing, the window's
// plain read stands.
func TestOutcomeTheReadingIsAskedBeforeAWideWindowsPlainRead(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt := oAt.Add(400 * time.Millisecond)
	fAt := oAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec, live := secs(rAt.Add(-2*time.Second)), secs(fAt.Add(-2*time.Second))
	in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 3200*time.Millisecond).ComparesOffsets(), nil, nil)
	in.Landed = Landed{Where: LandedBetween}
	defer func(f func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool)) {
		clockInWindow = f
	}(clockInWindow)
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{Min: -2010 * time.Millisecond, Max: -1990 * time.Millisecond, Res: time.Millisecond, Why: "the stub reading's line"}, "", true
	}
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "a read two seconds back in a window that also holds it plain", d)
	if d.Clock != nil && (d.Clock.Min > -2*time.Second || d.Clock.Max < -2*time.Second) {
		t.Errorf("the Clock's offset is %v..%v; want the reading's, around -2s", d.Clock.Min, d.Clock.Max)
	}
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{}, "", false
	}
	d = oWord(t, Decide(in), rec, live)
	iClock(t, "the same read where the reading finds no offset", d)
	if d.Clock != nil && (d.Clock.Min != 0 || d.Clock.Max != 0) {
		t.Errorf("the Clock's offset is %v..%v; want none where the reading finds none", d.Clock.Min, d.Clock.Max)
	}
}

// api-server's worker claims a job at a second that, under load, is also the
// second of a UnixNano in an app's name an earlier answer gave. The answer
// holds round F's second only as a cut of that finer instant: no application
// picks a second out of a name, and no variable could hold that text. It is
// no pick, and the call's window holds the read: a Clock, whose check goes
// red if it was a pick. An answer that gives the second itself stays a pick.
func TestOutcomeASecondInsideAnAnswersNanosecondNameIsNoPick(t *testing.T) {
	const p = "body.updates.0.u.$set.claimedAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rAt := oAt.Add(400 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 300*time.Millisecond)
	rec, live := secs(rAt.Add(-5*time.Millisecond)), secs(fAt.Add(-5*time.Millisecond))
	answer := func(v string) Earlier {
		return Earlier{Mock: "mock-223", At: rAt.Add(-200 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: oText("resp.0.0.cursor.firstBatch.0.app.name", v), Live: v}}}
	}
	name := "[E2E] app-crud-" + strconv.FormatInt(fAt.Add(-150*time.Millisecond).UnixNano(), 10)
	in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), []Earlier{answer(name)}, nil)
	iClock(t, "a second inside an answer's nanosecond name", oWord(t, Decide(in), rec, live))
	in = iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), []Earlier{answer(live)}, nil)
	oRefused(t, "an answer that gives round F's second itself", oWord(t, Decide(in), rec, live), "pick", "mock-223")
}

// A read plus a time to live (a lease until now + 30 min, in epoch
// milliseconds) its call's window holds in both runs is a Clock with that
// offset, and its line names the offset: the window alone holds the read,
// not the value (integrations' background_clockread_test.go:303).
func TestOutcomeAClockWithATimeToLiveNamesItsOffset(t *testing.T) {
	const p = "body.docs.0.lease_until.$date.$numberLong"
	rAt := oAt.Add(12 * time.Millisecond)
	fAt := oAt.Add(time.Minute + 270*time.Millisecond)
	rec, live := iEpochMs(rAt.Add(30*time.Minute-5*time.Millisecond)), iEpochMs(fAt.Add(30*time.Minute-5*time.Millisecond))
	in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 20*time.Millisecond).ComparesOffsets(), nil, nil)
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "a lease of 30 minutes", d)
	if d.Clock == nil || d.Clock.Min > 30*time.Minute || d.Clock.Max < 30*time.Minute || !strings.Contains(d.Line, "clock + 30m") {
		t.Errorf("a lease of 30 minutes: %s, line %q; want a Clock at + 30m whose line names it", oDescribe([]Decision{d}), d.Line)
	}
}

// A read of the clock learned past a place that held the recorded moment
// names that place, set aside as its source, as a fresh word's line does: a
// person reads what keploy ruled out.
func TestOutcomeAClockLineNamesThePlaceSetAside(t *testing.T) {
	const p = "body.since"
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	rec, live := iMs(rAt.Add(-time.Second)), iMs(fAt.Add(-time.Second))
	unserved := RawOf(&models.Mock{Name: "mock-645", Kind: models.HTTP, Spec: models.MockSpec{
		ReqTimestampMock: rAt.Add(-time.Second),
		ResTimestampMock: rAt.Add(-time.Second + time.Millisecond),
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://api/ticks", Body: `{"tick":"` + rec + `"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 201},
	}}, false)
	in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 2*time.Second), nil, []Raw{unserved})
	d := oWord(t, Decide(in), rec, live)
	iClock(t, "a read beside a recording that held the recorded moment", d)
	if !strings.Contains(d.Line, "set aside: mock-645 held the recorded value and round F's value does not stand there") {
		t.Errorf("the clock line %q; want it to name mock-645, set aside", d.Line)
	}
}

// An app's createdAt in seconds, read when the app was made, beside an
// earlier send of its test that holds the same second: a PUT's lastUsedAt,
// seconds with a fraction, which its own call learned as its read of the
// clock. The two share a second only when the runs' timing puts them in one,
// so a copy bound there flips with load: at a millisecond or coarser, a send
// that is itself a read of the clock is no copy's source, and the value is
// read as the clock, here since the test began. A send its call did not
// learn as a read of the clock stays the copy's source, and so does a clock
// read at a cut finer than a millisecond, which no second shares by chance.
func TestOutcomeACoarseCopyOfAClockReadIsAClock(t *testing.T) {
	const p = "body.docs.msgs.0.app.createdAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	frac := func(t time.Time) string { return secs(t) + "." + fmt.Sprintf("%06d", t.Nanosecond()/1000) }
	r0, f0 := oAt.Add(6300*time.Millisecond), oAt.Add(61*time.Second+14800*time.Millisecond)
	rAt, fAt := r0.Add(900*time.Millisecond), f0.Add(1400*time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = r0, f0
	rec, live := secs(r0.Add(100*time.Millisecond)), secs(f0.Add(150*time.Millisecond))
	call := func(put Earlier) Input {
		in := iCall(p, Number, rec, live, rAt, fAt, w, []Earlier{put}, nil)
		in.Drifts = oClientDrifts()
		return in
	}
	put := iSent("mock-204", rAt.Add(-500*time.Millisecond), "body.data.lastUsedAt", Number, frac(r0.Add(120*time.Millisecond)), frac(f0.Add(170*time.Millisecond)))
	put.Seen[0].Clock = true
	d := oWord(t, Decide(call(put)), rec, live)
	iClock(t, "a second shared with a send that is its own read of the clock", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("%+v; want a read since the test began", *d.Clock)
	}
	put.Seen[0].Clock = false
	iValue(t, "a second shared with a send that is no read of the clock", oWord(t, Decide(call(put)), rec, live), "mock-204")

	const q = "body.docs.msgs.0.app.createdAtNanos"
	nr, nf := iEpochNs(r0.Add(100*time.Millisecond+7)), iEpochNs(f0.Add(150*time.Millisecond+9))
	fine := iSent("mock-204", rAt.Add(-500*time.Millisecond), "body.data.lastUsedAtNanos", Number, nr, nf)
	fine.Seen[0].Clock = true
	in := iCall(q, Number, nr, nf, rAt, fAt, w, []Earlier{fine}, nil)
	in.Drifts = oClientDrifts()
	iValue(t, "a nanosecond sent by an earlier read of the clock", oWord(t, Decide(in), nr, nf), "mock-204")
}

// A copy at exactly a millisecond is no coarse cut: two reads of the clock
// do not share a millisecond as the runs' timing falls, so a value that an
// earlier send of its test held as the same millisecond in both runs is
// bound to that send, whether or not that send's call read the clock. A
// typed date (a BSON date: its leaf a date and time, its place in the
// earlier send the raw milliseconds) is held by its moment at its own cut,
// whatever text each side writes. A place that holds it only as a cut of a
// finer instant is no source, and the read since the test began stays; at
// a second, a send that is its call's read of the clock is no source.
func TestOutcomeAMillisecondCopyIsBoundToItsSource(t *testing.T) {
	const p, src = "body.cmd.documents.0.auth_time", "body.cmd.documents.0.lease_until"
	ms := time.Millisecond
	r0, f0 := oAt.Add(6300*ms), oAt.Add(61*time.Second+14800*ms)
	rAt, fAt := r0.Add(900*ms), f0.Add(1400*ms)
	w := iWin(rAt, fAt, 100*ms)
	w.Rec.Began, w.F.Began = r0, f0
	readR, readF := r0.Add(107*ms), f0.Add(153*ms)
	typed := func(at time.Time, res time.Duration) string { return InstantLeaf(p, p, at, res).Value }
	decide := func(typ Type, rec, live string, put Earlier) Decision {
		t.Helper()
		in := iCall(p, typ, rec, live, rAt, fAt, w, []Earlier{put}, nil)
		in.Drifts = oClientDrifts()
		return oWord(t, Decide(in), rec, live)
	}
	since := func(what string, d Decision) {
		t.Helper()
		iClock(t, what, d)
		if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
			t.Errorf("%s: %+v; want a read since the test began", what, *d.Clock)
		}
	}
	for _, clock := range []bool{false, true} {
		put := iSent("mock-204", rAt.Add(-500*ms), src, Number, iEpochMs(readR), iEpochMs(readF))
		put.Seen[0].Clock = clock
		iValue(t, fmt.Sprintf("milliseconds an earlier send held as its text (its call read the clock: %v)", clock),
			decide(Number, iEpochMs(readR), iEpochMs(readF), put), "mock-204")
		iValue(t, fmt.Sprintf("a typed date an earlier send held as its milliseconds (its call read the clock: %v)", clock),
			decide(Instant, typed(readR, ms), typed(readF, ms), put), "mock-204")
	}
	micro := iSent("mock-204", rAt.Add(-500*ms), src, String, iMicro(readR.Add(412*time.Microsecond)), iMicro(readF.Add(388*time.Microsecond)))
	since("a typed date an earlier send held only as a cut of its microseconds", decide(Instant, typed(readR, ms), typed(readF, ms), micro))
	sec := iSent("mock-204", rAt.Add(-500*ms), src, Instant, typed(readR, time.Second), typed(readF, time.Second))
	sec.Seen[0].Clock = true
	since("a typed second an earlier send held as its call's read of the clock", decide(Instant, typed(readR, time.Second), typed(readF, time.Second), sec))
}

// An answer recorded after the call that round F gave the application
// before it, holding round F's moment: in round F the value may be a pick
// from it, so it is refused naming it, as main refuses it, never read as
// the call's clock read. So is one whose whole field is a finer instant F's
// minute is a cut of (a SentAt, cut or rounded to the minute), and one that
// holds the recorded minute too, by another cut: it was recorded after the
// call, so the recorded application never read it. Where F never gave it,
// or it holds F's minute only as a cut of a finer instant inside a longer
// text (a UnixNano in a name: no pick, as at the answer step), the call's
// window reads the clock.
func TestOutcomeAMomentALaterRecordingGaveBeforeTheCallIsAPick(t *testing.T) {
	const p = "body.sent_at"
	minute := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z07:00") }
	for _, at := range []struct {
		what     string
		rAt, fAt time.Time
	}{
		{"an hour apart", oAt.Add(30 * time.Second), oAt.Add(time.Hour + 61*time.Second)},
		{"in the next minute", oAt.Add(time.Hour + 57*time.Second), oAt.Add(time.Hour + 61*time.Second)},
	} {
		rAt, fAt := at.rAt, at.fAt
		rec, live := minute(rAt), minute(fAt)
		w := iWin(rAt, fAt, 100*time.Millisecond)
		// mock-0 was recorded 15 s after the call.
		later := func(word string) Raw { return oAnswer("mock-0", oAt.Sub(rAt.Add(15*time.Second)), word) }
		fMinute := fAt.Truncate(time.Minute)
		for _, tc := range []struct {
			what string
			raw  Raw
			pick bool
		}{
			{"round F's minute, given before the call", later(live).WithServed(true).ServedAsRecorded(), true},
			{"round F's minute at its first second, given before the call", later(fMinute.Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), true},
			{"round F's minute as a cut of a later second, given before the call", later(fMinute.Add(5 * time.Second).Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), true},
			{"round F's minute as a second before it, rounded, given before the call", later(fMinute.Add(-20 * time.Second).Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), true},
			{"round F's minute as a cut of a UnixNano in a name, given before the call", later("app-crud-" + iEpochNs(fMinute.Add(5*time.Second+123))).WithServed(true).ServedAsRecorded(), false},
			{"round F's minute, never given", later(live), false},
		} {
			what := at.what + ": " + tc.what
			d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, nil, []Raw{tc.raw})), rec, live)
			if tc.pick {
				oRefused(t, what, d, "pick", "mock-0")
			} else {
				iClock(t, what, d)
			}
		}
	}
}
