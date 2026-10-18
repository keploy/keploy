package models

import (
	"container/heap"
	"sort"
	"time"
)

// A test set's mappings derived from its recorded test windows: what a
// recording made without --sync maps each test to under the proven switch
// (design v14 row S3), and what each test owns in a replay under the switch
// of a set with no mappings.yaml (S12), which the replay never writes.

// RecordedWindow is the window a recorded test case opens when it runs: its
// [request, response] span, for an HTTP or a gRPC test case. A test case of any
// other kind opens none: its Start is zero.
func RecordedWindow(tc *TestCase) TestWindow {
	if tc == nil {
		return TestWindow{}
	}
	w := TestWindow{TestCase: tc.Name}
	switch tc.Kind {
	case HTTP:
		w.Start, w.End = tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp
	case GRPC_EXPORT:
		w.Start, w.End = tc.GrpcReq.Timestamp, tc.GrpcResp.Timestamp
	}
	return w
}

// WindowedMock is what the window derivation reads of one recorded mock.
type WindowedMock struct {
	Name string
	Kind Kind
	// At is when its request was recorded, and Res when its response was; a
	// mock without At is in no window.
	At, Res time.Time
	// Place is where it sits in the recording (PlaceOf): At, or Res for a
	// mock placed at its answer. The window that holds it owns it.
	Place time.Time
	// TestMock says a test may own it: a data-plane mock (IsProtocolPhase) that
	// no async lane serves. An async lane's mocks are served in their lane's
	// recorded order whichever test is open, so no test owns one.
	TestMock bool
	// InFlight says the in-flight rule applies to it (InFlightEligible): sent
	// in the gap before a test and answered at or after its start, it is that
	// test's.
	InFlight bool
}

// NewWindowedMock is m as the window derivation reads it.
func NewWindowedMock(m *Mock) WindowedMock {
	return WindowedMock{
		Name:     m.Name,
		Kind:     m.Kind,
		At:       m.Spec.ReqTimestampMock,
		Res:      m.Spec.ResTimestampMock,
		Place:    PlaceOf(m),
		TestMock: !m.IsProtocolPhase() && !m.IsAsync(),
		InFlight: InFlightEligible(m),
	}
}

// placed is where m sits: its Place, or At for one built without it.
func (m WindowedMock) placed() time.Time {
	if m.Place.IsZero() {
		return m.At
	}
	return m.Place
}

// entry is the mock as a mapping lists it.
func (m WindowedMock) entry() MockEntry {
	return MockEntry{
		Name:             m.Name,
		Kind:             string(m.Kind),
		Timestamp:        m.At.Unix(),
		ReqTimestampMock: FormatMockTimestamp(m.At),
		ResTimestampMock: FormatMockTimestamp(m.Res),
	}
}

// WindowMappings is a test set's mappings derived from its test windows.
type WindowMappings struct {
	// Tests holds one entry per test window, in window order: the test mocks
	// recorded inside it, in recorded order, or none.
	Tests []MappedTestCase
	// Concurrent names, in window order, the tests whose window was open
	// together with another test's while a test mock was recorded: which of
	// them owns that mock was decided by time alone.
	Concurrent []string
	// Worker is the set's worker scope (Mapping.Worker): the test mocks
	// recorded while no window was open, which no test owns, in recorded
	// order, each placed where its recording holds it (MockEntry.Place): the
	// test whose window began last before it, or the set's start before
	// every window.
	Worker []MockEntry
}

