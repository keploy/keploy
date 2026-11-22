package learn

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// epochSecond is at as epoch seconds, as a poll's filter carries it.
func epochSecond(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

// newSetAt notes on c a test set begun at the real instant at
// (ServeClock.NewSet), by the serve clock's clock, set back once the test
// ends.
func newSetAt(t *testing.T, c *ServeClock, at time.Time) {
	t.Helper()
	prev := serveNow
	t.Cleanup(func() { serveNow = prev })
	serveNow = func() time.Time { return at }
	c.NewSet()
}

// servedAt notes on c a serve made at the real instant at (ServeClock.
// Served), by the serve clock's clock, set back once the test ends.
func servedAt(t *testing.T, c *ServeClock, at time.Time) {
	t.Helper()
	prev := serveNow
	t.Cleanup(func() { serveNow = prev })
	serveNow = func() time.Time { return at }
	c.Served()
}

// A background poll reads the app's clock just before a second boundary,
// the open test's own call is served just after it, and the poll reaches
// keploy 16 ms later, with no move of the clock between: the poll's read is
// ordered with no call keploy served, so its spans start at the set's
// begin and its read holds, under the proven switch (the learn tracker) and
// with it off (the serve clock). A span from the test's call, the latest
// serve of any flow, starts after the read and holds it in none. A value
// from before the set began holds in none. Mutation: bound the spans by a
// serve.
func TestABackgroundPollIsBoundedByTheSetsBeginNotByAServe(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 51 * time.Second
	early := runWrite{from: base, to: base.Add(5 * time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{early}}
	setBegan, read := base.Add(time.Second), base.Add(1990*time.Millisecond)
	prevPoll, testCall, arrival := base.Add(1900*time.Millisecond), base.Add(2001*time.Millisecond), base.Add(2017*time.Millisecond)
	stamp := EpochOffset{Res: time.Second}
	v := epochSecond(read.Add(ahead))
	beforeSet := epochSecond(base.Add(500 * time.Millisecond).Add(ahead))

	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.NewSetAt(setBegan)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, prevPoll)
	tr.ServedMockAt(&models.Mock{Name: "test-own-find", Kind: models.Mongo}, testCall)
	ctx := WithTracker(context.Background(), tr)
	if last, ok := CallSpanOf(ctx, arrival); !ok || stamp.Holds(v, last) {
		t.Fatalf("span = %+v,%v from the test's call holds %s; the row tests nothing", last, ok, v)
	}
	spans, ok := CallSpansOf(ctx, arrival)
	if !ok || len(spans) != 1 {
		t.Fatalf("spans = %+v,%v; want the one span from the set's begin", spans, ok)
	}
	if want := (Span{From: setBegan.Add(ahead), To: arrival.Add(ahead)}); spans[0].From != want.From || spans[0].To != want.To {
		t.Fatalf("span = %v..%v; want %v..%v: from the set's begin", spans[0].From, spans[0].To, want.From, want.To)
	}
	if !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read before the test's call was served, holds in none of %+v", v, spans)
	}
	if stamp.HoldsIn(beforeSet, spans) {
		t.Fatalf("%s, read before the set began, holds in %+v", beforeSet, spans)
	}

	sc := &ServeClock{}
	sc.SetClock(func(r time.Time) (time.Time, bool) { return r.Add(ahead), true })
	newSetAt(t, sc, setBegan)
	servedAt(t, sc, prevPoll)
	servedAt(t, sc, testCall)
	if last, ok := sc.SpanTo(arrival); !ok || stamp.Holds(v, last) {
		t.Fatalf("serve clock span = %+v,%v from the test's call holds %s; the row tests nothing", last, ok, v)
	}
	spans, ok = CallSpansOf(WithServeClock(context.Background(), sc), arrival)
	if !ok || len(spans) != 1 || spans[0].From != setBegan.Add(ahead) || spans[0].To != arrival.Add(ahead) || !stamp.HoldsIn(v, spans) || stamp.HoldsIn(beforeSet, spans) {
		t.Fatalf("serve clock spans = %+v,%v; want the one from the set's begin, holding %s and not %s", spans, ok, v, beforeSet)
	}
}

