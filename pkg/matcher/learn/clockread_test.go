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
	ln := ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: typed(sec.Add(61 * time.Second))}}, Shape{Leaves: map[string]Leaf{p: typed(sec)}}, sec.Add(time.Millisecond), nil, nil, nil, nil)[0]
	o, ok := ln.Clock()
	if ln.Kind != KindClockRead || !ok || o.Res < time.Second {
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
		{"a typed time of day, which names no day", TimeOfDayLeaf(path, path, 12*time.Hour, time.Second)},
	} {
		if ok, why := CheckClockRead(path, plain, c.live, s, true); ok || !strings.Contains(why, "no instant keploy reads") {
			t.Errorf("%s: %v %q; want refused as no instant keploy reads", c.what, ok, why)
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
// names its zone where the learn step read a wall clock of none is refused
// as changed. One its window holds in UTC is read in UTC
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
			{"the replay's now, the application's zone not told", form.leaf(ist)(now.Add(5 * time.Millisecond)), Span{From: span.From, To: span.To}, false, "keploy cannot tell the application's zone"},
		} {
			ok, why := CheckClockRead(p, o, row.live, row.s, true)
			if ok != row.want || !strings.Contains(why, row.why) || !ok && !strings.HasPrefix(why, p+" was learned as clock, a wall clock of no zone in the application's zone") {
				t.Errorf("%s, %s: %v %q; want %v naming %q", form.what, row.what, ok, why, row.want, row.why)
			}
			if got := ClockReadHolds(o, row.live, row.s); got != row.want {
				t.Errorf("%s, %s: ClockReadHolds %v; want %v", form.what, row.what, got, row.want)
			}
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
	if ok, why := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Zone: models.ZoneApp}, named, span, true); ok || !strings.Contains(why, "names its zone") {
		t.Errorf("a value that names its zone, learned as a wall clock of none: %v %q; want refused, saying it names its zone", ok, why)
	}
	if ok, why := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond, Zone: "pst"}, text(ist)(now.Add(5*time.Millisecond)), span, true); ok || !strings.Contains(why, "rather than read it in another zone. Record the test again with this keploy") {
		t.Errorf("a zone this keploy does not read: %v %q; want refused whatever it carries, saying so", ok, why)
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

// A time of day its protocol types (a Postgres time) that moved by the lead
// stays noise with no read of the clock: it names no day, so no call's span
// holds it and no replay check reads one (clockReadOf). A stated limit,
// never a read a replay refuses whatever it carries.
func TestATimeOfDayTheLeadMovedIsNoClockReadAReplayChecks(t *testing.T) {
	call := time.Date(2026, 10, 27, 3, 30, 0, 500e6, time.UTC)
	read := call.Add(-5 * time.Millisecond)
	const p = "body.cmd.filter.at"
	tod := func(at time.Time) Leaf { return TimeOfDayLeaf(p, p, time.Duration(clockOfDay(at)), time.Microsecond) }
	ln := classifyZoned(nil, false, p, tod(read), tod(read.Add(FullRoundLead+37*time.Millisecond)), call, nil)
	if _, ok := ln.Clock(); ln.Kind != KindNoise || ok {
		t.Errorf("a time of day the lead moved: %s Read=%+v (%s); want noise with no read of the clock", ln.Kind, ln.Read, ln.Why)
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
// pass at one hour and a refusal at another; a date two days off is refused
// at every hour, and one a day off passes only where it is the other zone's
// date (the stated limit). For UTC, IST and EDT applications, writing their
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
							s := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: app.zone}
							at := now.Add(5 * time.Millisecond)
							if ok, why := CheckClockRead(p, o, form.leaf(at, write), s, true); !ok {
								t.Errorf("%s: the application's own date at %02d:10Z refused (%s)", what, rh, why)
							}
							if ok, _ := CheckClockRead(p, o, form.leaf(at.Add(48*time.Hour), write), s, true); ok {
								t.Errorf("%s: a date two days off at %02d:10Z passed", what, rh)
							}
							other := day(at, app.zone)
							if write == app.zone {
								other = day(at, time.UTC)
							}
							off := form.leaf(at.Add(24*time.Hour), write)
							passes := o.Zone == models.ZoneEither && day(at.Add(24*time.Hour), write).Equal(other)
							if ok, _ := CheckClockRead(p, o, off, s, true); ok != passes {
								t.Errorf("%s: a date a day off at %02d:10Z: passed %v; want %v (only where it is the other zone's date, under either zone)", what, rh, ok, passes)
							}
						}
						if o.Zone == models.ZoneEither {
							now := time.Date(2026, 11, 3, 12, 10, 0, 0, time.UTC)
							if ok, why := CheckClockRead(p, o, form.leaf(now, write), Span{From: now, To: now.Add(20 * time.Millisecond)}, true); ok || !strings.Contains(why, "keploy cannot tell the application's zone") {
								t.Errorf("%s: the application's zone not told at replay: %v %q; want refused, saying so", what, ok, why)
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
