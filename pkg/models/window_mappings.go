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
// of a set with no mappings.yaml, which the replay never writes.

// RecordedWindow is the window a recorded test case opens when it runs: its
// [request, response] span, for an HTTP or a gRPC test case. A test case of any
// other kind opens none: its Start is zero.
func RecordedWindow(tc *TestCase) TestWindow {
	if tc == nil {
		return TestWindow{}
	}
	w := TestWindow{TestCase: tc.Name, Clock: tc.Origin.clock()}
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
	// TestMock says a test may own it: a data-plane mock that is no answer
	// the client never read (OutsideTestScopes) and that no async lane
	// serves. An async lane's mocks are served in their lane's recorded order
	// whichever test is open, so no test owns one.
	TestMock bool
	// InFlight says the in-flight rule applies to it (InFlightEligible): sent
	// in the gap before a test and answered at or after its start, it is that
	// test's.
	InFlight bool
	// Idle says its parser declares its recorded answer idle (IsIdleAnswer):
	// an answer a worker's poll gets while its dependency has nothing, which
	// no test owns wherever it was recorded.
	Idle bool
	// Clock names the clock At is on: the mock's Origin.Clock, "" for an
	// untrusted stamp. WriteKind is its write kind, by its
	// parser's registration (WriteKindOf): "" for no by-kind write. Both come
	// with the mock's origin, which only a record under the proven switch
	// writes: a mock without one has neither, and its parser is not asked.
	// Only the switch acts on either.
	Clock     string
	WriteKind string
	// Comm and Exe are the process that made the call (Origin.Comm, Exe), for
	// the line that says why it was routed; "" when its origin names none.
	// Unnamed and PID say why its origin names none (Origin.Unnamed, PID),
	// which that line says instead (Process).
	Comm, Exe string
	Unnamed   string
	PID       uint32
}

// Process names the process that made the call as a log line does
// (Origin.ProcessText): "" when its origin names none.
func (m WindowedMock) Process() string {
	o := Origin{Comm: m.Comm, Exe: m.Exe, Unnamed: m.Unnamed, PID: m.PID}
	return o.ProcessText()
}

// FileProcess names it as a committed reason line does
// (Origin.FileProcessText): no kernel pid.
func (m WindowedMock) FileProcess() string {
	o := Origin{Comm: m.Comm, Exe: m.Exe, Unnamed: m.Unnamed, PID: m.PID}
	return o.FileProcessText()
}

// NewWindowedMock is m as the window derivation reads it.
func NewWindowedMock(m *Mock) WindowedMock {
	w := WindowedMock{
		Name:     m.Name,
		Kind:     m.Kind,
		At:       m.Spec.ReqTimestampMock,
		Res:      m.Spec.ResTimestampMock,
		Place:    PlaceOf(m),
		TestMock: !m.OutsideTestScopes() && !m.IsAsync() && !m.IsReadThrough(),
		InFlight: InFlightEligible(m),
		Idle:     IsIdleAnswer(m),
		Clock:    m.Origin.clock(),
	}
	if m.Origin != nil {
		w.WriteKind, _ = WriteKindOf(m)
		w.Comm, w.Exe = m.Origin.Comm, m.Origin.Exe
		w.Unnamed, w.PID = m.Origin.Unnamed, m.Origin.PID
	}
	return w
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
	// Routed is, under DeriveOptions.RouteInFlightWrites, the in-flight
	// writes the derivation routed to the worker scope rather than to the
	// test they were in flight into, in recorded order: each
	// is also in Worker, at the set's start.
	Routed []RoutedWrite
}

// RoutedWrite is one write DeriveWindowMappingsWith routed to the worker
// scope: the mock as the derivation read it, and the test it was in flight
// into when that test began.
type RoutedWrite struct {
	Mock WindowedMock
	Test string
}

// DeriveOptions are the window derivation's options.
type DeriveOptions struct {
	// RouteInFlightWrites is proven routing at record: a call
	// the in-flight rule gives to the test it was in flight into that is a
	// by-kind write (WindowedMock.WriteKind) whose request was stamped on the
	// clock that test's window start was (equal non-empty Clock), is no
	// test's: it was sent before the test began, so the test did not make it.
	// It goes to the worker scope at the set's start (by-kind answers ignore
	// a place), and is listed in Routed. A call on an untrusted or another
	// clock, a call of any other kind (a long poll among them), and one in
	// flight into the first test (which no gap precedes: it is the start's,
	// the worker scope's, already) are derived as without it. keploy record
	// sets it only for a set it marks for the background rules
	// (RecordedWithS12Routing), which only those rules serve.
	RouteInFlightWrites bool
}

