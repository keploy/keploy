package learn

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
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
	// Handed as one recording that also stamps the timetable with a finer
	// time of another moment: the departure stands there at the call's
	// cut, so the stamp makes it no less a pick.
	body, _ := json.Marshal(map[string]any{"date": "2026-10-14", "departures": []string{"10:00", "10:02", "10:03"}, "generated_at": "2026-10-14T09:58:12.345Z"})
	table := RawOf(&models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://rail.local/timetable"},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: string(body)},
		ReqTimestampMock: rAt.Add(-100 * time.Millisecond), ResTimestampMock: rAt.Add(-99 * time.Millisecond),
	}}, true).ServedAs(1).ServedAsRecorded()
	in := iCall(p, String, "2026-10-14 10:01", "2026-10-14 10:02", rAt, fAt, iWin(rAt, fAt, time.Hour), nil, []Raw{table})
	oRefused(t, "a time of day in a stamped timetable", oWord(t, Decide(in), "01", "02"), "pick", "mock-0")
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
	// 5 s after the read in the recording, 8 s in round F: neither the
	// window nor one look-back in both runs holds it.
	far := iWin(rAt.Add(5*time.Second), fAt.Add(8*time.Second), 100*time.Millisecond)
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

// An answer recorded before the call that holds the recorded moment, which
// round F served changed: keploy read the views F gave there. Where no view
// holds F's moment, F's application took its moment from elsewhere, so the
// answer is not one F gave it: no source, in or outside the call's scope,
// and the call's window holds a read of the clock. Where a view holds F's
// moment, or F served it with no view keploy read, it may be: an answer of
// the scope binds the value, and one outside the scope is refused naming
// the edit.
func TestOutcomeAnAnswerWhoseViewsInFHoldNoneOfFsMomentIsNoSource(t *testing.T) {
	const p = "bind.$2"
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 300*time.Millisecond)
	rec, live := iMicro(rAt.Add(-50*time.Millisecond)), iMicro(fAt.Add(-50*time.Millisecond))
	ans := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
	call := func(x Raw, outside bool) Decision {
		in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 60*time.Millisecond), nil, []Raw{x})
		if outside {
			in.Scope = func(string) Scope { return ScopeTest("test-1") }
		}
		return oWord(t, Decide(in), rec, live)
	}
	served := ans(rec).ServedAs(1)
	for _, outside := range []bool{false, true} {
		where := map[bool]string{false: "in the call's scope", true: "outside the call's scope"}[outside]
		iClock(t, "views as recorded and another, "+where, call(served.ServedChanged(ans(rec), ans("no")), outside))
		iClock(t, "one view of neither moment, "+where, call(served.ServedChanged(ans("no")), outside))
	}
	iValue(t, "a view holds F's moment, in the call's scope", call(served.ServedChanged(ans(rec), ans(live)), false), "mock-7")
	iValue(t, "no view keploy read, in the call's scope", call(served.ServedChanged(), false), "mock-7")
	oRefused(t, "a view holds F's moment, outside the call's scope", call(served.ServedChanged(ans(rec), ans(live)), true), RefusalAnotherScope, "mock-7")
	oRefused(t, "no view keploy read, outside the call's scope", call(served.ServedChanged(), true), RefusalAnotherScope, "mock-7")
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
	// Each read under a second before its call: a plain read wherever a
	// window holds it.
	rT, fT := rAt.Add(-700*time.Millisecond).Truncate(time.Microsecond), fAt.Add(-640*time.Millisecond).Truncate(time.Microsecond)
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
	// Round F's text standing in an answer given before the call, the same
	// in both runs, is a pick, as a moment standing there is: keploy cannot
	// read it as a finer time's cut, so it never sets the pick aside.
	at := []Raw{oAnswer("mock-7", 20*time.Millisecond, live).ServedAs(1).ServedAsRecorded()}
	d = oWord(t, Decide(iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), nil, at)), rec, live)
	oRefused(t, "an unparsed 10-digit number round F's earlier answer held", d, RefusalPick, "mock-7")
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
	fAt := f0.Add(2400 * time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = r0, f0
	// The second stands 1.2 s before its call in the recording and 2.2 s in
	// round F: no one look-back holds it in both runs.
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
// plain read stands. Where the reading refuses the call's cut (a millisecond
// value a second or more before its call in both runs, as a consumer's
// processed_at read a second before its insert is), its refusal stands: the
// window's plain read is not tried in its place.
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
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{}, "the stub reading's refusal", false
	}
	oRefused(t, "the same read where the reading refuses its cut", oWord(t, Decide(in), rec, live), RefusalNoClockRead, "the stub reading's refusal")
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
	const aside = "set aside: mock-645 held the recorded value and round F's value does not stand there"
	if !strings.Contains(d.Line, aside) {
		t.Errorf("the clock line %q; want it to name mock-645, set aside", d.Line)
	}
	if d.Clock != nil && !strings.Contains(d.Clock.Why, aside) {
		t.Errorf("the clock's why %q; want it to name mock-645, set aside", d.Clock.Why)
	}
	// A clock reading that writes the offset's own line (a read plus a lead)
	// keeps it, and the why a replay shows still names the place set aside
	// (integrations' mongo InAClientTestARecordingAnOlderTestServed...).
	defer func(f func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool)) {
		clockInWindow = f
	}(clockInWindow)
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{Min: -1010 * time.Millisecond, Max: -990 * time.Millisecond, Res: time.Millisecond, Why: "the stub reading's line"}, "", true
	}
	in.Window = iWin(rAt, fAt, 2*time.Second).ComparesOffsets()
	d = oWord(t, Decide(in), rec, live)
	iClock(t, "a read a second back beside a recording that held the recorded moment", d)
	if d.Clock != nil && (!strings.Contains(d.Clock.Why, "the stub reading's line") || !strings.Contains(d.Clock.Why, aside)) {
		t.Errorf("the offset clock's why %q; want the reading's line, naming mock-645 set aside", d.Clock.Why)
	}
}

// A client test's name minted from the clock in nanoseconds some 11 ms
// before its call ("[E2E] app-crud-<UnixNano>", api-server's TestAppCRUD)
// is a read made since its test began, whether or not the two runs' gaps
// from minting to the call agree: where they agreed to well under a
// millisecond (the runs' load set them), the window's reading found one
// offset, refused its cut and kept the read from step 7, so the kept count
// flipped with load.
func TestOutcomeANameMintedFromTheClockBeforeItsCallIsAReadAtAnyGap(t *testing.T) {
	const p = "body.cmd.query.app.name"
	rAt, fAt := oAt.Add(123457*time.Nanosecond), oAt.Add(66*time.Second+987653*time.Nanosecond)
	for _, gap := range []struct {
		what   string
		rg, fg time.Duration
	}{{"gaps alike", 11346410 * time.Nanosecond, 11276112 * time.Nanosecond}, {"gaps apart", 11546410 * time.Nanosecond, 30123457 * time.Nanosecond}} {
		recN, liveN := iEpochNs(rAt.Add(-gap.rg)), iEpochNs(fAt.Add(-gap.fg))
		w := iWin(rAt, fAt, 2*time.Millisecond)
		w.Rec.Began, w.F.Began = rAt.Add(-200*time.Millisecond), fAt.Add(-200*time.Millisecond)
		in := iCall(p, String, "[E2E] app-crud-"+recN, "[E2E] app-crud-"+liveN, rAt, fAt, w, nil, nil)
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), recN, liveN)
		iClock(t, "a name minted "+gap.what, d)
		if !strings.Contains(d.Line, "made since its test began in both runs") {
			t.Errorf("a name minted %s: line %q; want a read since its test began", gap.what, d.Line)
		}
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
// the call's clock read. So is one that holds the recorded minute too, by
// another cut: it was recorded after the call, so the recorded application
// never read it. A pick needs round F's moment at the call's cut: where F
// never gave it, or it holds F's minute only as a finer time cut or rounded
// to it (a SentAt whole, at any second of the minute, its first included,
// or a UnixNano in a name), the call's window reads the clock, checked at
// replay, where main refuses the SentAt.
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
			{"round F's minute as a cut of its first second, given before the call", later(fMinute.Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), false},
			{"round F's minute as a cut of its first millisecond, given before the call", later(iMs(fMinute)).WithServed(true).ServedAsRecorded(), false},
			{"round F's minute as a cut of a later second, given before the call", later(fMinute.Add(5 * time.Second).Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), false},
			{"round F's minute as a second before it, rounded, given before the call", later(fMinute.Add(-20 * time.Second).Format(time.RFC3339)).WithServed(true).ServedAsRecorded(), false},
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

