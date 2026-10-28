package learn

import (
	"context"
	"fmt"
	"net/http"
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
// in it) stays plain noise (Read nil); a wall clock that names no zone is a
// plain read too, in UTC where the learn step reads such a clock in UTC
// alone (models.ZoneUTC).
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
		return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: l}}, Shape{Leaves: map[string]Leaf{p: r}}, rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	}
	ln := classify(text(rec), text(live))
	o, ok := ln.Clock()
	if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Res != time.Nanosecond {
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
	if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() || o.Res < time.Millisecond {
		t.Fatalf("the typed read: %s %+v,%v (%s); want noise that is a plain read", ln.Kind, o, ok, ln.Why)
	} else if !ClockReadHolds(o, typed(now), s) || ClockReadHolds(o, typed(now.Add(-30*24*time.Hour)), s) || !ClockReadHolds(o, typed(s.To.Add(time.Millisecond)), s) || ClockReadHolds(o, typed(s.To.Add(2*time.Millisecond)), s) {
		t.Errorf("the typed read: want the replay's now and a millisecond past the span (a writer that rounds) held, thirty days before and two milliseconds past the span not")
	}

	id := func(at time.Time) Leaf { return Leaf{Path: "body.id", Type: String, Value: "ord-" + ns(at)} }
	if ln := classify(id(rec), id(live)); ln.Kind == KindNoise && ln.Read != nil {
		t.Errorf("a read inside a longer value: Read=%+v; want plain noise", ln.Read)
	}
	wall := func(at time.Time) Leaf {
		return WallClockLeaf("body.at", "body.at", at.Truncate(time.Millisecond), time.Millisecond)
	}
	if ln := classify(wall(rec), wall(live)); !zonedRead(ln, models.ZoneUTC) {
		t.Errorf("a wall clock of no zone: %s Read=%+v; want a plain read in UTC", ln.Kind, ln.Read)
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

// A value the application cuts coarser than a millisecond, below its
// encoding (a second's value written in nanoseconds), shows its cut
// (readRes: the coarsest step both values are whole multiples of), which
// the learn step learns (founder decision, 2026-10-27) and a replay checks
// it at (CheckClockRead): a value cut so stands for its whole cut, so it
// passes wherever in its second its read fell, one up to a second stale
// passes, and one stale by more is refused as changed, naming the cut it
// was read at. A value that is no whole second is checked at its own
// nanosecond. A cut of a millisecond is learned and checked at the cut the
// same. An offset whose Res is coarser than the replay's encoding is
// refused up front, naming both resolutions.
func TestACutClockReadIsLearnedAndCheckedAtItsCut(t *testing.T) {
	ns := func(at time.Time) Leaf { return Leaf{Type: String, Value: strconv.FormatInt(at.UnixNano(), 10)} }
	from := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	span := Span{From: from.Add(400 * time.Millisecond), To: from.Add(600 * time.Millisecond)}
	rec := time.Date(2026, 10, 26, 9, 0, 1, 0, time.UTC)
	if r := textClockRead(ns(rec).Value, ns(rec.Add(62*time.Second)).Value); r == nil || r.Res != time.Second {
		t.Fatalf("textClockRead = %+v; want a Res of the second both values are cut to", r)
	} else {
		const p = "createdAt"
		in := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: ns(at).Value}}}
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, in(rec.Add(62*time.Second)), in(rec), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]; ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Res != time.Second {
			t.Errorf("an epoch in nanoseconds cut to the second, at learn: %s Read=%+v (%s); want a clock read learned at its 1s cut", ln.Kind, ln.Read, ln.Why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from), span, true); !ok {
			t.Errorf("a value cut to the second its read fell in: %v %q; want it checked at the cut, and passed", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(-time.Second)), span, true); !ok {
			t.Errorf("a value cut to the second before its span's (one cut stale): %v %q; want passed", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(-2*time.Second)), span, true); ok || !strings.HasSuffix(why, "it changed (read as standing for the whole 1s it is cut to)") {
			t.Errorf("a value cut to the second two before its span's: %v %q; want refused as changed, naming the cut", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(500*time.Millisecond+7)), span, true); !ok {
			t.Errorf("a value no whole second, in the span: %v %q; want it checked at its own nanosecond, and passed", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(400*time.Millisecond-7)), span, true); ok || !strings.HasSuffix(why, "it changed") {
			t.Errorf("a value no whole second, 7ns before the span: %v %q; want refused as changed", ok, why)
		}
	}
	rec = time.Date(2026, 10, 26, 9, 0, 0, 123000000, time.UTC)
	if r := textClockRead(ns(rec).Value, ns(rec.Add(FullRoundLead+7*time.Millisecond)).Value); r == nil || r.Res != time.Millisecond {
		t.Fatalf("textClockRead = %+v; want a Res of the millisecond both values are cut to", r)
	} else {
		late := Span{From: from.Add(5*time.Millisecond + 400*time.Microsecond), To: from.Add(5*time.Millisecond + 600*time.Microsecond)}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(5*time.Millisecond)), late, true); !ok {
			t.Errorf("a value cut to the millisecond its read fell in: %v %q; want it checked at the cut, and passed", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(4*time.Millisecond)), late, true); !ok {
			t.Errorf("a value cut to the millisecond before its span's (one cut stale): %v %q; want passed", ok, why)
		}
		if ok, why := CheckClockRead("createdAt", *r, ns(from.Add(3*time.Millisecond)), late, true); ok || !strings.HasSuffix(why, "it changed (read as standing for the whole 1ms it is cut to)") {
			t.Errorf("a value cut to the millisecond two before its span's: %v %q; want refused as changed", ok, why)
		}
	}
	// The same for an instant its protocol types: a $date of milliseconds
	// whose two runs' values are whole seconds is learned at the second's
	// cut, and a replay checks a value cut so at it.
	const p = "body.at.$date.$numberLong"
	typed := func(at time.Time) Leaf { return InstantLeaf(p, "body.at", at, time.Millisecond) }
	sec := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: typed(sec.Add(61 * time.Second))}}, Shape{Leaves: map[string]Leaf{p: typed(sec)}}, sec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	if ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Res != time.Second {
		t.Errorf("the typed cut read: %s Read=%+v (%s); want a clock read learned at the second's cut", ln.Kind, ln.Read, ln.Why)
	} else {
		if ok, why := CheckClockRead("at", *ln.Read, typed(from), span, true); !ok {
			t.Errorf("a typed value cut to the second its read fell in: %v %q; want passed", ok, why)
		}
		if ok, why := CheckClockRead("at", *ln.Read, typed(from.Add(-time.Second)), span, true); !ok {
			t.Errorf("a typed value cut to the second, a second stale: %v %q; want passed", ok, why)
		}
		if ok, why := CheckClockRead("at", *ln.Read, typed(from.Add(-2*time.Second)), span, true); ok || !strings.HasSuffix(why, "it changed (read as standing for the whole 1s it is cut to)") {
			t.Errorf("a typed value cut to the second, two seconds stale: %v %q; want refused naming the cut", ok, why)
		}
	}
	// An offset learned at a millisecond, against a value written in
	// nanoseconds: checked at the millisecond (a clock read plus an offset,
	// then cut, is a clock read, 2026-10-28), as standing for its whole
	// millisecond: one in the span plus the offset passes, whatever its
	// nanoseconds; one 395 ms before the span's start plus the offset is
	// refused, naming the cut.
	off := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond}
	for _, v := range []time.Time{from.Add(30*time.Minute + 500*time.Millisecond + 7), from.Add(30*time.Minute + 500*time.Millisecond)} {
		if ok, why := CheckClockRead("expiresAt", off, ns(v), span, true); !ok {
			t.Errorf("FALSE RED: an offset learned at 1ms, a nanosecond value in the span %s: %v %q; want passed", ns(v).Value, ok, why)
		}
	}
	for _, v := range []time.Time{from.Add(30*time.Minute + 5*time.Millisecond + 7), from.Add(30*time.Minute + 5*time.Millisecond)} {
		if ok, why := CheckClockRead("expiresAt", off, ns(v), span, true); ok || !strings.HasSuffix(why, "it changed (checked as standing for the whole 1ms the learn step read it cut to)") {
			t.Errorf("an offset learned at 1ms, a nanosecond value %s: %v %q; want refused naming the 1ms cut", ns(v).Value, ok, why)
		}
	}
	// A window too wide to tell a time to live changed by a minute: refused
	// saying so, never that the value changed.
	secOff := models.ClockOffset{Min: 30*time.Minute - 155*time.Millisecond, Max: 30*time.Minute + 368*time.Millisecond, Res: time.Second}
	wide := Span{From: from, To: from.Add(57 * time.Second)}
	if ok, why := CheckClockRead("expiresAt", secOff, typed(from.Add(30*time.Minute)), wide, true); ok || !strings.Contains(why, "but this call's window (57s) with the offset's range and three of its cut is no narrower than the minute the offset reads in") || strings.Contains(why, "it changed") {
		t.Errorf("an offset at a second's cut, a window 57 s wide: %v %q; want refused naming the window", ok, why)
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
	const noZone = "a wall clock that names no zone, where the learn step read an instant that names its moment"
	for _, c := range []struct {
		what, says string
		live       Leaf
	}{
		{"a suffix after the instant", "no instant keploy reads", text(now + "x")},
		{"a prefix before it", "no instant keploy reads", text("x" + now)},
		{"a time of day, which names no day", "no instant keploy reads", text("12:00:00")},
		{"a typed wall clock, which names no zone", noZone, Leaf{Path: path, Noise: path, Type: Instant, Value: "2026-10-27T12:00"}},
		{"a text wall clock, which names no zone", noZone, text("2026-10-27T12:00:00")},
		{"a typed time of day, which names no day", "no instant keploy reads", TimeOfDayLeaf(path, path, 12*time.Hour, time.Second)},
	} {
		if ok, why := CheckClockRead(path, plain, c.live, s, true); ok || !strings.Contains(why, c.says) {
			t.Errorf("%s: %v %q; want refused as %s", c.what, ok, why, c.says)
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

// runClock is a clock the hooks move the way enterprise's run clock is
// moved: by writes, each from its begin to its end (to), after which the
// app's clock shows the real instant plus its offset. A past instant is
// told by the writes made by then, none while one was being written, and
// the real clock before the first.
type runClock struct{ writes []runWrite }

type runWrite struct {
	from, to time.Time
	offset   time.Duration
}

func (c *runClock) at(r time.Time) (time.Time, bool) {
	b, ok := c.began(r)
	if !ok {
		return time.Time{}, false
	}
	off := time.Duration(0)
	for _, w := range c.writes {
		if !b.Before(w.to) {
			off = w.offset
		}
	}
	return r.Add(off), true
}

// began is where the clock's run to r began (Tracker.SetAppClockRun): the
// end of the last write begun by r; zero before the first.
func (c *runClock) began(r time.Time) (time.Time, bool) {
	var b time.Time
	for _, w := range c.writes {
		if r.Before(w.from) {
			break
		}
		if r.Before(w.to) {
			return time.Time{}, false
		}
		b = w.to
	}
	return b, true
}

// A server test's begin moves the app's clock (the enterprise run clock)
// after the previous test's last serve, so the clock did not run as one
// from that serve to the test's first call. Where the hooks tell where the
// clock's run began (SetAppClockRun), the call's span starts there, the
// move's write's end, and a replay checks the call's learned read of the
// clock in it: a read of the moved clock passes, one of the clock before
// the move does not. Of two moves the later starts it; the next test's move
// changes nothing, however late the call is checked; a call that reaches
// keploy while a move is written has none; and a server test's call with
// no serve before it has none. Hooks that tell no runs leave it none.
func TestALiveSpanStartsWhereTheAppsClocksRunBegan(t *testing.T) {
	base := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	back := -30 * 24 * time.Hour // the run clock goes back to the test's recorded day
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: back}
	clock := &runClock{writes: []runWrite{move}}
	arrival := base.Add(3 * time.Second)
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base.Add(time.Second))
	if s, ok := tr.LiveSpan(arrival); ok {
		t.Fatalf("span = %+v across the move with no run told; want none", s)
	}
	tr.SetAppClockRun(clock.began)
	want := Span{From: move.to.Add(back), To: arrival.Add(back)}
	if s, ok := tr.LiveSpan(arrival); !ok || s != want {
		t.Fatalf("span = %+v,%v; want %+v: from where the clock's run began", s, ok, want)
	}

	const p = "body.since"
	read := models.ClockOffset{Res: time.Millisecond}
	at := func(v time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: v.UTC().Format("2006-01-02T15:04:05.000Z")}
	}
	s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), arrival)
	if !ok {
		t.Fatal("no span for the test's first call")
	}
	if ok, why := CheckClockRead(p, read, at(arrival.Add(back-time.Millisecond)), s, true); !ok {
		t.Fatalf("a read of the moved clock as the call was made: refused (%s)", why)
	}
	if ok, _ := CheckClockRead(p, read, at(arrival.Add(-time.Millisecond)), s, true); ok {
		t.Fatal("a read of the clock before its move passed the moved clock's span")
	}
	if ok, _ := CheckClockRead(p, read, at(move.from.Add(back)), s, true); ok {
		t.Fatal("a read the moved clock shows before the move's write ended passed")
	}

	// The next test's move, after the call, however late the call is checked.
	clock.writes = append(clock.writes, runWrite{from: arrival.Add(time.Millisecond), to: arrival.Add(2 * time.Millisecond), offset: back + time.Hour})
	if s, ok := tr.LiveSpan(arrival); !ok || s != want {
		t.Fatalf("span = %+v,%v after the next test's move; want %+v", s, ok, want)
	}

	// Of two moves after the serve, the later starts the span.
	second := runWrite{from: base.Add(2500 * time.Millisecond), to: base.Add(2500*time.Millisecond + 1), offset: back + time.Minute}
	clock.writes = []runWrite{move, second}
	if s, ok := tr.LiveSpan(arrival); !ok || s != (Span{From: second.to.Add(second.offset), To: arrival.Add(second.offset)}) {
		t.Fatalf("span = %+v,%v; want it from the later move's end %v", s, ok, second.to)
	}

	// A call that reaches keploy while a move is written has none.
	if s, ok := tr.LiveSpan(second.from); ok {
		t.Fatalf("span = %+v for a call that reached keploy mid-write; want none", s)
	}

	// A server test's call with no serve before it has none, moved or not.
	tr = NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	if s, ok := tr.LiveSpan(arrival); ok {
		t.Fatalf("span = %+v with no serve before the call; want none", s)
	}
}

