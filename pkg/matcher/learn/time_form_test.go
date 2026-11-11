package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows of the time default (Input.TimeForm): a changed time keeps its test
// and passes at replay with any value of its type and form; the clock
// checks run only where TimeForm is off (keploy.yml's strict times), and
// every row of them elsewhere in this package runs so.

// tForm is the form of cR, cF: an RFC 3339 text at the microsecond in UTC.
const tForm = "text 9999-99-99a99:99:99.fa"

// tKept checks ds is one time checked by form alone.
func tKept(t *testing.T, what string, ds []Decision, form string) {
	t.Helper()
	if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil {
		t.Errorf("%s: %s; want one time checked by its form", what, oDescribe(ds))
		return
	}
	if c := *ds[0].Clock; c.Form != form {
		t.Errorf("%s: checked as %#v (%s); want the form %q", what, c, ds[0].Line, form)
	}
}

// tTyped is in with its leaf at cPath typed by its decoder (Instant).
func tTyped(in Input) Input {
	for _, sh := range []*Shape{&in.Rec, &in.Live} {
		l := sh.Leaves[cPath]
		l.Type = Instant
		sh.Leaves = map[string]Leaf{cPath: l}
	}
	return in
}

// tRefused checks ds is one refusal of kind.
func tRefused(t *testing.T, what string, ds []Decision, kind RefusalKind) {
	t.Helper()
	if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != kind {
		t.Errorf("%s: %s; want refused %s", what, oDescribe(ds), kind)
	}
}

// tStrictThenDefault checks in is refused kind under strict times, and
// under the default kept by form (kept) or refused kind still.
func tStrictThenDefault(t *testing.T, what string, in Input, kind RefusalKind, kept bool, form string) {
	t.Helper()
	tRefused(t, what+", strict times", Decide(in), kind)
	in.TimeForm = true
	ds := Decide(in)
	if !kept {
		tRefused(t, what+", the time default", ds, kind)
		return
	}
	tKept(t, what, ds, form)
	if len(ds) == 1 && (!strings.Contains(ds[0].Line, "checked by its form alone") || !strings.Contains(ds[0].Line, string(kind))) {
		t.Errorf("%s: line %q; want it to say the form is checked and name the clock checks' refusal", what, ds[0].Line)
	}
}