// routes reports whether m, in flight into w, is routed to the worker scope
// (DeriveOptions.RouteInFlightWrites): a by-kind write stamped on w's clock.
func (o DeriveOptions) routes(m WindowedMock, w TestWindow) bool {
	return o.RouteInFlightWrites && m.WriteKind != "" && m.Clock != "" && m.Clock == w.Clock
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
	return DeriveWindowMappingsWith(windows, mocks, DeriveOptions{})
}

// BackgroundWindowOptions supplies recorded-schedule facts so
// WindowClassifier.ClassifyForBackground fails closed whenever earlier or
// middle test windows may have been removed.
type BackgroundWindowOptions struct {
	// RecordedWindows, when non-empty, is the COMPLETE immutable schedule of
	// every test window ever recorded for this set (persisted at record time so
	// tests removed later by dedup, --must-pass, or auto-replay remain in it).
	// When provided, any call inside, in-flight into, or edge-owned by any
	// recorded window is never promoted, and a gap between two held windows is
	// promoted only when those two windows are adjacent in RecordedWindows with
	// no removed test between them.
	RecordedWindows []TestWindow
	// RefusalWindows lists test windows known to have been deleted during this
	// run (e.g. --must-pass deletions). They ONLY add refusal zones (blocking
	// promotion of any call inside, in-flight into, or edge-owned by them) and
	// NEVER make an incomplete schedule appear complete or prefix-safe.
	RefusalWindows []TestWindow
	// RecordedFirstStart is the earliest test start the TestDB remembers
	// (RecordedStartReader). Ignored when RecordedStartUnreadable is true.
	RecordedFirstStart time.Time
	// RecordedStartKnown is true when the TestDB authoritatively reported
	// RecordedFirstStart (nil error and non-zero time).
	RecordedStartKnown bool
	// RecordedStartUnreadable is true when RecordedStartReader returned an
	// error, forcing the prefix before the first held window to fail closed.
	RecordedStartUnreadable bool
	// ScheduleIncomplete is true when the TestDB could not supply the full
	// recorded schedule (e.g. RecordedWindowsReader errored), forcing all
	// window-derived classification to fail closed.
	ScheduleIncomplete bool
}

// WindowClassifier classifies individual mocks as worker-scope (outside every
// recorded test window, or idle answers) using the exact same rules as
// DeriveWindowMappings, in O(log W) time and O(W) memory where W is the number
// of valid test windows, without buffering mocks in RAM.
type WindowClassifier struct {
	ws    []TestWindow
	sched *WindowSchedule

	// Precomputed fail-closed background schedule state (populated by
	// NewWindowClassifierForBackground; lazy/defaulted by NewWindowClassifier).
	hasBgOpts       bool
	bgDisabled      bool
	allSched        *WindowSchedule
	allHeld         []bool
	hasFullSchedule bool
	prefixSafe      bool
	effectiveFirst  time.Time
	earlierMissing  bool
}

// NewWindowClassifier prepares a streaming worker-scope classifier for windows.
func NewWindowClassifier(windows []TestWindow) *WindowClassifier {
	sched := NewWindowSchedule(windows)
	return &WindowClassifier{ws: sched.windows, sched: sched, allSched: sched}
}

// HasFullSchedule reports whether this classifier was constructed with an
// authoritative, complete recorded test schedule (opts.RecordedWindows with no
// schedule read error). Name order never proves completeness.
func (c *WindowClassifier) HasFullSchedule() bool {
	return c != nil && !c.bgDisabled && c.hasFullSchedule
}

