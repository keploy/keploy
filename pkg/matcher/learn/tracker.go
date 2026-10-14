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
	open   bool
	// names are the running test's variables, by where each is bound (its
	// mock and place), and taken the names given.
	names map[string]string
	taken map[string]bool
}

// NewTracker makes a tracker with no test open.
func NewTracker() *Tracker {
	return &Tracker{}
}

// Begin starts the next test: nothing is earlier than its first call.
func (t *Tracker) Begin() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.epoch++
	t.served = nil
	t.mocks = nil
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
// far, leaf by leaf, and every mock any parser served in it so far
// (ServedMock). A request reads it as it starts, and hands the number back
// to Served.
func (t *Tracker) Test() (uint64, []Earlier, []*models.Mock) {
	if t == nil {
		return 0, nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch, append([]Earlier(nil), t.served...), append([]*models.Mock(nil), t.mocks...)
}

// ServedMock notes a mock any parser served in the running test: an earlier
// place of the test's recording, whatever its kind, for Classify (Raw).
func (t *Tracker) ServedMock(m *models.Mock) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	// Between tests a served mock is no test's: it is not kept.
	if t.open {
		t.mocks = append(t.mocks, m)
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
// same for every call that uses it, and no other variable's in the test
// (variables are named per test). ok is false when that test has ended. A
// nil tracker, which knows of no test, names a variable by its place alone.
func (t *Tracker) Name(n uint64, mock, place string) (name string, ok bool) {
	if t == nil {
		return varName(place), true
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
	for i := 2; t.taken[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	t.names[key], t.taken[name] = name, true
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
