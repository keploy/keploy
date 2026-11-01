package learn

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// concurrentRun is the timeline of keploy test's post-url-2 in the sqs
// sample's forced variant (CI 2026-10-31, r9/fail-head4): the test's begin
// moves the app's clock to the recorded begin .933485077; the handler reads
// its clock 0.6 ms later and writes the upsert's created at a cut of a
// millisecond, .934; the consumer, a goroutine of its own, makes two calls
// keploy serves after that read (1.3 ms and 3.9 ms after the move); the
// handler's upsert reaches keploy only after them (4.4 ms), its driver
// having waited for the pooled connection the consumer held.
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
		b:        move.to.Add(3900 * time.Microsecond),
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

// A call's span on the app's clock starts no later than where its test
// began: another goroutine's call that keploy serves after the test began
// (a consumer beside the test's handler) can be served between a read of
// the clock and the call that carries it, so it bounds nothing. The
// handler's upsert, whose created the app read 0.6 ms after the test's
// begin moved its clock and wrote at a cut of a millisecond (.934), reached
// keploy after the consumer's two calls, served 1.3 ms and 3.9 ms after the
// move: the replay's check (record's confirming round, and keploy test)
// passes it, in a server test and in a client test alike, as it passes a
// read at the move itself and one at the call's arrival. A value outside
// the test's span is refused as before: a read of the clock before the
// move (the previous test's), a value a millisecond before the recorded
// begin's cut and less a cut, and one more than a cut after the arrival.
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
		for _, c := range []struct {
			what string
			v    time.Time
			ok   bool
		}{
			{"the handler's read, before the consumer's serves", r.appRead, true},
			{"a read at the move's end", r.recBegin, true},
			{"a read at the upsert's arrival", r.upTo.Add(r.move.offset), true},
			{"a read of the clock before the move", r.read, false},
			{"a read of the previous test, on the moved clock", r.recBegin.Add(-2 * time.Millisecond), false},
			{"a value more than a cut after the arrival", r.upTo.Add(r.move.offset + 2*time.Millisecond), false},
		} {
			if ok, why := CheckClockRead(p, read, at(c.v), s, true); ok != c.ok {
				t.Errorf("client %v: %s (%s): ok %v (%s); want %v", client, c.what, at(c.v).Value, ok, why, c.ok)
			}
		}
	}
}

// A replay's span carries where its test began (Span.TestFrom) only where a
// serve made after the test began starts it, the test is open, the test
// began no later than the call, and the app's clock ran as one from there:
// in a server test where the hooks tell the clock's run (SetAppClockRun),
// in a client test where they tell none too (as its span's begin is read).
// The span itself still starts at the last serve, which the learn step's
// windows read (CallWindowFrom), and the next set's begin clears it.
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
		{"a server test whose hooks tell no run", func(tr *Tracker) { tr.SetAppClockRun(nil) }, r.upTo},
		{"the next set", func(tr *Tracker) { tr.NewSetAt(r.b.Add(time.Microsecond)) }, r.upTo},
	} {
		tr := r.tracker(false)
		c.setup(tr)
		if s, _ := tr.LiveSpan(c.at); !s.TestFrom.IsZero() {
			t.Errorf("%s: span %+v carries the test's begin; want none", c.what, s)
		}
	}
	// The learn step's window of the call reads the span from the serve,
	// and carries no test's begin: only a replay's check reads it.
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = r.recBegin.Add(500 * time.Microsecond)
	w := r.tracker(false).CallWindowFrom(r.recBegin.Add(-150*time.Millisecond), paired, r.upTo)
	if w == nil || !w.F.From.Equal(r.b.Add(r.move.offset)) || !w.F.TestFrom.IsZero() {
		t.Fatalf("learn window = %+v; want F from the last serve, with no test's begin", w)
	}
}

// A read plus an offset is checked in the span from the last serve first:
// a time to live checkable there may not be in the test's longer span
// (models.ClockOffset.Checkable), where the replay would refuse it rather
// than pass it unchecked, so the test's begin only ever passes more. A test
// that began two minutes before its call: now + 30m read after the last
// serve passes, as it did before; one read in the test before that serve
// is refused, its check uncheckable in the test's span, as before; one
// changed by a minute is refused.
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
		if ok, why := CheckClockRead(p, ttl, text(c.v), s, true); ok != c.ok {
			t.Errorf("%s: ok %v (%s); want %v", c.what, ok, why, c.ok)
		}
	}
}
