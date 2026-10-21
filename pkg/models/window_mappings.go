package models

import (
	"container/heap"
	"sort"
	"strconv"
	"strings"
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
	// Idle says its parser declares its recorded answer idle (IsIdleAnswer):
	// an answer a worker's poll gets while its dependency has nothing, which
	// no test owns wherever it was recorded.
	Idle bool
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
		Idle:     IsIdleAnswer(m),
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
	return m.entryIn("")
}

// entryIn is the mock as a mapping lists it, in concurrency group group ("" for
// none: recorded while one window was open).
func (m WindowedMock) entryIn(group string) MockEntry {
	return MockEntry{
		Group:            group,
		Name:             m.Name,
		Kind:             string(m.Kind),
		Timestamp:        m.At.Unix(),
		ReqTimestampMock: FormatMockTimestamp(m.At),
		ResTimestampMock: FormatMockTimestamp(m.Res),
	}
}

// placedAfter is the mock as the worker scope lists it: placed after the
// window that began last before it, ws[began-1] (began is how many of ws, in
// start order, had begun), or at the set's start when none had.
func (m WindowedMock) placedAfter(ws []TestWindow, began int) MockEntry {
	e := m.entry()
	if began > 0 {
		e.Place = ws[began-1].TestCase
	}
	return e
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
	// Groups are the concurrency groups the entries of Tests name
	// (MockEntry.Group), in order of first use: each the windows that
	// contained a test mock's place when more than one did, in window order,
	// every window's test named (Unkept is the writer's to count). The
	// window that opened first lists the mock; any of them may have made
	// it.
	Groups []ConcurrencyGroup
	// Worker is the set's worker scope (Mapping.Worker): the test mocks
	// recorded while no window was open, and the idle answers wherever they
	// were recorded, which no test owns, in recorded order, each placed where
	// its recording holds it (MockEntry.Place): the test whose window began
	// last before it, or the set's start before every window.
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
// the sync-mode mock manager gives it when it has nothing else to go by. Time
// alone cannot tell which of them made it, so its entry names the windows that
// contained it, its concurrency group (MockEntry.Group, WindowMappings.Groups),
// from the recorded request and answer times alone: keploy record's full round
// gives it to the test that makes its call. A --sync recording's mock manager
// attributes by request time only, so it gives an in-flight call to no test.
//
// A test mock recorded while no window was open that the in-flight rule gives
// to no test is the set's worker scope (WindowMappings.Worker, the founder's
// D2): the application made it on its own, and whichever window a replay
// makes it in, no test owns it. It is placed where its recording holds it.
//
// So is an idle answer (WindowedMock.Idle, the founder's D4), wherever it was
// recorded, the in-flight rule's window included: how many polls find nothing
// is a matter of timing, not of what a test does, so no test is charged with
// one, and the set's end does not require it either. It is placed after the
// window that began last before it, its own when it was recorded inside one.
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
	// groupOf names each concurrency group by its windows (groupKey).
	groupOf := map[string]string{}

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
		if m.Idle {
			// An idle answer: the worker scope's, inside a window or not.
			out.Worker = append(out.Worker, m.placedAfter(ws, next))
			continue
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
			// at.
			out.Worker = append(out.Worker, m.placedAfter(ws, next))
			continue
		}
		owner := open[0]
		group := ""
		if ending.Len() > 1 {
			// ending holds exactly the windows that contain the mock's
			// place: its concurrency group, named by them in window order.
			members := append([]int(nil), ending.idx...)
			sort.Ints(members)
			key := groupKey(members)
			if group = groupOf[key]; group == "" {
				group = "g" + strconv.Itoa(len(out.Groups)+1)
				groupOf[key] = group
				g := ConcurrencyGroup{ID: group, Tests: make([]string, 0, len(members))}
				for _, i := range members {
					g.Tests = append(g.Tests, ws[i].TestCase)
				}
				out.Groups = append(out.Groups, g)
			}
			for _, i := range members {
				concurrent[i] = true
			}
		}
		out.Tests[owner].Mocks = append(out.Tests[owner].Mocks, m.entryIn(group))
	}
	for i, c := range concurrent {
		if c {
			out.Concurrent = append(out.Concurrent, ws[i].TestCase)
		}
	}
	return out
}

// groupKey is a concurrency group's windows, sorted, as one key.
func groupKey(members []int) string {
	var b strings.Builder
	for i, w := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(w))
	}
	return b.String()
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