// A second the call reads from the clock in its own window, which round
// F's view of an earlier answer holds only inside a finer time (an app's
// name holding the UnixNano it was made at, a $date or a time of
// milliseconds, or a name and a $date in one answer), is the call's own
// read of the clock, checked at replay: that answer gave the application
// no value at the call's cut to pick from. So it is where the finer time
// was made at the second's first millisecond or nanosecond, which names
// the second itself, and where the recorded answer holds the recorded
// second the same way, and so where its encoder writes it without its
// trailing zeros and one run's text shows the whole second: the place is
// judged at the finest unit either run's text there shows. Only where
// neither run shows a fraction is it a time of seconds. One that holds
// round F's second at the call's cut (as a number
// of seconds, or a time of seconds, or inside an encoded text that decodes
// to it) is a pick, refused naming it. As a
// recording, leaf by leaf, and in a window the call landed in from another
// test, the decision is the same.
func TestOutcomeAFinerTimeHoldingRoundFsSecondIsNoPick(t *testing.T) {
	const p = "body.cmd.query.dirtyAt.$lte.$numberLong"
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	rec, live := sec(rAt), sec(fAt)
	// The app was made 37 s before the call in the recording, in another
	// minute, and in the call's second in round F.
	rMade := rAt.Add(-37 * time.Second)
	find := func(fields string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://db.local/apps/find", Body: `{"find":"apps"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"cursor":{"firstBatch":[{` + fields + `}]}}`},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	name := func(x time.Time) string { return `"app":{"name":"[E2E] page-queries-` + iEpochNs(x) + `"}` }
	date := func(x time.Time) string { return `"created_at":{"$date":{"$numberLong":"` + iEpochMs(x) + `"}}` }
	iso := func(x time.Time) string { return `"created_at":"` + iMs(x) + `"` }
	isoAt := func(x time.Time) string {
		return `"created_at":"` + x.In(time.FixedZone("", 19800)).Format("2006-01-02T15:04:05.000-07:00") + `"`
	}
	// A number of seconds with its milliseconds (Python's time.time(), a
	// JavaScript Date.now()/1000) holds the second as a word of its own,
	// before its fraction: that word is the finer time's cut, as inside
	// any other finer time.
	frac := func(x time.Time) string { return `"made":` + fmt.Sprintf("%d.%03d", x.Unix(), x.Nanosecond()/1e6) }
	leaves := func(rm, lm *models.Mock) Earlier {
		e := leafedOf(rm, true, 1)
		e.Payload = e.Payload.ServedLive(RawOf(lm, true))
		v, _ := DecodeJSON([]byte(lm.Spec.HTTPResp.Body))
		ll := map[string]Leaf{}
		JSONLeaves("body", v, ll)
		for i := range e.Seen {
			if s := &e.Seen[i]; s.Side == Response {
				s.Live = ll[s.Leaf.Path].Value
			}
		}
		return e
	}
	for _, tc := range []struct {
		what   string
		fields func(time.Time) string
		pick   bool
	}{
		{"a name holding the UnixNano it was made at", name, false},
		{"a $date of milliseconds", date, false},
		{"a time of milliseconds", iso, false},
		{"a time of milliseconds at an offset", isoAt, false},
		{"a name and a $date beside it", func(x time.Time) string { return name(x) + "," + date(x) }, false},
		{"a number of seconds with its milliseconds", frac, false},
		{"the second, as a time of seconds, beside a $date", func(x time.Time) string { return date(x) + `,"claimed_at":"` + x.UTC().Format(time.RFC3339) + `"` }, true},
		{"the second, as a number of seconds", func(x time.Time) string { return `"claimedAt":{"$numberLong":"` + sec(x) + `"}` }, true},
		{"the second, as a time of seconds", func(x time.Time) string { return `"claimed_at":"` + x.UTC().Format(time.RFC3339) + `"` }, true},
		{"the second beside a name", func(x time.Time) string { return name(x) + `,"claimedAt":{"$numberLong":"` + sec(x) + `"}` }, true},
		{"the second beside a number of seconds with its milliseconds", func(x time.Time) string { return frac(x) + `,"claimedAt":{"$numberLong":"` + sec(x) + `"}` }, true},
		// A page cursor holds the second in its decoded text, a word no
		// instant of the answer is: the second stands outside the $date.
		{"the second in a base64 page cursor beside a $date", func(x time.Time) string {
			return date(x) + `,"cursor":"` + base64.StdEncoding.EncodeToString([]byte(`{"t":`+sec(x)+`}`)) + `"`
		}, true},
	} {
		for _, made := range []struct {
			what       string
			rMade      time.Time
			fMade      time.Time
			inBothRuns bool
		}{
			{"made 300 ms before the call in round F", rMade, fAt.Add(-300 * time.Millisecond), false},
			{"made at the first millisecond of the call's second in round F", rMade, fAt.Truncate(time.Second), false},
			{"made 300 ms before the call in both runs", rAt.Add(-300 * time.Millisecond), fAt.Add(-300 * time.Millisecond), true},
			{"made at the first millisecond of the call's second in both runs", rAt.Truncate(time.Second), fAt.Truncate(time.Second), true},
		} {
			// An answer that holds the second at the call's cut in both
			// runs is an answer, not a pick: those rows are step 3's.
			if tc.pick && made.inBothRuns {
				continue
			}
			rm, lm := find(tc.fields(made.rMade)), find(tc.fields(made.fMade))
			for _, form := range []struct {
				what    string
				earlier []Earlier
				raw     []Raw
			}{
				{"as a recording", nil, []Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))}},
				{"leaf by leaf", []Earlier{leaves(rm, lm)}, nil},
			} {
				for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
					in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), form.earlier, form.raw)
					what := tc.what + ", " + made.what + ", " + form.what
					if mode == ModeElsewhere {
						in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
						what += ", landed from another test"
					}
					d := oWord(t, Decide(in), rec, live)
					if tc.pick {
						oRefused(t, what, d, RefusalPick, "mock-799")
					} else {
						iClock(t, what, d)
					}
				}
			}
		}
	}
	// A $date beside its day, as a recording: the day names no second, so
	// the call's own clock read, made at the second's first millisecond or
	// later. (Leaf by leaf, the day is a leaf of its own, judged as a day
	// alone is.)
	day := func(x time.Time) string { return date(x) + `,"day":"` + x.UTC().Format("2006-01-02") + `"` }
	for _, off := range []time.Duration{0, 100 * time.Millisecond} {
		raw := []Raw{RawOf(find(day(rMade)), true).ServedAs(1).ServedLive(RawOf(find(day(fAt.Truncate(time.Second).Add(off))), true))}
		for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
			in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, raw)
			if mode == ModeElsewhere {
				in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
			}
			iClock(t, "a $date beside its day made "+off.String()+" into the call's second in round F", oWord(t, Decide(in), rec, live))
		}
	}
	// A $date beside another time of seconds, the same in both runs (an
	// expiry a day on): that time names no moment of the call's, at its cut
	// or any other, so the $date alone holds F's second, and only as its
	// finer cut: the call's own clock read.
	expiry := `,"expires_at":"` + oAt.Add(24*time.Hour).UTC().Format(time.RFC3339) + `"`
	for _, off := range []time.Duration{0, 100 * time.Millisecond} {
		raw := []Raw{RawOf(find(date(rMade)+expiry), true).ServedAs(1).ServedLive(RawOf(find(date(fAt.Truncate(time.Second).Add(off))+expiry), true))}
		for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
			in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, raw)
			if mode == ModeElsewhere {
				in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
			}
			iClock(t, "a $date beside another time of seconds made "+off.String()+" into the call's second in round F", oWord(t, Decide(in), rec, live))
		}
	}
	// The call's second as a typed time of seconds, which the answer holds
	// in both runs only inside a finer time made in the call's second: no
	// source keploy can bind, so the call's own clock read, whether that
	// time was made at the second's first millisecond or later.
	tsec := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		what   string
		fields func(time.Time) string
	}{
		{"a $date of milliseconds", date},
		{"a time of milliseconds", iso},
		{"a time of milliseconds at an offset", isoAt},
	} {
		for _, off := range []time.Duration{0, 100 * time.Millisecond} {
			rm, lm := find(tc.fields(rAt.Truncate(time.Second).Add(off))), find(tc.fields(fAt.Truncate(time.Second).Add(off)))
			for _, form := range []struct {
				what    string
				earlier []Earlier
				raw     []Raw
			}{
				{"as a recording", nil, []Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))}},
				{"leaf by leaf", []Earlier{leaves(rm, lm)}, nil},
			} {
				in := iCall("body.cmd.query.dirtyAt.$lte", Instant, tsec(rAt), tsec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), form.earlier, form.raw)
				iClock(t, "a typed second beside "+tc.what+" made "+off.String()+" into the call's second in both runs, "+form.what, oWord(t, Decide(in), tsec(rAt), tsec(fAt)))
			}
		}
	}
	// A time of milliseconds its encoder writes without its trailing zeros
	// (Go's RFC3339Nano, Java's Instant.toString, Python's isoformat, a
	// number of seconds written as a float): made at a second's first
	// millisecond it shows no fraction at all. A place
	// is judged at the finest unit either run's text there shows, so one
	// run's whole second is still that finer time's cut, and the verdict is
	// the one every other millisecond gives. Only where neither run shows a
	// fraction is the place a time of seconds.
	for _, tc := range []struct {
		what   string
		fields func(time.Time) string
	}{
		{"a time without its trailing zeros", func(x time.Time) string { return `"created_at":"` + x.UTC().Format(time.RFC3339Nano) + `"` }},
		{"a time without its trailing zeros at an offset", func(x time.Time) string {
			return `"created_at":"` + x.In(time.FixedZone("", 19800)).Format(time.RFC3339Nano) + `"`
		}},
		{"a time without its trailing zeros, its fraction after a comma", func(x time.Time) string {
			return `"created_at":"` + strings.Replace(x.UTC().Format(time.RFC3339Nano), ".", ",", 1) + `"`
		}},
		{"a number of seconds without its trailing zeros", func(x time.Time) string {
			return `"made":` + strconv.FormatFloat(float64(x.UnixMilli())/1000, 'f', -1, 64)
		}},
		// Beside an expiry a day on, the same in both runs: a time of
		// another layout, which keploy reads first. Each run's number is
		// paired with the other's, the k-th of its own layout.
		{"a number of seconds without its trailing zeros beside an expiry", func(x time.Time) string {
			return `"expires_at":"` + oAt.Add(24*time.Hour).UTC().Format(time.RFC3339) + `","made":` + strconv.FormatFloat(float64(x.UnixMilli())/1000, 'f', -1, 64)
		}},
	} {
		offs := []time.Duration{0, time.Millisecond, 120 * time.Millisecond, 999 * time.Millisecond}
		forms := func(rm, lm *models.Mock) []struct {
			what    string
			earlier []Earlier
			raw     []Raw
		} {
			return []struct {
				what    string
				earlier []Earlier
				raw     []Raw
			}{
				{"as a recording", nil, []Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))}},
				{"leaf by leaf", []Earlier{leaves(rm, lm)}, nil},
			}
		}
		// The pick side: made 37 s before the call in the recording (a
		// fraction shows), and in the call's second in round F.
		for _, made := range []struct {
			rMade time.Time
			fOff  time.Duration
			pick  bool
		}{
			{rMade, 0, false}, {rMade, time.Millisecond, false}, {rMade, 120 * time.Millisecond, false}, {rMade, 999 * time.Millisecond, false},
			{rMade.Truncate(time.Second), 0, true},
		} {
			rm, lm := find(tc.fields(made.rMade)), find(tc.fields(fAt.Truncate(time.Second).Add(made.fOff)))
			for _, form := range forms(rm, lm) {
				for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
					in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), form.earlier, form.raw)
					what := fmt.Sprintf("%s made %v before the call and %v into the call's second in round F, %s", tc.what, rAt.Sub(made.rMade), made.fOff, form.what)
					if mode == ModeElsewhere {
						in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
						what += ", landed from another test"
					}
					d := oWord(t, Decide(in), rec, live)
					if made.pick {
						oRefused(t, what, d, RefusalPick, "mock-799")
					} else {
						iClock(t, what, d)
					}
				}
			}
		}
		// The answer side: a typed second the answer holds in both runs
		// only inside that time, made in the call's second in each run.
		for _, ro := range offs {
			for _, fo := range offs {
				rm, lm := find(tc.fields(rAt.Truncate(time.Second).Add(ro))), find(tc.fields(fAt.Truncate(time.Second).Add(fo)))
				for _, form := range forms(rm, lm) {
					in := iCall("body.cmd.query.dirtyAt.$lte", Instant, tsec(rAt), tsec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), form.earlier, form.raw)
					what := fmt.Sprintf("a typed second beside %s made %v and %v into the call's second, %s", tc.what, ro, fo, form.what)
					d := oWord(t, Decide(in), tsec(rAt), tsec(fAt))
					if ro == 0 && fo == 0 {
						iValue(t, what, d, "mock-799")
					} else {
						iClock(t, what, d)
					}
				}
			}
		}
	}
	// Round F's second as a time or a number of seconds, beside the other
	// run's text at that place that is no trimmed form of it: the place
	// holds round F's second at the call's cut, a pick. The other run holds
	// a number of milliseconds there (another shape), or one more time
	// before it (another count), or its own second after a finer time (the
	// same count, read in order).
	trim := func(x time.Time) string { return x.UTC().Format(time.RFC3339Nano) }
	for _, c := range []struct{ what, rec, live string }{
		{"a number of seconds where the recording holds one of milliseconds",
			`"claimedAt":{"$numberLong":"` + iEpochMs(rMade) + `"}`, `"claimedAt":{"$numberLong":"` + sec(fAt) + `"}`},
		{"a time of seconds where the recording holds a finer time before it",
			`"created_at":"` + trim(rMade) + `","claimed_at":"` + tsec(rMade) + `"`, `"claimed_at":"` + tsec(fAt) + `"`},
		{"a time of seconds after a finer time in both runs",
			`"created_at":"` + trim(rMade) + `","claimed_at":"` + tsec(rMade) + `"`, `"created_at":"` + trim(fAt) + `","claimed_at":"` + tsec(fAt) + `"`},
	} {
		raw := []Raw{RawOf(find(c.rec), true).ServedAs(1).ServedLive(RawOf(find(c.live), true))}
		in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, raw)
		oRefused(t, "round F's second as "+c.what, oWord(t, Decide(in), rec, live), RefusalPick, "mock-799")
	}
}

