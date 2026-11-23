package proxy

import (
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// maxReclaimPathDepth bounds the BFS augmenting-path search over overlapping
// window reclaim edges.
const maxReclaimPathDepth = 8

// windowReclaimEntry is a consumed mock that answered a call of the test
// whose window is [winStart, winEnd] (holder), with that window's unconsumed
// recordings of the same call and answer (alts): the holder's call is credited
// with mock until a reclaim moves its credit to one of alts.
type windowReclaimEntry struct {
	mock     *models.Mock
	winStart time.Time
	winEnd   time.Time
	holder   string
	alts     []*models.Mock
}

type windowReclaimState struct {
	mu        sync.Mutex
	lastStart time.Time
	byName    map[string]*windowReclaimEntry
	consumed  map[string]struct{}
}

func (m *MockManager) clearWindowReclaim() {
	m.reclaim.mu.Lock()
	m.reclaim.byName = nil
	m.reclaim.consumed = nil
	m.reclaim.lastStart = time.Time{}
	m.reclaim.mu.Unlock()
}

func (m *MockManager) isReclaimConsumed(name string) bool {
	if name == "" {
		return false
	}
	m.reclaim.mu.Lock()
	_, ok := m.reclaim.consumed[name]
	m.reclaim.mu.Unlock()
	return ok
}

func (m *MockManager) pruneWindowReclaim(start time.Time) {
	if start.IsZero() {
		return
	}
	m.reclaim.mu.Lock()
	defer m.reclaim.mu.Unlock()
	if !m.reclaim.lastStart.IsZero() && start.Before(m.reclaim.lastStart) {
		// Window start moved backward (e.g. retry pass): drop stale reclaim edges.
		m.reclaim.byName = nil
		m.reclaim.consumed = nil
		m.reclaim.lastStart = start
		return
	}
	m.reclaim.lastStart = start
	if len(m.reclaim.byName) == 0 {
		return
	}
	for name, e := range m.reclaim.byName {
		if e.winEnd.Add(models.MockWindowEdgeSkew).Before(start) {
			delete(m.reclaim.byName, name)
		}
	}
}

func (m *MockManager) isNameConsumed(name string) bool {
	if name == "" {
		return true
	}
	m.consumedMu.Lock()
	_, ok := m.consumedPersistent[name]
	m.consumedMu.Unlock()
	return ok
}

// RecordWindowAlternatives records unconsumed equivalent alternatives inside
// [winStart, winEnd] when consumed answered a call in that window, so a later
// overlapping test window containing consumed can reclaim it by consuming one
// of those still-unconsumed alternatives instead.
func (m *MockManager) RecordWindowAlternatives(consumed *models.Mock, winStart, winEnd time.Time, alts []*models.Mock) {
	if consumed == nil || consumed.Name == "" || len(alts) == 0 || winStart.IsZero() || winEnd.IsZero() {
		return
	}
	if m.exactView() != nil {
		return
	}
	filtered := make([]*models.Mock, 0, len(alts))
	seen := make(map[string]struct{}, len(alts)+1)
	seen[consumed.Name] = struct{}{}
	for _, a := range alts {
		if a == nil || a.Name == "" {
			continue
		}
		if _, dup := seen[a.Name]; dup {
			continue
		}
		seen[a.Name] = struct{}{}
		filtered = append(filtered, a)
	}
	if len(filtered) == 0 {
		return
	}
	// A reclaim moves this test's credit to one of alts, which the replay
	// applies to the test it names (MockState.CreditTo): a window no one
	// recorded test opens records none.
	holder := m.testOfWindow(winStart, winEnd)
	if holder == "" {
		return
	}
	m.reclaim.mu.Lock()
	defer m.reclaim.mu.Unlock()
	if m.reclaim.byName == nil {
		m.reclaim.byName = make(map[string]*windowReclaimEntry)
	}
	m.reclaim.byName[consumed.Name] = &windowReclaimEntry{
		mock:     consumed,
		winStart: winStart,
		winEnd:   winEnd,
		holder:   holder,
		alts:     filtered,
	}
}

// testOfWindow is the recorded test whose window is [start, end]
// (SeedRecordedWindows), "" when none or several are, or none was seeded.
func (m *MockManager) testOfWindow(start, end time.Time) string {
	s := m.testWindows.Load()
	name, found := "", 0
	for i := s.CurrentIndex(start); i >= 0 && s.Window(i).Start.Equal(start); i-- {
		if w := s.Window(i); w.End.Equal(end) {
			name, found = w.TestCase, found+1
		}
	}
	if found != 1 {
		return ""
	}
	return name
}

// ReclaimableInWindow returns previously consumed mocks placed (models.PlaceOf)
// inside [winStart, winEnd], or given to it by the edge rule, that have
// recorded reclaim alternatives, ordered by place then sort order.
func (m *MockManager) ReclaimableInWindow(winStart, winEnd time.Time) []*models.Mock {
	if winStart.IsZero() || winEnd.IsZero() || m.exactView() != nil {
		return nil
	}
	m.reclaim.mu.Lock()
	if len(m.reclaim.byName) == 0 {
		m.reclaim.mu.Unlock()
		return nil
	}
	out := make([]*models.Mock, 0, 4)
	for _, e := range m.reclaim.byName {
		if e == nil || e.mock == nil {
			continue
		}
		// Placed in the window (models.PlaceOf, as the staging places it),
		// or given to it by the edge rule.
		place := models.PlaceOf(e.mock)
		if place.IsZero() {
			continue
		}
		inWin := (!place.Before(winStart) && !place.After(winEnd)) || m.isEdgeOwnedByWindow(e.mock, winStart, winEnd)
		if !inWin {
			continue
		}
		out = append(out, e.mock)
	}
	m.reclaim.mu.Unlock()
	if len(out) > 1 {
		sort.SliceStable(out, func(i, j int) bool {
			ti, tj := models.PlaceOf(out[i]), models.PlaceOf(out[j])
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return out[i].TestModeInfo.SortOrder < out[j].TestModeInfo.SortOrder
		})
	}
	return out
}

// CommitWindowReclaim attempts to reclaim reclaimed for the window
// [winStart, winEnd] the call was matched in by finding an augmenting path
// from reclaimed to a still-unconsumed alternative mock, atomically consuming
// that alternative in the manager's pool and consumption history, and
// updating the reclaim graph edges along the path.
//
// Lock order: reclaim.mu is taken first, and under it consumedMu
// (recordConsumes, isNameConsumed), windowMu (recordConsumes' scopeIsOpen)
// and treesMu (takeReclaimedReplacement), and the leaf locks those
// functions take: revMu (bumpRevisionKind), each TreeDb's own
// (deleteByName), and, under consumedMu, the open test windows' (m.windows,
// noteAnsweredInLocked); none of them is held where reclaim.mu is taken.
func (m *MockManager) CommitWindowReclaim(reclaimed *models.Mock, winStart, winEnd time.Time) (*models.Mock, bool) {
	if reclaimed == nil || reclaimed.Name == "" || m.exactView() != nil {
		return nil, false
	}
	// The open test: the window the call was matched in.
	running := m.testOfWindow(winStart, winEnd)
	m.reclaim.mu.Lock()
	defer m.reclaim.mu.Unlock()

	if _, ok := m.reclaim.byName[reclaimed.Name]; !ok {
		return nil, false
	}

	type bfsNode struct {
		name  string
		depth int
	}
	type parentStep struct {
		from string
		alt  *models.Mock
	}
	visited := map[string]struct{}{reclaimed.Name: {}}
	parent := map[string]parentStep{}
	queue := []bfsNode{{name: reclaimed.Name, depth: 0}}

	var freeMock *models.Mock
	for len(queue) > 0 && freeMock == nil {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxReclaimPathDepth {
			continue
		}
		entry := m.reclaim.byName[cur.name]
		if entry == nil {
			continue
		}
		for _, alt := range entry.alts {
			if alt == nil || alt.Name == "" {
				continue
			}
			if _, seen := visited[alt.Name]; seen {
				continue
			}
			visited[alt.Name] = struct{}{}
			parent[alt.Name] = parentStep{from: cur.name, alt: alt}
			if !m.isNameConsumed(alt.Name) {
				freeMock = alt
				break
			}
			if _, hasNext := m.reclaim.byName[alt.Name]; hasNext {
				queue = append(queue, bfsNode{name: alt.Name, depth: cur.depth + 1})
			}
		}
	}
	if freeMock == nil {
		return nil, false
	}

	// The augmenting path reclaimed = u_0, u_1, ..., u_k = freeMock: step
	// (u_i -> u_i+1) is the window that holds u_i (its entry's holder) taking
	// u_i+1 in its place. path is built from freeMock back, so path[0] is the
	// step to freeMock. Every holder must be known: the replay credits each by
	// name (MockState.CreditTo).
	var path []parentStep
	for cur := freeMock.Name; cur != reclaimed.Name; {
		step := parent[cur]
		path = append(path, step)
		cur = step.from
	}
	// No holder may be the open test, nor unknown: a step the open test held
	// would move a credit inside its own entry, which only one drain can
	// carry (a streaming test's two drains, or a late call after its drain
	// was read, would leave the recording listed for two tests). The matcher
	// can offer such a candidate (the open test's earlier call took a
	// recording another window owns, placed in its window, and recorded its
	// spares as their holder); refused, the call is left to the matcher's
	// next candidate (commitReclaim), or to its other fallbacks. A window
	// two tests share exactly names no test (testOfWindow), so no reclaim is
	// recorded or committed for it.
	if running == "" {
		return nil, false
	}
	holders := make([]string, len(path))
	for i, step := range path {
		e := m.reclaim.byName[step.from]
		if e == nil || e.holder == "" || e.holder == running {
			return nil, false
		}
		holders[i] = e.holder
	}
	if !m.takeReclaimedReplacement(freeMock) {
		return nil, false
	}
	// The credits. Each call is credited with one recording, and each
	// recording with one call: the open test with reclaimed, recorded in its
	// window, which an earlier test's identical call took; each holder on the
	// path with the next recording, the last of them with freeMock, which is
	// the one recording newly used (one serve for the call it takes over; the
	// others served once already). The credits are recorded from
	// the free end of the path back, the order the drain lists them in and
	// the replay applies them (carryOverAttribution.recredit): where one
	// holder holds two steps (u_i and u_i+1), the step off u_i+1 must move
	// its entry first, before the step behind it moves the entry onto
	// u_i+1. reclaimed is never the open window's by the edge rule: it was
	// staged in its holder's window, which is not the open test's.
	// All of them in one section of the drain's lock (recordConsumes): a
	// drain between two would split a credit composed with an earlier one
	// from the state that overwrites that one, and the reclaimed recording's
	// own consume from its credits.
	recs := make([]consumeRecord, 0, len(path)+1)
	for i, step := range path {
		st := mockStateDeleted(step.alt)
		st.CreditTo, st.CreditReplaces = holders[i], step.from
		recs = append(recs, consumeRecord{state: st, staged: step.alt.TestModeInfo.StagedSet, served: step.alt.Name == freeMock.Name})
	}
	recs = append(recs, consumeRecord{state: mockStateDeleted(reclaimed), staged: reclaimed.TestModeInfo.StagedSet})
	_ = m.recordConsumes(recs)
	if m.reclaim.consumed == nil {
		m.reclaim.consumed = make(map[string]struct{})
	}
	m.reclaim.consumed[freeMock.Name] = struct{}{}
	m.ownership.reclaims.Add(1)
	// Each step credits an earlier test the next copy.
	m.ownership.recredits.Add(uint64(len(path)))

	// Move each holder's entry along the path, from path[0] backwards, so each
	// destination slot is written after its old entry has already moved on.
	for _, step := range path {
		oldEntry := m.reclaim.byName[step.from]
		delete(m.reclaim.byName, step.from)
		if oldEntry == nil {
			continue
		}
		nextAlts := make([]*models.Mock, 0, len(oldEntry.alts))
		for _, a := range oldEntry.alts {
			if a != nil && a.Name != step.alt.Name {
				nextAlts = append(nextAlts, a)
			}
		}
		if len(nextAlts) > 0 {
			m.reclaim.byName[step.alt.Name] = &windowReclaimEntry{
				mock:     step.alt,
				winStart: oldEntry.winStart,
				winEnd:   oldEntry.winEnd,
				holder:   oldEntry.holder,
				alts:     nextAlts,
			}
		}
	}
	return freeMock, true
}

// takeReclaimedReplacement takes mock, a reclaim's free recording, out of the
// staged per-test trees (by name: tier-local IDs are stamped anew at every
// staging). It refuses, before taking anything, a mock an earlier set pooled
// (stalePooled, the check recordConsume's errPreviousSet makes), so the free
// recording's credit is not refused. A set fence between that check and the
// credits' record (only at a set boundary, whose staging clears the reclaim
// state) can still refuse some of them.
func (m *MockManager) takeReclaimedReplacement(mock *models.Mock) bool {
	if mock == nil || mock.Name == "" || m.stalePooled(mock) {
		return false
	}
	m.treesMu.RLock()
	globalTree := m.filtered
	m.treesMu.RUnlock()
	if globalTree != nil {
		globalTree.deleteByName(mock.Name)
	}
	flt, _ := m.ensureKindTrees(mock.Kind)
	m.treesMu.Lock()
	if flt != nil {
		flt.deleteByName(mock.Name)
	}
	m.treesMu.Unlock()
	m.bumpRevisionKind(mock.Kind)
	// A call that consumed it meanwhile (it was free when the path was
	// found) keeps it: no credit replaces that consume.
	return !m.isNameConsumed(mock.Name)
}

func mockStateDeleted(mock *models.Mock) models.MockState {
	return models.MockState{
		Name:             mock.Name,
		Kind:             mock.Kind,
		Usage:            models.Deleted,
		IsFiltered:       mock.TestModeInfo.IsFiltered,
		SortOrder:        mock.TestModeInfo.SortOrder,
		Type:             mock.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(mock),
		Lifetime:         mock.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		AnswerPlaced:     models.IsAnswerPlaced(mock),
		ReqBodyNoise:     reqBodyNoiseOf(mock.Spec),
		Variables:        variablesOf(mock.Spec),
		Clocks:           clocksOf(mock.Spec),
		Binds:            bindsOf(mock.Spec),
	}
}

// isEdgeOwnedByWindow reports whether the edge rule gives mock to the window
// [start, end] (models.WindowSchedule.EdgeOwner): a plain per-test timed mock
// (models.EdgeSkewEligible) placed (models.PlaceOf) outside every recorded
// window, whose nearest window edge, within models.MockWindowEdgeSkew, is this
// window's, and which no other rule places. Any other shape keeps its own rule.
func (m *MockManager) isEdgeOwnedByWindow(mock *models.Mock, start, end time.Time) bool {
	return m.testWindows.Load().EdgeOwnedBy(mock, start, end)
}

// edgeLeftInFlight reports whether the edge rule would give mock to the window
// [start, end] but leaves it to the in-flight rule
// (models.WindowSchedule.EdgeDecision's EdgeLeftInFlight): sent in the gap
// after this window, within models.MockWindowEdgeSkew of its end, and
// answered once the next window began, which the next window's staging
// serves as its own, as on main.
func (m *MockManager) edgeLeftInFlight(mock *models.Mock, start, end time.Time) bool {
	s := m.testWindows.Load()
	idx, outcome := s.EdgeDecision(mock)
	if outcome != models.EdgeLeftInFlight {
		return false
	}
	w := s.Window(idx)
	return w.Start.Equal(start) && w.End.Equal(end)
}

// EdgeWindows is the recorded test windows the edge rule reads
// (pkg.ServeWindow.Windows) for the agent's window filter, which then hands
// this manager the mocks it gives a window: the windows it was seeded with
// (SeedRecordedWindows), none under exact serving, where the rule is off.
func (m *MockManager) EdgeWindows() *models.WindowSchedule {
	if m.exactView() != nil {
		return nil
	}
	return m.testWindows.Load()
}

// EdgeOwnerWindow returns the recorded test window the edge rule gives mock to
// (models.WindowSchedule.EdgeOwner; integrations.WindowOwnership).
func (m *MockManager) EdgeOwnerWindow(mock *models.Mock) (owner models.TestWindow, owned bool) {
	s := m.testWindows.Load()
	idx, ok := s.EdgeOwner(mock)
	if !ok {
		return models.TestWindow{}, false
	}
	return s.Window(idx), true
}

// edgeStagedNow reports whether the last staging took the mock named name by
// the edge rule (setMocksWithWindowLocked's edgeNames).
func (m *MockManager) edgeStagedNow(name string) bool {
	p := m.edgeStaged.Load()
	if p == nil || *p == nil {
		return false
	}
	_, ok := (*p)[name]
	return ok
}

// ownershipTally counts, for the test set being replayed, what the window
// ownership rules did: each time a staging took a mock by the edge rule
// (edgeStaged: a window staged again counts again) or left one to the
// in-flight rule (edgeLeftInFlight), the reclaims and their re-credits, and
// what the matcher reports (integrations.OwnershipTally). logSetOwnership
// says them at the set's end; a set boundary starts them again
// (sayOwnership).
type ownershipTally struct {
	edgeStaged       atomic.Uint64
	edgeLeftInFlight atomic.Uint64
	edgeAnswered     atomic.Uint64
	deferred         atomic.Uint64
	passedOver       atomic.Uint64
	driftRanked      atomic.Uint64
	reclaims         atomic.Uint64
	recredits        atomic.Uint64
	mu               sync.Mutex
	saidGen          uint64
}

func (t *ownershipTally) resetCounts() {
	t.edgeStaged.Store(0)
	t.edgeLeftInFlight.Store(0)
	t.edgeAnswered.Store(0)
	t.deferred.Store(0)
	t.passedOver.Store(0)
	t.driftRanked.Store(0)
	t.reclaims.Store(0)
	t.recredits.Store(0)
}

// TallyOwnership implements integrations.OwnershipTally.
func (m *MockManager) TallyOwnership(e integrations.OwnershipEvent) {
	switch e {
	case integrations.OwnershipEdgeAnswered:
		m.ownership.edgeAnswered.Add(1)
	case integrations.OwnershipDeferred:
		m.ownership.deferred.Add(1)
	case integrations.OwnershipPassedOver:
		m.ownership.passedOver.Add(1)
	case integrations.OwnershipDriftRanked:
		m.ownership.driftRanked.Add(1)
	}
}

// logSetOwnership says, once for the test set being replayed (its staging
// generation), what the window ownership rules did in it, and how many of its
// per-test recordings answered calls, with how many serves (setServed: each
// call credited to one recording is one serve). keploy test's read of the
// set's end calls it (Proxy.GetConsumedMocks, Proxy.GetMockErrors).
func (m *MockManager) logSetOwnership() {
	m.sayOwnership(false)
}

// sayOwnership says the open set's ownership summary unless it was said for
// this set already, or, onlyIfCounted, it counted nothing, and starts the
// counts again. A set boundary (dropPreviousSetConsumes) calls it
// onlyIfCounted, before the generation moves: a set whose end read never came
// is said then, and no set's counts reach the next.
func (m *MockManager) sayOwnership(onlyIfCounted bool) {
	gen := m.setGen.Load()
	t := &m.ownership
	m.consumedMu.Lock()
	served := m.setServed
	m.setServed = nil
	m.consumedMu.Unlock()
	counts := []uint64{
		t.edgeStaged.Swap(0), t.edgeLeftInFlight.Swap(0), t.edgeAnswered.Swap(0), t.deferred.Swap(0),
		t.passedOver.Swap(0), t.driftRanked.Swap(0), t.reclaims.Swap(0), t.recredits.Swap(0),
	}
	t.mu.Lock()
	said := t.saidGen == gen
	t.saidGen = gen
	t.mu.Unlock()
	if said || m.logger == nil {
		return
	}
	var serves, servedAgain int
	for _, n := range served {
		serves += int(n)
		if n > 1 {
			servedAgain++
		}
	}
	if onlyIfCounted && len(served) == 0 && !slices.ContainsFunc(counts, func(c uint64) bool { return c != 0 }) {
		return
	}
	m.logger.Info("window ownership summary for the test set: how often each rule that tells overlapping tests' recordings apart decided a call, and how many per-test recordings answered",
		zap.Uint64("edge_rule_staged", counts[0]),
		zap.Uint64("edge_rule_left_to_in_flight_rule", counts[1]),
		zap.Uint64("edge_rule_answered", counts[2]),
		zap.Uint64("first_match_deferred_for_ownership", counts[3]),
		zap.Uint64("answered_by_another_match", counts[4]),
		zap.Uint64("drifted_query_ranked_over_score", counts[5]),
		zap.Uint64("reclaims", counts[6]),
		zap.Uint64("recredited_to_earlier_test", counts[7]),
		zap.Int("per_test_recordings_served", len(served)),
		zap.Int("per_test_serves", serves),
		zap.Int("per_test_recordings_served_twice_or_more", servedAgain))
}

var _ integrations.OwnershipTally = (*MockManager)(nil)