// A clock put back on real time, frozen, and put back again (a real test,
// then a frozen one, then a real one): the clock reads the same offset at a
// serve in the first real stretch and at the call in the last, but its run
// began at the last move, so the span starts there, wherever the call lands
// in it, and a read made in the first stretch is refused. Without the run
// told, the span from that serve runs across the frozen stretch, as before.
func TestALiveSpanDoesNotRunAcrossAStretchTheClockLeftAndCameBackTo(t *testing.T) {
	base := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	w := func(at time.Duration, off time.Duration) runWrite {
		return runWrite{from: base.Add(at), to: base.Add(at + time.Millisecond), offset: off}
	}
	last := w(3500*time.Millisecond, 0)
	clock := &runClock{writes: []runWrite{w(0, time.Hour), w(1500*time.Millisecond, 0), w(2500*time.Millisecond, 2*time.Hour), last}}
	const p = "body.since"
	read := models.ClockOffset{Res: time.Millisecond}
	stale := Leaf{Path: p, Noise: p, Type: String, Value: base.Add(1600 * time.Millisecond).Format("2006-01-02T15:04:05.000Z")}
	for _, serve := range []time.Duration{time.Second, 1600 * time.Millisecond} {
		for _, arrival := range []time.Duration{3900 * time.Millisecond, 4 * time.Second, 5 * time.Second} {
			tr := NewTracker()
			tr.SetAppClock(clock.at)
			tr.SetAppClockRun(clock.began)
			tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base.Add(serve))
			s, ok := tr.LiveSpan(base.Add(arrival))
			if want := (Span{From: last.to, To: base.Add(arrival)}); !ok || s != want {
				t.Fatalf("serve %v, arrival %v: span = %+v,%v; want %+v", serve, arrival, s, ok, want)
			}
			if ok, _ := CheckClockRead(p, read, stale, s, true); ok {
				t.Fatalf("serve %v, arrival %v: a read made before the frozen stretch passed", serve, arrival)
			}
		}
	}
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base.Add(1600*time.Millisecond))
	if s, ok := tr.LiveSpan(base.Add(3900 * time.Millisecond)); !ok || !s.From.Equal(base.Add(1600*time.Millisecond)) {
		t.Fatalf("span = %+v,%v with no run told; want it from the serve, read at its two ends", s, ok)
	}
	// Hooks that cannot tell the run where they tell the clock: none.
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, false })
	if s, ok := tr.LiveSpan(base.Add(3900 * time.Millisecond)); ok {
		t.Fatalf("span = %+v where the run cannot be told; want none", s)
	}
}

// In a client test the test's begin bounds a span where the clock's run
// began before it; where the run began after the begin, the span starts at
// the run, with or without a serve before the call.
func TestAClientTestsSpanStartsAtTheLaterOfItsBeginAndTheClocksRun(t *testing.T) {
	base := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: time.Hour}
	clock := &runClock{writes: []runWrite{move}}
	arrival := base.Add(3 * time.Second)
	for _, tc := range []struct {
		name   string
		served bool
		began  time.Time
		from   time.Time // on the real clock
	}{
		{"a begin after the move", true, move.to.Add(10 * time.Millisecond), move.to.Add(10 * time.Millisecond)},
		{"a begin before the move", true, move.from.Add(-time.Millisecond), move.to},
		{"no serve, a begin before the move", false, move.from.Add(-time.Millisecond), move.to},
		{"no serve, a begin after the move", false, move.to.Add(10 * time.Millisecond), move.to.Add(10 * time.Millisecond)},
		// A begin after the arrival (read after the call came) bounds nothing.
		{"a begin after the arrival", true, arrival.Add(500 * time.Millisecond), move.to},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.SetClientTests(true)
			tr.SetAppClock(clock.at)
			tr.SetAppClockRun(clock.began)
			if tc.served {
				tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base.Add(time.Second))
			}
			tr.BeginAt(tc.began)
			want := Span{From: tc.from.Add(time.Hour), To: arrival.Add(time.Hour)}
			if s, ok := tr.LiveSpan(arrival); !ok || s != want {
				t.Fatalf("span = %+v,%v; want %+v", s, ok, want)
			}
		})
	}
}

// clockedDrifts is an empty cache of a tracker that knows which clock the
// application reads (DriftCache.appClock), as the agent's own hooks tell it
// (Tracker.SetAppClock): the learn step's every row but those of a run
// whose clock keploy cannot tell, which make theirs with NewDriftCache.
func clockedDrifts() *DriftCache {
	dc := NewDriftCache()
	dc.appClock = true
	return dc
}

// plainRead reports whether ln is noise that is a plain read of the
// application's clock (Learned.Read), which a replay checks in its call's
// span, never noise that lets any value pass.
func plainRead(ln Learned) bool {
	o, ok := ln.Clock()
	return ln.Kind == KindClockRead && ok && o.Plain()
}

