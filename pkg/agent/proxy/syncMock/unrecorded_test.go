package manager

import (
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// spanLog records what an UnrecordedConn opens and closes.
type spanLog struct {
	mu     sync.Mutex
	starts []time.Time
	opens  int
	closes int
}

func (l *spanLog) open(start time.Time) func() {
	l.mu.Lock()
	l.opens++
	l.starts = append(l.starts, start)
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.closes++
			l.mu.Unlock()
		})
	}
}

func (l *spanLog) counts() (opens, closes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens, l.closes
}

func (l *spanLog) lastStart() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.starts) == 0 {
		return time.Time{}
	}
	return l.starts[len(l.starts)-1]
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("not within %v: %s", timeout, what)
}

const (
	testIdleGrace = 40 * time.Millisecond
	testIdleCheck = 10 * time.Millisecond
)

// A stopped connection costs a test case only while the app uses it. These pin
// that its span follows its activity, not the rest of the recording.
func TestUnrecordedConnFollowsTraffic(t *testing.T) {
	t.Parallel()

	t.Run("nothing before Stop", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, testIdleGrace, testIdleCheck)
		for i := 0; i < 5; i++ {
			u.Note(time.Now())
		}
		time.Sleep(3 * testIdleCheck)
		if o, _ := l.counts(); o != 0 {
			t.Fatalf("opened %d spans for a connection that is still recorded", o)
		}
		u.End()
	})

	t.Run("a span from the stop, closed once the connection goes idle", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, testIdleGrace, testIdleCheck)
		at := time.Now().Add(-5 * time.Millisecond)
		u.Stop(at)
		waitUntil(t, time.Second, "a span opens", func() bool { o, _ := l.counts(); return o == 1 })
		if got := l.lastStart(); !got.Equal(at) {
			t.Fatalf("span starts at %v, want the stop at %v", got, at)
		}
		waitUntil(t, time.Second, "the span closes when idle", func() bool { _, c := l.counts(); return c == 1 })
		u.End()
		if o, c := l.counts(); o != c {
			t.Fatalf("opens=%d closes=%d: a span left open suppresses the rest of the recording", o, c)
		}
	})

	t.Run("only the first Stop counts", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, time.Hour, testIdleCheck)
		first := time.Now().Add(-time.Second)
		if !u.Stop(first) {
			t.Fatal("first Stop reported false")
		}
		if u.Stop(time.Now()) {
			t.Fatal("second Stop reported true")
		}
		waitUntil(t, time.Second, "one span", func() bool { o, _ := l.counts(); return o == 1 })
		u.End()
		waitUntil(t, time.Second, "End closes it", func() bool { _, c := l.counts(); return c == 1 })
		if o, _ := l.counts(); o != 1 || !l.lastStart().Equal(first) {
			t.Fatalf("opens=%d start=%v, want one span from the first stop %v", o, l.lastStart(), first)
		}
	})

	t.Run("a busy connection stays covered by one span", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, testIdleGrace, testIdleCheck)
		u.Stop(time.Now())
		for deadline := time.Now().Add(6 * testIdleGrace); time.Now().Before(deadline); {
			u.Note(time.Now())
			time.Sleep(testIdleCheck / 2)
		}
		if o, c := l.counts(); o != 1 || c != 0 {
			t.Fatalf("opens=%d closes=%d, want 1/0: a gap between spans lets a test case through", o, c)
		}
		u.End()
		waitUntil(t, time.Second, "End closes it", func() bool { _, c := l.counts(); return c == 1 })
	})

	// The span that reopens must start at the FIRST byte after the idle close.
	// Traffic that resumes and keeps flowing is only noticed on the next tick,
	// and the latest byte by then is up to a tick later than the first: a span
	// from there leaves the start of the spell, and any request served inside
	// it, uncovered.
	t.Run("traffic that resumes reopens from its first byte", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, testIdleGrace, 50*time.Millisecond)
		u.Stop(time.Now())
		waitUntil(t, 2*time.Second, "the first span closes", func() bool { _, c := l.counts(); return c == 1 })

		first := time.Now()
		u.Note(first)
		for i := 0; i < 20; i++ { // keeps flowing, well inside the next tick
			time.Sleep(time.Millisecond)
			u.Note(time.Now())
		}
		waitUntil(t, time.Second, "a second span", func() bool { o, _ := l.counts(); return o == 2 })
		if got := l.lastStart(); got.After(first) {
			t.Fatalf("reopened at %v, %v after the traffic resumed at %v: that part of it is not covered",
				got, got.Sub(first), first)
		}
		u.End()
		waitUntil(t, time.Second, "End closes it", func() bool { o, c := l.counts(); return o == c })
	})

	t.Run("no follower is left once idle, and none after End", func(t *testing.T) {
		t.Parallel()
		l := &spanLog{}
		u := NewUnrecordedConn(l.open, testIdleGrace, testIdleCheck)
		u.Stop(time.Now())
		waitUntil(t, time.Second, "idle close", func() bool { return !u.running.Load() })
		u.End()
		u.Note(time.Now())
		time.Sleep(3 * testIdleCheck)
		if o, c := l.counts(); o != 1 || c != 1 {
			t.Fatalf("opens=%d closes=%d after End: traffic after the connection ended opened a span", o, c)
		}
	})
}

