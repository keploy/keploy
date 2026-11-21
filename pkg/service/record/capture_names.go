package record

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// captureNames binds each capture id a recording's test cases came with
// (models.TestCase.CaptureID) to the name the recording saved that test case
// under, so that what the agent names by the capture id, a mapping entry or a
// revoke, is filed under the saved name.
//
// The agent gives a request it captures without --sync one capture id, and
// maps its mocks, owns them and revokes it by it; the test case comes with no
// name of its own and is named as it is saved (the testdb's naming strategy).
// Its mapping often comes before it is saved: the agent resolves a request's
// mocks before it hands its test case over, and the two come on separate
// streams. The mapping consumer holds such an entry until the test case is
// saved (saved) or known not to be (notSaved, or end: the test-case stream is
// over without it), and is woken on each (wakeCh).
//
// A binding is kept for the whole recording: a test's later mapping (its
// late-binned mocks) and a revoke of it can come at any time before the end.
type captureNames struct {
	mu sync.Mutex
	// names maps a capture id to the name its test case was saved under, or
	// to "" when its test case was not saved.
	names map[string]string
	// ended is set once the test-case stream is over: an id with no binding
	// then has no test case.
	ended bool
	// wake is signalled on every change; capacity 1, so a signal is never
	// lost and the test-case loop never blocks on it.
	wake chan struct{}
}

func newCaptureNames() *captureNames {
	return &captureNames{names: map[string]string{}, wake: make(chan struct{}, 1)}
}

// inserted tells the names how the store took the test case that came with
// capture id id: saved under name (err nil and a name), or not saved (an
// error, or no name: a store that took it and wrote nothing). An empty id (a
// test case with a name of its own) is no capture's. Nil-safe.
func (c *captureNames) inserted(id, name string, err error) {
	if c == nil || id == "" {
		return
	}
	if err != nil {
		name = ""
	}
	c.mu.Lock()
	c.names[id] = name
	c.mu.Unlock()
	c.signal()
}

// end tells the names the test-case stream is over: no test case comes any
// more, so an id with no binding has none. Idempotent; nil-safe.
func (c *captureNames) end() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.ended = true
	c.mu.Unlock()
	c.signal()
}

// nameOf reports the name the test case of capture id id was saved under, and
// whether that is decided: "" and true when it was not saved, or the stream
// ended without it; false while its test case may still come. Any other name
// is no capture id's, and is its own name, decided. Nil-safe: a recording
// with no names files a name as it comes.
func (c *captureNames) nameOf(id string) (name string, decided bool) {
	if c == nil || !models.IsCaptureID(id) {
		return id, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.names[id]; ok {
		return n, true
	}
	return "", c.ended
}

// signal wakes the consumer without blocking.
func (c *captureNames) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// wakeCh is the channel the mapping consumer selects on; nil (never ready)
// with no names.
func (c *captureNames) wakeCh() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.wake
}