// A time field is a decoder-typed time, a field a clock rule declares, or a
// value learning read as the application's clock; on one the time default
// settles the refusals of which read or which scope a moment is. A text in
// a clock layout that is none of them (a string that looks like a date) is
// found by its shape alone: it stays as learned, word by word. A moment an
// earlier answer holds is a bound value and stays refused on every field.
func TestTheTimeDefaultKeepsATimeTheClockChecksRefuse(t *testing.T) {
	const typedForm = "typed 9999-99-99a99:99:99.fa"
	stale := iMicro(cFAt.Add(-time.Minute))
	echo := cCall(cR, cF, nil, false)
	echo.Resp = []Leaf{{Path: "body.createdAt", Noise: "body.createdAt", Type: String, Value: cR}}
	rule := cCall(cR, cF, []Raw{cAns(cR, cF)}, true)
	rule.Source = cClock(cPath, models.ClockOffset{Min: -3 * time.Second, Max: -time.Second, Res: time.Microsecond})
	for _, c := range []struct {
		what              string
		in                Input
		kind              RefusalKind
		keptText, keptTyp bool
	}{
		{"an answer outside the scope holds the moment", cCall(cR, cF, []Raw{cAns(cR, cF)}, true), RefusalAnotherScope, false, true},
		{"round F's value a cached time, a minute old", cCall(cR, stale, nil, false), RefusalNoClockRead, false, true},
		{"an answer served with no view stated", cCall(cR, cF, []Raw{oAnswer("mock-7", 100*time.Millisecond, cF).ServedAs(1)}, false), RefusalUnknown, false, false},
		{"an answer that gave round F's moment as recorded", cCall(cR, cF, []Raw{oAnswer("mock-7", 100*time.Millisecond, cF).ServedAs(1).ServedAsRecorded()}, false), RefusalPick, false, false},
		{"the call's own answer holds the read", echo, RefusalCannotCheck, true, true},
		{"a clock rule's range holds neither run", rule, RefusalNoClockRead, true, true},
	} {
		tStrictThenDefault(t, c.what+" (a text)", c.in, c.kind, c.keptText, tForm)
		tStrictThenDefault(t, c.what+" (typed)", tTyped(c.in), c.kind, c.keptTyp, typedForm)
	}

	// A read learning holds in the call's span keeps its range under strict
	// times and is checked by form alone under the default.
	in := cCall(cR, cF, nil, false)
	ds := Decide(in)
	if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].Clock.Form != "" {
		t.Fatalf("a read in the span, strict times: %s; want a read of the clock with no form", oDescribe(ds))
	}
	in.TimeForm = true
	ds = Decide(in)
	tKept(t, "a read in the span", ds, tForm)
	if len(ds) == 1 && ds[0].Clock != nil && ds[0].Clock.Res != time.Microsecond {
		t.Errorf("a read in the span: %+v; want its learned cut kept beside the form", *ds[0].Clock)
	}

	// A time that is one word of a text keeps where it stands: a replay
	// checks that word by its form and every other byte as recorded.
	word := cCall("at "+cR, "at "+stale, nil, false)
	word.Source, word.TimeForm = rule.Source, true
	ds = Decide(word)
	tKept(t, "a declared time inside a text", ds, tForm)
	if len(ds) == 1 && ds[0].Clock != nil {
		o := *ds[0].Clock
		if o.Word.IsZero() {
			t.Errorf("a declared time inside a text: %+v; want the word's place kept", o)
		}
		if ok, why := CheckClockReadForm(cPath, o, Leaf{Type: String, Value: "at " + iMicro(cFAt.Add(time.Hour))}, Span{}, false, true); !ok {
			t.Errorf("another instant in the same text: %s; want it passed", why)
		}
		if ok, _ := CheckClockReadForm(cPath, o, Leaf{Type: String, Value: "on " + cF}, Span{}, false, true); ok {
			t.Errorf("FALSE GREEN: the text around the time changed: passed; want refused")
		}
	}
}

// A read plus an offset this protocol's replay does not compare, in the
// call's window or since its test began: learning read it as the clock, so
// the time default checks a text of it by its form.
func TestTheTimeDefaultKeepsAReadWhoseOffsetIsNotCompared(t *testing.T) {
	const p = "body.filter.dirtyAt.$lte"
	rAt, fAt := oAt.Add(400*time.Millisecond), oAt.Add(2*time.Hour+150*time.Millisecond)
	rec, live := iMs(rAt.Add(-2*time.Second)), iMs(fAt.Add(-2*time.Second))
	off := models.ClockOffset{Min: -2010 * time.Millisecond, Max: -1990 * time.Millisecond, Res: time.Millisecond, Why: "the stub reading's line"}
	defer func(f func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool), g func(Learned) (Learned, bool)) {
		clockInWindow, clockSinceBegin = f, g
	}(clockInWindow, clockSinceBegin)
	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return off, "", true
	}
	tStrictThenDefault(t, "an offset in the call's window", iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), nil, nil), RefusalCannotCheck, true, "text 9999-99-99a99:99:99.fa")

	clockInWindow = func(string, typedInstant, typedInstant, *Window, *time.Location) (models.ClockOffset, string, bool) {
		return models.ClockOffset{}, "", false
	}
	clockSinceBegin = func(Learned) (Learned, bool) {
		o := off
		o.Since = models.SinceTestBegin
		return Learned{Kind: KindClockOffset, Offset: &o}, true
	}
	w := iWin(rAt, fAt, 30*time.Millisecond)
	w.Rec.Began, w.F.Began = rAt.Add(-5*time.Second), fAt.Add(-5*time.Second)
	in := iCall(p, String, rec, live, rAt, fAt, w, nil, nil)
	in.Drifts.clientTests = true
	tStrictThenDefault(t, "an offset since the test began", in, RefusalCannotCheck, true, "text 9999-99-99a99:99:99.fa")
}

