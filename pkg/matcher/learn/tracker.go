package learn

import (
	"context"
	"sync"
)

// Tracker keeps, for the test that is running, the mocks F has served in it:
// the earlier places Classify looks in. The agent owns one; it begins a new
// test whenever the replay opens the next test's window, and the parsers find
// it on their connection's context.
type Tracker struct {
	mu     sync.Mutex
	epoch  uint64
	served []Earlier
	vars   Variables
}

// NewTracker makes a tracker whose variables are followed by vars (nil:
// Unfollowed).
func NewTracker(vars Variables) *Tracker {
	if vars == nil {
		vars = Unfollowed
	}
	return &Tracker{vars: vars}
}

// Begin starts the next test: nothing is earlier than its first call.
func (t *Tracker) Begin() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.epoch++
	t.served = nil
	t.mu.Unlock()
}

// Test is the running test's number, and what F served in it so far. A
// request reads it as it starts, and hands the number back to Served.
func (t *Tracker) Test() (uint64, []Earlier) {
	if t == nil {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch, append([]Earlier(nil), t.served...)
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

// Variables is the runtime the tracker hands learned variables to.
func (t *Tracker) Variables() Variables {
	if t == nil || t.vars == nil {
		return Unfollowed
	}
	return t.vars
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
