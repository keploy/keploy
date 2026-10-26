package learn

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// m10 of the api-server suite: a usage query's createdAt.$lte, the app's now
// in nanoseconds, moved by record's full round's lead, is learned as noise
// that is a plain read of the clock (Learned.Read, Clock), so a caller
// persists it to MockSpec.Clocks and its replay checks it (CheckClockRead):
// the replay's own now, in its call's span, passes; now less thirty days
// (m10) and the epoch (m10b) are refused, saying the value changed; a span
// keploy cannot tell refuses even the right value, saying so. The typed
// $date of the same read (an instant its protocol types) is learned and
// checked alike. A value that is not one instant whole (an id with the read
// in it) and a wall clock that names no zone stay plain noise (Read nil).
func TestALearnedClockReadIsCheckedAtReplayInItsCallsSpan(t *testing.T) {
	rec := time.Date(2026, 10, 26, 9, 0, 0, 123456789, time.UTC)
	live := rec.Add(FullRoundLead + 137*time.Microsecond + 11)
	ns := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	const path = "body.cmd.pipeline.0.$match.createdAt.$lte.$numberLong"
	text := func(at time.Time) Leaf {
		return Leaf{Path: path, Noise: "body.cmd.pipeline.0.$match.createdAt.$lte", Type: String, Value: ns(at)}
	}

	classify := func(r, l Leaf) Learned {
		p := r.Path
		return ClassifyIn(NewDriftCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: l}}, Shape{Leaves: map[string]Leaf{p: r}}, rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	}
	ln := classify(text(rec), text(live))
	o, ok := ln.Clock()
	if ln.Kind != KindNoise || !ok || !o.Plain() || o.Res != time.Nanosecond {
		t.Fatalf("the nanosecond read: %s Read=%+v Clock=%+v,%v (%s); want noise that is a plain read at 1ns", ln.Kind, ln.Read, o, ok, ln.Why)
	}

	from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := Span{From: from, To: from.Add(40 * time.Millisecond)}
	now := from.Add(17*time.Millisecond + 3)
	for _, row := range []struct {
		what  string
		at    string
		known bool
		want  bool
		why   string
	}{
		{"the replay's now", ns(now), true, true, ""},
		{"m10: now less thirty days", ns(now.Add(-30 * 24 * time.Hour)), true, false, "it changed"},
		{"m10b: the epoch", "0", true, false, "no instant keploy reads"},
		{"the replay's now, no clock told", ns(now), false, false, "keploy cannot tell the application's clock"},
	} {
		got, why := CheckClockRead("createdAt.$lte", o, Leaf{Path: path, Type: String, Value: row.at}, s, row.known)
		if got != row.want || !strings.Contains(why, row.why) || !row.want && !strings.Contains(why, "createdAt.$lte was learned as clock") {
			t.Errorf("%s: %v %q; want %v naming %q", row.what, got, why, row.want, row.why)
		}
	}

	typed := func(at time.Time) Leaf {
		return InstantLeaf("body.createdAt.$date.$numberLong", "body.createdAt", at.Truncate(time.Millisecond), time.Millisecond)
	}
	ln = classify(typed(rec), typed(live))
	if o, ok := ln.Clock(); ln.Kind != KindNoise || !ok || !o.Plain() || o.Res < time.Millisecond {
		t.Fatalf("the typed read: %s %+v,%v (%s); want noise that is a plain read", ln.Kind, o, ok, ln.Why)
	} else if !ClockReadHolds(o, typed(now), s) || ClockReadHolds(o, typed(now.Add(-30*24*time.Hour)), s) || ClockReadHolds(o, typed(s.To.Add(time.Millisecond)), s) {
		t.Errorf("the typed read: want the replay's now held, thirty days before and a millisecond past the span not")
	}

	id := func(at time.Time) Leaf { return Leaf{Path: "body.id", Type: String, Value: "ord-" + ns(at)} }
	if ln := classify(id(rec), id(live)); ln.Kind == KindNoise && ln.Read != nil {
		t.Errorf("a read inside a longer value: Read=%+v; want plain noise", ln.Read)
	}
	wall := func(at time.Time) Leaf {
		return WallClockLeaf("body.at", "body.at", at.Truncate(time.Millisecond), time.Millisecond)
	}
	if ln := classify(wall(rec), wall(live)); ln.Read != nil {
		t.Errorf("a wall clock of no zone: Read=%+v; want none", ln.Read)
	}
}