// tTwoReads is a value the call's own test sent before it at two places in
// both runs, the later one its own call's read of the clock (learned a
// Clock there), and the earlier one too where both.
func tTwoReads(both bool) Input {
	const p = "body.since"
	rAt, fAt := oAt.Add(300*time.Millisecond), oAt.Add(time.Hour+320*time.Millisecond)
	rec, live := iMs(oAt.Add(-200*time.Millisecond)), iMs(oAt.Add(time.Hour-205*time.Millisecond))
	carried := iSent("mock-4", oAt.Add(-150*time.Millisecond), "body.createdAt", String, rec, live)
	reread := iSent("mock-7", oAt.Add(-120*time.Millisecond), "body.updatedAt", String, rec, live)
	carried.Seen[0].Clock, reread.Seen[0].Clock = both, true
	return iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 50*time.Millisecond), []Earlier{carried, reread}, nil)
}

// A moment sent at two places, each its own call's read of the clock, is a
// copy of a read: the default checks it by form. Beside a place that is no
// read it is a bound value of two sources and stays refused.
func TestTheTimeDefaultKeepsAMomentOfTwoClockReads(t *testing.T) {
	tStrictThenDefault(t, "two reads of the clock", tTwoReads(true), RefusalTwoSources, true, "text 9999-99-99a99:99:99.fa")
	tStrictThenDefault(t, "a carried value beside a read", tTwoReads(false), RefusalTwoSources, false, "")

	// A read of two reads, then a text no read explains, in one call: the
	// second is still no time.
	in := tTwoReads(true)
	rAt, fAt := in.At, in.Arrived
	rec, live := iMs(rAt.Add(-20*time.Millisecond)), iMs(fAt.Add(-time.Minute))
	for _, sh := range []struct {
		s *Shape
		v string
	}{{&in.Rec, rec}, {&in.Live, live}} {
		sh.s.Leaves["body.zz"] = Leaf{Path: "body.zz", Noise: "body.zz", Type: String, Value: sh.v}
	}
	in.Drift = []string{"body.since", "body.zz"}
	in.TimeForm = true
	ds := Decide(in)
	if len(ds) != 2 || ds[0].Outcome != OutcomeClock || ds[1].Refusal == nil || ds[1].Refusal.Kind != RefusalNoClockRead {
		t.Errorf("two reads, then a cached text: %s; want a time, then refused no-clock-read", oDescribe(ds))
	}
}

