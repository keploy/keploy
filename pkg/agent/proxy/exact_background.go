package proxy

import (
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// THE BACKGROUND RULES IN EXACT SERVING (replay.RegisterS12Serving).
//
// A set staged under the background rules (ServeExactBackground, which the
// agent calls only for a set a proven-routing record marked, models.RecordedWithS12Routing,
// replayed by a build whose judging enables them) has a background: its worker scope and its pool,
// the recordings no test owns. Exact serving then differs from today's in
// four ways, and only for such a set:
//
//   - In a test, the view shows the pool's data-plane recordings too, after
//     the open test's own and the worker scope (exactView.shows, keep). One
//     of them, unless an async lane's, is spent for the staging through
//     whichever door a parser consumes it (exactConsumeIn), in a test or
//     between tests.
//   - The worker scope the view shows is ordered by its places: the earlier
//     place first, and within one place a delivering answer before an idle
//     one, in recorded order (ServeExactWorker's order, which the agent
//     settles).
//   - A parser reads the background apart (integrations.ExactBackground):
//     what is unused, and what its staging consumed (for an answer by kind).
//   - Record's full round pairs a call nothing answered with a recording
//     another test owns (integrations.Pairing), while its session pairs
//     (SetPairing: OutgoingOptions.Learn). A claim is no consumption, and
//     each recording is claimed once in a staging.
//
// Any other set is served exactly as before: none of this applies.

// backgroundState is a staged set's background under the background rules: its pool
// and its whole worker scope, by name. A settling one (StageExactBackground)
// is a set staged under them whose background is not named yet: it names no
// recording, and pairs none.
type backgroundState struct {
	pool        map[string]struct{}
	worker      map[string]struct{}
	workerOrder []string
	settling    bool
	mu          sync.Mutex
	spent       map[string]bool
	// ownedInTest reports whether a recording of the background in force in
	// a staging epoch is one a test's identical call is the test's own call
	// in (MockManager.testOwnsStartupAt); nil: none is.
	ownedInTest func(epoch uint64, name string) bool
}

// isSpent reports whether name was consumed in unset-mode background serving.
func (b *backgroundState) isSpent(name string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent[name]
}

// trySpend atomically consumes name once in unset-mode background serving.
func (b *backgroundState) trySpend(name string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent[name] {
		return false
	}
	if b.spent == nil {
		b.spent = map[string]bool{}
	}
	b.spent[name] = true
	return true
}

// forgetSpends forgets every unset-mode spend (trySpend): a new instance of
// the application starts the set's start again (RearmAppStart), and what the
// earlier one consumed as it started is not spent for the new one.
func (b *backgroundState) forgetSpends() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.spent)
}

// inWorker reports whether name is one of the background's worker entries.
func (b *backgroundState) inWorker(name string) bool {
	if b == nil {
		return false
	}
	_, ok := b.worker[name]
	return ok
}

// has reports whether name is one of the background's recordings.
func (b *backgroundState) has(name string) bool {
	if b == nil {
		return false
	}
	if _, ok := b.pool[name]; ok {
		return true
	}
	_, ok := b.worker[name]
	return ok
}

// inPool reports whether name is one of the background's pool.
func (b *backgroundState) inPool(name string) bool {
	if b == nil {
		return false
	}
	_, ok := b.pool[name]
	return ok
}

// StageExactBackground opens a set's staging under rules, before any view of
// the staging is installed: the previous set's background goes, and when rules
// is models.RecordedWithS12Routing (under the proven switch) the views the
// staging installs before its background is named (ServeExactBackground) read
// the set as served by them (BackgroundCursorOf, RoundSnapshot.Background),
// so a call those views refuse is never told the set was recorded without
// proven routing. With the switch off the settling background is kept too,
// and servedBackground serves it to no view and no read. Such a view names no recording of the background, since
// the trees still hold the previous set's, whose names may be this set's
// (names are unique only within a set), and pairs nothing (pairing). Any
// other rules are today's, as ServeExactBackground's. Whatever the rules, a
// view installed from now until ServeExactBackground settles the set is a
// staging view: a call it refuses is told it was refused while the set's
// mocks were being staged (exactMissStaging). It installs no view itself.
func (m *MockManager) StageExactBackground(rules string) {
	m.exact.staging.Store(true)
	if rules == models.RecordedWithS12Routing {
		m.exact.bg.Store(&backgroundState{settling: true})
		return
	}
	m.exact.bg.Store(nil)
}