// NewWindowClassifierForBackground prepares a streaming classifier with
// fail-closed recorded-schedule rules for window-derived background collection.
// Only an explicit RecordedWindows schedule proves gap adjacency; test name
// order never proves adjacency, and RefusalWindows (--must-pass deletions) only
// add refusal zones without turning a partial schedule into a complete one.
func NewWindowClassifierForBackground(held []TestWindow, opts BackgroundWindowOptions) *WindowClassifier {
	sched := NewWindowSchedule(held)
	c := &WindowClassifier{
		ws:        sched.windows,
		sched:     sched,
		allSched:  sched,
		hasBgOpts: true,
	}
	if len(c.ws) == 0 || opts.ScheduleIncomplete {
		c.bgDisabled = true
		return c
	}

	type winKey struct {
		name       string
		start, end int64
	}
	keyOf := func(w TestWindow) winKey {
		end := w.End
		if end.IsZero() || end.Before(w.Start) {
			end = w.Start
		}
		return winKey{name: w.TestCase, start: w.Start.UnixNano(), end: end.UnixNano()}
	}
	heldKeys := make(map[winKey]bool, len(c.ws))
	for _, w := range c.ws {
		heldKeys[keyOf(w)] = true
	}

	heldFirst := c.ws[0].Start
	if len(opts.RecordedWindows) > 0 {
		c.hasFullSchedule = true
		merged := make([]TestWindow, 0, len(opts.RecordedWindows)+len(opts.RefusalWindows)+len(c.ws))
		seen := make(map[winKey]bool, len(opts.RecordedWindows)+len(opts.RefusalWindows)+len(c.ws))
		addWindow := func(w TestWindow) {
			if w.Start.IsZero() {
				return
			}
			k := keyOf(w)
			if !seen[k] {
				seen[k] = true
				merged = append(merged, w)
			}
		}
		for _, w := range opts.RecordedWindows {
			addWindow(w)
		}
		for _, w := range opts.RefusalWindows {
			addWindow(w)
		}
		for _, w := range c.ws {
			addWindow(w)
		}
		c.allSched = NewWindowSchedule(merged)
		c.allHeld = make([]bool, len(c.allSched.windows))
		for i, w := range c.allSched.windows {
			c.allHeld[i] = heldKeys[keyOf(w)]
		}
		if !opts.RecordedStartUnreadable && len(c.allSched.windows) > 0 {
			allFirst := c.allSched.windows[0].Start
			c.effectiveFirst = allFirst
			if opts.RecordedStartKnown && !opts.RecordedFirstStart.IsZero() && opts.RecordedFirstStart.Before(c.effectiveFirst) {
				c.effectiveFirst = opts.RecordedFirstStart
			}
			c.earlierMissing = c.effectiveFirst.Before(heldFirst)
			c.prefixSafe = true
		}
		return c
	}

	// No full RecordedWindows supplied: merge any RefusalWindows strictly as
	// refusal zones so calls inside deleted tests are never promoted, while
	// keeping hasFullSchedule=false (gaps fail closed).
	if len(opts.RefusalWindows) > 0 {
		merged := make([]TestWindow, 0, len(opts.RefusalWindows)+len(c.ws))
		seen := make(map[winKey]bool, len(opts.RefusalWindows)+len(c.ws))
		for _, w := range opts.RefusalWindows {
			if w.Start.IsZero() {
				continue
			}
			k := keyOf(w)
			if !seen[k] {
				seen[k] = true
				merged = append(merged, w)
			}
		}
		for _, w := range c.ws {
			k := keyOf(w)
			if !seen[k] {
				seen[k] = true
				merged = append(merged, w)
			}
		}
		c.allSched = NewWindowSchedule(merged)
	}

	if opts.RecordedStartUnreadable {
		c.prefixSafe = false
		return c
	}
	if opts.RecordedStartKnown && !opts.RecordedFirstStart.IsZero() {
		c.earlierMissing = opts.RecordedFirstStart.Before(heldFirst)
		c.effectiveFirst = heldFirst
		if c.earlierMissing {
			c.effectiveFirst = opts.RecordedFirstStart
		}
		c.prefixSafe = true
	}
	return c
}

