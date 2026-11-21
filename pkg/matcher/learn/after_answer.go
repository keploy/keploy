package learn

import (
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
)

// afterAnswer is the bookkeeping of the running agent's calls that waits for
// its call's answer to be written (Tracker.AfterAnswer): one ordered queue
// for the tracker, so every set's calls are booked in the order their
// adapters queued them. Each item is a pure recording move through Book (it
// returns nothing, so no failure of it can reach an answer already sent).
//
// Every acquisition of the tracker's lock but a Book method's own settles
// the queue first (Tracker.lock): a later call's read, any serve the store
// numbers, a test's Begin and End and the set's NewSet all see every item
// queued before them, as they saw it when the item ran before its answer
// was written. A call that reaches keploy after the application read an
// answer therefore reads the facts of that answer's call, whichever
// goroutine ran them.
type afterAnswer struct {
	// pending counts the items queued and not yet run: a settle with none
	// costs one atomic load.
	pending atomic.Int64
	// mu guards q and working.
	mu sync.Mutex
	q  []func(Book)
	// working is whether a worker goroutine is draining q (Kick).
	working bool
	// run is held while items run: one at a time, in queue order, whether
	// the worker or a settling caller runs them.
	run sync.Mutex
}

// maxAfterAnswer is how many calls' bookkeeping may wait at once. A call
// that finds that many waiting settles the queue itself, its own item
// included, before its answer is written, as every call did before the
// queue: the waiting bookkeeping, and the memory it holds, stay bounded
// however far the application's calls run ahead of the worker.
const maxAfterAnswer = 256

// Book is the tracker as a call's bookkeeping sees it once the call's answer
// is written (Tracker.AfterAnswer): it records what the call served and
// learned, for later calls and tests to read, and decides nothing. Its
// methods are the tracker's of the same names, run in queue order and
// without settling the queue they run from.
type Book struct{ t *Tracker }

// ServedNamed is Tracker.ServedNamed, booked after the answer.
func (b Book) ServedNamed(n uint64, e Earlier, vars models.MockVariables, clocks map[string]models.ClockOffset) {
	b.t.servedNamedBooked(n, e, vars, clocks)
}

// ServedMockAsRecorded is Tracker.ServedMockAsRecorded, booked after the
// answer.
func (b Book) ServedMockAsRecorded(m *models.Mock) { b.t.servedMockAsRecordedBooked(m) }

// ServedMockLive is Tracker.ServedMockLive, booked after the answer.
func (b Book) ServedMockLive(m *models.Mock, key string, view func() (*models.Mock, error)) {
	b.t.servedMockLiveBooked(m, key, view)
}

// AfterAnswer queues f, a call's bookkeeping, to run after every item queued
// before it and before any later acquisition of the tracker's lock (lock),
// so it is never read late. The caller queues it before it writes the
// call's answer and calls Kick once the answer is written: the worker runs
// it while the application reads the answer. Where maxAfterAnswer items
// already wait, the caller runs the queue, f included, itself, as before
// the queue. A nil tracker runs f at once.
func (t *Tracker) AfterAnswer(f func(Book)) {
	if f == nil {
		return
	}
	if t == nil {
		f(Book{})
		return
	}
	a := &t.after
	a.mu.Lock()
	a.q = append(a.q, f)
	a.pending.Add(1)
	full := len(a.q) > maxAfterAnswer
	a.mu.Unlock()
	if full {
		t.settle()
	}
}

// Kick starts the queue's worker, if it has items and none runs: the caller
// wrote its answer. An item no Kick follows (an answer that was never
// written) runs at the next settle.
func (t *Tracker) Kick() {
	if t == nil {
		return
	}
	a := &t.after
	a.mu.Lock()
	if a.working || len(a.q) == 0 {
		a.mu.Unlock()
		return
	}
	a.working = true
	a.mu.Unlock()
	go t.work()
}

// work drains the queue until it is empty, and is the worker no more.
func (t *Tracker) work() {
	a := &t.after
	for {
		t.drain()
		a.mu.Lock()
		if len(a.q) == 0 {
			a.working = false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
}

// settle returns once every item queued before it was called has run.
func (t *Tracker) settle() {
	if t.after.pending.Load() == 0 {
		return
	}
	t.drain()
}

// drain runs the queue's items, in order, until it is empty. One drain runs
// at a time (run): a settling caller waits for the item the worker runs,
// then runs what is left itself.
func (t *Tracker) drain() {
	a := &t.after
	a.run.Lock()
	defer a.run.Unlock()
	for {
		a.mu.Lock()
		if len(a.q) == 0 {
			a.mu.Unlock()
			return
		}
		f := a.q[0]
		a.q[0] = nil
		a.q = a.q[1:]
		a.mu.Unlock()
		f(Book{t: t})
		a.pending.Add(-1)
	}
}

// lock takes the tracker's lock once every bookkeeping item queued before it
// has run (settle). Every method takes the lock so, but Book's: an item
// never waits for the queue it runs from. An Order's own reads of its
// tracker (o.t.mu) take it as they did: the Order was read under lock, so
// every item queued before its read had run, and an item queued after it is
// a call's that reached keploy after the read, which the Order's numbers
// (upTo) leave out as they did when the item ran before its answer.
func (t *Tracker) lock() {
	t.settle()
	t.mu.Lock()
}