func TestTheTimeDefaultLeavesWhatIsNoTime(t *testing.T) {
	declare := func(in Input) Input {
		in.Source = cClock(cPath, models.ClockOffset{Min: -3 * time.Second, Max: -time.Second, Res: time.Microsecond})
		in.TimeForm = true
		return in
	}
	// Two runs that write the value in two forms (a zone offset where Z
	// was): no one form, so the clock checks stay.
	tz := cFAt.Add(-50 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000-07:00")
	if ds := Decide(declare(cCall(cR, tz, []Raw{cAns(cR, tz)}, true))); len(ds) != 1 || ds[0].Outcome == OutcomeClock {
		t.Errorf("FALSE GREEN? two forms: %s; want refused", oDescribe(ds))
	}
	// A time inside a text whose words around it changed too: a replay
	// reads no clock there.
	if ds := Decide(declare(cCall("x "+cR, "y "+cF, nil, false))); len(ds) != 2 || ds[1].Refusal == nil || ds[1].Refusal.Kind != RefusalCannotCheck {
		t.Errorf("FALSE GREEN? a time in a text that changed around it: %s; want its word refused cannot-check", oDescribe(ds))
	}
	win := iWin(cRAt, cFAt, 60*time.Millisecond)
	near, nearF := iEpochMs(cRAt.Add(-50*time.Millisecond)), iEpochMs(cFAt.Add(-50*time.Millisecond))
	num := func(r, f string, w *Window) Input {
		return declare(iCall(cPath, Number, r, f, cRAt, cFAt, w, nil, nil))
	}
	// An epoch number found by its shape alone is no time.
	plain := iCall(cPath, Number, near, iEpochMs(cFAt.Add(-time.Minute)), cRAt, cFAt, win, nil, nil)
	plain.TimeForm = true
	if ds := Decide(plain); len(ds) != 1 || ds[0].Outcome == OutcomeClock {
		t.Errorf("FALSE GREEN? an epoch number no rule declares and no read explains: %s; want refused", oDescribe(ds))
	}
	// A declared epoch number is checked by its form where its moment lies
	// within a year of its call in both runs: ten days stale, or an hour
	// ahead (an expiry). Not a 13-digit number far from its call in either
	// run (an id), nor one with no span of its call.
	for _, c := range []struct {
		what string
		r, f string
		w    *Window
		kept bool
	}{
		{"ten days stale in round F", near, iEpochMs(cFAt.Add(-10 * 24 * time.Hour)), win, true},
		{"an hour ahead in round F", near, iEpochMs(cFAt.Add(time.Hour)), win, true},
		{"ten days stale as recorded", iEpochMs(cRAt.Add(-10 * 24 * time.Hour)), nearF, win, true},
		{"far in round F alone", near, iEpochMs(cFAt.Add(-2 * epochNear)), win, false},
		{"far as recorded alone", iEpochMs(cRAt.Add(-2 * epochNear)), nearF, win, false},
		{"far ahead in round F alone", near, iEpochMs(cFAt.Add(2 * epochNear)), win, false},
		{"a 13-digit id", "1234567890123", "1234567890999", win, false},
		{"no span of its call", near, nearF, nil, false},
		{"a counter", "42", "43", win, false},
	} {
		ds := Decide(num(c.r, c.f, c.w))
		switch {
		case c.kept:
			tKept(t, "a declared epoch number "+c.what, ds, "number 9999999999999")
		case len(ds) != 1 || ds[0].Outcome == OutcomeClock:
			t.Errorf("FALSE GREEN? a declared epoch number %s: %s; want refused", c.what, oDescribe(ds))
		}
	}
	// A declared word the reader reads no moment in (a number it matches by
	// its text alone: an epoch with a fraction ends inside it, so no instant
	// stands at its bytes) keeps its refusal, though the word alone reads as
	// an instant of one form in both runs.
	glued := declare(cCall("1731300000."+near, "1731300000."+nearF, nil, false))
	if ds := Decide(glued); len(ds) != 1 || ds[0].Outcome == OutcomeClock || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalNoClockRead {
		t.Errorf("FALSE GREEN? a declared word with no moment read in it: %s; want refused %s", oDescribe(ds), RefusalNoClockRead)
	}
	// A declared text keeps its form however far from its call: a text in a
	// clock layout names its moment, as no 13-digit id does.
	far := iMicro(cFAt.Add(-2 * epochNear))
	tKept(t, "a declared text two years stale", Decide(declare(cCall(cR, far, nil, false))), tForm)
	// A minted rule names no clock: its field is no time by it.
	minted := cCall(cR, iMicro(cFAt.Add(-time.Minute)), nil, false)
	minted.Source = func(f string) (Source, bool) { return Source{Kind: SourceMinted, Rule: "an-id"}, f == cPath }
	minted.TimeForm = true
	if ds := Decide(minted); len(ds) != 1 || ds[0].Outcome == OutcomeClock {
		t.Errorf("FALSE GREEN? a field a minted rule names: %s; want no time", oDescribe(ds))
	}
	// A value keploy reads as no instant has no form.
	for _, l := range []Leaf{{Type: String, Value: "me"}, {Type: Number, Value: "42"}, {Type: String, Value: "0x1f"}} {
		if f := TimeForm(l); f != "" {
			t.Errorf("FALSE GREEN? %+v, no instant, has the form %q; want none", l, f)
		}
	}
	// A value an answer of the scope holds stays bound to it.
	val := cCall(cR, cF, []Raw{cAns(cR, cF)}, false)
	val.TimeForm = true
	if ds := Decide(val); len(ds) != 1 || ds[0].Outcome != OutcomeValue {
		t.Errorf("a value an answer of the scope holds: %s; want a Value still", oDescribe(ds))
	}
	// keploy reads an epoch number's moment only where it knows a span of
	// both runs: a span with no start is none.
	dc := &decider{in: Input{Window: &Window{Rec: Span{To: cRAt}, F: Span{From: cFAt.Add(-time.Second), To: cFAt}}}}
	if dc.nearCall(Leaf{Type: Number, Value: near}, Leaf{Type: Number, Value: nearF}) {
		t.Errorf("FALSE GREEN? an epoch number near a span with no start: read as near")
	}
}

func TestATimeFormPassesAnyInstantOfItsForm(t *testing.T) {
	s := func(v string) Leaf { return Leaf{Type: String, Value: v} }
	n := func(v string) Leaf { return Leaf{Type: Number, Value: v} }
	for _, c := range []struct {
		what      string
		rec, live Leaf
		pass      bool
	}{
		{"another instant, years apart", s("2026-11-11T08:00:00.123456Z"), s("2011-01-02T03:04:05.654321Z"), true},
		{"a fraction of fewer digits (trailing zeros dropped)", s("2026-11-11T08:00:00.123456Z"), s("2026-11-11T08:00:00.12Z"), true},
		{"an epoch in seconds", n("1762848000"), n("1762848999"), true},
		{"no fraction where one was", s("2026-11-11T08:00:00.123Z"), s("2026-11-11T08:00:00Z"), false},
		{"a zone offset where Z was", s("2026-11-11T08:00:00Z"), s("2026-11-11T13:30:00+05:30"), false},
		{"a space where T was", s("2026-11-11T08:00:00Z"), s("2026-11-11 08:00:00Z"), false},
		{"seconds where milliseconds were", n("1762848000000"), n("1762848000"), false},
		{"a text where a number was", n("1762848000000"), s("1762848000000"), false},
		{"a text where a typed instant was", Leaf{Type: Instant, Value: "2026-11-11T08:00:00.000Z"}, s("2026-11-11T08:00:00.000Z"), false},
		{"no instant", s("2026-11-11T08:00:00Z"), s("me"), false},
		{"another time-ordered UUID", s("c232ab00-9414-11ec-b3c8-9f6bdeced846"), s("5f3e2a10-1b2c-11ef-8a1b-0242ac120002"), true},
		{"a random UUID where a time-ordered one was", s("c232ab00-9414-11ec-b3c8-9f6bdeced846"), s("5f3e2a10-1b2c-41ef-8a1b-0242ac120002"), false},
	} {
		o := models.ClockOffset{Form: TimeForm(c.rec)}
		if o.Form == "" {
			t.Fatalf("%s: %+v reads as no time", c.what, c.rec)
		}
		ok, why := CheckClockReadForm("body.at", o, c.live, Span{}, false, true)
		if ok != c.pass {
			t.Errorf("%s: %+v against %q passed %v (%s); want %v", c.what, c.live, o.Form, ok, why, c.pass)
		}
		if !ok && (!strings.Contains(why, "learned as a time written as "+o.Form) || !strings.Contains(why, "it changed")) {
			t.Errorf("%s: refusal %q; want it to name the learned form and say the value changed", c.what, why)
		}
		if got := ReadsAsClockRead(o, c.live); got != c.pass {
			t.Errorf("%s: ReadsAsClockRead %v; want %v", c.what, got, c.pass)
		}
	}
	// A typed time keeps its form whatever its instant.
	ty := Leaf{Type: Instant, Value: "2026-11-11T08:00:00.000Z"}
	if ok, why := CheckClockReadForm("body.at", models.ClockOffset{Form: TimeForm(ty)}, Leaf{Type: Instant, Value: "2001-02-03T04:05:06.789Z"}, Span{}, false, true); !ok {
		t.Errorf("a typed time of another instant: %s; want it passed", why)
	}
}

func TestTimeFormsMeet(t *testing.T) {
	a, b := models.ClockOffset{Form: tForm}, models.ClockOffset{Form: "number 9999999999"}
	if m := a.Meet(a); m.Form != tForm || m.Empty() {
		t.Errorf("one form met with itself: %+v; want it kept", m)
	}
	if m := a.Meet(b); m.Form != models.FormApart || !m.Empty() {
		t.Errorf("two forms met: %+v; want apart, refused", m)
	}
	r := models.ClockOffset{Res: time.Millisecond}
	if m := a.Meet(r); m.Form != "" {
		t.Errorf("a form met with a range: %+v; want the range's checks", m)
	}
	if m := r.Meet(a); m.Form != "" {
		t.Errorf("a range met with a form: %+v; want the range's checks", m)
	}
}

// Under keploy.yml's mock.times: strict a replay sets a learned form aside
// (CheckClockReadForm with the default off; CheckClockRead always): a read
// learned with its range is checked by that range again, so a value of the
// form outside it is refused, and a time learned under the default by its
// form alone, with no range, is refused, saying to record it again under
// strict. With the default on, both pass any value of their form.
func TestStrictTimesSetALearnedFormAside(t *testing.T) {
	in := cCall(cR, cF, nil, false)
	in.TimeForm = true
	ds := Decide(in)
	if len(ds) != 1 || ds[0].Clock == nil || ds[0].Clock.Form == "" || ds[0].Clock.Res <= 0 || in.Window == nil {
		t.Fatalf("a read in the span under the default: %s; want a read with its range and a form", oDescribe(ds))
	}
	read, span := *ds[0].Clock, in.Window.F
	held, later := in.Live.Leaves[cPath], Leaf{Type: String, Value: iMicro(cFAt.Add(time.Hour))}
	formOnly := models.ClockOffset{Form: read.Form}
	const reRecord = "learned under the time default; re-record with mock.times: strict to check its range"
	for _, c := range []struct {
		what     string
		o        models.ClockOffset
		live     Leaf
		timeForm bool
		pass     bool
		says     string
	}{
		{"a read in its range, strict", read, held, false, true, ""},
		{"a read of the form an hour out of its range, strict", read, later, false, false, "a read of the application's clock"},
		{"a read of the form an hour out of its range, the default", read, later, true, true, ""},
		{"a time learned by its form alone, strict", formOnly, held, false, false, reRecord},
		{"a time learned by its form alone, the default", formOnly, later, true, true, ""},
	} {
		ok, why := CheckClockReadForm(cPath, c.o, c.live, span, true, c.timeForm)
		if ok != c.pass || !strings.Contains(why, c.says) {
			t.Errorf("%s: passed %v (%q); want %v saying %q", c.what, ok, why, c.pass, c.says)
		}
		if c.timeForm {
			continue
		}
		if ok2, why2 := CheckClockRead(cPath, c.o, c.live, span, true); ok2 != ok || why2 != why {
			t.Errorf("%s: CheckClockRead %v (%q); want CheckClockReadForm's with the default off", c.what, ok2, why2)
		}
	}
}

// A Value (a time learning bound to an earlier place that holds it) keeps
// its bind; beside it Decide gives the time default's check by its form
// (Decision.ByForm), which an adapter takes where it cannot hold the bind.
// Only a time gets one: a field a clock rule declares, a decoder-typed time,
// or a value bound to a place learning read as a plain read of the clock.
// A value of the same shape that is none of them, and any value under
// strict times, gets none.
func TestAValueThatIsATimeCarriesItsCheckByForm(t *testing.T) {
	const p = "body.since"
	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	rec, live := iMicro(rAt.Add(-600*time.Microsecond)), iMicro(fAt.Add(-610*time.Microsecond))
	sent := func(clock bool) []Earlier {
		e := iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.createdAt", String, rec, live)
		e.Seen[0].Clock = clock
		return []Earlier{e}
	}
	call := func(clock, declared, typed, timeForm bool) Input {
		in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), sent(clock), nil)
		if declared {
			in.Source = cClock(p, cMicro)
		}
		if typed {
			for _, sh := range []*Shape{&in.Rec, &in.Live} {
				l := sh.Leaves[p]
				l.Type = Instant
				sh.Leaves = map[string]Leaf{p: l}
			}
		}
		in.TimeForm = timeForm
		return in
	}
	for _, c := range []struct {
		what                                    string
		clock, declared, typed, tform, twoForms bool
		form                                    string
	}{
		{what: "bound to a read of the clock", clock: true, tform: true, form: "text 9999-99-99a99:99:99.fa"},
		{what: "a field a clock rule declares", declared: true, tform: true, form: "text 9999-99-99a99:99:99.fa"},
		{what: "a decoder-typed time", typed: true, tform: true},
		{what: "a value that is no time", tform: true},
		{what: "bound to a read of the clock, two forms", clock: true, tform: true, twoForms: true},
		{what: "bound to a read of the clock, strict times", clock: true},
		{what: "a field a clock rule declares, strict times", declared: true},
	} {
		in := call(c.clock, c.declared, c.typed, c.tform)
		if c.twoForms {
			// Round F wrote the moment at the millisecond, the earlier send
			// with it: a copy, of no one form in both runs.
			liveMs := iMs(fAt.Add(-610 * time.Microsecond))
			in.Live.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: liveMs}
			in.Earlier[0].Seen[0].Live = liveMs
		}
		var d Decision
		if c.typed {
			ds := Decide(in)
			if len(ds) != 1 {
				t.Fatalf("%s: %s; want one decision", c.what, oDescribe(ds))
			}
			d = ds[0]
		} else {
			ds := Decide(in)
			if c.twoForms {
				if len(ds) != 1 || ds[0].ByForm != nil {
					t.Errorf("FALSE GREEN? %s: %s; want no check by form", c.what, oDescribe(ds))
				}
				continue
			}
			d = oWord(t, ds, rec, live)
		}
		if c.typed {
			// A typed instant Decide reads by its decoder: its form is the
			// typed one, whatever the reader made of its text.
			if d.Outcome == OutcomeValue && (d.ByForm == nil || !strings.HasPrefix(d.ByForm.Form, "typed ")) {
				t.Errorf("%s: %s, by form %+v; want a typed form beside the Value", c.what, oDescribe([]Decision{d}), d.ByForm)
			}
			continue
		}
		iValue(t, c.what, d, "mock-1")
		switch {
		case c.form == "" && d.ByForm != nil:
			t.Errorf("FALSE GREEN? %s: checked by form %+v; want none", c.what, *d.ByForm)
		case c.form != "" && (d.ByForm == nil || d.ByForm.Form != c.form || !d.ByForm.Plain() || !strings.Contains(d.ByForm.Why, "checked by its form alone")):
			t.Errorf("%s: by form %+v; want %q checked by its form alone", c.what, d.ByForm, c.form)
		}
	}
}