// noiseOrRead reports whether ln is learned as noise or as a plain read of
// the clock, and a read only as its own kind: noise never carries a read
// (KindNoise with Read nil), and a read always does (KindClockRead), so a
// caller that writes its noise leaves to the noise a replay skips never
// stops comparing a read of the clock.
func noiseOrRead(ln Learned) bool {
	return ln.Kind == KindNoise && ln.Read == nil || plainRead(ln)
}

// todRead reports whether ln is learned as a plain read of the clock that
// wrote a time of day (models.LayoutTimeOfDay), which a replay checks
// modulo a day in its call's span, in zone ("": any zone).
func todRead(ln Learned, zone string) bool {
	o, ok := ln.Clock()
	return plainRead(ln) && ok && models.LayoutTimeOfDay(o.Layout) && (zone == "" || o.Zone == zone)
}

// zonedRead reports whether ln is a plain read of the clock (plainRead) that
// a replay reads in zone (models.ClockOffset.Zone).
func zonedRead(ln Learned, zone string) bool {
	o, ok := ln.Clock()
	return plainRead(ln) && ok && o.Zone == zone
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
		return ClassifyIn(clockedDrifts(), []string{p}, shape(l), shape(r), call, nil, nil, nil, w)[0]
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
		if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() {
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
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(row.l), shape(row.r), call, nil, nil, nil, window)[0]
		switch {
		case row.refuse:
			if ln.Kind != KindUnaccounted || ln.Why != outsideWindowWhy || !strings.Contains(ln.Why, "a clock read before its call's window") {
				t.Errorf("%s: %s (%s); want refused as a clock read before its call's window", row.what, ln.Kind, ln.Why)
			}
		case textClockRead(row.r, row.l) != nil:
			if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() {
				t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock", row.what, ln.Kind, ln.Read, ln.Why)
			}
		default:
			if ln.Kind != KindNoise || ln.Read != nil {
				t.Errorf("%s: %s Read=%+v (%s); want noise", row.what, ln.Kind, ln.Read, ln.Why)
			}
		}
	}
}

// A wall clock of no zone (a text "2026-10-27 09:00:00.123", a type of its
// protocol that names no zone) that its call's window holds only in the
// application's zone, IST here, is a plain read of the clock in that zone
// (models.ZoneApp), never noise left unchecked: a replay checks it as the
// wall clock of the application's zone the replay's span carries
// (Span.Zone). The replay's own now there passes; one an hour on is
// refused, naming the field and the zone; with no zone told at replay, even
// the right value is refused, saying keploy cannot tell the zone; one that
// names its zone where the learn step read a wall clock of none, in any of
// the zones it reads one in, is refused as changed. One its window holds in UTC is read in UTC
// (models.ZoneUTC), whatever zone the replay carries.
func TestAWallClockOfNoZoneIsCheckedAtReplayInTheZoneItWasLearnedIn(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	fRead := read.Add(FullRoundLead + 3400*time.Millisecond)
	w := &Window{Rec: Span{From: read.Add(-5 * time.Millisecond), To: call}, F: Span{From: fRead.Add(-5 * time.Millisecond), To: fRead.Add(time.Millisecond)}}
	const p = "body.cmd.filter.seenAt"
	text := func(in *time.Location) func(time.Time) Leaf {
		return func(at time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: at.In(in).Format("2006-01-02 15:04:05.000")}
		}
	}
	typed := func(in *time.Location) func(time.Time) Leaf {
		return func(at time.Time) Leaf { return WallClockLeaf(p, p, at.In(in), time.Millisecond) }
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}
	for _, form := range []struct {
		what string
		leaf func(*time.Location) func(time.Time) Leaf
	}{{"a text", text}, {"a typed wall clock", typed}} {
		ln := classifyZoned(ist, false, p, form.leaf(ist)(read), form.leaf(ist)(fRead), call, w)
		if !zonedRead(ln, models.ZoneApp) {
			t.Fatalf("%s its window holds only in IST: %s Read=%+v (%s); want a plain read in the application's zone", form.what, ln.Kind, ln.Read, ln.Why)
		}
		o, _ := ln.Clock()
		for _, row := range []struct {
			what string
			live Leaf
			s    Span
			want bool
			why  string
		}{
			{"the replay's now in the application's zone", form.leaf(ist)(now.Add(5 * time.Millisecond)), span, true, ""},
			{"an hour on", form.leaf(ist)(now.Add(time.Hour)), span, false, "it changed"},
			{"the replay's now written in UTC", form.leaf(time.UTC)(now.Add(5 * time.Millisecond)), span, false, "it changed"},
			// The learn step named the zone (+05:30, models.ClockOffset.AppZone):
			// a replay reads it there whatever its tracker knows.
			{"the replay's now, the application's zone not told", form.leaf(ist)(now.Add(5 * time.Millisecond)), Span{From: span.From, To: span.To}, true, ""},
		} {
			ok, why := CheckClockRead(p, o, row.live, row.s, true)
			if ok != row.want || !strings.Contains(why, row.why) || !ok && !strings.HasPrefix(why, p+" was learned as clock, a wall clock of no zone in the application's zone") {
				t.Errorf("%s, %s: %v %q; want %v naming %q", form.what, row.what, ok, why, row.want, row.why)
			}
			if got := ClockReadHolds(o, row.live, row.s); got != row.want {
				t.Errorf("%s, %s: ClockReadHolds %v; want %v", form.what, row.what, got, row.want)
			}
		}
		if o.AppZone != "+05:30" {
			t.Errorf("%s: learned in a zone of no name at UTC+05:30: %+v; want it named +05:30", form.what, o)
		}
		// An old file names no zone: a replay that cannot tell the
		// application's zone refuses it, saying so.
		oldFile := o
		oldFile.AppZone = ""
		if ok, why := CheckClockRead(p, oldFile, form.leaf(ist)(now.Add(5*time.Millisecond)), Span{From: span.From, To: span.To}, true); ok || !strings.Contains(why, "keploy cannot tell the application's zone") {
			t.Errorf("%s, an old file, the application's zone not told: %v %q; want refused, saying so", form.what, ok, why)
		}
		ln = classifyZoned(ist, false, p, form.leaf(time.UTC)(read), form.leaf(time.UTC)(fRead), call, w)
		if !zonedRead(ln, models.ZoneUTC) {
			t.Fatalf("%s its window holds in UTC: %s Read=%+v (%s); want a plain read in UTC", form.what, ln.Kind, ln.Read, ln.Why)
		}
		o, _ = ln.Clock()
		if ok, why := CheckClockRead(p, o, form.leaf(time.UTC)(now.Add(5*time.Millisecond)), span, true); !ok {
			t.Errorf("%s read in UTC, the replay's now in UTC: refused (%s)", form.what, why)
		}
		if ok, _ := CheckClockRead(p, o, form.leaf(ist)(now.Add(5*time.Millisecond)), span, true); ok {
			t.Errorf("%s read in UTC, the replay's now in IST: passed", form.what)
		}
	}
	named := Leaf{Path: p, Type: String, Value: now.Add(5 * time.Millisecond).Format("2006-01-02T15:04:05.000Z")}
	for _, zone := range []string{models.ZoneUTC, models.ZoneApp, models.ZoneEither} {
		if ok, why := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Zone: zone}, named, span, true); ok || !strings.Contains(why, "names its zone") {
			t.Errorf("a value that names its zone, the replay's now, learned as a wall clock of none in %s: %v %q; want refused, saying it names its zone", zone, ok, why)
		}
	}
	if ok, why := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Zone: "pst"}, text(ist)(now.Add(5*time.Millisecond)), span, true); ok || !strings.Contains(why, "rather than read it in another zone. Record the test again with this keploy") {
		t.Errorf("a zone this keploy does not read: %v %q; want refused whatever it carries, saying so", ok, why)
	}
}

// A text read of the clock written in one layout that names its zone in one
// run ("…Z") and none in the other (a wall clock of no zone) is refused at
// learn, naming both forms, in either order, with a window or none, in an
// application east of UTC or one keploy reads in UTC alone: a replay checks
// a read in the form its learn step saw, and this one saw two
// (textFormChangedWhy). A place learned as an instant that names its moment
// (Zone "") refuses a wall clock of no zone at replay, a text or a typed
// one, naming it, and passes one that names its moment.
func TestATextClockReadThatNamesItsZoneInOneRunOnlyIsRefused(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	fRead := read.Add(FullRoundLead + 3400*time.Millisecond)
	w := &Window{Rec: Span{From: read.Add(-5 * time.Millisecond), To: call}, F: Span{From: fRead.Add(-5 * time.Millisecond), To: fRead.Add(time.Millisecond)}}
	const p = "body.cmd.filter.seenAt"
	named := func(at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.UTC().Format("2006-01-02T15:04:05.000Z")}
	}
	wall := func(at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.UTC().Format("2006-01-02T15:04:05.000")}
	}
	for _, zone := range []*time.Location{ist, nil} {
		for _, win := range []*Window{w, nil} {
			for _, runs := range [][2]func(time.Time) Leaf{{named, wall}, {wall, named}} {
				rec, live := runs[0](read), runs[1](fRead)
				ln := classifyZoned(zone, false, p, rec, live, call, win)
				if ln.Kind != KindUnaccounted || ln.Read != nil || ln.Why != textFormChangedWhy(rec.Value, live.Value) || !strings.Contains(ln.Why, "a wall clock that names no zone") || !strings.Contains(ln.Why, "an instant that names its moment") {
					t.Errorf("%s then %s (zone %v, window %v): %s Read=%+v (%s); want refused, naming both forms", rec.Value, live.Value, zone, win != nil, ln.Kind, ln.Read, ln.Why)
				}
			}
			// One form in both runs: a read.
			for _, form := range []func(time.Time) Leaf{named, wall} {
				if ln := classifyZoned(zone, false, p, form(read), form(fRead), call, win); !plainRead(ln) {
					t.Errorf("%s then %s (zone %v, window %v): %s Read=%+v (%s); want a plain read", form(read).Value, form(fRead).Value, zone, win != nil, ln.Kind, ln.Read, ln.Why)
				}
			}
		}
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}
	o := models.ClockOffset{Res: time.Millisecond}
	for _, live := range []Leaf{wall(now.Add(5 * time.Millisecond)), WallClockLeaf(p, p, now.Add(5*time.Millisecond), time.Millisecond)} {
		if ok, why := CheckClockRead(p, o, live, span, true); ok || !strings.Contains(why, "is a wall clock that names no zone, where the learn step read an instant that names its moment") {
			t.Errorf("a wall clock of no zone (%s) at a place learned as an instant that names its moment: %v %q; want refused, naming it", live.Value, ok, why)
		}
	}
	if ok, why := CheckClockRead(p, o, named(now.Add(5*time.Millisecond)), span, true); !ok {
		t.Errorf("an instant that names its moment, the replay's now, at a place learned as one: refused (%s)", why)
	}
}

