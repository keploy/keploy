package proxy

import (
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
)

// setFollow is what a parser that follows a set across its tests reads of the
// store (integrations.SetFollower). Its judgement is the parser's: this only
// says when the set can be read, when a cycle began, and which recordings the
// parser answered for elsewhere.
type setFollow struct {
	// readable is the staging epoch whose recordings are all readable.
	readable atomic.Uint64
	mu       sync.Mutex
	// epoch is the staging the fields below are of.
	epoch uint64
	// tests holds the tests that opened in the staging; cycle counts the
	// times one opened again.
	tests map[string]struct{}
	cycle uint64
	// excuses holds the recordings out of their test's outcome.
	excuses map[string]struct{}
}

// in moves f to the staging epoch: what it holds is one staging's. f.mu held.
func (f *setFollow) in(epoch uint64) {
	if f.epoch != epoch {
		f.epoch, f.tests, f.cycle, f.excuses = epoch, nil, 0, nil
	}
}

// opened notes that test opened in the staging epoch.
func (f *setFollow) opened(epoch uint64, test string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in(epoch)
	if _, again := f.tests[test]; again {
		f.cycle++
		f.tests = nil
	}
	if f.tests == nil {
		f.tests = map[string]struct{}{}
	}
	f.tests[test] = struct{}{}
}

// excused reports whether a parser took name out of its test's outcome. A nil
// f, a view no test serves, excuses nothing.
func (f *setFollow) excused(name string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.excuses[name]
	return ok
}

// MarkRecordingsReadable says the staged set's recordings can all be read: the
// agent has wired its on-disk store (the readers of the set's recordings).
func (m *MockManager) MarkRecordingsReadable() {
	m.exact.follow.readable.Store(m.stagingEpoch.Load())
}

// RecordingsReadable implements integrations.SetFollower.
func (m *MockManager) RecordingsReadable() uint64 { return m.exact.follow.readable.Load() }

// TestCycle implements integrations.SetFollower.
func (m *MockManager) TestCycle() uint64 {
	f := &m.exact.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in(m.stagingEpoch.Load())
	return f.cycle
}

// ExcuseRecording implements integrations.SetFollower.
func (m *MockManager) ExcuseRecording(name string) {
	f := &m.exact.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in(m.stagingEpoch.Load())
	if f.excuses == nil {
		f.excuses = map[string]struct{}{}
	}
	f.excuses[name] = struct{}{}
}

var _ integrations.SetFollower = (*MockManager)(nil)
