package proxy

import (
	"slices"
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// SERVED, UNCHECKED: A SET-ASIDE TEST'S RECORDINGS, TO THE CALLS AFTER IT.
//
// A test the replay sets aside (the proven gate skips it) makes none of its
// calls, but what its calls did can still be needed: a cache it filled at
// record is empty at replay, and the next test that reads the cache makes the
// set-aside test's call in its own window, where no recording of its own
// answers it. Under exact serving such a call is served the set-aside test's
// recording (served, never required; a data-plane call is
// answered by the recording it is, wherever the replay's timing puts it): by
// exact match only, unchecked and never required of the test it lands in, at
// most once in the staging, and named on that test's evidence ("<mock>
// served from <test> (set aside)", models.ScopeEvidence.ServedAside). A
// recording that places or binds variables is not served aside: its values
// flow from or into its own test's other calls, which were never made. A
// recording whose request holds a read of the application's clock the learn
// step learned (MockSpec.Clocks) is served aside, checked: that value comes
// from the clock at the call, not from another call, so the parser checks it
// against the later call's own window, as in its own test (HTTP's
// clockDiffs; Mongo's sameMongoRequest), and a call whose read is not one
// made in its window is not that recording.
//
// The set-aside recordings are in none of the view's reads: a parser asks
// for them (integrations.SetAsideReader) only for a call that no recording
// it reads answered, matches the call exactly against them alone, and
// consumes the one it matched through the store's door. That includes a call
// one more than the open test recorded: a set-aside test's per-process
// first-use refresh (a read, the same read again, a write) lands in the
// window of the next test that uses it, beside that test's own copy of the
// read. Each set-aside recording answers once in the staging, so the calls of
// one shape across the set never exceed what the set recorded; past every
// exact copy left the call stays an extra call (never a
// recording served again, nor one the call is not). A parser that does not
// ask serves none, as before.
//
// keploy mock replay also hands aside the recordings of a test it has not
// begun when a test recorded after it begins (a run that does not select
// it). Should that test begin later, its own copy of each is the same one
// answer (exactView.handed): it answers the test only if no test was
// served it aside, and once it has, it is offered aside to no later test.
// That holds for a call in flight before the test's begin too (a pool view's
// ahead): the claim is taken as the copy answers, under the view's lock, so
// one answer of an in-flight call and another test's aside never both land.

// THE HAND-ASIDE HOLDS NAMES, NOT RECORDINGS (founder rule no-oom-components).
// A partial replay hands, at its first begin, every earlier test it has not
// run: thousands of tests. The manager keeps, per handed test, the agent's
// own resident copies (its store holds them anyway) and only the names of
// those the agent keeps on disk, and no view files any of them: a call that
// no recording answered asks for them by its request keys (RangeSetAside),
// one handed test at a time, in hand order, and only the recordings of the
// keys it asks by are decoded, and only for that ask. What the hand-aside
// holds is O(recordings handed) names; what a miss holds is one handed
// test's recordings under its keys. A copy a call is served is filed in the
// view as it answers (exactView.asideServed), so a view holds only the
// set-aside recordings its calls were served.

// asideIndex is what the replay handed aside in the session
// (ServeExactAside, ServeExactAsideOnDisk): the tests in hand order, and
// which test each handed recording is, and whether it is read from disk.
type asideIndex struct {
	mu     sync.RWMutex
	tests  []*asideTest
	byFrom map[string]int
	names  map[string]asideName
}

// asideTest is one handed test: the recordings the agent handed resident,
// in its order, and keep, which a recording read from disk must pass (one
// whose destination keploy now reaches for real is never served). Never
// changed once filed: a later hand of the same test files a new one.
type asideTest struct {
	from     string
	resident []*models.Mock
	onDisk   bool
	keep     func(*models.Mock) bool
}

// asideName is a handed recording: its test, and whether it is on disk.
type asideName struct {
	from   string
	onDisk bool
}

// hand files from's resident recordings and the names of its recordings on
// disk; a recording already handed stays its first test's.
func (x *asideIndex) hand(from string, resident []*models.Mock, onDisk []string, keep func(*models.Mock) bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.names == nil {
		x.names, x.byFrom = map[string]asideName{}, map[string]int{}
	}
	t := &asideTest{from: from, keep: keep}
	i, again := x.byFrom[from]
	if again {
		prev := x.tests[i]
		t.resident, t.onDisk = slices.Clone(prev.resident), prev.onDisk
		if t.keep == nil {
			t.keep = prev.keep
		}
	}
	for _, mk := range resident {
		if mk == nil || mk.Name == "" {
			continue
		}
		if _, dup := x.names[mk.Name]; dup {
			continue
		}
		x.names[mk.Name] = asideName{from: from}
		t.resident = append(t.resident, mk)
	}
	for _, n := range onDisk {
		if _, dup := x.names[n]; dup || n == "" {
			continue
		}
		x.names[n] = asideName{from: from, onDisk: true}
		t.onDisk = true
	}
	if again {
		x.tests[i] = t
		return
	}
	x.byFrom[from] = len(x.tests)
	x.tests = append(x.tests, t)
}

// snapshot is the handed tests in hand order; each is never changed.
func (x *asideIndex) snapshot() []*asideTest {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return slices.Clone(x.tests)
}

// of is the handed recording name, if it is one.
func (x *asideIndex) of(name string) (asideName, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	n, ok := x.names[name]
	return n, ok
}

// reset forgets every hand (a session reset).
func (x *asideIndex) reset() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.tests, x.byFrom, x.names = nil, nil, nil
}