// Many Notes racing the idle close must never leave traffic without a span:
// after the last Note, a span must exist that started no later than it, or the
// connection must have gone idle after it with that span closed.
func TestUnrecordedConnNoteRacingIdleClose(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		spans := &Spans{}
		u := NewUnrecordedConn(spans.Open, 2*time.Millisecond, time.Millisecond)
		u.Stop(time.Now())
		var last time.Time
		for j := 0; j < 30; j++ {
			time.Sleep(time.Duration(j%4) * time.Millisecond) // straddle the idle bound
			last = time.Now()
			u.Note(last)
			if ok, _ := spans.Overlaps(last, last); !ok {
				// The follower may not have opened yet: give it a moment.
				waitUntil(t, time.Second, "traffic after the stop is covered", func() bool {
					ok, _ := spans.Overlaps(last, last)
					return ok
				})
			}
		}
		u.End()
	}
}

func TestSpansOverlapsOpenAndClosed(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(-time.Minute)
	s.Record(base, base.Add(time.Second))
	closeIt := s.Open(base.Add(10 * time.Second))
	if ok, n := s.Overlaps(base.Add(500*time.Millisecond), base.Add(600*time.Millisecond)); !ok || n != 1 {
		t.Fatalf("closed span: (%v,%d), want (true,1)", ok, n)
	}
	if ok, _ := s.Overlaps(base.Add(2*time.Second), base.Add(3*time.Second)); ok {
		t.Fatal("a window between the spans overlapped")
	}
	if ok, _ := s.Overlaps(time.Now().Add(-time.Millisecond), time.Now()); !ok {
		t.Fatal("an open span must extend to now")
	}
	closeIt()
	time.Sleep(5 * time.Millisecond)
	if ok, _ := s.Overlaps(time.Now(), time.Now()); ok {
		t.Fatal("a closed span still extends to now")
	}
	if c, o := s.Counts(); c != 2 || o != 0 {
		t.Fatalf("Counts = (%d,%d), want (2,0)", c, o)
	}
	var nilS *Spans
	nilS.Record(base, base)
	nilS.Open(base)()
	if ok, _ := nilS.Overlaps(base, base); ok {
		t.Fatal("nil Spans overlapped")
	}
}

// The span opens before Stop (or the Note that restarts it) returns: a test
// case checked right after the stop is seen must already be covered, however
// late the follower goroutine gets to run.
func TestUnrecordedConnOpensItsSpanBeforeReturning(t *testing.T) {
	t.Parallel()
	spans := &Spans{}
	u := NewUnrecordedConn(spans.Open, testIdleGrace, testIdleCheck)
	defer u.End()
	at := time.Now().Add(-time.Millisecond)
	u.Stop(at)
	if ok, _ := spans.Overlaps(at, at); !ok {
		t.Fatal("no span right after Stop returned")
	}
	waitUntil(t, time.Second, "idle close", func() bool { return !u.running.Load() })
	resume := time.Now()
	u.Note(resume)
	if _, open := spans.Counts(); open != 1 {
		t.Fatal("no span open right after the Note that resumed traffic returned")
	}
}

// A span still open is never evicted to make room for spans long closed: a
// connection that stopped hours ago and stays busy must stay covered while
// another one opens and closes a span every idle spell.
func TestSpansKeepAnOpenSpanPastManyClosedOnes(t *testing.T) {
	t.Parallel()
	var s Spans
	longAgo := time.Now().Add(-time.Hour)
	s.Open(longAgo) // conn A: stopped long ago, still busy
	for i := 0; i < maxPressureRanges+100; i++ {
		s.Open(time.Now())() // conn B: a span per spell
	}
	if ok, _ := s.Overlaps(time.Now().Add(-time.Millisecond), time.Now()); !ok {
		t.Fatal("the still-open span was evicted by closed ones")
	}
	if closed, open := s.Counts(); open != 1 || closed == 0 || closed > maxPressureRanges {
		t.Fatalf("Counts = (%d closed, %d open), want (1..%d, 1)", closed, open, maxPressureRanges)
	}
}

// Every span recorded stays covered however many more come after it. A
// DaemonSet agent records a span per mock its stream drops: at a production workload's rate
// that is thousands a second, while the test cases they cover can be checked
// half a minute later. A span evicted to keep the count down let those test
// cases through without their mocks. Past the cap spans are joined instead,
// which can only leave out more: those with the smallest gaps, here all equal,
// the oldest first.
func TestSpansKeepEverySpanPastTheCap(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(-time.Hour)
	const n = 3 * maxPressureRanges
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * 3 * time.Millisecond)
		s.Record(at, at.Add(time.Millisecond))
	}
	for _, i := range []int{0, 1, maxPressureRanges, n / 2, n - 1} {
		at := base.Add(time.Duration(i)*3*time.Millisecond + 500*time.Microsecond)
		if ok, _ := s.Overlaps(at, at); !ok {
			t.Fatalf("span %d of %d is no longer covered: a test case over it would be saved without its mocks", i, n)
		}
	}
	if closed, open := s.Counts(); closed > maxPressureRanges || open != 0 {
		t.Fatalf("Counts = (%d closed, %d open), want at most %d closed", closed, open, maxPressureRanges)
	}
	// The newest spans are as they were: a test case in a gap between them
	// is saved.
	for _, i := range []int{n - 2, n - maxPressureRanges/2} {
		gap := base.Add(time.Duration(i)*3*time.Millisecond + 2*time.Millisecond)
		if ok, _ := s.Overlaps(gap, gap); ok {
			t.Fatalf("the gap after span %d of %d, among the newest, is covered", i, n)
		}
	}
	// Joining is local: a window well clear of every span still passes.
	clear := base.Add(time.Duration(n)*3*time.Millisecond + time.Second)
	if ok, _ := s.Overlaps(clear, clear.Add(time.Millisecond)); ok {
		t.Fatal("a window after the last span is covered")
	}
	before := base.Add(-time.Second)
	if ok, _ := s.Overlaps(before, before.Add(time.Millisecond)); ok {
		t.Fatal("a window before the first span is covered")
	}
}

