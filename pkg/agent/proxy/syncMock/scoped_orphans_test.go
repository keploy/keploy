package manager

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

// servedByOneOf is a scope naming the test cases the processes in procs
// served.
func servedByOneOf(procs ...uint32) OrphanScope {
	return func(servedBy uint32, _ time.Time) bool {
		for _, p := range procs {
			if p == servedBy {
				return true
			}
		}
		return false
	}
}

// A scoped span leaves out the test cases its scope names, and those whose
// serving process the capture could not tell; the others it overlaps are kept.
// A nil scope is every test case's, as a global span is.
func TestAScopedOrphanSpanLeavesOutOnlyTheTestCasesItsScopeNames(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(1_000_000, 0)
	m.RecordScopedOrphanWindow(base, base.Add(10*time.Second), servedByOneOf(7, 9), "TLS of TGID 7 not decrypted")
	in := func(servedBy uint32) bool {
		ok, _, _ := m.WasMockOrphanedFor(base.Add(time.Second), base.Add(2*time.Second), servedBy)
		return ok
	}
	if !in(7) || !in(9) {
		t.Fatal("a test case the span's scope names was kept")
	}
	if in(8) {
		t.Fatal("a test case another process served, which the span's scope does not name, was left out")
	}
	if !in(0) {
		t.Fatal("a test case whose serving process the capture could not tell was kept: a scope cannot clear what it cannot name")
	}
	if ok, _, _ := m.WasMockOrphanedFor(base.Add(11*time.Second), base.Add(12*time.Second), 7); ok {
		t.Fatal("a test case after the span was left out")
	}
	// WasMockOrphanedInWindow, which has no test case's server, takes every
	// scoped span that overlaps.
	if ok, n := m.WasMockOrphanedInWindow(base.Add(time.Second), base.Add(2*time.Second)); !ok || n != 1 {
		t.Fatalf("WasMockOrphanedInWindow over a scoped span = %v, %d; want true, 1", ok, n)
	}

	g := &SyncMockManager{}
	g.RecordScopedOrphanWindow(base, base.Add(10*time.Second), nil, "everyone's")
	if ok, _, _ := g.WasMockOrphanedFor(base.Add(time.Second), base.Add(2*time.Second), 8); !ok {
		t.Fatal("a span with no scope kept a test case it overlaps")
	}
}