// ServeExactBackground sets the rules the staged set's background is served
// by: the background rules when rules is models.RecordedWithS12Routing (with the set's pool
// and its whole worker scope by name), today's for any other rules (pool and
// worker unread). It
// installs no view itself: the next ServeExactTest or ServeExactPool shows it.
// The agent calls it at each set's staging, once the set's mocks are staged
// and before the view that serves them, so a set never inherits the previous
// one's rules.
func (m *MockManager) ServeExactBackground(rules string, pool, worker []string) {
	// The set's mocks are staged: the views from now on are no staging's.
	m.exact.staging.Store(false)
	if rules != models.RecordedWithS12Routing {
		m.exact.bg.Store(nil)
		m.bumpViewAll()
		return
	}
	b := &backgroundState{
		pool:        make(map[string]struct{}, len(pool)),
		worker:      make(map[string]struct{}, len(worker)),
		spent:       make(map[string]bool),
		ownedInTest: m.testOwnsStartupAt,
	}
	for _, n := range pool {
		if n != "" {
			b.pool[n] = struct{}{}
		}
	}
	for _, n := range worker {
		if n != "" {
			if _, dup := b.worker[n]; !dup {
				b.worker[n] = struct{}{}
				b.workerOrder = append(b.workerOrder, n)
			}
		}
	}
	m.exact.bg.Store(b)
	m.bumpViewAll()
	if m.logger != nil {
		m.logger.Debug("exact serving: the set's background is served by proven routing",
			zap.Int("pool", len(b.pool)), zap.Int("worker", len(b.worker)))
	}
}

// SetPairing says whether the session pairs (integrations.Pairing): record's
// full round's, OutgoingOptions.Learn under the proven switch. Proxy.Mock
// sets it for each session.
func (m *MockManager) SetPairing(on bool) {
	m.exact.pairs.Store(on && provenswitch.On())
}

// SetLearnEnded says whether the test set's learn step has ended
// (models.MockFilterParams.LearnEnded): the agent sets it from every keploy
// test filter, before the filter's view goes live. While it is set the
// session pairs nothing (pairing), and no parser's learn step decides a
// request (LearnEnded): keploy replay's retry cycles run the passing tests
// again in another order than the one they were recorded in, where a late
// call of one test read in another's window would be paired with that test's
// own recording, and what was learned of it kept.
//
// A learn step that takes up again holds no kept learning (SetKeptLearning):
// keploy sends it only with the learn step ended.
func (m *MockManager) SetLearnEnded(ended bool) {
	if !ended {
		m.exact.kept.set(nil, false)
	}
	m.exact.learnEnded.Store(ended)
	if ended {
		// What the staging's matchers filed is not served from here on,
		// whether or not the round keeps any learning (keptOverlay.onto).
		m.exact.kept.ended()
	}
}

// SetLearnHeld holds the learn step, or releases it: while it is held the
// session pairs nothing (pairing), and the learn tracker's classification
// refuses every changed value (learn.Tracker.SetLearnHeld, which the proxy
// sets beside it), but nothing else changes: it has not ended (LearnEnded),
// so every parser's learn step still decides the requests, serves what is
// exactly a recording as it is, and states what it served, as for any test,
// and releasing it takes the learn step back up as it was. The agent holds
// it while a test of record's full round whose recording the round does not
// learn onto is open (a test that keeps an earlier record's recording):
// what its calls differ in would otherwise be learned in memory onto a
// recording that keeps none of it, so the round would judge it on learning
// no later replay has. A session's start releases it.
func (m *MockManager) SetLearnHeld(held bool) {
	m.exact.learnHeld.Store(held)
}

// LearnHeld reports whether the learn step is held (SetLearnHeld).
func (m *MockManager) LearnHeld() bool {
	return m.exact.learnHeld.Load()
}

// LearnEnded implements integrations.LearnEnder: the set's learn step has
// ended (SetLearnEnded). A held one (SetLearnHeld) has not.
func (m *MockManager) LearnEnded() bool {
	return m.exact.learnEnded.Load()
}

var _ integrations.LearnEnder = (*MockManager)(nil)

// SetRecordedOfKind sets where the pairing, and HasUnservedRecording, read
// the staged set's recordings of a kind the trees do not hold (the agent's
// on-disk store of per-test mocks: DiskMocks.RangeKind, which decodes one
// only as visit loads it); nil for none, when every one is in the trees. The
// agent sets it at every staging, under either switch, before the staging,
// which installs it as the source of the set as recorded (RangeRecordedSet)
// with its trees.
func (m *MockManager) SetRecordedOfKind(src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error) {
	m.exact.ofKind.mu.Lock()
	defer m.exact.ofKind.mu.Unlock()
	m.exact.ofKind.src = src
}

// recordedOfKindSource is where the pairing reads recordings of a kind the
// trees do not hold (SetRecordedOfKind): src as the agent handed it, and
// staged as the last staging installed it (installRecordedOfKindLocked).
// mu is a leaf, taken under swapMu by the staging and by RangeRecordedSet.
type recordedOfKindSource struct {
	mu     sync.Mutex
	src    func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error
	staged func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error
}

// installRecordedOfKindLocked makes the source the agent handed last
// (SetRecordedOfKind) the staging's, holding swapMu, which the staging holds
// as it changes the trees and moves the staging epoch: a reader of the set
// as recorded that takes swapMu (RangeRecordedSet) reads one staging's trees
// and source, never one set's trees with the next set's source, which the
// agent hands before it stages that set.
func (m *MockManager) installRecordedOfKindLocked() {
	m.exact.ofKind.mu.Lock()
	m.exact.ofKind.staged = m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
}

// stagedOfKind is the staged trees' recordings of kind (per-test, session,
// startup, in that order), each once, by name.
func (m *MockManager) stagedOfKind(kind models.Kind) []*models.Mock {
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	seen := map[string]bool{}
	var out []*models.Mock
	for _, t := range trees {
		for _, mk := range m.treeValues(t, kind) {
			if mk.Name == "" || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			out = append(out, mk)
		}
	}
	return out
}