// DeriveWindowMappings gives each test the test mocks recorded inside its
// window: those whose place (WindowedMock.Place: the request time, or the
// answer's for a mock placed at its answer) is in the window's [Start, End].
// A mock placed while no window was open belongs to no test (start-up traffic,
// a call sent and answered between two tests: the worker scope, below),
// except one the in-flight rule applies to (WindowedMock.InFlight) that was
// sent in the gap before a test and answered at or after its start: it was in
// flight when that test began, and is that test's (WindowSchedule.InFlightInto,
// which gives it to neither of two tests that start together). A mock recorded
// while two windows were
// open goes to the one that opened first: the earliest-started window that
// contains it, the window models.WindowSchedule releases it at, and the one
// the sync-mode mock manager gives it when it has nothing else to go by. A
// --sync recording's mock manager attributes by request time only, so it gives
// an in-flight call to no test.
//
// A test mock recorded while no window was open that the in-flight rule gives
// to no test is the set's worker scope (WindowMappings.Worker, the founder's
// D2): the application made it on its own, and whichever window a replay
// makes it in, no test owns it. It is placed where its recording holds it.
//
// A window with no Start is left out; one whose End is missing or before its
// Start ends at its Start. Mocks that are not test mocks are skipped.
func DeriveWindowMappings(windows []TestWindow, mocks []WindowedMock) WindowMappings {
	ws := make([]TestWindow, 0, len(windows))
	for _, w := range windows {
		if w.Start.IsZero() {
			continue
		}
		if w.End.IsZero() || w.End.Before(w.Start) {
			w.End = w.Start
		}
		ws = append(ws, w)
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Start.Before(ws[j].Start) })

	ms := make([]WindowedMock, 0, len(mocks))
	for _, m := range mocks {
		if m.TestMock && !m.At.IsZero() {
			ms = append(ms, m)
		}
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].placed().Before(ms[j].placed()) })

	sched := NewWindowSchedule(ws)
	out := WindowMappings{Tests: make([]MappedTestCase, len(ws))}
	for i, w := range ws {
		out.Tests[i] = MappedTestCase{ID: w.TestCase, Mocks: []MockEntry{}}
	}
	concurrent := make([]bool, len(ws))

	// A sweep over the mocks in time order. open holds the windows that have
	// started, in start order: its head, once the windows that ended before
	// the mock are dropped from it, is the earliest-started window containing
	// the mock. ending holds the same windows by end, so that once those that
	// ended before the mock are popped it holds exactly the windows containing
	// it. A window that ended before a mock ended before every later one.
	var open []int
	ending := &windowEnds{ws: ws}
	next := 0
	for _, m := range ms {
		at := m.placed()
		for next < len(ws) && !ws[next].Start.After(at) {
			open = append(open, next)
			heap.Push(ending, next)
			next++
		}
		for len(open) > 0 && ws[open[0]].End.Before(at) {
			open = open[1:]
		}
		for ending.Len() > 0 && ws[ending.idx[0]].End.Before(at) {
			heap.Pop(ending)
		}
		if len(open) == 0 {
			// The gap between two tests. A call sent here and answered at or
			// after the next test's start was in flight when that test began
			// (a consumer's long poll): that test's, as
			// WindowSchedule.InFlightInto has it. Any other is the worker
			// scope's, the app's start included.
			if m.InFlight && next < len(ws) && sched.InFlightInto(m.At, m.Res, ws[next].Start) {
				out.Tests[next].Mocks = append(out.Tests[next].Mocks, m.entry())
				continue
			}
			// No test's: the worker scope's, from the place it was recorded
			// at, after the window that began last before it (none before
			// every window).
			e := m.entry()
			if next > 0 {
				e.Place = ws[next-1].TestCase
			}
			out.Worker = append(out.Worker, e)
			continue
		}
		owner := open[0]
		out.Tests[owner].Mocks = append(out.Tests[owner].Mocks, m.entry())
		if ending.Len() > 1 {
			for _, i := range ending.idx {
				concurrent[i] = true
			}
		}
	}
	for i, c := range concurrent {
		if c {
			out.Concurrent = append(out.Concurrent, ws[i].TestCase)
		}
	}
	return out
}

// windowEnds is a min-heap of window indexes by the window's End.
type windowEnds struct {
	ws  []TestWindow
	idx []int
}

func (h *windowEnds) Len() int           { return len(h.idx) }
func (h *windowEnds) Less(i, j int) bool { return h.ws[h.idx[i]].End.Before(h.ws[h.idx[j]].End) }
func (h *windowEnds) Swap(i, j int)      { h.idx[i], h.idx[j] = h.idx[j], h.idx[i] }
func (h *windowEnds) Push(x any)         { h.idx = append(h.idx, x.(int)) }
func (h *windowEnds) Pop() any {
	n := len(h.idx)
	x := h.idx[n-1]
	h.idx = h.idx[:n-1]
	return x
}
