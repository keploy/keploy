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
//     the open test's own and the worker scope (exactView.shows, keep). One a
//     parser consumes through a delete door is spent for the staging, as one
//     consumed outside every test is.
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
	pool     map[string]struct{}
	worker   map[string]struct{}
	settling bool
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
// proven routing. Such a view names no recording of the background, since
// the trees still hold the previous set's, whose names may be this set's
// (names are unique only within a set), and pairs nothing (pairing). Any
// other rules are today's, as ServeExactBackground's. Whatever the rules, a
// view installed from now until ServeExactBackground settles the set is a
// staging view: a call it refuses is told it was refused while the set's
// mocks were being staged (exactMissStaging). It installs no view itself.
func (m *MockManager) StageExactBackground(rules string) {
	m.exact.staging.Store(true)
	if rules == models.RecordedWithS12Routing && provenswitch.On() {
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
	if rules != models.RecordedWithS12Routing || !provenswitch.On() {
		m.exact.bg.Store(nil)
		return
	}
	b := &backgroundState{pool: make(map[string]struct{}, len(pool)), worker: make(map[string]struct{}, len(worker))}
	for _, n := range pool {
		if n != "" {
			b.pool[n] = struct{}{}
		}
	}
	for _, n := range worker {
		if n != "" {
			b.worker[n] = struct{}{}
		}
	}
	m.exact.bg.Store(b)
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

// LearnEnded implements integrations.LearnEnder: the set's learn step has
// ended (SetLearnEnded).
func (m *MockManager) LearnEnded() bool {
	return m.exact.learnEnded.Load()
}

var _ integrations.LearnEnder = (*MockManager)(nil)

// SetRecordedOfKind sets where the pairing reads the staged set's recordings
// of a kind the trees do not hold (the agent's on-disk store of per-test
// mocks: DiskMocks.RangeKind, which decodes one only as visit loads it); nil
// for none, when every one is in the trees. The agent sets it as it stages a
// set.
func (m *MockManager) SetRecordedOfKind(src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error) {
	m.exact.ofKind.mu.Lock()
	defer m.exact.ofKind.mu.Unlock()
	m.exact.ofKind.src = src
}

// recordedOfKindSource is where the pairing reads recordings of a kind the
// trees do not hold (SetRecordedOfKind).
type recordedOfKindSource struct {
	mu  sync.Mutex
	src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error
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
// does not serve: protocol-phase mocks and async lanes keep today's reuse.
func (v *exactView) isBackgroundRecording(mk *models.Mock) bool {
	return mk != nil && v.bg.has(mk.Name) && !mk.IsProtocolPhase() && !mk.IsAsync()
}

// Background is, under the background rules, kind's background recordings
// the view shows unused (integrations.ExactBackground), in the view's order:
// the worker scope it shows (the earlier place first, delivering before idle,
// then recorded order), then the pool, in recorded order. nil for a set
// served by today's rules, and with exact serving off.
func (m *MockManager) Background(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v == nil || v.bg == nil {
		return nil
	}
	var shown []*models.Mock
	for _, mk := range v.keep(m.stagedOfKind(kind)) {
		if v.isBackgroundRecording(mk) {
			shown = append(shown, mk)
		}
	}
	return shown
}

// SpentBackground is, under the background rules, kind's background
// recordings this staging consumed (integrations.ExactBackground), read-only,
// in recorded order: what an answer by kind reuses once its kind's recordings
// are used up. nil for a set served by today's rules.
func (m *MockManager) SpentBackground(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v == nil || v.bg == nil {
		return nil
	}
	var out []*models.Mock
	for _, mk := range m.stagedOfKind(kind) {
		if v.isBackgroundRecording(mk) && v.spentBackground(mk.Name) {
			out = append(out, mk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Spec.ReqTimestampMock.Before(out[j].Spec.ReqTimestampMock)
	})
	return out
}

var _ integrations.ExactBackground = (*MockManager)(nil)

// KindBackground is, under the background rules, kind's whole background
// as answering by kind reads it (integrations.ByKindBackground): its
// data-plane recordings, no async lane's, of the worker scope at every place,
// reached or not, and of the pool, split into those unused and those this
// staging spent, each in recorded order. By-kind answers ignore Place, so its
// answer does not depend on the places the run has reached.
// nil, nil for a set served by today's rules, and with exact serving off.
func (m *MockManager) KindBackground(kind models.Kind) (unused, spent []*models.Mock) {
	v := m.exactView()
	if v == nil || v.bg == nil {
		return nil, nil
	}
	for _, mk := range m.stagedOfKind(kind) {
		if !v.isBackgroundRecording(mk) {
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
		if v == nil || v.bg == nil || !v.isBackgroundRecording(mk) {
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
		ok, wait := m.spendByKindIn(v, mk)
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
// to be settled again once wait is closed.
func (m *MockManager) spendByKindIn(v *exactView, mk *models.Mock) (bool, <-chan struct{}) {
	if v == nil || v.bg == nil || !v.isBackgroundRecording(mk) || v.spentBackground(mk.Name) {
		return false, nil
	}
	// The view's closed check and the spend are one step, as exactConsume's.
	v.mu.Lock()
	if wait := v.closedWait(); wait != nil {
		v.mu.Unlock()
		return false, wait
	}
	verdict := m.consumeOutside(mk)
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
	return v.spentIn(name) || v.spentOutside(name)
}

// BackgroundCursor is, under the background rules, the stateful cursor of
// the set's background (integrations.BackgroundCursor): the one the calls
// between tests advance, under the pool views' key (cursorSlotIn), which a
// call inside a test that a background recording answers reads and advances
// too. The background's recorded sequence is then one whether its calls land
// inside a test or between tests (its cursor is shared with
// background callers). False for a set served by today's rules, and with
// exact serving off.
func (m *MockManager) BackgroundCursor() (integrations.MockCursor, bool) {
	v := m.exactView()
	if v == nil || v.bg == nil {
		return nil, false
	}
	return backgroundCursor{m: m}, true
}

// InBackground implements integrations.BackgroundMember: name is one of the
// background's recordings, and the view in force holds it as no test's own
// (one judging moved into a test is that test's, as hidesName reads it): the
// pool's or the worker scope's of a set the view serves by the background
// rules, or, in a test under any rules, one of the worker scope the run
// reached (isWorker). False with exact serving off.
func (m *MockManager) InBackground(name string) bool {
	v := m.exactView()
	if v == nil || !v.bg.has(name) && !(v.test != "" && v.isWorker(name)) {
		return false
	}
	_, own := v.index[name]
	return !own
}

var _ integrations.BackgroundMember = (*MockManager)(nil)

// backgroundCursor is the background's stateful cursor (BackgroundCursor),
// and, in a test under any rules, the worker scope's (SequenceView.Between):
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

// learns reports whether the running set's learn step is on: the session is
// record's full round (SetPairing) and the set's learn step has not ended
// (SetLearnEnded).
func (m *MockManager) learns() bool {
	return m.exact.pairs.Load() && !m.exact.learnEnded.Load()
}

// pairing is the view to pair in, and the staging's epoch: a view of a set
// served by the background rules, its background named (not settling), while
// the session pairs and the set's learn step has not ended (SetLearnEnded);
// nil when none is.
func (m *MockManager) pairing() (*exactView, uint64) {
	if !m.exact.pairs.Load() || m.exact.learnEnded.Load() {
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
	seen := map[string]bool{}
	var out []*models.Mock
	for _, mk := range m.stagedOfKind(kind) {
		seen[mk.Name] = true
		if m.pairable(v, epoch, mk) {
			out = append(out, mk)
		}
	}
	m.exact.ofKind.mu.Lock()
	src := m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
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
