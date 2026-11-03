package learn

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// concurrentRun models keploy test's post-url-2 in the sqs sample's forced
// variant (CI 2026-10-31): the test's begin moves the app's clock to the
// recorded begin .933485077; the handler reads its clock after the move and
// writes the upsert's created at a cut of a millisecond, .934 (so a read at
// least 0.51 ms after the move); the consumer, a goroutine of its own, makes
// two calls (a count and an insert) on the pooled connection, which keploy
// serves after that read; the handler's upsert reaches keploy only after
// them. The logs fix that order, not the times within it: the row's times
// after the move (the read at 0.6 ms, the serves at 1.3 ms and 3.5 ms, the
// upsert's arrival at 4.4 ms) are illustrative.
type concurrentRun struct {
	move              runWrite
	began, prevTest   time.Time // the test's begin and the previous test's last serve
	read, a, b, upTo  time.Time // the handler's read, the consumer's two serves, and the upsert's arrival, real
	clock             *runClock
	recBegin, appRead time.Time // the moved clock at the move's end, and the handler's read on it
}

func newConcurrentRun() concurrentRun {
	base := time.Date(2026, 10, 31, 8, 54, 6, 986_000_000, time.UTC)
	recBegin := time.Date(2026, 10, 31, 8, 53, 33, 933_485_077, time.UTC)
	move := runWrite{from: base, to: base.Add(50 * time.Microsecond)}
	move.offset = recBegin.Sub(move.to)
	r := concurrentRun{
		move:     move,
		began:    base.Add(-100 * time.Microsecond),
		prevTest: base.Add(-25 * time.Millisecond),
		read:     move.to.Add(600 * time.Microsecond),
		a:        move.to.Add(1300 * time.Microsecond),
		b:        move.to.Add(3500 * time.Microsecond),
		upTo:     move.to.Add(4400 * time.Microsecond),
		clock:    &runClock{writes: []runWrite{move}},
		recBegin: recBegin,
	}
	r.appRead = r.read.Add(move.offset)
	return r
}

// tracker is a tracker that ran r up to the upsert's arrival: the set's
// begin, the previous test's last serve, the test's begin, and the
// consumer's two serves.
func (r concurrentRun) tracker(client bool) *Tracker {
	tr := NewTracker()
	tr.SetClientTests(client)
	tr.SetAppClock(r.clock.at)
	tr.SetAppClockRun(r.clock.began)
	tr.NewSetAt(r.prevTest.Add(-time.Second))
	tr.ServedMockAt(&models.Mock{Name: "mock-4", Kind: models.HTTP}, r.prevTest)
	tr.BeginAt(r.began)
	tr.ServedMockAt(&models.Mock{Name: "mock-7", Kind: models.Mongo}, r.a)
	tr.ServedMockAt(&models.Mock{Name: "mock-8", Kind: models.Mongo}, r.b)
	return tr
}

// concurrentRead is a value a row checks at the upsert, and whether the
// replay's check passes it.
type concurrentRead struct {
	what string
	v    time.Time
	ok   bool
}

// reads are the values the rows check at the upsert, on the app's clock.
func (r concurrentRun) reads() []concurrentRead {
	return []concurrentRead{
		{"the handler's read, before the consumer's serves", r.appRead, true},
		{"a read at the move's end", r.recBegin, true},
		{"a read at the upsert's arrival", r.upTo.Add(r.move.offset), true},
		{"a read of the clock before the move", r.read, false},
		{"a value two cuts before the test's begin", r.recBegin.Add(-2 * time.Millisecond), false},
		{"a value more than a cut after the arrival", r.upTo.Add(r.move.offset + 2*time.Millisecond), false},
	}
}