// An id whose bytes begin with a moment (a Mongo ObjectId: its first four
// bytes are the second it was made, a hex text; a filter's _id, a role
// binding's key) is no time the time default checks by its form, even
// bound to an earlier place learning read as a read of the clock: Decide
// keeps it a Value with no check by form, so an adapter that cannot hold
// its bind refuses it.
func TestAnIdThatBeginsWithAMomentGetsNoCheckByForm(t *testing.T) {
	const p = "body.filter._id"
	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	oid := func(at time.Time, rest string) string { return strconv.FormatInt(at.Unix(), 16) + rest }
	rec, live := oid(rAt.Add(-time.Second), "32e505659b477299"), oid(fAt.Add(-time.Second), "32e505659b4772a3")
	for _, clock := range []bool{true, false} {
		e := iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.documents.0._id", String, rec, live)
		e.Seen[0].Clock = clock
		in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), []Earlier{e}, nil)
		in.TimeForm = true
		d := oWord(t, Decide(in), rec, live)
		iValue(t, "an ObjectId", d, "mock-1")
		if d.ByForm != nil {
			t.Errorf("FALSE GREEN? an ObjectId bound to a place learned as a read of the clock (%v): checked by form %+v; want none", clock, *d.ByForm)
		}
	}
}

// A Value that is one word of a longer text ("since <time> ok") carries the
// word in its check by form, as a Clock in a text does (Word: its bytes and
// the hashes of the text on each side), so a replay checks that word alone
// by its form and the rest of the text as it is. Where the text around it
// changed too, the sides a replay compares are not the same in both runs:
// the Value keeps its bind and gets no check by form.
func TestAValueThatIsAWordOfATextCarriesItsWordInItsCheckByForm(t *testing.T) {
	const p = "body.note"
	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	rec, live := iMicro(rAt.Add(-600*time.Microsecond)), iMicro(fAt.Add(-610*time.Microsecond))
	for _, c := range []struct {
		what              string
		recText, liveText string
		word              bool
	}{
		{"a word of a text whose rest is the same", "since " + rec + " ok", "since " + live + " ok", true},
		{"a word of a text that changed around it", "since " + rec + " n=1", "since " + live + " n=2", false},
	} {
		e := iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.createdAt", String, rec, live)
		e.Seen[0].Clock = true
		in := iCall(p, String, c.recText, c.liveText, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), []Earlier{e}, nil)
		in.TimeForm = true
		d := oWord(t, Decide(in), rec, live)
		iValue(t, c.what, d, "mock-1")
		switch {
		case !c.word && d.ByForm != nil:
			t.Errorf("FALSE GREEN? %s: checked by form %+v; want none", c.what, *d.ByForm)
		case c.word && d.ByForm == nil:
			t.Errorf("%s: no check by form; want one at its word", c.what)
		case c.word:
			start := len("since ")
			if want := models.ClockWordOf(c.recText, start, start+len(rec)); d.ByForm.Word != want {
				t.Errorf("%s: word %+v; want %+v", c.what, d.ByForm.Word, want)
			}
		}
	}
}

