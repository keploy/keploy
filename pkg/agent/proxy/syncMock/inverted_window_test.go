package manager

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test case whose response is dated before its request asks about a window
// that ends before it starts. It holds nothing (PendingIn's walk: no time is
// both at or after the start and at or before the end), and it must not reach
// the notes and held mocks between its two times as a slice from the start's
// index to the end's: that index pair is inverted, and slicing it panicked
// the agent at a recording's stop under load ("slice bounds out of range
// [17549:17397]", PendingIn, from TestCaseHold.final).
func TestPendingIn_AWindowThatEndsBeforeItStartsHoldsNothingAndDoesNotPanic(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 1024), make(chan models.TestMockMapping, 1024))
	t0 := time.Now()
	m.mu.Lock()
	for i := range 5 {
		// Startup mocks sent for a claim a request has taken: each one counts
		// as pending inside its own time.
		m.sentClaimed = append(m.sentClaimed, sentClaim{name: "s", at: t0.Add(time.Duration(i) * time.Millisecond), takenBy: "test-1"})
		m.held = append(m.held, heldMock{mock: httpMockAt(t0.Add(time.Duration(i) * time.Millisecond)), owner: &resolvedWindow{}})
	}
	m.heldShared = len(m.held)
	m.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PendingIn panicked on an inverted window: %v", r)
		}
	}()
	if m.PendingIn(t0.Add(3*time.Millisecond), t0.Add(time.Millisecond)) {
		t.Fatal("a window that ends before it starts holds a mock")
	}

	// The same notes and mocks are pending inside a window the right way round.
	if !m.PendingIn(t0.Add(time.Millisecond), t0.Add(3*time.Millisecond)) {
		t.Fatal("a window over the notes does not hold them")
	}
}

// The range helpers every caller slices or splices by give an empty range for
// an inverted window: takeSentClaimedLocked's splice copied the notes after
// the end's index over those after the start's, dropping and duplicating
// notes, and PendingIn sliced the held mocks with the inverted pair.
func TestRangeHelpers_AnInvertedWindowIsAnEmptyRange(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 1024), make(chan models.TestMockMapping, 1024))
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range 6 {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		m.sentClaimed = append(m.sentClaimed, sentClaim{name: string(rune('a' + i)), at: at})
		m.held = append(m.held, heldMock{mock: httpMockAt(at)})
	}
	start, end := t0.Add(4*time.Millisecond), t0.Add(time.Millisecond)
	if lo, hi := m.sentClaimRangeLocked(start, end); hi < lo || hi-lo != 0 {
		t.Errorf("sentClaimRangeLocked(inverted) = [%d, %d), want an empty range", lo, hi)
	}
	if lo, hi := m.heldRangeLocked(start, end); hi < lo || hi-lo != 0 {
		t.Errorf("heldRangeLocked(inverted) = [%d, %d), want an empty range", lo, hi)
	}
	before := make([]string, 0, len(m.sentClaimed))
	for _, c := range m.sentClaimed {
		before = append(before, c.name)
	}
	ids := m.takeSentClaimedLocked(start, end, time.Time{}, nil, "test-x", func() *resolvedWindow { return nil }, nil)
	if len(ids) != 0 {
		t.Errorf("an inverted window took notes %v", ids)
	}
	var after []string
	for _, c := range m.sentClaimed {
		after = append(after, c.name)
	}
	if len(after) != len(before) {
		t.Fatalf("notes %v became %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("notes %v became %v", before, after)
		}
	}
}

// A revoke reaching from a time past its to (the to before from) revokes and
// keeps by the same rule as any other: it takes out none of the test cases
// that start after to, and it does not slice the list from from's index to
// to's, which is then the lower one.
func TestRevokeReaching_AToBeforeFromTakesNothingAndDoesNotPanic(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	var l sentList
	for i := range 6 {
		at := t0.Add(time.Duration(i) * time.Second)
		l.add(string(rune('a'+i)), at, at.Add(100*time.Millisecond))
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("revokeReaching panicked on a to before from: %v", r)
		}
	}()
	var got []string
	if n := l.revokeReaching(t0.Add(4*time.Second), t0.Add(2*time.Second), true, func(name string) { got = append(got, name) }); n != 0 || len(got) != 0 {
		t.Fatalf("revoked %d %v, want none", n, got)
	}
	if len(l.tcs) != 6 {
		t.Fatalf("%d test cases left of 6", len(l.tcs))
	}
}

