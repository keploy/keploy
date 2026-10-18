package proxy

import (
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Exact serving: what a parser may answer a request with, under the proven
// switch (design v14 §5.2, S12). It is the one path that decides what a
// parser sees there, whatever tier a mock was staged in and whatever lifetime
// its kind derives; the agent turns it on for a replay and moves it at every
// begin and end of a test.
//
//   - While a test is open, the test's own mocks (the data-plane mocks of its
//     mapping) answer, each at most one request, in recorded order, and the
//     pool answers only protocol-phase requests (handshakes, session
//     settings, DNS). One more request identical to one a test mock already
//     answered finds no mock: a miss (models.MissExtraCall).
//   - While no test is open, the current key's pool slice answers, with
//     reuse or cursor-saturate as its mocks were recorded, beside the
//     protocol-phase mocks. Its stateful cursors are the key's own.
//
// A data-plane recording a parser consumes through a delete door while no
// test is open (a call before the first test or between tests) is consumed
// for the staging, not for the view (founder decision 31): no begin or end
// brings it back, and the next staging (stagingEpoch) does. The manager
// gives no last answer to repeat: which recordings are one call's sequence
// is the parser's to know (its matching), so a parser that has one
// saturates itself (HTTP's stateful cursor; SQS answers a drained
// ReceiveMessage with its empty answer), and an exhausted call of any other
// misses. A protocol-phase recording (a handshake, ZooKeeper's connect, an
// SQS config call) stays reusable as before: one consumed through a delete
// door is gone until the view changes. In a test, a consume through a
// delete door is gone for the rest of the test, as before.
//
// The staged trees keep every mock of the set. Each read a parser makes is
// narrowed to what the view shows, and the open test's mocks are read from
// the view itself, as per-test mocks (the manager's copies, consumed through
// whichever door a parser uses: DeleteFilteredMock, DeleteUnFilteredMock,
// DeleteStartupMock, UpdateUnFilteredMock or MarkMockAsUsed). A consume of a
// test mock that already answered, or of a mock the view does not show,
// fails, so two requests that read the same snapshot cannot both be answered
// by one test mock. Each change of what the view shows (a begin, an end, a
// test mock answering) bumps the revisions, so a parser that keeps an index
// of what it read reads again.

// exactView is what exact serving shows. It is replaced, never changed,
// except for which of its test mocks have answered (mu).
type exactView struct {
	// test is the open test's ID; "" while no test is open.
	test string
	// gen is the test view's own number: each copy it serves carries it
	// (TestModeInfo.ServedIn), so a copy read in an earlier view, a previous
	// attempt of the same test, cannot answer for this one.
	gen uint64
	// owned are the open test's test mocks, the manager's own copies, in
	// recorded order; index is each one's position, by name. session marks
	// those staged in the session tier (their kind derives a reusable
	// lifetime): a parser that reads only that tier finds them there too.
	owned   []*models.Mock
	index   map[string]int
	session []bool
	// served is, on a pool view (no test open), the copy each owned mock
	// answered with, by position: what its consume says (noise, variables)
	// when it is flagged consumed, as the next test opens (ServeExactTest),
	// not when it answered, so the ended test's drain never reads it.
	served []*models.Mock
	// carried names, on a test view, its mocks the pool view before it
	// answered (ServeExactPool's ahead): the variables their answers bound
	// are this test's (MockManager.StartVariables).
	carried map[string]bool
	// inFlight names, on a test view, its mocks the pool view before it
	// served as its calls in flight when it began, answered or not: recorded
	// in the gap before its window, and its own (InFlightIntoOpenTest).
	inFlight map[string]bool
	// pool is, while no test is open, the names of the pool slice served.
	pool map[string]struct{}
	// poolKey is the key of that slice: its stateful cursors are its own.
	poolKey string

	mu sync.Mutex
	// closed: another view replaced this one; nothing consumes in it after
	// (installExact), so what it says it consumed is final.
	closed bool
	// consumed says which owned mock has answered; order lists their
	// positions in the order they did.
	consumed []bool
	order    []int
	// spent are the staged mocks this view shows that a parser consumed
	// through a delete door in its test, or a protocol-phase mock one
	// evicted outside every test: gone until the view changes. The staged
	// trees keep them. A data-plane recording consumed outside every test is
	// the staging's (outside), not the view's.
	spent map[string]bool
	// outside is the manager's outsideAnswers, and epoch its stagingEpoch.
	outside *outsideAnswers
	epoch   *atomic.Uint64
}

// exactState is a manager's exact serving: off until the agent turns it on
// for a replay session (view nil), off again at the next one.
type exactState struct {
	view atomic.Pointer[exactView]
	// fired: a test has begun since exact serving was turned on. It is what
	// tells a tier-routing parser (Postgres v3) "between tests" from
	// "before the first test".
	fired atomic.Bool
	// last is the latest test view, open or ended: its outcome is read after
	// the test stops being served (ExactTestOutcome).
	last atomic.Pointer[exactView]
	// gen numbers the test views.
	gen atomic.Uint64
	// outside is what the calls made outside every test consumed in the
	// current staging (outsideAnswers).
	outside outsideAnswers
}

// outsideAnswers is the staged data-plane recordings a parser consumed
// through a delete door while no test was open, in one staging
// (stagingEpoch): hidden from every read of exact serving until the next
// staging.
type outsideAnswers struct {
	// mu serializes building cur; reads and spends need no lock. It is taken
	// under swapMu, never the other way round.
	mu  sync.Mutex
	cur atomic.Pointer[outsideState]
}

// outsideState is outsideAnswers for one staging.
type outsideState struct {
	epoch uint64
	// staged is, by name, every staged recording of the staging, read under
	// the staging lock: a consume is checked against it.
	staged map[string]*models.Mock
	// spent holds the names consumed: the first consume of one wins.
	spent sync.Map
}

// hidden reports whether name was consumed outside every test in the staging
// epoch is.
func (o *outsideAnswers) hidden(name string, epoch uint64) bool {
	st := o.cur.Load()
	if st == nil || st.epoch != epoch {
		return false
	}
	_, spent := st.spent.Load(name)
	return spent
}

// prepareOutside builds the outside answers of the current staging, once: the
// staged recordings by name, read with the staging lock held, so no staging
// is ever half read. It runs before any view's lock is taken.
func (m *MockManager) prepareOutside() {
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	o := &m.exact.outside
	o.mu.Lock()
	defer o.mu.Unlock()
	epoch := m.stagingEpoch.Load()
	if st := o.cur.Load(); st != nil && st.epoch == epoch {
		return
	}
	st := &outsideState{epoch: epoch, staged: map[string]*models.Mock{}}
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	for _, tree := range trees {
		for _, mk := range m.treeValues(tree, "") {
			if _, dup := st.staged[mk.Name]; !dup {
				st.staged[mk.Name] = mk
			}
		}
	}
	o.cur.Store(st)
}

// RestartExactKey starts key's calls outside every test afresh: a test
// process that claims key again in the session (a retried package,
// --rerun-fails) is a new scope, as a test begun again is. The recordings of
// names, key's pool slice, that calls outside every test consumed in the
// staging answer again, and key's stateful cursors (exactCursorKey) start
// over. No other key's are touched: pool slices name disjoint recordings.
func (m *MockManager) RestartExactKey(key string, names []string) {
	if st := m.exact.outside.cur.Load(); st != nil {
		for _, n := range names {
			st.spent.Delete(n)
		}
	}
	// A carry-over mock of the pool the app consumed as it started is the
	// retried package's to consume again.
	m.forgetBootTaken(names)
	prefix := key + "\x00"
	m.cursorMu.Lock()
	for k := range m.cursors {
		if strings.HasPrefix(k, prefix) {
			delete(m.cursors, k)
		}
	}
	m.cursorMu.Unlock()
	// Parsers that keep an index of what they read read again.
	m.bumpRevisionAll()
}

// consumeOutside settles a delete-door consume of a data-plane recording mk
// while no test is open (decision 31), once prepareOutside has run. The
// first consume of a recording in the staging is served and spends it. A
// recording another call consumed since it was read is refused, and so is
// one that is not the staging's own (read before the staging landed, under
// a name the staging stages too or none it stages) and any consume while a
// staging lands: the parser reads again. It takes no tree lock.
func (m *MockManager) consumeOutside(mk *models.Mock) exactVerdict {
	st := m.exact.outside.cur.Load()
	if st == nil || st.epoch != m.stagingEpoch.Load() {
		return exactRefused
	}
	staged := st.staged[mk.Name]
	if staged == nil || staged.Kind != mk.Kind || !staged.Spec.ReqTimestampMock.Equal(mk.Spec.ReqTimestampMock) ||
		!staged.Spec.ResTimestampMock.Equal(mk.Spec.ResTimestampMock) {
		return exactRefused
	}
	if _, taken := st.spent.LoadOrStore(mk.Name, true); taken {
		return exactRefused
	}
	return exactServed
}

// exactView returns the view exact serving shows, nil when it is off.
func (m *MockManager) exactView() *exactView { return m.exact.view.Load() }

// ExactServing reports whether the manager serves each test exactly its own
// mocks (KEPLOY_PROVEN, S12): every read shows one view, which changes as a
// test begins and ends, and a consume the view refuses returns false. A
// parser that keeps an index across reads (Postgres v3's tier engines) reads
// it at each dispatch: exact serving can begin after a connection opened.
func (m *MockManager) ExactServing() bool { return m.exactView() != nil }

var _ integrations.ExactServingReader = (*MockManager)(nil)

// FindStagedNotOwned is, under exact serving with a test open, the name of
// the first mock of kind staged in the manager's trees (per-test, session,
// startup, in that order) that the open test does not own and match accepts,
// whatever the open view shows (integrations.StagedFinder); "" for none. For a
// miss's diagnosis only: the mock is never handed out, nor served.
func (m *MockManager) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	v := m.exactView()
	if v == nil || v.test == "" || match == nil {
		return ""
	}
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	found := ""
	for _, t := range trees {
		if t == nil || found != "" {
			continue
		}
		t.rangeValues(func(val interface{}) bool {
			mk, ok := val.(*models.Mock)
			if !ok || mk == nil || mk.Kind != kind {
				return true
			}
			if _, own := v.index[mk.Name]; own {
				return true
			}
			if match(mk) {
				found = mk.Name
				return false
			}
			return true
		})
	}
	return found
}

