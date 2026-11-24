package manager

import (
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

// A kept test case whose response is dated before its request is counted and
// named once, with both times; one in order, one whose response has no time,
// and one that is not kept, are not.
func TestResolveRange_CountsAKeptTestCaseWhoseResponseIsDatedBeforeItsRequest(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 1024), make(chan models.TestMockMapping, 1024))
	core, logs := observer.New(zap.WarnLevel)
	m.SetLogger(zap.New(core))
	t0 := time.Now()
	m.resolveRange(t0, t0.Add(time.Millisecond), "in-order", true, true, nil)
	m.resolveRange(t0, time.Time{}, "no-response-time", true, true, nil)
	m.resolveRange(t0.Add(time.Millisecond), t0, "not-kept", false, true, nil)
	m.resolveRange(t0.Add(time.Millisecond), t0, "inverted", true, true, nil)
	if n := m.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	got := logs.FilterMessage(invertedWarning).All()
	if len(got) != 1 {
		t.Fatalf("want one warning, got %v", logs.All())
	}
	f := got[0].ContextMap()
	if f["test"] != "inverted" || f["inverted_so_far"] != uint64(1) || f["by"] != time.Millisecond {
		t.Fatalf("warning fields %v", f)
	}
	if req, ok := f["request"].(time.Time); !ok || !req.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("request field %v, want the later time", f["request"])
	}
	if res, ok := f["response"].(time.Time); !ok || !res.Equal(t0) {
		t.Fatalf("response field %v, want the earlier time", f["response"])
	}
	var nilManager *SyncMockManager
	if nilManager.InvertedTestCases() != 0 {
		t.Fatal("a nil manager counts inverted test cases")
	}
}