// A call's span on the app's clock starts no later than where its test
// began: another goroutine's call that keploy serves after the test began
// (a consumer beside the test's handler) can be served between a read of
// the clock and the call that carries it, so it bounds nothing. The
// handler's upsert, whose created the app read after the test's begin
// moved its clock and wrote at a cut of a millisecond (.934), reached
// keploy after the consumer's two calls were served: the replay's check
// (record's confirming round, and keploy test) passes it, in a server test
// and in a client test alike, as it passes a read at the move itself and
// one at the call's arrival. A value outside the test's span is refused as
// before: a read of the clock as it ran before the move, a value two cuts
// before the test's begin, and one more than a cut after the arrival.
func TestAConcurrentServeAfterTheReadDoesNotNarrowItsCallsSpan(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.docs.msgs[].u.{}.created"
	read := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	for _, client := range []bool{false, true} {
		tr := r.tracker(client)
		s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), r.upTo)
		if !ok {
			t.Fatalf("client %v: no span for the upsert", client)
		}
		if got := at(r.appRead).Value; !strings.HasSuffix(got, ":33.934Z") {
			t.Fatalf("the handler's read writes %s; want the .934 cut", got)
		}
		for _, c := range r.reads() {
			if ok, why := CheckClockRead(p, read, at(c.v), s, true); ok != c.ok {
				t.Errorf("client %v: %s (%s): ok %v (%s); want %v", client, c.what, at(c.v).Value, ok, why, c.ok)
			}
		}
	}
}

// A word read (one word of a text, here the millisecond count in an event's
// id, learned as a plain read of the clock) is checked from its test's begin
// as a whole value is: the same values, in the same timeline, pass and are
// refused, in a server test and in a client test alike. The word stands
// where it was recorded in every text here, so each refusal is the clock
// check's.
func TestAWordReadIsCheckedFromItsTestsBeginPastAnotherGoroutinesServes(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.msg"
	text := func(v time.Time) string { return "evt-" + strconv.FormatInt(v.UnixMilli(), 10) + "-sqs" }
	if got := text(r.appRead); !strings.HasSuffix(got, "934-sqs") {
		t.Fatalf("the handler's read writes %s; want the .934 cut", got)
	}
	read := models.ClockOffset{Res: time.Millisecond, Word: sides{"evt-", "-sqs"}.at(t, text(r.recBegin))}
	for _, client := range []bool{false, true} {
		s, ok := ClockSpanFrom(WithTracker(context.Background(), r.tracker(client)), r.upTo)
		if !ok {
			t.Fatalf("client %v: no span for the upsert", client)
		}
		for _, c := range r.reads() {
			live := Leaf{Path: p, Noise: p, Type: String, Value: text(c.v)}
			ok, why := CheckClockRead(p, read, live, s, true)
			if ok != c.ok {
				t.Errorf("client %v: %s (%s): ok %v (%s); want %v", client, c.what, live.Value, ok, why, c.ok)
			} else if !ok && !strings.Contains(why, "not a read of the application's clock made in this call's window") {
				t.Errorf("client %v: %s (%s): refused with %q; want the clock check's refusal", client, c.what, live.Value, why)
			}
		}
	}
}