// ServeExactAside adds the recordings of test from, which the replay set
// aside (skipped) or has not begun, to those served to the tests after it,
// cumulative over the session, as the package comment says. mocks are the
// agent's own copies, which its store keeps resident: the manager holds
// them, no copy. It installs no view: a view reads them only as a call asks
// (RangeSetAside). A session reset forgets them.
func (m *MockManager) ServeExactAside(from string, mocks []*models.Mock) {
	if from == "" || len(mocks) == 0 {
		return
	}
	m.exact.aside.hand(from, mocks, nil, nil)
}

// ServeExactAsideOnDisk is ServeExactAside for the recordings of from the
// agent keeps on disk, by name: none is read until a call asks by a request
// key it is filed under (RangeSetAside), through the recording source the
// agent set for the staged set (SetRecordingSource); keep, when not nil, is
// what one read must pass to be served.
func (m *MockManager) ServeExactAsideOnDisk(from string, names []string, keep func(*models.Mock) bool) {
	if from == "" || len(names) == 0 {
		return
	}
	m.exact.aside.hand(from, nil, names, keep)
}

// markHanded marks the view's recordings the replay handed aside before
// their test began, each with its test (exactView.handed): on a test view
// its own, on a pool view its ahead's (the next test's calls in flight
// before its begin), whichever test the hand says. Called once v holds
// every one of its own, before any aside.
func (v *exactView) markHanded(x *asideIndex) {
	for i, mk := range v.owned {
		h, ok := x.of(mk.Name)
		if !ok || (v.test != "" && h.from != v.test) {
			continue
		}
		if v.handed == nil {
			v.handed = make([]string, len(v.owned))
		}
		v.handed[i] = h.from
	}
}

// servableAside reports whether a handed recording may answer a later
// test's call: never one that places or binds variables, whose values flow
// from or into its own test's other calls, nor a protocol-phase one.
func servableAside(mk *models.Mock) bool {
	return mk != nil && !mk.IsProtocolPhase() && len(variablesOf(mk.Spec)) == 0 && len(bindsOf(mk.Spec)) == 0
}

// asideLoad is a handed recording on disk under a call's keys, not read yet.
type asideLoad struct {
	name string
	load func() (*models.Mock, error)
}

// RangeSetAside implements integrations.SetAsideReader: it visits the open
// test's set-aside recordings of kind (every kind for "", of the resident
// ones), one handed test at a time in hand
// order, each test's in recorded order, until visit returns false. Each is
// a copy filed for the open view (models.TestModeInfo.SetAsideFrom names its
// test) that no test answered a call with in the staging and that is no
// recording of the view's own: one it holds stays its own, as it is. Of the
// recordings the agent keeps on disk only those filed under one of keys
// (models.RequestKeys) are read, each test's as its turn comes, and none is
// kept: a kind whose parser declares no keys, or a call that asks by none,
// is offered the resident ones alone. None while no test is open.
func (m *MockManager) RangeSetAside(kind models.Kind, keys []string, visit func([]*models.Mock) bool) {
	v := m.exactView()
	if v == nil || v.test == "" || visit == nil {
		return
	}
	tests := m.exact.aside.snapshot()
	if len(tests) == 0 {
		return
	}
	epoch := m.stagingEpoch.Load()
	offered := func(name string) bool {
		if _, own := v.index[name]; own {
			return false
		}
		_, claimed := m.exact.asideClaims.claimant(epoch, name)
		return !claimed
	}
	var disk map[string][]asideLoad
	if len(keys) > 0 && slices.ContainsFunc(tests, func(t *asideTest) bool { return t.onDisk }) {
		m.rangeRecorded(kind, keys, func(name string, load func() (*models.Mock, error)) bool {
			if h, ok := m.exact.aside.of(name); ok && h.onDisk && offered(name) {
				if disk == nil {
					disk = map[string][]asideLoad{}
				}
				disk[h.from] = append(disk[h.from], asideLoad{name: name, load: load})
			}
			return true
		})
	}
	n := 0
	for _, t := range tests {
		var batch []*models.Mock
		for _, mk := range t.resident {
			if (kind == "" || mk.Kind == kind) && offered(mk.Name) && servableAside(mk) {
				batch = append(batch, mk)
			}
		}
		for _, l := range disk[t.from] {
			mk, err := l.load()
			if err != nil {
				if m.logger != nil {
					m.logger.Debug("a set-aside test's recording kept on disk could not be read; it answers no later call", zap.String("mock", l.name), zap.String("test", t.from), zap.Error(err))
				}
				continue
			}
			if mk == nil || mk.Name != l.name || mk.Kind != kind || !servableAside(mk) || (t.keep != nil && !t.keep(mk)) {
				continue
			}
			batch = append(batch, mk)
		}
		if len(batch) == 0 {
			continue
		}
		inRecordedOrder(batch)
		for i, mk := range batch {
			batch[i] = v.asideCopy(mk, t.from, n)
			n++
		}
		if !visit(batch) {
			return
		}
	}
}