// A value the clock reading takes for a look-back no replay of its call
// compares, which a place of the same test held before the call only in
// another form (the epoch nanoseconds of a name, beside the second): as
// much a copy of that place as a look-back, it is refused naming the place
// that held it, never as an offset keploy cannot check that names none.
// Read as a look-back by either clock reading or by none, the decision is
// the same.
func TestALookBackAPlaceHeldInAnotherFormIsRefusedNamingThatPlace(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }

	// A keploy test: the second 5 s before its call in both runs, beside
	// an answer whose name holds the moment as epoch nanoseconds.
	const p = "body.created_at"
	rAt, fAt := oAt.Truncate(time.Second).Add(700*time.Millisecond), oAt.Truncate(time.Second).Add(102*time.Second+700*time.Millisecond)
	before := oAt.Sub(rAt.Add(-300 * time.Millisecond))
	name := func(x time.Time) string { return "[E2E] x-" + iEpochNs(x.Add(-300*time.Millisecond)) }
	ans := []Raw{oAnswer("mock-1624", before, name(rAt)).ServedAs(1).ServedLive(oAnswer("mock-1624", before, name(fAt)))}
	far := iWin(rAt.Add(5*time.Second), fAt.Add(5*time.Second), 100*time.Millisecond)
	d := oWord(t, Decide(iCall(p, String, sec(rAt), sec(fAt), rAt, fAt, far, nil, ans)), sec(rAt), sec(fAt))
	oRefused(t, "a keploy test's second 5 s before its call in both runs", d, RefusalNoClockRead, "mock-1624", "another form")

	// A client test with no begin known: the second 1 s before its call in
	// both runs, beside a name sent before it holding epoch nanoseconds.
	const q = "body.docs.msgs.0.app.createdAt"
	nanos := func(x time.Time) string { return "app-crud-" + strconv.FormatInt(x.UnixNano(), 10) }
	r0, f0 := oAt.Add(6300*time.Millisecond), oAt.Add(61*time.Second+14800*time.Millisecond)
	cAt, cfAt := r0.Add(900*time.Millisecond), f0.Add(1400*time.Millisecond)
	rec, live := sec(r0.Add(100*time.Millisecond)), sec(f0.Add(200*time.Millisecond))
	sent := iSent("mock-502", cAt.Add(-300*time.Millisecond), "body.cmd.query.app.name", String, nanos(r0.Add(90*time.Millisecond)), nanos(f0.Add(190*time.Millisecond)))
	in := iCall(q, Number, rec, live, cAt, cfAt, iWin(cAt, cfAt, 100*time.Millisecond), []Earlier{sent}, nil)
	in.Drifts = oClientDrifts()
	oRefused(t, "a client test's second 1 s before its call, no begin known", oWord(t, Decide(in), rec, live), RefusalNoClockRead, "mock-502", "another form")
}