// A replay checks a call against the recording it pairs with in that
// recording's span (ClockSpanOf, as HTTP's strict match reads it): from the
// test's own serves where the store says the recording is the test's own
// (SetServeOwnerOf), from any flow's serve otherwise. On a clock that ran as
// one from the test's begin, the test served a call of its own, read its
// clock, and a consumer's call was served before the call reached keploy.
// The span carries where the test began (Span.TestFrom), as ClockSpanFrom's
// does, wherever the call is the test's: where no owner is told, where the
// store cannot say (no test's view in force), where it calls the consumer's
// recording the test's own (the span starts at the consumer's serve), and
// where it calls it another's (the span starts at the test's own serve). So
// the test's read, and one made before the test's own serve, pass there.
// Where the store cannot say, a call checked against a worker:'s recording
// gets the test's span too (a stated limit): the tracker reads a recording
// only through the store's answer, so keploy cannot tell then that the call
// is not the test's, and the test's begin still bounds it. Checked against a
// recording the store says is not the test's (a worker:'s), the span starts
// at the consumer's serve and carries no begin: both reads, and the test's
// expiry read before the consumer's serve, are refused; a read and an expiry
// read at the arrival pass. In each, a wrong value (two cuts before the
// test's begin, more than a cut past the arrival) is refused, as is an
// expiry whose time to live dropped, by a minute or to none.
func TestARecordingsSpanCarriesItsTestsBeginUnlessTheStoreSaysItIsAnothers(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	early, ownServe, read := base.Add(time.Millisecond), base.Add(2*time.Millisecond), base.Add(3*time.Millisecond)
	consumer, arrival := base.Add(5*time.Millisecond), base.Add(6*time.Millisecond)
	const p = "body.created"
	clock := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond, Layout: models.LayoutTyped}
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	upsert := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	worker := &models.Mock{Name: "mock-9", Kind: models.Mongo}
	tests := func(names ...string) func(*models.Mock) ServeOwner {
		return func(m *models.Mock) ServeOwner {
			if slices.Contains(names, m.Name) {
				return OwnerTest
			}
			return OwnerOther
		}
	}
	for _, c := range []struct {
		what     string
		owner    func(*models.Mock) ServeOwner
		rec      *models.Mock
		from     time.Time
		testFrom time.Time
	}{
		{"no owner told", nil, upsert, consumer, base},
		{"the store cannot say", func(*models.Mock) ServeOwner { return OwnerUnknown }, upsert, consumer, base},
		// The tracker reads a recording only through the store's answer, so
		// this runs as the row above: the stated limit, made literal.
		{"the store cannot say, checked against a worker's recording", func(*models.Mock) ServeOwner { return OwnerUnknown }, worker, consumer, base},
		{"the consumer's recording the test's own", tests("mock-1", "mock-7", "mock-5"), upsert, consumer, base},
		{"the consumer's recording another's", tests("mock-1", "mock-5"), upsert, ownServe, base},
		{"an owner told, checked against no recording", tests("mock-1", "mock-5"), nil, consumer, base},
		{"checked against a worker's recording", tests("mock-1", "mock-5"), worker, consumer, time.Time{}},
	} {
		for _, client := range []bool{false, true} {
			tr := NewTracker()
			tr.SetClientTests(client)
			tr.SetServeOwnerOf(c.owner)
			tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
			tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
			tr.NewSetAt(base.Add(-time.Second))
			tr.ServedMockAt(&models.Mock{Name: "mock-4", Kind: models.HTTP}, base.Add(-25*time.Millisecond))
			tr.BeginAt(base)
			tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, ownServe)
			tr.ServedMockAt(&models.Mock{Name: "mock-7", Kind: models.Mongo}, consumer)
			s, ok := ClockSpanOf(WithTracker(context.Background(), tr), arrival, c.rec)
			if !ok || !s.From.Equal(c.from) || !s.TestFrom.Equal(c.testFrom) {
				t.Fatalf("%s, client %v: span = %+v,%v; want from %v carrying the test's begin %v", c.what, client, s, ok, c.from, c.testFrom)
			}
			testsCall := !c.testFrom.IsZero()
			for _, v := range []struct {
				what string
				o    models.ClockOffset
				at   time.Time
				ok   bool
			}{
				{"a read before the test's own serve", clock, early, testsCall},
				{"the test's read, before the consumer's serve", clock, read, testsCall},
				{"a read at the arrival", clock, arrival, true},
				{"a value two cuts before the test's begin", clock, base.Add(-2 * time.Millisecond), false},
				{"a value more than a cut past the arrival", clock, arrival.Add(2 * time.Millisecond), false},
				{"an expiry read at the arrival", ttl, arrival.Add(30 * time.Minute), true},
				{"the test's expiry, read before the consumer's serve", ttl, read.Add(30 * time.Minute), testsCall},
				{"an expiry whose time to live dropped by a minute", ttl, arrival.Add(29 * time.Minute), false},
				{"an expiry whose time to live dropped to none", ttl, arrival, false},
			} {
				if ok, why := CheckClockRead(p, v.o, at(v.at), s, true); ok != v.ok {
					t.Errorf("%s, client %v: %s (%s): ok %v (%s); want %v", c.what, client, v.what, at(v.at).Value, ok, why, v.ok)
				}
			}
		}
	}
}

