package proxy

import (
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// S12'S BACKGROUND RULES IN EXACT SERVING (replay.RegisterS12Serving).
//
// A set staged under S12's background rules (ServeExactBackground, which the
// agent calls only for a set an S12 record marked, models.RecordedWithS12Routing,
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

// backgroundState is a staged set's background under S12's rules: its pool
// and its whole worker scope, by name.
type backgroundState struct {
	pool   map[string]struct{}
	worker map[string]struct{}
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

// ServeExactBackground sets the rules the staged set's background is served
// by: S12's when rules is models.RecordedWithS12Routing (with the set's pool
// and its whole worker scope by name), today's for any other rules (pool and
// worker unread). It
// installs no view itself: the next ServeExactTest or ServeExactPool shows it.
// The agent calls it at each set's staging, before the set's first view, so a
// set never inherits the previous one's rules.
func (m *MockManager) ServeExactBackground(rules string, pool, worker []string) {
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
		m.logger.Debug("exact serving: the set's background is served by S12's rules",
			zap.Int("pool", len(b.pool)), zap.Int("worker", len(b.worker)))
	}
}

// SetPairing says whether the session pairs (integrations.Pairing): record's
// full round's, OutgoingOptions.Learn under the proven switch. Proxy.Mock
// sets it for each session.
func (m *MockManager) SetPairing(on bool) {
	m.exact.pairs.Store(on && provenswitch.On())
}

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
// background (the worker scope or the pool, S12's rules), which an async lane
// does not serve: protocol-phase mocks and async lanes keep today's reuse.
func (v *exactView) isBackgroundRecording(mk *models.Mock) bool {
	return mk != nil && v.bg.has(mk.Name) && !mk.IsProtocolPhase() && !mk.IsAsync()
}

// Background is, under S12's background rules, kind's background recordings
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

// SpentBackground is, under S12's background rules, kind's background
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

// KindBackground is, under S12's background rules, kind's whole background
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
	return settleInForce(m, false, func(v *exactView) (bool, <-chan struct{}) {
		return m.spendByKindIn(v, mk)
	})
}

// spendByKindIn is SpendByKind settled against v. wait, when not nil, says v
// is closed: nothing was spent, and the spend is to be settled again once
// wait is closed.
func (m *MockManager) spendByKindIn(v *exactView, mk *models.Mock) (bool, <-chan struct{}) {
	if v == nil || v.bg == nil || !v.isBackgroundRecording(mk) || v.spentBackground(mk.Name) {
		return false, nil
	}
	m.prepareOutside()
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
	m.noteExactSpent(mk)
	return true, nil
}

var _ integrations.ByKindBackground = (*MockManager)(nil)

// spentBackground reports whether name, one of the background's recordings,
// is spent: consumed in the view, or for the staging.
func (v *exactView) spentBackground(name string) bool {
	v.mu.Lock()
	spent := v.spent[name]
	v.mu.Unlock()
	return spent || v.spentOutside(name)
}

// BackgroundCursor is, under S12's background rules, the stateful cursor of
// the set's background (integrations.BackgroundCursor): the one the calls
// between tests advance, under the pool views' key (exactCursorKey), which a
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

// backgroundCursor is the background's stateful cursor (BackgroundCursor).
type backgroundCursor struct{ m *MockManager }

func (c backgroundCursor) MockCursorIndex(key string, n int) int {
	return c.m.cursorIndexAt(c.m.backgroundCursorKey(key), true, n)
}

func (c backgroundCursor) AdvanceMockCursor(key string, servedIdx, n int) {
	c.m.advanceCursorAt(c.m.backgroundCursorKey(key), true, servedIdx, n)
}

// backgroundCursorKey is the cursor key a call between tests gives key
// (exactCursorKey): under the key of the pool view installed last.
func (m *MockManager) backgroundCursorKey(key string) string {
	pool := models.PoolKeyNone
	if p := m.exact.poolCursor.Load(); p != nil {
		pool = *p
	}
	return pool + "\x00" + key
}

var _ integrations.BackgroundCursor = (*MockManager)(nil)

// pairing is the view to pair in, and the staging's epoch: a view of a set
// served by S12's rules, while the session pairs; nil when none is.
func (m *MockManager) pairing() (*exactView, uint64) {
	if !m.exact.pairs.Load() {
		return nil, 0
	}
	v := m.exactView()
	if v == nil || v.bg == nil {
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
// the proven switch) in a set served by S12's background rules, kind's
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
	v, epoch := m.pairing()
	if v == nil || mk == nil || mk.IsProtocolPhase() || mk.IsAsync() || !m.ownedElsewhere(v, mk.Name) {
		return false
	}
	// The claim is the one guard against a second: one step, so two calls
	// that race for a recording never both take it.
	return m.exact.claimed.claim(epoch, mk.Name)
}

var _ integrations.Pairing = (*MockManager)(nil)