// StagedMocks is every mock staged in the manager's trees (per-test, session,
// startup, in that order), whatever the open view shows: exact serving spends
// a served mock without taking it out of its tree, so a recording served or
// not is here (integrations.StagedReader). For the learn step's earlier
// places only: the mocks are never handed out, nor served.
func (m *MockManager) StagedMocks() []*models.Mock {
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	var out []*models.Mock
	for _, t := range trees {
		out = append(out, m.treeValues(t, "")...)
	}
	return out
}

var _ integrations.StagedReader = (*MockManager)(nil)

// ServeExactTest opens test under exact serving: from now its test mocks,
// owned (the data-plane mocks of its mapping, in recorded order), each answer
// at most one request, and the pool answers only protocol-phase requests.
// The manager serves copies of them; owned is not written. A test opened
// again (a retry) starts with all its test mocks unanswered, but for those the
// pool view just before it answered as the test's mocks in flight when it
// began (ServeExactPool's ahead): those count as answered, first, in the order
// they did.
func (m *MockManager) ServeExactTest(test string, owned []*models.Mock) {
	answered, ahead := m.closeAhead()
	v := &exactView{test: test, gen: m.exact.gen.Add(1), index: make(map[string]int, len(owned))}
	v.addOwned(owned)
	v.consumed = make([]bool, len(v.owned))
	for _, name := range ahead {
		if _, ok := v.index[name]; ok {
			if v.inFlight == nil {
				v.inFlight = map[string]bool{}
			}
			v.inFlight[name] = true
		}
	}
	for _, a := range answered {
		i, ok := v.index[a.owned.Name]
		if !ok || v.consumed[i] {
			continue
		}
		v.consumed[i] = true
		v.order = append(v.order, i)
		if v.carried == nil {
			v.carried = map[string]bool{}
		}
		v.carried[a.owned.Name] = true
	}
	m.exact.fired.Store(true)
	m.exact.last.Store(v)
	m.installExact(v)
	// Their consumes are flagged now, as this test's: the test before it was
	// drained since (the replay drains it after the pool call), and this
	// test's drain reads them. One this test does not own was answered for
	// a test that did not open next (one the replay could not open): it is
	// no consume of this test's, and is not flagged as one.
	for _, a := range answered {
		if v.carried[a.owned.Name] {
			m.noteExactServed(a.owned, a.served)
		} else if m.logger != nil {
			m.logger.Debug("a call answered in the gap as the next test's is not this test's, which opened instead",
				zap.String("mock", a.owned.Name), zap.String("test", test))
		}
	}
}