// A replay's span carries where its test began (Span.TestFrom) only where a
// serve made after the test began starts it, the test is open, the test
// began no later than the call, and the app's clock ran as one from there:
// in a server test where the hooks tell the clock's run (SetAppClockRun),
// in a client test where they tell none too (as its span's begin is read).
// The table here moves the clock after the test began, so its server test
// with no run told is refused by that move; the run told itself is pinned
// on a clock that ran as one by
// TestATestsBeginIsCarriedWithNoRunToldOnlyInAClientTest. The span itself
// still starts at the last serve, as the learn step's window of the call
// does (CallWindowFrom), which carries the test's begin too, and the next
// set's begin clears it.
func TestALiveSpanCarriesItsTestsBeginOnlyWhereALaterServeStartsIt(t *testing.T) {
	r := newConcurrentRun()
	want := Span{From: r.b.Add(r.move.offset), To: r.upTo.Add(r.move.offset), TestFrom: r.recBegin}
	s, ok := r.tracker(false).LiveSpan(r.upTo)
	if !ok || s != want {
		t.Fatalf("span = %+v,%v; want %+v", s, ok, want)
	}
	for _, c := range []struct {
		what  string
		setup func(tr *Tracker)
		at    time.Time
	}{
		{"no serve after the test began", func(tr *Tracker) {}, r.a.Add(-time.Microsecond)},
		{"the test ended", func(tr *Tracker) { tr.End() }, r.upTo},
		{"a call that reached keploy before its test began", func(tr *Tracker) {}, r.began.Add(-time.Microsecond)},
		{"a server test whose hooks tell no run, its clock moved since the test began", func(tr *Tracker) { tr.SetAppClockRun(nil) }, r.upTo},
		{"the next set", func(tr *Tracker) { tr.NewSetAt(r.b.Add(time.Microsecond)) }, r.upTo},
	} {
		tr := r.tracker(false)
		c.setup(tr)
		if s, _ := tr.LiveSpan(c.at); !s.TestFrom.IsZero() {
			t.Errorf("%s: span %+v carries the test's begin; want none", c.what, s)
		}
	}
	// The learn step's window of the call reads the span from the serve,
	// and carries the test's begin as the replay's span does: the learn
	// step reads a read from there too where the window refuses it
	// (Window.fromTestBegin). The recording's span, whose previous call is
	// the previous test's, carries none (no recorded begin was told).
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = r.recBegin.Add(500 * time.Microsecond)
	w := r.tracker(false).CallWindowFrom(r.recBegin.Add(-150*time.Millisecond), paired, r.upTo)
	if w == nil || !w.F.From.Equal(r.b.Add(r.move.offset)) || !w.F.TestFrom.Equal(r.recBegin) || !w.Rec.TestFrom.IsZero() {
		t.Fatalf("learn window = %+v; want F from the last serve, with the test's begin", w)
	}
}

