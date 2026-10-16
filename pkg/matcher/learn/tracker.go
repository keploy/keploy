package learn

import (
	"context"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// Tracker keeps, for the test that is running, the mocks F has served in it
// (the earlier places Classify looks in) and the names of the variables
// learned in it. The agent owns one; it begins a new test whenever the replay
// opens the next test's window, and the parsers find it on their
// connection's context.
type Tracker struct {
	mu     sync.Mutex
	epoch  uint64
	served []Earlier
	mocks  []*models.Mock
	// before are the mocks served before the running test began: in the
	// previous test, between tests, or while the next test's mocks were
	// being put in place. The test's boundary and the mock store's do not
	// move at one moment, so a call near it may be either test's, and its
	// earlier places may be there.
	before []*models.Mock
	// outside are the mocks served while no test was open (the start-up,
	// between tests), by every later test: each is an earlier place of every
	// test that follows, not of the next one alone. The exact view shows a
	// data-plane mock of the pool only while no test is open, so a value the
	// app sent in a start-up call is found here in the second test on, and
	// refused there as in the first (only a test's own calls carry its
	// variables), never learned as noise. outsideKept are their identities
	// (mockID). They are the running set's: NewSet forgets them.
	outside     []*models.Mock
	outsideKept map[string]bool
	// kept are the identities (mockID) in mocks: a mock served again is kept
	// once.
	kept map[string]bool
	// answered are the test's own mocks that answered a request of the
	// running test, by name: each answers one (design §5.2), whatever
	// lifetime the store gives it. They are kept, as they were recorded, to
	// compete still once the store let them go (a per-test mock is deleted
	// when consumed).
	answered map[string]*models.Mock
	// drifts keeps what each value drifted, for the running test's
	// requests.
	drifts *DriftCache
	open   bool
	// names are the running test's variables, by where each is bound (its
	// mock and place), and taken the names given.
	names map[string]string
	taken map[string]bool
	// run are the exchanges the learn step served before the first test
	// began (the start-up's) that carried a value the run made (one that
	// drifted from its recording): an earlier place of every test of the set
	// (Run). runNames are the variables bound at them, by mock and place, and
	// runTaken their names, which no test's variable takes. setTaken are the
	// names every test of the set has taken so far, which no run's variable
	// takes: a run's variable is named when a test first uses it, after
	// earlier tests named their own.
	run      []Earlier
	runNames map[string]string
	runTaken map[string]bool
	setTaken map[string]bool
}

// NewTracker makes a tracker with no test open.
func NewTracker() *Tracker {
	return &Tracker{}
}

// NewSet starts the next test set's replay on the same agent: what was served
// before it (the previous set's tests, its start-up, between its tests) is no
// earlier place of the new set's, and no test is open until the next Begin.
// Mock names restart in every set (mock-0 again), so a mock served in one set
// must not stand for, or hide, another set's mock of its name.
func (t *Tracker) NewSet() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.mocks, t.kept, t.before = nil, nil, nil
	t.outside, t.outsideKept = nil, nil
	t.run, t.runNames, t.runTaken, t.setTaken = nil, nil, nil, nil
	t.open = false
	t.mu.Unlock()
}

// mockID is a served mock's identity: its name, which is unique within one
// set's recording only, with its kind and recorded times.
func mockID(m *models.Mock) string {
	return m.Name + "\x00" + string(m.Kind) + "\x00" + strconv.FormatInt(m.Spec.ReqTimestampMock.UnixNano(), 10) +
		"\x00" + strconv.FormatInt(m.Spec.ResTimestampMock.UnixNano(), 10)
}

// Begin starts the next test: nothing is earlier than its first call.
func (t *Tracker) Begin() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.epoch++
	t.served = nil
	t.before, t.mocks, t.kept = t.mocks, nil, nil
	t.answered = nil
	t.drifts = NewDriftCache()
	t.names, t.taken = nil, nil
	t.open = true
	t.mu.Unlock()
}

// End ends the running test: until the next Begin no test is open, and a
// request belongs to no test's own mocks.
func (t *Tracker) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.open = false
	t.mu.Unlock()
}

// Open reports whether a test is open. A nil tracker, which knows nothing of
// tests, answers true.
func (t *Tracker) Open() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open
}

// Test is the running test's number, what the learn step served in it so
// far, leaf by leaf, every mock any parser served in it so far
// (ServedMock), and the mocks served before it began: those served since the
// previous test began (see Tracker.before), and every one served while no
// test was open (see Tracker.outside), each once. A request reads it once
// its scope is read, and hands the number back to Served.
func (t *Tracker) Test() (uint64, []Earlier, []*models.Mock, []*models.Mock) {
	if t == nil {
		return 0, nil, nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := append(make([]*models.Mock, 0, len(t.before)+len(t.outside)), t.before...)
	seen := make(map[string]bool, len(t.before))
	for _, m := range t.before {
		seen[mockID(m)] = true
	}
	for _, m := range t.outside {
		if !seen[mockID(m)] {
			before = append(before, m)
		}
	}
	earlier := append(append(make([]Earlier, 0, len(t.run)+len(t.served)), t.run...), t.served...)
	return t.epoch, earlier, append([]*models.Mock(nil), t.mocks...), before
}

// ServedMock notes a mock any parser served in the running test: an earlier
// place of the test's recording, whatever its kind, for Classify (Raw).
func (t *Tracker) ServedMock(m *models.Mock) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	id := mockID(m)
	// Kept between tests too: the next Begin moves it to before.
	if !t.kept[id] {
		if t.kept == nil {
			t.kept = map[string]bool{}
		}
		t.kept[id] = true
		t.mocks = append(t.mocks, m)
	}
	// Served while no test is open: an earlier place of every later test of
	// the set.
	if !t.open && !t.outsideKept[id] {
		if t.outsideKept == nil {
			t.outsideKept = map[string]bool{}
		}
		t.outsideKept[id] = true
		t.outside = append(t.outside, m)
	}
	t.mu.Unlock()
}