// Two pollers run one poll (a worker pool claiming jobs with one
// findAndModify shape): poller A served its previous run, then read its
// clock at :01.995; poller B's run of the same poll reached keploy at
// :02.001 and was served; A reached keploy at :02.010. No move. A's read is
// ordered with none of B's serves, and keploy cannot tell the two apart:
// A's read holds, under the proven switch (the learn tracker) and with it
// off (the serve clock), as it would not on a span from B's serve.
// Mutation: bound the spans by the latest serve of the poll's own
// recordings.
func TestTwoPollersOfOnePollHoldEachReadOnTheSetsBegin(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 51 * time.Second
	early := runWrite{from: base, to: base.Add(5 * time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{early}}
	aPrev := base.Add(1500 * time.Millisecond)
	aRead := base.Add(1995 * time.Millisecond)
	bArr := base.Add(2001 * time.Millisecond)
	aArr := base.Add(2010 * time.Millisecond)
	stamp := EpochOffset{Res: time.Second}
	v := epochSecond(aRead.Add(ahead))
	fromB := Span{From: bArr.Add(ahead), To: aArr.Add(ahead)}
	if stamp.Holds(v, fromB) {
		t.Fatalf("%s holds on the span from B's serve %+v; the row tests nothing", v, fromB)
	}

	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.NewSetAt(base)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, aPrev)
	tr.ServedMockAt(&models.Mock{Name: "poll-2", Kind: models.Mongo}, bArr)
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), aArr)
	if !ok || !stamp.HoldsIn(v, spans) {
		t.Fatalf("poller A's read %s holds in none of %+v (ok=%v): bounded by poller B's serve of the same poll", v, spans, ok)
	}

	sc := &ServeClock{}
	sc.SetClock(func(r time.Time) (time.Time, bool) { return r.Add(ahead), true })
	newSetAt(t, sc, base)
	servedAt(t, sc, aPrev)
	servedAt(t, sc, bArr)
	spans, ok = CallSpansOf(WithServeClock(context.Background(), sc), aArr)
	if !ok || !stamp.HoldsIn(v, spans) {
		t.Fatalf("serve clock: poller A's read %s holds in none of %+v (ok=%v)", v, spans, ok)
	}
}

// The test's begin moves the clock forward while a poll's read is in
// flight, and the test's own call is served after the move and before the
// poll reaches keploy: the poll's spans run from the set's begin through
// what the clock showed before the move, and its read before the move
// holds.
func TestABackgroundPollReadBeforeAMoveHoldsWithTheTestsCallServedAfterIt(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 3 * time.Second
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{move}}
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.NewSetAt(base.Add(time.Second))
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(1995*time.Millisecond))
	tr.ServedMockAt(&models.Mock{Name: "test-own-find", Kind: models.Mongo}, move.to.Add(2*time.Millisecond))
	arrival := move.to.Add(20 * time.Millisecond)
	v := epochSecond(base.Add(1990 * time.Millisecond)) // the clock before the move: the real clock
	stamp := EpochOffset{Res: time.Second}
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival)
	if !ok || len(spans) != 3 || !stamp.HoldsIn(v, spans) {
		t.Fatalf("spans = %+v,%v; want three, the read before the move (%s) holding in one", spans, ok, v)
	}
}

// A background call is bounded by the set's begin: what the clock showed
// from it to the move the test's begin wrote holds a poll read before that
// move, whether the call arrives after the move or while it is written, in
// a server test and in a client test (whose own begin bounds no poller's
// read), with the open test's call served between. A value from before the
// set's begin holds in none. Mutation: read the earlier spans or the
// mid-move ones from a serve, or a client test's from its begin.
func TestABackgroundCallIsBoundedByTheSetsBegin(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 3 * time.Second
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{move}}
	stamp := EpochOffset{Res: time.Second}
	read := epochSecond(base.Add(1500 * time.Millisecond))
	beforeSet := epochSecond(base)
	for _, client := range []bool{false, true} {
		tr := NewTracker()
		tr.SetAppClock(clock.at)
		tr.SetAppClockRun(clock.began)
		tr.SetAppClockShown(clock.shown)
		tr.SetClientTests(client)
		tr.NewSetAt(base.Add(time.Second))
		tr.BeginAt(move.to.Add(time.Millisecond))
		tr.ServedMockAt(&models.Mock{Name: "test-own-find", Kind: models.Mongo}, move.to.Add(2*time.Millisecond))
		ctx := WithTracker(context.Background(), tr)
		for _, call := range []struct {
			what    string
			arrival time.Time
			spans   int
		}{
			{"after the move", move.to.Add(20 * time.Millisecond), 3},
			{"mid-move", move.from.Add(time.Millisecond), 2},
		} {
			spans, ok := CallSpansOf(ctx, call.arrival)
			if !ok || len(spans) != call.spans {
				t.Fatalf("client %v, %s: spans = %+v,%v; want %d", client, call.what, spans, ok, call.spans)
			}
			if want := base.Add(time.Second); spans[0].From != want {
				t.Fatalf("client %v, %s: first span from %v; want the set's begin %v", client, call.what, spans[0].From, want)
			}
			if !stamp.HoldsIn(read, spans) {
				t.Fatalf("client %v, %s: %s, read after the set began and before the move, holds in none of %+v", client, call.what, read, spans)
			}
			if stamp.HoldsIn(beforeSet, spans) {
				t.Fatalf("client %v, %s: %s, from before the set began, holds in %+v", client, call.what, beforeSet, spans)
			}
		}
	}
}