// Where the hooks tell no run of the app's clock, a server test's span
// carries no test's begin even on a clock that ran as one from it (here the
// real clock, which from() reads as one run): the clock may have left the
// begin's offset and come back to it since, so keploy cannot vouch for that
// span, and a read made in the test before the last serve is refused. A
// client test's span, whose hooks tell none either, carries its begin (its
// harness acts after the begin, as its span's begin is read), and the read
// passes.
func TestATestsBeginIsCarriedWithNoRunToldOnlyInAClientTest(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	served, arrival := base.Add(4*time.Millisecond), base.Add(5*time.Millisecond)
	const p = "body.created"
	read := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	early := InstantLeaf(p, p, base.Add(time.Millisecond), time.Millisecond)
	for _, client := range []bool{false, true} {
		tr := NewTracker()
		tr.SetClientTests(client)
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.NewSetAt(base.Add(-time.Second))
		tr.BeginAt(base)
		tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, served)
		s, ok := tr.LiveSpan(arrival)
		if !ok || !s.From.Equal(served) {
			t.Fatalf("client %v: span = %+v,%v; want from the serve", client, s, ok)
		}
		var want time.Time
		if client {
			want = base
		}
		if !s.TestFrom.Equal(want) {
			t.Errorf("client %v: span %+v carries test's begin %v; want %v", client, s, s.TestFrom, want)
		}
		if ok, why := CheckClockRead(p, read, early, s, true); ok != client {
			t.Errorf("client %v: a read in the test before the last serve (%s): ok %v (%s); want %v", client, early.Value, ok, why, client)
		}
	}
}

// A serve the store cannot say of (OwnerUnknown: no test's view in force as
// it served) is not filed as the open test's own, so it starts no span of a
// call checked against the test's own recording: that span starts at the
// test's own serve before it, as one the store says is another's does. A
// server test whose hooks tell no run carries no test's begin (so nothing
// else widens the span here): the test's read, made after its own serve and
// before the unsaid one, passes, and a read two cuts before its own serve is
// refused.
func TestAServeTheStoreCannotSayOfStartsNoSpanOfTheTestsOwnRecording(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	ownServe, read, unsaid, arrival := base.Add(3*time.Millisecond), base.Add(4*time.Millisecond), base.Add(6*time.Millisecond), base.Add(7*time.Millisecond)
	const p = "body.created"
	clock := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	for _, unsaidOwner := range []ServeOwner{OwnerUnknown, OwnerOther} {
		tr := NewTracker()
		tr.SetServeOwnerOf(func(m *models.Mock) ServeOwner {
			if m.Name == "mock-7" {
				return unsaidOwner
			}
			return OwnerTest
		})
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.NewSetAt(base.Add(-time.Second))
		tr.BeginAt(base)
		tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, ownServe)
		tr.ServedMockAt(&models.Mock{Name: "mock-7", Kind: models.Mongo}, unsaid)
		s, ok := ClockSpanOf(WithTracker(context.Background(), tr), arrival, &models.Mock{Name: "mock-5", Kind: models.Mongo})
		if !ok || !s.From.Equal(ownServe) || !s.TestFrom.IsZero() {
			t.Fatalf("owner %d: span = %+v,%v; want from the test's own serve %v, no test's begin", unsaidOwner, s, ok, ownServe)
		}
		if ok, why := CheckClockRead(p, clock, at(read), s, true); !ok {
			t.Errorf("owner %d: the test's read before the serve (%s): refused (%s); want it passed", unsaidOwner, at(read).Value, why)
		}
		if ok, _ := CheckClockRead(p, clock, at(base.Add(time.Millisecond)), s, true); ok {
			t.Errorf("owner %d: a read two cuts before the test's own serve passed; want it refused", unsaidOwner)
		}
	}
}

