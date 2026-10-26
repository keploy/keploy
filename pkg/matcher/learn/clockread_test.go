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
