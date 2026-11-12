package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// kMove is in with its one drifted leaf at from moved to path, and a clock
// rule keploy.yml declares at path where in declared one at from.
func kMove(in Input, from, path string) Input {
	for _, sh := range []*Shape{&in.Rec, &in.Live} {
		l := sh.Leaves[from]
		l.Path, l.Noise = path, path
		sh.Leaves = map[string]Leaf{path: l}
	}
	in.Drift = []string{path}
	if in.Source != nil {
		if s, ok := in.Source(from); ok {
			in.Source = func(f string) (Source, bool) { return s, f == path }
		}
	}
	return in
}

// kKeys are places whose value names what the call fetches or writes
// (keyPlace); kFields are places a time is carried at that are no key.
var (
	kKeys   = []string{"url.path.3", "url.path.0", "body._id", "body.cmd.filter._id", "body.docs.msgs.0._id", "body.cmd.filter._id.$in[0]", "body.cmd.filter._id[0]", "body.docs.msgs.0.u.$set._id"}
	kFields = []string{"body.docs.msgs.0.user_id", "body.cmd.filter.createdAt", "body.last_idle_at", "url.query.since"}
)

func TestKeyPlaceReadsTheLeafsPlaceAlone(t *testing.T) {
	for _, p := range kKeys {
		if !keyPlace(p) {
			t.Errorf("%s: no key; want a key", p)
		}
	}
	for _, p := range append(kFields, "body.idx", "body.ids[0]", "body.owner_id", "body.url.path.3", "body._idle") {
		if keyPlace(p) {
			t.Errorf("FALSE KEY: %s: a key; want none", p)
		}
	}
}

// A time at a key is an id: a typed instant, or a read learning refused as
// one the clock checks cannot place (another scope, a cached time, a read
// the call's own answer holds), is checked by its form under the time
// default at any other field, and keeps its refusal at a key, as under
// strict times; so does a moment of two reads of the clock. A clock rule
// keploy.yml declares at a key decides it there as at any other field.
func TestTheTimeDefaultKeepsATimeAtAKeyAsLearned(t *testing.T) {
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
		{"the call's own answer holds the read", echo, RefusalCannotCheck, true, true},
	} {
		for _, p := range kKeys {
			tStrictThenDefault(t, c.what+" (a text) at "+p, kMove(c.in, cPath, p), c.kind, false, "")
			tStrictThenDefault(t, c.what+" (typed) at "+p, kMove(tTyped(c.in), cPath, p), c.kind, false, "")
		}
		for _, p := range kFields {
			tStrictThenDefault(t, c.what+" (a text) at "+p, kMove(c.in, cPath, p), c.kind, c.keptText, tForm)
			tStrictThenDefault(t, c.what+" (typed) at "+p, kMove(tTyped(c.in), cPath, p), c.kind, c.keptTyp, typedForm)
		}
	}
	for _, p := range append(append([]string{}, kKeys...), kFields...) {
		tStrictThenDefault(t, "a clock rule's range holds neither run, declared at "+p, kMove(rule, cPath, p), RefusalNoClockRead, true, tForm)
		tStrictThenDefault(t, "a clock rule's range holds neither run, typed, declared at "+p, kMove(tTyped(rule), cPath, p), RefusalNoClockRead, true, typedForm)
	}
	for _, p := range kKeys {
		tStrictThenDefault(t, "two reads of the clock at "+p, kMove(tTwoReads(true), "body.since", p), RefusalTwoSources, false, "")
	}
	tStrictThenDefault(t, "two reads of the clock at a field", kMove(tTwoReads(true), "body.since", kFields[1]), RefusalTwoSources, true, "text 9999-99-99a99:99:99a")
}