// A read plus an offset is checked in the span from the last serve first:
// a time to live checkable there may not be in the test's longer span
// (models.ClockOffset.Checkable), where the replay would refuse it rather
// than pass it unchecked, so the test's begin only ever passes more. A test
// that began two minutes before its call: now + 30m read after the last
// serve passes, as it did before; one read in the test before that serve
// is refused, its check uncheckable in the test's span, as before; one
// changed by a minute is refused. Each refusal gives the reason the span
// from the last serve gives it, not the test's span's.
func TestAReadPlusAnOffsetIsCheckedInTheSpanFromTheLastServeFirst(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	began, served, arrival := base, base.Add(2*time.Minute), base.Add(2*time.Minute+10*time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.NewSetAt(base.Add(-time.Second))
	tr.BeginAt(began)
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.HTTP}, served)
	s, ok := tr.LiveSpan(arrival)
	if !ok || !s.From.Equal(served) || !s.TestFrom.Equal(began) {
		t.Fatalf("span = %+v,%v; want from the serve, the test's begin carried", s, ok)
	}
	const p = "body.expires"
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Second, Res: time.Second}
	text := func(v time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: v.UTC().Format(time.RFC3339)}
	}
	wide := s
	wide.From, wide.TestFrom = s.TestFrom, time.Time{}
	if ok, _ := CheckClockRead(p, ttl, text(served.Add(30*time.Minute)), wide, true); ok {
		t.Fatal("the row needs a time to live the test's span cannot check")
	}
	for _, c := range []struct {
		what string
		v    time.Time
		ok   bool
	}{
		{"now + 30m, read after the last serve", served.Add(5*time.Millisecond + 30*time.Minute), true},
		{"now + 30m, read in the test before the last serve", began.Add(time.Minute + 30*time.Minute), false},
		{"now + 31m", served.Add(31 * time.Minute), false},
	} {
		ok, why := CheckClockRead(p, ttl, text(c.v), s, true)
		if ok != c.ok {
			t.Errorf("%s: ok %v (%s); want %v", c.what, ok, why, c.ok)
		}
		if !ok {
			narrow := s
			narrow.TestFrom = time.Time{}
			if _, nwhy := CheckClockRead(p, ttl, text(c.v), narrow, true); why != nwhy {
				t.Errorf("%s: refused with %q; want the reason of the span from the last serve, %q", c.what, why, nwhy)
			}
		}
	}
}

// The cost of the test's begin, pinned: keploy cannot tell which of the
// application's goroutines made a call, so a serve made after the test
// began bounds no read, the test's own earlier call's no more than another
// goroutine's. A handler that read its clock as its test began and wrote
// that value at its second call, after keploy served its first, passes
// (a timestamp read once for a request); a value read before the test
// began, or more than a cut after the call, is refused as before. A read
// plus an offset bears the same cost, bounded by the test's own span: an
// expiry read as the test began passes, and so does one whose time to live
// is shorter than the learned 30m by up to the time from the start of the
// cut the test began in to its read, plus a cut (under two cuts more than
// the time from the begin itself). The test here begins on a cut, so the
// edge is exact: read 42 ms in, 43 ms shorter passes; one whose time to
// live dropped by 44 ms, by a minute, or to none (the read itself) is
// refused.
func TestAReadEarlierInItsTestPassesAtALaterCallOfTheSameTest(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	began, served, arrival := base, base.Add(40*time.Millisecond), base.Add(45*time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.NewSetAt(base.Add(-time.Second))
	tr.BeginAt(began)
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Postgres}, served)
	s, ok := tr.LiveSpan(arrival)
	if !ok || !s.From.Equal(served) || !s.TestFrom.Equal(began) {
		t.Fatalf("span = %+v,%v; want from the first call's serve, the test's begin carried", s, ok)
	}
	const p = "body.created"
	read := models.ClockOffset{Res: time.Millisecond, Layout: models.LayoutTyped}
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond, Layout: models.LayoutTyped}
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	after := served.Add(2 * time.Millisecond)
	for _, c := range []struct {
		what string
		o    models.ClockOffset
		v    time.Time
		ok   bool
	}{
		{"a read after the first call's serve", read, after, true},
		{"a read as the test began, before the first call's serve", read, began.Add(time.Millisecond), true},
		{"a read before the test began", read, began.Add(-5 * time.Millisecond), false},
		{"a value more than a cut after the call", read, arrival.Add(2 * time.Millisecond), false},
		{"an expiry read after the first call's serve", ttl, after.Add(30 * time.Minute), true},
		{"an expiry read as the test began", ttl, began.Add(time.Millisecond + 30*time.Minute), true},
		{"an expiry whose time to live is 43 ms shorter, the time from the test's begin to its read plus a cut", ttl, after.Add(30*time.Minute - 43*time.Millisecond), true},
		{"an expiry whose time to live dropped by 44 ms, past the test's span", ttl, after.Add(30*time.Minute - 44*time.Millisecond), false},
		{"an expiry whose time to live dropped by a minute", ttl, after.Add(29 * time.Minute), false},
		{"an expiry whose time to live dropped to none", ttl, after, false},
	} {
		if ok, why := CheckClockRead(p, c.o, at(c.v), s, true); ok != c.ok {
			t.Errorf("%s (%s): ok %v (%s); want %v", c.what, at(c.v).Value, ok, why, c.ok)
		}
	}
}