// Drifts is what each value drifted, kept for the running test (Classify),
// nil (a cache of one request's) when no test began.
func (t *Tracker) Drifts() *DriftCache {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drifts
}

// Answered reports whether the running test's own mock name has answered a
// request of the test: it answers no other. A nil tracker, which knows of no
// test, answers false.
func (t *Tracker) Answered(name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.answered[name] != nil
}

// Answer takes the test's own mock m to answer a request of test number n:
// false when it answered one already, or that test has ended, and the
// request looks again. Two requests never take one mock. A nil tracker takes
// every mock.
func (t *Tracker) Answer(n uint64, m *models.Mock) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != n || t.answered[m.Name] != nil {
		return false
	}
	if t.answered == nil {
		t.answered = map[string]*models.Mock{}
	}
	t.answered[m.Name] = m
	return true
}

// AnsweredMocks are the running test's own mocks that answered a request of
// it, in no order.
func (t *Tracker) AnsweredMocks() []*models.Mock {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*models.Mock, 0, len(t.answered))
	for _, m := range t.answered {
		out = append(out, m)
	}
	return out
}

// Unanswer gives back a mock Answer took for a request that was not answered
// from it after all.
func (t *Tracker) Unanswer(n uint64, name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.epoch == n {
		delete(t.answered, name)
	}
	t.mu.Unlock()
}

// Served adds a mock F served to test number n's history, until the next test
// begins: a request of that test answered after it ended, or between it and
// the next, lands there too, and the next Begin forgets it. Served before the
// first test began (the start-up's), it is the run's when it carried a value
// the run made (Earlier.Run): an earlier place of every test of the set.
func (t *Tracker) Served(n uint64, e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	switch {
	case !t.open && t.epoch == 0 && drifted(e):
		e.Run = true
		t.run = append(t.run, e)
	case t.epoch == n:
		t.served = append(t.served, e)
	}
	t.mu.Unlock()
}

// drifted reports whether an exchange's request carried a value other than
// its recording's at some place a variable can hold.
func drifted(e Earlier) bool {
	for _, s := range e.Seen {
		if s.Side == Request && s.Place != "" && s.Live != "" && s.Live != s.Leaf.Value {
			return true
		}
	}
	return false
}

// Name is the name of test number n's variable bound at place of mock: the
// same for every call that uses it, no other variable's in the test
// (variables are named per test), and none of reserved (the test case's
// template keys). ok is false when that test has ended. A nil tracker,
// which knows of no test, names a variable by its place alone.
func (t *Tracker) Name(n uint64, mock, place string, reserved map[string]bool) (name string, ok bool) {
	if t == nil {
		name = varName(place)
		for i := 2; reserved[name]; i++ {
			name = varName(place) + "-" + strconv.Itoa(i)
		}
		return name, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := mock + "\x00" + place
	if name, ok := t.runNames[key]; ok {
		return name, true
	}
	if t.epoch != n {
		return "", false
	}
	if name, ok := t.names[key]; ok {
		return name, true
	}
	if t.names == nil {
		t.names, t.taken = map[string]string{}, map[string]bool{}
	}
	if t.setTaken == nil {
		t.setTaken = map[string]bool{}
	}
	name = t.free(place, reserved, t.taken)
	t.names[key], t.taken[name], t.setTaken[name] = name, true, true
	return name, true
}

// RunName names the variable bound at place of mock, an exchange of the run
// (Earlier.Run): one name for the whole set, which no test's variable takes,
// and none a test of the set took before it.
func (t *Tracker) RunName(mock, place string, reserved map[string]bool) string {
	if t == nil {
		name, _ := t.Name(0, mock, place, reserved)
		return name
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := mock + "\x00" + place
	if name, ok := t.runNames[key]; ok {
		return name
	}
	if t.runNames == nil {
		t.runNames, t.runTaken = map[string]string{}, map[string]bool{}
	}
	name := t.free(place, reserved, t.setTaken)
	t.runNames[key], t.runTaken[name] = name, true
	return name
}

// free is a name for a variable at place that none of reserved, the run's
// names and taken holds. t.mu is held.
func (t *Tracker) free(place string, reserved, taken map[string]bool) string {
	base := varName(place)
	name := base
	for i := 2; taken[name] || t.runTaken[name] || reserved[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

type trackerKey struct{}

// WithTracker puts t on ctx, for the parsers of a connection.
func WithTracker(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, trackerKey{}, t)
}

// TrackerFrom is the tracker on ctx, nil when there is none.
func TrackerFrom(ctx context.Context) *Tracker {
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	return t
}
