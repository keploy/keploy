package manager

import (
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
// not. A span recorded after a test case went charges it too, at the stream's
// end (late.go).

// maxOrphanWhys bounds how many distinct reasons orphan spans are kept by
// (orphanWhys). Reasons are keploy's own words (a parser's kind and the cause
// it gave), so a recording has few; past the bound a span is still recorded,
// and charged, without its reason.
const maxOrphanWhys = 32

// Whats of a test case's CaptureLossMockDropped (TestCaseLosses).
const (
	// MockDroppedUnderPressure: memory pressure overlapped the test case.
	MockDroppedUnderPressure = "the agent's memory guard paused recording while it ran, and let mocks go"
	// MockDroppedOnItsWay: a mock of the test case's own was dropped on its
	// way to keploy record (WasMockDroppedForTC).
	MockDroppedOnItsWay = "a mock of its own was dropped on its way to keploy record, its queue full or closed"
	// MockDroppedWhileItRan: a mock recorded while the test case ran, which
	// no test case was named the owner of, was dropped on its way to keploy
	// record (spanDroppedMock).
	MockDroppedWhileItRan = "a mock recorded while it ran was dropped on its way to keploy record, its queue full or closed"
)

// orphanWhys keeps the orphan spans again, by why they were recorded
// (RecordOrphanWindowWhy, OpenOrphanWindowWhy), so the test cases recorded
// over one can be told what it was. Only under the proven switch. The zero
// value is ready to use.
type orphanWhys struct {
	mu sync.Mutex
	by map[string]*Spans
}

// spans is why's spans, made on first use; nil for no why, or past
// maxOrphanWhys (Spans' methods are nil-safe).
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
		return nil
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
// sorted.
func (o *orphanWhys) overlapping(start, end time.Time) []string {
	var out []string
	for why, s := range o.all() {
		if ok, _ := s.Overlaps(start, end); ok {
			out = append(out, why)
		}
	}
	sort.Strings(out)
	return out
}

// checkedBefore is Spans.CheckedBefore for every reason's spans.
func (o *orphanWhys) checkedBefore(t time.Time) {
	for _, s := range o.all() {
		s.CheckedBefore(t)
	}
}

// RecordOrphanWindowWhy is RecordOrphanWindow with why: what keploy could not
// record over the span, in keploy's own words (a parser's kind and the cause
// it gave, never a value the application sent). Under the proven switch the
// test cases recorded over the span are charged with it (TestCaseLosses);
// with the switch off it is RecordOrphanWindow.
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

// spanDroppedMock records, under the proven switch, the span of a mock
// dropped on its way to keploy record (the outChan capacity path) that names
// no test case its owner (a recording without --sync names none) and that no
// OnDrop hook takes: [its request, its response]. A test case whose window
// overlaps it is charged with it (TestCaseLosses, the stream's late watch):
// which test case made the call cannot be told, so each that ran meanwhile
// carries it. With the switch off nothing is recorded, and such a test case
// is streamed as before. A mock with no request time is in no window.
func (m *SyncMockManager) spanDroppedMock(mock *models.Mock) {
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
	m.dropped.Record(start, end)
	m.noteLate(models.CaptureLossMockDropped, start, end, MockDroppedWhileItRan)
}

// TestCaseLosses is what test case name, recorded over [start, end], lost
// (models.TestCase.CaptureLost), as far as the spans recorded so far say:
//   - the orphan spans its window overlaps, as exchanges keploy could not
//     record (models.CaptureLossExchangeUnrecorded), What naming why each was
//     recorded, where it was told;
//   - the pressure spans it overlaps, a mock of its own dropped on its way to
//     keploy record, and the spans of mocks dropped so that named no owner
//     (spanDroppedMock), as mocks let go (models.CaptureLossMockDropped).
//
// Empty for a test case none of them touches. The orphan and pressure spans
// and the dropped owners are what routes/record.go leaves a test case out for
// with the proven switch off (WasMockOrphanedInWindow,
// WasPressureActiveInWindow, WasMockDroppedForTC), read the same way: past
// their cap the spans are joined (Spans), which can charge a test case
// checked after the join that sat in a joined gap, as it could leave one out.
// The dropped mocks' spans are kept under the switch only. A span recorded
// after the test case went is the stream's late watch's (late.go). Nil-safe.
func (m *SyncMockManager) TestCaseLosses(name string, start, end time.Time) models.CaptureLosses {
	if m == nil {
		return nil
	}
	var out models.CaptureLosses
	if ok, n := m.orphans.Overlaps(start, end); ok {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossExchangeUnrecorded, Count: uint64(n),
			What: strings.Join(m.orphanWhys.overlapping(start, end), "; ")})
	}
	var dropped uint64
	var why []string
	if ok, n := m.pressure.Overlaps(start, end); ok {
		dropped += uint64(n)
		why = append(why, MockDroppedUnderPressure)
	}
	if m.WasMockDroppedForTC(name) {
		dropped++
		why = append(why, MockDroppedOnItsWay)
	}
	if ok, n := m.dropped.Overlaps(start, end); ok {
		dropped += uint64(n)
		why = append(why, MockDroppedWhileItRan)
	}
	if dropped > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossMockDropped, Count: dropped, What: strings.Join(why, "; ")})
	}
	return out
}