// With no window (a test's own call), a wall clock of no zone is read in the
// zone of its recorded reading nearest its call, no later than it
// (nearestZone): an application east of UTC (IST) or west of it (New
// York's -4h) writing its own zone's wall clock is read in the
// application's zone; one writing UTC's, in UTC; a recorded reading that is
// one instant in both zones (the application's zone is UTC), in either
// (models.ZoneEither), as the recording cannot tell the two apart.
func TestAWallClockWithNoWindowIsReadInTheZoneOfItsReadingNearestItsCall(t *testing.T) {
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	fRead := read.Add(FullRoundLead + 3400*time.Millisecond)
	const p = "body.cmd.filter.seenAt"
	text := func(in *time.Location) func(time.Time) Leaf {
		return func(at time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: at.In(in).Format("2006-01-02 15:04:05.000")}
		}
	}
	east, west := time.FixedZone("IST", 5*3600+1800), time.FixedZone("EDT", -4*3600)
	for _, row := range []struct {
		what        string
		zone, write *time.Location
		want        string
	}{
		{"east of UTC, its own zone's", east, east, models.ZoneApp},
		{"east of UTC, UTC's", east, time.UTC, models.ZoneUTC},
		{"west of UTC, its own zone's", west, west, models.ZoneApp},
		{"west of UTC, UTC's", west, time.UTC, models.ZoneUTC},
		{"the application's zone UTC", time.UTC, time.UTC, models.ZoneEither},
	} {
		if ln := classifyZoned(row.zone, false, p, text(row.write)(read), text(row.write)(fRead), call, nil); !zonedRead(ln, row.want) {
			t.Errorf("%s: %s Read=%+v (%s); want a plain read in %q", row.what, ln.Kind, ln.Read, ln.Why, row.want)
		}
	}
	if got := nearestZone(read.Add(5*time.Hour+30*time.Minute), time.Time{}, east); got != models.ZoneUTC {
		t.Errorf("a call not known: %q; want UTC", got)
	}
	// Both readings after the call: the earlier.
	if got := nearestZone(call.Add(6*time.Hour), call, east); got != models.ZoneApp {
		t.Errorf("both readings after the call, the application's the earlier: %q; want the application's zone", got)
	}
	if got := nearestZone(call.Add(time.Hour), call, west); got != models.ZoneUTC {
		t.Errorf("both readings after the call, UTC's the earlier: %q; want UTC", got)
	}
}

// A replay's span carries the application's zone the tracker was told
// (SetAppZone), and none where it was told none.
func TestALiveSpanCarriesTheApplicationsZone(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, base)
	if s, ok := tr.LiveSpan(base.Add(time.Second)); !ok || s.Zone != nil {
		t.Fatalf("no zone told: %+v,%v; want a span with no zone", s, ok)
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	tr.SetAppZone(ist)
	if s, ok := tr.LiveSpan(base.Add(time.Second)); !ok || s.Zone != ist {
		t.Fatalf("IST told: %+v,%v; want a span in IST", s, ok)
	}
}

