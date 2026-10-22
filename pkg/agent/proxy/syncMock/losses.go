package manager

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// losses.go is what a record's test case lost, under the proven switch.
//
// A test case whose window overlaps an orphan span (an exchange a connection
// carried that keploy could not record as a mock) or a pressure span (the
// memory guard let mocks go), or one of whose own mocks was dropped on its way
// to keploy record, may lack a mock it needs. With the switch off,
// routes/record.go leaves such a test case out of the recording, and only its
// summary's tcs_suppressed_total says so. Under the switch a record-time loss
// never takes a test case out of the recording unsaid: the test case is
// recorded with the loss as its evidence (TestCaseLosses), keploy record
// charges it to the test case, and the replay that judges the test case keeps
// it if it replays, or quarantines it with the loss in its reason if it does
// not. A span recorded after a test case streamed with nothing lost went
// charges it too, at the stream's end (late.go).

// maxOrphanWhys bounds how many distinct reasons orphan spans are kept by
// (orphanWhys). Reasons are keploy's own words (a parser's kind and the cause
// it gave), so a recording has few; past the bound a span is still recorded,
// and charged, and the test cases over it are told a source past the bound
// was (WhyPastBound).
const maxOrphanWhys = 32

// WhyPastBound is what a test case's loss names for a span whose reason came
// past maxOrphanWhys others (orphanWhys).
var WhyPastBound = fmt.Sprintf("a source past the %d keploy names in one recording", maxOrphanWhys)

// JoinedNote ends what a test case's loss names for a span it overlaps only
// where Spans joined spans over a gap, past their cap (Spans.OverlapsJoined):
// the test case may have run in that gap, beside what the span stands for.
var JoinedNote = fmt.Sprintf("(maybe: past %d such stretches keploy joins the nearest, so this test case may have run beside it rather than over it)", maxPressureRanges)

// Whats of a test case's CaptureLossMockDropped (TestCaseLosses).
const (
	// MockDroppedUnderPressure: memory pressure overlapped the test case.
	MockDroppedUnderPressure = "the agent's memory guard paused recording while it ran, and let mocks go"
	// MockDroppedOnItsWay: a mock of the test case's own was dropped on its
	// way to keploy record (WasMockDroppedForTC).
	MockDroppedOnItsWay = "a mock of its own was dropped on its way to keploy record, its queue full or closed"
	// MockDroppedWhileItRan: a mock recorded while the test case ran, which
	// no test case was named the owner of, was dropped on its way to keploy
	// record (SpanDroppedMock).
	MockDroppedWhileItRan = "a mock recorded while it ran was dropped on its way to keploy record, its queue full or closed"
)

// orphanWhys keeps spans again, by why they were recorded (the orphan spans,
// RecordOrphanWindowWhy and OpenOrphanWindowWhy; the dropped mocks' spans,
// SpanDroppedMock), so the test cases recorded over one can be told what it
// was. Only under the proven switch. The zero value is ready to use.
type orphanWhys struct {
	mu sync.Mutex
	by map[string]*Spans
	// past is the spans whose why came past maxOrphanWhys others: told as
	// WhyPastBound.
	past Spans
}

// spans is why's spans, made on first use: past maxOrphanWhys, the spans of
// every why past it (past); nil for no why (Spans' methods are nil-safe).
func (o *orphanWhys) spans(why string) *Spans {
	if why == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.by[why]; ok {
		return s
	}
	if len(o.by) >= maxOrphanWhys {
		return &o.past
	}
	if o.by == nil {
		o.by = make(map[string]*Spans)
	}
	s := &Spans{}
	o.by[why] = s
	return s
}

// all is a copy of the spans by why, to read without o.mu.
func (o *orphanWhys) all() map[string]*Spans {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[string]*Spans, len(o.by))
	for why, s := range o.by {
		out[why] = s
	}
	return out
}

// overlapping is why each span that overlaps [start, end] was recorded,
// sorted, a why whose spans overlap it only where they were joined over a gap
// ending JoinedNote, and WhyPastBound last for a span whose why came past the
// bound.
func (o *orphanWhys) overlapping(start, end time.Time) []string {
	var out []string
	for why, s := range o.all() {
		if ok, _, joined := s.OverlapsJoined(start, end); ok {
			out = append(out, withJoinedNote(why, joined))
		}
	}
	sort.Strings(out)
	if ok, _, joined := o.past.OverlapsJoined(start, end); ok {
		out = append(out, withJoinedNote(WhyPastBound, joined))
	}
	return out
}

// withJoinedNote is what, ending JoinedNote when joined: a span overlapped only
// where spans were joined over a gap (Spans.OverlapsJoined).
func withJoinedNote(what string, joined bool) string {
	if !joined {
		return what
	}
	if what == "" {
		return JoinedNote
	}
	return what + " " + JoinedNote
}

// checkedBefore is Spans.CheckedBefore for every reason's spans.
func (o *orphanWhys) checkedBefore(t time.Time) {
	for _, s := range o.all() {
		s.CheckedBefore(t)
	}
	o.past.CheckedBefore(t)
}

// RecordOrphanWindowWhy is RecordOrphanWindow with why: what keploy could not
// record over the span, in keploy's own words (a parser's kind and the cause
// it gave, never a value the application sent). Under the proven switch the
// test cases recorded over the span are charged with it (TestCaseLosses);
// with the switch off it is RecordOrphanWindow. Every orphan span with an end
// is recorded here (RecordOrphanWindow is this with no why), and every open
// one in OpenOrphanWindowWhy: a rule for orphan spans belongs in these two.
func (m *SyncMockManager) RecordOrphanWindowWhy(start, end time.Time, why string) {
	if m == nil {
		return
	}
	m.orphans.Record(start, end)
	if provenswitch.On() {
		m.orphanWhys.spans(why).Record(start, end)
	}
	m.noteLate(models.CaptureLossExchangeUnrecorded, start, end, why)
}