// Where a place held the moment in another form, a look-back no replay of
// its call compares is never read from its test's begin: a client test's
// milliseconds exactly a minute before its call in both runs, a minute
// after its test began, are refused, never a plain read since the begin,
// which a replay that dropped the look-back would pass. With no such place
// they stay refused as one keploy cannot check.
func TestALookBackAPlaceHeldInAnotherFormIsNeverReadFromItsTestsBegin(t *testing.T) {
	const p = "body.since"
	rAt := oAt.Add(2*time.Hour + 400*time.Millisecond)
	fAt := rAt.Add(time.Hour + 130*time.Millisecond)
	rT, fT := rAt.Add(-time.Minute).Truncate(time.Millisecond), fAt.Add(-time.Minute).Truncate(time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = rAt.Add(-2*time.Minute), fAt.Add(-2*time.Minute)
	held := iSent("mock-9", rAt.Add(-30*time.Second), "body.name", String, "job-"+iEpochNs(rT), "job-"+iEpochNs(fT))
	call := func(earlier []Earlier) Decision {
		in := iCall(p, String, iMs(rT), iMs(fT), rAt, fAt, w, earlier, nil)
		in.Drifts = oClientDrifts()
		return oWord(t, Decide(in), iMs(rT), iMs(fT))
	}
	oRefused(t, "a look-back no replay compares beside a place in another form", call([]Earlier{held}), RefusalNoClockRead, "mock-9", "another form", "clock - 1m")
	oRefused(t, "a look-back no replay compares", call(nil), RefusalCannotCheck, "clock - 1m")
	// Only a place of the call's scope, sent before the call, is such a
	// place: one sent at the call's own instant may have come after it, and
	// one of another test is outside the scope. The look-back stays refused
	// as one keploy cannot check, its line naming neither as a place that
	// held the moment in another form.
	after := held
	after.At = rAt
	other := held
	other.Own, other.Run = false, true
	for _, c := range []struct {
		what string
		e    Earlier
	}{
		{"beside a place in another form sent at the call's own instant", after},
		{"beside another test's place in another form", other},
	} {
		d := call([]Earlier{c.e})
		oRefused(t, "a look-back no replay compares "+c.what, d, RefusalCannotCheck, "clock - 1m")
		if d.Refusal != nil && (strings.Contains(d.Refusal.Why, "another form") || strings.Contains(d.Line, "another form")) {
			t.Errorf("a look-back no replay compares %s: refused %q (line %q); want no place named as holding the moment in another form", c.what, d.Refusal.Why, d.Line)
		}
	}
}

// A recording made after the call that round F served before it, whose view
// holds round F's second only in the request the application sent there (a
// worker that claims a row with F's second): that is a send, which the send
// rules decide, never something round F gave the application. The call's
// own window holds the read, so the second is a Clock, checked at every
// replay, as it is where the worker sends another second. Where round F's
// answer to the worker gives F's second back, F gave it: refused as a pick
// naming the recording. One rule for a Mongo command and a Postgres query.
// The same holds where the worker was answered before the call, or at the
// call's own instant (its state in round F unknown): the send is no pick. Beside
// an answer of the call's scope that holds both runs' seconds, the send is
// no second source either: the value is bound at that answer.
func TestOutcomeASecondOnlyTheApplicationSentToALaterRecordingIsNoPick(t *testing.T) {
	const p = "body.cmd.documents.0.createdAt"
	rAt := oAt.Truncate(time.Second).Add(7*time.Second + 400*time.Millisecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 200*time.Millisecond)
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	rec, live, other := sec(rAt), sec(fAt), sec(fAt.Add(7*time.Second))
	// mock-0 was asked 15 s after the call and answered 1 ms later.
	later := rAt.Add(15 * time.Second)
	num := func(s string) string { return `{"$numberLong":"` + s + `"}` }
	mongo := func(asked time.Time, sent, claimed string) Raw {
		value := "null"
		if claimed != "" {
			value = `{"claimedAt":` + num(claimed) + `}`
		}
		return RawOf(&models.Mock{Name: "mock-0", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","update":{"$set":{"claimedAt":` + num(sent) + `}},"$db":"app"} }`,
			}}}},
			MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"value":` + value + `,"ok":{"$numberDouble":"1.0"}} }`,
			}}}},
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Millisecond),
		}}, false)
	}
	pg := func(asked time.Time, sent, claimed string) Raw {
		var rows []models.PostgresV3Cells
		if claimed != "" {
			rows = []models.PostgresV3Cells{{{Value: claimed}}}
		}
		return RawOf(&models.Mock{Name: "mock-0", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "UPDATE ptw_endpoints SET claimed_at = " + sent + " WHERE id = 1 RETURNING claimed_at",
				Response:      &models.PostgresV3Response{Rows: rows, CommandComplete: "UPDATE 1"},
			}},
		}}, false)
	}
	w := iWin(rAt, fAt, 100*time.Millisecond)
	decide := func(raw ...Raw) Decision {
		return oWord(t, Decide(iCall(p, Number, rec, live, rAt, fAt, w, nil, raw)), rec, live)
	}
	for _, proto := range []struct {
		what string
		raw  func(asked time.Time, sent, claimed string) Raw
	}{{"a Mongo command", mongo}, {"a Postgres query", pg}} {
		recorded := proto.raw(later, sec(later), "")
		for _, tc := range []struct {
			what string
			view Raw
			pick bool
		}{
			{"the worker sends round F's second", proto.raw(later, live, ""), false},
			{"the worker sends another second", proto.raw(later, other, ""), false},
			{"round F's answer to the worker gives round F's second back", proto.raw(later, other, live), true},
		} {
			what := proto.what + ": " + tc.what
			d := decide(recorded.ServedAs(1).ServedLive(tc.view))
			if tc.pick {
				oRefused(t, what, d, RefusalPick, "mock-0")
			} else {
				iClock(t, what, d)
			}
		}
		// Answered before the call (300 ms before it, in the call's second),
		// or at the call's own instant: the recorded worker sent an earlier
		// second, round F's sent F's.
		for _, asked := range []struct {
			what string
			at   time.Time
		}{{"answered before the call", rAt.Add(-300 * time.Millisecond)}, {"answered at the call's own instant", rAt.Add(-time.Millisecond)}} {
			raw := proto.raw(asked.at, sec(rAt.Add(-5*time.Second)), "").ServedAs(1).ServedLive(proto.raw(asked.at, live, ""))
			iClock(t, proto.what+": the worker, "+asked.what+", sends round F's second", decide(raw))
		}
		ans := oAnswer("mock-1624", time.Second, rec).ServedAs(1).ServedLive(oAnswer("mock-1624", time.Second, live))
		iValue(t, proto.what+": the worker sends round F's second beside an answer that holds both runs' seconds",
			decide(ans, recorded.ServedAs(2).ServedLive(proto.raw(later, live, ""))), "mock-1624")
	}
}

// A recording keploy read once and cannot read again leaves the call
// unread: which half of it holds round F's second (its answer, a pick; its
// request, a send) is read again from the recording, never decided from
// what an earlier read of the whole text kept, and a read that fails
// refuses every change of the call, naming the recording keploy could not
// read again (and not one it read). The recording is past the set's store
// of payloads, so each read loads it again, and its load fails after the
// first.
func TestOutcomeARecordingReadOnceAndNotAgainLeavesTheCallUnread(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	const p = "body.cmd.documents.0.createdAt"
	rAt := oAt.Truncate(time.Second).Add(7*time.Second + 400*time.Millisecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 200*time.Millisecond)
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	rec, live := sec(rAt), sec(fAt)
	later := rAt.Add(15 * time.Second)
	worker := &models.Mock{Name: "mock-0", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","update":{"$set":{"claimedAt":{"$numberLong":"` + live + `"}}},"$db":"app"} }`,
		}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"value":null,"ok":{"$numberDouble":"1.0"}} }`,
		}}}},
		ReqTimestampMock: later, ResTimestampMock: later.Add(time.Millisecond),
	}}
	for _, failFrom := range []int{1, 2} {
		// A set of its own each: the store keeps whether a recording was
		// read past its payload.
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		dc := tr.Drifts()
		dc.appClock = true
		loads := 0
		raw := dc.LazyRawOf(worker, true, func() (*models.Mock, error) {
			if loads++; loads >= failFrom {
				return nil, errors.New("the parked answer is gone")
			}
			return worker, nil
		}).ServedAs(1).ServedAsRecorded()
		read := RawOf(getAnswer("mock-9", "http://api/items/1", `{"note":"kept"}`, rAt.Add(-time.Second)), true).ServedAs(2).ServedAsRecorded()
		in := iCall(p, Number, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, []Raw{read, raw})
		in.Drifts = dc
		what := fmt.Sprintf("a recording whose load fails from its read %d on", failFrom)
		d := oWord(t, Decide(in), rec, live)
		tr.End()
		if loads < failFrom {
			t.Errorf("%s: loaded %d times; the row needs that read to happen", what, loads)
		}
		oRefused(t, what, d, RefusalUnread, "mock-0")
		// The line names what keploy could not read, once, and neither a
		// recording it read (mock-9) nor a budget the call did not pass.
		if d.Refusal == nil {
			continue
		}
		if why := d.Refusal.Why; strings.Contains(why, "budget") || strings.Contains(why, "mock-9") ||
			failFrom > 1 && (!strings.Contains(why, "read mock-0 again") || strings.Count(why, "mock-0") != 1) {
			t.Errorf("%s: refused %q; want it to say keploy could not read mock-0 again, naming it once", what, why)
		}
	}
}

// An answer keploy reads again for each question it asks of it, and that
// it cannot read from some read on, leaves the call unread or decided as
// if read whole, whichever read fails: never another verdict, and never a
// crash. The answer, given before the call in both runs, holds the second
// read as a number of seconds, beside a time of milliseconds of the day
// before. Read whole, it is the second's source: a Value bound there. The
// answer is past the set's store of payloads and the call's index keeps
// none of its text, so each question loads it again.
func TestOutcomeAnAnswerNotReadAgainIsUnreadOrReadWhole(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	const p = "body.created_at"
	rAt, fAt := oAt.Add(700*time.Millisecond), oAt.Add(102*time.Second+700*time.Millisecond)
	before := oAt.Sub(rAt.Add(-300 * time.Millisecond))
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	cells := func(x time.Time) []models.PostgresV3Cells {
		return []models.PostgresV3Cells{{{Value: sec(x)}, {Value: iMs(oAt.Add(-24*time.Hour + 123*time.Millisecond))}}}
	}
	run := func(failFrom int) (Decision, int) {
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		m := oPgMock("mock-1624", before, "SELECT next_slot FROM mock-1624", cells(rAt), "SELECT 1")
		loads := 0
		raw := dc.LazyRawOf(m, true, func() (*models.Mock, error) {
			if loads++; failFrom > 0 && loads >= failFrom {
				return nil, errors.New("the parked answer is gone")
			}
			return m, nil
		}).ServedAs(1).ServedLive(RawOf(oPgMock("mock-1624", before, "SELECT next_slot FROM mock-1624", cells(fAt), "SELECT 1"), true))
		in := iCall(p, String, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, []Raw{raw})
		in.Drifts = dc
		return oWord(t, Decide(in), sec(rAt), sec(fAt)), loads
	}
	whole, all := run(0)
	iValue(t, "the answer read whole", whole, "mock-1624")
	if all < 4 {
		t.Fatalf("read whole, the answer was loaded %d times; the row needs it read again for each question", all)
	}
	unread := 0
	for failFrom := 1; failFrom <= all; failFrom++ {
		what := fmt.Sprintf("an answer whose load fails from its read %d of %d", failFrom, all)
		d, _ := run(failFrom)
		if d.Outcome == OutcomeRefused && d.Refusal != nil && d.Refusal.Kind == RefusalUnread {
			oRefused(t, what, d, RefusalUnread, "mock-1624")
			unread++
		} else if oDescribe([]Decision{d}) != oDescribe([]Decision{whole}) {
			t.Errorf("%s: %s; want it refused as unread, or %s as read whole", what, oDescribe([]Decision{d}), oDescribe([]Decision{whole}))
		}
	}
	if unread == 0 {
		t.Errorf("no failed load left the call unread")
	}
}

// A place whose text keploy cannot read again counts as holding the call's
// word outside its times finer than the call's moment: never as a finer
// time's cut alone (fail closed). Read, the text holds the second only
// inside a number of seconds with its milliseconds. Through Decide each
// place's text is in hand by then (a recording's halves are cut from its
// text read once, a view is read when it is handed in), so the branch is
// pinned here, at the question, on a recording read lazily.
func TestOutcomeAPlaceNotReadAgainHoldsTheWordOutsideItsFinerTimes(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96*time.Second + 123*time.Millisecond)
	w := strconv.FormatInt(at.Unix(), 10)
	m := getAnswer("mock-7", "http://api/jobs/1", fmt.Sprintf(`{"made":%d.123}`, at.Unix()), oAt.Add(-time.Second))
	gone := false
	raw := dr.LazyRawOf(m, true, func() (*models.Mock, error) {
		if gone {
			return nil, errors.New("the parked answer is gone")
		}
		return m, nil
	})
	sec := moment{res: time.Second}
	for _, tc := range []struct {
		what string
		gone bool
		want bool
	}{
		{"read again", false, false},
		{"not read again", true, true},
	} {
		x := newReaderIndex(dr)
		tx := x.textOf(raw)
		gone = false
		spans, read := tx.spansOf()
		if !read || !slices.ContainsFunc(spans, func(sp instant) bool { return sp.res < sec.res }) {
			t.Fatalf("%s: read %v, %d instants; the row needs the number of seconds with its milliseconds read", tc.what, read, len(spans))
		}
		gone = tc.gone
		if got := heldOutside(tx, spans, w, sec); got != tc.want {
			t.Errorf("%s: the second held outside the finer time: %v; want %v", tc.what, got, tc.want)
		}
	}
}

// A place whose text, or the other run's text there, keploy cannot read
// again, or the other run's text keploy could not read at all, keeps its
// instants at its own text's unit, as read: a time the recording wrote as
// a whole second, which the other run wrote with its milliseconds, stays a
// second's (fail closed: it names the call's second), and is never judged
// by a text not read, nor does the question crash. Read again, the place
// is the other run's millisecond. Nor is work done that no answer needs:
// the other run's text is not loaded again where the recording was not
// read again, and no instant is scanned where the other run's were not
// read. Through Decide each place's text is in hand by then, so the
// branch is pinned here, at the question, on recordings read lazily.
func TestOutcomeAPlaceNotReadAgainIsJudgedAtItsOwnUnit(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second)
	body := func(x time.Time) string { return `{"made":"` + x.UTC().Format(time.RFC3339Nano) + `"}` }
	// lazy is m read lazily, whose load fails past its first failAfter
	// loads (none: never); loads counts them.
	lazy := func(m *models.Mock, failAfter int, loads *int) Raw {
		return dr.LazyRawOf(m, true, func() (*models.Mock, error) {
			if *loads++; failAfter >= 0 && *loads > failAfter {
				return nil, errors.New("the parked answer is gone")
			}
			return m, nil
		})
	}
	recorded := getAnswer("mock-7", "http://api/jobs/1", body(at), oAt.Add(-time.Second))
	given := getAnswer("mock-7", "http://api/jobs/1", body(at.Add(123*time.Millisecond)), oAt.Add(-time.Second))
	// k is how many loads one read of the instants takes.
	k := 0
	if spans, read := newReaderIndex(dr).textOf(lazy(recorded, -1, &k)).spansOf(); !read ||
		!slices.ContainsFunc(spans, func(sp instant) bool { return sp.keyed && sp.res == time.Second }) {
		t.Fatalf("read %v, %d instants; the row needs the recording's time read as a second's", read, len(spans))
	}
	if k == 0 {
		t.Fatalf("reading the recording loaded it %d times; the row needs it read lazily", k)
	}
	var dc decider
	for _, tc := range []struct {
		what             string
		failRec, failRun int
		want             time.Duration
	}{
		{"both read again", -1, -1, time.Millisecond},
		{"the recording not read again", k, -1, time.Second},
		{"the other run's text not read again", -1, k, time.Second},
		{"the other run's text not read", -1, 0, time.Second},
	} {
		x := newReaderIndex(dr)
		var nRec, nRun int
		tx, ox := x.textOf(lazy(recorded, tc.failRec, &nRec)), x.textOf(lazy(given, tc.failRun, &nRun))
		dc.passes = 0
		// The question must not crash: a text not read again is no text
		// to index, so a crash here fails the row by name.
		spans, read, crashed := func() (spans []instant, read bool, crashed any) {
			defer func() { crashed = recover() }()
			spans, read = dc.atFinest(tx, ox)
			return spans, read, nil
		}()
		if crashed != nil {
			t.Errorf("%s: the question crashed (%v); want the place judged at its own text's unit", tc.what, crashed)
			continue
		}
		if !read {
			t.Errorf("%s: the place's instants not read; want them read as the first read gave them", tc.what)
			continue
		}
		// Each text whose instants were read is read again: the recording
		// where the other run's instants were read, and the other run's
		// where the recording was read again.
		if tc.failRun != 0 && nRec <= k || tc.failRec < 0 && tc.failRun != 0 && nRun <= k {
			t.Fatalf("%s: loaded %d and %d times; the row needs each text read again after its instants", tc.what, nRec, nRun)
		}
		// No work no answer needs: where the recording is not read again,
		// the other run's text is not loaded again, and where the other
		// run's instants were not read, no instant of the place is scanned
		// for a finer unit.
		if tc.failRec >= 0 && nRun != k {
			t.Errorf("%s: the other run's text loaded %d times; want %d, to read its instants alone", tc.what, nRun, k)
		}
		if tc.failRun == 0 && dc.passes != 0 {
			t.Errorf("%s: %d instants scanned; want none, as the other run's instants were not read", tc.what, dc.passes)
		}
		var units []time.Duration
		for _, sp := range spans {
			if sp.keyed {
				units = append(units, sp.res)
			}
		}
		if len(units) != 1 || units[0] != tc.want {
			t.Errorf("%s: the place's times at %v; want one, at %v", tc.what, units, tc.want)
		}
	}
}

// A place is read again only for a time the other run writes at a finer
// unit, and then once (the founder's bounded work): a place whose time
// both runs write at one unit (a whole second, another second in each) is
// judged there with the recording loaded only to read its instants, and
// one with two times the other run writes with their milliseconds is read
// again once for both.
func TestOutcomeAPlaceIsReadAgainOnceAndOnlyForAFinerTime(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second)
	stamp := func(x time.Time) string { return `"` + x.UTC().Format(time.RFC3339Nano) + `"` }
	one := func(made, _ time.Time) string { return `{"made":` + stamp(made) + `}` }
	two := func(made, seen time.Time) string { return `{"made":` + stamp(made) + `,"seen":` + stamp(seen) + `}` }
	answer := func(body string) *models.Mock {
		return getAnswer("mock-7", "http://api/jobs/1", body, oAt.Add(-time.Second))
	}
	var dc decider
	for _, tc := range []struct {
		what      string
		body      func(made, seen time.Time) string
		rec, run  [2]time.Time
		want      []time.Duration
		readAgain int
	}{
		{"one time at the second in both runs", one, [2]time.Time{at}, [2]time.Time{at.Add(time.Second)}, []time.Duration{time.Second}, 0},
		{"two times the other run writes with their milliseconds", two,
			[2]time.Time{at, at.Add(2 * time.Second)}, [2]time.Time{at.Add(123 * time.Millisecond), at.Add(2*time.Second + 456*time.Millisecond)},
			[]time.Duration{time.Millisecond, time.Millisecond}, 1},
	} {
		loads := 0
		recorded := answer(tc.body(tc.rec[0], tc.rec[1]))
		raw := dr.LazyRawOf(recorded, true, func() (*models.Mock, error) { loads++; return recorded, nil })
		x := newReaderIndex(dr)
		if _, read := x.textOf(raw).spansOf(); !read || loads == 0 {
			t.Fatalf("%s: read %v, loaded %d times; the row needs the recording read lazily", tc.what, read, loads)
		}
		k := loads
		loads = 0
		spans, read := dc.atFinest(x.textOf(raw), x.textOf(RawOf(answer(tc.body(tc.run[0], tc.run[1])), true)))
		var units []time.Duration
		for _, sp := range spans {
			if sp.keyed {
				units = append(units, sp.res)
			}
		}
		if !read || !slices.Equal(units, tc.want) {
			t.Errorf("%s: read %v, the place's times at %v; want %v", tc.what, read, units, tc.want)
		}
		if loads != k+tc.readAgain {
			t.Errorf("%s: the recording loaded %d times; want %d to read its instants and %d again", tc.what, loads, k, tc.readAgain)
		}
	}
}

// Judging a place's instants at the unit the other run's text there shows
// leaves what the call's index keeps of the text as read: another place
// that holds the same text, whose other run wrote the whole second as the
// recording did, is a second's, and so is the text asked again.
func TestOutcomeAPlaceJudgedAtTheOtherRunsUnitLeavesTheTextAsRead(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	x := newReaderIndex(tr.Drifts())
	at := oAt.Add(96 * time.Second)
	body := func(x time.Time) string { return `{"made":"` + x.UTC().Format(time.RFC3339Nano) + `"}` }
	rec, fine := body(at), body(at.Add(123*time.Millisecond))
	unit := func(spans []instant, read bool) time.Duration {
		if !read || len(spans) != 1 {
			t.Fatalf("read %v, %d instants; want the one time read", read, len(spans))
		}
		return spans[0].res
	}
	var dc decider
	if got := unit(dc.atFinest(x.text(rec), x.text(fine))); got != time.Millisecond {
		t.Fatalf("the place the other run wrote with its milliseconds: at %v; want %v", got, time.Millisecond)
	}
	if got := unit(dc.atFinest(x.text(rec), x.text(rec))); got != time.Second {
		t.Errorf("a second place of the same text, written as a whole second in both runs: at %v; want %v", got, time.Second)
	}
	if got := unit(x.text(rec).spansOf()); got != time.Second {
		t.Errorf("the text asked again: at %v; want %v", got, time.Second)
	}
	if x.used == 0 {
		t.Errorf("the index kept nothing; the row needs the text's instants kept")
	}
}

// sentAtMock is a recording whose request sends the moment sent (an UPDATE
// that stamps a row), asked at req and answered at res (none where res is
// zero); its answer is the tag alone, or returns returned where it is set.
func sentAtMock(name, sent, returned string, req, res time.Time) *models.Mock {
	sql, resp := "UPDATE jobs SET at = '"+sent+"' WHERE id = 1", &models.PostgresV3Response{CommandComplete: "UPDATE 1"}
	if returned != "" {
		sql += " RETURNING at"
		resp.Rows = []models.PostgresV3Cells{{{Value: returned}}}
	}
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: req, ResTimestampMock: res,
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: sql, Response: resp,
		}},
	}}
}

// A moment that stands in a recording's request is what the recorded
// application sent: a send, placed by when it was sent and decided by the
// send rules, as an exchange's request leaf is, however the recording is
// handed in and whenever (or if ever) it was answered. Asked a second
// before the call, answered before it, two seconds after it or at no time
// keploy can read: an exchange of the test, and a start-up recording whose
// view sent round F's moment, are each a copy bound at the send; one served
// as recorded sent the recorded moment in round F too, a conflict; one
// never served sent nothing in round F, a time no place gave. Asked at the
// call's own instant, every form is the call's own read of the clock; asked
// after it, none is a source. What round F gave in a recording's answer
// keeps the answer's time: an answer that returns round F's moment, given
// before the call but answered after it or at no time keploy can read, is
// still refused as a pick.
func TestOutcomeAMomentARecordingsRequestHoldsIsASend(t *testing.T) {
	const p = "body.at"
	ms := time.Millisecond
	rAt := oAt.Add(2*time.Hour + 400*time.Millisecond + 123*time.Microsecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 211*time.Millisecond + 7*time.Microsecond)
	w := iWin(rAt, fAt, 100*ms)
	hows := []string{"an exchange of the test", "a start-up recording whose view sent round F's moment",
		"a start-up recording served as recorded", "a start-up recording never served"}
	decide := func(how string, req, res time.Duration, noAnswer bool) Decision {
		rec, live := iMicro(rAt.Add(req)), iMicro(fAt.Add(req))
		asked, answered := rAt.Add(req), rAt.Add(res)
		if noAnswer {
			answered = time.Time{}
		}
		m := sentAtMock("mock-5", rec, "", asked, answered)
		var earlier []Earlier
		var raw []Raw
		switch how {
		case hows[0]:
			m.Name = "mock-6"
			earlier = []Earlier{{Mock: "mock-6", At: asked, Own: true, Payload: RawOf(m, true),
				Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "q.at", Noise: "q.at", Type: String, Value: rec}, Live: live}}}}
		case hows[1]:
			raw = []Raw{RawOf(m, true).ServedAs(1).ServedLive(RawOf(sentAtMock("mock-5", live, "", asked, answered), true))}
		case hows[2]:
			raw = []Raw{RawOf(m, true).ServedAs(1).ServedAsRecorded()}
		default:
			raw = []Raw{RawOf(m, false)}
		}
		return oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, earlier, raw)), rec, live)
	}
	sentBefore := func(what, how string, d Decision) {
		t.Helper()
		switch how {
		case hows[0]:
			iValue(t, what, d, "mock-6")
		case hows[1]:
			iValue(t, what, d, "mock-5")
		case hows[2]:
			oRefused(t, what, d, RefusalConflict, "mock-5")
		default:
			oRefused(t, what, d, RefusalNoClockRead)
		}
	}
	for _, c := range []struct {
		name     string
		req, res time.Duration
		noAnswer bool
		check    func(what, how string, d Decision)
	}{
		{"asked a second before the call and answered 0.9s before it", -time.Second, -900 * ms, false, sentBefore},
		{"asked a second before the call and answered two seconds after it", -time.Second, 2 * time.Second, false, sentBefore},
		{"asked a second before the call, with no answer time", -time.Second, 0, true, sentBefore},
		{"asked at the call's own instant and answered 1ms after it", 0, ms, false, func(what, _ string, d Decision) {
			t.Helper()
			iClock(t, what, d)
		}},
		{"asked a second after the call", time.Second, time.Second + ms, false, func(what, _ string, d Decision) {
			t.Helper()
			oRefused(t, what, d, RefusalNoClockRead)
		}},
	} {
		for _, how := range hows {
			c.check(how+" "+c.name, how, decide(how, c.req, c.res, c.noAnswer))
		}
	}
	rec, live := iMicro(rAt.Add(-time.Second)), iMicro(fAt.Add(-time.Second))
	asked := rAt.Add(-time.Second)
	for _, c := range []struct {
		what     string
		answered time.Time
		why      string
	}{
		{"answered two seconds after the call", rAt.Add(2 * time.Second), "recorded after it"},
		{"with no answer time", time.Time{}, noAnswerAt},
	} {
		raw := RawOf(sentAtMock("mock-5", rec, rec, asked, c.answered), true).ServedAs(1).
			ServedLive(RawOf(sentAtMock("mock-5", live, live, asked, c.answered), true))
		d := oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, nil, []Raw{raw})), rec, live)
		oRefused(t, "round F's answer returns round F's moment, "+c.what, d, RefusalPick, "mock-5", c.why)
	}
}

// A recording whose request and answer both hold the recorded moment (an
// UPDATE that returns the time it set) is one place, named once in a line,
// though each half is read apart: as a Clock's place set aside, as a place
// that held the recorded value, and as another test's recording that sent
// the moment in both runs.
func TestOutcomeARecordingsTwoHalvesAreNamedOnce(t *testing.T) {
	const p = "body.at"
	ms := time.Millisecond
	rAt := oAt.Add(2*time.Hour + 400*time.Millisecond + 123*time.Microsecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 211*time.Millisecond + 7*time.Microsecond)
	w := iWin(rAt, fAt, 100*ms)
	once := func(what string, d Decision, want func(string, Decision)) {
		t.Helper()
		want(what, d)
		line := d.Line
		if d.Refusal != nil {
			line = d.Refusal.Why
			if n := strings.Count(strings.Join(d.Refusal.Places, " "), "mock-5"); n != 1 {
				t.Errorf("%s: places %v; want mock-5 once", what, d.Refusal.Places)
			}
		}
		if strings.Contains(line, "mock-5, mock-5") || strings.Count(line, "stood at mock-5") > 1 || strings.Count(line, "mock-5 in ") > 1 {
			t.Errorf("%s: %q names mock-5 more than once", what, line)
		}
	}
	asked := rAt.Add(-time.Second)
	rec, live := iMicro(rAt), iMicro(fAt)
	never := RawOf(sentAtMock("mock-5", rec, rec, asked, asked.Add(100*ms)), false)
	once("a Clock beside a recording never served", oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, nil, []Raw{never})), rec, live),
		func(what string, d Decision) { t.Helper(); iClock(t, what, d) })
	rec, live = iMicro(asked), iMicro(fAt.Add(-time.Second))
	never = RawOf(sentAtMock("mock-5", rec, rec, asked, asked.Add(100*ms)), false)
	once("a time no place gave, beside a recording never served", oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, nil, []Raw{never})), rec, live),
		func(what string, d Decision) { t.Helper(); oRefused(t, what, d, RefusalNoClockRead, "mock-5") })
	other := RawOf(sentAtMock("mock-5", rec, rec, asked, asked.Add(100*ms)), true).ServedAs(1).
		ServedLive(RawOf(sentAtMock("mock-5", live, live, asked, asked.Add(100*ms)), true)).OfAnotherTest()
	other.servedAfter = true
	once("a time another test's recording sent in both runs", oWord(t, Decide(iCall(p, String, rec, live, rAt, fAt, w, nil, []Raw{other})), rec, live),
		func(what string, d Decision) { t.Helper(); oRefused(t, what, d, RefusalNoClockRead, "mock-5") })
}

// A place's instants are each judged at the finest unit their place shows
// in either run: the JSON member each is the value of, named by its path
// (each element of a list at one path), its layout, and its text but for
// its fraction's digits. Where both runs hold as many there, the k-th of
// each are one time's; where a member's count differs (a list that grew or
// shrank between runs), each takes the finest unit the other run shows at
// the member at that path. A time of no member (an element of a list of
// times, one after a quoted word with no colon), or of a member named
// without quotes, pairs only by count; one at
// another member, offset or layout keeps its own unit. The recording is
// read again only where the other run shows a finer unit in one of its
// instants' layouts.
func TestOutcomeAPlaceIsJudgedAtTheFinestUnitItsMemberShowsInEitherRun(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second).Truncate(time.Second)
	s := func(x time.Time) string { return `"` + x.UTC().Format(time.RFC3339) + `"` }
	f := func(x time.Time) string {
		return `"` + x.Add(473*time.Millisecond).UTC().Format(time.RFC3339Nano) + `"`
	}
	n := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	nf := func(x time.Time) string { return n(x) + ".473" }
	ago := func(d int) time.Time { return at.Add(-time.Duration(d) * time.Second) }
	ms, sec := time.Millisecond, time.Second
	answer := func(body string) *models.Mock {
		return getAnswer("mock-7", "http://api/jobs/1", body, oAt.Add(-time.Second))
	}
	var dc decider
	for _, tc := range []struct {
		what, rec, run string
		want           []time.Duration
		readAgain      int
	}{
		{"one time the other run writes with its milliseconds", `{"made":` + s(at) + `}`, `{"made":` + f(ago(-1)) + `}`, []time.Duration{ms}, 1},
		{"one time at the second in both runs", `{"made":` + s(at) + `}`, `{"made":` + s(ago(-1)) + `}`, []time.Duration{sec}, 0},
		{"a list that grew between runs",
			`{"items":[{"created_at":` + s(at) + `},{"created_at":` + f(ago(40)) + `},{"created_at":` + f(ago(41)) + `}]}`,
			`{"items":[{"created_at":` + f(ago(80)) + `},{"created_at":` + f(ago(81)) + `}]}`, []time.Duration{ms, ms, ms}, 1},
		{"a list that shrank between runs", `{"items":[{"created_at":` + s(at) + `}]}`,
			`{"items":[{"created_at":` + f(ago(80)) + `},{"created_at":` + f(ago(81)) + `},{"created_at":` + f(ago(82)) + `}]}`, []time.Duration{ms}, 1},
		{"one member in two elements of a list, as many in each run", `[{"at":` + f(ago(50)) + `},{"at":` + s(at) + `}]`,
			`[{"at":` + f(ago(60)) + `},{"at":` + s(ago(10)) + `}]`, []time.Duration{ms, sec}, 1},
		{"elements of lists of times of other lengths", `{"rows":[[` + s(at) + `,` + f(ago(5)) + `]]}`,
			`{"rows":[[` + s(ago(60)) + `,` + f(ago(61)) + `],[` + s(ago(70)) + `,` + f(ago(71)) + `]]}`, []time.Duration{sec, ms}, 1},
		{"two members, the other run finer at one", `{"a":` + s(at) + `,"b":` + s(ago(-1)) + `}`,
			`{"a":` + f(ago(60)) + `,"b":` + s(ago(59)) + `}`, []time.Duration{ms, sec}, 1},
		{"a time at another member", `{"a":` + s(at) + `}`, `{"b":` + f(ago(60)) + `}`, []time.Duration{sec}, 1},
		{"a time at another offset", `{"a":` + s(at) + `}`,
			`{"a":"` + ago(60).Add(473*time.Millisecond).In(time.FixedZone("", 19800)).Format(time.RFC3339Nano) + `"}`, []time.Duration{sec}, 1},
		{"a time in another layout", `{"a":` + s(at) + `}`, `{"a":` + iEpochMs(ago(60)) + `}`, []time.Duration{sec}, 0},
		{"a fraction after a comma", `{"a":` + s(at) + `}`, `{"a":` + strings.Replace(f(ago(60)), ".", ",", 1) + `}`, []time.Duration{ms}, 1},
		{"a number of seconds with its fraction", `{"t":` + n(at) + `}`, `{"t":` + nf(ago(60)) + `}`, []time.Duration{ms}, 1},
		{"a list of numbers of seconds that grew", `[{"t":` + n(at) + `},{"t":` + nf(ago(5)) + `}]`, `[{"t":` + nf(ago(60)) + `}]`, []time.Duration{ms, ms}, 1},
		{"spaces about the colon, the list grew", `[{"a" : ` + s(at) + `}]`, `[{"a" : ` + f(ago(60)) + `},{"a" : ` + f(ago(61)) + `}]`, []time.Duration{ms}, 1},
		{"a member named without quotes, the list grew", `[{a:` + s(at) + `},{a:` + f(ago(5)) + `}]`, `[{a:` + f(ago(60)) + `}]`, []time.Duration{sec, ms}, 1},
		{"a time after a quoted word and no colon, the list grew", `["a" ` + s(at) + `]`, `["a" ` + f(ago(60)) + `,"a" ` + f(ago(61)) + `]`, []time.Duration{sec}, 1},
		{"a member of the same name at another path, the list grew", `{"at":` + s(at) + `,"items":[{"at":` + f(ago(40)) + `}]}`,
			`{"at":` + s(ago(-1)) + `,"items":[{"at":` + f(ago(80)) + `},{"at":` + f(ago(81)) + `}]}`, []time.Duration{sec, ms}, 1},
		{"a member of the same name in a sibling object, the list grew", `{"job":{"at":` + s(at) + `},"history":[{"at":` + f(ago(40)) + `}]}`,
			`{"job":{"at":` + s(ago(-1)) + `},"history":[{"at":` + f(ago(80)) + `},{"at":` + f(ago(81)) + `}]}`, []time.Duration{sec, ms}, 1},
		{"an escaped quote and a brace in a string before the member", `{"note":"a \"{","at":` + s(at) + `}`, `{"note":"b","at":` + f(ago(60)) + `}`, []time.Duration{ms}, 1},
		{"a quote alone on a line before the member", "\"\n" + `{"at":` + s(at) + `}`, `{"at":` + f(ago(60)) + `}`, []time.Duration{ms}, 1},
		{"a colon, a comma and closers with nothing open before the list, which grew", "a: b, c]}\n" + `{"items":[{"at":` + s(at) + `}]}`,
			`{"items":[{"at":` + f(ago(60)) + `},{"at":` + f(ago(61)) + `}]}`, []time.Duration{ms}, 1},
		{"a list in an object named by no key, the list grew", `{{"x":[{"at":` + s(at) + `}]}}`, `{{"x":[{"at":` + f(ago(60)) + `},{"at":` + f(ago(61)) + `}]}}`, []time.Duration{sec}, 1},
		{"a list in an object named by a bare word (a Mongo section), the list grew", `{ SectionSingle msg: {"items":[{"at":` + s(at) + `}]} }`,
			`{ SectionSingle msg: {"items":[{"at":` + f(ago(60)) + `},{"at":` + f(ago(61)) + `}]} }`, []time.Duration{ms}, 1},
		{"a member with no container open, in a field of its own", `"at":` + s(at), `"at":` + f(ago(60)), []time.Duration{ms}, 1},
		{"a quote alone on a line before a list that grew", "\"\n" + `{"items":[{"at":` + s(at) + `}]}`,
			`{"items":[{"at":` + f(ago(60)) + `},{"at":` + f(ago(61)) + `}]}`, []time.Duration{ms}, 1},
		{"an object after a comma with no key of its own (a text of another kind), beside the member before it",
			`{"a":{"at":` + f(ago(40)) + `},{"at":` + s(at) + `}}`,
			`{"a":{"at":` + f(ago(80)) + `},{"at":` + s(ago(-1)) + `},{"at":` + s(ago(-2)) + `}}`, []time.Duration{ms, sec}, 1},
		{"a member after a list that grew, at another path", `{"a":[{"at":` + f(ago(40)) + `}],"b":{"at":` + s(at) + `}}`,
			`{"a":[{"at":` + f(ago(80)) + `},{"at":` + f(ago(81)) + `}],"b":{"at":` + s(ago(-1)) + `}}`, []time.Duration{ms, sec}, 1},
		{"a key holding [] beside the list it spells, which grew", `{"a":[{"at":` + f(ago(40)) + `}],"a[]":{"at":` + s(at) + `}}`,
			`{"a":[{"at":` + f(ago(80)) + `},{"at":` + f(ago(81)) + `}],"a[]":{"at":` + s(ago(-1)) + `}}`, []time.Duration{ms, sec}, 1},
		{"a key holding a dot beside the member it spells", `{"b.at":` + s(at) + `}`, `{"b.at":` + s(ago(-1)) + `,"b":{"at":` + f(ago(80)) + `}}`, []time.Duration{sec}, 1},
		{"a key holding a quote beside the member its end spells", `{"x\"at":` + s(at) + `}`, `{"x\"at":` + s(ago(-1)) + `,"at":` + f(ago(80)) + `}`, []time.Duration{sec}, 1},
		{"a list under a key holding a quote beside the list its end spells", `{"x\"items":[{"at":` + s(at) + `}]}`,
			`{"x\"items":[{"at":` + s(ago(-1)) + `}],"items":[{"at":` + f(ago(80)) + `}]}`, []time.Duration{sec}, 1},
	} {
		loads := 0
		recorded := answer(tc.rec)
		raw := dr.LazyRawOf(recorded, true, func() (*models.Mock, error) { loads++; return recorded, nil })
		x := newReaderIndex(dr)
		if _, read := x.textOf(raw).spansOf(); !read || loads == 0 {
			t.Fatalf("%s: read %v, loaded %d times; the row needs the recording read lazily", tc.what, read, loads)
		}
		k := loads
		loads = 0
		// A text of another kind must not crash the question: a crash
		// fails the row by name.
		spans, read, crashed := func() (spans []instant, read bool, crashed any) {
			defer func() { crashed = recover() }()
			spans, read = dc.atFinest(x.textOf(raw), x.textOf(RawOf(answer(tc.run), true)))
			return spans, read, nil
		}()
		if crashed != nil {
			t.Errorf("%s: the question crashed (%v)", tc.what, crashed)
			continue
		}
		var units []time.Duration
		for _, sp := range spans {
			units = append(units, sp.res)
		}
		if !read || !slices.Equal(units, tc.want) {
			t.Errorf("%s: read %v, the place's times at %v; want %v", tc.what, read, units, tc.want)
		}
		if loads != k+tc.readAgain {
			t.Errorf("%s: the recording loaded %d times; want %d to read its instants and %d again", tc.what, loads, k, tc.readAgain)
		}
	}
}

// Each run's times are placed in that run's own layout: where a field
// before the time's (a request's url, laid out before its answer) is
// longer in one run, the other run's member still pairs with it. The
// recording's whole second takes the millisecond the other run shows at
// that member, whichever run's field is longer, and by however much: past
// the time's own start, and past the rest of the other run's answer after
// it.
func TestOutcomeEachRunsTimesArePlacedInItsOwnLayout(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second).Truncate(time.Second)
	s := func(x time.Time) string { return `"` + x.UTC().Format(time.RFC3339) + `"` }
	f := func(x time.Time) string {
		return `"` + x.Add(473*time.Millisecond).UTC().Format(time.RFC3339Nano) + `"`
	}
	ago := func(d int) time.Time { return at.Add(-time.Duration(d) * time.Second) }
	short, long := "http://api/jobs/1", "http://api/jobs/1?page=2&cursor=b3JkZXJzLzEyMzQ1Njc4OTA"
	var dc decider
	for _, tc := range []struct {
		what, recURL, runURL, rec, run string
	}{
		{"the recording's url longer", long, short, `{"at":` + s(at) + `}`, `{"at":` + f(ago(60)) + `}`},
		{"round F's url longer", short, long, `{"at":` + s(at) + `}`, `{"at":` + f(ago(60)) + `}`},
		{"the recording's url longer, a list before the time", long, short,
			`{"items":[{"at":` + f(ago(40)) + `}],"at":` + s(at) + `}`, `{"items":[{"at":` + f(ago(80)) + `}],"at":` + f(ago(60)) + `}`},
		{"round F's url longer, a list before the time", short, long,
			`{"items":[{"at":` + f(ago(40)) + `}],"at":` + s(at) + `}`, `{"items":[{"at":` + f(ago(80)) + `}],"at":` + f(ago(60)) + `}`},
	} {
		x := newReaderIndex(dr)
		rec, run := x.textOf(RawOf(getAnswer("mock-7", tc.recURL, tc.rec, oAt.Add(-time.Second)), true)), x.textOf(RawOf(getAnswer("mock-7", tc.runURL, tc.run, oAt.Add(-time.Second)), true))
		spans, read := dc.atFinest(rec, run)
		var units []time.Duration
		for _, sp := range spans {
			units = append(units, sp.res)
		}
		if want := slices.Repeat([]time.Duration{time.Millisecond}, strings.Count(tc.rec, `"at"`)); !read || !slices.Equal(units, want) {
			t.Errorf("%s: read %v, the place's times at %v; want %v", tc.what, read, units, want)
		}
	}
}

// A member in each element of a field's list is one member: a Mongo
// answer's responses (MongoResponses[0], [1], ... [10]) each holding "at",
// two or eleven in the recording and fewer in round F, pair at one member
// whatever their index, so the recording's whole second, in a response
// round F has no such index for, takes the millisecond round F shows there.
func TestOutcomeAMemberInEachElementOfAFieldsListIsOneMember(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second).Truncate(time.Second)
	s := func(x time.Time) string { return `"` + x.UTC().Format(time.RFC3339) + `"` }
	f := func(x time.Time) string {
		return `"` + x.Add(473*time.Millisecond).UTC().Format(time.RFC3339Nano) + `"`
	}
	ago := func(d int) time.Time { return at.Add(-time.Duration(d) * time.Second) }
	// answer is a Mongo answer of one response per time, each a section
	// whose document holds it at "at".
	answer := func(times []string) Raw {
		var rs []models.MongoResponse
		for _, x := range times {
			rs = append(rs, models.MongoResponse{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"at":` + x + `},"ok":{"$numberDouble":"1.0"}} }`,
			}}})
		}
		return RawOf(&models.Mock{Name: "mock-7", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"jobs","$db":"app"} }`,
			}}}},
			MongoResponses:   rs,
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}, true)
	}
	var dc decider
	for _, tc := range []struct {
		what     string
		rec, run []string
	}{
		{"two responses then one", []string{f(ago(40)), s(at)}, []string{f(ago(80))}},
		{"eleven responses then three", append(slices.Repeat([]string{f(ago(40))}, 10), s(at)), []string{f(ago(80)), f(ago(81)), f(ago(82))}},
	} {
		x := newReaderIndex(dr)
		spans, read := dc.atFinest(x.textOf(answer(tc.rec)), x.textOf(answer(tc.run)))
		var units []time.Duration
		for _, sp := range spans {
			units = append(units, sp.res)
		}
		if want := slices.Repeat([]time.Duration{time.Millisecond}, len(tc.rec)); !read || !slices.Equal(units, want) {
			t.Errorf("%s: read %v, the place's times at %v; want %v", tc.what, read, units, want)
		}
	}
}

// Judging a place's instants at the finest unit either run shows reads
// each text's instants a few times, however many it holds: a list of
// 50,000 times that grew by one between runs, every time judged at the
// other run's milliseconds, reads each instant at most five times, each
// read counted where it is made (pairing them included), walks each
// text's bytes at most once to name the members they stand at, and loads
// the recording again once.
func TestOutcomeAPlaceOfManyTimesIsReadInAFewPassesEach(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	at := oAt.Add(96 * time.Second).Truncate(time.Second)
	list := func(n int, made func(i int) time.Time) string {
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"created_at":"` + made(i).UTC().Format(time.RFC3339Nano) + `","n":"#` + strconv.Itoa(i) + `"}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	const n = 50000
	whole := func(i int) time.Time { return at.Add(-time.Duration(i) * time.Second) }
	fine := func(i int) time.Time { return whole(i).Add(-time.Hour + 473*time.Millisecond) }
	recorded := getAnswer("mock-7", "http://api/jobs/1", list(n, whole), oAt.Add(-time.Second))
	loads := 0
	raw := dr.LazyRawOf(recorded, true, func() (*models.Mock, error) { loads++; return recorded, nil })
	x := newReaderIndex(dr)
	other := RawOf(getAnswer("mock-7", "http://api/jobs/1", list(n+1, fine), oAt.Add(-time.Second)), true)
	tx, ox := x.textOf(raw), x.textOf(other)
	rs, read := tx.spansOf()
	os, oread := ox.spansOf()
	if !read || !oread || len(rs) != n || len(os) != n+1 || loads == 0 {
		t.Fatalf("read %v and %v, %d and %d instants, loaded %d times; the row needs %d and %d, the recording read lazily", read, oread, len(rs), len(os), loads, n, n+1)
	}
	k := loads
	loads = 0
	dc := decider{}
	spans, read := dc.atFinest(tx, ox)
	finer := 0
	for _, sp := range spans {
		if sp.res == time.Millisecond {
			finer++
		}
	}
	if !read || finer != n {
		t.Errorf("read %v, %d of %d times at the other run's milliseconds; want all", read, finer, n)
	}
	if bound := 5 * (len(rs) + len(os)); dc.passes > bound {
		t.Errorf("read %d instants of the two texts' %d and %d; want at most %d, five reads of each", dc.passes, len(rs), len(os), bound)
	}
	if bound := len(RawOf(recorded, true).Text()) + len(other.Text()); dc.walked > bound {
		t.Errorf("walked %d bytes of the two texts' %d; want each walked at most once", dc.walked, bound)
	}
	if loads != k+1 {
		t.Errorf("the recording loaded %d times; want %d to read its instants and 1 again", loads, k)
	}
}

// A find answer whose list grew or shrank between runs, each document
// holding the time it was made at, written without its trailing zeros (a
// time, or a JavaScript number of seconds): each document's time is judged
// at the finest unit that member shows in the other run, so the verdict is
// the one every other millisecond gives, wherever in its second the new
// document was made. The call's second inside round F's newest time is the
// call's own clock read (pick side), and so is a typed second the answer
// holds in both runs only inside its newest time (answer side). A second
// the application picks, at a member that also holds a finer time in
// another element of the list, in the same order in both runs, stays a
// pick: as many in each run, the k-th pair with each other.
func TestOutcomeAListThatGrewBetweenRunsKeepsItsTimesVerdict(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	tsec := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	find := func(docs []string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://db.local/apps/find", Body: `{"find":"apps"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"cursor":{"firstBatch":[` + strings.Join(docs, ",") + `]}}`},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	trim := func(x time.Time) string { return `{"created_at":"` + x.UTC().Format(time.RFC3339Nano) + `"}` }
	js := func(x time.Time) string {
		return `{"t":` + strconv.FormatFloat(float64(x.UnixMilli())/1000, 'f', -1, 64) + `}`
	}
	// docs is n older documents, made 40 s and more before end, then the
	// newest, made at made.
	docs := func(doc func(time.Time) string, end time.Time, n int, made time.Time) []string {
		var out []string
		for i := range n {
			out = append(out, doc(end.Add(-time.Duration(40+i)*time.Second+400*time.Millisecond)))
		}
		return append(out, doc(made))
	}
	offs := []time.Duration{0, time.Millisecond, 120 * time.Millisecond, 999 * time.Millisecond}
	for _, sh := range []struct {
		what         string
		doc          func(time.Time) string
		rOld, fOld   int
		typedAnswers bool
	}{
		{"a time, 2 documents then 3", trim, 1, 2, true},
		{"a time, 3 documents then 2", trim, 2, 1, true},
		{"a number of seconds, 2 documents then 3", js, 1, 2, false},
		{"a number of seconds, 3 documents then 2", js, 2, 1, false},
	} {
		// The pick side: the recording's newest made 37 s before its call,
		// round F's in the call's second.
		for _, fo := range offs {
			rm := find(docs(sh.doc, rAt, sh.rOld, rAt.Add(-37*time.Second)))
			lm := find(docs(sh.doc, fAt, sh.fOld, fAt.Truncate(time.Second).Add(fo)))
			for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
				in := iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
					[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))})
				what := fmt.Sprintf("%s, round F's newest made %v into the call's second", sh.what, fo)
				if mode == ModeElsewhere {
					in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
					what += ", landed from another test"
				}
				iClock(t, what, oWord(t, Decide(in), sec(rAt), sec(fAt)))
			}
		}
		if !sh.typedAnswers {
			continue
		}
		// The answer side: each run's newest made in its call's second.
		for _, ro := range offs {
			for _, fo := range offs {
				rm := find(docs(sh.doc, rAt, sh.rOld, rAt.Truncate(time.Second).Add(ro)))
				lm := find(docs(sh.doc, fAt, sh.fOld, fAt.Truncate(time.Second).Add(fo)))
				in := iCall("body.cmd.query.dirtyAt.$lte", Instant, tsec(rAt), tsec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
					[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))})
				iClock(t, fmt.Sprintf("a typed second beside %s, the newest made %v and %v into each call's second", sh.what, ro, fo), oWord(t, Decide(in), tsec(rAt), tsec(fAt)))
			}
		}
	}
	pick := func(x time.Time) string { return `{"at":"` + tsec(x) + `"}` }
	made := func(x time.Time) string { return `{"at":"` + x.UTC().Format(time.RFC3339Nano) + `"}` }
	rm := find([]string{made(rAt.Add(-50*time.Second + 400*time.Millisecond)), pick(rAt.Add(-37 * time.Second))})
	lm := find([]string{made(fAt.Add(-50*time.Second + 400*time.Millisecond)), pick(fAt)})
	in := iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
		[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))})
	oRefused(t, "a picked second beside a finer time at the same member, in the same order in both runs", oWord(t, Decide(in), sec(rAt), sec(fAt)), RefusalPick, "mock-799")
}

// A time the application picks, or one an answer gives that the
// application sends back, at a member whose name a member at another path
// of the same answer shares (a top-level "at" beside each item's "at", a
// job's "at" beside each history entry's "at", "a.b.at" beside each
// element's "b.at", and a top-level "at" after a note whose text ends in an
// escaped backslash, which closes the note's string), is paired only with the
// other run's times at the member at that path: a list at the other path
// that grew or shrank between runs, or was empty when it was recorded,
// lends it no finer unit. The pick stays refused, in either mode, and the
// time sent back stays the answer's value (refused where the call landed
// in another test's window, which teaches clock reads only), as where the
// list's length did not change: neither is ever learned as the call's own
// clock read.
func TestOutcomeATimeIsPairedAtTheMemberAtItsPathAlone(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	tsec := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	answer := func(body string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.local/jobs/1", Body: ``},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: body},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	// items is n entries (item), each holding the time it was made, with
	// its milliseconds, 40 s and more before end.
	items := func(item func(at string) string, end time.Time, n int) string {
		var s []string
		for i := range n {
			s = append(s, item(end.Add(-time.Duration(40+i)*time.Second+400*time.Millisecond).UTC().Format(time.RFC3339Nano)))
		}
		return strings.Join(s, ",")
	}
	at := func(at string) string { return `{"at":"` + at + `"}` }
	for _, sh := range []struct {
		what string
		item func(at string) string
		body func(at string, list string) string
	}{
		{"a top-level at beside each item's at", at, func(at, list string) string { return `{"at":"` + at + `","items":[` + list + `]}` }},
		{"a job's at beside each history entry's at", at, func(at, list string) string {
			return `{"job":{"at":"` + at + `"},"history":[` + list + `]}`
		}},
		{"a.b.at beside each element's b.at", func(at string) string { return `{"b":{"at":"` + at + `"}}` }, func(at, list string) string {
			return `{"a":{"b":{"at":"` + at + `"}},"c":[` + list + `]}`
		}},
		{"a top-level at after a note ending in an escaped backslash", at, func(at, list string) string {
			return `{"note":"x}]{[\"\\","at":"` + at + `","items":[` + list + `]}`
		}},
	} {
		for _, n := range [][2]int{{2, 2}, {2, 3}, {3, 2}, {1, 2}, {0, 1}, {1, 0}} {
			what := fmt.Sprintf("%s, %d entries then %d", sh.what, n[0], n[1])
			for _, mode := range []Mode{ModeOwn, ModeElsewhere} {
				elsewhere := func(in Input, what string) (Input, string) {
					if mode == ModeElsewhere {
						in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
						what += ", landed from another test"
					}
					return in, what
				}
				// The pick: round F's answer holds round F's second at the
				// member, the recording's a second 37 s before its call.
				rm := answer(sh.body(tsec(rAt.Add(-37*time.Second)), items(sh.item, rAt, n[0])))
				lm := answer(sh.body(tsec(fAt), items(sh.item, fAt, n[1])))
				in, w := elsewhere(iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
					[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))}), what+", round F's second picked")
				if mode == ModeOwn {
					oRefused(t, w, oWord(t, Decide(in), sec(rAt), sec(fAt)), RefusalPick, "mock-799")
				} else {
					oRefused(t, w, oWord(t, Decide(in), sec(rAt), sec(fAt)), RefusalPick)
				}
				// The answer: each run's call second at the member, sent
				// back.
				rm, lm = answer(sh.body(tsec(rAt), items(sh.item, rAt, n[0]))), answer(sh.body(tsec(fAt), items(sh.item, fAt, n[1])))
				in, w = elsewhere(iCall("body.cmd.query.dirtyAt.$lte", Instant, tsec(rAt), tsec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
					[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))}), what+", the answer's time sent back")
				if d := oWord(t, Decide(in), tsec(rAt), tsec(fAt)); mode == ModeOwn {
					iValue(t, w, d, "mock-799")
				} else {
					oRefused(t, w, d, RefusalLandedElsewhere)
				}
			}
		}
	}
}

// Each document of a find answer holds the time it was made, with its
// milliseconds (created_at), and a whole second the application picks
// (claimed_at); round F's newest claimed_at is round F's second. Where the
// list grew or shrank between runs, each claimed_at is judged at the
// finest unit the other run's claimed_at shows, never at a finer
// created_at's beside it: the pick stays refused, as where the list's
// length did not change.
func TestOutcomeAPickedSecondBesideAFinerMemberStaysRefusedInAListThatGrew(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	tsec := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	find := func(docs []string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://db.local/apps/find", Body: `{"find":"apps"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"cursor":{"firstBatch":[` + strings.Join(docs, ",") + `]}}`},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	doc := func(made, claimed time.Time) string {
		return `{"created_at":"` + made.UTC().Format(time.RFC3339Nano) + `","claimed_at":"` + tsec(claimed) + `"}`
	}
	// docs is n older documents, made 40 s and more before end, then the
	// newest, claimed at claimed.
	docs := func(end time.Time, n int, claimed time.Time) []string {
		var out []string
		for i := range n {
			x := end.Add(-time.Duration(40+i)*time.Second + 400*time.Millisecond)
			out = append(out, doc(x, x.Add(-3*time.Second)))
		}
		return append(out, doc(end.Add(-2*time.Second+473*time.Millisecond), claimed))
	}
	for _, c := range []struct {
		what       string
		rOld, fOld int
	}{{"2 documents then 2", 1, 1}, {"2 documents then 3", 1, 2}, {"3 documents then 2", 2, 1}} {
		rm, lm := find(docs(rAt, c.rOld, rAt.Add(-37*time.Second))), find(docs(fAt, c.fOld, fAt))
		in := iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
			[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))})
		oRefused(t, "a picked claimed_at beside a finer created_at, "+c.what, oWord(t, Decide(in), sec(rAt), sec(fAt)), RefusalPick, "mock-799")
	}
}

// A recording's half keeps where each of its fields stands in it
// (textHalf), so each time in it is placed as in the recording's whole
// text: by the field that holds it and the member at its path in that
// field, whether the call keeps the half or lets it go with its instant
// (past the reader's budget). In the answer, the body's top-level time, its
// items' times and a header's JSON time are three places, the items' one
// place.
func TestOutcomeAHalfPlacesATimeAsItsRecordingDoes(t *testing.T) {
	s := oAt.Add(96 * time.Second).UTC().Format(time.RFC3339)
	m := &models.Mock{Name: "mock-7", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://api/jobs", Body: `{"at":"` + s + `"}`},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Meta": `{"at":"` + s + `"}`},
			Body: `{"at":"` + s + `","items":[{"at":"` + s + `"},{"at":"` + s + `"}]}`},
		ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
	}}
	defer func(r int64) { readerBudget = r }(readerBudget)
	for _, c := range []struct {
		what   string
		budget int64
	}{{"within the reader's budget", 0}, {"past it", 1}} {
		readerBudget = c.budget
		x := newReaderIndex(nil)
		dc := decider{x: x}
		places := func(t *readerText) []instantPlace {
			spans, read := t.spansOf()
			text, fields, ok := dc.laidOut(t)
			if !read || !ok {
				return nil
			}
			return dc.placesIn(text, fields, spans).at
		}
		// Each half's places, and the whole text's places of each half's
		// fields, in order.
		var halves, wholes [2][]instantPlace
		crashed := func() (crashed any) {
			defer func() { crashed = recover() }()
			whole := x.textOf(RawOf(m, true))
			req, ans := dc.halves(0, evPlace{t: &placeText{rec: whole, live: whole}})
			halves = [2][]instantPlace{places(req.t.rec), places(ans.t.live)}
			for _, p := range places(whole) {
				field, _, _ := strings.Cut(p.field, "\x00")
				if requestPath(field) {
					wholes[0] = append(wholes[0], p)
				} else {
					wholes[1] = append(wholes[1], p)
				}
			}
			return nil
		}()
		if crashed != nil {
			t.Fatalf("%s: the question crashed (%v)", c.what, crashed)
		}
		if kept := len(dc.laid) > 0; kept == (c.budget == 1) || kept == (len(dc.laidNow) > 0) {
			t.Errorf("%s: the call kept %d halves' fields and holds %d for the instant; want them kept within the budget, held for the instant past it", c.what, len(dc.laid), len(dc.laidNow))
		}
		for i, what := range []string{"request", "answer"} {
			if len(halves[i]) == 0 || !slices.Equal(halves[i], wholes[i]) {
				t.Errorf("%s: the %s half placed its times at %#v; want %#v, as the recording's whole text places them", c.what, what, halves[i], wholes[i])
			}
		}
		if a := halves[1]; len(a) != 4 || slices.ContainsFunc(a, func(p instantPlace) bool { return p.field == "" }) ||
			a[0] == a[1] || a[1] != a[2] || a[3] == a[0] || a[3] == a[1] {
			t.Errorf("%s: the answer's times placed at %#v; want the body's top-level time, its items' times and the header's time three named places, the items' one", c.what, a)
		}
	}
}

// One call sends back n times one earlier answer listed (each document's
// updated_at, the shape of an optimistic lock or a bulk sync), each a
// changed instant that answer holds. The answer's halves are built once
// for the call and kept while the index's budget holds them; past the
// budget each instant builds them again and lets go of them once it is
// decided. Either way the halves the call holds at once never grow with
// the instants it decides: at most one place's two halves in each run.
// Each time is the answer's value sent back.
func TestOutcomeAnAnswersHalvesAreBuiltOnceAndHeldBoundedAcrossItsInstants(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	const n = 120
	var rDocs, fDocs, drift []string
	leaves, live := map[string]Leaf{}, map[string]Leaf{}
	for i := range n {
		r := rAt.Add(-50*time.Second + time.Duration(i)*7*time.Millisecond + 123*time.Microsecond).UTC().Format(time.RFC3339Nano)
		f := fAt.Add(-50*time.Second + time.Duration(i)*7*time.Millisecond + 456*time.Microsecond).UTC().Format(time.RFC3339Nano)
		rDocs = append(rDocs, fmt.Sprintf(`{"id":%d,"updated_at":"%s"}`, i, r))
		fDocs = append(fDocs, fmt.Sprintf(`{"id":%d,"updated_at":"%s"}`, i, f))
		p := fmt.Sprintf("body.docs[%d].updated_at", i)
		drift = append(drift, p)
		leaves[p], live[p] = Leaf{Path: p, Noise: p, Type: Instant, Value: r}, Leaf{Path: p, Noise: p, Type: Instant, Value: f}
	}
	mock := func(docs []string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.local/docs", Body: ``},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"docs":[` + strings.Join(docs, ",") + `]}`},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	rm, lm := RawOf(mock(rDocs), true), RawOf(mock(fDocs), true)
	// One place's two halves in both runs: each run's text once, a byte
	// more per field, and its fields.
	bound := int64(len(rm.Text())+len(lm.Text())) + 4*int64(len(rm.payload().fields)+len(lm.payload().fields))*(rawFieldBytes+64)
	for _, c := range []struct {
		what   string
		budget int64
		builds int
	}{{"within the budget", 0, 1}, {"past the budget", 1, n}} {
		readerBudget = c.budget
		in := Input{
			Drifts: oDrifts(), Drift: drift,
			Live: Shape{Leaves: live}, Rec: Shape{Leaves: leaves},
			At: rAt, Arrived: fAt, Raw: []Raw{rm.ServedAs(1).ServedLive(lm)},
			Window: iWin(rAt, fAt, 100*time.Millisecond), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
		}
		x := newReaderIndex(in.Drifts)
		dc := newDecider(in, false, x)
		ds, _ := dc.decide()
		x.release()
		values := 0
		for _, d := range ds {
			if d.Outcome == OutcomeValue {
				values++
			}
		}
		if values != n || len(ds) != n {
			t.Errorf("%s: %d decisions, %d of them the answer's value; want %d, each the answer's value: %s", c.what, len(ds), values, n, oDescribe(ds[:min(len(ds), 3)]))
		}
		if dc.halfBuilds != c.builds {
			t.Errorf("%s: the answer halved %d times for %d instants; want %d", c.what, dc.halfBuilds, n, c.builds)
		}
		if dc.halfPeak == 0 || dc.halfPeak > bound {
			t.Errorf("%s: the call held %d bytes of halves at once; want some, and at most %d, one place's halves in both runs", c.what, dc.halfPeak, bound)
		}
		if dc.laidNow != nil || dc.halfNow != 0 {
			t.Errorf("%s: the call ended holding %d halves (%d bytes) of its last instant; want none", c.what, len(dc.laidNow), dc.halfNow)
		}
	}
}

