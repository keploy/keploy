package manager

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// mappingsOwed holds the mappings the manager has decided and not yet put on
// the recorder's mapping stream (its channel): from the moment a resolve or a
// flush decides one, under mu, before it hands that mapping's mocks on, until
// sendMapping puts it on the channel, directly or through the overflow queue
// and its drainer, or loses it and says so. A recording's stop waits for one
// owed for a test from before it (BacklogBefore): the mocks go first, and
// once they are on the mock stream nothing else held that test, so an agent
// that ran late between the two sends answered "clear" with the mapping still
// to come, and the stop's mapping drain ended on quiet without it.
type mappingsOwed struct {
	mu   sync.Mutex
	next uint64
	// at is, for each mapping owed, the request time of its test's earliest
	// mock (what a stop asks about: mappingOwedBefore), and the mapping
	// stream it was decided under (mappingGen): a new stream forgives the
	// debts of the ones before (forgiveMappingsBefore).
	at map[uint64]owedAt
}

type owedAt struct {
	at  time.Time
	gen uint64
}

// owedMapping is a mapping on its way to the stream, with what says it got
// there (or was lost): paid. paid is nil for a mapping nothing owes.
type owedMapping struct {
	entry models.TestMockMapping
	paid  func()
}

// oweMapping notes a mapping owed for a test whose earliest mock was
// requested at at, and returns the func that pays it: once, when the mapping
// is on the stream or lost.
func (m *SyncMockManager) oweMapping(at time.Time) (paid func()) {
	o := &m.mappingsOwed
	// Lock order: mu, then mappingOverflowMu, then o.mu.
	m.mappingOverflowMu.Lock()
	o.mu.Lock()
	if o.at == nil {
		o.at = make(map[uint64]owedAt)
	}
	o.next++
	id := o.next
	o.at[id] = owedAt{at: at, gen: m.mappingGen}
	o.mu.Unlock()
	m.mappingOverflowMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			delete(o.at, id)
			o.mu.Unlock()
		})
	}
}

// mappingOwedBefore reports whether a mapping is owed for a test whose
// earliest mock was requested at or before `before`.
func (m *SyncMockManager) mappingOwedBefore(before time.Time) bool {
	o := &m.mappingsOwed
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range o.at {
		if !d.at.After(before) {
			return true
		}
	}
	return false
}

// forgiveMappingsBefore drops the debts decided under a mapping stream before
// gen: their recording has ended, and no stop waits for them any more, however
// they were left (a drainer blocked on a stream that is gone, a mapping read
// for a stream since replaced). Caller holds mappingOverflowMu.
func (m *SyncMockManager) forgiveMappingsBefore(gen uint64) {
	o := &m.mappingsOwed
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, d := range o.at {
		if d.gen < gen {
			delete(o.at, id)
		}
	}
}

// oweLateMappingsLocked owes each of a batch's mappings to past windows
// (late), timed by the earliest of its mocks in the batch, and returns their
// pay funcs by test name: none when no mapping stream is wired. Caller holds
// mu, before the batch's mocks are handed on.
func (m *SyncMockManager) oweLateMappingsLocked(wired bool, late map[string][]string, batch []ownedMock) map[string]func() {
	if !wired || len(late) == 0 {
		return nil
	}
	requested := make(map[string]time.Time, len(batch))
	for _, om := range batch {
		if om.mock != nil {
			requested[om.mock.Name] = om.mock.Spec.ReqTimestampMock
		}
	}
	paid := make(map[string]func(), len(late))
	for tn, ids := range late {
		if len(ids) == 0 {
			continue
		}
		paid[tn] = m.oweMapping(earliestOf(ids, requested))
	}
	return paid
}

// earliestOf is the earliest request time among the mocks named ids; zero,
// which every stop counts, when none is known.
func earliestOf(ids []string, requested map[string]time.Time) time.Time {
	var earliest time.Time
	for _, id := range ids {
		if at, ok := requested[id]; ok && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	return earliest
}