// A place two learnings met as Empty (a plain read and one plus thirty
// minutes) is refused at replay whatever the call carries, even the value
// the replay's own clock wrote, saying the learnings disagree, whether or
// not keploy can tell the application's clock.
func TestAnEmptyClockReadIsRefusedAtReplaySayingTheLearningsDisagree(t *testing.T) {
	ms := time.Millisecond
	e := models.ClockOffset{Res: ms}.Meet(models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + ms, Res: ms})
	from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	now := InstantLeaf("body.at.$date.$numberLong", "body.at", from.Add(5*ms), ms)
	for _, known := range []bool{true, false} {
		ok, why := CheckClockRead("at", e, now, Span{From: from, To: from.Add(10 * ms)}, known)
		if ok || !strings.Contains(why, "at was learned at more than one call as clock reads no one offset holds") || !strings.Contains(why, "no one read of the application's clock explains them all") {
			t.Errorf("known=%v: %v %q; want refused, saying the learnings disagree", known, ok, why)
		}
	}
}

// A value the application cuts coarser than its encoding (a millisecond
// value written in nanoseconds) is learned with that cut as its Res, and a
// replay refuses it whatever it is, saying so: checked at the nanosecond, it
// would pass or fail by where its read fell in its millisecond.
func TestACutClockReadIsRefusedAtReplayNamingTheCut(t *testing.T) {
	rec := time.Date(2026, 10, 26, 9, 0, 0, 123000000, time.UTC)
	live := rec.Add(FullRoundLead + 7*time.Millisecond)
	if r := textClockRead(strconv.FormatInt(rec.UnixNano(), 10), strconv.FormatInt(live.UnixNano(), 10)); r == nil || r.Res < time.Millisecond {
		t.Fatalf("textClockRead = %+v; want a Res of the millisecond cut", r)
	} else {
		from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
		ok, why := CheckClockRead("createdAt", *r, Leaf{Type: String, Value: strconv.FormatInt(from.UnixNano(), 10)}, Span{From: from, To: from.Add(time.Second)}, true)
		if ok || !strings.Contains(why, "the application cuts its value") {
			t.Errorf("a cut read: %v %q; want refused naming the cut", ok, why)
		}
	}
	// The same for an instant its protocol types: a $date of milliseconds
	// whose two runs' values are whole seconds is learned with the second's
	// cut, and refused at replay saying so.
	const p = "body.at.$date.$numberLong"
	typed := func(at time.Time) Leaf { return InstantLeaf(p, "body.at", at, time.Millisecond) }
	sec := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	ln := ClassifyIn(NewDriftCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: typed(sec.Add(61 * time.Second))}}, Shape{Leaves: map[string]Leaf{p: typed(sec)}}, sec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	o, ok := ln.Clock()
	if ln.Kind != KindNoise || !ok || o.Res < time.Second {
		t.Fatalf("the typed cut read: %s %+v,%v (%s); want noise that is a read at the second's cut", ln.Kind, o, ok, ln.Why)
	}
	from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if ok, why := CheckClockRead("at", o, typed(from), Span{From: from, To: from.Add(time.Second)}, true); ok || !strings.Contains(why, "the application cuts its value") {
		t.Errorf("a typed cut read: %v %q; want refused naming the cut", ok, why)
	}
}

