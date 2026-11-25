package record

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockLeftOutKey is the context key of withMockLeftOutAtStop.
type mockLeftOutKey struct{}

// withMockLeftOutAtStop returns ctx carrying fn, which the mock drain of the
// frame streams opened with it (GetTestAndMockChans) calls with each mock it
// leaves out as traffic the app served after the stop. Start uses it to settle
// the mapping entries that name such a mock: the request they map is not
// recorded, so a mock missing from them is no loss.
func withMockLeftOutAtStop(ctx context.Context, fn func(*models.Mock)) context.Context {
	return context.WithValue(ctx, mockLeftOutKey{}, fn)
}

// mockLeftOutAtStop is the fn withMockLeftOutAtStop put on ctx; nil if none.
func mockLeftOutAtStop(ctx context.Context) func(*models.Mock) {
	fn, _ := ctx.Value(mockLeftOutKey{}).(func(*models.Mock))
	return fn
}

// leftOutMock marks, in asyncMockIDs, a mock the stop's drain left out as
// traffic the app served after the stop (leftOutMarker): a skip, as
// skippedMock is, that keeps count of the marks alive.
type leftOutMock struct{ live *atomic.Int64 }

// leftOutMarker settles the mapping entries that name a mock the stop's drain
// leaves out as traffic the app served after the stop (withMockLeftOutAtStop).
//
// Such a mock is never saved, by the recording's own rule, and the mapping
// that names it is of a request the recording does not record: its test case
// is left out too. A recorded test case was completed by the stop, and the
// agent maps a mock to a test only from inside that test's request window, so
// none of a recorded test's mocks was requested after the stop. So an entry
// naming one is settled as one naming a mock a hook skipped: never part of a
// test's mock set, and no loss. Before, each waited for the mock stream to end
// and was then counted and logged as a mock the recording lost: on a live
// workload (a k8s-proxy sidecar recording, which records as docker compose),
// one entry per Redis or MySQL call of every request after the stop. A mock
// the drain took before the stop, or never took, is not touched.
type leftOutMarker struct {
	asyncMockIDs *sync.Map
	waits        *mappingWaits
	logger       *zap.Logger
	// off: the recording maps no test to its mocks (--disable-mapping), so
	// no entry would ever consume a mark.
	off bool
	// live counts the marks not yet consumed: at most maxLeftOutAtStopMarks.
	live atomic.Int64
	// capSaid is set once the cap has turned a mock away.
	capSaid atomic.Bool
}

// mark marks m for the mapping entries that name it, and hands back those
// already waiting for it (mappingWaits.arrived), so they settle at once
// rather than holding a place under the waits' cap until the mock stream
// ends. Past the cap m is not marked: an entry naming it is counted as a mock
// the recording lost, as before, and the cap is said once.
func (l *leftOutMarker) mark(m *models.Mock) {
	if l.off || m.Name == "" {
		return
	}
	if l.live.Load() >= int64(maxLeftOutAtStopMarks) {
		if l.capSaid.CompareAndSwap(false, true) {
			l.logger.Warn("too many mocks of traffic after the stop to keep track of: an undated mapping entry naming one past the cap may be counted as a mock the recording lost",
				zap.Int("maxLeftOutAtStopMarks", maxLeftOutAtStopMarks),
				zap.String("next_step", "the app served a lot of traffic while the recording was stopping; if the stop then says mappings named mocks the recording does not hold, part of that count is this traffic, not lost mocks"))
		}
		return
	}
	// Published before arrived: an entry handed back settles on it.
	if _, had := l.asyncMockIDs.LoadOrStore(m.Name, leftOutMock{live: &l.live}); !had {
		l.live.Add(1)
	}
	l.waits.arrived(m.Name)
}

// stopCut is the instant a recording's stop came, fixed as its context ends
// (or by the first of its drains to look at the stop, if that look comes
// first): one cut-off for the three drains (test cases, mocks, mappings), so
// a test case kept as captured before the stop never has its mapping judged
// to be of a request after it.
type stopCut struct {
	once sync.Once
	t    time.Time
}

// at fixes the cut-off at its first call, and returns it. A nil cut is now.
func (c *stopCut) at() time.Time {
	if c == nil {
		return time.Now()
	}
	c.once.Do(func() { c.t = time.Now() })
	return c.t
}

// mappingAfterStop reports whether the agent dated m after cut: a mapping of
// a request made after the stop (models.TestMockMapping.RequestedAt). An
// undated one, from an agent that does not say, is not.
func mappingAfterStop(m models.TestMockMapping, cut time.Time) bool {
	return m.RequestedAt.After(cut)
}

// unmappedEntryLog is the debug line of one mapping entry settled with no mock
// (addSettled): with its tempMockID, why, and next_step.
const unmappedEntryLog = "a mapping entry names a mock the recording does not hold"

// UnmappedSummaryLog is the one line a recording's stop says them in
// (sayUnmapped), with their count by why.
const UnmappedSummaryLog = "the agent's test-mock mappings named mocks the recording does not hold"

// unmappedMocks counts the mapping entries a recording settled with no mock
// (addSettled), by why, so that its stop says them in one line
// (sayUnmapped), not one per entry: a mapping names about a hundred mocks,
// and an ERROR with a stack for each pushed everything else out of the log.
type unmappedMocks struct {
	mu sync.Mutex
	by map[lossReason]int
}

// add counts one entry settled with no mock, for why.
func (u *unmappedMocks) add(why lossReason) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.by == nil {
		u.by = make(map[lossReason]int)
	}
	u.by[why]++
}

// take returns the counts by reason name, and their total, and forgets them:
// the stop that says them is its recording's last word on them, and a
// recorder started again counts its own.
func (u *unmappedMocks) take() (total int, by map[string]int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	by = make(map[string]int, len(u.by))
	for why, n := range u.by {
		by[lossReasonName(why)] += n
		total += n
	}
	u.by = nil
	return total, by
}

// lossReasonName is why, as the stop's summary names it.
func lossReasonName(why lossReason) string {
	switch why {
	case reasonTaken:
		return "another_entry_took_it"
	case reasonUntrackedDrop:
		return "its_save_failed_untracked"
	case reasonCapFull:
		return "too_many_entries_waiting"
	case reasonAbandoned:
		return "teardown_gave_up_the_mock_loop"
	case reasonConsumerGivenUp:
		return "teardown_gave_up_the_mapping_consumer"
	case reasonRevoked:
		return "its_test_revoked"
	default:
		return "mock_stream_ended_without_it"
	}
}

// sayUnmapped says, once for the recording's stop, how many mapping entries
// named a mock the recording does not hold, and why: counts only, no test or
// mock name (each entry is at debug, with why).
func (r *Recorder) sayUnmapped(testSetID string) {
	total, by := r.unmapped.take()
	if total == 0 {
		return
	}
	reasons := make([]string, 0, len(by))
	for name := range by {
		reasons = append(reasons, name)
	}
	sort.Strings(reasons)
	fields := []zap.Field{zap.Int("mappingEntries", total)}
	for _, name := range reasons {
		fields = append(fields, zap.Int(name, by[name]))
	}
	fields = append(fields,
		zap.String("testSetID", testSetID),
		zap.String("next_step", "those tests are saved without these mocks and replay may fail them as if the app had changed: re-record the test set if they matter; keploy's debug log has one line per entry with why"))
	r.logger.Warn(UnmappedSummaryLog, fields...)
}