// aheadAnswer is a mock a pool view answered as the next test's (its ahead):
// the view's copy and the copy it answered with.
type aheadAnswer struct{ owned, served *models.Mock }

// closeAhead closes the view being served, if it is a pool view that answers
// the next test's mocks in flight when it began (ServeExactPool's ahead), and
// returns those that answered, in the order they did, and the names of all of
// them. Closed first, so no consume lands between the read and the next view.
func (m *MockManager) closeAhead() (answered []aheadAnswer, ahead []string) {
	prev := m.exact.view.Load()
	if prev == nil || prev.test != "" || len(prev.owned) == 0 {
		return nil, nil
	}
	prev.mu.Lock()
	defer prev.mu.Unlock()
	prev.closed = true
	answered = make([]aheadAnswer, 0, len(prev.order))
	for _, i := range prev.order {
		answered = append(answered, aheadAnswer{owned: prev.owned[i], served: prev.served[i]})
	}
	ahead = make([]string, 0, len(prev.owned))
	for _, mk := range prev.owned {
		ahead = append(ahead, mk.Name)
	}
	return answered, ahead
}

// addOwned files owned into v as the mocks it answers, each at most once:
// the manager's copies, in owned's order.
func (v *exactView) addOwned(owned []*models.Mock) {
	for _, mk := range owned {
		if mk == nil || mk.Name == "" {
			continue
		}
		if _, dup := v.index[mk.Name]; dup {
			continue
		}
		v.session = append(v.session, mk.TestModeInfo.Lifetime == models.LifetimeSession)
		c := mk.ShallowCopy()
		// Served as a per-test mock, consumed once, whatever lifetime its
		// kind derives: every parser consumes a per-test mock through one
		// of the doors exact serving holds.
		c.TestModeInfo.Lifetime = models.LifetimePerTest
		c.TestModeInfo.LifetimeDerived = true
		c.TestModeInfo.Consume = models.ConsumeReuse
		c.TestModeInfo.IsFiltered = true
		c.TestModeInfo.ID = len(v.owned)
		c.TestModeInfo.SortOrder = int64(len(v.owned)) + 1
		c.TestModeInfo.ServedIn = v.gen
		c.MarkPooled()
		v.index[c.Name] = len(v.owned)
		v.owned = append(v.owned, c)
	}
}