// NoteInvertedTestCase counts a dropped inverted test case and warns once with
// its name, both times, and actionable next_step; resolveRange on an inverted
// kept window refuses mocks and revokes the test name without incrementing the
// inverted counter (avoiding double-counting with stream-level drops).
func TestResolveRange_RefusesMocksWithoutCountingAndRevokesByName(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 1024), make(chan models.TestMockMapping, 1024))
	core, logs := observer.New(zap.WarnLevel)
	m.SetLogger(zap.New(core))
	t0 := time.Now()
	m.resolveRange(t0, t0.Add(time.Millisecond), "in-order", true, true, nil)
	m.resolveRange(t0, time.Time{}, "no-response-time", true, true, nil)
	m.resolveRange(t0.Add(time.Millisecond), t0, "not-kept", false, true, nil)
	m.resolveRange(t0.Add(time.Millisecond), t0, "inverted", true, true, nil)
	if n := m.InvertedTestCases(); n != 0 {
		t.Fatalf("%d inverted test cases counted by resolveRange, want 0 (count only where TC is dropped)", n)
	}
	if !m.WasMockDroppedForTC("inverted") {
		t.Fatal("resolveRange on inverted kept window did not revoke test name")
	}
	if m.WasMockDroppedForTC("in-order") || m.WasMockDroppedForTC("not-kept") {
		t.Fatal("non-inverted or non-kept resolves must not revoke test name")
	}

	// Capture-level NoteInvertedTestCase counts once and logs the sampled WARN.
	m.NoteInvertedTestCase("inverted-capture", t0.Add(time.Millisecond), t0)
	if n := m.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	got := logs.FilterMessage(invertedWarning).All()
	if len(got) != 1 {
		t.Fatalf("want one warning, got %v", logs.All())
	}
	f := got[0].ContextMap()
	if f["test"] != "inverted-capture" || f["inverted_so_far"] != uint64(1) || f["by"] != time.Millisecond {
		t.Fatalf("warning fields %v", f)
	}
	if req, ok := f["request"].(time.Time); !ok || !req.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("request field %v, want the later time", f["request"])
	}
	if res, ok := f["response"].(time.Time); !ok || !res.Equal(t0) {
		t.Fatalf("response field %v, want the earlier time", f["response"])
	}
	if f["next_step"] != models.InvertedLeftOutNextStep {
		t.Fatalf("next_step = %v, want %q", f["next_step"], models.InvertedLeftOutNextStep)
	}
	var nilManager *SyncMockManager
	if nilManager.InvertedTestCases() != 0 {
		t.Fatal("a nil manager counts inverted test cases")
	}
}