// isBackgroundRecording reports whether mk is a data-plane recording of v's
// background (the worker scope or the pool, the background rules), which an async lane
// does not serve: protocol-phase mocks, async lanes, and sources replaced by
// the open test's derived mocks are excluded.
func (v *exactView) isBackgroundRecording(mk *models.Mock) bool {
	if mk == nil || !v.bg.has(mk.Name) || mk.IsProtocolPhase() || mk.IsAsync() {
		return false
	}
	_, gone := v.sourcesReplaced[mk.Name]
	return !gone
}

// isBackgroundRecordingTo is isBackgroundRecording to a call read in a test's
// window (windowed) or between tests: to one read in a test's window the
// set's plain startup: recordings are none (startupOwned), as the view shows
// them none (sharesRestTo). A by-kind answer reads and spends by it.
func (v *exactView) isBackgroundRecordingTo(mk *models.Mock, windowed bool) bool {
	return v.isBackgroundRecording(mk) && (!windowed || !v.startupOwned(mk.Name))
}

// startupOwned reports whether name is one of the plain startup: recordings
// of the background in force in the staging epoch the view was installed in
// (MockManager.testOwnsStartupAt): a call read in a test's window is the
// test's own call in it. False with no background rules.
func (v *exactView) startupOwned(name string) bool {
	return v.bg != nil && v.bg.ownedInTest != nil && v.bg.ownedInTest(v.installed.Epoch, name)
}

// scopeReplacedSet returns the open unset-mode scope's replaced source names
// (nil when none). The map is immutable once published by SetScopeMocks.
func (m *MockManager) scopeReplacedSet() map[string]struct{} {
	m.scopeOwnedMu.Lock()
	r := m.scopeReplaced
	m.scopeOwnedMu.Unlock()
	return r
}

// unsetBackgroundCandidates returns kind's data-plane recordings that belong to
// bg when exactView is off (KEPLOY_PROVEN unset), gathered from both the seeded
// background recordings (which persist across legacy stageScope narrowings) and
// the staged trees, de-duplicated by name, omitting any source replaced by the
// open test's derived mocks.
func (m *MockManager) unsetBackgroundCandidates(kind models.Kind, bg *backgroundState) map[string]*models.Mock {
	byName := make(map[string]*models.Mock)
	replaced := m.scopeReplacedSet()
	add := func(mk *models.Mock) {
		if mk == nil || mk.Name == "" || !bg.has(mk.Name) || mk.IsProtocolPhase() || mk.IsAsync() {
			return
		}
		if _, gone := replaced[mk.Name]; gone {
			return
		}
		if _, exists := byName[mk.Name]; !exists {
			byName[mk.Name] = mk
		}
	}
	for _, rec := range m.BackgroundRecordings(kind) {
		add(rec.Mock)
	}
	for _, mk := range m.stagedOfKind(kind) {
		add(mk)
	}
	return byName
}

// inUnfilteredKind reports whether name is currently staged in the reusable
// unfiltered tree for kind.
func (m *MockManager) inUnfilteredKind(kind models.Kind, name string) bool {
	if name == "" {
		return false
	}
	m.treesMu.RLock()
	u := m.unfiltered
	m.treesMu.RUnlock()
	for _, mk := range m.treeValues(u, kind) {
		if mk != nil && mk.Name == name {
			return true
		}
	}
	return false
}

// servedBackground is the one place in the manager that decides whether it
// serves its set's background (ServeExactBackground) by the background rules,
// and so whether a call takes the exact background path. v is the exact view
// being installed (installExactWith), which keeps the answer as exactView.bg
// for every read through it; nil for a read with no exact view armed, which
// asks on each read: Background, SpentBackground, BackgroundCursor,
// InBackground, RoundSnapshot, OwnedRecordings and the consume doors
// (consumeUnsetBackground). Nothing else reads m.exact.bg. A scopedMockDb
// reads its manager through those methods, so the run's manager, each test
// worker's (ServeInstance) and every scoped view of one decide by this alone.
// Outside the manager, S12Background, ConsumeRefused and InBackground read
// the answer through BackgroundCursor and InBackground; OwnRepeatRead and the
// HTTP matcher keep their switch-on behavior and, with the switch off, act
// only where S12Background says this predicate serves the background.
// ExactServing reads only whether an exact view is armed.
//
// It returns the background, and whether a call takes the exact background
// path for it now (exact):
//   - Under the proven switch, an exact view serves the background by the
//     rules, a settling background too; with no exact view, nothing does
//     (the agent arms the view before it names the background).
//   - With the switch off, an exact view never does: a manager that serves
//     exactly (a test worker's) reads S12Background, InBackground,
//     ConsumeRefused and OwnRepeatRead as it does for a set without a
//     background. With no exact view (one test process), the settled
//     background is served by the rules to the background passes inside an
//     open test only (exact); a test's own calls keep today's matching, since
//     ExactServing stays false. Outside a test it is returned with exact
//     false: a pool recording an evicting door consumes then through today's
//     doors is spent for the background (consumeUnsetBackground), and
//     nothing else reads it.
func (m *MockManager) servedBackground(v *exactView) (bg *backgroundState, exact bool) {
	bg = m.exact.bg.Load()
	if bg == nil {
		return nil, false
	}
	if provenswitch.On() {
		if v == nil {
			return nil, false
		}
		return bg, true
	}
	if v != nil || bg.settling || m.exactView() != nil {
		return nil, false
	}
	return bg, m.scopeIsOpen()
}

