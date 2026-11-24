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
	ids := m.takeSentClaimedLocked(start, end, time.Time{}, nil, false, "test-x", func() *resolvedWindow { return nil }, nil)
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
	m.ResolveRange(t0, t0.Add(time.Millisecond), "in-order", true, true)
	m.ResolveRange(t0, time.Time{}, "no-response-time", true, true)
	m.ResolveRange(t0.Add(time.Millisecond), t0, "not-kept", false, true)
	m.ResolveRange(t0.Add(time.Millisecond), t0, "inverted", true, true)
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

// A kept test case whose response is dated before its request is resolved
// over the window from its request to when it is resolved
// (models.InvertedWindowEnd): it keeps the mocks its request caused, mapped
// to it, and none requested before its request. Over the window its stamps
// give, which ends before it starts, it was recorded with no mock at all. A
// test case in order is resolved over its stamps, as before.
func TestResolveRange_AnInvertedKeptTestCaseKeepsTheMocksRequestedFromItsRequestOn(t *testing.T) {
	out, maps := make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	m := openWindowManager(out, maps)
	req := time.Now().Add(-time.Second)
	before := httpMockAt(req.Add(-50 * time.Millisecond)) // between its response's stamp and its request
	in1, in2 := httpMockAt(req.Add(10*time.Millisecond)), httpMockAt(req.Add(20*time.Millisecond))
	for _, mk := range []*models.Mock{before, in1, in2} {
		m.AddMock(mk)
	}
	m.ResolveRange(req, req.Add(-100*time.Millisecond), "inverted", true, true)
	got := sentMocks(out)
	if len(got) != 2 || !hasMock(got, in1) || !hasMock(got, in2) {
		t.Fatalf("sent %d mocks, want the two requested after its request", len(got))
	}
	ids := mappedIDs(maps, "inverted")
	if len(ids) != 2 || !ids[in1.Name] || !ids[in2.Name] {
		t.Fatalf("mapped %v to the inverted test case, want its two mocks", ids)
	}
	if n := m.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}

	// In order: its window ends at its response.
	out2, maps2 := make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	n := openWindowManager(out2, maps2)
	a, b := httpMockAt(req.Add(10*time.Millisecond)), httpMockAt(req.Add(20*time.Millisecond))
	n.AddMock(a)
	n.AddMock(b)
	n.ResolveRange(req, req.Add(15*time.Millisecond), "in-order", true, true)
	if got := sentMocks(out2); len(got) != 1 || got[0] != a {
		t.Fatalf("a test case in order took %d mocks, want the one inside its stamps", len(got))
	}
	if ids := mappedIDs(maps2, "in-order"); len(ids) != 1 || !ids[a.Name] {
		t.Fatalf("mapped %v to the test case in order, want its one mock", ids)
	}
	if n.InvertedTestCases() != 0 {
		t.Fatal("a test case in order was counted as inverted")
	}
}

// pendingWatermark settles a test case once its manager holds no mock of its
// window (PendingIn), as proxy mode's does once its parsers are past it.
type pendingWatermark struct{ m *SyncMockManager }

func (w pendingWatermark) Settled(_, _ string, start, end time.Time) bool {
	return !w.m.PendingIn(start, end)
}

// The hold asks about an inverted test case's window as the agent ended it
// (TestCase.WindowEnd): it holds it while a mock requested after its request
// is still to be decided, and lets it go once its resolve has handed that
// mock on. Asked about its stamps, a window that ends before it starts, it
// let the test case go at once, before its mocks had settled.
func TestTestCaseHold_HoldsAnInvertedTestCaseUntilItsMocksSettle(t *testing.T) {
	out, maps := make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	m := openWindowManager(out, maps)
	req := time.Now().Add(-time.Second)
	mock := httpMockAt(req.Add(10 * time.Millisecond))
	m.AddMock(mock)
	arrived := time.Now()
	tc := &models.TestCase{Name: "test-1", Kind: models.HTTP,
		HTTPReq:   models.HTTPReq{Timestamp: req},
		HTTPResp:  models.HTTPResp{Timestamp: req.Add(-100 * time.Millisecond)},
		WindowEnd: models.InvertedWindowEnd(req, req.Add(-100*time.Millisecond), arrived)}
	h := NewTestCaseHold(pendingWatermark{m}, time.Minute, 1<<20)
	h.hold = nil
	h.Add(tc, "", "")
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v while a mock of its window was still to be decided", names(got))
	}
	start, end := models.TestCaseWindow(tc)
	m.ResolveRange(start, end, tc.Name, true, true)
	if got := sentMocks(out); len(got) != 1 || got[0] != mock {
		t.Fatalf("its resolve sent %d mocks, want its one", len(got))
	}
	if got := h.Release(); len(got) != 1 || got[0].Unsettled {
		t.Fatalf("released %+v, want the test case, settled, once its mock was handed on", got)
	}
}

// Of an inverted test case's window, which reaches past its stamps to when it
// was resolved, what another request still in flight claims is that
// request's: the resolve leaves it held for that request, and takes the rest.
func TestResolveRange_AnInvertedTestCaseLeavesARequestInFlightItsMocks(t *testing.T) {
	out, maps := make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	m := openWindowManager(out, maps)
	req := time.Now().Add(-time.Second)
	own := httpMockAt(req.Add(10 * time.Millisecond))
	m.AddMock(own)
	other := m.OpenWindow(req.Add(20*time.Millisecond), nil) // in flight, after the inverted request
	defer other.Close()
	theirs := httpMockAt(req.Add(30 * time.Millisecond))
	m.AddMock(theirs)
	m.ResolveRange(req, req.Add(-100*time.Millisecond), "inverted", true, true)
	got := sentMocks(out)
	if len(got) != 1 || got[0] != own {
		t.Fatalf("the inverted test case took %d mocks, want only its own: the one a request in flight claims is that request's", len(got))
	}
	if ids := mappedIDs(maps, "inverted"); len(ids) != 1 || !ids[own.Name] {
		t.Fatalf("mapped %v to the inverted test case, want its own mock", ids)
	}
	if !m.PendingIn(req.Add(30*time.Millisecond), req.Add(30*time.Millisecond)) {
		t.Fatal("the mock of the request in flight was not kept for it")
	}
}

// The windows that came before an inverted test case's request claim nothing
// of its window, as of a window in order that did not yield: its own, still
// open as a resolve without it runs (a dedup queue's, an embedder's
// capture), and a longer one in flight beside it (a gRPC stream). It takes
// the mocks its request caused.
func TestResolveRange_AnInvertedTestCaseTakesItsMocksFromTheWindowsBeforeIt(t *testing.T) {
	out, maps := make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	m := openWindowManager(out, maps)
	req := time.Now().Add(-time.Second)
	longer := m.OpenWindow(req.Add(-time.Second), nil) // in flight since before it
	defer longer.Close()
	own := m.OpenWindow(req, nil) // its own, still open
	defer own.Close()
	mk := httpMockAt(req.Add(10 * time.Millisecond))
	m.AddMock(mk)
	m.ResolveRange(req, req.Add(-100*time.Millisecond), "inverted", true, true)
	if got := sentMocks(out); len(got) != 1 || got[0] != mk {
		t.Fatalf("the inverted test case took %d mocks, want the one its request caused", len(got))
	}
	if ids := mappedIDs(maps, "inverted"); len(ids) != 1 || !ids[mk.Name] {
		t.Fatalf("mapped %v to the inverted test case, want its mock", ids)
	}
}
