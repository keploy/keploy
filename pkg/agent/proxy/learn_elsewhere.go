package proxy

import (
	"maps"
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// Record's full round learning a recording's changing fields from a call of
// its shape made in another window than the recording's, for learning only
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

// LearnElsewhere keeps noise and clocks, what a call made in another window
// taught mk, for record's full round to write onto mk (LearnedElsewhere),
// merged with what earlier calls taught it as flagMockAsUsed merges a
// consumed recording's states. It consumes, claims and answers nothing.
// False when the session does not learn, mk is no candidate, or there is
// nothing to keep.
func (m *MockManager) LearnElsewhere(mk *models.Mock, noise map[string][]string, clocks map[string]models.ClockOffset) bool {
	return m.learnElsewhereAt(mk, noise, clocks, nil)
}

// learnElsewhereAt is LearnElsewhere holding the round lock and, with g,
// only while the store is still at g's round: a call of an earlier staging
// teaches nothing to a recording of this one's that shares its name.
func (m *MockManager) learnElsewhereAt(mk *models.Mock, noise map[string][]string, clocks map[string]models.ClockOffset, g *roundGuard) bool {
	if mk == nil || mk.IsProtocolPhase() || mk.IsAsync() || (len(noise) == 0 && len(clocks) == 0) {
		return false
	}
	for {
		m.lockRound()
		ok, wait := m.guardLocked(g, m.exactView())
		if ok {
			kept := m.learnElsewhereLocked(mk, noise, clocks)
			m.exact.round.mu.Unlock()
			return kept
		}
		if wait == nil {
			return false
		}
		m.waitReplaced(wait)
	}
}

// learnElsewhereLocked is LearnElsewhere holding the round lock.
func (m *MockManager) learnElsewhereLocked(mk *models.Mock, noise map[string][]string, clocks map[string]models.ClockOffset) bool {
	v, epoch := m.learningElsewhere()
	if v == nil || !m.pairable(v, epoch, mk) {
		return false
	}
	st := models.MockState{Name: mk.Name, Kind: mk.Kind, ReqBodyNoise: cloneNoise(noise), Clocks: maps.Clone(clocks)}
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	if prev, ok := m.learnedElsewhere[mk.Name]; ok {
		st.Clocks = models.MergeClocks(prev.Clocks, st.Clocks)
		if len(prev.ReqBodyNoise) > 0 {
			st.ReqBodyNoise = mocknoise.MergeLearned(st.ReqBodyNoise, prev.ReqBodyNoise)
		}
	}
	if m.learnedElsewhere == nil {
		m.learnedElsewhere = map[string]models.MockState{}
	}
	m.learnedElsewhere[mk.Name] = st
	return true
}

// LearnedElsewhere is what calls made in other windows than their
// recordings' taught them in the current test set (LearnElsewhere), one
// state per recording, by name. Not drained: each read is the set's whole.
func (m *MockManager) LearnedElsewhere() []models.MockState {
	m.consumedMu.Lock()
	out := slices.Collect(maps.Values(m.learnedElsewhere))
	m.consumedMu.Unlock()
	for i := range out {
		out[i].ReqBodyNoise = cloneNoise(out[i].ReqBodyNoise)
		out[i].Clocks = maps.Clone(out[i].Clocks)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cloneNoise is a deep copy of a request noise map.
func cloneNoise(noise map[string][]string) map[string][]string {
	if noise == nil {
		return nil
	}
	out := make(map[string][]string, len(noise))
	for k, v := range noise {
		out[k] = slices.Clone(v)
	}
	return out
}

var _ integrations.LearnElsewhere = (*MockManager)(nil)
