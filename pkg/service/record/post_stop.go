package record

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stopCut is the instant a recording's stop came, fixed as its context ends
// (or by the first of its drains to look at the stop, if that look comes
// first): one cut-off for the three drains (test cases, mocks, mappings), so
// a test case kept as captured before the stop never has its mapping judged
// to be of a request after it.
//
// The cut-off is the instant the stop was made when the stop says it
// (utils.StoppedAt: the recording's frame drains run on a context its stop
// cancels so, Start's reqCtx), else the instant the cut is first asked for. A
// request made between the stop and that ask (the goroutine that runs it
// scheduled later on a loaded host) would be dated before a cut fixed then,
// and recorded.
type stopCut struct {
	once sync.Once
	t    time.Time
	// ctx is the recording's context, whose stop's instant the cut is.
	ctx context.Context
}

// at fixes the cut-off at its first call, and returns it. A nil cut is now.
func (c *stopCut) at() time.Time {
	if c == nil {
		return time.Now()
	}
	c.once.Do(func() {
		if t, ok := utils.StoppedAt(c.ctx); ok {
			c.t = t
			return
		}
		c.t = time.Now()
	})
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