// A second the application picks from an answer that holds an escape
// beside round F's second (a quote or a backslash at a note's end, a
// newline, the "\/" a PHP encoder writes in a link), before or after it:
// reading the escape leaves the time as written, so the answer holds round
// F's second, and the pick stays refused, never learned as the call's own
// clock read.
func TestOutcomeAPickBesideAnEscapeElsewhereInItsAnswerStaysRefused(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	tsec := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	answer := func(body string) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.local/jobs/1", Body: ``},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: body},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	for _, note := range []string{`"x\""`, `"x\\"`, `"x\n"`, `"https:\/\/cdn.local\/a.png"`} {
		for _, sh := range []struct {
			what string
			body func(at string) string
		}{
			{"before", func(at string) string { return `{"note":` + note + `,"at":"` + at + `"}` }},
			{"after", func(at string) string { return `{"at":"` + at + `","note":` + note + `}` }},
		} {
			rm, lm := answer(sh.body(tsec(rAt.Add(-37*time.Second)))), answer(sh.body(tsec(fAt)))
			in := iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
				[]Raw{RawOf(rm, true).ServedAs(1).ServedLive(RawOf(lm, true))})
			oRefused(t, "a picked second with the note "+note+" "+sh.what+" it", oWord(t, Decide(in), sec(rAt), sec(fAt)), RefusalPick, "mock-799")
		}
	}
}

// A time an answer holds percent-encoded ("09%3A50%3A20Z", a link's query):
// as written its date alone reads as an instant of a day, which is no hold
// of the time it opens to. Where it opens to round F's second, the
// application picked that second from the answer: refused. Where it opens
// to another time of the call's day, the call's second is its own clock
// read. A hold finer than a millisecond as written stands, its escapes
// read or not.
func TestOutcomeAPercentEncodedSecondHoldsOnlyTheTimeItOpensTo(t *testing.T) {
	sec := func(x time.Time) string { return strconv.FormatInt(x.Unix(), 10) }
	pct := func(x time.Time) string { return strings.ReplaceAll(x.UTC().Format(time.RFC3339), ":", "%3A") }
	rAt, fAt := oAt.Add(20400*time.Millisecond), oAt.Add(96400*time.Millisecond)
	answer := func(at time.Time) *models.Mock {
		return &models.Mock{Name: "mock-799", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.local/jobs/1", Body: ``},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"next":"/v1/events?since=` + pct(at) + `"}`},
			ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + time.Millisecond),
		}}
	}
	call := func(r, f time.Time) Input {
		return iCall("body.cmd.query.dirtyAt.$lte.$numberLong", Number, sec(rAt), sec(fAt), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
			[]Raw{RawOf(answer(r), true).ServedAs(1).ServedLive(RawOf(answer(f), true))})
	}
	d := oWord(t, Decide(call(rAt.Add(-37*time.Second), fAt)), sec(rAt), sec(fAt))
	oRefused(t, "round F's second picked from an answer that holds it percent-encoded", d, RefusalPick, "mock-799")
	d = oWord(t, Decide(call(rAt.Add(-10*time.Minute), fAt.Add(-10*time.Minute))), sec(rAt), sec(fAt))
	iClock(t, "the call's second beside a percent-encoded time ten minutes before it", d)
	// A time the answer writes at a cut finer than a millisecond, its
	// zone percent-encoded after it ("09:00:23.654321%2B05:30", a link's
	// query): as written it holds the call's microsecond, and a hold that
	// fine stands, however its escapes open: the application copied its
	// digits.
	micro := func(x time.Time) string { return x.UTC().Format("2006-01-02T15:04:05.000000Z07:00") }
	rT, fT := rAt.Add(-37*time.Second+123456*time.Microsecond), fAt.Add(-37*time.Second+654321*time.Microsecond)
	zoned := func(x time.Time) *models.Mock {
		m := answer(x)
		m.Spec.HTTPResp.Body = `{"next":"/v1/events?since=` + x.UTC().Format("2006-01-02T15:04:05.000000") + `%2B05:30"}`
		return m
	}
	d = oWord(t, Decide(iCall("body.cmd.query.dirtyAt.$lte", Instant, micro(rT), micro(fT), rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil,
		[]Raw{RawOf(zoned(rT), true).ServedAs(1).ServedLive(RawOf(zoned(fT), true))})), micro(rT), micro(fT))
	iValue(t, "a microsecond the answer writes before a percent-encoded zone", d, "mock-799")
}