// consumeUnsetBackground settles a consume of a background recording (worker:
// or @startup) when exactView is off (KEPLOY_PROVEN unset) through the same
// ServeExactBackground state (m.exact.bg). Reusable unfiltered mocks (evicts
// false and staged in m.unfiltered) and non-background mocks return
// handled=false so test-own and reusable calls continue through main's exact
// legacy doors.
func (m *MockManager) consumeUnsetBackground(mk *models.Mock, evicts bool, g *roundGuard, tier func() bool, observe func()) (handled, ok bool) {
	if mk == nil || mk.IsProtocolPhase() || mk.IsAsync() {
		return false, false
	}
	bg, exact := m.servedBackground(nil)
	if bg == nil || !bg.has(mk.Name) {
		return false, false
	}
	if !evicts && m.inUnfilteredKind(mk.Kind, mk.Name) {
		return false, false
	}
	if !exact {
		// Outside an open test scope (boot / between tests), main's legacy
		// trees own consumption so DeleteFilteredMock can fall through to
		// deleteStartupMock without stranding the mock in m.startup. What a
		// successful consume here spends in bg, so a test's window is not
		// shown it again: a @startup pool mock consumed by an evicting door;
		// and a once series' recording (answersOnce), consumed by an
		// evicting door or one that does not evict, as exact serving spends
		// it outside every test (consumeBetween). A declared startup: true
		// rule places a createIndexes made at the application's boot in
		// worker: as such a series. A guarded consume returns below before
		// any spend, and a reusable unfiltered mock's non-evicting consume
		// returned above.
		if g.checks() {
			return false, false
		}
		if tier == nil || !tier() {
			return true, false
		}
		if (evicts && bg.inPool(mk.Name)) || answersOnce(&m.exact.poolTrails, m.exact.poolSeries.Load(), mk.Name) {
			bg.trySpend(mk.Name)
		}
		if observe != nil {
			observe()
		}
		return true, true
	}
	if g.checks() && m.stagingEpoch.Load() != g.at.Epoch {
		g.refuse("the staging moved under the call")
		return true, false
	}
	if _, gone := m.scopeReplacedSet()[mk.Name]; gone {
		return true, false
	}
	if !bg.trySpend(mk.Name) {
		return true, false
	}
	if tier != nil {
		_ = tier()
	}
	m.noteExactSpent(mk)
	m.bumpViewKind(mk.Kind)
	m.bumpViewAll()
	if observe != nil {
		observe()
	}
	return true, true
}

// Background is, under the background rules, kind's background recordings
// the view shows unused (integrations.ExactBackground), in the view's order:
// the worker scope it shows (the earlier place first, delivering before idle,
// then recorded order), then the pool, in recorded order. nil for a set
// served by today's rules.
func (m *MockManager) Background(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v != nil {
		return m.backgroundTo(v, v.test != "", kind)
	}
	bg, exact := m.servedBackground(nil)
	if !exact {
		return nil
	}
	return m.unsetBackground(kind, bg)
}

// backgroundTo is Background in v to a call read in a test's window
// (windowed) or between tests, whatever view is in force as it reads
// (roundGuard.windowed): what v shows that call (showsTo), so to one read
// between tests the set's plain startup: recordings are the pool's though
// the next test opened before the read, and to one read in a test's window
// they are none (isBackgroundRecordingTo).
func (m *MockManager) backgroundTo(v *exactView, windowed bool, kind models.Kind) []*models.Mock {
	if v.bg == nil {
		return nil
	}
	var shown []*models.Mock
	for _, mk := range v.keepWith(m.stagedOfKind(kind), func(mk *models.Mock) bool { return v.showsTo(mk, windowed) }) {
		if v.isBackgroundRecordingTo(mk, windowed) {
			shown = append(shown, mk)
		}
	}
	return shown
}

// unsetBackground is Background with no exact view, for bg, the background
// servedBackground serves in an open test: kind's unused recordings of it,
// the worker scope in its order first, then the pool in recorded order.
func (m *MockManager) unsetBackground(kind models.Kind, bg *backgroundState) []*models.Mock {
	byName := m.unsetBackgroundCandidates(kind, bg)
	var workers, pool []*models.Mock
	seen := make(map[string]bool, len(byName))
	for _, name := range bg.workerOrder {
		if mk, ok := byName[name]; ok && !bg.isSpent(name) {
			workers = append(workers, mk)
			seen[name] = true
		}
	}
	for name, mk := range byName {
		if seen[name] || bg.inWorker(name) || bg.isSpent(name) {
			continue
		}
		pool = append(pool, mk)
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if !pool[i].Spec.ReqTimestampMock.Equal(pool[j].Spec.ReqTimestampMock) {
			return pool[i].Spec.ReqTimestampMock.Before(pool[j].Spec.ReqTimestampMock)
		}
		return pool[i].Name < pool[j].Name
	})
	return append(workers, pool...)
}

