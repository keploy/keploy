package learn

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// bgPolls names the background group of the rows' poll: the recordings
// named poll-N, the poll's earlier runs.
func bgPolls(name string) bool { return strings.HasPrefix(name, "poll-") }

// epochSecond is at as epoch seconds, as a poll's filter carries it.
func epochSecond(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

// serveMockAt notes on c a serve of the recording name at the real instant
// at (ServeClock.ServedMock), by the serve clock's clock, set back once the
// test ends.
func serveMockAt(t *testing.T, c *ServeClock, name string, at time.Time) {
	t.Helper()
	prev := serveNow
	t.Cleanup(func() { serveNow = prev })
	serveNow = func() time.Time { return at }
	c.ServedMock(name)
}

// A background poll reads the app's clock just before a second boundary,
// the open test's own call is served just after it, and the poll reaches
// keploy 16 ms later, with no move of the clock between: the poll's read is
// ordered with its own previous run, not with the test's call, so its
// spans start at its group's previous serve and its read holds, under the
// proven switch (the learn tracker) and with it off (the serve clock). A
// span from the test's call, the latest serve of any flow, starts after
// the read and holds it in none. Mutation: bound the spans by the latest
// serve of any flow, or of a recording not of the group.
func TestABackgroundPollIsBoundedByItsOwnGroupsServeNotByAnotherFlows(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 51 * time.Second
	early := runWrite{from: base, to: base.Add(5 * time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{early}}
	prevPoll, read := base.Add(1900*time.Millisecond), base.Add(1990*time.Millisecond)
	testCall, arrival := base.Add(2001*time.Millisecond), base.Add(2017*time.Millisecond)
	stamp := EpochOffset{Res: time.Second}
	v := epochSecond(read.Add(ahead))

	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, prevPoll)
	tr.ServedMockAt(&models.Mock{Name: "test-own-find", Kind: models.Mongo}, testCall)
	ctx := WithTracker(context.Background(), tr)
	if last, ok := CallSpanOf(ctx, arrival); !ok || stamp.Holds(v, last) {
		t.Fatalf("span = %+v,%v from the test's call holds %s; the row tests nothing", last, ok, v)
	}
	spans, ok := CallSpansOf(ctx, arrival, bgPolls)
	if !ok || len(spans) != 1 {
		t.Fatalf("spans = %+v,%v; want the one span from the poll's previous run", spans, ok)
	}
	if want := (Span{From: prevPoll.Add(ahead), To: arrival.Add(ahead)}); spans[0].From != want.From || spans[0].To != want.To {
		t.Fatalf("span = %v..%v; want %v..%v: from the group's previous serve", spans[0].From, spans[0].To, want.From, want.To)
	}
	if !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read before the test's call was served, holds in none of %+v", v, spans)
	}
	if v := epochSecond(base.Add(500 * time.Millisecond).Add(ahead)); stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read before the poll's previous run, holds in %+v", v, spans)
	}
	if spans, ok := CallSpansOf(ctx, arrival, func(name string) bool { return name == "test-own-find" }); !ok || stamp.HoldsIn(v, spans) {
		t.Fatalf("spans = %+v,%v of a group the test's call is of hold %s; want them from that call, holding it in none", spans, ok, v)
	}

	sc := &ServeClock{}
	sc.SetClock(func(r time.Time) (time.Time, bool) { return r.Add(ahead), true })
	serveMockAt(t, sc, "poll-1", prevPoll)
	serveMockAt(t, sc, "test-own-find", testCall)
	sctx := WithServeClock(context.Background(), sc)
	if last, ok := sc.SpanTo(arrival); !ok || stamp.Holds(v, last) {
		t.Fatalf("serve clock span = %+v,%v from the test's call holds %s; the row tests nothing", last, ok, v)
	}
	spans, ok = CallSpansOf(sctx, arrival, bgPolls)
	if !ok || len(spans) != 1 || spans[0].From != prevPoll.Add(ahead) || !stamp.HoldsIn(v, spans) {
		t.Fatalf("serve clock spans = %+v,%v; want the one from the poll's previous run, holding %s", spans, ok, v)
	}
}