// What leaves a test case out says why: each overlapping scoped span's reason,
// once, in the spans' order; a global span counts but gives none.
func TestTheReasonsOfTheSpansThatLeaveATestCaseOutAreSaid(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(2_000_000, 0)
	m.RecordScopedOrphanWindow(base.Add(2*time.Second), base.Add(5*time.Second), servedByOneOf(7), "second")
	m.RecordScopedOrphanWindow(base, base.Add(5*time.Second), servedByOneOf(7), "first")
	m.RecordScopedOrphanWindow(base, base.Add(5*time.Second), servedByOneOf(7), "first")
	m.RecordScopedOrphanWindow(base, base.Add(5*time.Second), servedByOneOf(8), "not this one's")
	m.RecordOrphanWindow(base, base.Add(5*time.Second))
	ok, n, reasons := m.WasMockOrphanedFor(base.Add(3*time.Second), base.Add(4*time.Second), 7)
	if !ok || n != 4 {
		t.Fatalf("WasMockOrphanedFor = %v, %d; want true and 4 spans (three scoped ones naming it and the global one)", ok, n)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(reasons, want) {
		t.Fatalf("the reasons said are %q, want %q", reasons, want)
	}
}

// The closer of an open scoped span ends it where it is told: a loss that
// ended before the caller learned of it (a process's exit, which the kernel
// recorded as it happened) leaves out no test case that began after it. An end
// later than the closer's call is kept (the test cases' clock can be ahead of
// a wall clock stepped back since), and one before the span's start is its
// start.
func TestAnOpenScopedSpanEndsWhereItsCloserSays(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	start := time.Now().Add(-time.Hour)
	exit := start.Add(10 * time.Minute)
	end := m.OpenScopedOrphanWindow(start, servedByOneOf(7), "TLS of TGID 7")
	if ok, _, _ := m.WasMockOrphanedFor(exit.Add(time.Minute), exit.Add(2*time.Minute), 7); !ok {
		t.Fatal("an open span left a test case it covers in")
	}
	end(exit)
	end(time.Now()) // idempotent: the first end stands
	if ok, _, _ := m.WasMockOrphanedFor(exit.Add(time.Minute), exit.Add(2*time.Minute), 7); ok {
		t.Fatal("a test case after the span's end, back-dated to the loss's end, was left out")
	}
	if ok, _, _ := m.WasMockOrphanedFor(exit.Add(-time.Minute), exit.Add(-time.Second), 7); !ok {
		t.Fatal("a test case inside the closed span was kept")
	}

	// An end past the closer's call is kept: the test cases' clock can be
	// ahead of a wall clock stepped back since, and a later end leaves out
	// more, never fewer.
	future := m.OpenScopedOrphanWindow(start, nil, "to a later end")
	future(time.Now().Add(time.Hour))
	if ok, _, _ := m.WasMockOrphanedFor(time.Now().Add(time.Minute), time.Now().Add(2*time.Minute), 7); !ok {
		t.Fatal("a span closed with an end past its closer's call was cut to the call's wall-clock time")
	}

	before := m.OpenScopedOrphanWindow(start.Add(time.Hour/2), nil, "clamped")
	before(start)
	_, closed, open := m.scopedOrphans.counts()
	if closed != 3 || open != 0 {
		t.Fatalf("scoped spans closed %d, open %d; want 3 and 0", closed, open)
	}
	var clamped []OrphanSpanHit
	for _, h := range m.scopedOrphans.hits(start.Add(time.Hour/2), start.Add(time.Hour/2), 9) {
		if slices.Contains(h.Reasons, "clamped") {
			clamped = append(clamped, h)
		}
	}
	if len(clamped) != 1 || !clamped[0].End.Equal(wallTime(start.Add(time.Hour/2))) {
		t.Fatalf("a span closed before its start: %+v, want it to end at its start", clamped)
	}
}

// The global spans' closer takes an end too (Spans.OpenUntil), clamped as the
// scoped spans'; Open's closer ends at its call, as before.
func TestAGlobalOpenSpanEndsWhereItsCloserSays(t *testing.T) {
	t.Parallel()
	var s Spans
	start := time.Now().Add(-time.Hour)
	end := s.OpenUntil(start)
	end(start.Add(time.Minute))
	if ok, _ := s.Overlaps(start.Add(2*time.Minute), start.Add(3*time.Minute)); ok {
		t.Fatal("a span closed at start+1m reaches start+2m")
	}
	if ok, _ := s.Overlaps(start.Add(30*time.Second), start.Add(40*time.Second)); !ok {
		t.Fatal("a span closed at start+1m does not cover start+30s")
	}
	closeNow := s.Open(start.Add(10 * time.Minute))
	closeNow()
	if ok, _ := s.Overlaps(time.Now().Add(-time.Second), time.Now().Add(-time.Second)); !ok {
		t.Fatal("Open's closer no longer ends the span at its call")
	}
}

// A scoped span is never dropped: a test case still on its way to the record
// route can start before every one the route holds (CheckedBefore), and must
// still be judged against it. (A long request that ends after a short one is
// handed on after it.)
func TestAScopedSpanBeforeWhatTheRouteHoldsIsNeverDropped(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(4_000_000, 0)
	m.RecordScopedOrphanWindow(base.Add(2*time.Second), base.Add(5*time.Second), servedByOneOf(7), "TLS of TGID 7")
	m.CheckedBefore(base.Add(30 * time.Second))                                                                     // a short request of TGID 9's, held
	m.RecordScopedOrphanWindow(base.Add(40*time.Second), base.Add(41*time.Second), servedByOneOf(7), "a later one") // an add after it
	if ok, _, _ := m.WasMockOrphanedFor(base.Add(time.Second), base.Add(30*time.Second+2*time.Millisecond), 7); !ok {
		t.Fatal("a long request of TGID 7's over the span, handed on after a later-starting one was held, was let through")
	}
}

// Past the cap, the adjacent closed spans whose union is narrowest are
// joined, as many as the cap needs, each group into one span of every test
// case's (more is left out, never fewer) that keeps their reasons; a span
// joined is never dropped, nor is one joined again. An open span past the cap
// with fewer than two closed spans to join is kept as a global span, whose
// closer still ends it where it is told. Each span so treated is counted once.
func TestScopedSpansPastTheCapAreJoinedNeverDropped(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 4
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(5_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	m.RecordScopedOrphanWindow(at(0), at(1), servedByOneOf(7), "a")
	m.RecordScopedOrphanWindow(at(10), at(11), servedByOneOf(7), "b")
	m.RecordScopedOrphanWindow(at(100), at(101), servedByOneOf(7), "c")
	closeOpen := m.OpenScopedOrphanWindow(at(200), servedByOneOf(7), "open")
	m.CheckedBefore(at(50))
	// Past the cap: one join, of the narrowest union, a with b ([0, 11]).
	m.RecordScopedOrphanWindow(at(300), at(301), servedByOneOf(7), "d")
	if got := m.ScopedOrphansJoined(); got != 2 {
		t.Fatalf("%d spans joined, want 2 (a and b)", got)
	}
	ok, _, reasons := m.WasMockOrphanedFor(at(5), at(6), 8) // between a and b, another process's
	if !ok || !reflect.DeepEqual(reasons, []string{"a", "b"}) {
		t.Fatalf("a test case between joined spans, of another process: %v %q; want left out, saying their reasons", ok, reasons)
	}
	for _, s := range []int{100, 300} {
		if ok, _, _ := m.WasMockOrphanedFor(at(s), at(s), 8); ok {
			t.Fatalf("a span not joined (at %ds) lost its scope", s)
		}
	}
	m.RecordScopedOrphanWindow(at(400), at(401), servedByOneOf(7), "e") // past it again: one more join
	if got := m.ScopedOrphansJoined(); got < 3 || got > 4 {
		t.Fatalf("%d spans joined, want 3 or 4 (one more join, of the narrowest union)", got)
	}
	for _, s := range []int{0, 5, 11, 100, 300, 400} {
		if ok, _, _ := m.WasMockOrphanedFor(at(s), at(s), 7); !ok {
			t.Fatalf("a test case of TGID 7 at %ds, inside a span, was let through", s)
		}
		if ok, _, _ := m.WasMockOrphanedFor(at(s), at(s), 0); !ok {
			t.Fatalf("a test case with no serving process at %ds, inside a span, was let through", s)
		}
	}
	if ok, _, _ := m.WasMockOrphanedFor(at(250), at(250), 8); ok {
		t.Fatal("a test case of another process in a gap no join closed was left out")
	}
	// Every span open: the next is kept as a global span.
	m2 := &SyncMockManager{}
	for i := 0; i < 4; i++ {
		_ = m2.OpenScopedOrphanWindow(at(i), servedByOneOf(7), "open")
	}
	end := m2.OpenScopedOrphanWindow(at(10), servedByOneOf(7), "past")
	if m2.ScopedOrphansJoined() != 1 {
		t.Fatalf("the span kept as a global one past the cap is not counted: %d", m2.ScopedOrphansJoined())
	}
	if _, closed, open := m2.scopedOrphans.counts(); closed != 0 || open != 4 {
		t.Fatalf("past the cap, the scoped spans are %d closed and %d open; want 0 and 4", closed, open)
	}
	end(at(20))
	if ok, _ := m2.orphans.Overlaps(at(15), at(15)); !ok {
		t.Fatal("the span kept as a global one does not cover its time")
	}
	if ok, _ := m2.orphans.Overlaps(at(25), at(25)); ok {
		t.Fatal("the span kept as a global one reaches past the end its closer gave")
	}
	closeOpen(at(210))
}

// A join that finds only one span to take (the rest open, or the one span an
// earlier join made) never drops it, whichever place it holds: two spans join
// or nothing changes.
func TestAJoinWithOneSpanToTakeDropsNone(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 4
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	base := time.Unix(6_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }

	// An earlier join's span, not last, and every later add.
	m := &SyncMockManager{}
	m.RecordScopedOrphanWindow(at(0), at(1), servedByOneOf(7), "a")
	m.RecordScopedOrphanWindow(at(10), at(11), servedByOneOf(7), "b")
	m.RecordScopedOrphanWindow(at(100), at(101), servedByOneOf(7), "c")
	closeOpen := m.OpenScopedOrphanWindow(at(200), servedByOneOf(7), "open")
	defer closeOpen(at(210))
	m.CheckedBefore(at(50))
	for i, s := range []int{300, 400, 500, 600, 700} {
		m.RecordScopedOrphanWindow(at(s), at(s+1), servedByOneOf(7), string(rune('d'+i)))
		for _, inside := range []int{0, 10, 100, s} {
			if ok, _, _ := m.WasMockOrphanedFor(at(inside), at(inside), 7); !ok {
				t.Fatalf("after the add at %ds, TGID 7's test case at %ds inside a span was let through", s, inside)
			}
			if ok, _, _ := m.WasMockOrphanedFor(at(inside), at(inside), 0); !ok {
				t.Fatalf("after the add at %ds, a test case with no serving process at %ds was let through", s, inside)
			}
		}
	}

	// One closed span among open ones, not last: it and the new one join.
	m2 := &SyncMockManager{}
	c1 := m2.OpenScopedOrphanWindow(at(20), servedByOneOf(7), "o1")
	m2.RecordScopedOrphanWindow(at(0), at(1), servedByOneOf(7), "B")
	c2 := m2.OpenScopedOrphanWindow(at(30), servedByOneOf(7), "o2")
	c3 := m2.OpenScopedOrphanWindow(at(40), servedByOneOf(7), "o3")
	defer func() { c1(at(50)); c2(at(50)); c3(at(50)) }()
	m2.RecordScopedOrphanWindow(at(60), at(61), servedByOneOf(7), "past the cap")
	if ok, _, _ := m2.WasMockOrphanedFor(at(0), at(0), 7); !ok {
		t.Fatal("span B was dropped at the cap")
	}
	if ok, _ := m2.WasMockOrphanedInWindow(at(0), at(0)); !ok {
		t.Fatal("WasMockOrphanedInWindow lost span B at the cap")
	}
	if ok, _, _ := m2.WasMockOrphanedFor(at(60), at(60), 8); !ok {
		t.Fatal("the span added past the cap, joined with B, does not cover its time")
	}
}

// A joined span keeps the first maxJoinedReasons reasons by name and counts
// the rest, a reason with "; " in it included, across joins of joins.
func TestAJoinedSpanKeepsItsReasonsAndCountsTheRest(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 4
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(7_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	// More spans than the cap and one joined span's names: the joins gather
	// the earliest ones into one span, past maxJoinedReasons.
	n := maxJoinedReasons + 3 + maxScopedOrphanSpans
	for i := 0; i < n; i++ {
		m.RecordScopedOrphanWindow(at(i), at(i), servedByOneOf(7), fmt.Sprintf("r%d; with a semicolon", i))
	}
	var named, more int
	for _, h := range m.scopedOrphans.hits(at(0), at(n), 7) {
		if len(h.Reasons) > maxJoinedReasons {
			t.Fatalf("a joined span names %d reasons, more than %d: %q", len(h.Reasons), maxJoinedReasons, h.Reasons)
		}
		named += len(h.Reasons)
		more += h.More
	}
	if named+more != n || more == 0 {
		t.Fatalf("the spans name %d reasons and count %d more, want %d in all, some counted past the names", named, more, n)
	}
	// The record route is told the named ones, and that there are more.
	_, _, reasons := m.WasMockOrphanedFor(at(0), at(n), 7)
	if len(reasons) != named+1 || reasons[len(reasons)-1] != UnnamedOrphanReasons {
		t.Fatalf("the reasons said are %q, want the %d named ones and then UnnamedOrphanReasons", reasons, named)
	}
}

// A test case whose window the capture left unstamped (a zero bound) is left
// out by every span: a test case cannot be cleared by a time it does not
// have, nor by a scope asked of a process at a time it does not have. A
// window whose end is before its start is judged over both.
func TestATestCaseWithNoWindowIsLeftOutByEverySpan(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(8_000_000, 0)
	if ok, _, _ := m.WasMockOrphanedFor(time.Time{}, time.Time{}, 7); ok {
		t.Fatal("with no span at all, a test case with no window was left out")
	}
	// A scope that says the process served nothing before base (a TGID
	// reused there): asked at the zero time, it would clear the test case.
	m.RecordScopedOrphanWindow(base, base.Add(time.Second), func(_ uint32, req time.Time) bool { return !req.Before(base) }, "scoped")
	if ok, _, reasons := m.WasMockOrphanedFor(time.Time{}, base.Add(time.Hour), 7); !ok || len(reasons) != 1 {
		t.Fatalf("a test case with no start was kept past a scoped span: %v %q", ok, reasons)
	}
	if ok, _, _ := m.WasMockOrphanedFor(base.Add(-time.Hour), time.Time{}, 8); !ok {
		t.Fatal("a test case with no end was kept past a scoped span")
	}
	g := &SyncMockManager{}
	g.RecordOrphanWindow(base, base.Add(time.Second))
	if ok, _, _ := g.WasMockOrphanedFor(time.Time{}, time.Time{}, 8); !ok {
		t.Fatal("a test case with no window was kept past a global span")
	}
	if ok, _ := g.WasMockOrphanedInWindow(time.Time{}, time.Time{}); ok {
		t.Fatal("WasMockOrphanedInWindow, which makes no claim on a zero bound, claimed one")
	}
	// A window backwards (its end before its start, both outside the span):
	// judged over both, so over the span between them.
	if ok, _, _ := g.WasMockOrphanedFor(base.Add(2*time.Second), base.Add(-time.Hour), 8); !ok {
		t.Fatal("a window whose end is before its start, over a span, was kept")
	}
}

// The scope is asked with the test case's request time: what it knew of the
// serving process then.
func TestTheScopeIsAskedAtTheRequestsTime(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(9_000_000, 0)
	var asked []time.Time
	m.RecordScopedOrphanWindow(base, base.Add(time.Hour), func(_ uint32, req time.Time) bool {
		asked = append(asked, req)
		return true
	}, "r")
	req, resp := base.Add(time.Minute), base.Add(2*time.Minute)
	m.WasMockOrphanedFor(req, resp, 7)
	if len(asked) != 1 || !asked[0].Equal(wallTime(req)) {
		t.Fatalf("the scope was asked at %v, want once at the request's time %v", asked, req)
	}
}

// A global span's closer keeps an end past its call (a wall clock stepped back
// since must not cut it short), and clamps one before the span's start to
// that start: its own, where it was joined into a host that opened earlier.
// An open span covers every time from its start.
func TestAGlobalSpansEndIsKeptPastItsCloseAndClampedToItsOwnStart(t *testing.T) {
	var s Spans
	start := time.Now().Add(-time.Hour)
	end := s.OpenUntil(start)
	if ok, _ := s.overlapsFromStart(time.Now().Add(time.Hour), time.Now().Add(time.Hour)); !ok {
		t.Fatal("an open global span does not cover, for a test case's window, a time after now")
	}
	end(time.Now().Add(time.Hour))
	if ok, _ := s.Overlaps(time.Now().Add(time.Minute), time.Now().Add(2*time.Minute)); !ok {
		t.Fatal("a global span closed with an end past its closer's call was cut to the call's wall-clock time")
	}

	var j Spans
	base := time.Now().Add(-time.Hour)
	closers := make([]func(time.Time), 0, maxPressureRanges)
	for i := 0; i < maxPressureRanges; i++ {
		closers = append(closers, j.OpenUntil(base.Add(time.Duration(i)*time.Hour*24)))
	}
	b := j.OpenUntil(base.Add(50 * time.Second)) // past the cap: joins the host opened at base
	for _, c := range closers {
		c(base.Add(40 * time.Second))
	}
	b(base.Add(40 * time.Second)) // back-dated before its own start
	if ok, _ := j.Overlaps(base.Add(50*time.Second), base.Add(50*time.Second)); !ok {
		t.Fatal("a joined span whose closers were all back-dated misses the start of the span joined into it")
	}
}

// A global span joined past the cap ends at the latest end its closers gave,
// whichever closes last: a back-dated close does not shrink another's claim.
func TestAJoinedGlobalSpanEndsAtItsClosersLatestEnd(t *testing.T) {
	t.Parallel()
	var s Spans
	base := time.Now().Add(-time.Hour)
	closers := make([]func(time.Time), 0, maxPressureRanges)
	for i := 0; i < maxPressureRanges; i++ {
		closers = append(closers, s.OpenUntil(base.Add(time.Duration(i)*time.Second)))
	}
	joined := s.OpenUntil(base.Add(time.Millisecond)) // past the cap: joins the span nearest its start, the first
	closers[0](base.Add(30 * time.Minute))            // the host, closed with the later end
	joined(base.Add(time.Second))                     // the joined one last, back-dated earlier
	// Every other span covers a millisecond from its second; the time
	// checked falls between two of them, so only the joined one, at its
	// closers' latest end, can cover it.
	for i, c := range closers[1:] {
		c(base.Add(time.Duration(i+1)*time.Second + time.Millisecond))
	}
	checked := base.Add(20*time.Minute + 500*time.Millisecond)
	if ok, _ := s.Overlaps(checked, checked); !ok {
		t.Fatal("a joined span ended at its last closer's earlier end, shrinking the other closer's claim")
	}
}

// One closed span among open ones, and the span added past the cap open as
// well: no two closed spans to join, so the new one goes global, and the
// closed one keeps its place and its scope.
func TestALoneClosedSpanStaysWhenAnOpenOneGoesPastTheCap(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 4
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(10_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	c1 := m.OpenScopedOrphanWindow(at(20), servedByOneOf(7), "o1")
	m.RecordScopedOrphanWindow(at(0), at(1), servedByOneOf(7), "B")
	c2 := m.OpenScopedOrphanWindow(at(30), servedByOneOf(7), "o2")
	c3 := m.OpenScopedOrphanWindow(at(40), servedByOneOf(7), "o3")
	c4 := m.OpenScopedOrphanWindow(at(60), servedByOneOf(7), "past the cap")
	defer func() { c1(at(50)); c2(at(50)); c3(at(50)); c4(at(70)) }()
	if ok, _, reasons := m.WasMockOrphanedFor(at(0), at(0), 7); !ok || len(reasons) != 1 || reasons[0] != "B" {
		t.Fatalf("span B, closed among open ones, at the cap: %v %q; want it kept, saying B", ok, reasons)
	}
	if ok, _, _ := m.WasMockOrphanedFor(at(0), at(0), 8); ok {
		t.Fatal("span B lost its scope at the cap")
	}
	if ok, _ := m.orphans.Overlaps(at(65), at(65)); !ok {
		t.Fatal("the open span past the cap was not kept as a global span")
	}
}

// A join past the cap with few closed spans joins as few as it must, never
// every closed one: a join gives up the scopes over all the width it covers.
func TestAJoinWithFewClosedSpansJoinsOnlyWhatItMust(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 8
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(11_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	var closers []func(time.Time)
	for i := 0; i < 4; i++ {
		closers = append(closers, m.OpenScopedOrphanWindow(at(5000+i), servedByOneOf(7), "open"))
	}
	defer func() {
		for _, c := range closers {
			c(at(6000))
		}
	}()
	for i, r := range []string{"A", "B", "C", "D"} {
		m.RecordScopedOrphanWindow(at(i*1000), at(i*1000+1), servedByOneOf(7), r)
	}
	m.RecordScopedOrphanWindow(at(4000), at(4001), servedByOneOf(7), "E") // past the cap
	if got := m.ScopedOrphansJoined(); got != 2 {
		t.Fatalf("%d spans joined past the cap, want 2 (one join)", got)
	}
	kept := 0
	for _, s := range []int{500, 1500, 2500, 3500} {
		if ok, _, _ := m.WasMockOrphanedFor(at(s), at(s), 8); !ok {
			kept++
		}
	}
	if kept != 3 {
		t.Fatalf("%d of the four gaps kept another process's test case, want 3 (one join covers one gap)", kept)
	}
}

// A span joined again is counted once, and its reasons stay each once, the
// named ones at most maxJoinedReasons a span.
func TestARejoinedSpanCountsOnceAndKeepsEachReasonOnce(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 4
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(12_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	n := 3 * maxJoinedReasons
	for i := 0; i < n; i++ {
		// Each span's reason twice over: as itself, and repeated later.
		m.RecordScopedOrphanWindow(at(10*i), at(10*i+1), servedByOneOf(7), fmt.Sprintf("r%d", i%(n/2)))
	}
	if got, want := m.ScopedOrphansJoined(), n-maxScopedOrphanSpans+1; got < want || got > n {
		t.Fatalf("%d spans counted as joined, want between %d and the %d added", got, want, n)
	}
	named := map[string]int{}
	more := 0
	for _, h := range m.scopedOrphans.hits(at(0), at(10*n), 7) {
		if len(h.Reasons) > maxJoinedReasons {
			t.Fatalf("a joined span names %d reasons, more than %d: %q", len(h.Reasons), maxJoinedReasons, h.Reasons)
		}
		for _, r := range h.Reasons {
			named[r]++
		}
		more += h.More
	}
	if len(named)+more < n/2 {
		t.Fatalf("the spans name %d reasons and count %d more, want at least the %d distinct ones", len(named), more, n/2)
	}
}

// Past the cap, the join takes the pair whose union is narrowest, measured
// over overlapping spans by their earliest start and latest end.
func TestAJoinTakesTheNarrowestUnion(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 3
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(13_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	m.RecordScopedOrphanWindow(at(0), at(100), servedByOneOf(7), "wide")    // [0, 100]
	m.RecordScopedOrphanWindow(at(10), at(20), servedByOneOf(7), "inside")  // within it
	m.RecordScopedOrphanWindow(at(150), at(160), servedByOneOf(7), "after") // 50 past its end
	m.RecordScopedOrphanWindow(at(300), at(301), servedByOneOf(7), "late")  // past the cap
	// The narrowest union is wide with inside ([0, 100]); measured from
	// inside's own end (20) the gap to after looks smaller (130 < 140), and
	// joining them would cover [10, 160], beyond every span.
	if ok, _, _ := m.WasMockOrphanedFor(at(120), at(120), 8); ok {
		t.Fatal("the join covered the gap between the wide span and the one after it: not the narrowest union")
	}
	if ok, _, reasons := m.WasMockOrphanedFor(at(50), at(50), 8); !ok || !reflect.DeepEqual(reasons, []string{"wide", "inside"}) {
		t.Fatalf("another process's test case in the wide span: %v %q; want left out by the joined [wide; inside]", ok, reasons)
	}

	// Where the gap after a span's own end and the union disagree: B, inside
	// the wide A, ends before A does; joined with C right after it, their
	// union is 7s, while A with B is A's whole 100s, though the gap from A's
	// end to B's start is the smallest. The narrowest union keeps A's scope.
	m2 := &SyncMockManager{}
	m2.RecordScopedOrphanWindow(at(0), at(100), servedByOneOf(7), "A")
	m2.RecordScopedOrphanWindow(at(90), at(95), servedByOneOf(7), "B")
	m2.RecordScopedOrphanWindow(at(96), at(97), servedByOneOf(7), "C")
	m2.RecordScopedOrphanWindow(at(300), at(301), servedByOneOf(7), "D") // past the cap
	if ok, _, _ := m2.WasMockOrphanedFor(at(50), at(50), 8); ok {
		t.Fatal("the join gave up A's scope over its whole width: not the narrowest union")
	}
}

// A test case whose response is stamped before its request is judged as one
// no scope can clear, like one with no window: asked at either stamp, a scope
// that keeps its contract (it answers for the serving process at the
// request's time) could clear a test case it should leave out.
func TestAReversedWindowIsJudgedAsOneNoScopeCanClear(t *testing.T) {
	t.Parallel()
	m := &SyncMockManager{}
	base := time.Unix(14_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	// TGID 7 is of the span's line from +5s on.
	line := func(servedBy uint32, req time.Time) bool { return servedBy == 7 && !req.Before(at(5)) }
	m.RecordScopedOrphanWindow(at(0), at(10), line, "the line of 7")
	if ok, _, _ := m.WasMockOrphanedFor(at(8), at(3), 7); !ok {
		t.Fatal("a test case of TGID 7 inside the span, its response stamped before its request, was kept")
	}
	if ok, _, _ := m.WasMockOrphanedFor(at(3), at(4), 7); ok {
		t.Fatal("a test case of TGID 7 from before it was of the span's line was left out")
	}
}

// An open span covers every time from its start, a test case stamped after
// the wall clock's now included (the test cases' clock can be ahead of a wall
// clock stepped back): through WasMockOrphanedFor, for a global span and for a
// scoped one.
func TestAnOpenSpanLeavesOutATestCaseStampedAfterNow(t *testing.T) {
	t.Parallel()
	later := time.Now().Add(time.Hour)
	g := &SyncMockManager{}
	closeGlobal := g.OpenOrphanWindow(time.Now().Add(-time.Minute))
	defer closeGlobal()
	if ok, _, _ := g.WasMockOrphanedFor(later, later.Add(time.Second), 7); !ok {
		t.Fatal("an open global span did not reach a test case stamped after now")
	}
	m := &SyncMockManager{}
	closeScoped := m.OpenScopedOrphanWindow(time.Now().Add(-time.Minute), servedByOneOf(7), "open")
	defer closeScoped(time.Time{})
	if ok, _, _ := m.WasMockOrphanedFor(later, later.Add(time.Second), 7); !ok {
		t.Fatal("an open scoped span did not reach a test case of its process stamped after now")
	}
	if ok, _, _ := m.WasMockOrphanedFor(later, later.Add(time.Second), 8); ok {
		t.Fatal("an open scoped span left out another process's test case")
	}
}

// A join gives up only the scope a span still has: next to a span already
// joined (which has none), the join that covers the least new scope is the
// joined span with its neighbour, not two scoped spans with a wide gap.
func TestAJoinCountsOnlyTheScopeItGivesUp(t *testing.T) {
	var s ScopedSpans
	base := time.Unix(16_000_000, 0)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	j := &scopedSpan{start: at(0), end: at(1000), joined: 2, reasons: []string{"j"}}
	x := &scopedSpan{start: at(1001), end: at(1002), scope: servedByOneOf(7), reasons: []string{"x"}}
	p := &scopedSpan{start: at(3000), end: at(3001), scope: servedByOneOf(7), reasons: []string{"p"}}
	q := &scopedSpan{start: at(3500), end: at(3501), scope: servedByOneOf(7), reasons: []string{"q"}}
	s.closed = []*scopedSpan{j, x, p, q}
	s.joinLocked(1)
	if len(s.closed) != 3 || s.closed[1] != p || s.closed[2] != q || s.closed[0].scope != nil || !s.closed[0].end.Equal(at(1002)) {
		t.Fatalf("the join made %d spans, want the joined one with x ([0s, 1002s]) and p and q as they were", len(s.closed))
	}
	if hits := s.hits(at(3200), at(3200), 8); len(hits) != 0 {
		t.Fatalf("another process's test case between p and q was left out: %+v", hits)
	}
}

// A joined span's count of the reasons it could not keep is a floor of what
// its members stand for: the larger of theirs, which may be the same reasons,
// never their sum.
func TestAJoinsUncountedReasonsAreAFloor(t *testing.T) {
	var s ScopedSpans
	base := time.Unix(17_000_000, 0)
	a := &scopedSpan{start: base, end: base.Add(time.Second), joined: 3, uncounted: 5}
	b := &scopedSpan{start: base.Add(2 * time.Second), end: base.Add(3 * time.Second), joined: 2, uncounted: 3}
	into := s.joinPair(a, b)
	if n, atLeast := into.more(); n != 5 || !atLeast {
		t.Fatalf("a join of spans that could not keep 5 and 3 reasons stands for %d more (at least: %v), want at least 5", n, atLeast)
	}
}

// An add past the cap joins in place: no sort of every closed span, nor a
// slice per span, under the lock every check takes. A few allocations an
// add, whatever the cap (the span, its reasons, the joined span's).
func TestAnAddPastTheCapJoinsInPlace(t *testing.T) {
	m := &SyncMockManager{}
	base := time.Unix(18_000_000, 0)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }
	scope := servedByOneOf(7)
	for i := 0; i < maxScopedOrphanSpans; i++ {
		m.RecordScopedOrphanWindow(at(2*i), at(2*i+1), scope, "r")
	}
	i := maxScopedOrphanSpans
	allocs := testing.AllocsPerRun(200, func() {
		m.RecordScopedOrphanWindow(at(2*i), at(2*i+1), scope, "r")
		i++
	})
	if allocs > 16 {
		t.Fatalf("an add past the cap of %d spans allocates %.0f times, want a few", maxScopedOrphanSpans, allocs)
	}
	if _, closed, open := m.scopedOrphans.counts(); closed != maxScopedOrphanSpans || open != 0 {
		t.Fatalf("%d spans closed and %d open after the adds, want the cap's %d and none", closed, open, maxScopedOrphanSpans)
	}
}

// Closed spans join in the order they start, whatever the order they were
// recorded or closed in: a span recorded late but starting early is a
// neighbour of the ones next to it in time, and a join of non-neighbours
// would cover neither's time as it should.
func TestClosedSpansJoinInTheOrderTheyStart(t *testing.T) {
	prev := maxScopedOrphanSpans
	maxScopedOrphanSpans = 3
	t.Cleanup(func() { maxScopedOrphanSpans = prev })
	m := &SyncMockManager{}
	base := time.Unix(19_000_000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	m.RecordScopedOrphanWindow(at(100_000), at(101_000), servedByOneOf(7), "C")
	m.RecordScopedOrphanWindow(at(0), at(1_000), servedByOneOf(7), "A")
	m.RecordScopedOrphanWindow(at(2_000), at(3_000), servedByOneOf(7), "B")
	m.RecordScopedOrphanWindow(at(1_000_000), at(1_001_000), servedByOneOf(7), "D") // past the cap
	// The narrowest union is A with B ([0s, 3s]): another process's test case
	// between them is left out, one inside A too, and one between B and C kept.
	for _, ms := range []int{500, 1_500} {
		if ok, _, _ := m.WasMockOrphanedFor(at(ms), at(ms), 8); !ok {
			t.Fatalf("another process's test case at %dms, inside A joined with B, was kept", ms)
		}
	}
	if ok, _, _ := m.WasMockOrphanedFor(at(50_000), at(50_000), 8); ok {
		t.Fatal("another process's test case between B and C was left out: the join was not of neighbours in time")
	}
}