// A time of day the lead moved (a Postgres time its protocol types, or a
// text "10:00:41" whole) names no day, so no window holds it: it is a read of
// the clock all the same, in the zone its recording read it in (UTC, the
// application's, or either where the application's zone is UTC there),
// which every replay checks modulo a day in its call's span, the midnight
// it crosses too: a value of the replay's own clock passes, one moved by a
// minute is refused, naming the place. A replay that cannot tell the
// application's zone refuses one read in it; a value in another layout, or
// a time of day where the place was learned as an instant that names its
// day (and the reverse), is refused. Never noise that lets any value pass.
// One the lead did not move, with no window, stays refused at learn (a
// plain read needs a day: noWindowRead).
func TestATimeOfDayTheLeadMovedIsAClockReadCheckedModuloTheDay(t *testing.T) {
	const p = "body.cmd.filter.at"
	ist := time.FixedZone("IST", 5*3600+1800)
	typed := func(tod time.Duration) Leaf { return TimeOfDayLeaf(p, p, tod, time.Microsecond) }
	text := func(tod time.Duration) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: timeOfDayDay.Add(tod).Format("15:04:05.000")}
	}
	todIn := func(at time.Time, loc *time.Location) time.Duration {
		at = at.In(loc)
		return at.Sub(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc))
	}
	forms := map[string]func(time.Duration) Leaf{"typed": typed, "text": text}
	for name, leaf := range forms {
		for _, tc := range []struct {
			zone     string
			app, loc *time.Location
			call     time.Time
		}{
			{models.ZoneUTC, nil, time.UTC, time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)},
			{models.ZoneUTC, ist, time.UTC, time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)},
			{models.ZoneApp, ist, ist, time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)},
			// Read just before midnight in the zone, the full round just after.
			{models.ZoneUTC, ist, time.UTC, time.Date(2026, 10, 27, 23, 59, 30, 0, time.UTC)},
			{models.ZoneApp, ist, ist, time.Date(2026, 10, 27, 18, 29, 30, 0, time.UTC)},
			{models.ZoneEither, time.UTC, time.UTC, time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)},
		} {
			at := fmt.Sprintf("%s, read in %s, called at %s", name, tc.zone, tc.call.Format(time.RFC3339Nano))
			read := tc.call.Add(-5*time.Millisecond - 123*time.Microsecond)
			ln := classifyZoned(tc.app, false, p, leaf(todIn(read, tc.loc)), leaf(todIn(read.Add(FullRoundLead+37*time.Millisecond+411*time.Microsecond), tc.loc)), tc.call, nil)
			o, ok := ln.Clock()
			if !todRead(ln, tc.zone) || !ok {
				t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock modulo a day, in %s", at, ln.Kind, ln.Read, ln.Why, tc.zone)
				continue
			}
			// A replay 3 days later, its call a second long.
			replay := tc.call.Add(72*time.Hour + 2*time.Second)
			s := Span{From: replay.Add(-time.Second), To: replay, Zone: tc.app}
			for _, held := range []time.Time{s.From, s.From.Add(400 * time.Millisecond), s.To} {
				if ok, why := CheckClockRead(p, o, leaf(todIn(held, tc.loc)), s, true); !ok {
					t.Errorf("%s: the replay's own time of day %s refused: %s", at, held.In(tc.loc).Format("15:04:05.000"), why)
				}
			}
			for _, moved := range []time.Duration{-time.Minute, time.Minute, 12 * time.Hour, -5*time.Hour - 30*time.Minute, 5*time.Hour + 30*time.Minute} {
				v := leaf(todIn(s.From.Add(400*time.Millisecond+moved), tc.loc))
				if ok, why := CheckClockRead(p, o, v, s, true); ok || !strings.HasPrefix(why, p+" was learned as") {
					t.Errorf("%s: the replay's time of day moved by %s: %v (%s); want refused, naming the place", at, moved, ok, why)
				}
			}
			// A replay whose call's span crosses midnight in the zone: a
			// read on either side of it passes, one a minute off does not.
			y, m, d := replay.In(tc.loc).Date()
			midnight := time.Date(y, m, d+1, 0, 0, 0, 0, tc.loc)
			ms := Span{From: midnight.Add(-980 * time.Millisecond), To: midnight.Add(20 * time.Millisecond), Zone: tc.app}
			for _, held := range []time.Time{ms.From.Add(100 * time.Millisecond), midnight, ms.To} {
				if ok, why := CheckClockRead(p, o, leaf(todIn(held, tc.loc)), ms, true); !ok {
					t.Errorf("%s: the replay's own time of day %s, its span across midnight, refused: %s", at, held.In(tc.loc).Format("15:04:05.000"), why)
				}
			}
			for _, moved := range []time.Duration{-time.Minute, time.Minute} {
				if ok, _ := CheckClockRead(p, o, leaf(todIn(midnight.Add(moved), tc.loc)), ms, true); ok {
					t.Errorf("%s: a time of day %s off midnight passed a span across midnight", at, moved)
				}
			}
			if tc.zone != models.ZoneUTC {
				// An old file names no zone (models.ClockOffset.AppZone): a
				// replay that cannot tell the application's refuses it; one
				// learned naming it reads it there.
				oldFile := o
				oldFile.AppZone = ""
				if ok, why := CheckClockRead(p, oldFile, leaf(todIn(s.To, tc.loc)), Span{From: s.From, To: s.To}, true); ok || !strings.Contains(why, "cannot tell the application's zone") {
					t.Errorf("%s: an old file, the zone unknown at replay: %v (%s); want refused, saying so", at, ok, why)
				}
				if o.AppZone != "" {
					if ok, why := CheckClockRead(p, o, leaf(todIn(s.To, tc.loc)), Span{From: s.From, To: s.To}, true); !ok {
						t.Errorf("%s: learned naming %s, the zone unknown at replay: refused (%s); want read in it", at, o.AppZone, why)
					}
				}
			}
			other := forms["text"]
			if name == "text" {
				other = forms["typed"]
			}
			if ok, why := CheckClockRead(p, o, other(todIn(s.To, tc.loc)), s, true); ok || !strings.Contains(why, "in another form") {
				t.Errorf("%s: the replay's value in the other form: %v (%s); want refused, saying so", at, ok, why)
			}
			if ok, _ := CheckClockRead(p, o, InstantLeaf(p, p, s.To, time.Millisecond), s, true); ok {
				t.Errorf("%s: an instant naming its day where a time of day was learned passed", at)
			}
		}
	}
	// Where the recording could not tell UTC from the application's zone
	// (models.ZoneEither), a replay in IST passes the time of day read in
	// either zone, and no other.
	either := models.ClockOffset{Res: time.Millisecond, Zone: models.ZoneEither, Layout: "time-second"}
	is := Span{From: time.Date(2026, 10, 30, 3, 30, 0, 0, time.UTC), To: time.Date(2026, 10, 30, 3, 30, 1, 0, time.UTC), Zone: ist}
	for _, loc := range []*time.Location{time.UTC, ist} {
		if ok, why := CheckClockRead(p, either, text(todIn(is.To, loc)), is, true); !ok {
			t.Errorf("either zone, a replay in IST, the time of day in %s: refused: %s", loc, why)
		}
	}
	if ok, _ := CheckClockRead(p, either, text(todIn(is.To, time.FixedZone("EDT", -4*3600))), is, true); ok {
		t.Error("either zone, a replay in IST, the time of day in EDT: passed")
	}
	// A text time of day the lead moved where keploy cannot tell the
	// application's clock: a plain read all the same, as a typed one is,
	// which a replay with no app clock refuses.
	{
		read := time.Date(2026, 10, 27, 3, 29, 59, 123e6, time.UTC)
		dc := NewDriftCache()
		ln := ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: text(todIn(read.Add(FullRoundLead+37*time.Millisecond), time.UTC))}}, Shape{Leaves: map[string]Leaf{p: text(todIn(read, time.UTC))}}, read.Add(5*time.Millisecond), nil, nil, nil, nil)[0]
		if !todRead(ln, models.ZoneUTC) {
			t.Errorf("a text time of day the lead moved, no app clock: %s Read=%+v (%s); want a plain read modulo a day, in UTC", ln.Kind, ln.Read, ln.Why)
		}
	}
	// Recorded at midnight to the second, "00:00:00" then "00:01:01": the
	// two share the second, never more than a day's grain (todGrain), so a
	// replay's value of its own second passes.
	{
		rec := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
		sec := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		ln := ClassifyIn(clockedDrifts(), []string{p}, sec("00:01:01"), sec("00:00:00"), rec.Add(400*time.Millisecond), nil, nil, nil, nil)[0]
		o, _ := ln.Clock()
		rs := Span{From: rec.Add(48*time.Hour + 7*time.Second), To: rec.Add(48*time.Hour + 7*time.Second + 300*time.Millisecond)}
		if ok, why := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: "00:00:07"}, rs, true); !todRead(ln, models.ZoneUTC) || !ok {
			t.Errorf("recorded at midnight: %s Read=%+v (%s); the replay's own second: %v (%s); want a read to the second that passes it", ln.Kind, ln.Read, ln.Why, ok, why)
		}
	}
	// A time of day where the place was learned as an instant: none.
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	s := Span{From: call.Add(-time.Second), To: call}
	for _, lay := range []string{models.LayoutTyped, "iso-second", ""} {
		if ok, _ := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Layout: lay}, typed(todIn(call, time.UTC)), s, true); ok {
			t.Errorf("a time of day at a place learned as an instant (layout %q) passed", lay)
		}
	}
	// A text time of day the lead did not move (an hour, 37 s, a minute
	// back), with no window: the word rule lets it go, but it is a time of
	// day whole in both runs, so a plain read modulo a day all the same,
	// never noise, which a replay checks in its call's span: its own time of
	// day passes, one an hour stale is refused. Where keploy cannot tell the
	// application's clock it is refused at learn (noAppClockWhy).
	for _, d := range []time.Duration{time.Hour, 37 * time.Second, -time.Minute} {
		read := call.Add(-5 * time.Millisecond)
		rec, live := text(todIn(read, time.UTC)), text(todIn(read.Add(d), time.UTC))
		ln := classifyZoned(nil, false, p, rec, live, call, nil)
		o, _ := ln.Clock()
		if !todRead(ln, models.ZoneUTC) {
			t.Errorf("a text time of day %s then %s, no window: %s Read=%+v (%s); want a plain read modulo a day, in UTC", rec.Value, live.Value, ln.Kind, ln.Read, ln.Why)
			continue
		}
		rs := Span{From: call.Add(48*time.Hour - time.Second), To: call.Add(48 * time.Hour)}
		if ok, why := CheckClockRead(p, o, text(todIn(rs.To, time.UTC)), rs, true); !ok {
			t.Errorf("a text time of day %s then %s: the replay's own time of day refused: %s", rec.Value, live.Value, why)
		}
		stale := text(todIn(rs.To.Add(-time.Hour), time.UTC))
		if ok, _ := CheckClockRead(p, o, stale, rs, true); ok {
			t.Errorf("a text time of day %s then %s: a time of day an hour stale, %s, passed", rec.Value, live.Value, stale.Value)
		}
		dc := NewDriftCache()
		ln = ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, call, nil, nil, nil, nil)[0]
		if ln.Kind != KindUnaccounted || ln.Read != nil || ln.Why != noAppClockWhy {
			t.Errorf("a text time of day %s then %s, no window, no app clock: %s Read=%+v (%s); want refused: %s", rec.Value, live.Value, ln.Kind, ln.Read, ln.Why, noAppClockWhy)
		}
	}
	// Not one the lead moved: refused at learn, with no window (R04).
	read := call.Add(-5 * time.Millisecond)
	if ln := classifyZoned(nil, false, p, typed(todIn(read, time.UTC)), typed(todIn(read.Add(time.Hour), time.UTC)), call, nil); ln.Kind != KindUnaccounted || ln.Read != nil || ln.Why != instantNotClock {
		t.Errorf("a typed time of day moved by an hour, no window: %s Read=%+v (%s); want refused: %s", ln.Kind, ln.Read, ln.Why, instantNotClock)
	}
	// Two layouts in its two runs (to the minute, then the second): refused.
	if ln := classifyZoned(nil, false, p, Leaf{Path: p, Noise: p, Type: String, Value: "03:29"}, Leaf{Path: p, Noise: p, Type: String, Value: "03:31:01"}, call, nil); ln.Kind != KindUnaccounted || ln.Read != nil || !strings.Contains(ln.Why, "a time of day to the minute") {
		t.Errorf("a text time of day in two layouts: %s Read=%+v (%s); want refused, naming both", ln.Kind, ln.Read, ln.Why)
	}
	// One learned with an offset, or in no zone, which no learn step writes
	// (a clocks entry edited by hand, or written by another build), holds
	// nothing, and its reason names which: keploy checks neither. Without
	// these guards the replay refuses it all the same, the time of day of
	// a forward offset read on the day before, but with another check's
	// reason (a cut, or "it changed"); the value its offset names (an hour
	// after the clock, as a time to live's end) is no exception.
	for name, leaf := range forms {
		layout := models.LayoutTypedTimeOfDay
		if name == "text" {
			layout = "time-second"
		}
		for _, tc := range []struct {
			o    models.ClockOffset
			v    time.Time
			want string
		}{
			{models.ClockOffset{Min: time.Hour, Max: time.Hour + time.Minute, Res: time.Millisecond, Zone: models.ZoneUTC, Layout: layout}, call.Add(time.Hour - 500*time.Millisecond), "a time of day plus an offset"},
			{models.ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Microsecond, Zone: models.ZoneUTC, Layout: layout}, call, "a time of day plus an offset"},
			{models.ClockOffset{Res: time.Millisecond, Layout: layout}, call, "a time of day read in no zone"},
		} {
			if ok, why := CheckClockRead(p, tc.o, leaf(todIn(tc.v, time.UTC)), s, true); ok || !strings.Contains(why, tc.want) {
				t.Errorf("a %s time of day learned as %+v, the value %s: %v (%s); want refused, saying %q", name, tc.o, tc.v.Format("15:04:05.000"), ok, why, tc.want)
			}
		}
	}
}