// A replay's span (LiveSpan, ClockSpanFrom) is CallWindowFrom's own half: from
// the last data-plane serve before the call to the call, on the app's clock.
// It is none with no app clock (keploy cannot tell the application's clock),
// with no serve before the call, or where the app's clock did not run as one;
// the tracker on ctx tells it after the set's learn step ended, when
// TrackerFrom gives none.
func TestAReplaysSpanIsTheLiveHalfOfItsCallWindow(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	offset := -21 * time.Hour
	tr := NewTracker()
	ctx := WithTracker(context.Background(), tr)
	arrival := base.Add(3 * time.Second)
	if _, ok := tr.LiveSpan(arrival); ok {
		t.Fatal("a span with no app clock")
	}
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r.Add(offset), true })
	if _, ok := tr.LiveSpan(arrival); ok {
		t.Fatal("a span with no serve before the call")
	}
	m := &models.Mock{Name: "mock-1", Kind: models.Mongo}
	tr.ServedMockAt(m, base.Add(time.Second))
	tr.ServedMockAt(&models.Mock{Name: "mock-2", Kind: models.Mongo}, base.Add(4*time.Second))
	tr.SetLearnEnded(true)
	s, ok := ClockSpanFrom(ctx, arrival)
	want := Span{From: base.Add(time.Second + offset), To: arrival.Add(offset)}
	if !ok || s != want || TrackerFrom(ctx) != nil {
		t.Fatalf("span = %+v,%v; want %+v, read after the learn step ended", s, ok, want)
	}
	tr.SetAppClock(func(r time.Time) (time.Time, bool) {
		if r.After(base.Add(2 * time.Second)) {
			return r.Add(offset + time.Minute), true
		}
		return r.Add(offset), true
	})
	if _, ok := tr.LiveSpan(arrival); ok {
		t.Fatal("a span across a move of the app's clock")
	}
	if _, ok := ClockSpanFrom(context.Background(), arrival); ok {
		t.Fatal("a span with no tracker on ctx")
	}
}

// A replay reads a clock read only from a value that is one whole instant
// naming its moment: a suffix or a prefix around an instant, a time of day
// and a wall clock with no zone are refused, whatever the window. An
// offset's refusal says the value is no read of the clock plus that offset.
// A learning is a clock read only as noise with a read or a clock + offset.
func TestAClockReadIsOnlyAWholeInstantNamingItsMoment(t *testing.T) {
	from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := Span{From: from, To: from.Add(40 * time.Millisecond)}
	plain := models.ClockOffset{Res: time.Second}
	const path = "body.since"
	text := func(v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }
	const now = "2026-10-27T12:00:00Z"
	if ok, why := CheckClockRead(path, plain, text(now), s, true); !ok {
		t.Fatalf("the window's own now refused: %s", why)
	}
	for _, c := range []struct {
		what string
		live Leaf
	}{
		{"a suffix after the instant", text(now + "x")},
		{"a prefix before it", text("x" + now)},
		{"a time of day, which names no day", text("12:00:00")},
		{"a typed wall clock, which names no zone", Leaf{Path: path, Noise: path, Type: Instant, Value: "2026-10-27T12:00"}},
	} {
		if ok, _ := CheckClockRead(path, plain, c.live, s, true); ok {
			t.Errorf("%s: answered as a read of the clock", c.what)
		}
	}
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Second, Res: time.Second}
	if ok, why := CheckClockRead(path, ttl, text(now), s, true); ok || !strings.Contains(why, "plus that offset") {
		t.Fatalf("an offset's refusal: %v %q", ok, why)
	}
	if got, ok := (Learned{Kind: KindClockOffset, Offset: &ttl}).Clock(); !ok || got != ttl {
		t.Fatalf("a clock + offset learning reads as %+v,%v", got, ok)
	}
	if _, ok := (Learned{Kind: KindVariable, Read: &plain}).Clock(); ok {
		t.Fatal("a variable read as a clock read")
	}
	if _, ok := (Learned{Kind: KindNoise}).Clock(); ok {
		t.Fatal("noise with no read read as a clock read")
	}
}

