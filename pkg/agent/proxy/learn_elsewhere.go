package proxy

import (
	"maps"
	"slices"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// Record's full round learning a recording's clock reads from a call of its
// shape made in another window than the recording's, for learning only
// (integrations.LearnElsewhere; founder, 2026-11-01). A timer (a reconciler,
// a stale scan, a janitor) fires wherever the clock puts it: recorded in one
// test's window, its call lands in another's in the full round, and a
// recording the learn step taught nothing cannot answer the timer's next
// call once judging moved it to the worker scope.
//
// What such a call teaches is kept apart from consumption: no test consumed
// the recording, so the recording stays its owner's, and attributing the
// set's calls to tests and judging them (which read the consumed list) never
// see it. Record's full round reads it (LearnedElsewhere) and writes it onto
// the recording as it writes what a consumed recording learned.

// learningElsewhere is the view to learn in, and the staging's epoch: while
// the session runs record's full round's learn step (SetPairing), not ended
// (SetLearnEnded) nor held (SetLearnHeld), a view installed, its background
// named when the set has one (not settling). The rule is the pairing's
// (pairing), whatever rules serve the set: nil when none is.
func (m *MockManager) learningElsewhere() (*exactView, uint64) {
	if !m.exact.pairs.Load() || m.exact.learnEnded.Load() || m.exact.learnHeld.Load() {
		return nil, 0
	}
	v := m.exactView()
	if v == nil || (v.bg != nil && v.bg.settling) {
		return nil, 0
	}
	return v, m.stagingEpoch.Load()
}

// LearnElsewhereCandidates is, while the session learns (learningElsewhere),
// kind's data-plane recordings neither the view's own nor the background's
// (under today's rules, which name none, the pool's are candidates too) that
// the set's calls have not consumed and no pairing has claimed (pairable), in
// recorded order: those the trees hold, then those the agent keeps on disk.
// nil otherwise. A parser pairs a call with one only by its drift pairing's
// guards, and the round writes what it learned onto a recording only of a
// test it learns onto (or the pool's, in a full record).
func (m *MockManager) LearnElsewhereCandidates(kind models.Kind) []*models.Mock {
	v, epoch := m.learningElsewhere()
	if v == nil {
		return nil
	}
	return m.pairableOfKind(v, epoch, kind)
}

// LearnElsewhere keeps clocks, the clock reads a call made in another window
// taught mk, for record's full round to write onto mk (LearnedElsewhere),
// merged with what earlier calls taught it as flagMockAsUsed merges a
// consumed recording's clock reads. Clock reads alone: what such a call
// shows as noise is never kept (integrations.LearnElsewhere). It consumes,
// claims and answers nothing. False when the session does not learn, mk is
// no candidate, or there is nothing to keep.
func (m *MockManager) LearnElsewhere(mk *models.Mock, clocks map[string]models.ClockOffset) bool {
	return m.learnElsewhereAt(mk, clocks, nil, nil)
}

// callRead is the round a call was read in (integrations.CallRound.Round);
// known is false for a read of a store that keeps no generation.
type callRead struct {
	at    integrations.RoundAt
	known bool
}

// learnElsewhereAt is LearnElsewhere holding the round lock and, with g,
// only while the store is still at g's round: a call of an earlier staging
// teaches nothing to a recording of this one's that shares its name. read
// is the round the call was read in, nil for a call taught through the
// manager itself (the open view's).
func (m *MockManager) learnElsewhereAt(mk *models.Mock, clocks map[string]models.ClockOffset, g *roundGuard, read *callRead) bool {
	if mk == nil || mk.IsProtocolPhase() || mk.IsAsync() || len(clocks) == 0 {
		return false
	}
	for {
		m.lockRound()
		ok, wait := m.guardLocked(g, m.exactView())
		if ok {
			kept := m.learnElsewhereLocked(mk, clocks, read)
			m.exact.round.mu.Unlock()
			return kept
		}
		if wait == nil {
			return false
		}
		m.waitReplaced(wait)
	}
}

// learnElsewhereLocked is LearnElsewhere holding the round lock. The window
// it names (MockState.TaughtIn) is the open view's test, "" for the pool's.
// A call whose round moved since it was read (a view's install, a test's
// begin or end: RoundAt.Bounds), or whose read keeps no round, came in a
// window keploy no longer knows: it names none, as a call between tests
// does, so whatever undoes a window's learning undoes it with any.
func (m *MockManager) learnElsewhereLocked(mk *models.Mock, clocks map[string]models.ClockOffset, read *callRead) bool {
	v, epoch := m.learningElsewhere()
	if v == nil || !m.pairable(v, epoch, mk) {
		return false
	}
	st := models.MockState{Name: mk.Name, Kind: mk.Kind, Clocks: maps.Clone(clocks)}
	window := models.TeachingWindow{Test: v.test, At: time.Now()}
	if read != nil && (!read.known || read.at.Epoch != epoch || read.at.Bounds != m.exact.round.bounds) {
		window.Test = ""
	}
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	if prev, ok := m.learnedElsewhere[mk.Name]; ok {
		st.Clocks = models.MergeClocks(prev.Clocks, st.Clocks)
		st.TaughtIn = slices.Clone(prev.TaughtIn)
	}
	st.TaughtIn = taughtIn(st.TaughtIn, window)
	if m.learnedElsewhere == nil {
		m.learnedElsewhere = map[string]models.MockState{}
	}
	m.learnedElsewhere[mk.Name] = st
	return true
}

// taughtIn is windows with w: its test's entry moved to w's instant, or w
// added. A window is named once, so the list is as long as the windows that
// taught the recording, however many calls did.
func taughtIn(windows []models.TeachingWindow, w models.TeachingWindow) []models.TeachingWindow {
	for i := range windows {
		if windows[i].Test == w.Test {
			if w.At.After(windows[i].At) {
				windows[i].At = w.At
			}
			return windows
		}
	}
	return append(windows, w)
}

// LearnedElsewhere is the clock reads calls made in other windows than their
// recordings' taught them in the current test set (LearnElsewhere), one
// state per recording, by name, each with the windows that taught it
// (MockState.TaughtIn): the test open when such a call came, "" between
// tests, and when it taught. Not drained: each read is the set's whole.
func (m *MockManager) LearnedElsewhere() []models.MockState {
	m.consumedMu.Lock()
	out := slices.Collect(maps.Values(m.learnedElsewhere))
	m.consumedMu.Unlock()
	for i := range out {
		out[i].Clocks = maps.Clone(out[i].Clocks)
		out[i].TaughtIn = slices.Clone(out[i].TaughtIn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var _ integrations.LearnElsewhere = (*MockManager)(nil)