// ServeExactPool turns exact serving on, or keeps it on, with no test open:
// the pool slice names, of key, answers beside the protocol-phase mocks.
// ahead, when given, is the next test's mocks that were in flight when it
// began (models.ExactServing.Ahead): recorded in the gap before its window,
// answered once it had begun. They answer now too, each at most once, as that
// test's mocks: one answered so counts as answered when the test opens
// (ServeExactTest), and the replay judges it with the test.
//
// A pool view that replaces one which already answered some of its ahead
// keeps those it names too answered, and what they bound: no call is answered
// twice. One it does not name was answered for a test that did not open next
// (one the replay could not open): it is no consume of the next test's, and is
// not flagged as one (ServeExactTest).
func (m *MockManager) ServeExactPool(key string, names []string, ahead ...*models.Mock) {
	answered, _ := m.closeAhead()
	v := &exactView{poolKey: key, pool: make(map[string]struct{}, len(names))}
	for _, n := range names {
		v.pool[n] = struct{}{}
	}
	kept := map[string]bool{}
	if len(ahead) > 0 {
		v.gen = m.exact.gen.Add(1)
		v.index = make(map[string]int, len(ahead))
		v.addOwned(ahead)
		v.consumed = make([]bool, len(v.owned))
		v.served = make([]*models.Mock, len(v.owned))
		for _, a := range answered {
			if i, ok := v.index[a.owned.Name]; ok && !v.consumed[i] {
				v.consumed[i] = true
				v.order = append(v.order, i)
				v.served[i] = a.served
				kept[a.owned.Name] = true
			}
		}
	}
	// The scope the next test's calls in flight bind in (AheadVariables)
	// starts over with what the answers kept bound.
	var bound []models.VariableBinding
	for _, b := range m.aheadVars.Snapshot() {
		if kept[b.Mock] {
			bound = append(bound, b)
		}
	}
	m.aheadVars.StartCarrying(nil, bound)
	m.installExact(v)
	for _, a := range answered {
		if !kept[a.owned.Name] && m.logger != nil {
			m.logger.Debug("a call answered in the gap as the next test's is not the next test's: the one named did not open",
				zap.String("mock", a.owned.Name))
		}
	}
}