// resolveRange (via ResolveKept, ResolveRange, and DedupQueue.ResolveJob) must
// refuse mocks when keep && IsInvertedWindow: close the window, revoke the
// test by name, emit zero mocks and zero mappings for it, record no resolved
// window, leave InvertedTestCases unchanged (no double-count with stream), and
// leave mocks belonging to an earlier open window intact so that window
// receives them when it resolves.
func TestResolveRange_FailsClosedOnInvertedWindowAndProtectsEarlierOpenWindow(t *testing.T) {
	out := make(chan *models.Mock, 32)
	maps := make(chan models.TestMockMapping, 32)
	m := openWindowManager(out, maps)
	t0 := time.Now().Add(-time.Second)

	// Earlier long-running request B opens before inverted request A.
	winB := m.OpenWindow(t0, nil)
	m1 := httpMockAt(t0.Add(10 * time.Millisecond))
	m1.Name = "mock-for-B-early"
	m.AddMock(m1)

	reqA := t0.Add(30 * time.Millisecond)
	winA := m.OpenWindow(reqA, nil)
	m2 := httpMockAt(t0.Add(50 * time.Millisecond))
	m2.Name = "mock-for-B-late"
	m.AddMock(m2)

	// Inverted request A resolves via ResolveKept (the DaemonSet / sync path).
	m.ResolveKept(winA, reqA, reqA.Add(-20*time.Millisecond), "test-A-inverted", true)

	if n := m.InvertedTestCases(); n != 0 {
		t.Fatalf("InvertedTestCases = %d after inverted ResolveKept, want 0 (resolveRange does not drop TC)", n)
	}
	if !m.WasMockDroppedForTC("test-A-inverted") {
		t.Fatal("inverted ResolveKept did not revoke test-A-inverted by name")
	}
	if n := m.OpenWindows(); n != 1 {
		t.Fatalf("OpenWindows = %d after inverted ResolveKept, want 1 (winA closed, winB open)", n)
	}
	select {
	case got := <-out:
		t.Fatalf("inverted ResolveKept emitted mock %q instead of refusing mocks", got.Name)
	default:
	}
	select {
	case got := <-maps:
		t.Fatalf("inverted ResolveKept emitted mapping %+v instead of refusing mocks", got)
	default:
	}

	// DedupQueue.ResolveJob on an inverted window also refuses mocks without counting.
	dq := NewDedupQueue()
	job := dq.Enqueue(t0.Add(60 * time.Millisecond))
	dq.ResolveJob(job, false, t0.Add(40*time.Millisecond), "test-dedup-inverted", true, m)
	if n := m.InvertedTestCases(); n != 0 {
		t.Fatalf("InvertedTestCases = %d after inverted ResolveJob, want 0", n)
	}
	if !m.WasMockDroppedForTC("test-dedup-inverted") {
		t.Fatal("inverted ResolveJob did not revoke test-dedup-inverted by name")
	}
	select {
	case got := <-out:
		t.Fatalf("inverted ResolveJob emitted mock %q instead of refusing mocks", got.Name)
	default:
	}
	m.mu.Lock()
	for _, rw := range m.recentWindows {
		if rw.testName == "test-A-inverted" || rw.testName == "test-dedup-inverted" {
			m.mu.Unlock()
			t.Fatalf("inverted resolve recorded resolvedWindow %+v instead of skipping", rw)
		}
	}
	m.mu.Unlock()

	// Now B resolves cleanly and receives BOTH of its mocks intact.
	m.ResolveKept(winB, t0, t0.Add(100*time.Millisecond), "test-B-valid", true)
	var bMocks []string
	for len(out) > 0 {
		bMocks = append(bMocks, (<-out).Name)
	}
	if len(bMocks) != 2 {
		t.Fatalf("valid window B received mocks %v, want both mock-for-B-early and mock-for-B-late", bMocks)
	}
	select {
	case got := <-maps:
		if got.TestName != "test-B-valid" || len(got.MockIDs) != 2 {
			t.Fatalf("mapping for B = %+v, want test-B-valid with 2 mocks", got)
		}
	default:
		t.Fatal("valid window B received no mapping")
	}
}

// Even when every test case is inverted, resolveRange / SweepStaleMocks still
// sweeps stale out-of-window per-test mocks and flushes reusable session mocks
// so m.buffer cannot grow unbounded, while protecting recent mocks of open
// windows and recording no resolvedWindow or mapping for the inverted test.
func TestResolveRange_InvertedStillSweepsStaleMocksAndFlushesSessionMocks(t *testing.T) {
	out := make(chan *models.Mock, 32)
	maps := make(chan models.TestMockMapping, 32)
	m := openWindowManager(out, maps)
	now := time.Now()

	// Stale orphan per-test mock (older than StaleHorizon, no open window covers it).
	stale := httpMockAt(now.Add(-(StaleHorizon + 3*time.Second)))
	stale.Name = "stale-orphan"
	m.AddMock(stale)

	// Session-scoped reusable mock sitting in buffer (placed while unbound,
	// then outChan bound) should be flushed by the empty-window sweep.
	m.mu.Lock()
	sessMock := &models.Mock{
		Kind:         models.HTTP,
		Name:         "session-reusable",
		Spec:         models.MockSpec{ReqTimestampMock: now.Add(-time.Second)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true},
	}
	m.buffer = append(m.buffer, sessMock)
	m.mu.Unlock()

	// Earlier open window B with a recent per-test mock must remain protected.
	tB := now.Add(-time.Second)
	winB := m.OpenWindow(tB, nil)
	mB := httpMockAt(tB.Add(10 * time.Millisecond))
	mB.Name = "recent-for-B"
	m.AddMock(mB)

	reqA := now.Add(-500 * time.Millisecond)
	winA := m.OpenWindow(reqA, nil)
	m.ResolveKept(winA, reqA, reqA.Add(-100*time.Millisecond), "test-inverted-sweep", true)

	// Stale mock was swept; only mB stays buffered for winB.
	_, _, _, bufLen := m.GetDropStats()
	if bufLen != 1 {
		t.Fatalf("buffer size = %d after inverted ResolveKept sweep, want 1 (stale swept, recent-for-B kept)", bufLen)
	}
	// Session-scoped mock was flushed to outChan.
	select {
	case got := <-out:
		if got.Name != "session-reusable" {
			t.Fatalf("unexpected mock flushed on inverted sweep: %q, want session-reusable", got.Name)
		}
	default:
		t.Fatal("session-scoped reusable mock was not flushed on inverted sweep")
	}
	select {
	case extra := <-out:
		t.Fatalf("unexpected extra mock flushed on inverted sweep: %q", extra.Name)
	default:
	}
	select {
	case mp := <-maps:
		t.Fatalf("inverted sweep emitted mapping %+v, want none", mp)
	default:
	}

	// winB still gets its recent mock intact.
	m.ResolveKept(winB, tB, now, "test-B-after-sweep", true)
	select {
	case got := <-out:
		if got == nil {
			t.Fatal("nil mock for winB")
		}
	default:
		t.Fatal("winB did not receive its protected mock after inverted sweep")
	}
}

