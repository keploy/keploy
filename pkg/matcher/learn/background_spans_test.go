package learn

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// shown is what c showed between the real instants from and to
// (Tracker.SetAppClockShown): a span per offset in force at some instant
// between them, the real clock before the first write, each over the real
// stretch it was in force, a write counted in both offsets from its begin
// to its end.
func (c *runClock) shown(from, to time.Time) ([]Span, bool) {
	var out []Span
	add := func(start, end time.Time, off time.Duration) {
		if start.Before(from) {
			start = from
		}
		if end.IsZero() || end.After(to) {
			end = to
		}
		if !start.After(end) {
			out = append(out, Span{From: start.Add(off), To: end.Add(off)})
		}
	}
	var start time.Time
	off := time.Duration(0)
	for _, w := range c.writes {
		add(start, w.to, off)
		start, off = w.from, w.offset
	}
	add(start, time.Time{}, off)
	return out, true
}

// A background poller reads the app's clock for its call, a test's begin
// moves the clock, and the call reaches keploy after the move: its span to
// arrival starts at the move (SetAppClockRun), which no read of the call's
// was made after, so its read holds on no span of it. The background
// check's spans (CallSpansOf) are that span and, before it, what the clock
// showed from the previous serve to the move: the poll's read holds in the
// earlier one. A value the clock never showed (one the move jumped over)
// and one from before the previous serve hold in none. A call that reaches
// keploy while the move is written holds on both offsets, to arrival.
// Mutation: tell no spans mid-move; tell them where the hooks cannot tell
// what the clock showed, or tell none.
func TestABackgroundPollThatReadTheClockBeforeAMoveHoldsOnWhatTheClockShowedBeforeIt(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 3 * time.Second // the test's begin moves the clock to its recorded instant
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{move}}
	arrival := move.to.Add(15 * time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(time.Second))
	ctx := WithTracker(context.Background(), tr)

	stamp := EpochOffset{Res: time.Second} // a stamp of now, in epoch seconds
	epoch := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	read := epoch(base.Add(1900 * time.Millisecond)) // read before the move, on the clock before it

	last, ok := CallSpanOf(ctx, arrival)
	if !ok {
		t.Fatal("no span to arrival")
	}
	if want := (Span{From: move.to.Add(ahead), To: arrival.Add(ahead)}); last.From != want.From || last.To != want.To {
		t.Fatalf("span = %v..%v; want %v..%v: from the move", last.From, last.To, want.From, want.To)
	}
	if stamp.Holds(read, last) {
		t.Fatal("the read before the move holds on the span from the move; the row tests nothing")
	}
	spans, ok := CallSpansOf(ctx, arrival)
	if !ok || len(spans) != 1 || spans[0] != last {
		t.Fatalf("spans = %+v,%v with no shown clock told; want the span from the move alone", spans, ok)
	}

	tr.SetAppClockShown(clock.shown)
	spans, ok = CallSpansOf(ctx, arrival)
	if !ok || len(spans) != 3 {
		t.Fatalf("spans = %+v,%v; want what the clock showed before the move, through its write in both offsets, then the span from it", spans, ok)
	}
	if want := (Span{From: base.Add(time.Second), To: move.to}); spans[0].From != want.From || spans[0].To != want.To {
		t.Fatalf("first span = %v..%v; want %v..%v: the clock before the move, from the previous serve to the move's end", spans[0].From, spans[0].To, want.From, want.To)
	}
	if spans[2] != last {
		t.Fatalf("last span = %+v; want CallSpanOf's %+v", spans[2], last)
	}
	if !stamp.HoldsIn(read, spans) {
		t.Fatalf("the read before the move holds in none of %+v", spans)
	}
	if v := epoch(base.Add(4 * time.Second)); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, a value the move jumped over, holds in %+v", v, spans)
	}
	if v := epoch(base.Add(500 * time.Millisecond)); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read before the previous serve, holds in %+v", v, spans)
	}

	// Hooks that cannot tell what the clock showed leave the span from the
	// move alone; so does a call whose clock did not move since its bound.
	tr.SetAppClockShown(func(from, to time.Time) ([]Span, bool) { return []Span{{From: from, To: to}}, false })
	if spans, ok := CallSpansOf(ctx, arrival); !ok || len(spans) != 1 || spans[0] != last {
		t.Fatalf("spans = %+v,%v with the shown clock untold; want the span from the move alone", spans, ok)
	}
	asked := false
	tr.SetAppClockShown(func(from, to time.Time) ([]Span, bool) { asked = true; return clock.shown(from, to) })
	tr.ServedMockAt(&models.Mock{Name: "poll-2", Kind: models.Mongo}, move.to.Add(time.Millisecond))
	if spans, ok := CallSpansOf(ctx, arrival); !ok || len(spans) != 1 || asked {
		t.Fatalf("spans = %+v,%v (shown asked: %v) for a call served after the move; want its one span, nothing shown asked", spans, ok, asked)
	}
	// A call that reaches keploy while a move is written has no span to
	// arrival (where the clock's run began cannot be told), and holds on
	// both offsets: what the clock showed from the previous serve, before
	// the move, and what it showed from the move's begin, both to arrival.
	// A value the move jumped over still holds in none.
	mid := move.from.Add(time.Millisecond)
	if _, ok := CallSpanOf(ctx, mid); ok {
		t.Fatal("a call that arrived mid-move has a span to arrival; the row tests nothing")
	}
	spans, ok = CallSpansOf(ctx, mid)
	if !ok || len(spans) != 2 {
		t.Fatalf("spans = %+v,%v for a call that arrived mid-move; want the clock before the move and from its begin", spans, ok)
	}
	if want := (Span{From: base.Add(time.Second), To: mid}); spans[0].From != want.From || spans[0].To != want.To {
		t.Fatalf("first mid-move span = %v..%v; want %v..%v: the clock before the move, from the previous serve to arrival", spans[0].From, spans[0].To, want.From, want.To)
	}
	if want := (Span{From: move.from.Add(ahead), To: mid.Add(ahead)}); spans[1].From != want.From || spans[1].To != want.To {
		t.Fatalf("second mid-move span = %v..%v; want %v..%v: the clock the move writes, from its begin to arrival", spans[1].From, spans[1].To, want.From, want.To)
	}
	if !stamp.HoldsIn(read, spans) {
		t.Fatalf("the read before the move holds in none of the mid-move spans %+v", spans)
	}
	if v := epoch(move.from.Add(ahead)); !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read on the offset the move writes, holds in none of %+v", v, spans)
	}
	if v := epoch(base.Add(4 * time.Second)); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, a value the move jumped over, holds in the mid-move spans %+v", v, spans)
	}
	// Hooks that cannot tell what the clock showed mid-move (a write that
	// failed part way) leave the call with no span.
	tr.SetAppClockShown(func(from, to time.Time) ([]Span, bool) { return []Span{{From: from, To: to}}, false })
	if spans, ok := CallSpansOf(ctx, mid); ok || spans != nil {
		t.Fatalf("spans = %+v,%v mid-move with the shown clock untold; want none", spans, ok)
	}
	tr.SetAppClockShown(nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("mid-move with no shown clock CallSpansOf panicked: %v", r)
			}
		}()
		if spans, ok := CallSpansOf(ctx, mid); ok || spans != nil {
			t.Fatalf("spans = %+v,%v mid-move with no shown clock; want none", spans, ok)
		}
	}()
	// Each span is in the zone of the process that made the call.
	ist := time.FixedZone("IST", 5*3600+1800)
	zoned := NewTracker()
	zoned.SetAppClock(clock.at)
	zoned.SetAppClockRun(clock.began)
	zoned.SetAppClockShown(clock.shown)
	zoned.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(time.Second))
	spans, ok = CallSpansOf(WithProcZone(WithTracker(context.Background(), zoned), ProcZone{Loc: ist, Name: "+05:30"}), arrival)
	if !ok || len(spans) != 3 {
		t.Fatalf("spans = %+v,%v; want three", spans, ok)
	}
	for _, s := range spans {
		if s.Zone != ist || !s.Own {
			t.Fatalf("span %+v is not in the process's zone", s)
		}
	}
}