// installExact makes v the view, then tells every parser that keeps an index
// of what it read that what it may read changed: every kind's revision moves,
// and so does the window signal.
func (m *MockManager) installExact(v *exactView) {
	v.outside, v.epoch = &m.exact.outside, &m.stagingEpoch
	prev := m.exact.view.Swap(v)
	if prev != nil {
		prev.mu.Lock()
		prev.closed = true
		prev.mu.Unlock()
	}
	kinds := m.stagedKinds()
	for _, list := range [][]*models.Mock{v.owned, ownedOf(prev)} {
		for _, mk := range list {
			kinds[mk.Kind] = struct{}{}
		}
	}
	for k := range kinds {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
	m.signalWindowChange()
	if m.logger != nil {
		m.logger.Debug("exact serving", zap.String("test", v.test), zap.Int("test_mocks", len(v.owned)),
			zap.String("pool_key", v.poolKey), zap.Int("pool_mocks", len(v.pool)))
	}
}

func ownedOf(v *exactView) []*models.Mock {
	if v == nil {
		return nil
	}
	return v.owned
}

// stagedKinds is every kind the staged trees hold or have held.
func (m *MockManager) stagedKinds() map[models.Kind]struct{} {
	kinds := map[models.Kind]struct{}{}
	m.treesMu.RLock()
	for k := range m.filteredByKind {
		kinds[k] = struct{}{}
	}
	for k := range m.unfilteredByKind {
		kinds[k] = struct{}{}
	}
	startup := m.startup
	m.treesMu.RUnlock()
	if startup != nil {
		startup.rangeValues(func(v interface{}) bool {
			if mk, ok := v.(*models.Mock); ok && mk != nil {
				kinds[mk.Kind] = struct{}{}
			}
			return true
		})
	}
	m.revMu.RLock()
	for k := range m.revByKind {
		kinds[k] = struct{}{}
	}
	m.revMu.RUnlock()
	return kinds
}

// ExactTestOutcome is, under exact serving, the latest test opened, open or
// ended, and which of its test mocks have answered (in the order they did)
// and which have not (in recorded order). Read once the test stops being
// served, it is final: no consume reaches a view that is not served. test is
// "" when no test has been opened; ok is false when exact serving is off.
func (m *MockManager) ExactTestOutcome() (test string, consumed, unconsumed []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, nil, false
	}
	v := m.exact.last.Load()
	if v == nil {
		return "", nil, nil, true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, i := range v.order {
		consumed = append(consumed, v.owned[i].Name)
	}
	for i, done := range v.consumed {
		if !done {
			unconsumed = append(unconsumed, v.owned[i].Name)
		}
	}
	return v.test, consumed, unconsumed, true
}

// ConsumedTestMocks implements integrations.ExactConsumedReader: the open
// test's mocks that have answered, in the order they did.
func (m *MockManager) ConsumedTestMocks() []*models.Mock {
	v := m.exactView()
	if v == nil || v.test == "" {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(v.order))
	for _, i := range v.order {
		out = append(out, v.owned[i])
	}
	return out
}

var _ integrations.ExactConsumedReader = (*MockManager)(nil)

// exactMissKind is where a miss reported now was made: in the open test, or
// outside every test. "" when exact serving is off.
func (m *MockManager) exactMissKind() string {
	v := m.exactView()
	switch {
	case v == nil:
		return ""
	case v.test != "":
		return models.MissInTest
	default:
		return models.MissOutsideTest
	}
}

// unanswered is the view's test mocks that have not answered, of kind (every
// kind for ""), in recorded order; with sessionOnly, those staged in the
// session tier. A pool view's are the next test's mocks in flight when it
// began (ServeExactPool's ahead).
func (v *exactView) unanswered(kind models.Kind, sessionOnly ...bool) []*models.Mock {
	if len(v.owned) == 0 {
		return nil
	}
	only := len(sessionOnly) > 0 && sessionOnly[0]
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(v.owned))
	for i, mk := range v.owned {
		if !v.consumed[i] && (kind == "" || mk.Kind == kind) && (!only || v.session[i]) {
			out = append(out, mk)
		}
	}
	return out
}