// Spans that overlap or touch are one span; disjoint ones stay apart, in any
// order of arrival.
func TestSpansJoinOverlappingSpans(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(-time.Minute)
	ms := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
	s.Record(ms(10), ms(20))
	s.Record(ms(40), ms(50))
	s.Record(ms(0), ms(5))
	s.Record(ms(18), ms(30)) // overlaps the first
	s.Record(ms(30), ms(40)) // touches both neighbours
	if closed, _ := s.Counts(); closed != 2 {
		t.Fatalf("closed = %d, want 2 ([0,5] and [10,50])", closed)
	}
	for _, c := range []struct {
		at   int
		want bool
	}{{3, true}, {7, false}, {10, true}, {35, true}, {50, true}, {51, false}} {
		if ok, _ := s.Overlaps(ms(c.at), ms(c.at)); ok != c.want {
			t.Fatalf("Overlaps(%dms) = %v, want %v", c.at, ok, c.want)
		}
	}
	if ok, n := s.Overlaps(ms(4), ms(12)); !ok || n != 2 {
		t.Fatalf("Overlaps(4ms..12ms) = (%v,%d), want (true,2)", ok, n)
	}
}

// Every open span stays open, from its start, until its own closer runs,
// however many are open at once.
func TestSpansKeepEveryOpenSpanPastTheCap(t *testing.T) {
	t.Parallel()
	var s Spans
	// Every start is in the past: n+101 seconds of them.
	base := time.Now().Add(-3 * time.Hour)
	const n = maxPressureRanges
	closers := make([]func(), n)
	for i := 0; i < n; i++ {
		closers[i] = s.Open(base.Add(time.Duration(i) * time.Second))
	}
	// Past the cap: 100 more, and one that starts before all of them.
	var late []func()
	for i := 0; i < 100; i++ {
		late = append(late, s.Open(base.Add(time.Duration(n+i)*time.Second)))
	}
	earliest := base.Add(-time.Hour)
	late = append(late, s.Open(earliest))
	if _, open := s.Counts(); open > maxPressureRanges {
		t.Fatalf("open = %d, want at most %d", open, maxPressureRanges)
	}
	for _, at := range []time.Time{earliest, base} {
		if ok, _ := s.Overlaps(at, at); !ok {
			t.Fatalf("the span opened at %v is not covered while it is open", at)
		}
	}
	// The spans opened within the cap close; those joined past it do not yet.
	for _, c := range closers {
		c()
		c() // idempotent
	}
	if _, open := s.Counts(); open == 0 {
		t.Fatal("no span open while spans joined past the cap have not closed")
	}
	if ok, _ := s.Overlaps(time.Now().Add(-time.Millisecond), time.Now()); !ok {
		t.Fatal("a span joined past the cap closed with the span it joined")
	}
	for _, c := range late {
		c()
	}
	if _, open := s.Counts(); open != 0 {
		t.Fatalf("open = %d after every closer ran, want 0", open)
	}
	for _, at := range []time.Time{earliest, base} {
		if ok, _ := s.Overlaps(at, at); !ok {
			t.Fatalf("the span opened at %v is not covered once closed", at)
		}
	}
	if ok, _ := s.Overlaps(time.Now().Add(time.Second), time.Now().Add(2*time.Second)); ok {
		t.Fatal("a span still extends past its close")
	}
}

// Past the cap, exactly the smallest gaps are joined, down to keep spans.
func TestJoinNearestJoinsTheSmallestGaps(t *testing.T) {
	t.Parallel()
	base := time.Now().Round(0)
	ms := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
	// Gaps: 1, 1, 5, 1, 9 (ms), six spans.
	spans := []pressureRange{{ms(0), ms(1)}, {ms(2), ms(3)}, {ms(4), ms(5)}, {ms(10), ms(11)}, {ms(12), ms(13)}, {ms(22), ms(23)}}
	got := joinNearest(slices.Clone(spans), 4)
	want := []pressureRange{{ms(0), ms(5)}, {ms(10), ms(11)}, {ms(12), ms(13)}, {ms(22), ms(23)}}
	if !slices.Equal(got, want) {
		t.Fatalf("joinNearest(keep 4) = %v, want %v (the first two of the three 1 ms gaps)", got, want)
	}
	got = joinNearest(slices.Clone(spans), 2)
	want = []pressureRange{{ms(0), ms(13)}, {ms(22), ms(23)}}
	if !slices.Equal(got, want) {
		t.Fatalf("joinNearest(keep 2) = %v, want %v", got, want)
	}
	if got := joinNearest(slices.Clone(spans), 6); len(got) != 6 {
		t.Fatalf("joinNearest(keep 6) joined %d spans, want none", 6-len(got))
	}
}