// A serve of the poll's own group made at the very instant the poll reached
// keploy (another poller's run of the same work, answered as this one
// arrived) is not made before the poll's arrival, so the poll read its clock
// after no such serve: the bound is the group's serve before it, under the
// proven switch (the learn tracker) and with it off (the serve clock), and
// the poll's read between the two holds. Mutation: keep a serve made at
// arrival as the bound.
func TestABackgroundPollsGroupServedAtItsArrivalIsNotItsBound(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 51 * time.Second
	early := runWrite{from: base, to: base.Add(5 * time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{early}}
	prevPoll, read, arrival := base.Add(1900*time.Millisecond), base.Add(1990*time.Millisecond), base.Add(2017*time.Millisecond)
	stamp := EpochOffset{Res: time.Second}
	v := epochSecond(read.Add(ahead))

	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, prevPoll)
	tr.ServedMockAt(&models.Mock{Name: "poll-2", Kind: models.Mongo}, arrival)
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival, bgPolls)
	if !ok || len(spans) != 1 || spans[0].From != prevPoll.Add(ahead) || spans[0].To != arrival.Add(ahead) {
		t.Fatalf("spans = %+v,%v; want the one from the group's serve before arrival, %v..%v", spans, ok, prevPoll.Add(ahead), arrival.Add(ahead))
	}
	if !stamp.HoldsIn(v, spans) {
		t.Fatalf("%s, read after the group's serve before arrival, holds in none of %+v", v, spans)
	}

	sc := &ServeClock{}
	sc.SetClock(func(r time.Time) (time.Time, bool) { return r.Add(ahead), true })
	serveMockAt(t, sc, "poll-1", prevPoll)
	serveMockAt(t, sc, "poll-2", arrival)
	spans, ok = CallSpansOf(WithServeClock(context.Background(), sc), arrival, bgPolls)
	if !ok || len(spans) != 1 || spans[0].From != prevPoll.Add(ahead) || !stamp.HoldsIn(v, spans) {
		t.Fatalf("serve clock spans = %+v,%v; want the one from the group's serve before arrival, holding %s", spans, ok, v)
	}
}

// The test's begin moves the clock forward while a poll's read is in
// flight, and the test's own call is served after the move and before the
// poll reaches keploy: the poll's spans run from its group's previous serve
// through what the clock showed before the move, and its read before the
// move holds.
func TestABackgroundPollReadBeforeAMoveHoldsWithTheTestsCallServedAfterIt(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ahead := 3 * time.Second
	move := runWrite{from: base.Add(2 * time.Second), to: base.Add(2*time.Second + 5*time.Millisecond), offset: ahead}
	clock := &runClock{writes: []runWrite{move}}
	tr := NewTracker()
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.SetAppClockShown(clock.shown)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(1900*time.Millisecond))
	tr.ServedMockAt(&models.Mock{Name: "test-own-find", Kind: models.Mongo}, move.to.Add(2*time.Millisecond))
	arrival := move.to.Add(20 * time.Millisecond)
	v := epochSecond(base.Add(1990 * time.Millisecond)) // the clock before the move: the real clock
	stamp := EpochOffset{Res: time.Second}
	spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival, bgPolls)
	if !ok || len(spans) != 3 || !stamp.HoldsIn(v, spans) {
		t.Fatalf("spans = %+v,%v; want three, the read before the move (%s) holding in one", spans, ok, v)
	}
}

// A background call with no serve of its group before it in the set is
// bounded by the set's begin: what the clock showed from it to the move
// the test's begin wrote holds a poll read before that move, whether the
// call arrives after the move or while it is written, in a server test and
// in a client test (whose own begin bounds no poller's read), with the
// open test's call served between. A value from before the set's begin
// holds in none. Mutation: read the earlier spans or the mid-move ones
// from the group's serve alone (none), or a client test's from its begin.
func TestABackgroundCallWithNoServeOfItsGroupIsBoundedByTheSetsBegin(t *testing.T) {
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
			spans, ok := CallSpansOf(ctx, call.arrival, bgPolls)
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

// No group named tells no span, under either switch, and calls nothing; and
// no span holds no read. Mutation: read a nil group; hold a read in no span.
func TestABackgroundCallOfNoGroupHasNoSpans(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(realClock)
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.NewSetAt(base)
	tr.ServedMockAt(&models.Mock{Name: "poll-1", Kind: models.Mongo}, base.Add(time.Second))
	sc := &ServeClock{}
	sc.SetClock(realClock)
	serveMockAt(t, sc, "poll-1", base.Add(time.Second))
	arrival := base.Add(2 * time.Second)
	if spans, ok := CallSpansOf(WithTracker(context.Background(), tr), arrival, bgPolls); !ok || len(spans) != 1 {
		t.Fatalf("spans = %+v,%v of the group; want one: the row tests nothing", spans, ok)
	}
	for what, ctx := range map[string]context.Context{"tracker": WithTracker(context.Background(), tr), "serve clock": WithServeClock(context.Background(), sc)} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: a nil group panicked: %v", what, r)
				}
			}()
			if spans, ok := CallSpansOf(ctx, arrival, nil); ok || spans != nil {
				t.Fatalf("%s: spans = %+v,%v of no group; want none", what, spans, ok)
			}
		}()
	}
	if (EpochOffset{Res: time.Second}).HoldsIn(epochSecond(base), nil) {
		t.Fatal("a read holds in no span")
	}
}

