package proxy

import (
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
	// through a delete door (a per-test-lifetime pool mock, a protocol-phase
	// mock a parser evicts): gone until the view changes at the next begin
	// or end, as when each begin and end staged the set again. The staged
	// trees keep them.
	spent map[string]bool
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
}

// exactView returns the view exact serving shows, nil when it is off.
func (m *MockManager) exactView() *exactView { return m.exact.view.Load() }

// ExactServing reports whether the manager serves each test exactly its own
// mocks (KEPLOY_PROVEN, S12): every read shows one view, which changes as a
// test begins and ends, and a consume the view refuses returns false. A
// parser that keeps an index across reads (Postgres v3's tier engines) reads
// it at each dispatch: exact serving can begin after a connection opened.
func (m *MockManager) ExactServing() bool { return m.exactView() != nil }

// ServeExactTest opens test under exact serving: from now its test mocks,
// owned (the data-plane mocks of its mapping, in recorded order), each answer
// at most one request, and the pool answers only protocol-phase requests.
// The manager serves copies of them; owned is not written. A test opened
// again (a retry) starts with all its test mocks unanswered.
func (m *MockManager) ServeExactTest(test string, owned []*models.Mock) {
	v := &exactView{test: test, gen: m.exact.gen.Add(1), index: make(map[string]int, len(owned))}
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
	v.consumed = make([]bool, len(v.owned))
	m.exact.fired.Store(true)
	m.exact.last.Store(v)
	m.installExact(v)
}

// ServeExactPool turns exact serving on, or keeps it on, with no test open:
// the pool slice names, of key, answers beside the protocol-phase mocks.
func (m *MockManager) ServeExactPool(key string, names []string) {
	v := &exactView{poolKey: key, pool: make(map[string]struct{}, len(names))}
	for _, n := range names {
		v.pool[n] = struct{}{}
	}
	m.installExact(v)
}

// installExact makes v the view, then tells every parser that keeps an index
// of what it read that what it may read changed: every kind's revision moves,
// and so does the window signal.
func (m *MockManager) installExact(v *exactView) {
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
// session tier.
func (v *exactView) unanswered(kind models.Kind, sessionOnly ...bool) []*models.Mock {
	if v.test == "" || len(v.owned) == 0 {
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
	if spent {
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

// keep narrows staged mocks to those the view shows, in their order.
// unspent is in less the mocks a parser consumed through a delete door in
// this view; in itself when there are none.
func (v *exactView) unspent(in []*models.Mock) []*models.Mock {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.spent) == 0 || len(in) == 0 {
		return in
	}
	out := make([]*models.Mock, 0, len(in))
	for _, mk := range in {
		if mk != nil && !v.spent[mk.Name] {
			out = append(out, mk)
		}
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
	out := v.unanswered(kind, true)
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
// DeleteStartupMock): a staged mock the view shows is then spent until the
// view changes, and the staged trees keep it.
func (m *MockManager) exactConsume(mk *models.Mock, evicts bool) exactVerdict {
	v := m.exactView()
	if v == nil || mk == nil {
		return exactPass
	}
	if v.test != "" {
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

// noteExactSpent records that a staged mock answered through a delete door,
// and tells the parsers that keep an index that it is gone until the view
// changes.
func (m *MockManager) noteExactSpent(mk *models.Mock) {
	if err := m.flagMockAsUsed(models.MockState{
		Name:             mk.Name,
		Kind:             mk.Kind,
		Usage:            models.Updated,
		IsFiltered:       mk.TestModeInfo.IsFiltered,
		SortOrder:        mk.TestModeInfo.SortOrder,
		Type:             mk.Spec.Metadata["type"],
		Lifetime:         mk.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
		ReqBodyNoise:     reqBodyNoiseOf(mk.Spec),
		Variables:        variablesOf(mk.Spec),
		Binds:            bindsOf(mk.Spec),
	}); err != nil && m.logger != nil {
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
		Lifetime:         owned.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(owned.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(owned.Spec.ResTimestampMock),
		ReqBodyNoise:     reqBodyNoiseOf(served.Spec),
		Variables:        variablesOf(served.Spec),
		Binds:            bindsOf(served.Spec),
	}); err != nil && m.logger != nil {
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