// Past the cap, the spans that end before the consumer's watermark are joined
// into one first, however wide their gaps; the smallest gaps after it only
// once that is not room enough.
func TestJoinForRoomJoinsWhatIsCheckedFirst(t *testing.T) {
	t.Parallel()
	base := time.Now().Round(0)
	ms := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
	// Gaps: 9, 9, 1, 1, 1 (ms); checked from 21 ms: the first three end before.
	spans := []pressureRange{{ms(0), ms(1)}, {ms(10), ms(11)}, {ms(20), ms(21)}, {ms(22), ms(23)}, {ms(24), ms(25)}, {ms(26), ms(27)}}
	got := joinForRoom(slices.Clone(spans), 4, ms(22))
	want := []pressureRange{{ms(0), ms(21)}, {ms(22), ms(23)}, {ms(24), ms(25)}, {ms(26), ms(27)}}
	if !slices.Equal(got, want) {
		t.Fatalf("joinForRoom(keep 4, checked 22ms) = %v, want %v", got, want)
	}
	// Not room enough: then the smallest gaps, here all 1 ms, the oldest
	// first.
	got = joinForRoom(slices.Clone(spans), 2, ms(22))
	want = []pressureRange{{ms(0), ms(25)}, {ms(26), ms(27)}}
	if !slices.Equal(got, want) {
		t.Fatalf("joinForRoom(keep 2, checked 22ms) = %v, want %v", got, want)
	}
	// No watermark: the smallest gaps, as joinNearest.
	if got, want := joinForRoom(slices.Clone(spans), 4, time.Time{}), joinNearest(slices.Clone(spans), 4); !slices.Equal(got, want) {
		t.Fatalf("joinForRoom without a watermark = %v, want %v", got, want)
	}
}

// checkSpans fails the test if the closed spans are not what Overlaps' binary
// search needs: each one's start no later than its end, sorted, disjoint and
// not touching, with no monotonic clock reading (a comparison between one with
// and one without uses the wall clock, and one between two with uses the
// monotonic one, so a mix can be ordered two ways).
func checkSpans(t *testing.T, s *Spans) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.closed {
		if r.end.Before(r.start) {
			t.Fatalf("closed span %d of %d ends before it starts: [%v, %v]", i, len(s.closed), r.start, r.end)
		}
		for _, x := range []time.Time{r.start, r.end} {
			if strings.Contains(x.String(), " m=") {
				t.Fatalf("closed span %d of %d holds a monotonic clock reading: %v", i, len(s.closed), x)
			}
		}
		if i > 0 && !s.closed[i-1].end.Before(r.start) {
			t.Fatalf("closed spans %d and %d are out of order or touch: [%v, %v] then [%v, %v]",
				i-1, i, s.closed[i-1].start, s.closed[i-1].end, r.start, r.end)
		}
	}
	for i, o := range s.open {
		if strings.Contains(o.start.String(), " m=") {
			t.Fatalf("open span %d holds a monotonic clock reading: %v", i, o.start)
		}
	}
}

// A span Open returned for a start a little ahead of the clock that closes it
// (a wire time converted from the kernel clock, or the wall clock stepped back)
// ends before it starts. Stored as it was, it broke the order of the closed
// spans' ends that Overlaps searches, and a span before it was no longer found:
// a test case over that span was saved without its mocks.
func TestSpansAClosedSpanThatEndsBeforeItStartsHidesNothing(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(time.Hour).Round(0) // ahead of the clock the closer reads
	ms := func(f float64) time.Time { return base.Add(time.Duration(f * float64(time.Millisecond))) }
	s.Record(ms(0), ms(1))
	s.Record(ms(5), ms(6)) // the span a test case overlaps
	s.Record(ms(20), ms(21))
	s.Record(ms(30), ms(31))
	s.Open(ms(8))() // closed at once, at a time.Now() before its start
	checkSpans(t, &s)
	if ok, _ := s.Overlaps(ms(5.5), ms(5.6)); !ok {
		t.Fatal("a test case over [5ms, 6ms] is not covered once a span that closed before its start was added")
	}
	if ok, _ := s.Overlaps(ms(8), ms(8)); !ok {
		t.Fatal("the span that closed before its start covers nothing, not even its start")
	}
	if ok, _ := s.Overlaps(ms(10), ms(15)); ok {
		t.Fatal("the span that closed before its start covers past it")
	}
}

// What Spans keeps carries no monotonic clock reading, whatever it was given:
// Open's closer reads time.Now(), and callers pass both kinds.
func TestSpansKeepWallTimesOnly(t *testing.T) {
	t.Parallel()
	var s Spans
	now := time.Now() // carries a monotonic reading
	s.Record(now.Add(-3*time.Second), now.Add(-2*time.Second))
	s.Record(now.Add(-time.Second).Round(0), now.Add(-500*time.Millisecond))
	closeIt := s.Open(now.Add(-100 * time.Millisecond))
	keepOpen := s.Open(now.Add(-50 * time.Millisecond))
	checkSpans(t, &s)
	closeIt()
	checkSpans(t, &s)
	_ = keepOpen
}

// Past the cap, the spans of the test cases still to be checked stay as they
// were: those that end before every test case the consumer holds are joined
// first (CheckedBefore). Here the newest gaps are the smallest, so without
// the watermark the join takes them, and a pending test case in one is left
// out.
func TestSpansKeepThePendingTestCasesSpansExact(t *testing.T) {
	t.Parallel()
	for _, watermark := range []bool{true, false} {
		var s Spans
		base := time.Now().Add(-time.Hour).Round(0)
		// Span i lasts 1ms, and the gap after it is n-i µs: the newest gaps
		// are the smallest.
		const n = maxPressureRanges + maxPressureRanges/4 + 1 // one join
		starts := make([]time.Time, n+1)
		starts[0] = base
		for i := 1; i <= n; i++ {
			starts[i] = starts[i-1].Add(time.Millisecond + time.Duration(n-i+1)*time.Microsecond)
		}
		at := func(i int) time.Time { return starts[i] }
		// The consumer still holds test cases from span `pending` on.
		pending := n - maxPressureRanges/2
		if watermark {
			s.CheckedBefore(at(pending))
		}
		for i := 0; i < n; i++ {
			s.Record(at(i), at(i).Add(time.Millisecond))
		}
		checkSpans(t, &s)
		if closed, _ := s.Counts(); closed > maxPressureRanges {
			t.Fatalf("closed = %d, want at most %d", closed, maxPressureRanges)
		}
		covered := 0
		for _, i := range []int{n - 2, n - 3, pending} {
			gap := at(i).Add(time.Millisecond + (at(i+1).Sub(at(i))-time.Millisecond)/2)
			if ok, _ := s.Overlaps(gap, gap); ok {
				covered++
			}
		}
		switch {
		case watermark && covered > 0:
			t.Fatal("a gap after the watermark, where a test case is still to be checked, is covered")
		case !watermark && covered == 0:
			t.Fatal("without the watermark the smallest gaps are not the ones joined: the test does not show what the watermark keeps")
		}
		for _, i := range []int{0, 1, 100, pending - 1, n - 1} {
			mid := at(i).Add(500 * time.Microsecond)
			if ok, _ := s.Overlaps(mid, mid); !ok {
				t.Fatalf("span %d of %d is no longer covered", i, n)
			}
		}
	}
}

