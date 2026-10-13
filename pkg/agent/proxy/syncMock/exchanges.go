package manager

import (
	"sync"
	"time"
)

// OpenExchangeWait bounds how long, past the time a recording's stop asks
// about, an exchange still open keeps the stop draining (PendingBefore). It is
// the relay's own bound on a connection whose client has gone
// (relay.DefaultHalfCloseGrace): the relay forwards the client's close to the
// server and keeps reading the server's answer for that long. An exchange
// open longer, such as a stream that never ends, is not waited for.
const OpenExchangeWait = 10 * time.Second

// openExchangeWaiting is what a stop is told it waits on for an open exchange.
const openExchangeWaiting = "an HTTP exchange still open (its response is still being sent)"

// Exchanges keeps the exchanges parsers have open: a request captured whose
// response is not yet recorded. The zero value is ready to use. A recording's stop waits for those its test
// command started (PendingBefore): a server can finish a response after the
// command has exited, and what it finishes is recorded only while the
// recording runs. Measured on an enterprise-ui lane: the api-server ends a job's
// event stream 5 s after the job, so the last test's stream ended 2.4 s after
// the run, by when the mock set was saved without it, and that test's replay
// was served another test's stream.
type Exchanges struct {
	mu   sync.Mutex
	next uint64
	open map[uint64]int64 // id -> start, UnixNano
}

// Open says an exchange whose request started at start is open, and returns
// the func that says it has ended: its mock recorded, or not to be. The func
// may be called more than once. A zero start is taken as now.
func (e *Exchanges) Open(start time.Time) func() {
	if e == nil {
		return func() {}
	}
	if start.IsZero() {
		start = time.Now()
	}
	e.mu.Lock()
	if e.open == nil {
		e.open = map[uint64]int64{}
	}
	e.next++
	id := e.next
	e.open[id] = start.UnixNano()
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			delete(e.open, id)
			e.mu.Unlock()
		})
	}
}

// OpenBefore reports whether an exchange that started at or before `before`
// is still open, at now: never once now is OpenExchangeWait past before.
func (e *Exchanges) OpenBefore(before, now time.Time) bool {
	if e == nil || now.Sub(before) >= OpenExchangeWait {
		return false
	}
	b := before.UnixNano()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, start := range e.open {
		if start <= b {
			return true
		}
	}
	return false
}

// OpenExchange says a parser whose mocks go to m has an exchange open
// (Exchanges.Open). A capture with no watermark, the proxy's, answers
// PendingBefore from these. Nil-safe.
func (m *SyncMockManager) OpenExchange(start time.Time) func() {
	if m == nil {
		return func() {}
	}
	return m.exchanges.Open(start)
}