// inRecordedOrder sorts mocks by request time, those with none first, as
// the agent orders a test's recordings.
func inRecordedOrder(mocks []*models.Mock) {
	sort.SliceStable(mocks, func(i, j int) bool {
		ti, tj := mocks[i].Spec.ReqTimestampMock, mocks[j].Spec.ReqTimestampMock
		if ti.IsZero() || tj.IsZero() {
			return ti.IsZero() && !tj.IsZero()
		}
		return ti.Before(tj)
	})
}

// asideCopy is the copy of from's recording mk a call of v is offered, as
// v.add files its own (consumed once, whatever lifetime its kind derives),
// marked with its test and v's generation; n numbers it among those offered,
// past v's own positions.
func (v *exactView) asideCopy(mk *models.Mock, from string, n int) *models.Mock {
	c := mk.ShallowCopy()
	c.TestModeInfo.Lifetime = models.LifetimePerTest
	c.TestModeInfo.LifetimeDerived = true
	c.TestModeInfo.Consume = models.ConsumeReuse
	c.TestModeInfo.IsFiltered = true
	c.TestModeInfo.ID = len(v.owned) + n
	c.TestModeInfo.SortOrder = int64(len(v.owned)+n) + 1
	c.TestModeInfo.ServedIn = v.gen
	c.TestModeInfo.Concurrent = false
	c.TestModeInfo.SetAsideFrom = from
	c.MarkPooled()
	return c
}

// asideServe is a set-aside recording a call of the view was served: its
// test, and the copy that answered.
type asideServe struct {
	from string
	mock *models.Mock
}

// consumeAsideIn is exactConsumeIn for a copy RangeSetAside offered the
// open test: the first consume of it in the staging claims it for the test
// (asideClaims) and files it in the view as answered (asideServed, read by
// the outcome and the round as v.order's positions past v.owned), in one
// step under the view's lock; a copy of another view, of no handed
// recording, or one another test answered with, is refused.
func (m *MockManager) consumeAsideIn(v *exactView, mk *models.Mock, from string) (exactVerdict, <-chan struct{}, func()) {
	if mk.TestModeInfo.ServedIn != v.gen {
		return exactRefused, nil, nil
	}
	if h, ok := m.exact.aside.of(mk.Name); !ok || h.from != from {
		return exactRefused, nil, nil
	}
	v.mu.Lock()
	if wait := v.closedWait(); wait != nil {
		v.mu.Unlock()
		return exactRefused, wait, nil
	}
	for _, s := range v.asideServed {
		if s.mock.Name == mk.Name {
			v.mu.Unlock()
			return exactRefused, nil, nil
		}
	}
	if !m.exact.asideClaims.claim(m.stagingEpoch.Load(), mk.Name, v.test) {
		v.mu.Unlock()
		return exactRefused, nil, nil
	}
	v.order = append(v.order, len(v.owned)+len(v.asideServed))
	v.asideServed = append(v.asideServed, asideServe{from: from, mock: mk})
	v.mu.Unlock()
	m.takeLocked(integrations.Take{Mock: mk, How: integrations.TakeConsumed, Gen: v.round, Test: v.test})
	return exactServed, nil, func() { m.noteExactServed(mk, mk) }
}

var _ integrations.SetAsideReader = (*MockManager)(nil)

// ExactTestServedAside is, under exact serving, the latest test opened (as
// ExactTestOutcome names it) and the calls of its window a set-aside test's
// recording answered, in the order they did, each as
// models.ServedAsideLine names it. Read once the test stops being served, it
// is final. ok is false when exact serving is off.
func (m *MockManager) ExactTestServedAside() (test string, served []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, false
	}
	o := m.latestOutcome()
	if o == nil {
		return "", nil, true
	}
	return o.test, append([]string(nil), o.servedAside...), true
}