// SpentBackground is, under the background rules, kind's background
// recordings this staging consumed (integrations.ExactBackground), read-only,
// in recorded order: what an answer by kind reuses once its kind's recordings
// are used up. In a test's view it leaves out the set's plain startup:
// recordings (spentBackgroundTo). nil for a set served by today's rules.
func (m *MockManager) SpentBackground(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v != nil {
		return m.spentBackgroundTo(v, v.test != "", kind)
	}
	bg, exact := m.servedBackground(nil)
	if !exact {
		return nil
	}
	return m.unsetSpentBackground(kind, bg)
}

// spentBackgroundTo is SpentBackground in v to a call read in a test's
// window (windowed) or between tests: to one read in a test's window, none
// of the set's plain startup: recordings (isBackgroundRecordingTo).
func (m *MockManager) spentBackgroundTo(v *exactView, windowed bool, kind models.Kind) []*models.Mock {
	if v.bg == nil {
		return nil
	}
	var out []*models.Mock
	for _, mk := range m.stagedOfKind(kind) {
		if v.isBackgroundRecordingTo(mk, windowed) && v.spentBackground(mk.Name) {
			out = append(out, mk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Spec.ReqTimestampMock.Before(out[j].Spec.ReqTimestampMock)
	})
	return out
}

// unsetSpentBackground is SpentBackground with no exact view, for bg, the
// background servedBackground serves in an open test, in recorded order.
func (m *MockManager) unsetSpentBackground(kind models.Kind, bg *backgroundState) []*models.Mock {
	var out []*models.Mock
	for name, mk := range m.unsetBackgroundCandidates(kind, bg) {
		if bg.isSpent(name) {
			out = append(out, mk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Spec.ReqTimestampMock.Equal(out[j].Spec.ReqTimestampMock) {
			return out[i].Spec.ReqTimestampMock.Before(out[j].Spec.ReqTimestampMock)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

var _ integrations.ExactBackground = (*MockManager)(nil)

// KindBackground is, under the background rules, kind's whole background
// as answering by kind reads it (integrations.ByKindBackground): its
// data-plane recordings, no async lane's, of the worker scope at every place,
// reached or not, and of the pool, split into those unused and those this
// staging spent, each in recorded order. By-kind answers ignore Place, so its
// answer does not depend on the places the run has reached. In a test's view
// it leaves out the set's plain startup: recordings (kindBackgroundTo).
// nil, nil for a set served by today's rules, and with exact serving off.
func (m *MockManager) KindBackground(kind models.Kind) (unused, spent []*models.Mock) {
	v := m.exactView()
	if v == nil {
		return nil, nil
	}
	return m.kindBackgroundTo(v, v.test != "", kind)
}

// kindBackgroundTo is KindBackground in v to a call read in a test's window
// (windowed) or between tests: to one read in a test's window, none of the
// set's plain startup: recordings (isBackgroundRecordingTo), which such a
// call is the test's own call in.
func (m *MockManager) kindBackgroundTo(v *exactView, windowed bool, kind models.Kind) (unused, spent []*models.Mock) {
	if v.bg == nil {
		return nil, nil
	}
	for _, mk := range m.stagedOfKind(kind) {
		if !v.isBackgroundRecordingTo(mk, windowed) {
			continue
		}
		if v.spentBackground(mk.Name) {
			spent = append(spent, mk)
		} else {
			unused = append(unused, mk)
		}
	}
	byRecorded := func(ms []*models.Mock) {
		sort.SliceStable(ms, func(i, j int) bool {
			return ms[i].Spec.ReqTimestampMock.Before(ms[j].Spec.ReqTimestampMock)
		})
	}
	byRecorded(unused)
	byRecorded(spent)
	return unused, spent
}

// SpendByKind spends mk, one of KindBackground's unused, for the staging
// (integrations.ByKindBackground): a by-kind answer's consumption, whatever
// mk's place, as the delete doors spend a background recording the view shows
// (exactConsume). False when mk is no background recording of the set, was
// spent already, or the set is served by today's rules.
//
// A spend that finds its view closed (a test's begin or end replacing it) is
// no refusal: it waits until the replacing view is installed and the
// revisions moved, and spends against that one (closedWait), as exactConsume
// does. While a view is in force, its close never refuses a spend.
func (m *MockManager) SpendByKind(mk *models.Mock) bool {
	ok, _ := m.spendByKindAt(mk, nil)
	return ok
}

// spendByKindAt is SpendByKind settled holding the round lock, its take
// written to the staging's take log in the same step, and, with g, only
// while the store is still at g's round. why says what refused it.
func (m *MockManager) spendByKindAt(mk *models.Mock, g *roundGuard) (spent bool, why string) {
	why = "it is no unused background recording of the set"
	spent = settleInForce(m, false, func(v *exactView) (bool, <-chan struct{}) {
		if v == nil || v.bg == nil || !v.isBackgroundRecordingTo(mk, g.windowed(v)) {
			if g != nil && v == nil {
				why = "exact serving is off"
			}
			return false, nil
		}
		m.prepareOutside()
		m.lockRound()
		// Refused when the round moved; settled again in the view that
		// replaces a closing one, where it did (exactConsumeAt).
		if ok, wait := m.guardLocked(g, v); !ok {
			if g.moved != "" {
				why = g.moved
			}
			return false, wait
		}
		ok, wait := m.spendByKindIn(v, mk, g)
		m.exact.round.mu.Unlock()
		if ok {
			why = ""
			m.noteExactSpent(mk)
		}
		return ok, wait
	})
	return spent, why
}

// spendByKindIn is SpendByKind settled against v, holding the round lock.
// wait, when not nil, says v is closed: nothing was spent, and the spend is
// to be settled again once wait is closed. A worker-scope recording is read
// and spent for the view g's call was read in (roundGuard.windowed), as
// exactConsumeIn does.
func (m *MockManager) spendByKindIn(v *exactView, mk *models.Mock, g *roundGuard) (bool, <-chan struct{}) {
	if v == nil || v.bg == nil {
		return false, nil
	}
	windowed := g.windowed(v)
	if !v.isBackgroundRecordingTo(mk, windowed) {
		return false, nil
	}
	if v.spentIn(mk.Name) || v.spentOutsideIn(mk.Name, windowed) {
		return false, nil
	}
	// The view's closed check and the spend are one step, as exactConsume's.
	v.mu.Lock()
	if wait := v.closedWait(); wait != nil {
		v.mu.Unlock()
		return false, wait
	}
	if v.spendsBetween(mk, windowed) {
		// Outside every window one of the worker scope's answers again
		// (consumeBetween): it is listed spent for the next by-kind answer
		// between tests (spentBackground), which reuses it once its kind's
		// unused ones are used up, and the other doors still read it.
		verdict, first := m.consumeBetween(mk)
		v.mu.Unlock()
		if first {
			m.bumpGapOrder(mk.Kind)
		}
		return verdict == exactServed, nil
	}
	verdict := m.consumeOutside(mk, v.spendsInTests(mk, windowed))
	v.mu.Unlock()
	if verdict != exactServed {
		return false, nil
	}
	m.takeLocked(integrations.Take{Mock: m.stagedRecording(mk), How: integrations.TakeSpent, Gen: v.round, Test: v.test})
	return true, nil
}

var _ integrations.ByKindBackground = (*MockManager)(nil)

// spentBackground reports whether name, one of the background's recordings,
// is spent: consumed in the view, or for the staging.
func (v *exactView) spentBackground(name string) bool {
	return v.spentIn(name) || v.spentOutside(name) || v.usedBetween(name)
}

// usedBetween reports whether name, one of the worker scope's, answered a
// call read with no test open in the staging, read in a view with no test
// open (outsideState.between): it is no spend, and orders the gap's reads
// alone.
func (v *exactView) usedBetween(name string) bool {
	return v.test == "" && v.outside != nil && v.isWorker(name) && v.outside.consumedBetween(name, v.epoch.Load())
}

// BackgroundCursor is, under the background rules, the stateful cursor of
// the set's background (integrations.BackgroundCursor): the one the calls
// between tests advance, under the pool views' key (cursorSlotIn), which a
// call inside a test that a background recording answers reads and advances
// too. The background's recorded sequence is then one whether its calls land
// inside a test or between tests (its cursor is shared with
// background callers). False for a set served by today's rules, and with no
// exact view unless servedBackground serves the background by the rules
// (KEPLOY_PROVEN unset, inside an open test).
func (m *MockManager) BackgroundCursor() (integrations.MockCursor, bool) {
	v := m.exactView()
	if v != nil {
		if v.bg == nil {
			return nil, false
		}
		return backgroundCursor{m: m}, true
	}
	if _, exact := m.servedBackground(nil); exact {
		return backgroundCursor{m: m}, true
	}
	return nil, false
}

// InBackground implements integrations.BackgroundMember: name is one of the
// background's recordings, and the view in force holds it as no test's own
// (one judging moved into a test is that test's, as hidesName reads it): the
// pool's or the worker scope's of a set the view serves by the background
// rules, or, in a test under any rules, one of the worker scope the run
// reached (isWorker). With exact serving off, the background's recordings
// while servedBackground serves it by the rules: inside an open test only.
func (m *MockManager) InBackground(name string) bool {
	v := m.exactView()
	if v == nil {
		bg, exact := m.servedBackground(nil)
		return exact && bg.has(name)
	}
	// Neither the background's nor, in a test, the reached worker scope's.
	if !v.bg.has(name) && (v.test == "" || !v.isWorker(name)) {
		return false
	}
	_, own := v.index[name]
	return !own
}

var _ integrations.BackgroundMember = (*MockManager)(nil)

// backgroundCursor is the background's stateful cursor (BackgroundCursor):
// the one the calls between tests advance.
type backgroundCursor struct{ m *MockManager }

func (c backgroundCursor) MockCursorIndex(key string, n int) int {
	return c.m.cursorIndexAt(cursorSlot{key: c.m.backgroundCursorKey(key)}, n)
}

func (c backgroundCursor) AdvanceMockCursor(key string, servedIdx, n int) {
	c.m.advanceCursorAt(cursorSlot{key: c.m.backgroundCursorKey(key)}, servedIdx, n)
}

// backgroundCursorKey is the cursor key a call between tests gives key
// (cursorSlotIn): under the key of the pool view installed last.
func (m *MockManager) backgroundCursorKey(key string) string {
	pool := models.PoolKeyNone
	if p := m.exact.poolCursor.Load(); p != nil {
		pool = *p
	}
	return pool + "\x00" + key
}

var _ integrations.BackgroundCursor = (*MockManager)(nil)

// windowsCursor is, in a test under any rules, the worker scope's stateful
// cursor (SequenceView.Between): one sequence across the test windows of the
// staging, kept apart from the one the calls between tests advance
// (backgroundCursor), so no gap call walks or saturates what a window reads.
// It is under the key of the pool view installed last, as the calls between
// tests are, so a test process claimed again (RestartExactKey) starts it
// over and two keys' windows never share a sequence.
type windowsCursor struct{ m *MockManager }

// windowsCursorTag sets the windows' cursors apart from the calls between
// tests' under one pool key; no request key begins with a NUL.
const windowsCursorTag = "\x00windows\x00"

func (c windowsCursor) MockCursorIndex(key string, n int) int {
	return c.m.cursorIndexAt(cursorSlot{key: c.m.backgroundCursorKey(windowsCursorTag + key)}, n)
}

func (c windowsCursor) AdvanceMockCursor(key string, servedIdx, n int) {
	c.m.advanceCursorAt(cursorSlot{key: c.m.backgroundCursorKey(windowsCursorTag + key)}, servedIdx, n)
}

// learns reports whether the running set's learn step is on: the session is
// record's full round (SetPairing) and the set's learn step has not ended
// (SetLearnEnded). A held one (SetLearnHeld) is on: it still reads the set's
// recordings to serve what is exactly one, so the staging's read of them
// (warmLearnSet) stands while it is held.
func (m *MockManager) learns() bool {
	return m.exact.pairs.Load() && !m.exact.learnEnded.Load()
}

// pairing is the view to pair in, and the staging's epoch: a view of a set
// served by the background rules, its background named (not settling), while
// the session pairs and the set's learn step has not ended (SetLearnEnded)
// and is not held (SetLearnHeld); nil when none is.
func (m *MockManager) pairing() (*exactView, uint64) {
	if !m.exact.pairs.Load() || m.exact.learnEnded.Load() || m.exact.learnHeld.Load() {
		return nil, 0
	}
	v := m.exactView()
	if v == nil || v.bg == nil || v.bg.settling {
		return nil, 0
	}
	return v, m.stagingEpoch.Load()
}

// pairable reports whether mk may be paired in v (integrations.Pairing): a
// data-plane recording, no async lane's, of a test (neither the background's
// nor the view's own: the open test's, or the next test's calls in flight a
// pool view serves), that the set's calls have not consumed and that no
// pairing of the staging epoch has claimed.
func (m *MockManager) pairable(v *exactView, epoch uint64, mk *models.Mock) bool {
	if mk == nil || mk.IsProtocolPhase() || mk.IsAsync() {
		return false
	}
	return m.pairableName(v, epoch, mk.Name)
}

// pairableName is what pairable tells by a recording's name alone: another
// test's (ownedElsewhere), not claimed in the staging epoch. The on-disk
// candidates are read by it before they are decoded, so a round decodes only
// recordings a pairing may still take.
func (m *MockManager) pairableName(v *exactView, epoch uint64, name string) bool {
	return m.ownedElsewhere(v, name) && !m.exact.claimed.has(epoch, name)
}

// ownedElsewhere reports whether name is a recording of a test a pairing may
// take, claims apart: neither the background's nor the view's own (the open
// test's, or the next test's calls in flight a pool view serves), and not
// consumed in the set.
func (m *MockManager) ownedElsewhere(v *exactView, name string) bool {
	if name == "" || v.bg.has(name) {
		return false
	}
	if _, own := v.index[name]; own {
		return false
	}
	return !m.consumedInSet(name)
}

// PairingCandidates is, while the session pairs (record's full round, under
// the proven switch) in a set served by the background rules, kind's
// data-plane recordings other tests own that their owners have not consumed
// and no pairing has claimed (integrations.Pairing), in recorded order: those
// the trees hold, then those the agent keeps on disk (SetRecordedOfKind),
// each once. nil when the session does not pair.
func (m *MockManager) PairingCandidates(kind models.Kind) []*models.Mock {
	v, epoch := m.pairing()
	if v == nil {
		return nil
	}
	return m.pairableOfKind(v, epoch, kind)
}

// pairableOfKind is kind's recordings a pairing may take in v (pairable), in
// recorded order: those the trees hold, then those the agent keeps on disk
// (SetRecordedOfKind), each once.
func (m *MockManager) pairableOfKind(v *exactView, epoch uint64, kind models.Kind) []*models.Mock {
	m.exact.ofKind.mu.Lock()
	src := m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
	return m.pairableFrom(v, epoch, kind, m.stagedOfKind(kind), src)
}

// pairableUnderKeys is pairableOfKind of the recordings filed under any of
// keys (models.RequestKeys) alone: the trees' by their index
// (TreeDb.keyedValues), and those the agent keeps on disk by its store's
// (SetRecordingSource), so a recording filed under none is never decoded.
func (m *MockManager) pairableUnderKeys(v *exactView, epoch uint64, kind models.Kind, keys []string) []*models.Mock {
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	seen := map[string]bool{}
	var staged []*models.Mock
	for _, t := range trees {
		if t == nil {
			continue
		}
		for _, mk := range t.keyedValues(requestKeyIndex, keys, kind) {
			if mk.Name == "" || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			staged = append(staged, mk)
		}
	}
	m.exact.recorded.mu.Lock()
	src := m.exact.recorded.src
	m.exact.recorded.mu.Unlock()
	var ofKind func(models.Kind, func(string, func() (*models.Mock, error)) bool) error
	if src != nil {
		ofKind = func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
			return src(kind, keys, visit)
		}
	}
	return m.pairableFrom(v, epoch, kind, staged, ofKind)
}

// pairableFrom is, of staged (the trees' recordings of kind) and of what src
// visits (those the agent keeps on disk), the recordings a pairing may take
// in v (pairable), in recorded order, each once: one on disk is decoded only
// once its name passed (pairableName).
func (m *MockManager) pairableFrom(v *exactView, epoch uint64, kind models.Kind, staged []*models.Mock,
	src func(models.Kind, func(string, func() (*models.Mock, error)) bool) error) []*models.Mock {
	seen := map[string]bool{}
	var out []*models.Mock
	for _, mk := range staged {
		seen[mk.Name] = true
		if m.pairable(v, epoch, mk) {
			out = append(out, mk)
		}
	}
	if src != nil {
		err := src(kind, func(name string, load func() (*models.Mock, error)) bool {
			if seen[name] {
				return true
			}
			seen[name] = true
			if !m.pairableName(v, epoch, name) {
				return true
			}
			mk, err := load()
			if err != nil {
				if m.logger != nil {
					m.logger.Warn("record's full round could not read a recording kept on disk, so no call pairs with it: a call that is that recording misses where it was made",
						zap.String("mock", name), zap.Error(err),
						zap.String("next_step", "fix the error above and record the test set again"))
				}
				return true
			}
			if mk != nil && mk.Kind == kind && m.pairable(v, epoch, mk) {
				out = append(out, mk)
			}
			return true
		})
		if err != nil && m.logger != nil {
			m.logger.Warn("record's full round could not read the test set's recordings kept on disk, so no call pairs with those it did not read: a call that is one misses where it was made",
				zap.String("kind", string(kind)), zap.Error(err),
				zap.String("next_step", "fix the error above and record the test set again"))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Spec.ReqTimestampMock.Before(out[j].Spec.ReqTimestampMock)
	})
	return out
}

// ClaimPairing claims mk for one pairing in this staging
// (integrations.Pairing): false when another call claimed it, its owner
// consumed it, it is no recording a pairing may take (pairable), or the
// session does not pair. A claim is no consumption: mk stays its owner's,
// and its owner's test may still consume it.
func (m *MockManager) ClaimPairing(mk *models.Mock) bool {
	ok, _ := m.claimPairingAt(mk, nil)
	return ok
}

// claimPairingAt is ClaimPairing holding the round lock, the claim written
// to the staging's take log in the same step, and, with g, only while the
// store is still at g's round. why says what refused it.
func (m *MockManager) claimPairingAt(mk *models.Mock, g *roundGuard) (claimed bool, why string) {
	for {
		m.lockRound()
		ok, wait := m.guardLocked(g, m.exactView())
		if ok {
			claimed, why = m.claimPairingLocked(mk)
			m.exact.round.mu.Unlock()
			return claimed, why
		}
		if wait == nil {
			return false, g.moved
		}
		// The view is closing: the round moves once its replacement is in
		// force, and the claim is refused for that then.
		m.waitReplaced(wait)
	}
}

// claimPairingLocked is ClaimPairing holding the round lock.
func (m *MockManager) claimPairingLocked(mk *models.Mock) (claimed bool, why string) {
	v, epoch := m.pairing()
	switch {
	case v == nil:
		return false, "the session does not pair"
	case mk == nil || mk.IsProtocolPhase() || mk.IsAsync():
		return false, "it is no recording a pairing may take"
	case !m.ownedElsewhere(v, mk.Name) || m.takenInStagingLocked(mk.Name):
		return false, "it is no other test's recording the set left unused: the background's, the view's own, or consumed or spent in the staging"
	}
	// The claim is the one guard against a second: one step, so two calls
	// that race for a recording never both take it.
	if !m.exact.claimed.claim(epoch, mk.Name) {
		return false, "another call claimed it already"
	}
	m.takeLocked(integrations.Take{Mock: mk, How: integrations.TakeClaimed, Gen: v.round, Test: v.test})
	return true, ""
}

var _ integrations.Pairing = (*MockManager)(nil)