// The watermark only moves forward.
func TestSpansCheckedBeforeOnlyMovesForward(t *testing.T) {
	t.Parallel()
	var s Spans
	now := time.Now().Round(0)
	s.CheckedBefore(now)
	s.CheckedBefore(now.Add(-time.Minute))
	s.CheckedBefore(time.Time{})
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked.Equal(now) {
		t.Fatalf("checked = %v, want %v", s.checked, now)
	}
}

// Open and close, from many goroutines, past the cap: spans that join an open
// one (and move its start back), and the spans they joined closing while they
// are open. Every span stays covered from its start while it is open and once
// it has closed, the closed spans stay in order, and every closer, once run,
// is counted once.
func TestSpansOpenAndClosePastTheCapConcurrently(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(-time.Hour)
	hosts := make([]func(), maxPressureRanges)
	for i := range hosts {
		hosts[i] = s.Open(base.Add(time.Duration(i) * time.Millisecond))
	}
	const workers, each = 8, 200
	var wg sync.WaitGroup
	errs := make(chan string, workers*each)
	starts := make(chan time.Time, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				// Before, among and after the hosts' starts.
				start := base.Add(time.Duration((w*each+j)%(maxPressureRanges+400)-200) * time.Millisecond)
				closeIt := s.Open(start)
				if ok, _ := s.Overlaps(start, start); !ok {
					errs <- "a span joined past the cap is not covered while it is open: " + start.String()
				}
				if j%3 == 0 {
					closeIt()
					closeIt()
				} else {
					defer closeIt()
				}
				starts <- start
			}
		}(w)
	}
	// The hosts close while the others join them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, c := range hosts {
			c()
		}
	}()
	wg.Wait()
	close(errs)
	close(starts)
	for e := range errs {
		t.Fatal(e)
	}
	if _, open := s.Counts(); open != 0 {
		t.Fatalf("open = %d after every closer ran, want 0", open)
	}
	checkSpans(t, &s)
	for i := range hosts {
		at := base.Add(time.Duration(i) * time.Millisecond)
		if ok, _ := s.Overlaps(at, at); !ok {
			t.Fatalf("host %d's start is not covered once closed", i)
		}
	}
	for start := range starts {
		if ok, _ := s.Overlaps(start, start); !ok {
			t.Fatalf("the span opened at %v is not covered once closed", start)
		}
	}
}

// captureClock is how far a test's capture has got.
type captureClock struct{ ns atomic.Int64 }

func (c *captureClock) set(t time.Time)          { c.ns.Store(t.UnixNano()) }
func (c *captureClock) now() time.Time           { return time.Unix(0, c.ns.Load()) }
func at(t0 time.Time, d time.Duration) time.Time { return t0.Add(d) }

func overlaps(s *Spans, from, to time.Time) bool {
	ok, _ := s.Overlaps(from, to)
	return ok
}

// P20: on the capture's clock a stopped connection's span is the stamps of the
// chunks it carried: it opens at the stop's stamp, each chunk extends it to its
// stamp, and once the capture has got the idle grace past the last chunk with
// none in between it ends at that chunk. The capture here is far in the past,
// so a span dated by the agent's clock would reach the present.
func TestUnrecordedConnOnCaptureSpansAreTheChunksStamps(t *testing.T) {
	var spans Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	clock.set(t0)
	u := NewUnrecordedConnOnCapture(func(s time.Time) CaptureSpan { return spans.OpenAt(s, testIdleGrace) },
		clock.now, testIdleGrace, testIdleCheck)
	defer u.End()

	u.Note(t0) // before the stop: recorded traffic, no span
	if _, open := spans.Counts(); open != 0 {
		t.Fatal("a span opened before the stop")
	}
	u.Stop(t0)
	u.Note(at(t0, 10*time.Millisecond))
	clock.set(at(t0, 15*time.Millisecond)) // within the grace of the last chunk

	if !overlaps(&spans, at(t0, 5*time.Millisecond), at(t0, 6*time.Millisecond)) {
		t.Fatal("the traffic after the stop is not covered")
	}
	// Open, it reaches no further than the grace past its last chunk.
	past := at(t0, 10*time.Millisecond+testIdleGrace+time.Millisecond)
	if overlaps(&spans, past, past.Add(time.Millisecond)) {
		t.Fatal("an open span reached past its idle grace")
	}
	if overlaps(&spans, time.Now().Add(-time.Millisecond), time.Now()) {
		t.Fatal("an open span reached the agent's clock")
	}

	// The capture gets past the grace: it ends at the last chunk.
	clock.set(at(t0, time.Hour))
	waitUntil(t, 30*time.Second, "the span to close once the capture passed its grace", func() bool {
		_, open := spans.Counts()
		return open == 0
	})
	if !overlaps(&spans, at(t0, 10*time.Millisecond), at(t0, 10*time.Millisecond)) {
		t.Fatal("the closed span does not reach its last chunk")
	}
	if overlaps(&spans, at(t0, 11*time.Millisecond), at(t0, 12*time.Millisecond)) {
		t.Fatal("the closed span reaches past its last chunk")
	}

	// Traffic after a gap: a span of its own, from its own stamp.
	u.Note(at(t0, 500*time.Millisecond))
	if !overlaps(&spans, at(t0, 500*time.Millisecond), at(t0, 500*time.Millisecond)) {
		t.Fatal("traffic after the gap is not covered")
	}
	if overlaps(&spans, at(t0, 200*time.Millisecond), at(t0, 300*time.Millisecond)) {
		t.Fatal("the gap between the two spells is covered")
	}

	// The connection ends: its span ends at its last chunk.
	u.Note(at(t0, 600*time.Millisecond))
	u.End()
	if _, open := spans.Counts(); open != 0 {
		t.Fatal("End left a span open")
	}
	if overlaps(&spans, at(t0, 601*time.Millisecond), time.Now()) {
		t.Fatal("the span of a connection that ended reaches past its last chunk")
	}
}