// shows reports whether the view shows mk among the staged mocks: a
// protocol-phase mock always; a data-plane mock only between tests, and only
// in the pool slice served. The open test's own mocks are read from the view
// itself (unanswered), never from the staged trees.
func (v *exactView) shows(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	v.mu.Lock()
	spent := v.spent[mk.Name]
	v.mu.Unlock()
	if spent || v.spentOutside(mk.Name) {
		return false
	}
	if mk.IsProtocolPhase() {
		return true
	}
	if v.test != "" {
		return false
	}
	_, ok := v.pool[mk.Name]
	return ok
}

// spentOutside reports whether name, a data-plane recording, was consumed
// outside every test in the current staging (outsideAnswers).
func (v *exactView) spentOutside(name string) bool {
	return v.outside != nil && v.outside.hidden(name, v.epoch.Load())
}

// keep narrows staged mocks to those the view shows, in their order.
// unspent is in less the mocks a parser consumed through a delete door in
// this view, or outside every test in the staging; in itself when there are
// none.
func (v *exactView) unspent(in []*models.Mock) []*models.Mock {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(in) == 0 {
		return in
	}
	var out []*models.Mock
	for i, mk := range in {
		gone := mk != nil && (v.spent[mk.Name] || v.spentOutside(mk.Name))
		if gone && out == nil {
			out = append(make([]*models.Mock, 0, len(in)), in[:i]...)
		}
		if !gone && out != nil {
			out = append(out, mk)
		}
	}
	if out == nil {
		return in
	}
	return out
}

func (v *exactView) keep(in []*models.Mock) []*models.Mock {
	out := make([]*models.Mock, 0, len(in))
	for _, mk := range in {
		if v.shows(mk) {
			out = append(out, mk)
		}
	}
	return out
}

// perTest is a per-test read under exact serving: the open test's unanswered
// mocks of kind, then what the per-test tree holds that the view shows.
func (v *exactView) perTest(kind models.Kind, staged []*models.Mock) []*models.Mock {
	return append(v.unanswered(kind), v.keep(staged)...)
}

