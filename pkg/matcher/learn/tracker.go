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
	// minted are the values the running test made (Minted), in the order
	// they first appeared.
	minted []Minted
	// keyed: the run is under a claim (NewKey, the proven wire's claim of a
	// test process): its calls outside every test are its claim key's, and
	// what they sent first is an earlier place of the key's tests. The rest
	// is the current key's, kept across its tests: the exchanges the learn
	// step served outside every test (leaf by leaf), the values they made,
	// the names of the key's variables (by where each is bound) and those
	// names taken, and every name a test's variable took in the key.
	keyed         bool
	outsideServed []Earlier
	outsideMinted []Minted
	outsideNames  map[string]string
	outsideTaken  map[string]bool
	keyTaken      map[string]bool
}

// Minted is a value the running test made: one that first appears in the
// test's recording at a request place of one of the test's own HTTP calls,
// where F saw it drift (a name or an id the test, or the app, mints anew on
// every run). The learn step learns it there as noise or as an echo of that
// call; a later call of the same test whose recorded answer holds it whole
// makes it a variable of the whole test: bound where it first appeared, and
// answered there with this run's value.
type Minted struct {
	// Mock and Place are where it first appeared: the mock's name, and the
	// variable place (VarPlace) of its request that held it whole.
	Mock, Place string
	// Recorded is its value in the recording, Live the value F saw there.
	Recorded, Live string
	// Type is the type of the request leaf that held it: String or Number.
	Type Type
	// Outside: a call of the claim key made outside every test made it
	// (Tracker.MintOutside); it is the key's, not one test's.
	Outside bool
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
	t.minted = nil
	t.keyed = false
	t.forgetKey()
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
	t.minted = nil
	t.open = true
	t.mu.Unlock()
}

// Mint notes a value test number n made (Minted): nothing when that test has
// ended, for a value of another type than a string or a number, for an empty
// one, or when the test made a value recorded equal already (the first place
// it appeared at binds it).
func (t *Tracker) Mint(n uint64, m Minted) {
	if t == nil || m.Recorded == "" || m.Live == "" || m.Type != String && m.Type != Number {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != n || !t.open {
		return
	}
	for _, have := range t.minted {
		if have.Recorded == m.Recorded {
			return
		}
	}
	t.minted = append(t.minted, m)
}

// Minted are the values test number n made so far (Mint), in the order they
// first appeared; none once that test has ended.
func (t *Tracker) Minted(n uint64) []Minted {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != n {
		return nil
	}
	return append([]Minted(nil), t.minted...)
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
	return t.epoch, append([]Earlier(nil), t.served...), append([]*models.Mock(nil), t.mocks...), before
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

// Served adds a mock F served to test number n's history; nothing when that
// test has ended since.
func (t *Tracker) Served(n uint64, e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.epoch == n {
		t.served = append(t.served, e)
	}
	t.mu.Unlock()
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
	if t.epoch != n {
		return "", false
	}
	key := mock + "\x00" + place
	if name, ok := t.names[key]; ok {
		return name, true
	}
	if t.names == nil {
		t.names, t.taken = map[string]string{}, map[string]bool{}
	}
	base := varName(place)
	name = base
	for i := 2; t.taken[name] || reserved[name] || t.outsideTaken[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	t.names[key], t.taken[name] = name, true
	if t.keyTaken != nil {
		t.keyTaken[name] = true
	}
	return name, true
}

// NewKey begins a claim (the proven wire's claim of a test process, under
// key): what the previous claim's calls made outside every test is no earlier
// place of this claim's tests (a test process run again runs its beforeAll
// again, and mints its values anew). From now on a call outside every test is
// this claim's (ServedOutside, MintOutside, OutsideName).
func (t *Tracker) NewKey(string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.keyed = true
	t.forgetKey()
	t.mu.Unlock()
}

// forgetKey drops the claim key's state. The caller holds mu.
func (t *Tracker) forgetKey() {
	t.outsideServed, t.outsideMinted = nil, nil
	t.outsideNames, t.outsideTaken, t.keyTaken = map[string]string{}, map[string]bool{}, map[string]bool{}
}

// Keyed reports whether the run is under a claim (NewKey).
func (t *Tracker) Keyed() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.keyed
}

// ServedOutside adds an exchange the learn step served while no test was
// open to the claim key's (Earlier.Outside): an earlier place of every later
// test of the key. Nothing while a test is open, or with no claim.
func (t *Tracker) ServedOutside(e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.keyed || t.open {
		return
	}
	e.Outside, e.Own = true, false
	t.outsideServed = append(t.outsideServed, e)
}

// MintOutside notes a value a call of the claim key made outside every test
// (Minted.Outside), as Mint does a test's.
func (t *Tracker) MintOutside(m Minted) {
	if t == nil || m.Recorded == "" || m.Live == "" || m.Type != String && m.Type != Number {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.keyed || t.open {
		return
	}
	for _, have := range t.outsideMinted {
		if have.Recorded == m.Recorded {
			return
		}
	}
	m.Outside = true
	t.outsideMinted = append(t.outsideMinted, m)
}

// Outside are the claim key's exchanges served outside every test and the
// values they made, in order; none with no claim.
func (t *Tracker) Outside() ([]Earlier, []Minted) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.keyed {
		return nil, nil
	}
	return append([]Earlier(nil), t.outsideServed...), append([]Minted(nil), t.outsideMinted...)
}

// OutsideName is the name of the claim key's variable bound at place of
// mock (a call made outside every test): the same for every call of the key
// that carries it, and no other variable's in the key (a test's variables
// avoid it too: Name). ok is false with no claim.
func (t *Tracker) OutsideName(mock, place string) (name string, ok bool) {
	if t == nil {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.keyed {
		return "", false
	}
	key := mock + "\x00" + place
	if name, ok := t.outsideNames[key]; ok {
		return name, true
	}
	base := varName(place)
	name = base
	for i := 2; t.outsideTaken[name] || t.keyTaken[name] || t.taken[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	t.outsideNames[key], t.outsideTaken[name] = name, true
	return name, true
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