// Idleness is a gap between the capture's stamps, not between the agent's
// wake-ups: however long the agent waits, a span stays open while the capture
// has not got the idle grace past its last chunk (a starved capture reader,
// behind the traffic), and closes only once it has.
func TestUnrecordedConnOnCaptureIdlenessIsAGapBetweenStamps(t *testing.T) {
	var spans Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	clock.set(t0)
	u := NewUnrecordedConnOnCapture(func(s time.Time) CaptureSpan { return spans.OpenAt(s, testIdleGrace) },
		clock.now, testIdleGrace, testIdleCheck)
	defer u.End()
	u.Stop(t0)
	// Many idle graces of the agent's clock, many checks: the capture has
	// not moved, so the connection is not known to be idle.
	time.Sleep(10 * testIdleGrace)
	if _, open := spans.Counts(); open != 1 {
		t.Fatal("the span closed on the agent's clock while the capture had not got past its grace")
	}
	// The starved reader catches up and hands over a chunk from within the
	// grace: the same span covers it, without a gap.
	u.Note(at(t0, testIdleGrace/2))
	clock.set(at(t0, time.Hour))
	waitUntil(t, 30*time.Second, "the span to close", func() bool {
		_, open := spans.Counts()
		return open == 0
	})
	if closed, _ := spans.Counts(); closed != 1 {
		t.Fatalf("%d closed spans, want one: the late chunk within the grace was split off", closed)
	}
	if !overlaps(&spans, at(t0, testIdleGrace/4), at(t0, testIdleGrace/4)) {
		t.Fatal("the stretch up to the late chunk is not covered")
	}
}

// The span a stop opens reaches the newest chunk noted before it, whatever
// instant the stop is dated at: a caller may date it before chunks it had
// already noted. It reaches them at once, and ends no earlier than the newest,
// however far its grace and the horizon are from them.
func TestUnrecordedConnOnCaptureTheSpanAtTheStopReachesChunksNotedBeforeIt(t *testing.T) {
	var spans Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	backlog := at(t0, 5*testIdleGrace) // past the grace of the stop
	clock.set(backlog)                 // the capture has got there, and no further
	u := NewUnrecordedConnOnCapture(func(s time.Time) CaptureSpan { return spans.OpenAt(s, testIdleGrace) },
		clock.now, testIdleGrace, testIdleCheck)
	defer u.End()

	u.Note(at(t0, 3*testIdleGrace)) // carried before the stop, not consumed
	u.Note(backlog)
	u.Stop(t0)
	if !overlaps(&spans, backlog.Add(-time.Millisecond), backlog) {
		t.Fatal("the span the stop opened does not reach the chunks noted before it, stamped after it: a test case over the parser's backlog is saved without its mocks")
	}
	u.End()
	if !overlaps(&spans, backlog.Add(-time.Millisecond), backlog) {
		t.Fatal("the span the stop opened ended before the chunks noted before it, stamped after it")
	}
	if overlaps(&spans, backlog.Add(time.Millisecond), backlog.Add(2*time.Millisecond)) {
		t.Fatal("the span reaches past the newest chunk the connection carried")
	}
}

// Past the cap on open spans, a new span joins the open one whose start is
// nearest. A span opened with Open (the agent's clock) that joins one on the
// capture keeps the joined span on the agent's clock: it reaches now, however
// far the horizon lags, as it would alone.
func TestSpansALegacySpanJoinedPastTheCapStaysOnTheAgentsClock(t *testing.T) {
	t.Parallel()
	var s Spans
	var clock captureClock
	t0 := time.Now().Add(-time.Hour).Round(0)
	clock.set(t0.Add(-time.Minute)) // the horizon lags behind every span
	s.SetHorizon(clock.now)
	for i := 0; i < maxPressureRanges; i++ {
		s.OpenAt(t0.Add(time.Duration(i)*time.Millisecond), time.Millisecond)
	}
	closeLegacy := s.Open(t0) // joins the capture span opened at t0
	defer closeLegacy()
	if _, open := s.Counts(); open != maxPressureRanges {
		t.Fatalf("%d open spans, want %d: the legacy span did not join one past the cap", open, maxPressureRanges)
	}
	if !overlaps(&s, time.Now().Add(-time.Second), time.Now()) {
		t.Fatal("a legacy span joined to a capture span past the cap stops short of now: a test case recorded while it is open is saved without its mocks")
	}
}