func TestNoteInvertedTestCaseWithMeta_LogsDiagnosticsAndNextStep(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 8), make(chan models.TestMockMapping, 8))
	core, logs := observer.New(zap.WarnLevel)
	m.SetLogger(zap.New(core))
	t0 := time.Now().Add(-time.Second)

	m.NoteInvertedTestCaseWithMeta("GET /orders/:id", t0.Add(10*time.Millisecond), t0, InvertedMeta{
		Protocol: "http",
		Ingress:  "hooks.Capture",
		Clock:    "proxy-clock",
	})

	got := logs.FilterMessage(invertedWarning).All()
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1", len(got))
	}
	f := got[0].ContextMap()
	if f["test"] != "GET /orders/:id" || f["protocol"] != "http" || f["ingress"] != "hooks.Capture" || f["clock"] != "proxy-clock" {
		t.Fatalf("unexpected diagnostic fields: %v", f)
	}
	nextStep, _ := f["next_step"].(string)
	if nextStep != models.InvertedLeftOutNextStep {
		t.Fatalf("next_step = %q, want %q", nextStep, models.InvertedLeftOutNextStep)
	}
	lower := strings.ToLower(nextStep)
	if strings.Contains(lower, "ntp") || strings.Contains(lower, "chrony") {
		t.Fatalf("next_step must not speculate about NTP/chrony: %q", nextStep)
	}
}

// OpenInvertedTally counts only drops noted during its own open interval
// (excluding drops before OpenInvertedTally and after Close); overlapping
// tallies each observe drops noted while both are open, matching LossTally.
func TestOpenInvertedTally_CountsOnlyDropsWhileOpen(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 8), make(chan models.TestMockMapping, 8))
	t0 := time.Now()

	// Drop before any tally opens: neither tally sees it.
	m.NoteInvertedTestCase("pre", t0.Add(time.Millisecond), t0)

	s1 := m.OpenInvertedTally()
	m.NoteInvertedTestCase("s1-only", t0.Add(time.Millisecond), t0)

	s2 := m.OpenInvertedTally()
	m.NoteInvertedTestCase("both-1", t0.Add(time.Millisecond), t0)
	m.NoteInvertedTestCase("both-2", t0.Add(time.Millisecond), t0)

	s1.Close()
	m.NoteInvertedTestCase("s2-only", t0.Add(time.Millisecond), t0)
	s2.Close()

	// Drop after both closed: neither tally changes.
	m.NoteInvertedTestCase("post", t0.Add(time.Millisecond), t0)

	if got := s1.Read(); got != 3 {
		t.Fatalf("s1.Read() = %d, want 3", got)
	}
	if got := s2.Read(); got != 3 {
		t.Fatalf("s2.Read() = %d, want 3", got)
	}
	// Closing twice is idempotent and preserves Read().
	s1.Close()
	if got := s1.Read(); got != 3 {
		t.Fatalf("s1.Read() after double Close = %d, want 3", got)
	}
	var nilTally *InvertedTally
	nilTally.Close()
	if nilTally.Read() != 0 {
		t.Fatal("nil tally Read != 0")
	}
}