// A test's own call in keploy record's server tests reads its learn window
// (OwnCall) as a replay checks its reads (LiveSpanOf): a recording made in
// the call's recorded test that the store cannot say of (OwnerUnknown) is no
// previous call of it, as a serve of one made after the test began starts no
// span of it in this run. The window carries where the test began in each
// run (Span.TestFrom), as the replay's span does, so the learn step reads a
// read from the test's begin too (Window.fromTestBegin).
func TestATestsOwnCallsWindowSkipsWhatTheStoreCannotSayOfAndCarriesItsBegin(t *testing.T) {
	began := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	ownServe, unsaidServe, arrival := began.Add(10*time.Millisecond), began.Add(20*time.Millisecond), began.Add(30*time.Millisecond)
	recAt := began.Add(-time.Hour)
	recBegan := recAt.Add(-100 * time.Millisecond)
	at := func(name string, d time.Duration) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.Mongo}
		m.Spec.ReqTimestampMock = recAt.Add(d)
		return m
	}
	prevTest, own1, unsaid, own2 := at("worker-0", -2*time.Second), at("own-1", -30*time.Millisecond), at("unsaid-1", -10*time.Millisecond), at("own-2", 0)
	tests := recordedTests{prevTest.Spec.ReqTimestampMock: 1, own1.Spec.ReqTimestampMock: 2, unsaid.Spec.ReqTimestampMock: 2, own2.Spec.ReqTimestampMock: 2}
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.SetServeOwnerOf(func(m *models.Mock) ServeOwner {
		switch m.Name {
		case "own-1", "own-2":
			return OwnerTest
		case "unsaid-1":
			return OwnerUnknown
		}
		return OwnerOther
	})
	tr.NewSetAt(began.Add(-time.Second))
	tr.RecordedBegin(recBegan)
	tr.BeginAt(began)
	tr.ServedMockAt(&models.Mock{Name: "own-1", Kind: models.Mongo}, ownServe)
	tr.ServedMockAt(&models.Mock{Name: "unsaid-1", Kind: models.Mongo}, unsaidServe)
	oc := tr.NewOwnCall(own2, nil, tests)
	for _, m := range []*models.Mock{prevTest, own1, unsaid} {
		oc.See(m)
	}
	w := oc.Window(arrival)
	if w == nil || !w.F.From.Equal(ownServe) || !w.Rec.From.Equal(own1.Spec.ReqTimestampMock) {
		t.Fatalf("the own call's window %+v; want F from the test's own serve %v and the recording's half from own-1 %v, past the recording the store cannot say of", w, ownServe, own1.Spec.ReqTimestampMock)
	}
	if !w.F.TestFrom.Equal(began) || !w.Rec.TestFrom.Equal(recBegan) {
		t.Fatalf("the own call's window carries test begins F %v, recording %v; want %v and %v", w.F.TestFrom, w.Rec.TestFrom, began, recBegan)
	}
}