// reusable is a session-tier read under exact serving: the open test's
// unanswered mocks staged in that tier, then what the view shows of the
// session and the startup trees (each mock once). The reusable tiers show
// one set, whichever a parser reads: the staging filed the pool in both, a
// per-test-lifetime mock in the startup tree only, and a parser that reads
// one tier alone (enterprise SQS reads the session tier, Kafka never the
// startup tier) finds everything the view shows it.
func (v *exactView) reusable(kind models.Kind, staged ...[]*models.Mock) []*models.Mock {
	// Between tests a pool view's ahead (per-test mocks, in flight when the
	// next test began) is in every reusable read too: a tier-routing parser
	// reads only the reusable tiers while no test is open.
	out := v.unanswered(kind, v.test != "")
	seen := make(map[*models.Mock]struct{}, len(out))
	for _, list := range staged {
		for _, mk := range v.keep(list) {
			if _, dup := seen[mk]; dup || (kind != "" && mk.Kind != kind) {
				continue
			}
			seen[mk] = struct{}{}
			out = append(out, mk)
		}
	}
	return out
}

// exactVerdict is how exact serving settles a consume a parser reports.
type exactVerdict int

const (
	// exactPass: a mock the view shows from the staged trees (protocol-phase,
	// or the pool slice between tests): consumed through its tier, as
	// without exact serving.
	exactPass exactVerdict = iota
	// exactServed: one of the open test's mocks, answering for the first
	// time; it answers nothing more until the test begins again.
	exactServed
	// exactRefused: a test mock that already answered, or a mock the view
	// does not show now (a parser that read before a begin or an end).
	exactRefused
)

// exactConsume settles a parser's consume of mk under exact serving. evicts
// is a delete door (DeleteFilteredMock, DeleteUnFilteredMock,
// DeleteStartupMock): a staged mock the view shows is then spent, and the
// staged trees keep it. A data-plane recording consumed with no test open is
// spent for the staging (consumeOutside); any other, until the view changes.
func (m *MockManager) exactConsume(mk *models.Mock, evicts bool) exactVerdict {
	v := m.exactView()
	if v == nil || mk == nil {
		return exactPass
	}
	// The open test's mocks, or a pool view's ahead (the next test's mocks
	// in flight when it began): each answers once.
	if len(v.owned) > 0 {
		if i, ok := v.index[mk.Name]; ok {
			// A copy an earlier view served (a previous attempt's, read
			// before this begin) answers for nothing now. A mock a parser
			// built by name carries no view, and is the open test's.
			if g := mk.TestModeInfo.ServedIn; g != 0 && g != v.gen {
				return exactRefused
			}
			v.mu.Lock()
			if v.closed || v.consumed[i] {
				v.mu.Unlock()
				return exactRefused
			}
			v.consumed[i] = true
			v.order = append(v.order, i)
			owned := v.owned[i]
			if v.test == "" {
				// A pool view's ahead: its consume is flagged as the
				// next test opens (ServeExactTest), not now, so the
				// ended test's drain does not read it.
				v.served[i] = mk.ShallowCopy()
				v.mu.Unlock()
				m.bumpRevisionKind(owned.Kind)
				m.bumpRevisionAll()
				return exactServed
			}
			v.mu.Unlock()
			m.noteExactServed(owned, mk)
			return exactServed
		}
	}
	if !v.shows(mk) {
		return exactRefused
	}
	if !evicts {
		return exactPass
	}
	if v.test == "" && !mk.IsProtocolPhase() {
		m.prepareOutside()
		// The view's closed check and the spend are one step: no begin or
		// end lands between them.
		v.mu.Lock()
		verdict := exactRefused
		if !v.closed {
			verdict = m.consumeOutside(mk)
		}
		v.mu.Unlock()
		if verdict == exactServed {
			m.noteExactSpent(mk)
		}
		return verdict
	}
	v.mu.Lock()
	if v.closed || v.spent[mk.Name] {
		v.mu.Unlock()
		return exactRefused
	}
	if v.spent == nil {
		v.spent = map[string]bool{}
	}
	v.spent[mk.Name] = true
	v.mu.Unlock()
	m.noteExactSpent(mk)
	return exactServed
}