// Where keploy cannot tell which clock the application reads (no app clock:
// DriftCache.appClock), its calls have no window and no replay can check a
// read of the clock (Tracker.LiveSpan): an instant whole that neither the
// lead nor a window shows a read (one F read 5 s past the lead) is refused at
// learn, the reason saying why, never learned as a read only a replay would
// refuse. With an app clock it is a plain read, checked at replay. Either
// verdict is one wherever the recording fell in its second (.500, .996 and
// the last three milliseconds, where the recorded read and its call fall in
// the next second), for a typed instant (a Mongo $date, read whole) and a
// text one (RFC 3339, epoch milliseconds), beside the served receive whose
// Date header names the recording's second. One the lead moved is a read
// with no app clock too, which a replay with none refuses: fail closed
// either way.
func TestAnInstantIsRefusedAtLearnWhereKeployCannotTellTheAppsClock(t *testing.T) {
	const p = "body.cmd.documents.0.at"
	iso := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	forms := map[string]func(time.Time) Leaf{
		"a $date": func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Millisecond) },
		"RFC 3339 text": func(at time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: iso(at)}
		},
		"epoch milliseconds": func(at time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: strconv.FormatInt(at.UnixMilli(), 10)}
		},
	}
	for _, msec := range []int{500, 996, 997, 998, 999} {
		rec := time.Date(2026, 10, 24, 22, 4, 38, msec*int(time.Millisecond), time.UTC)
		read, call := rec.Add(3*time.Millisecond), rec.Add(4*time.Millisecond)
		receive := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "POST", URL: "/queue/receive", Body: `{"n":10}`},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": rec.Format(http.TimeFormat)},
				Body: `{"Messages":[{"Body":"m-1"}]}`},
			ReqTimestampMock: rec.Add(-2 * time.Second), ResTimestampMock: rec,
		}}
		for name, leaf := range forms {
			classify := func(dc *DriftCache, l time.Time) Learned {
				raw := []Raw{dc.RawOf(receive, true)}
				shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: leaf(at)}} }
				return ClassifyIn(dc, []string{p}, shape(l), shape(read), call, nil, raw, nil, nil)[0]
			}
			at := fmt.Sprintf("recorded at .%d, %s", msec, name)
			past := read.Add(FullRoundLead + 5*time.Second)
			if ln := classify(NewDriftCache(), past); ln.Kind != KindUnaccounted || ln.Read != nil || ln.Why != noAppClockWhy {
				t.Errorf("%s, 5 s past the lead, no app clock: %s Read=%+v (%s); want refused: %s", at, ln.Kind, ln.Read, ln.Why, noAppClockWhy)
			}
			if ln := classify(clockedDrifts(), past); !plainRead(ln) {
				t.Errorf("%s, 5 s past the lead, an app clock: %s Read=%+v (%s); want a plain read, checked at replay", at, ln.Kind, ln.Read, ln.Why)
			}
			if ln := classify(NewDriftCache(), read.Add(FullRoundLead)); !plainRead(ln) {
				t.Errorf("%s, moved by the lead, no app clock: %s Read=%+v (%s); want a plain read, which a replay with no app clock refuses", at, ln.Kind, ln.Read, ln.Why)
			}
		}
	}
	tr := NewTracker()
	tr.ServedMockAt(&models.Mock{Name: "mock-0", Kind: models.HTTP}, time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC))
	if _, ok := tr.LiveSpan(time.Date(2026, 10, 28, 7, 30, 1, 0, time.UTC)); ok || tr.Drifts().appClock {
		t.Fatal("a tracker with no app clock gives a call a span, or its cache says it knows the application's clock")
	}
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	if !tr.Drifts().appClock {
		t.Fatal("SetAppClock left the running cache with no app clock")
	}
	tr.Begin()
	if !tr.Drifts().appClock {
		t.Fatal("the next test's cache forgot the app clock")
	}
	tr.SetAppClock(nil)
	if tr.Drifts().appClock {
		t.Fatal("SetAppClock(nil) left the cache with an app clock")
	}
}

// A caller whose replay checks no read of the clock, and switches on the
// kind alone (a parser that writes every KindNoise to the noise its replay
// skips, and refuses any kind it does not know with its Why), never receives
// a read of the clock as noise: every read is its own kind (KindClockRead),
// so such a caller refuses it, never learns it as noise that lets any value
// pass at every replay. One keploy refused before it learned it as a read
// (an instant its protocol types with no call window that the lead did not
// move) keeps that reason; one it learned as noise before (the lead moved
// it, a window held it) says it is a read this replay does not check. A
// caller that checks reads takes each as a plain read (Clock).
func TestACallerThatChecksNoClockReadNeverReceivesOneAsNoise(t *testing.T) {
	rec := time.Date(2026, 10, 27, 9, 0, 0, 123e6, time.UTC)
	call := rec.Add(time.Millisecond)
	const p = "body.at"
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{p: l}} }
	text := func(at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.Format("2006-01-02T15:04:05.000Z07:00")}
	}
	typed := func(at time.Time) Leaf { return InstantLeaf(p, p, at, time.Millisecond) }
	wall := func(at time.Time) Leaf { return WallClockLeaf(p, p, at, time.Millisecond) }
	lead, late := rec.Add(FullRoundLead), rec.Add(FullRoundLead+3400*time.Millisecond)
	w := &Window{Rec: Span{From: rec.Add(-time.Second), To: call}, F: Span{From: late.Add(-time.Second), To: late.Add(time.Millisecond)}}
	// unaware is such a caller: noise for a KindNoise, a binding for a
	// variable or an echo, its refusal with the learn step's Why otherwise.
	unaware := func(ln Learned) (bool, string) {
		switch ln.Kind {
		case KindNoise:
			return true, ""
		case KindEcho, KindVariable:
			return false, ""
		}
		return false, p + " changed, and " + ln.Why
	}
	for _, row := range []struct {
		what   string
		r, l   Leaf
		w      *Window
		before string // why keploy refused it before it learned a read ("": noise)
	}{
		{"a text instant whole the lead moved", text(rec), text(lead), nil, ""},
		{"a text instant whole with no window, read late", text(rec), text(late), nil, ""},
		{"a text instant whole its window holds", text(rec), text(late), w, ""},
		{"a typed instant the lead moved", typed(rec), typed(lead), nil, ""},
		{"a typed instant its window holds", typed(rec), typed(late), w, ""},
		{"a typed instant with no window, read late", typed(rec), typed(late), nil, instantNotClock},
		{"a wall clock of no zone the lead moved", wall(rec), wall(lead), nil, ""},
		{"a wall clock of no zone with no window, read late", wall(rec), wall(late), nil, instantNotClock},
	} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(row.l), shape(row.r), call, nil, nil, nil, row.w)[0]
		if o, ok := ln.Clock(); ln.Kind != KindClockRead || ln.Read == nil || !ok || !o.Plain() {
			t.Errorf("%s: %s Read=%+v (%s); want a plain read of the clock, of its own kind", row.what, ln.Kind, ln.Read, ln.Why)
			continue
		}
		want := clockReadUncheckedWhy
		if row.before != "" {
			want = row.before
		}
		if noise, why := unaware(ln); noise || why != p+" changed, and "+want {
			t.Errorf("FALSE GREEN? %s, to a caller that checks no read: noise %v (%s); want refused: %s", row.what, noise, why, want)
		}
	}
}

// A date of no zone (a day's resolution) is read in the zone its recording
// tells, and where the recording cannot tell UTC's from the application's
// (its call falls in one day in both zones, or the application's zone is
// UTC there), in either (models.ZoneEither): a replay passes the value read
// in UTC or in the application's zone. The replay's verdict on the
// application's own value is then one at every hour of the day, whichever
// zone the application writes and whatever hour it was recorded at, never a
// pass at one hour and a refusal at another; a date a day off either way
// passes (one cut stale or ahead: models.ClockOffset.holdsRead), one three
// days off is refused at every hour, and one two days off passes only read
// in the other zone, under either zone, within a day of the span there (the
// stated limit). For UTC, IST and EDT applications, writing their
// own zone's date or UTC's, a text and a typed date, in a call with no
// window and in one whose window holds the call, recorded at every hour.
func TestADateOfNoZoneIsCheckedAtEveryHourWhicheverZoneItIsWrittenIn(t *testing.T) {
	const p = "body.day"
	day := func(at time.Time, in *time.Location) time.Time {
		a := at.In(in)
		return time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	}
	text := func(at time.Time, in *time.Location) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: day(at, in).Format("2006-01-02")}
	}
	typed := func(at time.Time, in *time.Location) Leaf { return WallClockLeaf(p, p, day(at, in), 24*time.Hour) }
	ist, edt := time.FixedZone("IST", 5*3600+1800), time.FixedZone("EDT", -4*3600)
	for _, app := range []struct {
		what string
		zone *time.Location
	}{{"UTC", time.UTC}, {"IST", ist}, {"EDT", edt}} {
		for _, write := range []*time.Location{app.zone, time.UTC} {
			for _, form := range []struct {
				what string
				leaf func(time.Time, *time.Location) Leaf
			}{{"text", text}, {"typed", typed}} {
				for _, windowed := range []bool{false, true} {
					for h := 0; h < 24; h++ {
						call := time.Date(2026, 10, 27, h, 30, 0, 500e6, time.UTC)
						read := call.Add(-5 * time.Millisecond)
						// record's full round a day later: the date moved.
						fRead, fCall := read.Add(24*time.Hour), call.Add(24*time.Hour)
						var w *Window
						if windowed {
							w = &Window{Rec: Span{From: read.Add(-5 * time.Millisecond), To: call}, F: Span{From: fRead.Add(-5 * time.Millisecond), To: fCall}}
						}
						what := fmt.Sprintf("%s app writing %s's date, %s, window %v, recorded at %02d:30Z", app.what, write, form.what, windowed, h)
						ln := classifyZoned(app.zone, false, p, form.leaf(read, write), form.leaf(fRead, write), call, w)
						o, ok := ln.Clock()
						if !plainRead(ln) || !ok {
							t.Fatalf("%s: %s Read=%+v (%s); want a plain read of the clock", what, ln.Kind, ln.Read, ln.Why)
						}
						want := models.ZoneApp
						switch {
						case day(call, time.UTC).Equal(day(call, app.zone)):
							want = models.ZoneEither
						case write == time.UTC:
							want = models.ZoneUTC
						}
						if o.Zone != want {
							t.Errorf("%s: learned in %q; want %q", what, o.Zone, want)
						}
						for rh := 0; rh < 24; rh++ {
							now := time.Date(2026, 11, 3, rh, 10, 0, 0, time.UTC)
							s := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: app.zone, Own: true}
							at := now.Add(5 * time.Millisecond)
							if ok, why := CheckClockRead(p, o, form.leaf(at, write), s, true); !ok {
								t.Errorf("%s: the application's own date at %02d:10Z refused (%s)", what, rh, why)
							}
							// One day off either way is within one cut of a
							// day of the span: it passes.
							for _, d := range []int{-1, 1} {
								if ok, why := CheckClockRead(p, o, form.leaf(at.AddDate(0, 0, d), write), s, true); !ok {
									t.Errorf("%s: a date %+d day off at %02d:10Z refused (%s); want passed (one cut stale or ahead)", what, d, rh, why)
								}
							}
							// Two days off passes only where it is read in the
							// other zone, under either zone, within one day of
							// the span there (the stated limit); three never.
							other := app.zone
							if write == app.zone {
								other = time.UTC
							}
							for _, d := range []int{-3, -2, 2, 3} {
								dd := day(at.AddDate(0, 0, d), write)
								in := time.Date(dd.Year(), dd.Month(), dd.Day(), 0, 0, 0, 0, other)
								f, e := s.From.In(other), s.To.In(other)
								lo := time.Date(f.Year(), f.Month(), f.Day()-1, 0, 0, 0, 0, other)
								passes := o.Zone == models.ZoneEither && (d == 2 || d == -2) && !in.Before(lo) && !in.After(e.AddDate(0, 0, 1))
								if ok, _ := CheckClockRead(p, o, form.leaf(at.AddDate(0, 0, d), write), s, true); ok != passes {
									t.Errorf("%s: a date %+d days off at %02d:10Z: passed %v; want %v (only read in the other zone, under either zone, within one day of the span)", what, d, rh, ok, passes)
								}
							}
						}
						if o.Zone == models.ZoneEither {
							now := time.Date(2026, 11, 3, 12, 10, 0, 0, time.UTC)
							// An old file names no zone (models.ClockOffset.AppZone).
							oldFile := o
							oldFile.AppZone = ""
							if ok, why := CheckClockRead(p, oldFile, form.leaf(now, write), Span{From: now, To: now.Add(20 * time.Millisecond)}, true); ok || !strings.Contains(why, "keploy could not read the zone of the process that made this call, and the learn step named none") {
								t.Errorf("%s: an old file, the application's zone not told at replay: %v %q; want refused, saying so", what, ok, why)
							}
							if ok, why := CheckClockRead(p, o, form.leaf(now, write), Span{From: now, To: now.Add(20 * time.Millisecond)}, true); o.AppZone == "" || !ok {
								t.Errorf("%s: learned naming %q, the application's zone not told at replay: %v %q; want read in it", what, o.AppZone, ok, why)
							}
							if !strings.HasPrefix(o.String(), "clock, a wall clock of no zone in UTC or in the application's zone") {
								t.Errorf("%s: String %q", what, o.String())
							}
						}
					}
				}
			}
		}
	}
}