// A live span needs the app's clock even with a serve before the call, and
// starts at the last serve before the call's arrival, never at one stamped
// at the arrival itself.
func TestALiveSpanNeedsTheAppsClockAndAServeBeforeTheCall(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	arrival := base.Add(3 * time.Second)
	tr := NewTracker()
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base.Add(time.Second))
	if _, ok := tr.LiveSpan(arrival); ok {
		t.Fatal("a span with a serve before the call but no app clock")
	}
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(&models.Mock{Name: "mock-2", Kind: models.Mongo}, arrival)
	s, ok := tr.LiveSpan(arrival)
	if want := (Span{From: base.Add(time.Second), To: arrival}); !ok || s != want {
		t.Fatalf("span = %+v,%v; want %+v: a serve at the call's own arrival is no serve before it", s, ok, want)
	}
}

// plainRead reports whether ln is noise that is a plain read of the
// application's clock (Learned.Read), which a replay checks in its call's
// span, never noise that lets any value pass.
func plainRead(ln Learned) bool {
	o, ok := ln.Clock()
	return ln.Kind == KindNoise && ok && o.Plain()
}

// A value that is one instant whole in both runs, in a call with no window
// (a test's own call, read by the lead alone), is a plain read of the clock,
// compared at every replay in its call's own span, whether or not the lead
// moved it: how late record's full round read its clock (a loaded box) must
// not decide what is learned, and noise there would let any value pass at
// every replay. In a window that holds it in both runs it is one too. A value
// that is no instant whole (a random id, an instant inside a longer value) is
// noise as before. A value learned so that is no read of the clock fails the
// next replay's check, record's confirming round's first, which names the
// field: it is filtered there with that reason, never kept to go red later.
func TestAWholeInstantWithNoCallWindowIsAClockReadCheckedAtReplay(t *testing.T) {
	rec := time.Date(2026, 10, 27, 9, 0, 0, 123e6, time.UTC)
	call := rec.Add(time.Millisecond)
	const p = "body.processed_at"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	iso := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000Z07:00") }
	classify := func(r, l string, w *Window) Learned {
		return ClassifyIn(NewDriftCache(), []string{p}, shape(l), shape(r), call, nil, nil, nil, w)[0]
	}
	// F read its clock 3.4 s later than the lead puts it (a loaded box), or
	// 0.4 s earlier.
	late := rec.Add(FullRoundLead + 3400*time.Millisecond)
	early := rec.Add(FullRoundLead - 400*time.Millisecond)
	window := &Window{Rec: Span{From: rec.Add(-5 * time.Millisecond), To: call}, F: Span{From: late.Add(-5 * time.Millisecond), To: late.Add(time.Millisecond)}}
	for _, row := range []struct {
		what string
		r, l string
		w    *Window
		read bool
	}{
		{"one instant whole, no window, read late", iso(rec), iso(late), nil, true},
		{"one instant whole, no window, read early", iso(rec), iso(early), nil, true},
		{"an epoch millisecond, no window, read late", strconv.FormatInt(rec.UnixMilli(), 10), strconv.FormatInt(late.UnixMilli(), 10), nil, true},
		{"one instant whole, no window, moved by the lead", iso(rec), iso(rec.Add(FullRoundLead)), nil, true},
		{"one instant whole, its window holds it in both runs", iso(rec), iso(late), window, true},
		{"a random id, no window", "a7f3c2e9", "9c21e04b", nil, false},
		{"an instant inside a longer value, no window", "ord-" + iso(rec), "ord-" + iso(late), nil, false},
	} {
		ln := classify(row.r, row.l, row.w)
		if !row.read {
			if ln.Kind != KindNoise || ln.Read != nil {
				t.Errorf("%s: %s Read=%+v (%s); want noise", row.what, ln.Kind, ln.Read, ln.Why)
			}
			continue
		}
		if o, ok := ln.Clock(); ln.Kind != KindNoise || !ok || !o.Plain() {
			t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock, compared at replay", row.what, ln.Kind, ln.Read, ln.Why)
		}
	}
	// The confirming round replays the test: its call carries a read of its
	// own clock (in its span), or a value that is no read of the clock, such
	// as the recorded one again (an instant the application built on what
	// the test gave it), which the check refuses, naming the field.
	o, ok := classify(iso(rec), iso(late), nil).Clock()
	if !ok {
		t.Fatal("a whole instant with no window learned no read of the clock")
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond)}
	if ok, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: iso(now.Add(5 * time.Millisecond))}, span, true); !ok {
		t.Errorf("a read of the confirming round's clock in its call's span: refused (%s); want it kept", why)
	}
	for _, v := range []string{iso(rec), iso(late), iso(now.AddDate(0, 0, -30))} {
		ok, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: v}, span, true)
		if ok || !strings.HasPrefix(why, p+" was learned as ") || !strings.Contains(why, "it changed") {
			t.Errorf("%s in the confirming round: ok %v (%s); want refused, naming %s and saying it changed", v, ok, why, p)
		}
	}
}

