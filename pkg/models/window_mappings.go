package models

import (
	"container/heap"
	"sort"
	"time"
)

// A test set's mappings derived from its recorded test windows: what a
// recording made without --sync maps each test to under the proven switch
// (design v14 row S3), and what a replay under the switch checks each test's
// consumption against before it creates a mappings.yaml.

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
	// Entry is the mock as a mapping lists it.
	Entry MockEntry
	// At is when its request was recorded; a mock without one is in no window.
	At time.Time
	// TestMock says a test may own it: a data-plane mock (IsProtocolPhase) that
	// no async lane serves. An async lane's mocks are served in their lane's
	// recorded order whichever test is open, so no test owns one.
	TestMock bool
}

// NewWindowedMock is m as the window derivation reads it.
func NewWindowedMock(m *Mock) WindowedMock {
	return WindowedMock{
		Entry: MockEntry{
			Name:             m.Name,
			Kind:             string(m.Kind),
			Timestamp:        m.Spec.ReqTimestampMock.Unix(),
			ReqTimestampMock: FormatMockTimestamp(m.Spec.ReqTimestampMock),
			ResTimestampMock: FormatMockTimestamp(m.Spec.ResTimestampMock),
		},
		At:       m.Spec.ReqTimestampMock,
		TestMock: !m.IsProtocolPhase() && !m.IsAsync(),
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
}

// DeriveWindowMappings gives each test the test mocks recorded inside its
// window: those whose request time is in the window's [Start, End]. A mock
// recorded while no window was open (start-up traffic, the gaps between
// tests) belongs to no test. A mock recorded while two windows were open goes
// to the one that opened first: the earliest-started window that contains it,
// the window models.WindowSchedule releases it at, and the one the sync-mode
// mock manager gives it when it has nothing else to go by.
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
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })

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
		for next < len(ws) && !ws[next].Start.After(m.At) {
			open = append(open, next)
			heap.Push(ending, next)
			next++
		}
		for len(open) > 0 && ws[open[0]].End.Before(m.At) {
			open = open[1:]
		}
		for ending.Len() > 0 && ws[ending.idx[0]].End.Before(m.At) {
			heap.Pop(ending)
		}
		if len(open) == 0 {
			continue
		}
		owner := open[0]
		out.Tests[owner].Mocks = append(out.Tests[owner].Mocks, m.Entry)
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