// Where the recording's window holds a date in both zones but record's full
// round's holds it in one alone (the full round ran a day and eight hours
// later, past the hour the two zones' dates part), the zone is the one both
// runs hold, never either: an IST application writing its own date is read
// in its zone, one writing UTC's in UTC. A text and a typed date.
func TestADateTheFullRoundTellsTheZoneOfIsReadInThatZoneAlone(t *testing.T) {
	const p = "body.day"
	ist := time.FixedZone("IST", 5*3600+1800)
	day := func(at time.Time, in *time.Location) time.Time {
		a := at.In(in)
		return time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	}
	text := func(at time.Time, in *time.Location) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: day(at, in).Format("2006-01-02")}
	}
	typed := func(at time.Time, in *time.Location) Leaf { return WallClockLeaf(p, p, day(at, in), 24*time.Hour) }
	call := time.Date(2026, 10, 27, 12, 0, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	fCall := call.Add(32 * time.Hour)
	fRead := fCall.Add(-5 * time.Millisecond)
	w := &Window{Rec: Span{From: read.Add(-5 * time.Millisecond), To: call}, F: Span{From: fRead.Add(-5 * time.Millisecond), To: fCall}}
	for _, form := range []struct {
		what string
		leaf func(time.Time, *time.Location) Leaf
	}{{"text", text}, {"typed", typed}} {
		for _, row := range []struct {
			write *time.Location
			want  string
		}{{ist, models.ZoneApp}, {time.UTC, models.ZoneUTC}} {
			ln := classifyZoned(ist, false, p, form.leaf(read, row.write), form.leaf(fRead, row.write), call, w)
			if !zonedRead(ln, row.want) {
				t.Errorf("%s written in %s: %s Read=%+v (%s); want a plain read in %q", form.what, row.write, ln.Kind, ln.Read, ln.Why, row.want)
			}
		}
	}
}

// A span holds an instant read at resolution res where some instant of the
// span lies in its unit [t, t+res), on UTC's grid of res or off it: the day
// of a date read in IST starts at 18:30 UTC, and a span at 20:00 UTC holds
// that day, never the one before or after it. An instant of no resolution is
// held where it lies in the span, its start and its end too.
func TestASpanHoldsTheUnitOfAnInstantOnOrOffUTCsGrid(t *testing.T) {
	from := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)
	s := Span{From: from, To: from.Add(20 * time.Millisecond)}
	istDay := time.Date(2026, 10, 27, 18, 30, 0, 0, time.UTC)
	for _, row := range []struct {
		what string
		at   time.Time
		res  time.Duration
		want bool
	}{
		{"IST's day of the span", istDay, 24 * time.Hour, true},
		{"IST's day after it", istDay.Add(24 * time.Hour), 24 * time.Hour, false},
		{"IST's day before it", istDay.Add(-24 * time.Hour), 24 * time.Hour, false},
		{"UTC's day of the span", time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), 24 * time.Hour, true},
		{"UTC's next day", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), 24 * time.Hour, false},
		{"the second before the span", from.Add(-time.Second), time.Second, false},
		{"no resolution, at the span's start", from, 0, true},
		{"no resolution, at its end", s.To, 0, true},
		{"no resolution, a nanosecond before it", from.Add(-time.Nanosecond), 0, false},
		{"no resolution, a nanosecond after it", s.To.Add(time.Nanosecond), 0, false},
	} {
		if got := s.holds(row.at, row.res); got != row.want {
			t.Errorf("%s: %v; want %v", row.what, got, row.want)
		}
	}
}

// A read of the clock is checked at replay in the layout its learn step saw
// it written in (models.ClockOffset.Layout): a value cut to the minute, the
// hour or the day, an epoch number where an ISO text was learned, or another
// separator, is refused, naming the form; the learned form passes, with a
// fraction or without one, as a writer that drops a fraction's trailing
// zeros writes a read that fell on a whole second (the stated limit: a value
// cut to the second where a fraction was learned passes). A read whose two
// runs wrote it in two layouts is refused at learn, naming both. A typed
// read refuses a text, and a read learned before Layout was reads any form.
func TestAClockReadIsCheckedAtReplayInTheLayoutItWasLearnedIn(t *testing.T) {
	rec := time.Date(2026, 10, 27, 9, 0, 0, 123e6, time.UTC)
	call := rec.Add(time.Millisecond)
	late := rec.Add(FullRoundLead + 3400*time.Millisecond)
	const p = "body.processed_at"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	classify := func(r, l string) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, shape(l), shape(r), call, nil, nil, nil, nil)[0]
	}
	iso := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000Z07:00") }
	ln := classify(iso(rec), iso(late))
	o, ok := ln.Clock()
	if ln.Kind != KindClockRead || !ok || o.Layout != "iso-second" {
		t.Fatalf("an ISO text to the millisecond: %s %+v (%s); want a read in the layout iso-second", ln.Kind, ln.Read, ln.Why)
	}
	now := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	span := Span{From: now, To: now.Add(20 * time.Millisecond)}
	for _, row := range []struct {
		what, value string
		ok          bool
	}{
		{"the learned form", "2026-10-28T07:30:00.012Z", true},
		{"the learned form, its fraction's trailing zeros dropped", "2026-10-28T07:30:00.01Z", true},
		{"the learned form on a whole second (the stated limit)", "2026-10-28T07:30:00Z", true},
		{"the learned form, another zone named", "2026-10-28T13:00:00.012+05:30", true},
		{"cut to the minute", "2026-10-28T07:30Z", false},
		{"cut to the hour", "2026-10-28T07Z", false},
		{"cut to the day", "2026-10-28", false},
		{"an epoch number of seconds", "1793172600", false},
		{"an epoch number of milliseconds", "1793172600012", false},
		{"a space before the time", "2026-10-28 07:30:00.012Z", false},
		{"an HTTP date", "Wed, 28 Oct 2026 07:30:00 GMT", false},
	} {
		got, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: row.value}, span, true)
		if got != row.ok {
			t.Errorf("%s (%s): %v (%s); want %v", row.what, row.value, got, why, row.ok)
		}
		if !row.ok && !strings.Contains(why, "in another form than the learn step saw") {
			t.Errorf("%s: why %q; want it to name the form", row.what, why)
		}
	}
	// An application-zone wall clock learned at a millisecond refuses a date
	// and a time to the minute in that zone.
	ist := time.FixedZone("IST", 5*3600+1800)
	app := models.ClockOffset{Res: time.Millisecond, Zone: models.ZoneApp, Layout: "iso-second-space"}
	istSpan := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}
	for _, row := range []struct {
		value string
		ok    bool
	}{{"2026-10-28 13:00:00.012", true}, {"2026-10-28", false}, {"2026-10-28 13:00", false}} {
		if got, why := CheckClockRead(p, app, Leaf{Path: p, Type: String, Value: row.value}, istSpan, true); got != row.ok {
			t.Errorf("an app-zone wall clock at %s: %v (%s); want %v", row.value, got, why, row.ok)
		}
	}
	// Two layouts in the two runs: refused at learn, naming both.
	for _, row := range []struct{ what, r, l string }{
		{"an ISO text, then an epoch millisecond", iso(rec), strconv.FormatInt(late.UnixMilli(), 10)},
		{"to the second, then to the minute", rec.Format("2006-01-02T15:04:05Z"), late.Format("2006-01-02T15:04Z")},
		{"a T, then a space", iso(rec), late.Format("2006-01-02 15:04:05.000Z07:00")},
	} {
		ln := classify(row.r, row.l)
		if ln.Kind != KindUnaccounted || ln.Read != nil || ln.Why != textLayoutChangedWhy(row.r, row.l) || !strings.Contains(ln.Why, "as recorded and as") {
			t.Errorf("%s: %s Read=%+v (%s); want refused, naming both layouts", row.what, ln.Kind, ln.Read, ln.Why)
		}
	}
	// A typed read refuses a text; a read learned before Layout was reads
	// any form, as it did then.
	typed := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	if got, why := CheckClockRead(p, typed, Leaf{Path: p, Type: String, Value: "2026-10-28T07:30:00.012Z"}, span, true); got || !strings.Contains(why, "in another form") {
		t.Errorf("a typed read, a text at replay: %v (%s); want refused, naming the form", got, why)
	}
	if got, why := CheckClockRead(p, typed, InstantLeaf(p, p, now.Add(12*time.Millisecond), time.Millisecond), span, true); !got {
		t.Errorf("a typed read, typed at replay: refused (%s); want it to pass", why)
	}
	old := models.ClockOffset{Res: time.Millisecond}
	for _, v := range []string{"2026-10-28T07:30:00.012Z", "1793172600012"} {
		if got, why := CheckClockRead(p, old, Leaf{Path: p, Type: String, Value: v}, span, true); !got {
			t.Errorf("a read learned before Layout was, %s at replay: refused (%s); want it to pass", v, why)
		}
	}
	// A Layout this keploy does not name holds nothing.
	if got, why := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Layout: "iso-week"}, Leaf{Path: p, Type: String, Value: "2026-10-28T07:30:00.012Z"}, span, true); got {
		t.Errorf("an unknown layout: passed (%s); want refused", why)
	}
}