// No span is told where no bound or clock is known, under the proven
// switch: hooks that tell no app clock (no panic), hooks that tell no run
// of it (the clock may have moved since the set's begin), and a call that
// reached keploy before the running set began (a call of the set before,
// checked late), though the hooks would tell what the clock showed there.
// No span holds no read. Mutation: drop the no-clock guard, the no-run
// bound, or the late-call refusal.
func TestABackgroundCallWithNoClockRunOrSetBeforeItHasNoSpans(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	arrival := base.Add(2 * time.Second)
	allShown := func(from, to time.Time) ([]Span, bool) { return []Span{{From: from, To: to}}, true }
	noPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s panicked: %v", what, r)
			}
		}()
		f()
	}
	for _, c := range []struct {
		what  string
		clock bool
		run   bool
		at    time.Time // the call's arrival
	}{
		{"no app clock", false, true, arrival},
		{"no run of the clock told", true, false, arrival},
		{"a call before the set began", true, true, base.Add(500 * time.Millisecond)},
	} {
		tr := NewTracker()
		if c.clock {
			tr.SetAppClock(realClock)
		}
		if c.run {
			tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
		}
		tr.SetAppClockShown(allShown)
		tr.NewSetAt(base.Add(time.Second))
		tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(1500*time.Millisecond))
		noPanic(c.what, func() {
			if spans, ok := CallSpansOf(WithTracker(context.Background(), tr), c.at); ok || spans != nil {
				t.Fatalf("%s: spans = %+v,%v; want none", c.what, spans, ok)
			}
		})
	}
	tr := NewTracker()
	tr.SetAppClock(realClock)
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.NewSetAt(base.Add(time.Second))
	if spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival); !ok || len(spans) != 1 {
		t.Fatalf("spans = %+v,%v with a clock, its run and a set begun; want one: the row tests nothing", spans, ok)
	}
	if (EpochOffset{Res: time.Second}).HoldsIn(epochSecond(base), nil) {
		t.Fatal("a read holds in no span")
	}
}

// With the proven switch off the serve clock starts a background call's
// span at the running set's begin (the latest NewSet), never at a serve,
// where the app's clock ran as one since; none where it moved since the
// set's begin, before any set began, for a call that reached keploy before
// the set began, with no clock, and of a nil serve clock. Mutation: start
// at a serve; keep an earlier set's begin; drop the no-clock guard.
func TestTheServeClockBoundsABackgroundCallByTheSetsBegin(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	stamp := EpochOffset{Res: time.Second}
	moved := base.Add(1500 * time.Millisecond)
	movingClock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r, true
		}
		return r.Add(time.Hour), true
	}
	arrival := base.Add(2017 * time.Millisecond)

	sc := &ServeClock{}
	sc.SetClock(realClock)
	servedAt(t, sc, base.Add(100*time.Millisecond))
	if spans, ok := sc.SpansOf(arrival); ok || spans != nil {
		t.Fatalf("spans = %+v,%v with a serve and no set begun; want none", spans, ok)
	}
	newSetAt(t, sc, base.Add(200*time.Millisecond))
	newSetAt(t, sc, base.Add(time.Second))
	servedAt(t, sc, base.Add(1900*time.Millisecond))
	servedAt(t, sc, base.Add(2001*time.Millisecond))
	spans, ok := sc.SpansOf(arrival)
	if !ok || len(spans) != 1 || spans[0].From != base.Add(time.Second) || spans[0].To != arrival {
		t.Fatalf("spans = %+v,%v; want the one from the running set's begin %v to %v", spans, ok, base.Add(time.Second), arrival)
	}
	if v := epochSecond(base.Add(1100 * time.Millisecond)); !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read after the set began and before any serve of it, holds in none of %+v", v, spans)
	}
	if v := epochSecond(base.Add(500 * time.Millisecond)); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read in the earlier set, holds in %+v", v, spans)
	}
	if spans, ok := sc.SpansOf(base.Add(900 * time.Millisecond)); ok || spans != nil {
		t.Fatalf("spans = %+v,%v of a call before the set began; want none", spans, ok)
	}
	sc.SetClock(movingClock)
	if spans, ok := sc.SpansOf(arrival); ok || spans != nil {
		t.Fatalf("spans = %+v,%v where the clock moved since the set's begin; want none", spans, ok)
	}
	sc.SetClock(nil)
	if spans, ok := sc.SpansOf(arrival); ok || spans != nil {
		t.Fatalf("spans = %+v,%v with no clock; want none", spans, ok)
	}
	var nilClock *ServeClock
	nilClock.NewSet()
	nilClock.NewSetAt(base)
	if spans, ok := nilClock.SpansOf(arrival); ok || spans != nil {
		t.Fatalf("spans = %+v,%v of a nil serve clock; want none", spans, ok)
	}
}