// A call keploy cannot place after any serve of its set has no span, and
// no earlier one: in a client test before any serve the span is read from
// the test's begin, and no bound tells what the clock showed before it;
// mid-move nothing does. With the proven switch off the serve clock tells
// the call's one span. Mutation: read what the clock showed from no bound.
func TestABackgroundCallWithNoBoundHasNoEarlierSpans(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: 3 * time.Second}
	clock := &runClock{writes: []runWrite{move}}
	arrival := move.to.Add(15 * time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	asked := false
	tr.SetAppClockShown(func(from, to time.Time) ([]Span, bool) { asked = true; return clock.shown(from, to) })
	tr.SetClientTests(true)
	tr.BeginAt(move.to.Add(time.Millisecond))
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival)
	if !ok || len(spans) != 1 || asked {
		t.Fatalf("spans = %+v,%v (shown asked: %v); want the span from the test's begin alone", spans, ok, asked)
	}
	// Mid-move, with no serve to bound it, nothing tells what the clock
	// showed: the shown clock is not asked from no bound.
	if spans, ok := CallSpansOf(WithTracker(context.Background(), tr), move.from.Add(time.Millisecond)); ok || spans != nil || asked {
		t.Fatalf("spans = %+v,%v (shown asked: %v) mid-move with no bound; want none", spans, ok, asked)
	}

	sc := &ServeClock{}
	sc.SetClock(func(r time.Time) (time.Time, bool) { return r, true })
	sctx := WithServeClock(context.Background(), sc)
	if spans, ok := CallSpansOf(sctx, arrival); ok || spans != nil {
		t.Fatalf("spans = %+v,%v with no serve noted; want none", spans, ok)
	}
	sc.Served()
	if spans, ok := CallSpansOf(sctx, time.Now().Add(time.Second)); !ok || len(spans) != 1 {
		t.Fatalf("spans = %+v,%v after a serve; want its one span", spans, ok)
	}
	if spans, ok := CallSpansOf(nil, arrival); ok || spans != nil { //nolint:staticcheck // a nil ctx tells none
		t.Fatalf("spans = %+v,%v of a nil ctx; want none", spans, ok)
	}
}