// IsWorker reports whether DeriveWindowMappings(windows, ...) would place m in
// WindowMappings.Worker.
func (c *WindowClassifier) IsWorker(m *Mock) bool {
	if c == nil || m == nil {
		return false
	}
	w := NewWindowedMock(m)
	if !w.TestMock || w.At.IsZero() {
		return false
	}
	if w.Idle {
		return true
	}
	at := w.placed()
	next := sort.Search(len(c.ws), func(i int) bool { return c.ws[i].Start.After(at) })
	if next > 0 && !c.sched.maxEnd[next-1].Before(at) {
		return false
	}
	if w.InFlight && next < len(c.ws) && c.sched.InFlightInto(w.At, w.Res, c.ws[next].Start) {
		return false
	}
	return true
}

// WindowBackgroundKind classifies a mock for window-derived background
// collection (both Pass 1 in-RAM derivation and mappings.yaml persistence).
type WindowBackgroundKind uint8

const (
	// WindowBgNone means the mock must NOT be promoted to background: either
	// it fell inside a recorded test window, was in-flight into one, or fell in
	// an unwindowed region where removed test cases may have lived.
	WindowBgNone WindowBackgroundKind = iota
	// WindowBgStartup means the mock was recorded strictly before the earliest
	// recorded test start (taking recordedFirstStart into account).
	WindowBgStartup
	// WindowBgOutsideWindow means the mock was recorded strictly outside every
	// recorded test window (and between held windows when earlier tests are
	// known missing).
	WindowBgOutsideWindow
	// WindowBgIdleInWindow means the mock is a parser-declared idle answer
	// (models.IsIdleAnswer) recorded during a test window (or during an
	// unwindowed gap), so callers must NOT label it "outside every recorded
	// test window".
	WindowBgIdleInWindow
)

// ClassifyForBackground classifies m against c's held (and, when supplied via
// NewWindowClassifierForBackground, complete recorded) test windows and the
// TestDB's earliest recorded test start (recordedFirstStart, zero when unknown
// or unchanged).
//
// Fail-closed rules:
//   - any non-idle call inside a window, in-flight into one, or owned by the
//     100us edge rule (WindowSchedule.EdgeDecision: EdgeOwned / EdgeLeftInFlight)
//     -> WindowBgNone
//   - prefix (< heldFirst): promoted to WindowBgStartup ONLY strictly before
//     effectiveFirst when the recorded start is provably known (from the full
//     recorded schedule or an authoritative RecordedStartReader). When unknown
//     or unreadable -> fail closed (WindowBgNone)
//   - gap between two held windows: promoted to WindowBgOutsideWindow ONLY when
//     the bounding windows are provably adjacent in the full recorded schedule
//     (both held and adjacent in RecordedWindows; name order never proves
//     adjacency). Any missing middle test or unprovable gap -> fail closed
//     (WindowBgNone)
//   - tail (>= heldLastEnd): always WindowBgNone
//   - parser-declared idle answers (IsIdleAnswer) inside a window, removed-test
//     gap, or tail -> WindowBgIdleInWindow (never falsely labeled "outside
//     every recorded test window").
func (c *WindowClassifier) ClassifyForBackground(m *Mock, recordedFirstStart time.Time) (WindowBackgroundKind, string) {
	if c == nil || m == nil || len(c.ws) == 0 || c.bgDisabled {
		return WindowBgNone, ""
	}
	w := NewWindowedMock(m)
	if !w.TestMock || w.At.IsZero() {
		return WindowBgNone, ""
	}
	allSched := c.allSched
	if allSched == nil {
		allSched = c.sched
	}
	allWS := allSched.windows
	at := w.placed()
	allNext := sort.Search(len(allWS), func(i int) bool { return allWS[i].Start.After(at) })
	inWindow := false
	inWindowName := ""
	if allNext > 0 && !allSched.maxEnd[allNext-1].Before(at) {
		inWindow = true
		for i := allNext - 1; i >= 0; i-- {
			if !allWS[i].End.Before(at) {
				inWindowName = allWS[i].TestCase
				break
			}
			if allSched.maxEnd[i].Before(at) {
				break
			}
		}
	}
	if !inWindow && allNext < len(allWS) {
		if (w.InFlight && allSched.InFlightInto(w.At, w.Res, allWS[allNext].Start)) ||
			(!w.Res.IsZero() && !w.Res.Before(allWS[allNext].Start)) {
			inWindow = true
			inWindowName = allWS[allNext].TestCase
		}
	}
	if !inWindow {
		if edgeIdx, edgeOut := allSched.EdgeDecision(m); edgeOut == EdgeOwned || edgeOut == EdgeLeftInFlight {
			inWindow = true
			if edgeIdx >= 0 && edgeIdx < len(allWS) {
				inWindowName = allWS[edgeIdx].TestCase
			}
		}
	}

	heldFirst := c.ws[0].Start
	heldLastEnd := c.sched.maxEnd[len(c.ws)-1]

	var prefixSafe, earlierMissing bool
	var effectiveFirst time.Time
	if c.hasBgOpts {
		prefixSafe = c.prefixSafe
		earlierMissing = c.earlierMissing
		effectiveFirst = c.effectiveFirst
	} else if !recordedFirstStart.IsZero() {
		earlierMissing = recordedFirstStart.Before(heldFirst)
		effectiveFirst = heldFirst
		if earlierMissing {
			effectiveFirst = recordedFirstStart
		}
		prefixSafe = true
	}

	gapSafe := false
	if !inWindow && !at.Before(heldFirst) && at.Before(heldLastEnd) {
		if c.hasFullSchedule {
			if allNext > 0 && allNext < len(allWS) && c.allHeld[allNext-1] && c.allHeld[allNext] {
				gapSafe = true
			}
		}
	}

	if w.Idle {
		if inWindow {
			return WindowBgIdleInWindow, inWindowName
		}
		if prefixSafe && m.Spec.ReqTimestampMock.Before(effectiveFirst) && at.Before(effectiveFirst) {
			return WindowBgStartup, ""
		}
		if earlierMissing && !at.Before(effectiveFirst) && at.Before(heldFirst) {
			return WindowBgIdleInWindow, ""
		}
		if at.Before(heldFirst) || !at.Before(heldLastEnd) || !gapSafe {
			return WindowBgIdleInWindow, ""
		}
		return WindowBgOutsideWindow, ""
	}

	if inWindow {
		return WindowBgNone, ""
	}
	if prefixSafe && m.Spec.ReqTimestampMock.Before(effectiveFirst) && at.Before(effectiveFirst) {
		return WindowBgStartup, ""
	}
	if at.Before(heldFirst) || !at.Before(heldLastEnd) || !gapSafe {
		return WindowBgNone, ""
	}
	return WindowBgOutsideWindow, ""
}