// OpenOrphanWindowWhy is OpenOrphanWindow with why (RecordOrphanWindowWhy).
// The closer it returns closes both, and is idempotent.
func (m *SyncMockManager) OpenOrphanWindowWhy(start time.Time, why string) func() {
	if m == nil {
		return func() {}
	}
	closeSpan := m.orphans.Open(start)
	if !provenswitch.On() {
		return closeSpan
	}
	closeWhy := m.orphanWhys.spans(why).Open(start)
	m.noteLate(models.CaptureLossExchangeUnrecorded, start, time.Time{}, why)
	return func() {
		closeSpan()
		closeWhy()
	}
}

// SpanDroppedMock records, under the proven switch, the span of a mock
// dropped on its way to keploy record ([its request, its response]): one the
// outChan capacity path dropped that names no test case its owner (a
// recording without --sync names none) and that no OnDrop hook takes, or one
// an OnDrop hook hands back (enterprise's, outside a DaemonSet). A test case
// whose window overlaps it is charged with it (TestCaseLosses, the stream's
// late watch), as mocks let go (models.CaptureLossMockDropped), What naming
// the mock's kind (droppedWhy): which test case made the call cannot be told,
// so each that ran meanwhile carries it. With the switch off nothing is
// recorded. A mock with no request time is in no window.
func (m *SyncMockManager) SpanDroppedMock(mock *models.Mock) {
	if m == nil || mock == nil || !provenswitch.On() {
		return
	}
	start, end := mock.Spec.ReqTimestampMock, mock.Spec.ResTimestampMock
	if start.IsZero() {
		return
	}
	if end.IsZero() {
		end = start
	}
	why := droppedWhy(mock)
	m.dropped.Record(start, end)
	m.droppedWhys.spans(why).Record(start, end)
	m.noteLate(models.CaptureLossMockDropped, start, end, why)
}

// WasDroppedMockInWindow reports whether the span of a dropped mock that named
// no owner (SpanDroppedMock) overlaps [start, end], and how many: a record
// stream whose client does not charge what a test case lost (an older keploy,
// another client of the agent's) leaves such a test case out, as it leaves
// one over an orphan span out, rather than stream it without the mock and say
// nothing. The spans are kept under the proven switch only, so with the
// switch off it is always false, as before. Nil-safe.
func (m *SyncMockManager) WasDroppedMockInWindow(start, end time.Time) (bool, int) {
	if m == nil {
		return false, 0
	}
	return m.dropped.Overlaps(start, end)
}

// droppedWhy is what a test case charged with dropped mock m's span is told
// (SpanDroppedMock): MockDroppedWhileItRan, after the mock's kind where it has
// one ("MySQL: ..."), as a span keploy could not record names its parser's.
func droppedWhy(m *models.Mock) string {
	if m == nil || m.Kind == "" {
		return MockDroppedWhileItRan
	}
	return string(m.Kind) + ": " + MockDroppedWhileItRan
}

// TestCaseLosses is what test case name, recorded over [start, end], lost
// (models.TestCase.CaptureLost), as far as the spans recorded so far say:
//   - the orphan spans its window overlaps, as exchanges keploy could not
//     record (models.CaptureLossExchangeUnrecorded), What naming why each was
//     recorded, where it was told;
//   - the pressure spans it overlaps, a mock of its own dropped on its way to
//     keploy record, and the spans of mocks dropped so that named no owner
//     (SpanDroppedMock), as mocks let go (models.CaptureLossMockDropped).
//
// Empty for a test case none of them touches. The orphan and pressure spans
// and the dropped owners are what routes/record.go leaves a test case out for
// with the proven switch off (WasMockOrphanedInWindow,
// WasPressureActiveInWindow, WasMockDroppedForTC), read the same way. Past
// their cap the spans are joined over gaps (Spans), which can charge a test
// case checked after the join that sat in a joined gap, as it could leave one
// out: a charge that only such a join makes says so (JoinedNote), so what the
// test case is told it lost stays true. The dropped mocks' spans are kept
// under the switch only. A span recorded after the test case went is the
// stream's late watch's (late.go). Nil-safe.
func (m *SyncMockManager) TestCaseLosses(name string, start, end time.Time) models.CaptureLosses {
	if m == nil {
		return nil
	}
	var out models.CaptureLosses
	if ok, n, joined := m.orphans.OverlapsJoined(start, end); ok {
		whys := m.orphanWhys.overlapping(start, end)
		if len(whys) == 0 && joined {
			whys = []string{JoinedNote}
		}
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossExchangeUnrecorded, Count: uint64(n),
			What: strings.Join(whys, "; ")})
	}
	var dropped uint64
	var why []string
	if ok, n, joined := m.pressure.OverlapsJoined(start, end); ok {
		dropped += uint64(n)
		why = append(why, withJoinedNote(MockDroppedUnderPressure, joined))
	}
	if m.WasMockDroppedForTC(name) {
		dropped++
		why = append(why, MockDroppedOnItsWay)
	}
	if ok, n, joined := m.dropped.OverlapsJoined(start, end); ok {
		dropped += uint64(n)
		whys := m.droppedWhys.overlapping(start, end)
		if len(whys) == 0 {
			whys = []string{withJoinedNote(MockDroppedWhileItRan, joined)}
		}
		why = append(why, whys...)
	}
	if dropped > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossMockDropped, Count: dropped, What: strings.Join(why, "; ")})
	}
	return out
}
