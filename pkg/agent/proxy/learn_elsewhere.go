package proxy

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Record's full round learning a recording's clock reads from a call of its
// shape made in another window than the recording's, for learning only
// (integrations.LearnElsewhere). A timer (a reconciler,
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

// LearnElsewhereCandidates implements integrations.LearnElsewhere (pairable,
// while learningElsewhere): those the trees hold, then those kept on disk.
// Under keys it reads each tree's index and the disk store's, decoding a
// recording only once its name passed: every miss of the learn step asks, a
// full round parks thousands of other tests' recordings on disk, and the
// parser compares a call only with recordings its keys file.
func (m *MockManager) LearnElsewhereCandidates(kind models.Kind, keys []string) []*models.Mock {
	v, epoch := m.learningElsewhere()
	if v == nil {
		return nil
	}
	if len(keys) == 0 || !models.HasRequestKeys(kind) {
		return m.pairableOfKind(v, epoch, kind)
	}
	return m.pairableUnderKeys(v, epoch, kind, keys)
}

// LearnElsewhere implements integrations.LearnElsewhere. One pairing per
// recording per round, so a timer that ticked more often than the recordings
// it left teaches each of them once (the parser reads a refused recording as
// spent and pairs again). Which call teaches first decides only which
// recording learns, never what a learned clock read accepts: each is checked
// at its own recording's offset.
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
//
// Besides what pairable offers, an idle answer of the background (a
// worker's "nothing yet", models.IsIdleAnswer) no call of the set consumed
// is kept: a poll's call teaches the idle recordings of its request their
// clock reads wherever it lands, as the parser carries them, and without
// that the recording cannot answer a later poll whose clock read moved. One
// a call consumed was paired in its own window, and is taught nothing
// across windows, as any recording.
func (m *MockManager) learnElsewhereLocked(mk *models.Mock, clocks map[string]models.ClockOffset, read *callRead) bool {
	v, epoch := m.learningElsewhere()
	if v == nil {
		return false
	}
	if idle := v.isBackgroundRecording(mk) && models.IsIdleAnswer(mk) && !m.consumedInSet(mk.Name); !idle && !m.pairable(v, epoch, mk) {
		return false
	}
	window := models.TeachingWindow{Test: v.test, At: time.Now()}
	if read != nil && (!read.known || read.at.Epoch != epoch || read.at.Bounds != m.exact.round.bounds) {
		window.Test = ""
	}
	st := models.MockState{Name: mk.Name, Kind: mk.Kind, Clocks: maps.Clone(clocks), TaughtIn: []models.TeachingWindow{window}}
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	if _, taught := m.learnedElsewhere[mk.Name]; taught {
		return false
	}
	if m.learnedElsewhere == nil {
		m.learnedElsewhere = map[string]models.MockState{}
	}
	m.learnedElsewhere[mk.Name] = st
	return true
}

// LearnedElsewhere is the clock reads calls made in other windows than their
// recordings' taught them in the current test set (LearnElsewhere), one
// state per recording, by name, each with the window that taught it
// (MockState.TaughtIn): the test open when the call came, "" between
// tests, and when it taught. Not drained: each read is the set's whole.
//
// A read says the set's count of learning-only pairings by operation
// (LearnElsewhereCounted) in the agent's log, once, and again only after
// more calls were counted: keploy mock record reads the round's learning
// as the round ends, and prints the agent's log with the round's.
func (m *MockManager) LearnedElsewhere() []models.MockState {
	m.consumedMu.Lock()
	out := slices.Collect(maps.Values(m.learnedElsewhere))
	unsaid := m.unsaidElsewhereCounts()
	m.consumedMu.Unlock()
	m.sayElsewhereCounts(unsaid)
	for i := range out {
		out[i].Clocks = maps.Clone(out[i].Clocks)
		out[i].TaughtIn = slices.Clone(out[i].TaughtIn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// elsewhereOp is what learning-only pairings are counted by.
type elsewhereOp struct {
	kind      models.Kind
	operation string
}

// LearnElsewhereCounted implements integrations.LearnElsewhereCounter, into
// the current test set's count: a read of the round's learning says it
// (LearnedElsewhere), and so does the next set's staging if no read did.
func (m *MockManager) LearnElsewhereCounted(kind models.Kind, operation, outcome string) {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	op := elsewhereOp{kind: kind, operation: operation}
	if m.elsewhereCounts[op] == nil {
		if m.elsewhereCounts == nil {
			m.elsewhereCounts = map[elsewhereOp]map[string]int{}
		}
		m.elsewhereCounts[op] = map[string]int{}
	}
	m.elsewhereCounts[op][outcome]++
	m.elsewhereCounted++
}

const elsewhereCountLine = "record's full round: calls made in another window than a recording of their operation, and what came of each"

// unsaidElsewhereCounts is the set's whole count, the fields of one line per
// operation, sorted, when calls were counted since the log last said it,
// and marks it said; nil otherwise. Caller holds consumedMu.
func (m *MockManager) unsaidElsewhereCounts() [][]zap.Field {
	if m.elsewhereCounted == m.elsewhereSaid {
		return nil
	}
	m.elsewhereSaid = m.elsewhereCounted
	ops := slices.SortedFunc(maps.Keys(m.elsewhereCounts), func(a, b elsewhereOp) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.operation, b.operation))
	})
	out := make([][]zap.Field, 0, len(ops))
	for _, op := range ops {
		calls, said := 0, []string{}
		for _, o := range slices.Sorted(maps.Keys(m.elsewhereCounts[op])) {
			n := m.elsewhereCounts[op][o]
			calls += n
			said = append(said, o+": "+strconv.Itoa(n))
		}
		out = append(out, []zap.Field{zap.String("kind", string(op.kind)), zap.String("operation", op.operation),
			zap.Int("calls", calls), zap.Strings("outcomes", said)})
	}
	return out
}

// sayElsewhereCounts logs lines outside consumedMu.
func (m *MockManager) sayElsewhereCounts(lines [][]zap.Field) {
	if m.logger == nil {
		return
	}
	for _, fields := range lines {
		m.logger.Info(elsewhereCountLine, fields...)
	}
}

var (
	_ integrations.LearnElsewhere        = (*MockManager)(nil)
	_ integrations.LearnElsewhereCounter = (*MockManager)(nil)
)