// BoundTimeLearned gives a variable bound whole the time default's check by
// its form where its field is a time and both runs write it in one form, an
// epoch number near its call in both. An echo, a variable bound to a part
// of a value, a value that is no time, two forms, an epoch far from its
// call, no window, and strict times get none.
func TestABoundTimeLearnedIsCheckedByFormOnlyWhereItIsATime(t *testing.T) {
	rAt := time.Date(2026, 11, 11, 18, 35, 46, 0, time.UTC)
	fAt := rAt.Add(time.Minute)
	w := iWin(rAt, fAt, time.Second)
	sec := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	leaf := func(v string, typ Type) Leaf { return Leaf{Path: "body.data.createdAt", Value: v, Type: typ} }
	for _, c := range []struct {
		what          string
		ln            Learned
		declared, off bool
		w             *Window
		form          string
	}{
		{what: "bound to a read of the clock", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Clock: true}}, w: w, form: "number 9999999999"},
		{what: "declared", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true}}, declared: true, w: w, form: "number 9999999999"},
		{what: "no time", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true}}, w: w},
		{what: "strict times", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Clock: true}}, off: true, w: w},
		{what: "an echo", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Echo: true, Clock: true}}, w: w},
		{what: "a part of a value", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Clock: true}}, w: w},
		{what: "noise", ln: Learned{Kind: KindNoise, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Clock: true}}, w: w},
		{what: "two forms", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(iEpochMs(fAt), Number), From: Place{Whole: true, Clock: true}}, w: w},
		{what: "far as recorded", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt.Add(-2*epochNear)), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Clock: true}}, w: w},
		{what: "a typed time, untyped in round F", ln: Learned{Kind: KindVariable, Recorded: leaf(iMs(rAt), Instant), Live: leaf(iMs(fAt), String), From: Place{Whole: true}}, w: w},
		{what: "far from its call", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt.Add(2*epochNear)), Number), From: Place{Whole: true, Clock: true}}, w: w},
		{what: "no window", ln: Learned{Kind: KindVariable, Recorded: leaf(sec(rAt), Number), Live: leaf(sec(fAt), Number), From: Place{Whole: true, Clock: true}}},
		{what: "a counter", ln: Learned{Kind: KindVariable, Recorded: leaf("42", Number), Live: leaf("43", Number), From: Place{Whole: true, Clock: true}}, w: w},
		{what: "a typed time", ln: Learned{Kind: KindVariable, Recorded: leaf(iMs(rAt), Instant), Live: leaf(iMs(fAt), Instant), From: Place{Whole: true}}, w: w, form: "typed 9999-99-99a99:99:99.fa"},
	} {
		got := BoundTimeLearned(c.ln, c.declared, c.w, !c.off)
		switch {
		case c.form == "" && got.ByForm != nil:
			t.Errorf("FALSE GREEN? %s: checked by form %+v; want none", c.what, *got.ByForm)
		case c.form != "" && (got.ByForm == nil || got.ByForm.Form != c.form || !strings.Contains(got.ByForm.Why, "checked by its form alone")):
			t.Errorf("%s: by form %+v; want %q", c.what, got.ByForm, c.form)
		}
		if got.Kind != c.ln.Kind {
			t.Errorf("%s: kind %v; want %v kept", c.what, got.Kind, c.ln.Kind)
		}
	}
}
