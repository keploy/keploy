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
	// owned are the open test's test mocks, the manager's own copies, in
	// recorded order; index is each one's position, by name.
	owned []*models.Mock
	index map[string]int
	// pool is, while no test is open, the names of the pool slice served.
	pool map[string]struct{}
	// poolKey is the key of that slice: its stateful cursors are its own.
	poolKey string

	mu sync.Mutex
	// consumed says which owned mock has answered; order lists their
	// positions in the order they did.
	consumed []bool
	order    []int
}

// exactState is a manager's exact serving: off until the agent turns it on
// for a replay session (view nil), off again at the next one.
type exactState struct {
	view atomic.Pointer[exactView]
	// fired: a test has begun since exact serving was turned on. It is what
	// tells a tier-routing parser (Postgres v3) "between tests" from
	// "before the first test".
	fired atomic.Bool
}

// exactView returns the view exact serving shows, nil when it is off.
func (m *MockManager) exactView() *exactView { return m.exact.view.Load() }

// ServeExactTest opens test under exact serving: from now its test mocks,
// owned (the data-plane mocks of its mapping, in recorded order), each answer
// at most one request, and the pool answers only protocol-phase requests.
// The manager serves copies of them; owned is not written. A test opened
// again (a retry) starts with all its test mocks unanswered.
func (m *MockManager) ServeExactTest(test string, owned []*models.Mock) {
	v := &exactView{test: test, index: make(map[string]int, len(owned))}
	for _, mk := range owned {
		if mk == nil || mk.Name == "" {
			continue
		}
		if _, dup := v.index[mk.Name]; dup {
			continue
		}
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
		c.MarkPooled()
		v.index[c.Name] = len(v.owned)
		v.owned = append(v.owned, c)
	}
	v.consumed = make([]bool, len(v.owned))
	m.exact.fired.Store(true)
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

// ExactTestOutcome is, under exact serving, the open test and which of its
// test mocks have answered (in the order they did) and which have not (in
// recorded order). test is "" when no test is open; ok is false when exact
// serving is off.
func (m *MockManager) ExactTestOutcome() (test string, consumed, unconsumed []string, ok bool) {
	v := m.exactView()
	if v == nil {
		return "", nil, nil, false
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
// kind for ""), in recorded order.
func (v *exactView) unanswered(kind models.Kind) []*models.Mock {
	if v.test == "" || len(v.owned) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(v.owned))
	for i, mk := range v.owned {
		if !v.consumed[i] && (kind == "" || mk.Kind == kind) {
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

// exactConsume settles a parser's consume of mk under exact serving.
func (m *MockManager) exactConsume(mk *models.Mock) exactVerdict {
	v := m.exactView()
	if v == nil || mk == nil {
		return exactPass
	}
	if v.test != "" {
		if i, ok := v.index[mk.Name]; ok {
			v.mu.Lock()
			if v.consumed[i] {
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
	if v.shows(mk) {
		return exactPass
	}
	return exactRefused
}

// noteExactServed records that test mock owned answered (consumed, served
// once) and tells the parsers that keep an index that it is gone. served is
// the copy the parser reported, which carries the request-body noise it
// detected, if any.
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
