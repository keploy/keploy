package learn

import (
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
const tForm = "text 9999-99-99a99:99:99a"

// tzForm is cF's form written with a zone offset where Z was.
const tzForm = "text 9999-99-99a99:99:99+99:99"

// tTwoForms reports whether o is checked by the two forms a and b, in the
// one order models.FormsOf gives them.
func tTwoForms(o *models.ClockOffset, a, b string) bool {
	form, formF := models.FormsOf(a, b)
	return o != nil && formF != "" && o.Form == form && o.FormF == formF
}

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
	const typedForm = "typed 9999-99-99a99:99:99a"
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
	tStrictThenDefault(t, "an offset in the call's window", iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), nil, nil), RefusalCannotCheck, true, "text 9999-99-99a99:99:99a")

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
	tStrictThenDefault(t, "an offset since the test began", in, RefusalCannotCheck, true, "text 9999-99-99a99:99:99a")
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
	tStrictThenDefault(t, "two reads of the clock", tTwoReads(true), RefusalTwoSources, true, "text 9999-99-99a99:99:99a")
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
	// was): checked by its two forms alone, never by the clock checks.
	tz := cFAt.Add(-50 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000-07:00")
	if ds := Decide(declare(cCall(cR, tz, []Raw{cAns(cR, tz)}, true))); len(ds) != 1 || !tTwoForms(ds[0].Clock, tForm, tzForm) || !strings.Contains(ds[0].Line, "checked by its form alone ("+tzForm+" or "+tForm+",") {
		t.Errorf("two forms: %s; want a time checked by both forms, its line naming them", oDescribe(ds))
	}
	if ds := Decide(cCall(cR, tz, []Raw{cAns(cR, tz)}, true)); len(ds) != 1 || ds[0].Outcome == OutcomeClock {
		t.Errorf("FALSE GREEN? two forms, strict times: %s; want refused", oDescribe(ds))
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
	w := &Window{Rec: Span{To: cRAt}, F: Span{From: cFAt.Add(-time.Second), To: cFAt}}
	if f, _ := timeForms(TimeForm(Leaf{Type: Number, Value: near}), TimeForm(Leaf{Type: Number, Value: nearF}), Leaf{Type: Number, Value: near}, Leaf{Type: Number, Value: nearF}, w); f != "" {
		t.Errorf("FALSE GREEN? an epoch number near a span with no start: read as near (%q)", f)
	}
	// Each run's epoch number is read against its own run's span: one near
	// the recorded span but in a round F span with no start is not near.
	wF := &Window{Rec: Span{From: cRAt.Add(-time.Second), To: cRAt}, F: Span{To: cFAt}}
	if f, _ := timeForms(TimeForm(Leaf{Type: Number, Value: near}), TimeForm(Leaf{Type: Number, Value: nearF}), Leaf{Type: Number, Value: near}, Leaf{Type: Number, Value: nearF}, wF); f != "" {
		t.Errorf("FALSE GREEN? round F's epoch number with no round F span: read as near (%q)", f)
	}
	if f, _ := timeForms(TimeForm(Leaf{Type: Number, Value: near}), TimeForm(Leaf{Type: Number, Value: near}), Leaf{Type: Number, Value: near}, Leaf{Type: Number, Value: near}, &Window{Rec: wF.Rec, F: wF.Rec}); f == "" {
		t.Errorf("precondition: an epoch number near both runs' spans has no form")
	}
	// Two runs two years apart: each epoch number is near its own run's
	// span and far from the other's.
	later := cRAt.AddDate(2, 0, 0)
	nearLater := iEpochMs(later.Add(-50 * time.Millisecond))
	w2 := &Window{Rec: Span{From: cRAt.Add(-time.Second), To: cRAt}, F: Span{From: later.Add(-time.Second), To: later}}
	if f, _ := timeForms(TimeForm(Leaf{Type: Number, Value: near}), TimeForm(Leaf{Type: Number, Value: nearLater}), Leaf{Type: Number, Value: near}, Leaf{Type: Number, Value: nearLater}, w2); f == "" {
		t.Errorf("runs two years apart, each epoch number near its own run's span: no form; want one")
	}
	// A run whose value has no form gives the time no form.
	if f, g := timeForms(tForm, "", Leaf{Type: String, Value: cR}, Leaf{Type: String, Value: "me"}, nil); f != "" || g != "" {
		t.Errorf("FALSE GREEN? round F's value of no form: forms %q, %q; want none", f, g)
	}
	if f, g := timeForms("", tForm, Leaf{Type: String, Value: "me"}, Leaf{Type: String, Value: cF}, nil); f != "" || g != "" {
		t.Errorf("FALSE GREEN? a recorded value of no form: forms %q, %q; want none", f, g)
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
		{"no fraction where one was (an all-zero fraction dropped)", s("2026-11-11T08:00:00.123Z"), s("2026-11-11T08:00:00Z"), true},
		{"a fraction where none was", s("2026-11-11T08:00:00Z"), s("2026-11-11T08:00:00.5Z"), true},
		{"a comma's fraction dropped", s("2026-11-11 08:00:00,123"), s("2026-11-11 08:00:00"), true},
		{"an epoch with its fraction dropped", n("1762848000.5"), n("1762848000"), true},
		{"a time of day with its fraction dropped", s("08:00:00.250"), s("08:00:00"), true},
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
	// A form leaves out a fraction of a second, and only that: an RFC 1123
	// date keeps its comma.
	for v, want := range map[string]string{
		"2026-11-11T08:00:00.123456Z":   "text 9999-99-99a99:99:99a",
		"2026-11-11 08:00:00,5":         "text 9999-99-99 99:99:99",
		"Wed, 11 Nov 2026 08:00:00 GMT": "text aaa, 99 aaa 9999 99:99:99 aaa",
	} {
		if got := TimeForm(s(v)); got != want {
			t.Errorf("the form of %q: %q; want %q", v, got, want)
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

// Under the time default a time whose two record runs wrote it in two forms
// (a zone offset in one where the other wrote Z) is checked by its forms
// alone: a replay passes a value written in either, with a fraction or
// without, never its range, and refuses any other form, naming both.
// keploy.yml's mock.times: strict refuses it, saying to record it again.
func TestATimeItsRunsWroteInTwoFormsPassesEither(t *testing.T) {
	form, formF := models.FormsOf(tForm, tzForm)
	o := models.ClockOffset{Form: form, FormF: formF}
	if form != tzForm || formF != tForm {
		t.Fatalf("precondition: forms %q, %q; want the lesser, %q, first", form, formF, tzForm)
	}
	for _, c := range []struct {
		what string
		live string
		pass bool
	}{
		{"the form written with Z", "2001-02-03T04:05:06.789Z", true},
		{"the form written with a zone offset", "2001-02-03T04:05:06.789+05:30", true},
		{"the form written with Z, its fraction dropped", "2001-02-03T04:05:06Z", true},
		{"the form written with a zone offset, its fraction dropped", "2001-02-03T04:05:06+05:30", true},
		{"a third form", "2001-02-03 04:05:06Z", false},
		{"no instant", "me", false},
	} {
		live := Leaf{Type: String, Value: c.live}
		ok, why := CheckClockReadForm("body.at", o, live, Span{}, false, true)
		if ok != c.pass {
			t.Errorf("%s: %q passed %v (%s); want %v", c.what, c.live, ok, why, c.pass)
		}
		if !ok && !strings.Contains(why, "learned as a time written as "+tzForm+" or "+tForm) {
			t.Errorf("%s: refusal %q; want it to name both forms", c.what, why)
		}
		if got := ReadsAsClockRead(o, live); got != c.pass {
			t.Errorf("%s: ReadsAsClockRead %v; want %v", c.what, got, c.pass)
		}
		if ok, why := CheckClockRead("body.at", o, live, Span{}, false); ok || !strings.Contains(why, "written as "+tzForm+" or "+tForm+", checked by its form alone: learned under the time default; re-record with mock.times: strict") {
			t.Errorf("%s, strict times: passed %v (%q); want refused, naming both forms and saying to record again", c.what, ok, why)
		}
	}
	if (models.ClockOffset{Form: tForm}).WrittenAs("") || o.WrittenAs("") {
		t.Errorf("FALSE GREEN? no form read as one a time is written as")
	}

	// Two learnings of one place meet with the forms both checked by, the
	// same in either order: of two forms and one of them, that one; of two
	// that share none, no one check; of a form and a range, the range.
	const epochForm, millisForm = "number 9999999999", "number 9999999999999"
	for _, c := range []struct {
		what        string
		p           models.ClockOffset
		form, formF string
	}{
		{"the same two forms", o, tzForm, tForm},
		{"the same two forms, written the other way round", models.ClockOffset{Form: tForm, FormF: tzForm}, tzForm, tForm},
		{"one of the two forms", models.ClockOffset{Form: tForm}, tForm, ""},
		{"the other of the two forms", models.ClockOffset{Form: tzForm}, tzForm, ""},
		{"one of the two forms beside another", models.ClockOffset{Form: epochForm, FormF: tForm}, tForm, ""},
		{"another form", models.ClockOffset{Form: epochForm}, models.FormApart, ""},
		{"two other forms", models.ClockOffset{Form: epochForm, FormF: millisForm}, models.FormApart, ""},
		{"two learnings apart", models.ClockOffset{Form: models.FormApart}, models.FormApart, ""},
		{"a range", models.ClockOffset{Res: time.Millisecond}, "", ""},
	} {
		if m := o.Meet(c.p); m.Form != c.form || m.FormF != c.formF || m.Empty() != (c.form == models.FormApart) {
			t.Errorf("%s: met as %+v; want form %q beside %q", c.what, m, c.form, c.formF)
		}
		if m, n := c.p.Meet(o), o.Meet(c.p); m != n {
			t.Errorf("%s, met the other way: %+v; want %+v, the same in either order", c.what, m, n)
		}
	}
	// However its learnings are grouped: two forms met with each of the two
	// alone hold no one form, as the two alone do.
	z, f := models.ClockOffset{Form: tzForm}, models.ClockOffset{Form: tForm}
	if l, r := o.Meet(z).Meet(f), o.Meet(z.Meet(f)); l.Form != models.FormApart || r != l {
		t.Errorf("grouped two ways: %+v and %+v; want both apart", l, r)
	}

	// Learning keeps both forms of a read of the clock in the span that round
	// F wrote with a zone offset, and one form of a decoder-typed time whose
	// round F value dropped its fraction (a cached time cut to the second,
	// which the clock checks refuse).
	read := cCall(cR, cFAt.Add(-50*time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000-07:00"), nil, false)
	read.TimeForm = true
	if ds := Decide(read); len(ds) != 1 || ds[0].Outcome != OutcomeClock || !tTwoForms(ds[0].Clock, tForm, tzForm) {
		t.Errorf("a read in the span round F wrote with a zone offset, the time default: %s; want a time checked by both forms", oDescribe(ds))
	}
	stale := cFAt.Add(-time.Minute).UTC().Format(time.RFC3339)
	in := tTyped(cCall(cR, stale, nil, false))
	tRefused(t, "a typed time whose round F value dropped its fraction, strict times", Decide(in), RefusalNoClockRead)
	in.TimeForm = true
	if ds := Decide(in); len(ds) != 1 || ds[0].Clock == nil || ds[0].Clock.Form != "typed 9999-99-99a99:99:99a" || ds[0].Clock.FormF != "" {
		t.Errorf("a typed time whose round F value dropped its fraction, the time default: %s; want it checked by its one form", oDescribe(ds))
	}
}