// DeriveWindowMappingsWith is DeriveWindowMappings with opts.
func DeriveWindowMappingsWith(windows []TestWindow, mocks []WindowedMock, opts DeriveOptions) WindowMappings {
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
				if opts.routes(m, ws[next]) {
					// Sent before the test began, on its window's clock:
					// not the test's.
					out.Worker = append(out.Worker, m.entry())
					out.Routed = append(out.Routed, RoutedWrite{Mock: m, Test: ws[next].TestCase})
					continue
				}
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

// MarkAtStop marks, of the worker scope, each mock placed at or after from
// (MockEntry.AtStop): the application's shutdown's calls, which a replay
// never waits for before it stops the application. keploy record passes the
// moment it began stopping the application. A mock's place is placed's
// (PlaceOf, as the window mappings read it: its answer's time for a mock
// placed at its answer), else its request time. A zero from marks none.
func MarkAtStop(worker []MockEntry, placed map[string]time.Time, from time.Time) {
	if from.IsZero() {
		return
	}
	for i := range worker {
		at, ok := placed[worker[i].Name]
		if !ok {
			var err error
			if at, err = ParseMockTimestamp(worker[i].ReqTimestampMock); err != nil {
				continue
			}
		}
		if !at.IsZero() && !at.Before(from) {
			worker[i].AtStop = true
		}
	}
}

// LastWindowEnd is when the last of windows ended, read as
// DeriveWindowMappings reads them (a window with no Start is left out; one
// whose End is missing or before its Start ends at its Start): zero for none.
// A worker-scope mock placed after it was recorded after every test's
// answer, by the application on its own or by its shutdown, which a
// recording with no mappings.yaml does not tell apart.
func LastWindowEnd(windows []TestWindow) time.Time {
	var last time.Time
	for _, w := range windows {
		if w.Start.IsZero() {
			continue
		}
		end := w.End
		if end.IsZero() || end.Before(w.Start) {
			end = w.Start
		}
		if end.After(last) {
			last = end
		}
	}
	return last
}