// A read of the clock learning holds in its call's span at a key keeps its
// range under the time default, as under strict times: checked by its form
// alone, any id of that form would pass. At any other field, and where a
// clock rule declares the key, the default checks it by its form.
func TestAClockReadAtAKeyKeepsItsRange(t *testing.T) {
	plain := func(p string, declared, timeForm bool) Decision {
		in := kMove(cCall(cR, cF, nil, false), cPath, p)
		if declared {
			in.Source = cClock(p, cMicro)
		}
		in.TimeForm = timeForm
		ds := Decide(in)
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil {
			t.Fatalf("a read in the span at %s (declared %v, the time default %v): %s; want a read of the clock", p, declared, timeForm, oDescribe(ds))
		}
		return ds[0]
	}
	for _, p := range kKeys {
		for _, tf := range []bool{false, true} {
			if d := plain(p, false, tf); d.Clock.Form != "" {
				t.Errorf("FALSE GREEN: a read in the span at the key %s (the time default %v): checked by its form %q; want its range", p, tf, d.Clock.Form)
			}
		}
		if d := plain(p, true, true); d.Clock.Form != tForm {
			t.Errorf("a read in the span a clock rule declares at the key %s: form %q; want %q", p, d.Clock.Form, tForm)
		}
	}
	for _, p := range kFields {
		if d := plain(p, false, true); d.Clock.Form != tForm {
			t.Errorf("a read in the span at %s: form %q; want %q", p, d.Clock.Form, tForm)
		}
	}
}

// A time bound whole to a place learning read as the clock, or a typed one,
// keeps its bind at a key: Decide gives it no check by form there, nor does
// BoundTimeLearned (the HTTP learn step), so a replay whose call carries
// another value than the bound place refuses it. A clock rule declared at
// the key gives one, as at any other field.
func TestABoundTimeAtAKeyKeepsItsBind(t *testing.T) {
	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	rec, live := iMicro(rAt.Add(-600*time.Microsecond)), iMicro(fAt.Add(-610*time.Microsecond))
	call := func(p string, declared, typed bool) Decision {
		e := iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.createdAt", String, rec, live)
		e.Seen[0].Clock = !typed
		typ := String
		if typed {
			typ = Instant
		}
		in := iCall(p, typ, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), []Earlier{e}, nil)
		if declared {
			in.Source = cClock(p, cMicro)
		}
		in.TimeForm = true
		ds := Decide(in)
		if len(ds) != 1 {
			t.Fatalf("bound at %s (declared %v, typed %v): %s; want one decision", p, declared, typed, oDescribe(ds))
		}
		iValue(t, "bound at "+p, ds[0], "mock-1")
		return ds[0]
	}
	for _, p := range kKeys {
		for _, typed := range []bool{false, true} {
			if d := call(p, false, typed); d.ByForm != nil {
				t.Errorf("FALSE GREEN: bound at the key %s (typed %v): checked by form %+v; want its bind alone", p, typed, *d.ByForm)
			}
		}
		if d := call(p, true, false); d.ByForm == nil {
			t.Errorf("bound at the key %s a clock rule declares: no check by form; want one", p)
		}
	}
	for _, p := range kFields {
		for _, typed := range []bool{false, true} {
			if d := call(p, false, typed); d.ByForm == nil {
				t.Errorf("bound at %s (typed %v): no check by form; want one", p, typed)
			}
		}
	}

	at := time.Date(2026, 11, 11, 18, 35, 46, 0, time.UTC)
	w := iWin(at, at.Add(time.Minute), time.Second)
	sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	learned := func(p string, typ Type) Learned {
		v := func(t time.Time) string {
			if typ == Instant {
				return iMs(t)
			}
			return sec(t)
		}
		return Learned{Kind: KindVariable, Recorded: Leaf{Path: p, Type: typ, Value: v(at)}, Live: Leaf{Path: p, Type: typ, Value: v(at.Add(time.Minute))},
			From: Place{Whole: true, Clock: true}}
	}
	for _, typ := range []Type{Number, Instant} {
		for _, p := range kKeys {
			if got := BoundTimeLearned(learned(p, typ), false, w, true); got.ByForm != nil {
				t.Errorf("FALSE GREEN: learned bound at the key %s (%v): checked by form %+v; want its bind alone", p, typ, *got.ByForm)
			}
			if got := BoundTimeLearned(learned(p, typ), true, w, true); got.ByForm == nil {
				t.Errorf("learned bound at the key %s (%v) a clock rule declares: no check by form; want one", p, typ)
			}
		}
		for _, p := range kFields {
			if got := BoundTimeLearned(learned(p, typ), false, w, true); got.ByForm == nil {
				t.Errorf("learned bound at %s (%v): no check by form; want one", p, typ)
			}
		}
	}
}