// A span on the capture reaches at least its last chunk, open or closed,
// however far the horizon lags behind it: it does while any connection's
// forwarder is stamping a chunk (relay.Relay.CapturedTo collapses to that
// direction's previous stamp, minutes old on an idle keep-alive connection).
func TestSpansACaptureSpanReachesItsLastChunkPastALaggingHorizon(t *testing.T) {
	var s Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	s.SetHorizon(clock.now)
	o := s.OpenAt(t0, time.Second)
	o.Extend(at(t0, time.Second))
	clock.set(t0.Add(-time.Minute)) // collapsed, behind the span's start
	if !overlaps(&s, at(t0, 900*time.Millisecond), at(t0, time.Second)) {
		t.Fatal("an open span stops at the lagging horizon, short of its last chunk")
	}
	o.Close(time.Time{}) // at the horizon
	if !overlaps(&s, at(t0, 900*time.Millisecond), at(t0, time.Second)) {
		t.Fatal("a span closed at a lagging horizon ends short of its last chunk")
	}
	if overlaps(&s, at(t0, 1100*time.Millisecond), at(t0, 1200*time.Millisecond)) {
		t.Fatal("a span closed at a lagging horizon reaches past its last chunk")
	}
}

// An open span on the capture extends to its last chunk, and on to the horizon
// no further than its grace past it, never looking at the agent's clock; one
// opened with Open is on the agent's clock: it extends to now, and its closer
// ends it then, however far the horizon lags.
func TestSpansAnOpenSpanExtendsOnItsOwnClock(t *testing.T) {
	var s Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	s.SetHorizon(clock.now)
	const grace = time.Second

	o := s.OpenAt(t0, grace)
	o.Extend(at(t0, time.Second))
	clock.set(at(t0, 1500*time.Millisecond))
	if !overlaps(&s, at(t0, 1400*time.Millisecond), at(t0, 1450*time.Millisecond)) {
		t.Fatal("an open span does not reach the horizon within its grace")
	}
	if overlaps(&s, at(t0, 1600*time.Millisecond), at(t0, 1700*time.Millisecond)) {
		t.Fatal("an open span reaches past the horizon")
	}
	clock.set(at(t0, 5*time.Second))
	if overlaps(&s, at(t0, 2500*time.Millisecond), at(t0, 3*time.Second)) {
		t.Fatal("an open span reaches past its grace")
	}
	o.Close(at(t0, time.Second))
	o.Close(at(t0, 4*time.Second)) // idempotent: the first close counts
	if overlaps(&s, at(t0, 1100*time.Millisecond), time.Now()) {
		t.Fatal("a span closed at its last chunk reaches past it")
	}

	// A span opened with Open is on the agent's clock, as it was before the
	// capture's clock existed: its traffic is not stamped, so it reaches now,
	// however far the horizon lags behind the clock, and its closer ends it at
	// the clock.
	legacy := s.Open(at(t0, 10*time.Second))
	clock.set(at(t0, 20*time.Second)) // the horizon lags far behind the clock
	if !overlaps(&s, at(t0, 21*time.Second), time.Now()) {
		t.Fatal("a span opened with Open stops at the lagging horizon, not the clock")
	}
	legacy()
	closedBy := time.Now()
	if !overlaps(&s, at(t0, 21*time.Second), closedBy.Add(-time.Millisecond)) {
		t.Fatal("Open's closer ended its span at the lagging horizon, not the clock")
	}
	if overlaps(&s, closedBy.Add(time.Second), closedBy.Add(2*time.Second)) {
		t.Fatal("Open's closer did not end its span")
	}

	var nilSpans *Spans
	nilSpans.OpenAt(t0, grace).Extend(t0) // nil-safe
	nilSpans.OpenAt(t0, grace).Close(t0)
	if s.OpenAt(time.Time{}, grace) != nil {
		t.Fatal("a zero start opened a span")
	}
}

// horizonWatermark is a watermark that tells how far its capture has got.
type horizonWatermark struct{ to time.Time }

func (horizonWatermark) Settled(_, _ string, _, _ time.Time) bool { return true }
func (h horizonWatermark) CapturedTo() time.Time                  { return h.to }

// The recording's open orphan spans extend as far as its capture has got: the
// earliest any of its watermarks tells; the clock when none can.
func TestSyncMockManagerCapturedTo(t *testing.T) {
	m := New(nil)
	if d := time.Since(m.CapturedTo()); d < 0 || d > time.Minute {
		t.Fatalf("CapturedTo with no watermark is %v off the clock", d)
	}
	t0 := time.Unix(5000, 0)
	defer m.AddWatermark(horizonWatermark{to: at(t0, 2*time.Second)})()
	defer m.AddWatermark(horizonWatermark{to: at(t0, time.Second)})()
	if got := m.CapturedTo(); !got.Equal(at(t0, time.Second)) {
		t.Fatalf("CapturedTo %v, want the earliest horizon %v", got, at(t0, time.Second))
	}
	o := m.OpenOrphanSpan(t0, 0)
	if ok, _ := m.WasMockOrphanedInWindow(at(t0, 500*time.Millisecond), at(t0, 600*time.Millisecond)); ok {
		t.Fatal("a span with no traffic after its start and no grace covers what comes after it")
	}
	// A span whose grace runs past the horizon reaches the horizon, the
	// earliest watermark's, and no further: the manager's spans take it from
	// CapturedTo (New). On the agent's clock it reached its whole grace.
	wide := m.OpenOrphanSpan(t0, time.Hour)
	if ok, _ := m.WasMockOrphanedInWindow(at(t0, 900*time.Millisecond), at(t0, 950*time.Millisecond)); !ok {
		t.Fatal("an open span does not reach the capture's horizon")
	}
	if ok, _ := m.WasMockOrphanedInWindow(at(t0, 1100*time.Millisecond), at(t0, 1200*time.Millisecond)); ok {
		t.Fatal("an open span reaches past the capture's horizon: the manager's spans do not take it from CapturedTo")
	}
	wide.Close(t0)
	// An orphan window (OpenOrphanWindow, what enterprise opens) is on the
	// agent's clock: it reaches now, past the lagging horizon.
	closeLegacy := m.OpenOrphanWindow(t0)
	if ok, _ := m.WasMockOrphanedInWindow(at(t0, 1100*time.Millisecond), time.Now()); !ok {
		t.Fatal("an orphan window stops at the capture's horizon, short of the agent's clock")
	}
	closeLegacy()
	o.Close(t0)
}