// exactConsumeValue is exactConsume for a door that takes the mock by value.
// Its copy is made only under exact serving: a door taking the mock's
// address would move every consume's mock to the heap, switch on or off.
func (m *MockManager) exactConsumeValue(mock models.Mock, evicts bool) exactVerdict {
	if m.exactView() == nil {
		return exactPass
	}
	mk := mock
	return m.exactConsume(&mk, evicts)
}

// dataPlaneTyped is MockState.DataPlane for mk: a test mock whose type tag
// says protocol-phase (a Generic exchange its recorder stated per-test).
func dataPlaneTyped(mk *models.Mock) bool {
	switch mk.Spec.Metadata["type"] {
	case "config", "connection":
		return !mk.IsProtocolPhase()
	}
	return false
}

// noteExactSpent records that a staged mock answered through a delete door,
// and tells the parsers that keep an index that it is gone: until the view
// changes, or for a data-plane recording consumed outside every test, until
// the next staging.
func (m *MockManager) noteExactSpent(mk *models.Mock) {
	if err := m.flagMockAsUsed(models.MockState{
		Name:             mk.Name,
		Kind:             mk.Kind,
		Usage:            models.Updated,
		IsFiltered:       mk.TestModeInfo.IsFiltered,
		SortOrder:        mk.TestModeInfo.SortOrder,
		Type:             mk.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(mk),
		Lifetime:         mk.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
		ReqBodyNoise:     reqBodyNoiseOf(mk.Spec),
		Variables:        variablesOf(mk.Spec),
		Binds:            bindsOf(mk.Spec),
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	m.bumpRevisionKind(mk.Kind)
	m.bumpRevisionAll()
}

// noteExactServed records that test mock owned answered (consumed, served
// once) and tells the parsers that keep an index that it is gone. served is
// the copy the parser reported, which carries the request-body noise it
// detected, if any, and under the proven switch what record's full round
// learned onto it (its variables and the binds it carries onto earlier
// calls), as the other doors' states do.
func (m *MockManager) noteExactServed(owned, served *models.Mock) {
	if err := m.flagMockAsUsed(models.MockState{
		Name:             owned.Name,
		Kind:             owned.Kind,
		Usage:            models.Deleted,
		IsFiltered:       true,
		SortOrder:        owned.TestModeInfo.SortOrder,
		Type:             owned.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(owned),
		Lifetime:         owned.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(owned.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(owned.Spec.ResTimestampMock),
		ReqBodyNoise:     reqBodyNoiseOf(served.Spec),
		Variables:        variablesOf(served.Spec),
		Binds:            bindsOf(served.Spec),
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	m.bumpRevisionKind(owned.Kind)
	m.bumpRevisionAll()
}

// exactCursorKey is the stateful cursor a key names under exact serving: the
// served pool slice's own, so two keys' calls never share a sequence. In a
// test no cursor is used (ok false): the test's mocks answer once each, in
// recorded order, and the pool serves no data-plane mock there.
func (m *MockManager) exactCursorKey(key string) (string, bool) {
	v := m.exactView()
	if v == nil {
		return key, true
	}
	if v.test != "" {
		return "", false
	}
	return v.poolKey + "\x00" + key, true
}

// startupTree and unfilteredTree are the staged reusable trees now.
func (m *MockManager) startupTree() *TreeDb {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	return m.startup
}

func (m *MockManager) unfilteredTree() *TreeDb {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	return m.unfiltered
}

// treeValues is what tree holds, of kind (every kind for ""), in order.
func (m *MockManager) treeValues(tree *TreeDb, kind models.Kind) []*models.Mock {
	if tree == nil {
		return nil
	}
	out := make([]*models.Mock, 0, 32)
	tree.rangeValues(func(v interface{}) bool {
		if mk, ok := v.(*models.Mock); ok && mk != nil && (kind == "" || mk.Kind == kind) {
			out = append(out, mk)
		}
		return true
	})
	return out
}