// clockTextLayout names each form's layout and resolution, but no fraction
// of a second and no zone.
func TestAClockTextsLayoutNamesItsFormAndResolution(t *testing.T) {
	for _, row := range []struct{ s, want string }{
		{"2026-10-28", "iso-date"},
		{"2026-10-28 UTC", "iso-date"},
		{"2026-10-28T07Z", "iso-hour"},
		{"2026-10-28T07:30", "iso-minute"},
		{"2026-10-28 07:30", "iso-minute-space"},
		{"2026-10-28T07:30:00", "iso-second"},
		{"2026-10-28T07:30:00.5Z", "iso-second"},
		{"2026-10-28T07:30:00,123456789+05:30", "iso-second"},
		{"2026-10-28 07:30:00.000", "iso-second-space"},
		{"Wed, 28 Oct 2026 07:30:00 GMT", "rfc1123"},
		{"20261028T073000Z", "compact"},
		{"1793172600", "epoch-s"},
		{"1793172600.25", "epoch-s"},
		{"1793172600012", "epoch-ms"},
		{"1793172600012345", "epoch-us"},
		{"1793172600012345678", "epoch-ns"},
		{"0199f1c4-8f40-7c3a-9b1e-2f6d3a4b5c6d", "uuid7"},
		{"07:30:00", ""},
		{"a7f3c2e9", ""},
		{"ord-2026-10-28", ""},
	} {
		if got := clockTextLayout(row.s); got != row.want {
			t.Errorf("%q: %q; want %q", row.s, got, row.want)
		}
		if row.want != "" {
			if _, ok := models.ClockLayoutNames[row.want]; !ok {
				t.Errorf("%q: layout %q has no name", row.s, row.want)
			}
		}
	}
}

// A time of day whose recording's windows hold its reading in UTC and in
// the application's zone alike (windows wider than the two readings lie
// apart: in an IST application, a time read in UTC reads in IST five and a
// half hours earlier, one read in IST reads in UTC eighteen and a half hours
// earlier) is learned in either zone (models.ZoneEither), as the recording
// cannot tell which the application read, so a replay passes it read in
// either; never in the zone whose reading is nearest the call alone, which
// would refuse the application's own value read in the other. Windows that
// hold one zone's reading learn that zone.
func TestATimeOfDayBothZonesWindowsHoldIsLearnedInEither(t *testing.T) {
	const p = "body.cmd.filter.at"
	ist := time.FixedZone("IST", 5*3600+1800)
	todIn := func(at time.Time, loc *time.Location) time.Duration {
		at = at.In(loc)
		return at.Sub(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc))
	}
	leaf := func(tod time.Duration) Leaf { return TimeOfDayLeaf(p, p, tod, time.Microsecond) }
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	fread := read.Add(FullRoundLead + 37*time.Millisecond)
	window := func(width time.Duration) *Window {
		return &Window{Rec: Span{From: call.Add(-width), To: call}, F: Span{From: fread.Add(5*time.Millisecond - width), To: fread.Add(5 * time.Millisecond)}}
	}
	for _, tc := range []struct {
		what  string
		loc   *time.Location
		width time.Duration
		want  string
	}{
		{"read in UTC, six-hour windows", time.UTC, 6 * time.Hour, models.ZoneEither},
		{"read in IST, six-hour windows", ist, 6 * time.Hour, models.ZoneApp},
		{"read in IST, nineteen-hour windows", ist, 19 * time.Hour, models.ZoneEither},
		{"read in UTC, one-second windows", time.UTC, time.Second, models.ZoneUTC},
		{"read in IST, one-second windows", ist, time.Second, models.ZoneApp},
	} {
		ln := classifyZoned(ist, false, p, leaf(todIn(read, tc.loc)), leaf(todIn(fread, tc.loc)), call, window(tc.width))
		if !todRead(ln, tc.want) {
			t.Errorf("%s: %s Read=%+v (%s); want a read modulo a day in %s", tc.what, ln.Kind, ln.Read, ln.Why, tc.want)
		}
	}
}

// Santiago's clocks skip from 00:00 to 01:00 on 2026-09-06: a date of no
// zone for that day is its first instant, 01:00, as Java and JavaScript
// read it (wallIn), never 23:00 the day before (Go's time.Date). So the
// day after, that date is one day stale and passes, the date before it is
// two and is refused, whatever hour the replay runs.
func TestADateOfNoZoneOnADayWithNoMidnightIsThatDaysFirstInstant(t *testing.T) {
	const p = "body.day"
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("no zone data: %v", err)
	}
	text := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	if got, want := wallIn(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).UnixNano(), santiago), time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("wallIn of 2026-09-06 00:00 in Santiago = %v, want %v (01:00 -03)", got, want)
	}
	// Ahead of UTC too, and over a skipped day: Lord Howe skips 02:00-02:30
	// on 2026-10-04 (+10:30 to +11), Samoa skipped 2011-12-30 (-10 to +14).
	for _, row := range []struct {
		zone       string
		wall, want time.Time
	}{
		{"Australia/Lord_Howe", time.Date(2026, 10, 4, 2, 10, 0, 0, time.UTC), time.Date(2026, 10, 3, 15, 40, 0, 0, time.UTC)},
		{"Pacific/Apia", time.Date(2011, 12, 30, 12, 0, 0, 0, time.UTC), time.Date(2011, 12, 30, 22, 0, 0, 0, time.UTC)},
		// Not skipped: as the zone's rules give it, just after a skip.
		{"Europe/Berlin", time.Date(2026, 3, 29, 3, 10, 0, 0, time.UTC), time.Date(2026, 3, 29, 1, 10, 0, 0, time.UTC)},
	} {
		loc, err := time.LoadLocation(row.zone)
		if err != nil {
			continue
		}
		if got := wallIn(row.wall.UnixNano(), loc); !got.Equal(row.want) || got.Location() != loc {
			t.Errorf("wallIn of %s in %s = %v, want %v there", row.wall.Format("2006-01-02 15:04"), row.zone, got, row.want.In(loc))
		}
	}
	// Learned in a plain period, the app in Santiago, at an hour its date is
	// not UTC's: the date is read in the app's zone alone (ZoneApp), so
	// every hour below reads it through wallIn.
	call := time.Date(2026, 1, 20, 2, 30, 0, 0, time.UTC)
	ln := classifyZoned(santiago, false, p, text("2026-01-19"), text("2026-01-20"), call, nil)
	o, ok := ln.Clock()
	if !plainRead(ln) || !ok || o.Res != 24*time.Hour || o.Zone != models.ZoneApp {
		t.Fatalf("learned %s %+v (%s); want a plain read of the clock cut to a day in the application's zone", ln.Kind, o, ln.Why)
	}
	for h := 0; h < 24; h++ {
		from := time.Date(2026, 9, 7, h, 10, 0, 0, santiago)
		s := Span{From: from, To: from.Add(time.Minute), Zone: santiago}
		for _, row := range []struct {
			v    string
			want bool
		}{{"2026-09-07", true}, {"2026-09-06", true}, {"2026-09-05", false}} {
			if ok, why := CheckClockRead(p, o, text(row.v), s, true); ok != row.want {
				t.Errorf("%s for a span from %s: %v (%s), want %v", row.v, from.Format(time.RFC3339), ok, why, row.want)
			}
		}
	}
}