// A value that is one instant whole in both runs, in a call whose window on
// the application's clock does not hold it in both runs, that the lead does
// not move, is refused as a clock read before its call's window (or no plain
// read of the clock): learned as noise, nothing would compare it again, and
// any value would pass. Moved by the lead it is still a plain read of the
// clock; a value that is no instant whole stays noise.
func TestAWholeInstantOutsideItsCallsWindowIsRefused(t *testing.T) {
	rec := time.Date(2026, 10, 27, 9, 0, 0, 123e6, time.UTC)
	call := rec.Add(time.Millisecond)
	const p = "body.audited_at"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	iso := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000Z07:00") }
	live := rec.Add(FullRoundLead + 8*time.Second)
	// The call's window opens 5 ms before it in both runs; the value was
	// read 2 s before the window in record and 6 s before it in F.
	window := &Window{Rec: Span{From: call.Add(-5 * time.Millisecond), To: call}, F: Span{From: live.Add(-5 * time.Millisecond), To: live.Add(time.Millisecond)}}
	before, fBefore := rec.Add(-2*time.Second), live.Add(-6*time.Second)
	for _, row := range []struct {
		what   string
		r, l   string
		refuse bool
	}{
		{"read before its call's window in both runs", iso(before), iso(fBefore), true},
		{"an epoch millisecond read before its window", strconv.FormatInt(before.UnixMilli(), 10), strconv.FormatInt(fBefore.UnixMilli(), 10), true},
		{"later than its call in both runs, no offset keploy reads", iso(call.Add(time.Hour)), iso(live.Add(2 * time.Hour)), true},
		{"outside its window, moved by the lead", iso(before), iso(before.Add(FullRoundLead)), false},
		{"a random id", "a7f3c2e9", "9c21e04b", false},
	} {
		ln := ClassifyIn(NewDriftCache(), []string{p}, shape(row.l), shape(row.r), call, nil, nil, nil, window)[0]
		switch {
		case row.refuse:
			if ln.Kind != KindUnaccounted || ln.Why != outsideWindowWhy || !strings.Contains(ln.Why, "a clock read before its call's window") {
				t.Errorf("%s: %s (%s); want refused as a clock read before its call's window", row.what, ln.Kind, ln.Why)
			}
		case textClockRead(row.r, row.l) != nil:
			if o, ok := ln.Clock(); ln.Kind != KindNoise || !ok || !o.Plain() {
				t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock", row.what, ln.Kind, ln.Read, ln.Why)
			}
		default:
			if ln.Kind != KindNoise || ln.Read != nil {
				t.Errorf("%s: %s Read=%+v (%s); want noise", row.what, ln.Kind, ln.Read, ln.Why)
			}
		}
	}
}