// With the proven switch off the serve clock starts a background call's
// span at its group's latest serve before arrival (never at a serve after
// it), else at the earliest serve it keeps; where the clock moved since
// that bound, at the latest serve of any flow, as SpanTo does. Mutation:
// read serves after arrival or slots never written; start at the earliest
// kept serve where the group has a later one; tell no span where the clock
// moved since the bound.
func TestTheServeClockBoundsABackgroundCallByItsGroup(t *testing.T) {
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	stamp := EpochOffset{Res: time.Second}
	moved := base.Add(1500 * time.Millisecond)
	movingClock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r, true
		}
		return r.Add(time.Hour), true
	}
	type serve struct {
		name string
		at   time.Duration
	}
	for _, c := range []struct {
		what    string
		clock   func(time.Time) (time.Time, bool)
		serves  []serve
		arrival time.Duration
		read    time.Time // the value the poll read, on the app's clock
		from    time.Duration
	}{
		{"no serve of the group: the earliest kept", realClock,
			[]serve{{"other-1", 1000 * time.Millisecond}, {"test-own-find", 2001 * time.Millisecond}}, 2017 * time.Millisecond,
			base.Add(1990 * time.Millisecond), 1000 * time.Millisecond},
		{"a serve of the group after arrival is no bound", realClock,
			[]serve{{"poll-1", 1900 * time.Millisecond}, {"test-own-find", 2001 * time.Millisecond}, {"poll-2", 3 * time.Second}}, 2017 * time.Millisecond,
			base.Add(1990 * time.Millisecond), 1900 * time.Millisecond},
		{"the clock moved since the earliest kept, not since the group's", movingClock,
			[]serve{{"other-1", 1000 * time.Millisecond}, {"poll-1", 1900 * time.Millisecond}, {"test-own-find", 2001 * time.Millisecond}}, 2017 * time.Millisecond,
			base.Add(1990 * time.Millisecond).Add(time.Hour), 1900 * time.Millisecond},
		{"the clock moved since the group's serve: the latest of any flow", movingClock,
			[]serve{{"poll-1", 1000 * time.Millisecond}, {"test-own-find", 1600 * time.Millisecond}}, 2017 * time.Millisecond,
			base.Add(1700 * time.Millisecond).Add(time.Hour), 1600 * time.Millisecond},
	} {
		sc := &ServeClock{}
		sc.SetClock(c.clock)
		for _, s := range c.serves {
			serveMockAt(t, sc, s.name, base.Add(s.at))
		}
		spans, ok := sc.SpansOf(base.Add(c.arrival), bgPolls)
		if !ok || len(spans) != 1 {
			t.Fatalf("%s: spans = %+v,%v; want one", c.what, spans, ok)
		}
		if want, _ := c.clock(base.Add(c.from)); spans[0].From != want {
			t.Fatalf("%s: span from %v; want %v", c.what, spans[0].From, want)
		}
		if v := epochSecond(c.read); !stamp.HoldsIn(v, spans) {
			t.Fatalf("%s: %s, the poll's read, holds in none of %+v", c.what, v, spans)
		}
	}
	var nilClock *ServeClock
	if spans, ok := nilClock.SpansOf(base, bgPolls); ok || spans != nil {
		t.Fatalf("spans = %+v,%v of a nil serve clock; want none", spans, ok)
	}
	if spans, ok := (&ServeClock{}).SpansOf(base, bgPolls); ok || spans != nil {
		t.Fatalf("spans = %+v,%v with no serve kept; want none", spans, ok)
	}
}