// kUUID1 is a version 1 UUID of instant at, its clock sequence and node
// node (12 hex digits).
func kUUID1(at time.Time, node string) string {
	ts := uint64(at.UnixNano()/100) + 0x01B21DD213814000 // 100 ns since 1582-10-15
	hex := func(v uint64, n int) string {
		s := strconv.FormatUint(v, 16)
		return strings.Repeat("0", n-len(s)) + s
	}
	return hex(ts&0xffffffff, 8) + "-" + hex(ts>>32&0xffff, 4) + "-" + hex(ts>>48&0x0fff|0x1000, 4) + "-80b4-" + node
}

// A time-ordered UUID is an id, not a time: bound whole to a place learning
// read as the clock, its use gets no check by form (Decide's valueByForm,
// BoundTimeLearned), so a replay whose call carries another node's UUID of
// that version, or another moment's, misses the bind as under strict
// times, never passing by the form "text uuid1". A run that wrote a UUID
// at the place and the other a time gets none either.
func TestATimeOrderedUUIDGetsNoCheckByForm(t *testing.T) {
	const p = "body.itemId"
	at := time.Date(2026, 11, 12, 10, 0, 0, 123_456_700, time.UTC)
	f := at.Add(time.Minute)
	w := iWin(at, f, time.Second)
	r, l := kUUID1(at, "00c04fd430c8"), kUUID1(f, "00c04fd430c8")
	if lay := leafLayout(Leaf{Type: String, Value: r}); lay != "uuid1" {
		t.Fatalf("fixture: %s reads as %q; want a uuid1", r, lay)
	}
	other := kUUID1(f.Add(37*time.Hour), "deadbeef0001")
	learned := func(rv, lv string) Learned {
		return Learned{Kind: KindVariable, Recorded: Leaf{Path: p, Type: String, Value: rv}, Live: Leaf{Path: p, Type: String, Value: lv},
			From: Place{Mock: "mock-1", Side: Request, Path: "body.id", Var: "body.id", Live: lv, Whole: true, Own: true, Clock: true}}
	}
	for _, c := range []struct{ what, r, l string }{
		{"a UUIDv1 in both runs", r, l},
		{"a UUIDv1 as recorded, a time in round F", r, iMs(f)},
		{"a time as recorded, a UUIDv1 in round F", iMs(at), l},
		{"a UUIDv7 in both runs", uuid7(at, "a3c8b1e4f2d6c9a0b1"), uuid7(f, "a3c8b1e4f2d6c9a0b1")},
	} {
		got := BoundTimeLearned(learned(c.r, c.l), false, w, true)
		if got.ByForm != nil {
			t.Errorf("FALSE GREEN: %s bound to a read of the clock: checked by form %+v, another node's id %s passes: %v; want its bind alone",
				c.what, *got.ByForm, other, HoldsByForm(*got.ByForm, Leaf{Type: String, Value: other}, true))
		}
	}
	// The control: a time bound so is checked by its form.
	if got := BoundTimeLearned(learned(iMs(at), iMs(f)), false, w, true); got.ByForm == nil {
		t.Errorf("a time bound to a read of the clock: no check by form; want one")
	}

	rAt := oAt.Add(900 * time.Microsecond)
	fAt := oAt.Add(time.Hour + 950*time.Microsecond)
	rec, live := kUUID1(rAt.Add(-600*time.Microsecond), "00c04fd430c8"), kUUID1(fAt.Add(-610*time.Microsecond), "00c04fd430c8")
	e := iSent("mock-1", rAt.Add(-500*time.Microsecond), "body.id", String, rec, live)
	e.Seen[0].Clock = true
	in := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), []Earlier{e}, nil)
	in.TimeForm = true
	for _, d := range Decide(in) {
		if d.ByForm != nil {
			t.Errorf("FALSE GREEN: a UUIDv1 bound to a read of the clock (Decide): %s, checked by form %+v; want its bind alone", oDescribe([]Decision{d}), *d.ByForm)
		}
		if d.Outcome == OutcomeClock && d.Clock != nil && d.Clock.Form != "" {
			t.Errorf("FALSE GREEN: a UUIDv1 (Decide): a time checked by its form %q; want none", d.Clock.Form)
		}
	}
}