// Between two tests the clock moves twice with no serve between (the
// earlier test's end, then the next's begin): a poll that read the clock
// between the two moves and reached keploy after the second holds in what
// the clock showed between them, and nowhere else.
func TestABackgroundPollReadBetweenTwoMovesHoldsOnTheClockBetweenThem(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	end := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + time.Millisecond), offset: -time.Hour}
	begin := runWrite{from: base.Add(5 * time.Second), to: base.Add(5*time.Second + time.Millisecond), offset: 10 * time.Second}
	clock := &runClock{writes: []runWrite{end, begin}}
	arrival := begin.to.Add(10 * time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(time.Second))
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival)
	if !ok || len(spans) != 4 {
		t.Fatalf("spans = %+v,%v; want the clock before, between and through the two moves, then the span from the last", spans, ok)
	}
	stamp := EpochOffset{Res: time.Second}
	between := base.Add(3 * time.Second).Add(-time.Hour)
	if v := strconv.FormatInt(between.Unix(), 10); !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read between the moves, holds in none of %+v", v, spans)
	}
	if v := strconv.FormatInt(base.Add(3*time.Second).Unix(), 10); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, the clock before the moves at an instant it had moved from, holds in %+v", v, spans)
	}
}

// Every span of a background call is in the app's zone where the tracker
// was told it (SetAppZone), and stays there where the agent read no zone
// of the call's own process (a ProcZone with no Loc), for a call that
// arrives after a move and for one that arrives while the move is written
// (the spans the hooks told, which carry no zone of their own). A nil
// tracker tells none and takes a shown clock without a panic.
func TestBackgroundSpansKeepTheAppsZoneAndANilTrackerTellsNone(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: 3 * time.Second}
	clock := &runClock{writes: []runWrite{move}}
	arrival := move.to.Add(15 * time.Millisecond)
	ist := time.FixedZone("IST", 5*3600+1800)
	tr := NewTracker()
	tr.SetAppZone(ist)
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(time.Second))
	ctx := WithTracker(context.Background(), tr)
	calls := []struct {
		what    string
		arrival time.Time
		spans   int
	}{
		{"after the move", arrival, 3},
		{"mid-move", move.from.Add(time.Millisecond), 2},
	}
	for name, c := range map[string]context.Context{"no process zone": ctx, "a process zone with no Loc": WithProcZone(ctx, ProcZone{Name: "unread"})} {
		for _, call := range calls {
			spans, ok := CallSpansOf(c, call.arrival)
			if !ok || len(spans) != call.spans {
				t.Fatalf("%s, %s: spans = %+v,%v; want %d", name, call.what, spans, ok, call.spans)
			}
			for _, s := range spans {
				if s.Zone != ist || s.Own {
					t.Fatalf("%s, %s: span %+v is not in the app's zone", name, call.what, s)
				}
			}
		}
	}

	var nilTracker *Tracker
	noPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s panicked: %v", what, r)
			}
		}()
		f()
	}
	noPanic("SetAppClockShown", func() { nilTracker.SetAppClockShown(clock.shown) })
	noPanic("BackgroundSpans", func() {
		if spans, ok := nilTracker.BackgroundSpans(arrival); ok || spans != nil {
			t.Fatalf("spans = %+v,%v of a nil tracker; want none", spans, ok)
		}
	})
	noPanic("CallSpansOf", func() {
		if spans, ok := CallSpansOf(context.Background(), arrival); ok || spans != nil {
			t.Fatalf("spans = %+v,%v of a ctx with no tracker; want none", spans, ok)
		}
	})
	noPanic("ClockSpansOf of a nil ctx", func() {
		if spans, ok := ClockSpansOf(nil, arrival); ok || spans != nil { //nolint:staticcheck // a nil ctx tells none
			t.Fatalf("spans = %+v,%v of a nil ctx; want none", spans, ok)
		}
	})
}