// The package's manager (Get) takes its orphan spans' horizon from its own
// CapturedTo, as every manager New makes does. Read off the spans, not by
// adding a watermark: the package manager is shared by the tests that run in
// parallel.
func TestThePackageManagersSpansHaveItsHorizon(t *testing.T) {
	if Get().orphans.horizon.Load() == nil {
		t.Fatal("the package manager's orphan spans have no horizon: open spans reach the agent's clock")
	}
	if New(nil).orphans.horizon.Load() == nil {
		t.Fatal("a new manager's orphan spans have no horizon")
	}
}

// A capture horizon that lags (relay.Relay.CapturedTo returns a direction's
// previous stamp while one of its chunks is being stamped, which on an idle
// keep-alive connection can be minutes old) must not narrow an orphan window
// opened with the old API (OpenOrphanWindow: what enterprise's managerSpans and
// capture_drop_report open): it covers a test case recorded inside it while it
// is open, and after its closer ran meanwhile.
func TestOrphanWindow_ALaggingHorizonDoesNotNarrowIt(t *testing.T) {
	m := New(nil)
	collapsed := time.Now().Add(-time.Minute) // an idle tracked connection's last stamp
	defer m.AddWatermark(horizonWatermark{to: collapsed})()

	start := time.Now()
	closeWin := m.OpenOrphanWindow(start)
	tcStart, tcEnd := start.Add(10*time.Millisecond), start.Add(20*time.Millisecond)
	for time.Now().Before(tcEnd) { // the test case ends inside the open window
		time.Sleep(time.Millisecond)
	}
	if ok, _ := m.WasMockOrphanedInWindow(tcStart, tcEnd); !ok {
		t.Errorf("an open orphan window from %v does not cover a test case inside it while the horizon lags at %v",
			start.Format(time.RFC3339Nano), collapsed.Format(time.RFC3339Nano))
	}
	closeWin()
	if ok, _ := m.WasMockOrphanedInWindow(tcStart, tcEnd); !ok {
		t.Errorf("an orphan window closed while the horizon lagged no longer covers the traffic it followed")
	}
}

// An opener that opens no span (NoSpan: the recording is stopping) leaves the
// connection with nothing to follow: no follower runs for a span that covers
// nothing, at the stop or at any burst after it.
func TestUnrecordedConnOnCaptureFollowsNoSpanThatOpensNone(t *testing.T) {
	var clock captureClock
	t0 := time.Unix(5000, 0)
	clock.set(t0)
	u := NewUnrecordedConnOnCapture(func(time.Time) CaptureSpan { return NoSpan }, clock.now, testIdleGrace, testIdleCheck)
	defer u.End()
	u.Stop(t0)
	u.Note(at(t0, 10*time.Millisecond))
	u.mu.Lock()
	cur := u.cur
	u.mu.Unlock()
	if cur != nil || u.running.Load() {
		t.Fatalf("an opener that opens no span left a span (%v) or a follower running (%v)", cur, u.running.Load())
	}
}

// The follower closes a span at the chunk its idle check judged idle past. A
// chunk the connection carries as it closes (Note moves the last stamp before
// it takes the lock) opens a span of its own: closed at that chunk instead,
// the span covered the whole idle stretch before it, minutes maybe, and left
// out every test case in it on every connection.
func TestUnrecordedConnOnCaptureClosesAtTheChunkItJudgedIdle(t *testing.T) {
	var spans Spans
	var clock captureClock
	t0 := time.Unix(5000, 0)
	clock.set(t0)
	u := NewUnrecordedConnOnCapture(func(s time.Time) CaptureSpan { return spans.OpenAt(s, testIdleGrace) },
		clock.now, testIdleGrace, testIdleCheck)
	defer u.End()
	late := t0.Add(time.Minute) // a chunk after a minute of idleness
	var fired atomic.Bool
	u.beforeIdleClose = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		go u.Note(late) // its stamp lands outside mu, before Note waits for it
		for u.last.Load() != late.UnixNano() {
			runtime.Gosched()
		}
	}
	u.Stop(t0)
	clock.set(t0.Add(2 * testIdleGrace))
	waitUntil(t, 30*time.Second, "the idle close to run", func() bool { return fired.Load() })
	clock.set(late.Add(time.Hour))
	waitUntil(t, 30*time.Second, "every span to close", func() bool { _, open := spans.Counts(); return open == 0 })
	if mid := t0.Add(30 * time.Second); overlaps(&spans, mid, mid) {
		t.Fatal("the idle minute is covered: the span closed at a chunk noted after its idle check")
	}
	if !overlaps(&spans, late, late) {
		t.Fatal("the chunk noted as the span closed is in no span of its own")
	}
}
